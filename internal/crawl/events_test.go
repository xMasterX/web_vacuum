package crawl

import (
	"sync"
	"testing"
	"time"
)

// TestBusSurvivesSubscribersLeavingMidPublish reproduces a crash that took the
// whole program down partway through a long download.
//
// Publishing used to copy the subscriber list, release the lock, and only then
// deliver. A subscriber cancelling in that window — a terminal interface
// quitting, a browser tab closing — closed its channel before the send landed,
// and sending on a closed channel is a panic, not an error. It needed the two
// to coincide, so it showed up as a crash "after a while" rather than
// immediately.
func TestBusSurvivesSubscribersLeavingMidPublish(t *testing.T) {
	b := newBus(64)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Several publishers, as the worker pool would be.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.publish(Event{Kind: EventFetched, Path: "a/b.html"})
				}
			}
		}()
	}

	// Subscribers arriving and leaving continuously, as UIs attach and detach.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					ch, cancel := b.Subscribe(1)
					// Reading nothing is deliberate: it forces the buffer to
					// fill so publish takes the drop path as well.
					_ = ch
					cancel()
				}
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Shutting down while publishers are still going must also be safe.
	var wg2 sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			for j := 0; j < 500; j++ {
				b.publish(Event{Kind: EventLog, Message: "x"})
			}
		}()
	}
	b.closeAll()
	wg2.Wait()

	// Closing twice, which happens when a job is stopped and then closed.
	b.closeAll()

	// A subscriber arriving after shutdown gets a closed channel rather than
	// one that never delivers anything.
	ch, cancel := b.Subscribe(1)
	defer cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("a subscriber after shutdown should not receive events")
		}
	case <-time.After(time.Second):
		t.Error("a subscriber after shutdown should get a closed channel")
	}
}

// TestBusDeliversToLiveSubscribers keeps the fix honest: not panicking is easy
// if nothing is ever delivered.
func TestBusDeliversToLiveSubscribers(t *testing.T) {
	b := newBus(16)
	ch, cancel := b.Subscribe(8)
	defer cancel()

	b.publish(Event{Kind: EventFetched, Path: "one.html"})
	b.publish(Event{Kind: EventFetched, Path: "two.html"})

	for _, want := range []string{"one.html", "two.html"} {
		select {
		case ev := <-ch:
			if ev.Path != want {
				t.Errorf("got %q, want %q", ev.Path, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("event %q was never delivered", want)
		}
	}
	if got := len(b.Recent()); got != 2 {
		t.Errorf("Recent() has %d events, want 2", got)
	}
}
