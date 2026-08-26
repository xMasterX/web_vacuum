package fetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/netwatch"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

func newTestClient(t *testing.T, tweak func(*config.Config)) *Client {
	t.Helper()
	cfg := config.Default()
	cfg.StartURLs = []string{"http://127.0.0.1/"}
	cfg.Destination = t.TempDir()
	cfg.Request.Attempts = 5
	cfg.Resilience.RetryBaseDelay = config.Duration(time.Millisecond)
	cfg.Resilience.RetryMaxDelay = config.Duration(10 * time.Millisecond)
	cfg.Resilience.OfflineMinWait = config.Duration(5 * time.Millisecond)
	cfg.Resilience.OfflineMaxWait = config.Duration(20 * time.Millisecond)
	if tweak != nil {
		tweak(cfg)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	gate := netwatch.New(netwatch.Options{
		ProbeURLs: netwatch.ProbeTargets(cfg.StartURLs),
		Threshold: 3,
		MinWait:   cfg.Resilience.OfflineMinWait.D(),
		MaxWait:   cfg.Resilience.OfflineMaxWait.D(),
		Timeout:   time.Second,
	})
	c, err := New(cfg, gate)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func get(t *testing.T, c *Client, raw string) (*Response, []byte, error) {
	t.Helper()
	u, err := urlx.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resp, err := c.Do(context.Background(), Request{URL: u})
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, err
	}
	return resp, body, nil
}

func TestRetriesTransientServerErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>ok</html>")
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	resp, body, err := get(t, c, srv.URL+"/page")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Status != 200 || string(body) != "<html>ok</html>" {
		t.Fatalf("status=%d body=%q", resp.Status, body)
	}
	if resp.Attempts != 4 {
		t.Fatalf("Attempts = %d, want 4", resp.Attempts)
	}
}

func TestGivesUpImmediatelyOnNotFound(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	_, _, err := get(t, c, srv.URL+"/missing")
	fe, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if fe.Status != 404 || fe.Kind != KindHTTP {
		t.Fatalf("err = %+v", fe)
	}
	if fe.Retryable() {
		t.Error("404 should not be marked retryable")
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("404 was requested %d times, want 1", n)
	}
}

func TestHonoursRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, "done")
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	start := time.Now()
	_, body, err := get(t, c, srv.URL+"/limited")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if string(body) != "done" {
		t.Fatalf("body = %q", body)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("Retry-After ignored: retried after %v", elapsed)
	}
}

// TestSurvivesConnectionLoss is the scenario that motivated this tool: the
// network dies mid-crawl and comes back later. Nothing may be abandoned.
func TestSurvivesConnectionLoss(t *testing.T) {
	var down atomic.Bool
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			// Hijack and close without a response: an abrupt disconnect, which
			// is what a dropped wifi link looks like to the client.
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		}
		served.Add(1)
		io.WriteString(w, "recovered")
	}))
	defer srv.Close()

	cfg := func(c *config.Config) {
		c.StartURLs = []string{srv.URL + "/"}
		c.Request.Attempts = 3
		c.Resilience.NeverGiveUp = true
	}
	c := newTestClient(t, cfg)

	down.Store(true)
	go func() {
		time.Sleep(300 * time.Millisecond)
		down.Store(false)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	u, _ := urlx.Parse(srv.URL + "/page")
	resp, err := c.Do(ctx, Request{URL: u})
	if err != nil {
		t.Fatalf("job was abandoned during an outage: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "recovered" {
		t.Fatalf("body = %q", body)
	}
}

func TestConditionalGetReportsNotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		io.WriteString(w, "content")
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	u, _ := urlx.Parse(srv.URL + "/x")
	resp, err := c.Do(context.Background(), Request{URL: u, ETag: `"v1"`})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if !resp.NotModified {
		t.Fatalf("expected 304, got %d", resp.Status)
	}
}

func TestMaxFileSizeStopsTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		buf := make([]byte, 1024)
		for i := 0; i < 100; i++ {
			w.Write(buf)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, func(cfg *config.Config) {
		cfg.Limits.MaxFileSize = config.ByteSize(4096)
	})
	_, _, err := get(t, c, srv.URL+"/big")
	if err != ErrTooLarge {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestPerHostConcurrencyIsCapped(t *testing.T) {
	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		io.WriteString(w, "x")
	}))
	defer srv.Close()

	c := newTestClient(t, func(cfg *config.Config) { cfg.Request.PerHostConns = 2 })
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			get(t, c, srv.URL+"/a")
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if p := peak.Load(); p > 2 {
		t.Fatalf("peak concurrency = %d, want <= 2", p)
	}
}

func TestRateLimiterPacesThroughput(t *testing.T) {
	r := newRateLimiter(100 * 1024)
	start := time.Now()
	for i := 0; i < 4; i++ {
		r.take(64 * 1024)
	}
	// 256 KiB at 100 KiB/s cannot finish instantly even allowing for the burst.
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("rate limiter did not pace: %v", elapsed)
	}
}
