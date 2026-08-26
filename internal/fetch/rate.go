package fetch

import (
	"sync"
	"time"
)

// rateLimiter is a token bucket over total download throughput. It exists so a
// long unattended crawl on a home connection does not saturate the link and
// make everything else on the network unusable.
type rateLimiter struct {
	mu          sync.Mutex
	bytesPerSec int64
	tokens      float64
	last        time.Time
	burst       float64
}

func newRateLimiter(bytesPerSec int64) *rateLimiter {
	burst := float64(bytesPerSec)
	if burst < 64*1024 {
		burst = 64 * 1024
	}
	return &rateLimiter{
		bytesPerSec: bytesPerSec,
		tokens:      burst,
		burst:       burst,
		last:        time.Now(),
	}
}

// take blocks until n bytes of allowance are available. A rate of zero means
// unlimited, so the check is a single atomic-free comparison on the hot path.
func (r *rateLimiter) take(n int) {
	if r == nil || n <= 0 {
		return
	}
	for {
		r.mu.Lock()
		if r.bytesPerSec <= 0 {
			r.mu.Unlock()
			return
		}
		now := time.Now()
		elapsed := now.Sub(r.last).Seconds()
		r.last = now
		r.tokens += elapsed * float64(r.bytesPerSec)
		if r.tokens > r.burst {
			r.tokens = r.burst
		}
		if r.tokens >= float64(n) {
			r.tokens -= float64(n)
			r.mu.Unlock()
			return
		}
		deficit := float64(n) - r.tokens
		wait := time.Duration(deficit / float64(r.bytesPerSec) * float64(time.Second))
		r.mu.Unlock()

		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		if wait > time.Second {
			wait = time.Second
		}
		time.Sleep(wait)
	}
}

// SetRate changes the cap at runtime, so the TUI's speed keys work mid-crawl.
func (r *rateLimiter) SetRate(bytesPerSec int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bytesPerSec = bytesPerSec
	burst := float64(bytesPerSec)
	if burst < 64*1024 {
		burst = 64 * 1024
	}
	r.burst = burst
	if r.tokens > burst {
		r.tokens = burst
	}
}

// Rate reports the current cap, 0 meaning unlimited.
func (r *rateLimiter) Rate() int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytesPerSec
}
