// Package netwatch keeps a download alive across network outages.
//
// A Gate sits between the crawler's workers and the network. Transport-level
// failures (DNS, connection refused, TLS, timeouts) do not consume a URL's
// retry budget and never end a job. Instead they park every worker on the Gate
// while a single prober waits for connectivity to come back, then release them
// all. A laptop that sleeps overnight, a VPN that drops, a router that reboots:
// the crawl pauses and continues by itself.
package netwatch

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// State is the connectivity state of a job.
type State int

const (
	// Online means requests are flowing.
	Online State = iota
	// Offline means the Gate is holding workers while it probes.
	Offline
)

func (s State) String() string {
	if s == Offline {
		return "offline"
	}
	return "online"
}

// Status is a snapshot for the UI.
type Status struct {
	State         State
	Since         time.Time
	Attempts      int
	NextProbeIn   time.Duration
	LastError     string
	TotalOutages  int
	TotalDowntime time.Duration
}

// Options configures a Gate.
type Options struct {
	// ProbeURLs are tried in order until one answers. Any HTTP response counts
	// as connectivity, including 500s: the question is whether packets move.
	ProbeURLs []string
	MinWait   time.Duration
	MaxWait   time.Duration
	// Threshold is how many consecutive transport failures declare an outage.
	// Above 1 so that a single unreachable third-party host does not stall a
	// crawl that is otherwise healthy.
	Threshold int
	// Timeout bounds a single probe request.
	Timeout time.Duration
	// Transport is used for probes; nil uses a private default.
	Transport http.RoundTripper
	// OnChange is called whenever the state flips, for logging and the UI.
	OnChange func(Status)
}

// Gate holds workers while the network is down.
type Gate struct {
	opt Options

	mu       sync.Mutex
	cond     *sync.Cond
	state    State
	since    time.Time
	attempts int
	nextAt   time.Time
	lastErr  string
	probing  bool

	consecutive atomic.Int64
	outages     atomic.Int64
	downtimeNS  atomic.Int64

	// knownGood records hosts that have answered at least once. Only their
	// failures count toward declaring an outage: a host that has never worked
	// is a dead link, not evidence that the network went away. Without this a
	// forum full of long-gone image hosts would park the crawl every few
	// minutes waiting for connectivity that was never missing.
	goodMu    sync.RWMutex
	knownGood map[string]bool

	client *http.Client
}

// New creates a Gate. It starts Online; nothing probes until a failure is
// reported, so an ordinary crawl pays nothing for this machinery.
func New(opt Options) *Gate {
	if opt.MinWait <= 0 {
		opt.MinWait = 2 * time.Second
	}
	if opt.MaxWait < opt.MinWait {
		opt.MaxWait = 2 * time.Minute
	}
	if opt.Threshold <= 0 {
		opt.Threshold = 3
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Second
	}
	g := &Gate{opt: opt, state: Online, knownGood: map[string]bool{}}
	g.cond = sync.NewCond(&g.mu)
	g.client = &http.Client{
		Timeout:   opt.Timeout,
		Transport: opt.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return g
}

// SetOnChange installs the state-change callback after construction, which is
// how the crawl engine wires outage notifications into its event stream.
func (g *Gate) SetOnChange(fn func(Status)) {
	g.mu.Lock()
	g.opt.OnChange = fn
	g.mu.Unlock()
}

// ProbeTargets derives probe URLs from the hosts being crawled. Probing the
// actual target is more useful than probing a third party: if the site is
// unreachable but the rest of the internet is fine, the job should still wait
// rather than burn through every URL marking it failed.
func ProbeTargets(startURLs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range startURLs {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			continue
		}
		origin := u.Scheme + "://" + u.Host + "/"
		if !seen[origin] {
			seen[origin] = true
			out = append(out, origin)
		}
	}
	return out
}

// TrustHosts marks hosts as expected to work before anything has been fetched,
// so an outage at the very start of a job is still detected. The hosts a crawl
// was pointed at are the natural choice.
func (g *Gate) TrustHosts(hosts ...string) {
	g.goodMu.Lock()
	for _, h := range hosts {
		if h != "" {
			g.knownGood[strings.ToLower(h)] = true
		}
	}
	g.goodMu.Unlock()
}

// Trusted reports whether a host has ever answered.
func (g *Gate) Trusted(host string) bool {
	g.goodMu.RLock()
	defer g.goodMu.RUnlock()
	return g.knownGood[strings.ToLower(host)]
}

// Wait blocks until the network is believed to be up, or ctx is done. Every
// worker calls this before each request; when online it is a single mutex
// acquisition, so the hot path stays cheap.
func (g *Gate) Wait(ctx context.Context) error {
	g.mu.Lock()
	if g.state == Online {
		g.mu.Unlock()
		return ctx.Err()
	}
	g.mu.Unlock()

	// sync.Cond cannot select on a context, so a watchdog goroutine wakes all
	// waiters when the context is cancelled.
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }
	go func() {
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.cond.Broadcast()
			g.mu.Unlock()
		case <-done:
		}
	}()
	defer stop()

	g.mu.Lock()
	for g.state == Offline && ctx.Err() == nil {
		g.cond.Wait()
	}
	g.mu.Unlock()
	return ctx.Err()
}

// ReportSuccess records that a request to host completed at the transport
// level. It clears the failure streak, so an outage needs genuinely consecutive
// failures, and remembers that the host works.
func (g *Gate) ReportSuccess(host string) {
	g.consecutive.Store(0)
	if host == "" {
		return
	}
	if !g.Trusted(host) {
		g.goodMu.Lock()
		g.knownGood[strings.ToLower(host)] = true
		g.goodMu.Unlock()
	}
}

// ReportFailure records a transport-level failure. When the streak crosses the
// threshold the Gate goes offline and starts probing. Returns true if this call
// triggered an outage.
func (g *Gate) ReportFailure(host string, err error) bool {
	if err == nil {
		return false
	}
	// A host that has never answered is a broken link, not a broken network.
	// Its failures are the crawl's problem to record, not a reason to stop.
	if host != "" && !g.Trusted(host) {
		return false
	}
	msg := err.Error()
	n := g.consecutive.Add(1)
	if int(n) < g.opt.Threshold {
		return false
	}

	g.mu.Lock()
	if g.state == Offline {
		g.lastErr = msg
		g.mu.Unlock()
		return false
	}
	g.state = Offline
	g.since = time.Now()
	g.attempts = 0
	g.lastErr = msg
	g.outages.Add(1)
	start := g.probing
	g.probing = true
	status := g.statusLocked()
	g.mu.Unlock()

	if g.opt.OnChange != nil {
		g.opt.OnChange(status)
	}
	if !start {
		go g.probeLoop()
	}
	return true
}

// ForceOffline puts the Gate into the offline state immediately, used when the
// crawler knows connectivity is gone (for example the OS reported the interface
// went down) without waiting for a failure streak.
func (g *Gate) ForceOffline(reason string) {
	g.consecutive.Store(int64(g.opt.Threshold))
	g.ReportFailure("", errors.New(reason))
}

// probeLoop runs until connectivity returns. Backoff doubles from MinWait to
// MaxWait, so a short blip resumes in seconds while an overnight outage does
// not spin.
func (g *Gate) probeLoop() {
	wait := g.opt.MinWait
	for {
		g.mu.Lock()
		g.attempts++
		g.nextAt = time.Now().Add(wait)
		attempts := g.attempts
		status := g.statusLocked()
		g.mu.Unlock()
		if g.opt.OnChange != nil && attempts > 1 {
			g.opt.OnChange(status)
		}

		timer := time.NewTimer(wait)
		<-timer.C
		timer.Stop()

		if g.probe() {
			g.goOnline()
			return
		}
		wait *= 2
		if wait > g.opt.MaxWait {
			wait = g.opt.MaxWait
		}
	}
}

// probe reports whether any target answered. Any HTTP status counts; only a
// transport error means still-offline.
func (g *Gate) probe() bool {
	targets := g.opt.ProbeURLs
	if len(targets) == 0 {
		targets = []string{"https://www.google.com/generate_204", "https://1.1.1.1/"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.opt.Timeout)
	defer cancel()

	for _, t := range targets {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, t, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "webvacuum-probe/1.0")
		req.Header.Set("Cache-Control", "no-cache")
		resp, err := g.client.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			return true
		}
		// Some servers reject HEAD outright at the application layer; that is
		// still proof the network works, so only transport errors continue.
		if !IsTransportError(err) {
			return true
		}
	}
	return false
}

func (g *Gate) goOnline() {
	g.mu.Lock()
	if g.state == Online {
		g.probing = false
		g.mu.Unlock()
		return
	}
	g.downtimeNS.Add(int64(time.Since(g.since)))
	g.state = Online
	g.probing = false
	g.attempts = 0
	g.nextAt = time.Time{}
	g.consecutive.Store(0)
	status := g.statusLocked()
	g.cond.Broadcast()
	g.mu.Unlock()

	if g.opt.OnChange != nil {
		g.opt.OnChange(status)
	}
}

// Status returns a snapshot for display.
func (g *Gate) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.statusLocked()
}

func (g *Gate) statusLocked() Status {
	s := Status{
		State:         g.state,
		Since:         g.since,
		Attempts:      g.attempts,
		LastError:     g.lastErr,
		TotalOutages:  int(g.outages.Load()),
		TotalDowntime: time.Duration(g.downtimeNS.Load()),
	}
	if !g.nextAt.IsZero() {
		if d := time.Until(g.nextAt); d > 0 {
			s.NextProbeIn = d
		}
	}
	if g.state == Offline {
		s.TotalDowntime += time.Since(g.since)
	}
	return s
}

// Online reports whether the Gate currently believes the network is up.
func (g *Gate) Online() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state == Online
}

// IsTransportError reports whether an error happened below HTTP: DNS failure,
// refused connection, reset, TLS failure or timeout. These are the errors that
// mean "try again later", as opposed to a server answering with 404.
func IsTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	msg := strings.ToLower(err.Error())

	// A TLS or certificate rejection means the connection got far enough to be
	// refused at the protocol level: the network is up and the job should fail
	// this URL rather than park itself waiting for connectivity that is not
	// missing. A handshake *timeout* is the exception and stays below.
	for _, frag := range []string{
		"remote error: tls",
		"first record does not look like a tls handshake",
		"tls: unknown certificate",
		"tls: bad certificate",
		"tls: protocol version not supported",
		"tls: handshake failure",
		"x509:",
		"certificate is not valid",
		"certificate signed by unknown authority",
		"malformed http response",
		"server gave http response to https client",
	} {
		if strings.Contains(msg, frag) {
			return false
		}
	}

	// Some failures only surface as strings from crypto/tls and net/http.
	for _, frag := range []string{
		"connection refused", "connection reset", "no such host",
		"network is unreachable", "host is unreachable", "no route to host",
		"i/o timeout", "tls handshake timeout", "eof", "broken pipe",
		"server closed", "connection aborted", "timeout awaiting",
		"proxyconnect", "http2: client conn",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}
