package fetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xMasterX/web_vacuum/internal/urlx"
)

// TestPauseStopsTransfersImmediately covers the difference between "stop
// starting downloads" and "stop downloading". Pausing has to do the second:
// with several connections streaming large files, only gating new requests
// leaves throughput high for as long as those files take to finish.
func TestPauseStopsTransfersImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		chunk := make([]byte, 16*1024)
		for i := 0; i < 400; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(time.Millisecond)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	u, _ := urlx.Parse(srv.URL + "/big")

	resp, err := c.Do(context.Background(), Request{URL: u})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	var read atomic.Int64
	go func() {
		buf := make([]byte, 8*1024)
		for {
			n, err := resp.Body.Read(buf)
			read.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()

	// Let it get going.
	time.Sleep(150 * time.Millisecond)
	if read.Load() == 0 {
		t.Fatal("nothing was transferred before pausing")
	}

	c.SetPaused(true)
	// Whatever was already inside a Read call may still land; take the mark
	// just after pausing and require the transfer to be flat from there.
	time.Sleep(50 * time.Millisecond)
	atPause := read.Load()
	time.Sleep(250 * time.Millisecond)
	if grew := read.Load() - atPause; grew > 0 {
		t.Errorf("transfer continued while paused: %d more bytes", grew)
	}

	c.SetPaused(false)
	time.Sleep(150 * time.Millisecond)
	if read.Load() <= atPause {
		t.Error("transfer did not continue after resuming")
	}
}

// TestReleasePauseUnblocksEverything makes sure a job shutting down cannot
// leave a reader parked on a gate that will never open.
func TestReleasePauseUnblocksEverything(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	c.SetPaused(true)

	u, _ := urlx.Parse(srv.URL + "/x")
	resp, err := c.Do(context.Background(), Request{URL: u})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	done := make(chan struct{})
	go func() {
		io.ReadAll(resp.Body)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the read should have been held by the pause")
	case <-time.After(120 * time.Millisecond):
	}

	c.ReleasePause()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReleasePause did not unblock the reader")
	}
}
