package crawl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
)

// TestLocalizeNowWorksMidCrawl covers the situation that cost a whole download:
// a long crawl that ends unexpectedly leaves every page pointing at the live
// site, because rewriting only ever happened at the end. Being able to run it
// on demand — and having it run periodically — means an interrupted download is
// still a usable copy.
func TestLocalizeNowWorksMidCrawl(t *testing.T) {
	var served atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			for i := 0; i < 60; i++ {
				fmt.Fprintf(w, `<a href="/p%d.html">page %d</a>`, i, i)
			}
			return
		}
		served.Add(1)
		// Slow enough that the crawl is still going when links are rewritten.
		time.Sleep(120 * time.Millisecond)
		fmt.Fprint(w, `<html><body><a href="/">home</a></body></html>`)
	})

	dest := t.TempDir()
	cfg := testConfig(t, srv.URL+"/", dest)
	cfg.General.Connections = 2
	cfg.General.LocalizeEvery = 0 // exercise the manual trigger on its own

	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	// Wait until a few pages exist, then rewrite without stopping anything.
	deadline := time.Now().Add(20 * time.Second)
	for served.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if msg := e.LocalizeNow(); !strings.Contains(msg, "rewriting") {
		t.Fatalf("LocalizeNow said %q", msg)
	}

	// Give the pass time to finish while downloads continue around it.
	time.Sleep(1500 * time.Millisecond)

	home, err := os.ReadFile(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatalf("home page not saved yet: %v", err)
	}
	body := string(home)
	if !strings.Contains(body, `href="./p`) {
		t.Errorf("no link was rewritten while the crawl was still running:\n%s", truncate(body, 400))
	}
	// Pages not yet fetched must point at the live site, not at nothing.
	if strings.Contains(body, `href="/p`) {
		t.Errorf("a site-absolute link survived, which cannot work offline:\n%s", truncate(body, 400))
	}

	e.Stop("test finished")
	<-done

	// The final pass must leave everything consistent.
	home, err = os.ReadFile(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(home), `href="/p`) {
		t.Errorf("links broken after the final pass:\n%s", truncate(string(home), 400))
	}
}

// TestLocalizeNowRefusesToOverlap keeps two passes off the same files.
func TestLocalizeNowRefusesToOverlap(t *testing.T) {
	var g localizeGuard
	if !g.tryStart() {
		t.Fatal("the first caller should get the pass")
	}
	if g.tryStart() {
		t.Error("a second caller should be turned away while one is running")
	}
	g.finish()
	if !g.tryStart() {
		t.Error("the pass should be available again once finished")
	}
	g.finish()
}

// TestLocalizeNowRespectsTheSetting reports rather than silently doing nothing
// when rewriting is switched off for the job.
func TestLocalizeNowRespectsTheSetting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html></html>")
	}))
	defer srv.Close()

	cfg := testConfig(t, srv.URL+"/", t.TempDir())
	cfg.General.FileModification = config.ModifyNone
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	if msg := e.LocalizeNow(); !strings.Contains(msg, "switched off") {
		t.Errorf("expected to be told rewriting is off, got %q", msg)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
