package crawl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

// TestConnectionsChangeWhileRunning is the setting people reach for while
// watching a download: too slow, so open more; too aggressive, so pull back.
// Both directions have to take effect on the job already running.
func TestConnectionsChangeWhileRunning(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			for i := 0; i < 400; i++ {
				fmt.Fprintf(w, `<a href="/p/%d.html">p</a>`, i)
			}
			return
		}
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()

		time.Sleep(40 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>page</body></html>")
	})

	cfg := testConfig(t, srv.URL+"/", t.TempDir())
	cfg.General.Connections = 2
	cfg.Request.PerHostConns = 16

	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	lowPeak := peak
	peak = 0
	mu.Unlock()

	if got := e.SetConnections(12); got != 12 {
		t.Fatalf("SetConnections returned %d", got)
	}
	if e.Config().General.Connections != 12 {
		t.Errorf("config not updated: %d", e.Config().General.Connections)
	}

	time.Sleep(1200 * time.Millisecond)
	mu.Lock()
	highPeak := peak
	mu.Unlock()

	e.Stop("test finished")
	<-done

	t.Logf("peak concurrency: %d before, %d after raising the limit", lowPeak, highPeak)
	if lowPeak > 3 {
		t.Errorf("started with 2 connections but reached %d", lowPeak)
	}
	if highPeak <= lowPeak {
		t.Errorf("raising connections had no effect (%d then %d)", lowPeak, highPeak)
	}
}

// TestReconfigureAppliesAndReportsWhatItCannot pins down the contract: safe
// settings change, unsafe ones are named rather than quietly dropped.
func TestReconfigureAppliesAndReportsWhatItCannot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>hi</body></html>")
	}))
	defer srv.Close()

	dest := t.TempDir()
	cfg := testConfig(t, srv.URL+"/", dest)
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	next := e.Config().Clone()
	next.Scope.Exclude = []string{`/private/`}
	next.Limits.MaxFiles = 500
	next.Request.Delay = config.Duration(250 * time.Millisecond)
	next.General.IgnoreRobots = true
	// These cannot change on a running job.
	next.Destination = t.TempDir()
	next.StartURLs = []string{"https://somewhere-else.example/"}

	res, err := e.Reconfigure(next)
	if err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if !res.Changed() {
		t.Fatal("nothing was applied")
	}
	if len(res.Ignored) < 2 {
		t.Errorf("destination and start URLs should have been reported as unchangeable: %v", res.Ignored)
	}

	got := e.Config()
	if got.Destination != dest {
		t.Errorf("destination was changed to %s", got.Destination)
	}
	if len(got.Scope.Exclude) != 1 || got.Scope.Exclude[0] != `/private/` {
		t.Errorf("exclude not applied: %v", got.Scope.Exclude)
	}
	if got.Limits.MaxFiles != 500 {
		t.Errorf("max files not applied: %d", got.Limits.MaxFiles)
	}
	if !got.General.IgnoreRobots {
		t.Error("robots setting not applied")
	}

	// The new scope rules must actually be the ones consulted now.
	if d := e.rules().CheckURL(mustParse(t, srv.URL+"/private/x.html"), 0, 1, nil); d.Allow {
		t.Error("the new exclude rule is not in force")
	}
}

// TestReconfigureRejectsBadSettingsWithoutDisturbingTheJob makes sure a typo in
// a pattern cannot take a running download down with it.
func TestReconfigureRejectsBadSettingsWithoutDisturbingTheJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html></html>")
	}))
	defer srv.Close()

	e, err := New(testConfig(t, srv.URL+"/", t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	before := e.Config().Clone()
	bad := e.Config().Clone()
	bad.Scope.Exclude = []string{"([unclosed"}

	if _, err := e.Reconfigure(bad); err == nil {
		t.Fatal("a malformed pattern should be rejected")
	}
	if len(e.Config().Scope.Exclude) != len(before.Scope.Exclude) {
		t.Error("the running job was modified by a rejected change")
	}
	if e.rules() == nil {
		t.Fatal("scope rules were lost")
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := urlx.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u
}
