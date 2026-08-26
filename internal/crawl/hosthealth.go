package crawl

import (
	"net/url"
	"strings"
	"sync"
)

// deadHostThreshold is how many transport failures a host that has never
// answered is allowed before the crawl stops trying it. Two is enough to
// distinguish a genuinely unreachable host from a single unlucky request, and
// low enough that an old forum's hundreds of dead image hosts cost seconds
// rather than hours.
const deadHostThreshold = 2

// hostHealth tracks which hosts are worth contacting.
//
// A site that has been running for years accumulates links to image hosts,
// trackers and CDNs that no longer resolve. Each one costs a DNS timeout, then
// a retry, then another, multiplied by every post that ever referenced it. The
// first couple of failures are worth paying; the rest are not, so a host that
// has never once answered is written off and everything still queued for it is
// skipped without touching the network.
type hostHealth struct {
	mu    sync.RWMutex
	stats map[string]*hostStat
}

type hostStat struct {
	failures  int
	successes int
	dead      bool
	reason    string
}

func newHostHealth() *hostHealth {
	return &hostHealth{stats: map[string]*hostStat{}}
}

func (h *hostHealth) entry(host string) *hostStat {
	s, ok := h.stats[host]
	if !ok {
		s = &hostStat{}
		h.stats[host] = s
	}
	return s
}

// success records that a host answered, which permanently clears it.
func (h *hostHealth) success(host string) {
	host = strings.ToLower(host)
	if host == "" {
		return
	}
	h.mu.Lock()
	s := h.entry(host)
	s.successes++
	s.failures = 0
	s.dead = false
	h.mu.Unlock()
}

// failure records a transport-level failure. It returns true the moment the
// host is written off, so the caller can say so once instead of per URL.
func (h *hostHealth) failure(host, reason string) bool {
	host = strings.ToLower(host)
	if host == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	s := h.entry(host)
	s.failures++
	// A host that has worked before is having a bad moment, not a permanent
	// one; the ordinary retry and offline handling covers it.
	if s.successes > 0 || s.dead {
		return false
	}
	if s.failures >= deadHostThreshold {
		s.dead = true
		s.reason = reason
		return true
	}
	return false
}

// dead reports whether a host has been written off, and why.
func (h *hostHealth) dead(host string) (bool, string) {
	host = strings.ToLower(host)
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.stats[host]
	if !ok {
		return false, ""
	}
	return s.dead, s.reason
}

// deadCount reports how many hosts were written off, for the final summary.
func (h *hostHealth) deadCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, s := range h.stats {
		if s.dead {
			n++
		}
	}
	return n
}

// deadHosts lists the written-off hosts, for the report.
func (h *hostHealth) deadHosts() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []string
	for host, s := range h.stats {
		if s.dead {
			out = append(out, host)
		}
	}
	return out
}

func hostKeyOf(u *url.URL) string {
	if u == nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
