package crawl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStoppedJobStillRewritesLinks covers the failure that makes a stopped
// download worthless: the pages are on disk, but every link in them still
// points at the live site, so the folder cannot be browsed offline at all.
//
// Interrupting means "stop fetching". It must not mean "leave the archive
// broken".
func TestStoppedJobStillRewritesLinks(t *testing.T) {
	var served int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>
<a href="/forumdisplay.php?f=8">forum 8</a>
<a href="/forumdisplay.php?f=9">forum 9</a>
</body></html>`)
	})
	mux.HandleFunc("/forumdisplay.php", func(w http.ResponseWriter, r *http.Request) {
		served++
		// Slow enough that the job is still running when it gets stopped.
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><h1>forum %s</h1><a href="/">home</a></body></html>`, r.URL.Query().Get("f"))
	})

	dest := t.TempDir()
	cfg := testConfig(t, srv.URL+"/", dest)
	cfg.General.Connections = 1

	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	// Let the home page and at least one forum page land, then stop, the way a
	// user pressing F10 or Ctrl-C would.
	time.Sleep(600 * time.Millisecond)
	e.Stop("stopped by the test")

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after Stop")
	}

	home, err := os.ReadFile(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatalf("home page missing: %v", err)
	}
	body := string(home)

	// Nothing may be left as a bare site-relative path: opened from the
	// filesystem those resolve against the disk root and go nowhere.
	if strings.Contains(body, `href="/forumdisplay.php`) {
		t.Errorf("links were never rewritten after the job was stopped:\n%s", body)
	}
	// Whatever was downloaded points at the local file; the rest points back at
	// the live site so it still works.
	hasLocal := strings.Contains(body, `href="./forumdisplay.php@f=`)
	hasAbsolute := strings.Contains(body, `href="`+srv.URL+`/forumdisplay.php`)
	if !hasLocal && !hasAbsolute {
		t.Errorf("links are neither local nor absolute:\n%s", body)
	}
	if !hasLocal {
		t.Errorf("at least one downloaded page should be linked locally:\n%s", body)
	}
}

// TestResumeRelocalizesEarlierPages covers the other half: a page rewritten
// during an interrupted run still points at the live site for everything that
// had not been fetched yet, and finishing the job has to fix those links.
func TestResumeRelocalizesEarlierPages(t *testing.T) {
	var allowSecond bool
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><a href="/later.html">later</a></body></html>`)
	})
	mux.HandleFunc("/later.html", func(w http.ResponseWriter, r *http.Request) {
		if !allowSecond {
			http.Error(w, "not yet", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><p>arrived on the second run</p></body></html>`)
	})

	dest := t.TempDir()

	// First run: the linked page cannot be fetched, so the link is absolutized.
	first := runEngine(t, testConfig(t, srv.URL+"/", dest))
	first.Close()

	home, _ := os.ReadFile(filepath.Join(dest, "index.html"))
	if !strings.Contains(string(home), srv.URL+"/later.html") {
		t.Fatalf("expected an absolute link after the first run:\n%s", home)
	}

	// Second run: the page is available, and the earlier link must be upgraded.
	allowSecond = true
	second := runEngine(t, testConfig(t, srv.URL+"/", dest))
	defer second.Close()

	home, err := os.ReadFile(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(home), `href="./later.html"`) {
		t.Errorf("the link was not upgraded to the newly downloaded file:\n%s", home)
	}
	if _, err := os.Stat(filepath.Join(dest, "later.html")); err != nil {
		t.Errorf("later.html should exist after the second run: %v", err)
	}
}
