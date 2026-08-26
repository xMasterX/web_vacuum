package fetch

import "sync"

// pauseGate holds transfers while a job is paused.
//
// Pausing only new requests is not really pausing: eight connections already
// streaming a large file keep going, and the speed reading takes a long time to
// fall. Gating the body reads themselves means throughput drops to zero the
// moment the key is pressed, and resuming continues the same downloads from
// where they stopped rather than starting them over.
type pauseGate struct {
	mu     sync.Mutex
	cond   *sync.Cond
	paused bool
	// released is set when the job ends, so nothing can be left waiting on a
	// gate that will never open again.
	released bool
}

func newPauseGate() *pauseGate {
	g := &pauseGate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// Wait blocks while the gate is closed.
func (g *pauseGate) Wait() {
	g.mu.Lock()
	for g.paused && !g.released {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

// Set opens or closes the gate.
func (g *pauseGate) Set(paused bool) {
	g.mu.Lock()
	g.paused = paused
	g.cond.Broadcast()
	g.mu.Unlock()
}

// Paused reports the current state.
func (g *pauseGate) Paused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

// Release permanently opens the gate, used when a job stops so no reader is
// left parked.
func (g *pauseGate) Release() {
	g.mu.Lock()
	g.released = true
	g.cond.Broadcast()
	g.mu.Unlock()
}
