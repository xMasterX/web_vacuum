package fetch

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// hostLimiter enforces per-host politeness: a concurrency cap and a minimum
// gap between requests. It also adapts downward when a host answers 429, and
// recovers slowly afterwards, which keeps long crawls from getting banned.
type hostLimiter struct {
	mu sync.Mutex
	// hosts is keyed by "scheme://host:port".
	hosts map[string]*hostState

	baseConns int
	baseDelay time.Duration
	jitter    time.Duration
	adaptive  bool
	minConns  int
	maxDelay  time.Duration
	rng       *rand.Rand
	rngMu     sync.Mutex
}

type hostState struct {
	sem      chan struct{}
	conns    int
	mu       sync.Mutex
	nextFree time.Time
	delay    time.Duration
	// throttled counts consecutive rate-limit signals; each one halves the
	// allowance, each clean stretch gives some back.
	throttled int
	ok        int
}

func newHostLimiter(conns int, delay, jitter time.Duration, adaptive bool) *hostLimiter {
	if conns <= 0 {
		conns = 4
	}
	return &hostLimiter{
		hosts:     map[string]*hostState{},
		baseConns: conns,
		baseDelay: delay,
		jitter:    jitter,
		adaptive:  adaptive,
		minConns:  1,
		maxDelay:  30 * time.Second,
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// semaphore returns the host's current slot channel.
func (h *hostState) semaphore() chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sem
}

// resize replaces the slot channel with one of a different width. Callers hold
// h.mu. Requests already running keep the channel they took a slot from.
func (h *hostState) resize(conns int) {
	if conns < 1 {
		conns = 1
	}
	if h.conns == conns {
		return
	}
	h.conns = conns
	h.sem = make(chan struct{}, conns)
}

func (l *hostLimiter) state(key string) *hostState {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.hosts[key]
	if !ok {
		h = &hostState{
			sem:   make(chan struct{}, l.baseConns),
			conns: l.baseConns,
			delay: l.baseDelay,
		}
		l.hosts[key] = h
	}
	return h
}

// acquire blocks until this host may be contacted. The returned release
// function must be called exactly once.
//
// The semaphore is captured here rather than read again on release. Resizing a
// host's allowance swaps in a fresh channel, and a release that looked the
// field up later would drain the new one — which is empty — and block forever,
// taking the worker with it. Holding on to the channel that was actually filled
// means a resize can never strand a request already in flight.
func (l *hostLimiter) acquire(ctx context.Context, key string) (func(), error) {
	h := l.state(key)
	sem := h.semaphore()
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	release := func() { <-sem }

	for {
		h.mu.Lock()
		wait := time.Until(h.nextFree)
		if wait <= 0 {
			d := h.delay
			if d > 0 && l.jitter > 0 {
				d += l.jitterDur()
			}
			h.nextFree = time.Now().Add(d)
			h.mu.Unlock()
			return release, nil
		}
		h.mu.Unlock()

		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			release()
			return nil, ctx.Err()
		}
	}
}

func (l *hostLimiter) jitterDur() time.Duration {
	l.rngMu.Lock()
	defer l.rngMu.Unlock()
	return time.Duration(l.rng.Int63n(int64(l.jitter) + 1))
}

// penalize is called on 429/503. It lengthens the delay for the host and, when
// adaptive throttling is on, permanently narrows concurrency for this run.
func (l *hostLimiter) penalize(key string, retryAfter time.Duration) {
	h := l.state(key)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ok = 0
	h.throttled++

	if h.delay <= 0 {
		h.delay = 500 * time.Millisecond
	} else {
		h.delay *= 2
	}
	if h.delay > l.maxDelay {
		h.delay = l.maxDelay
	}
	if retryAfter > 0 {
		if retryAfter > l.maxDelay {
			retryAfter = l.maxDelay
		}
		if retryAfter > h.delay {
			h.delay = retryAfter
		}
		if until := time.Now().Add(retryAfter); until.After(h.nextFree) {
			h.nextFree = until
		}
	}
	if l.adaptive && h.conns > l.minConns && h.throttled >= 2 {
		h.throttled = 0
		h.resize(max(l.minConns, h.conns/2))
	}
}

// reward is called after a clean response, slowly undoing a penalty so a
// temporary rate limit does not throttle the rest of a multi-hour crawl.
func (l *hostLimiter) reward(key string) {
	h := l.state(key)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ok++
	if h.ok < 20 {
		return
	}
	h.ok = 0
	h.throttled = 0
	if h.delay > l.baseDelay {
		h.delay = max(l.baseDelay, h.delay/2)
	}
	if h.conns < l.baseConns {
		h.resize(h.conns + 1)
	}
}

// setBase changes the defaults applied to hosts from now on, and widens or
// narrows the existing ones to match. Hosts that were throttled for answering
// 429 keep their penalty: a new setting is not a reason to forget what a server
// already said about how fast it wants to be asked.
func (l *hostLimiter) setBase(conns int, delay, jitter time.Duration) {
	if conns <= 0 {
		conns = 1
	}
	l.mu.Lock()
	l.baseConns = conns
	l.baseDelay = delay
	l.jitter = jitter
	states := make([]*hostState, 0, len(l.hosts))
	for _, h := range l.hosts {
		states = append(states, h)
	}
	l.mu.Unlock()

	for _, h := range states {
		h.mu.Lock()
		if h.throttled == 0 {
			h.delay = delay
			h.resize(conns)
		}
		h.mu.Unlock()
	}
}

// setDelay raises the floor for a host, used for robots.txt Crawl-delay.
func (l *hostLimiter) setDelay(key string, d time.Duration) {
	if d <= 0 {
		return
	}
	h := l.state(key)
	h.mu.Lock()
	defer h.mu.Unlock()
	if d > h.delay {
		h.delay = d
	}
}

// snapshot reports current per-host pacing, for the UI.
func (l *hostLimiter) snapshot() map[string]HostPace {
	l.mu.Lock()
	keys := make([]string, 0, len(l.hosts))
	states := make([]*hostState, 0, len(l.hosts))
	for k, v := range l.hosts {
		keys = append(keys, k)
		states = append(states, v)
	}
	l.mu.Unlock()

	out := make(map[string]HostPace, len(keys))
	for i, k := range keys {
		h := states[i]
		h.mu.Lock()
		out[k] = HostPace{Connections: h.conns, Delay: h.delay, Throttled: h.throttled > 0}
		h.mu.Unlock()
	}
	return out
}

// HostPace describes the pacing currently applied to one host.
type HostPace struct {
	Connections int
	Delay       time.Duration
	Throttled   bool
}
