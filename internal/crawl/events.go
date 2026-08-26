package crawl

import (
	"sync"
	"time"

	"github.com/xMasterX/web_vacuum/internal/fetch"
	"github.com/xMasterX/web_vacuum/internal/netwatch"
	"github.com/xMasterX/web_vacuum/internal/state"
)

// Phase is the stage a job is in, shown prominently in both UIs.
type Phase string

const (
	PhaseIdle       Phase = "idle"
	PhaseStarting   Phase = "starting"
	PhaseCrawling   Phase = "crawling"
	PhaseRetrying   Phase = "retrying"
	PhaseLocalizing Phase = "localizing"
	PhasePaused     Phase = "paused"
	PhaseOffline    Phase = "waiting for network"
	PhaseDone       Phase = "done"
	PhaseStopped    Phase = "stopped"
	PhaseFailed     Phase = "failed"
)

// EventKind classifies a notification.
type EventKind string

const (
	EventLog      EventKind = "log"
	EventFetched  EventKind = "fetched"
	EventFailed   EventKind = "failed"
	EventSkipped  EventKind = "skipped"
	EventPhase    EventKind = "phase"
	EventNetwork  EventKind = "network"
	EventFinished EventKind = "finished"
)

// Level is a log severity.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Event is one thing that happened, delivered to every subscribed UI.
type Event struct {
	Kind    EventKind `json:"kind"`
	Time    time.Time `json:"time"`
	Level   Level     `json:"level,omitempty"`
	URL     string    `json:"url,omitempty"`
	Path    string    `json:"path,omitempty"`
	Status  int       `json:"status,omitempty"`
	Size    int64     `json:"size,omitempty"`
	Depth   int       `json:"depth,omitempty"`
	Message string    `json:"message,omitempty"`
	Phase   Phase     `json:"phase,omitempty"`
	// Duration is how long the request took, and MediaType what came back.
	// Both matter when scanning a log for the slow or the unexpected.
	Duration  time.Duration `json:"duration_ns,omitempty"`
	MediaType string        `json:"media_type,omitempty"`
	Attempts  int           `json:"attempts,omitempty"`
	Rendered  bool          `json:"rendered,omitempty"`
}

// SlotState is what one worker is doing, which is the htop-style per-connection
// row in the TUI.
type SlotState struct {
	ID       int       `json:"id"`
	Busy     bool      `json:"busy"`
	URL      string    `json:"url,omitempty"`
	Host     string    `json:"host,omitempty"`
	Started  time.Time `json:"started,omitempty"`
	Bytes    int64     `json:"bytes"`
	Total    int64     `json:"total"`
	Activity string    `json:"activity,omitempty"`
	// Status is the response code once headers have arrived, MediaType what the
	// server said it is, and Speed the rate for this transfer alone.
	Status    int           `json:"status,omitempty"`
	MediaType string        `json:"media_type,omitempty"`
	Speed     float64       `json:"speed,omitempty"`
	Elapsed   time.Duration `json:"elapsed_ns,omitempty"`
}

// Snapshot is a complete picture of a job for rendering.
type Snapshot struct {
	Name        string          `json:"name"`
	Destination string          `json:"destination"`
	Phase       Phase           `json:"phase"`
	Stats       state.Stats     `json:"stats"`
	Fetch       fetch.Stats     `json:"fetch"`
	Network     netwatch.Status `json:"network"`
	Slots       []SlotState     `json:"slots"`
	Queued      int             `json:"queued"`
	InFlight    int             `json:"in_flight"`
	Elapsed     time.Duration   `json:"elapsed_ns"`
	BytesPerSec float64         `json:"bytes_per_sec"`
	FilesPerSec float64         `json:"files_per_sec"`
	ETA         time.Duration   `json:"eta_ns"`
	Pass        int             `json:"pass"`
	StartedAt   time.Time       `json:"started_at"`
	// Limits echo what would stop the job, so the UI can show progress bars.
	MaxFiles int64 `json:"max_files"`
	MaxBytes int64 `json:"max_bytes"`
	// RateLimit is the current throughput cap, 0 meaning unlimited.
	RateLimit int64 `json:"rate_limit"`
	// Paused is tracked separately from Phase so a UI can show the button state
	// even while the phase reads "waiting for network".
	Paused bool `json:"paused"`
}

// bus fans events out to subscribers. Subscribers that fall behind lose events
// rather than stalling the crawl: a slow terminal must never throttle a
// download.
type bus struct {
	mu     sync.RWMutex
	subs   map[int]chan Event
	nextID int
	// recent keeps a ring of the last N events so a UI attaching mid-job (the
	// web UI, typically) can render immediately instead of showing a blank log.
	recent   []Event
	capacity int
	dropped  int64
	// closed stops delivery once the job is finished, so a goroutine still
	// publishing during shutdown finds the door shut rather than a closed
	// channel to send into.
	closed bool
}

func newBus(scrollback int) *bus {
	if scrollback <= 0 {
		scrollback = 500
	}
	return &bus{subs: map[int]chan Event{}, capacity: scrollback}
}

// Subscribe returns a channel of events and a cancel function.
func (b *bus) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	ch := make(chan Event, buffer)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
}

// publish delivers an event to every subscriber.
//
// Delivery happens while the lock is held. Taking a copy of the subscriber list
// and sending after unlocking would be faster, but it leaves a window in which
// a subscriber cancels — closing its channel — between the copy and the send,
// and sending on a closed channel is a panic that takes the whole program with
// it. Sends here never block, so holding the lock across them costs nothing.
func (b *bus) publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}

	b.recent = append(b.recent, e)
	if len(b.recent) > b.capacity {
		b.recent = b.recent[len(b.recent)-b.capacity:]
	}

	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// A subscriber that cannot keep up loses events rather than
			// slowing the crawl down.
			b.dropped++
		}
	}
}

// Recent returns a copy of the buffered event history.
func (b *bus) Recent() []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]Event(nil), b.recent...)
}

func (b *bus) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
}

// rateMeter is an exponentially weighted rate estimate, which reads far more
// steadily than a raw instantaneous rate on a bursty connection.
type rateMeter struct {
	mu       sync.Mutex
	last     time.Time
	lastVal  int64
	rate     float64
	halfLife time.Duration
}

func newRateMeter(halfLife time.Duration) *rateMeter {
	return &rateMeter{last: time.Now(), halfLife: halfLife}
}

// Update feeds the latest cumulative total and returns the smoothed rate.
func (m *rateMeter) Update(total int64) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	dt := now.Sub(m.last).Seconds()
	if dt <= 0 {
		return m.rate
	}
	delta := total - m.lastVal
	if delta < 0 {
		delta = 0
	}
	instant := float64(delta) / dt
	// Weight recent samples by how much of a half-life has elapsed.
	alpha := 1 - pow2(-dt/m.halfLife.Seconds())
	if alpha < 0 {
		alpha = 0
	} else if alpha > 1 {
		alpha = 1
	}
	m.rate = m.rate + alpha*(instant-m.rate)
	m.last = now
	m.lastVal = total
	return m.rate
}

// Reset drops the accumulated estimate and re-baselines on the current total,
// so a meter that starts again does not ramp down from a stale reading.
func (m *rateMeter) Reset(total int64) {
	m.mu.Lock()
	m.rate = 0
	m.lastVal = total
	m.last = time.Now()
	m.mu.Unlock()
}

// Rate returns the last computed rate without advancing the estimate.
func (m *rateMeter) Rate() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rate
}

// pow2 computes 2**x without pulling in math for one call site.
func pow2(x float64) float64 {
	// 2**x == e**(x*ln2); a short series is plenty for the smoothing factor.
	y := x * 0.6931471805599453
	term := 1.0
	sum := 1.0
	for i := 1; i < 12; i++ {
		term *= y / float64(i)
		sum += term
	}
	return sum
}
