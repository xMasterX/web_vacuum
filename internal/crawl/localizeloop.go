package crawl

import (
	"context"
	"sync"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
)

// localizeGuard serializes rewrite passes. A pass triggered from a keystroke
// must not overlap one started by the timer, or two workers would rewrite the
// same file at once and the second would undo the first.
type localizeGuard struct {
	mu      sync.Mutex
	running bool
}

// tryStart claims the right to run a pass, or reports that one is already going.
func (g *localizeGuard) tryStart() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		return false
	}
	g.running = true
	return true
}

func (g *localizeGuard) finish() {
	g.mu.Lock()
	g.running = false
	g.mu.Unlock()
}

// LocalizeNow rewrites links across everything downloaded so far, without
// waiting for the crawl to finish. It is safe to call at any time and returns
// immediately; progress appears in the log.
//
// Rewriting is idempotent and only ever improves a link — a target that was not
// downloaded when the last pass ran becomes local once it is — so running it
// early costs nothing but a little disk reading.
func (e *Engine) LocalizeNow() string {
	if e.cfg().General.FileModification != config.ModifyLocalize {
		return "link rewriting is switched off for this job"
	}
	if !e.localizing.tryStart() {
		return "links are already being rewritten"
	}

	go func() {
		defer e.localizing.finish()
		start := time.Now()
		if err := e.Localize(context.Background()); err != nil {
			e.logf(LevelWarn, "rewriting links: %v", err)
			return
		}
		e.lastLocalizeAt.Store(time.Now().UnixNano())
		e.lastLocalizeCost.Store(int64(time.Since(start)))
	}()
	return "rewriting links…"
}

// startLocalizeLoop rewrites links at intervals while the crawl runs, so an
// unexpected end leaves a mirror that browses rather than one whose links all
// lead back to the internet.
//
// The interval stretches itself on a large mirror: a pass that takes a
// noticeable fraction of the gap between passes would otherwise spend the
// crawl's time re-reading files instead of downloading them.
func (e *Engine) startLocalizeLoop(ctx context.Context) func() {
	every := e.cfg().General.LocalizeEvery.D()
	if every <= 0 || e.cfg().General.FileModification != config.ModifyLocalize {
		return func() {}
	}

	done := make(chan struct{})
	go func() {
		interval := every
		timer := time.NewTimer(interval)
		defer timer.Stop()

		lastFiles := int64(0)
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-timer.C:
			}

			// Nothing new means nothing to improve.
			files := e.filesDone.Load()
			if files == lastFiles {
				timer.Reset(interval)
				continue
			}
			lastFiles = files

			if !e.localizing.tryStart() {
				timer.Reset(interval)
				continue
			}
			start := time.Now()
			err := e.Localize(ctx)
			cost := time.Since(start)
			e.localizing.finish()

			if err != nil {
				e.logf(LevelWarn, "rewriting links: %v", err)
			} else {
				e.lastLocalizeAt.Store(time.Now().UnixNano())
				e.lastLocalizeCost.Store(int64(cost))
			}

			// Back off when a pass is expensive relative to the interval, and
			// ease back towards the configured rate when it is cheap again.
			switch {
			case cost > interval/4:
				interval *= 2
				e.logf(LevelDebug, "link rewriting took %s; spacing passes out to %s",
					cost.Round(time.Millisecond), interval)
			case interval > every && cost < interval/20:
				interval /= 2
				if interval < every {
					interval = every
				}
			}
			timer.Reset(interval)
		}
	}()

	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}
