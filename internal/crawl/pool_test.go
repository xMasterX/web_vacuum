package crawl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
)

// endlessSite serves a site that cannot be finished: every page links to two
// more, so the queue only ever grows.
//
// Resizing the worker pool is watched over several seconds, and a test that
// races the crawl to the finish line measures whichever won. This removes the
// race: the crawl runs until the test stops it, so what is observed in between
// is the pool and nothing else.
func endlessSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><a href="/p/1.html">start</a></body></html>`)
	})
	mux.HandleFunc("/p/", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/p/"), ".html"))
		if err != nil || n < 1 {
			http.NotFound(w, r)
			return
		}
		// Enough delay that connections overlap and the pane has something to
		// show, little enough that the test is not waiting on the server.
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><a href="/p/%d.html">l</a><a href="/p/%d.html">r</a></body></html>`,
			n*2, n*2+1)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// startCrawl runs a job in the background and stops it when the test ends.
func startCrawl(t *testing.T, conns int) (*Engine, <-chan error) {
	t.Helper()
	srv := endlessSite(t)
	cfg := testConfig(t, srv.URL+"/", t.TempDir())
	cfg.General.Connections = conns
	cfg.Request.PerHostConns = conns
	// Link rewriting at the end of an endless crawl is a long walk over
	// thousands of files that proves nothing here.
	cfg.General.FileModification = config.ModifyNone
	cfg.General.LocalizeEvery = config.Duration(0)

	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	t.Cleanup(func() {
		e.Stop("test finished")
		cancel()
		<-done
		e.Close()
	})
	return e, done
}

// TestLoweringThenRaisingConnectionsDoesNotRunAway covers the pool resize that
// went wrong in both directions at once.
//
// Workers used to retire by comparing their slot number to the connection
// count, and slot numbers came from a counter that only ever went up. After a
// lowering they no longer bore any relation to the size of the pool: raise the
// count again and the supervisor spawned workers whose slot numbers were
// already past it, each retiring the instant it started. The pool never reached
// the number asked for, and a fresh batch followed a quarter of a second later,
// for as long as the job ran — twenty dead rows a second in the connections
// pane.
//
// Going 8 -> 2 -> 7 is exactly the sequence that triggers it.
func TestLoweringThenRaisingConnectionsDoesNotRunAway(t *testing.T) {
	e, done := startCrawl(t, 8)

	waitFor(t, done, func() bool { return e.Snapshot().Stats.Done >= 10 }, "the crawl to get going")

	e.SetConnections(2)
	settle(t, done, 700*time.Millisecond)

	before := e.Snapshot().Stats.Done
	e.SetConnections(7)
	// Long enough for the old code to have spawned eight batches of five.
	settle(t, done, 2*time.Second)

	snap := e.Snapshot()
	if n := len(snap.Slots); n > 8 {
		t.Errorf("connections pane shows %d rows after 8 -> 2 -> 7; the pool is spawning workers it immediately retires", n)
	}
	if snap.Stats.Done <= before {
		t.Errorf("nothing downloaded after raising the count back to 7 (%d -> %d); the pool did not refill",
			before, snap.Stats.Done)
	}
}

// TestConnectionsPaneFollowsTheConnectionCount checks the display keeps up with
// a lowering rather than leaving rows from the highest count ever set.
func TestConnectionsPaneFollowsTheConnectionCount(t *testing.T) {
	e, done := startCrawl(t, 12)

	waitFor(t, done, func() bool { return len(e.Snapshot().Slots) >= 12 }, "the pool to reach 12")

	e.SetConnections(3)
	waitFor(t, done, func() bool { return len(e.Snapshot().Slots) <= 3 }, "the pane to shrink to 3")
}

// TestRaisingConnectionsAddsWorkers is the other half: a raise has to actually
// produce connections, not just a larger number in the settings.
func TestRaisingConnectionsAddsWorkers(t *testing.T) {
	e, done := startCrawl(t, 2)

	waitFor(t, done, func() bool { return e.Snapshot().Stats.Done >= 5 }, "the crawl to get going")

	e.SetConnections(10)
	waitFor(t, done, func() bool { return busySlots(e) >= 8 }, "at least 8 connections to be working")
}

// TestConnectionCountIsCapped keeps a typo in the Setup pane from being taken
// literally.
func TestConnectionCountIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>hi</body></html>")
	}))
	defer srv.Close()

	e, err := New(testConfig(t, srv.URL+"/", t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	if got := e.SetConnections(100000); got != config.MaxConnections {
		t.Errorf("SetConnections(100000) = %d, want %d", got, config.MaxConnections)
	}
	if got := e.SetConnections(0); got != 1 {
		t.Errorf("SetConnections(0) = %d, want 1", got)
	}
}

func busySlots(e *Engine) int {
	n := 0
	for _, s := range e.Snapshot().Slots {
		if s.Busy {
			n++
		}
	}
	return n
}

// waitFor polls until cond holds, failing if the crawl ends or time runs out
// first. Polling beats a fixed sleep: the pool is resized by a supervisor on a
// ticker, so the interesting moment is "once it has noticed", not a wall-clock
// delay.
func waitFor(t *testing.T, done <-chan error, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("the crawl ended while waiting for %s (err: %v)", what, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settle lets the supervisor act, failing if the crawl finishes meanwhile —
// which would mean the test measured nothing.
func settle(t *testing.T, done <-chan error, d time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("the crawl finished too early to observe a resize (err: %v)", err)
	case <-time.After(d):
	}
}
