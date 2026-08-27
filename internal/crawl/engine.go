// Package crawl is the download engine: it owns the queue, the workers, the
// on-disk mirror and the end-of-job localization pass. Both the terminal UI and
// the web UI are thin views over one Engine.
package crawl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/fetch"
	"github.com/xMasterX/web_vacuum/internal/localize"
	"github.com/xMasterX/web_vacuum/internal/netwatch"
	"github.com/xMasterX/web_vacuum/internal/pathmap"
	"github.com/xMasterX/web_vacuum/internal/robots"
	"github.com/xMasterX/web_vacuum/internal/rules"
	"github.com/xMasterX/web_vacuum/internal/state"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

// maxTextInMemory bounds how much of a document is buffered for link scanning.
// Anything larger is streamed straight to disk and not scanned; a 64 MB "HTML"
// file is not a page worth parsing.
const maxTextInMemory = 64 << 20

// Engine runs one job.
type Engine struct {
	// cfgPtr and rulesPtr are swapped atomically so settings can be changed
	// while workers are running. Each read takes a consistent snapshot; an edit
	// that lands mid-page simply takes effect on the next one.
	cfgPtr   atomic.Pointer[config.Config]
	rulesPtr atomic.Pointer[rules.Engine]
	// reconfigureMu serializes edits so two cannot interleave.
	reconfigureMu sync.Mutex

	mapper *pathmap.Mapper
	store  *state.Store
	client *fetch.Client
	gate   *netwatch.Gate
	robots *robots.Cache
	bus    *bus
	// renderer is nil unless JavaScript rendering is configured.
	renderer *renderer

	frontier *frontier

	slotsMu sync.RWMutex
	slots   []SlotState

	phase atomic.Value // Phase
	// startedAt is written once as the job starts and read continuously by the
	// interfaces, which run on their own goroutines. It is behind an atomic for
	// that reason and not for any other: nothing races to set it.
	startedAt  atomic.Pointer[time.Time]
	pass       atomic.Int64
	stopOnce   sync.Once
	cancel     context.CancelFunc
	stopReason atomic.Value // string

	byteMeter *rateMeter
	fileMeter *rateMeter

	filesDone atomic.Int64
	bytesDone atomic.Int64

	desiredWorkers atomic.Int64

	// localizing serializes link-rewriting passes, which can be started by a
	// timer, a keystroke or the end of the crawl.
	localizing       localizeGuard
	lastLocalizeAt   atomic.Int64
	lastLocalizeCost atomic.Int64

	hostCountMu sync.Mutex
	hostCount   map[string]int64
	health      *hostHealth

	seedQueued map[string]bool

	logFile *os.File
	logMu   sync.Mutex

	urlPatterns []*regexp.Regexp
	subsEarly   []localize.Substitution
	subsLate    []localize.Substitution
}

// New prepares an Engine. The destination and its state directory are created
// here, so a failure to write shows up before anything is downloaded.
func New(cfg *config.Config) (*Engine, error) {
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Destination, 0o755); err != nil {
		return nil, fmt.Errorf("destination: %w", err)
	}
	if err := os.MkdirAll(cfg.WorkDir(), 0o755); err != nil {
		return nil, fmt.Errorf("work dir: %w", err)
	}

	re, err := rules.New(cfg)
	if err != nil {
		return nil, err
	}
	// When exactly one host is being mirrored, its files go straight into the
	// destination folder rather than a redundant nested folder.
	exempt := ""
	if cfg.Paths.FlattenSingleHost {
		if origins := startOrigins(cfg); len(origins) == 1 && len(cfg.Scope.Hosts) == 0 {
			exempt = origins[0]
		}
	}
	mp, err := pathmap.New(&cfg.Paths, exempt)
	if err != nil {
		return nil, err
	}
	st, err := state.Open(cfg.WorkDir())
	if err != nil {
		return nil, err
	}

	gate := netwatch.New(netwatch.Options{
		ProbeURLs: probeURLs(cfg),
		MinWait:   cfg.Resilience.OfflineMinWait.D(),
		MaxWait:   cfg.Resilience.OfflineMaxWait.D(),
		Threshold: 3,
		Timeout:   15 * time.Second,
	})
	cl, err := fetch.New(cfg, gate)
	if err != nil {
		st.Close()
		return nil, err
	}

	e := &Engine{
		mapper:     mp,
		store:      st,
		client:     cl,
		gate:       gate,
		robots:     robots.NewCache(),
		bus:        newBus(cfg.Log.MaxLines),
		frontier:   newFrontier(),
		slots:      make([]SlotState, cfg.General.Connections),
		byteMeter:  newRateMeter(5 * time.Second),
		fileMeter:  newRateMeter(10 * time.Second),
		hostCount:  map[string]int64{},
		health:     newHostHealth(),
		seedQueued: map[string]bool{},
	}
	e.cfgPtr.Store(cfg)
	e.rulesPtr.Store(re)
	e.desiredWorkers.Store(int64(cfg.General.Connections))
	for i := range e.slots {
		e.slots[i].ID = i + 1
	}
	e.phase.Store(PhaseIdle)
	e.stopReason.Store("")

	if err := e.compilePatterns(); err != nil {
		st.Close()
		return nil, err
	}
	if cfg.Render.Enabled() {
		r, err := newRenderer(&cfg.Render)
		if err != nil {
			st.Close()
			return nil, err
		}
		e.renderer = r
	}
	gate.SetOnChange(e.onNetworkChange)

	if cfg.Log.File != "" {
		f, err := os.OpenFile(cfg.Log.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("log file: %w", err)
		}
		e.logFile = f
	}
	return e, nil
}

// startOrigins lists the distinct "host:port" origins of the start URLs.
func startOrigins(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range cfg.StartURLs {
		u, err := urlx.Parse(raw)
		if err != nil {
			continue
		}
		h := strings.ToLower(u.Host)
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

func probeURLs(cfg *config.Config) []string {
	if len(cfg.Resilience.OfflineProbeURLs) > 0 {
		return cfg.Resilience.OfflineProbeURLs
	}
	return netwatch.ProbeTargets(cfg.StartURLs)
}

// Close releases the journal, the browser and the log handles.
func (e *Engine) Close() error {
	var err error
	if e.renderer != nil {
		e.renderer.close()
	}
	if e.store != nil {
		err = e.store.Close()
	}
	if e.logFile != nil {
		e.logFile.Close()
	}
	e.bus.closeAll()
	return err
}

// Subscribe returns a stream of events plus a cancel function.
func (e *Engine) Subscribe(buffer int) (<-chan Event, func()) { return e.bus.Subscribe(buffer) }

// Recent returns buffered history for a UI that attached late.
func (e *Engine) Recent() []Event { return e.bus.Recent() }

// Store exposes the journal for the UI's queue and error views.
func (e *Engine) Store() *state.Store { return e.store }

// Config returns the job's configuration.
func (e *Engine) Config() *config.Config { return e.cfg() }

// cfg returns the settings currently in force.
func (e *Engine) cfg() *config.Config { return e.cfgPtr.Load() }

// rules returns the scope rules currently in force.
func (e *Engine) rules() *rules.Engine { return e.rulesPtr.Load() }

// Phase reports the current stage.
func (e *Engine) Phase() Phase {
	p, _ := e.phase.Load().(Phase)
	return p
}

func (e *Engine) setPhase(p Phase) {
	e.phase.Store(p)
	e.bus.publish(Event{Kind: EventPhase, Phase: p, Message: string(p)})
}

// ---------------------------------------------------------------- control

// Pause holds workers without losing queued work.
func (e *Engine) Pause() {
	// Both halves matter: the frontier stops handing out work, and the client
	// freezes transfers already in progress so the speed falls to zero now.
	e.client.SetPaused(true)
	e.frontier.SetPaused(true)
	e.setPhase(PhasePaused)
	e.logf(LevelInfo, "paused")
}

// Resume releases paused workers.
func (e *Engine) Resume() {
	e.client.SetPaused(false)
	e.frontier.SetPaused(false)
	e.setPhase(PhaseCrawling)
	e.logf(LevelInfo, "resumed")
}

// Paused reports whether the job is held.
func (e *Engine) Paused() bool { return e.frontier.Paused() }

// TogglePause flips the pause state and reports the new value.
func (e *Engine) TogglePause() bool {
	if e.Paused() {
		e.Resume()
		return false
	}
	e.Pause()
	return true
}

// Stop ends the job. Queued work is left in the journal so a later run resumes.
func (e *Engine) Stop(reason string) {
	e.stopOnce.Do(func() {
		e.stopReason.Store(reason)
		e.logf(LevelWarn, "stopping: %s", reason)
		// A paused job must not stay parked once it is being shut down.
		e.client.ReleasePause()
		e.frontier.Close()
		if e.cancel != nil {
			e.cancel()
		}
	})
}

// ---------------------------------------------------------------- run

// Run executes the job: seed, crawl, retry sweeps, then localize. It returns
// nil when the job finished or was stopped cleanly.
func (e *Engine) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	defer cancel()

	if d := e.cfg().Limits.MaxDuration.D(); d > 0 {
		timer := time.AfterFunc(d, func() { e.Stop(fmt.Sprintf("time limit of %s reached", d)) })
		defer timer.Stop()
	}

	started := time.Now()
	e.startedAt.Store(&started)
	e.setPhase(PhaseStarting)
	e.logf(LevelInfo, "destination %s", e.cfg().Destination)

	if err := e.cfg().Save(filepath.Join(e.cfg().WorkDir(), "config.yaml")); err != nil {
		e.logf(LevelWarn, "could not save job config: %v", err)
	}

	seeded := e.seed(ctx)
	if seeded == 0 {
		e.setPhase(PhaseFailed)
		return errors.New("nothing to crawl: every start URL was excluded by the current rules")
	}

	stopCheckpoint := e.startCheckpointer(ctx)
	defer stopCheckpoint()

	stopWatch := e.watchConfig(ctx)
	defer stopWatch()

	stopLocalizeLoop := e.startLocalizeLoop(ctx)
	defer stopLocalizeLoop()

	e.setPhase(PhaseCrawling)
	e.runWorkers(ctx)

	// Retry sweeps pick up anything that failed for a reason a later attempt
	// might survive, which is most of what a flaky network leaves behind.
	for pass := 1; pass <= e.cfg().Resilience.RetryPasses && ctx.Err() == nil; pass++ {
		n := e.requeueFailed(pass)
		if n == 0 {
			break
		}
		e.pass.Store(int64(pass))
		e.setPhase(PhaseRetrying)
		e.logf(LevelInfo, "retry pass %d: re-queued %d failed URLs", pass, n)
		if d := e.cfg().Resilience.RetryPassDelay.D(); d > 0 {
			if err := sleepCtx(ctx, d); err != nil {
				break
			}
		}
		e.runWorkers(ctx)
	}

	if e.cfg().General.FileModification == config.ModifyLocalize {
		// Link rewriting runs even when the job was stopped. Interrupting a
		// download means "stop fetching", not "leave the archive unusable", and
		// skipping this step is the difference between a folder you can browse
		// and a folder of pages whose every link is dead. It is local disk work
		// on a fresh context, so a cancelled crawl still finishes it; a second
		// interrupt exits immediately.
		e.setPhase(PhaseLocalizing)
		if ctx.Err() != nil {
			e.logf(LevelInfo, "stopped — rewriting links in what was downloaded so it browses offline")
		}
		stopLocalizeLoop()
		// Waiting for any pass already running keeps two of them off the same
		// files; the guard is released as soon as that one finishes.
		for !e.localizing.tryStart() {
			time.Sleep(50 * time.Millisecond)
		}
		err := e.Localize(context.WithoutCancel(ctx))
		e.localizing.finish()
		if err != nil {
			e.logf(LevelError, "localize: %v", err)
		}
	}

	e.store.Flush()
	if err := e.store.Checkpoint(); err != nil {
		e.logf(LevelWarn, "checkpoint: %v", err)
	}
	if err := e.writeReport(); err != nil {
		e.logf(LevelWarn, "report: %v", err)
	}

	st := e.store.Stats()
	// Finishing with nothing downloaded and everything failing is not success;
	// saying so plainly beats a "done" that leaves an empty folder.
	if st.Done == 0 && st.Failed > 0 && ctx.Err() == nil {
		e.setPhase(PhaseFailed)
		e.bus.publish(Event{Kind: EventFinished, Phase: PhaseFailed})
		return fmt.Errorf("nothing could be downloaded: all %d request(s) failed — check the address, the scheme (http vs https), and your connection", st.Failed)
	}
	if reason, _ := e.stopReason.Load().(string); reason != "" {
		e.setPhase(PhaseStopped)
		e.logf(LevelWarn, "stopped: %s (%d saved, %d failed, %d queued)",
			reason, st.Done, st.Failed, st.Pending)
	} else if ctx.Err() != nil {
		e.setPhase(PhaseStopped)
	} else {
		e.setPhase(PhaseDone)
		e.logf(LevelInfo, "finished: %d files, %s, %d failed",
			st.Done, humanBytes(st.Bytes), st.Failed)
	}
	if n := e.health.deadCount(); n > 0 {
		e.logf(LevelInfo, "skipped %d unreachable host(s): %s",
			n, strings.Join(e.health.deadHosts(), ", "))
	}
	if e.renderer != nil {
		if rendered, fallbacks, disabled := e.renderer.stats(); rendered > 0 || fallbacks > 0 {
			note := ""
			if disabled {
				note = " (rendering was switched off after repeated failures)"
			}
			e.logf(LevelInfo, "rendered %d page(s) in a browser, %d fell back to plain HTML%s",
				rendered, fallbacks, note)
		}
	}
	e.bus.publish(Event{Kind: EventFinished, Phase: e.Phase()})
	return nil
}

// runWorkers runs the worker pool until the frontier drains.
//
// The pool is resizable while it runs. A supervisor starts workers when the
// connection count is raised; workers retire themselves when it is lowered,
// after finishing whatever they are holding, so changing the setting never
// abandons a download in progress.
//
// Only the supervisor starts workers, and it stops doing so before reporting
// that the pool is finished. That ordering is what keeps a late resize from
// adding a worker to a pool nobody is waiting on any more.
func (e *Engine) runWorkers(ctx context.Context) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	live := 0
	// inUse tracks which display slots are taken. Slot numbers are handed back
	// when a worker retires and the lowest free one is reused, so lowering the
	// connection count and raising it again does not leave a trail of dead rows
	// in the connections pane.
	var inUse []bool

	// takeSlot and freeSlot are called with mu held.
	takeSlot := func() int {
		for i, used := range inUse {
			if !used {
				inUse[i] = true
				return i
			}
		}
		inUse = append(inUse, true)
		return len(inUse) - 1
	}
	freeSlot := func(slot int) {
		if slot >= 0 && slot < len(inUse) {
			inUse[slot] = false
		}
	}

	want := func() int {
		n := int(e.desiredWorkers.Load())
		if n < 1 {
			n = 1
		}
		return n
	}

	spawn := func() {
		mu.Lock()
		slot := takeSlot()
		live++
		mu.Unlock()

		e.ensureSlots(slot + 1)
		wg.Add(1)
		go func() {
			defer wg.Done()

			// A worker retires when the pool is larger than the connection
			// count asks for. Two things decide it, and both are needed.
			//
			// How many go is decided by the number of workers running, not by
			// this worker's slot number. Slot numbers are reused and were never
			// a position in the pool, so comparing one to a count stops meaning
			// anything the moment the pool has been resized twice. Doing that
			// is what used to make a raise after a lowering spawn workers that
			// retired the instant they started, every quarter second, for as
			// long as the job ran.
			//
			// Which ones go is decided by slot order: a worker steps down only
			// once enough workers below it are staying. That keeps the surviving
			// workers on the lowest slots, which is what lets the connections
			// pane shrink — a row can only be dropped from the end, and a pool
			// that retired an arbitrary selection would leave the last row
			// occupied and the pane stuck at its largest size.
			//
			// The count is decremented here, under the lock that owns it and in
			// the same step as the decision, so several workers asking at once
			// cannot each conclude they are the surplus one. The answer is
			// final: a worker told to retire is finished, and is not asked
			// again.
			retiring := false
			stillWanted := func() bool {
				if retiring {
					return false
				}
				mu.Lock()
				defer mu.Unlock()
				n := want()
				if live <= n {
					return true
				}
				below := 0
				for i := 0; i < slot && i < len(inUse); i++ {
					if inUse[i] {
						below++
					}
				}
				if below < n {
					return true
				}
				live--
				retiring = true
				return false
			}

			e.worker(ctx, slot, stillWanted)

			mu.Lock()
			if !retiring {
				live--
			}
			freeSlot(slot)
			mu.Unlock()
			e.endSlot(slot)
		}()
	}

	supervised := make(chan struct{})
	go func() {
		defer close(supervised)
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()

		for i := 0; i < want(); i++ {
			spawn()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n := want()
				mu.Lock()
				running := live
				mu.Unlock()

				// Every worker having exited means the frontier drained, since
				// that is the only thing that ends a worker that is still
				// wanted. The pool is finished.
				if running == 0 {
					return
				}
				e.trimSlots(n)
				for i := running; i < n; i++ {
					spawn()
				}
			}
		}
	}()

	<-supervised
	wg.Wait()
}

// worker takes items off the frontier until the queue drains or the pool is
// asked to shrink. stillWanted answers the second question and is asked both
// between items and while parked waiting for one.
func (e *Engine) worker(ctx context.Context, slot int, stillWanted func() bool) {
	for {
		if !stillWanted() {
			return
		}
		item, ok := e.frontier.Pop(ctx, stillWanted)
		if !ok {
			return
		}
		e.process(ctx, slot, item)
		e.frontier.Done()
	}
}

// startCheckpointer periodically makes the journal durable so a power cut
// costs seconds of progress rather than the whole crawl.
func (e *Engine) startCheckpointer(ctx context.Context) func() {
	every := e.cfg().Resilience.CheckpointEvery.D()
	if every <= 0 {
		every = 5 * time.Second
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		compactAt := 0
		for {
			select {
			case <-t.C:
				if err := e.store.Flush(); err != nil {
					e.logf(LevelWarn, "state flush: %v", err)
				}
				// Compact occasionally so the journal stays proportional to the
				// number of URLs rather than the number of status changes.
				compactAt++
				if compactAt%60 == 0 {
					if err := e.store.Checkpoint(); err != nil {
						e.logf(LevelWarn, "state checkpoint: %v", err)
					}
				}
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// ---------------------------------------------------------------- seeding

// seed queues the start URLs plus anything left pending from a previous run.
func (e *Engine) seed(ctx context.Context) int {
	n := 0
	// The start URLs are identified up front because resume pushes whatever was
	// still pending before enqueueStart gets a turn, and a start URL left
	// pending by the previous run comes back through that path. It has to carry
	// the seed exemption either way.
	starts := e.startKeys()
	if e.cfg().Resilience.ResumeOnStart {
		for _, entry := range e.store.Select(func(en *state.Entry) bool {
			return en.Status == state.Pending || en.Status == state.Active
		}) {
			u, err := urlx.Parse(entry.URL)
			if err != nil {
				continue
			}
			role := rules.RolePage
			if entry.Role == "asset" {
				role = rules.RoleAsset
			}
			if e.seedQueued[entry.Key] {
				continue
			}
			e.seedQueued[entry.Key] = true
			if e.frontier.Push(&Item{
				Key: entry.Key, URL: u, Depth: entry.Depth,
				Role: role, Referer: entry.Referer, Seed: starts[entry.Key],
			}) {
				n++
			}
		}
		if n > 0 {
			e.logf(LevelInfo, "resuming: %d URLs still queued from the last run", n)
		}

		// Failures from an earlier run get another chance. A new run is a new
		// opportunity — the server may have recovered, the network may be back
		// — and without this a URL that failed once stays failed forever, since
		// it is no longer pending and the in-run retry passes have already
		// counted it. Permanent failures such as 404 are left alone.
		retried := 0
		for _, entry := range e.store.Select(func(en *state.Entry) bool {
			return en.Status == state.Failed && retryableEntry(en)
		}) {
			if e.seedQueued[entry.Key] {
				continue
			}
			u, err := urlx.Parse(entry.URL)
			if err != nil {
				continue
			}
			role := rules.RolePage
			if entry.Role == "asset" {
				role = rules.RoleAsset
			}
			en := entry
			en.Status = state.Pending
			en.Pass = 0
			e.store.Put(&en)
			e.seedQueued[entry.Key] = true
			if e.frontier.Push(&Item{
				Key: entry.Key, URL: u, Depth: entry.Depth,
				Role: role, Referer: entry.Referer, Seed: starts[entry.Key],
			}) {
				retried++
				n++
			}
		}
		if retried > 0 {
			e.logf(LevelInfo, "retrying %d URL(s) that failed in an earlier run", retried)
		}
	}

	for _, raw := range e.cfg().StartURLs {
		u, err := urlx.Parse(raw)
		if err != nil {
			e.logf(LevelError, "bad start URL %q: %v", raw, err)
			continue
		}
		if e.enqueueStart(u) {
			n++
		}
	}

	if e.cfg().Webpage.UseSitemap {
		n += e.seedSitemaps(ctx)
	}
	return n
}

// seedSitemaps queues URLs advertised by robots.txt and /sitemap.xml. Pages
// that nothing links to are otherwise invisible to a link crawler.
func (e *Engine) seedSitemaps(ctx context.Context) int {
	n := 0
	seen := map[string]bool{}
	var queue []string

	for _, raw := range e.cfg().StartURLs {
		u, err := urlx.Parse(raw)
		if err != nil {
			continue
		}
		r := e.robotsFor(ctx, u)
		for _, sm := range r.Sitemaps {
			if !seen[sm] {
				seen[sm] = true
				queue = append(queue, sm)
			}
		}
		def := u.Scheme + "://" + u.Host + "/sitemap.xml"
		if !seen[def] {
			seen[def] = true
			queue = append(queue, def)
		}
	}

	// A sitemap index points at more sitemaps; a couple of levels is plenty.
	for depth := 0; depth < 3 && len(queue) > 0; depth++ {
		var next []string
		for _, sm := range queue {
			if ctx.Err() != nil {
				return n
			}
			body, err := e.fetchBytes(ctx, sm, 8<<20)
			if err != nil {
				continue
			}
			for _, entry := range robots.ParseSitemap(body) {
				if entry.IsIndex {
					if !seen[entry.Loc] {
						seen[entry.Loc] = true
						next = append(next, entry.Loc)
					}
					continue
				}
				u, err := urlx.Parse(entry.Loc)
				if err != nil {
					continue
				}
				if e.enqueue(u, 0, rules.RolePage, "", nil, false) {
					n++
				}
			}
		}
		queue = next
	}
	if n > 0 {
		e.logf(LevelInfo, "sitemap: queued %d URLs", n)
	}
	return n
}

// fetchBytes retrieves a small auxiliary document (robots.txt, a sitemap).
func (e *Engine) fetchBytes(ctx context.Context, raw string, limit int64) ([]byte, error) {
	u, err := urlx.Parse(raw)
	if err != nil {
		return nil, err
	}
	resp, err := e.client.Do(ctx, fetch.Request{URL: u})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// ---------------------------------------------------------------- enqueue

// enqueue applies the scope rules and adds a URL to the frontier. force skips
// the rules, which is only used for the start URLs themselves.
func (e *Engine) enqueue(u *url.URL, depth int, role rules.Role, referer string, from *url.URL, force bool) bool {
	u = e.rules().NormalizeForQueue(u)
	key := urlx.Key(u)

	if e.store.Has(key) {
		return false
	}
	if !force {
		if d := e.rules().CheckURL(u, role, depth, from); !d.Allow {
			e.store.Put(&state.Entry{
				Key: key, URL: u.String(), Status: state.Skipped,
				Depth: depth, Role: role.String(), Err: d.Reason,
			})
			e.bus.publish(Event{Kind: EventSkipped, URL: u.String(), Message: d.Reason, Depth: depth})
			return false
		}
	}
	if !e.withinFileLimit() {
		return false
	}
	if dead, reason := e.health.dead(hostOf(u)); dead {
		e.store.Put(&state.Entry{
			Key: key, URL: u.String(), Status: state.Skipped,
			Depth: depth, Role: role.String(), Err: reason,
		})
		return false
	}

	// Claiming the key and queueing it are one step. The Has check at the top
	// of this function is an early exit, not the guarantee: two workers scanning
	// two pages that link to the same URL reach it at the same moment, both find
	// nothing, and without an atomic claim both would queue it and both would
	// download it over the top of each other.
	won, err := e.store.Claim(&state.Entry{
		Key: key, URL: u.String(), Status: state.Pending,
		Depth: depth, Role: role.String(), Referer: referer,
	})
	if err != nil {
		e.logf(LevelWarn, "state: %v", err)
	}
	if !won {
		return false
	}
	return e.frontier.Push(&Item{
		Key: key, URL: u, Depth: depth, Role: role, Referer: referer, From: from,
	})
}

// startKeys is the set of queue keys belonging to the configured start URLs.
func (e *Engine) startKeys() map[string]bool {
	keys := make(map[string]bool, len(e.cfg().StartURLs))
	for _, raw := range e.cfg().StartURLs {
		if u, err := urlx.Parse(raw); err == nil {
			keys[urlx.Key(e.rules().NormalizeForQueue(u))] = true
		}
	}
	return keys
}

// enqueueStart queues a start URL unconditionally. A start URL a previous run
// already downloaded must still enter the queue: whether it gets re-fetched is
// the file-replacement policy's call, not the dedup index's. Without this, the
// second run of any finished job reports "nothing to crawl".
func (e *Engine) enqueueStart(u *url.URL) bool {
	u = e.rules().NormalizeForQueue(u)
	key := urlx.Key(u)
	if e.seedQueued[key] {
		return false
	}
	e.seedQueued[key] = true

	entry, ok := e.store.Get(key)
	if !ok {
		entry = state.Entry{Key: key, URL: u.String(), Role: rules.RolePage.String()}
	}
	entry.URL = u.String()
	if entry.Status != state.Done {
		entry.Status = state.Pending
	}
	if err := e.store.Put(&entry); err != nil {
		e.logf(LevelWarn, "state: %v", err)
	}
	return e.frontier.Push(&Item{Key: key, URL: u, Depth: 0, Role: rules.RolePage, Seed: true})
}

// requeueFailed puts retryable failures back in the queue for a later sweep.
func (e *Engine) requeueFailed(pass int) int {
	n := 0
	starts := e.startKeys()
	for _, entry := range e.store.Select(func(en *state.Entry) bool {
		return en.Status == state.Failed && en.Pass < pass && retryableEntry(en)
	}) {
		u, err := urlx.Parse(entry.URL)
		if err != nil {
			continue
		}
		role := rules.RolePage
		if entry.Role == "asset" {
			role = rules.RoleAsset
		}
		en := entry
		en.Status = state.Pending
		en.Pass = pass
		e.store.Put(&en)
		if e.frontier.Push(&Item{
			Key: entry.Key, URL: u, Depth: entry.Depth,
			Role: role, Referer: entry.Referer, Pass: pass, Seed: starts[entry.Key],
		}) {
			n++
		}
	}
	return n
}

// retryableEntry decides whether a later sweep could plausibly succeed. The
// status code matters as much as the kind: re-requesting a 404 a thousand times
// only wastes the crawl's remaining time, while a 503 usually clears.
func retryableEntry(en *state.Entry) bool {
	switch en.ErrKind {
	case fetch.KindTransport.String(), "":
		return true
	case fetch.KindHTTP.String():
		switch {
		case en.HTTPCode == 0:
			return true
		case en.HTTPCode == http.StatusRequestTimeout,
			en.HTTPCode == http.StatusTooManyRequests:
			return true
		case en.HTTPCode >= 500:
			return true
		}
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

func hostOf(u *url.URL) string {
	if u == nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
