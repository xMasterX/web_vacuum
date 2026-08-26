package netwatch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGateStaysOnlineBelowThreshold(t *testing.T) {
	g := New(Options{Threshold: 3, MinWait: time.Millisecond})
	g.TrustHosts("trusted.test")
	g.ReportFailure("trusted.test", errors.New("connection refused"))
	g.ReportFailure("trusted.test", errors.New("connection refused"))
	if !g.Online() {
		t.Fatal("two failures should not declare an outage")
	}
	// A success in between must reset the streak, so scattered failures across
	// a long crawl never accumulate into a false outage.
	g.ReportSuccess("trusted.test")
	g.ReportFailure("trusted.test", errors.New("connection refused"))
	g.ReportFailure("trusted.test", errors.New("connection refused"))
	if !g.Online() {
		t.Fatal("streak should have been reset by the success")
	}
}

func TestGateBlocksThenReleasesWhenNetworkReturns(t *testing.T) {
	var reachable atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// A transport that fails until reachable flips, simulating a dead link.
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !reachable.Load() {
				return nil, &net.OpError{Op: "dial", Err: errors.New("network is unreachable")}
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}

	g := New(Options{
		ProbeURLs: []string{srv.URL},
		Threshold: 1,
		MinWait:   10 * time.Millisecond,
		MaxWait:   40 * time.Millisecond,
		Timeout:   time.Second,
		Transport: tr,
	})
	g.TrustHosts("trusted.test")

	g.ReportFailure("trusted.test", errors.New("no route to host"))
	if g.Online() {
		t.Fatal("gate should be offline after crossing the threshold")
	}

	// Five workers park on the gate, exactly as the crawler's workers would.
	const workers = 5
	var wg sync.WaitGroup
	released := make(chan time.Time, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.Wait(context.Background()); err != nil {
				t.Errorf("Wait: %v", err)
			}
			released <- time.Now()
		}()
	}

	select {
	case <-released:
		t.Fatal("a worker was released while the gate was offline")
	case <-time.After(60 * time.Millisecond):
	}

	reachable.Store(true)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("workers were not released after the network came back")
	}
	if !g.Online() {
		t.Fatal("gate should be online again")
	}
	st := g.Status()
	if st.TotalOutages != 1 {
		t.Fatalf("TotalOutages = %d, want 1", st.TotalOutages)
	}
	if st.TotalDowntime <= 0 {
		t.Fatal("downtime should have been recorded")
	}
}

func TestWaitHonoursContextCancellation(t *testing.T) {
	g := New(Options{Threshold: 1, MinWait: time.Hour, MaxWait: time.Hour,
		ProbeURLs: []string{"http://127.0.0.1:1"}, Timeout: 50 * time.Millisecond})
	g.TrustHosts("trusted.test")
	g.ReportFailure("trusted.test", errors.New("connection refused"))

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- g.Wait(ctx) }()

	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait ignored context cancellation")
	}
}

func TestIsTransportError(t *testing.T) {
	transport := []error{
		&net.OpError{Op: "dial", Err: errors.New("connection refused")},
		&net.DNSError{Err: "no such host"},
		errors.New("read tcp 1.2.3.4:80: connection reset by peer"),
		errors.New("net/http: TLS handshake timeout"),
		context.DeadlineExceeded,
	}
	for _, err := range transport {
		if !IsTransportError(err) {
			t.Errorf("IsTransportError(%v) = false, want true", err)
		}
	}
	if IsTransportError(nil) {
		t.Error("nil is not a transport error")
	}
	if IsTransportError(context.Canceled) {
		t.Error("a deliberate cancellation is not a network outage")
	}
	if IsTransportError(errors.New("404 not found")) {
		t.Error("an HTTP status is not a transport error")
	}
}

// TestUntrustedHostFailuresDoNotTripTheGate is the protection against an old
// site full of links to hosts that stopped existing years ago. Those failures
// must be recorded against the link, never mistaken for the network going away.
func TestUntrustedHostFailuresDoNotTripTheGate(t *testing.T) {
	g := New(Options{Threshold: 2, MinWait: time.Millisecond})
	g.TrustHosts("forum.example")

	for i := 0; i < 50; i++ {
		host := fmt.Sprintf("dead-image-host-%d.example", i)
		g.ReportFailure(host, &net.DNSError{Err: "no such host", IsNotFound: true})
	}
	if !g.Online() {
		t.Fatal("dead third-party hosts must not put the crawl into an outage")
	}

	// The site itself failing is a different matter entirely.
	g.ReportFailure("forum.example", &net.DNSError{Err: "no such host"})
	g.ReportFailure("forum.example", &net.DNSError{Err: "no such host"})
	if g.Online() {
		t.Fatal("the site's own host failing repeatedly should declare an outage")
	}
}

func TestSuccessMarksAHostTrusted(t *testing.T) {
	g := New(Options{Threshold: 2, MinWait: time.Millisecond})
	if g.Trusted("cdn.example") {
		t.Fatal("nothing should be trusted before it answers")
	}
	g.ReportSuccess("cdn.example")
	if !g.Trusted("cdn.example") {
		t.Fatal("a host that answered should be trusted")
	}
}
