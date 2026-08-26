// Package state is the crawl's memory. Every URL the crawler has seen is
// recorded in an append-only journal next to the download, so a job that is
// stopped, killed, or power-cycled resumes exactly where it left off instead of
// re-downloading a site from scratch.
package state

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Status is where a URL is in its lifecycle.
type Status string

const (
	// Pending means queued but not yet fetched.
	Pending Status = "pending"
	// Active means a worker is fetching it right now. Recorded so that a crash
	// mid-flight is recoverable: on resume, actives become pending again.
	Active Status = "active"
	// Done means saved to disk.
	Done Status = "done"
	// Failed means every attempt was used up.
	Failed Status = "failed"
	// Skipped means a rule excluded it; kept so the UI can explain why.
	Skipped Status = "skipped"
)

// Entry is everything known about one URL.
type Entry struct {
	Key       string `json:"k"`
	URL       string `json:"u"`
	Status    Status `json:"s"`
	Path      string `json:"p,omitempty"`
	Depth     int    `json:"d"`
	Role      string `json:"r,omitempty"`
	Referer   string `json:"ref,omitempty"`
	MediaType string `json:"mt,omitempty"`
	Size      int64  `json:"sz,omitempty"`
	ETag      string `json:"et,omitempty"`
	LastMod   string `json:"lm,omitempty"`
	HTTPCode  int    `json:"c,omitempty"`
	Err       string `json:"e,omitempty"`
	ErrKind   string `json:"ek,omitempty"`
	Attempts  int    `json:"a,omitempty"`
	Localized bool   `json:"loc,omitempty"`
	// Alias marks an entry that exists only so a second URL resolves to a file
	// downloaded under a different one — a redirect target, most often. It
	// points at a real file but is not itself a download, so it is excluded
	// from the counts.
	Alias bool `json:"al,omitempty"`
	// Pass records which retry sweep last touched this entry, so a later sweep
	// does not immediately re-try something it just tried.
	Pass      int   `json:"ps,omitempty"`
	UpdatedAt int64 `json:"t,omitempty"`
}

// Stats is an aggregate view for the UI.
type Stats struct {
	Total     int
	Pending   int
	Active    int
	Done      int
	Failed    int
	Skipped   int
	Bytes     int64
	Localized int
}

// Store is the on-disk journal plus its in-memory index.
type Store struct {
	dir  string
	path string

	mu      sync.RWMutex
	entries map[string]*Entry
	// paths is the reverse index used to keep two URLs from claiming one file.
	paths map[string]string

	f  *os.File
	bw *bufio.Writer

	dirty      int
	lastFlush  time.Time
	closed     bool
	bytesTotal int64
}

const journalName = "journal.jsonl"

// Open loads (or creates) the journal in dir. Any entry left Active by a crash
// is reset to Pending so nothing is silently lost.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	s := &Store{
		dir:       dir,
		path:      filepath.Join(dir, journalName),
		entries:   map[string]*Entry{},
		paths:     map[string]string{},
		lastFlush: time.Now(),
	}
	if err := s.replay(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	s.f = f
	s.bw = bufio.NewWriterSize(f, 128*1024)
	return s, nil
}

// replay rebuilds the index from the journal. A truncated final line (the
// normal result of a hard kill) is discarded rather than treated as corruption.
func (s *Store) replay() error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			// A partial write at the tail is expected after a crash.
			continue
		}
		if e.Key == "" {
			continue
		}
		if e.Status == Active {
			e.Status = Pending
		}
		cp := e
		s.applyLocked(&cp)
	}
	// A scanner error mid-file means the journal is damaged beyond the tail;
	// the entries read so far are still usable, so the crawl continues.
	return nil
}

// applyLocked installs an entry into the index. Caller holds no lock during
// replay (single-threaded) and the write lock otherwise.
func (s *Store) applyLocked(e *Entry) {
	prev, existed := s.entries[e.Key]
	if existed {
		s.bytesTotal -= prev.Size
		if prev.Path != "" && prev.Path != e.Path && !prev.Alias {
			delete(s.paths, prev.Path)
		}
	}
	s.entries[e.Key] = e
	s.bytesTotal += e.Size
	// An alias points at a file another entry owns, so it must not take over
	// that path in the reverse index.
	if e.Path != "" && !e.Alias {
		s.paths[e.Path] = e.Key
	}
}

// Put records an entry, appending to the journal. Writes are buffered; Flush
// or the periodic checkpoint makes them durable.
func (s *Store) Put(e *Entry) error {
	e.UpdatedAt = time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	cp := *e
	s.applyLocked(&cp)

	line, err := json.Marshal(&cp)
	if err != nil {
		return err
	}
	if _, err := s.bw.Write(line); err != nil {
		return err
	}
	if err := s.bw.WriteByte('\n'); err != nil {
		return err
	}
	s.dirty++
	return nil
}

// Get returns a copy of an entry.
func (s *Store) Get(key string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[key]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// Has reports whether a URL has been seen at all.
func (s *Store) Has(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.entries[key]
	return ok
}

// AddAlias records that another URL resolves to a file already downloaded.
// Nothing is fetched for it; it exists so link rewriting can find the file when
// a page refers to the URL a redirect landed on rather than the one requested.
func (s *Store) AddAlias(key, url, path string) error {
	if key == "" || path == "" {
		return nil
	}
	if existing, ok := s.Get(key); ok && !existing.Alias {
		// A real entry always wins; an alias must never mask a download.
		return nil
	}
	return s.Put(&Entry{
		Key: key, URL: url, Status: Done, Path: path, Alias: true,
	})
}

// ClaimPath reserves a destination-relative path for a key, returning a
// disambiguated variant when another URL already owns it.
func (s *Store) ClaimPath(key, want string, disambiguate func(string, func(string) bool) string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner, ok := s.paths[want]; ok && owner == key {
		return want
	}
	final := disambiguate(want, func(p string) bool {
		owner, taken := s.paths[p]
		return taken && owner != key
	})
	s.paths[final] = key
	return final
}

// PathFor returns the saved path for a key.
func (s *Store) PathFor(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[key]
	if !ok || e.Path == "" || e.Status != Done {
		return "", false
	}
	return e.Path, true
}

// HasPath reports whether a destination-relative path belongs to a saved file.
func (s *Store) HasPath(rel string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.paths[rel]
	if !ok {
		return false
	}
	e, ok := s.entries[key]
	return ok && e.Status == Done
}

// Select returns copies of every entry matching a predicate, in a stable order.
func (s *Store) Select(match func(*Entry) bool) []Entry {
	s.mu.RLock()
	out := make([]Entry, 0, 64)
	for _, e := range s.entries {
		if match == nil || match(e) {
			out = append(out, *e)
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Stats aggregates the index.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{Bytes: s.bytesTotal}
	for _, e := range s.entries {
		if e.Alias {
			// Aliases are pointers to files counted elsewhere; counting them
			// would inflate every total on screen.
			continue
		}
		st.Total++
		switch e.Status {
		case Pending:
			st.Pending++
		case Active:
			st.Active++
		case Done:
			st.Done++
		case Failed:
			st.Failed++
		case Skipped:
			st.Skipped++
		}
		if e.Localized {
			st.Localized++
		}
	}
	return st
}

// Flush makes buffered journal writes durable.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *Store) flushLocked() error {
	if s.closed || s.bw == nil {
		return nil
	}
	if err := s.bw.Flush(); err != nil {
		return err
	}
	s.dirty = 0
	s.lastFlush = time.Now()
	return s.f.Sync()
}

// Checkpoint compacts the journal to one line per entry and fsyncs it. Without
// this a long crawl's journal grows with every status change; with it, the file
// stays proportional to the number of URLs.
func (s *Store) Checkpoint() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	if err := s.bw.Flush(); err != nil {
		return err
	}

	tmp := s.path + ".compact"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 256*1024)
	for _, e := range s.entries {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		bw.Write(line)
		bw.WriteByte('\n')
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()

	// Swap the compacted file in, then reopen the append handle on it.
	s.f.Close()
	if err := os.Rename(tmp, s.path); err != nil {
		// Reopen the original so the crawl can keep recording.
		s.f, _ = os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		s.bw = bufio.NewWriterSize(s.f, 128*1024)
		return err
	}
	nf, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.f = nf
	s.bw = bufio.NewWriterSize(nf, 128*1024)
	s.dirty = 0
	s.lastFlush = time.Now()
	return nil
}

// Dirty reports how many writes are buffered, so the engine can decide when to
// checkpoint without holding a timer of its own.
func (s *Store) Dirty() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirty
}

// Close flushes and releases the journal.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	err := s.flushLocked()
	s.closed = true
	if s.f != nil {
		if cerr := s.f.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// Dir is where the journal lives.
func (s *Store) Dir() string { return s.dir }
