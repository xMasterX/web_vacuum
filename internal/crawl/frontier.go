package crawl

import (
	"container/heap"
	"context"
	"net/url"
	"sync"

	"github.com/xMasterX/web_vacuum/internal/rules"
)

// Item is one unit of work in the queue.
type Item struct {
	Key     string
	URL     *url.URL
	Depth   int
	Role    rules.Role
	Referer string
	// From is the page that linked here, needed by the host+1 scope rule.
	From *url.URL
	// Pass is the retry sweep that queued this item; 0 is the main crawl.
	Pass int
	// Seed marks a start URL. Start URLs enter the queue without consulting the
	// scope rules, so they are also exempt from the re-check that purges the
	// queue when the exclusions are tightened mid-run. Without that exemption a
	// pattern that happened to match the starting page would end the crawl
	// rather than narrow it.
	Seed bool
	// seq preserves first-in-first-out order within a depth, which turns the
	// priority queue into a breadth-first crawl.
	seq int64
}

// itemHeap orders by depth, then by insertion order.
type itemHeap []*Item

func (h itemHeap) Len() int { return len(h) }
func (h itemHeap) Less(i, j int) bool {
	if h[i].Depth != h[j].Depth {
		return h[i].Depth < h[j].Depth
	}
	return h[i].seq < h[j].seq
}
func (h itemHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *itemHeap) Push(x any)   { *h = append(*h, x.(*Item)) }
func (h *itemHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return it
}

// frontier is the work queue. It knows when the crawl is finished: the queue is
// empty and no worker is still holding an item. Getting that wrong is how
// crawlers either exit early or hang forever, so it is the one piece of state
// that is tracked explicitly rather than inferred.
type frontier struct {
	mu   sync.Mutex
	cond *sync.Cond
	h    itemHeap

	inFlight int
	seq      int64
	closed   bool
	paused   bool
	// pushed counts everything ever queued, for the UI's total.
	pushed int64
}

func newFrontier() *frontier {
	f := &frontier{}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// Push adds work. It is a no-op once the frontier is closed.
func (f *frontier) Push(it *Item) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.seq++
	it.seq = f.seq
	heap.Push(&f.h, it)
	f.pushed++
	f.cond.Signal()
	return true
}

// It returns ok=false when the crawl is finished, the frontier was closed, or
// ctx was cancelled. A returned item is counted as in flight until Done is
// called.
// Pop blocks until work is available. retire, when supplied, is consulted each
// time the caller wakes: returning false retires this worker, which is how the
// pool shrinks without interrupting a download already in progress.
func (f *frontier) Pop(ctx context.Context, retire func() bool) (*Item, bool) {
	stop := f.watchContext(ctx)
	defer stop()

	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		if ctx.Err() != nil || f.closed {
			return nil, false
		}
		if retire != nil && !retire() {
			// This worker is surplus to the current connection count.
			return nil, false
		}
		if !f.paused && f.h.Len() > 0 {
			it := heap.Pop(&f.h).(*Item)
			f.inFlight++
			return it, true
		}
		// Empty queue with nothing in flight means no worker can produce more
		// work, so the crawl has genuinely drained.
		if f.h.Len() == 0 && f.inFlight == 0 && !f.paused {
			return nil, false
		}
		f.cond.Wait()
	}
}

// watchContext wakes every waiter when ctx is cancelled, because sync.Cond
// cannot participate in a select.
func (f *frontier) watchContext(ctx context.Context) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-ctx.Done():
			f.mu.Lock()
			f.cond.Broadcast()
			f.mu.Unlock()
		case <-done:
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// Done releases an item taken by Pop.
func (f *frontier) Done() {
	f.mu.Lock()
	f.inFlight--
	if f.inFlight <= 0 && f.h.Len() == 0 {
		// The last worker just finished with an empty queue: release everyone
		// waiting so they can observe the drained state and exit.
		f.cond.Broadcast()
	} else {
		f.cond.Signal()
	}
	f.mu.Unlock()
}

// SetPaused holds workers at Pop without discarding queued work.
func (f *frontier) SetPaused(p bool) {
	f.mu.Lock()
	f.paused = p
	f.cond.Broadcast()
	f.mu.Unlock()
}

// Paused reports the pause state.
func (f *frontier) Paused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

// Wake releases every waiter so it can re-check its own conditions, used when
// the worker count changes and some of them should now retire.
func (f *frontier) Wake() {
	f.mu.Lock()
	f.cond.Broadcast()
	f.mu.Unlock()
}

// Close stops the frontier and wakes every waiter.
func (f *frontier) Close() {
	f.mu.Lock()
	f.closed = true
	f.cond.Broadcast()
	f.mu.Unlock()
}

// Len reports how many items are queued.
func (f *frontier) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.h.Len()
}

// InFlight reports how many items workers are currently holding.
func (f *frontier) InFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight
}

// Drain removes and returns everything queued, used when a limit is hit.
func (f *frontier) Drain() []*Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*Item, 0, f.h.Len())
	for f.h.Len() > 0 {
		out = append(out, heap.Pop(&f.h).(*Item))
	}
	f.cond.Broadcast()
	return out
}
