package crawl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTighteningExcludeClearsTheQueue is the reason the scope prohibitions are
// applied a second time.
//
// The realistic case is someone thirty thousand URLs into a forum who notices
// that a link pattern is generating nothing but junk. Filtering it at discovery
// only would leave every already-queued match to be downloaded regardless,
// which is the opposite of what adding an exclusion means.
//
// The test pins that down exactly: a single connection, an index that queues
// two hundred junk pages ahead of the pages worth keeping, and an exclusion
// added from inside the handler for the first junk page — so the moment the
// rule lands is known precisely and nothing else is in flight. Every junk page
// after that first one must go unrequested, and every keeper must still arrive.
func TestTighteningExcludeClearsTheQueue(t *testing.T) {
	const junkPages, keepPages = 200, 20

	var (
		junkHits, keepHits atomic.Int32
		engine             atomic.Pointer[Engine]
		once               sync.Once
		reconfigErr        error
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>")
		// Junk first, so it sits at the head of the queue and the purge has
		// something left to do by the time the rule arrives.
		for i := 0; i < junkPages; i++ {
			fmt.Fprintf(w, `<a href="/junk/%d.html">j%d</a>`, i, i)
		}
		for i := 0; i < keepPages; i++ {
			fmt.Fprintf(w, `<a href="/keep/%d.html">k%d</a>`, i, i)
		}
		fmt.Fprint(w, "</body></html>")
	})
	mux.HandleFunc("/junk/", func(w http.ResponseWriter, r *http.Request) {
		junkHits.Add(1)
		// The first junk page is where the rule is added. With one connection
		// the crawler is inside this handler and nowhere else, so afterwards
		// every remaining junk page is decided by the new rule alone.
		once.Do(func() {
			e := engine.Load()
			next := e.Config().Clone()
			next.Scope.Exclude = append(next.Scope.Exclude, "/junk/")
			_, reconfigErr = e.Reconfigure(next)
		})
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>junk</body></html>")
	})
	mux.HandleFunc("/keep/", func(w http.ResponseWriter, r *http.Request) {
		keepHits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>keep</body></html>")
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig(t, srv.URL+"/", t.TempDir())
	cfg.General.Connections = 1
	cfg.Request.PerHostConns = 1

	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	engine.Store(e)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := e.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reconfigErr != nil {
		t.Fatalf("Reconfigure: %v", reconfigErr)
	}

	if got := junkHits.Load(); got != 1 {
		t.Errorf("junk pages requested = %d, want 1 (the one that triggered the rule); "+
			"the rest were queued before the exclusion and should have been dropped from the queue", got)
	}
	if got := keepHits.Load(); got != keepPages {
		t.Errorf("kept pages requested = %d, want %d; the exclusion took out more than it should", got, keepPages)
	}
}

// TestPurgingTheQueueSparesTheStartURL keeps a narrowing exclusion from ending
// the job instead of narrowing it.
//
// Start URLs are queued without consulting the scope rules at all, so applying
// those rules to them on the way out would be inconsistent as well as
// destructive: a pattern that happened to match the starting page would leave
// the crawl with nothing to do and no obvious explanation.
func TestPurgingTheQueueSparesTheStartURL(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>index</body></html>")
	}))
	defer srv.Close()

	cfg := testConfig(t, srv.URL+"/index.php", t.TempDir())
	// An exclusion that matches the start URL itself.
	cfg.Scope.Exclude = []string{"index"}

	runEngine(t, cfg)

	if hits.Load() == 0 {
		t.Error("the start URL was never requested; an exclusion matching it must not cancel the job")
	}
}
