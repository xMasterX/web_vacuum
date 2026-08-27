package crawl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/localize"
	"github.com/xMasterX/web_vacuum/internal/netwatch"
	"github.com/xMasterX/web_vacuum/internal/pathmap"
	"github.com/xMasterX/web_vacuum/internal/rules"
	"github.com/xMasterX/web_vacuum/internal/state"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

// compilePatterns turns the user's regex settings into compiled form once, at
// start-up, so a bad pattern is reported before anything downloads.
func (e *Engine) compilePatterns() error {
	for _, p := range e.cfg().Webpage.URLExtraction {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("webpage.url_extraction %q: %w", p, err)
		}
		e.urlPatterns = append(e.urlPatterns, re)
	}
	var err error
	if e.subsEarly, err = compileSubs(e.cfg().Webpage.EarlySubstitutions); err != nil {
		return fmt.Errorf("webpage.early_substitutions: %w", err)
	}
	if e.subsLate, err = compileSubs(e.cfg().Webpage.LateSubstitutions); err != nil {
		return fmt.Errorf("webpage.late_substitutions: %w", err)
	}
	return nil
}

func compileSubs(in []config.Substitution) ([]localize.Substitution, error) {
	out := make([]localize.Substitution, 0, len(in))
	for _, s := range in {
		if s.Literal {
			out = append(out, localize.Substitution{Literal: s.Pattern, Repl: s.Replacement})
			continue
		}
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", s.Pattern, err)
		}
		out = append(out, localize.Substitution{Re: re, Repl: s.Replacement})
	}
	return out, nil
}

// onNetworkChange turns gate transitions into events and phase changes, so both
// UIs show "waiting for network" rather than appearing to hang.
func (e *Engine) onNetworkChange(st netwatch.Status) {
	if st.State == netwatch.Offline {
		e.setPhase(PhaseOffline)
		msg := "network unreachable, waiting to retry"
		if st.LastError != "" {
			msg = fmt.Sprintf("network unreachable (%s), waiting to retry", st.LastError)
		}
		e.bus.publish(Event{Kind: EventNetwork, Level: LevelWarn, Message: msg, Phase: PhaseOffline})
		e.logf(LevelWarn, "%s", msg)
		return
	}
	if e.Paused() {
		e.setPhase(PhasePaused)
	} else {
		e.setPhase(PhaseCrawling)
	}
	msg := fmt.Sprintf("network is back after %s, resuming", st.TotalDowntime.Round(time.Second))
	e.bus.publish(Event{Kind: EventNetwork, Level: LevelInfo, Message: msg, Phase: PhaseCrawling})
	e.logf(LevelInfo, "%s", msg)
}

// ---------------------------------------------------------------- logging

var levelRank = map[Level]int{LevelDebug: 0, LevelInfo: 1, LevelWarn: 2, LevelError: 3}

func (e *Engine) logf(level Level, format string, args ...any) {
	min := levelRank[Level(strings.ToLower(e.cfg().Log.Level))]
	if levelRank[level] < min {
		return
	}
	msg := fmt.Sprintf(format, args...)
	e.bus.publish(Event{Kind: EventLog, Level: level, Message: msg})

	if e.logFile != nil {
		e.logMu.Lock()
		fmt.Fprintf(e.logFile, "%s %-5s %s\n", time.Now().Format(time.RFC3339), strings.ToUpper(string(level)), msg)
		e.logMu.Unlock()
	}
}

// ---------------------------------------------------------------- snapshot

// Snapshot builds the picture both UIs render from.
// started reports when the job began, or the zero time before it has.
func (e *Engine) started() time.Time {
	if t := e.startedAt.Load(); t != nil {
		return *t
	}
	return time.Time{}
}

func (e *Engine) Snapshot() Snapshot {
	st := e.store.Stats()
	fs := e.client.Stats()

	e.slotsMu.RLock()
	slots := make([]SlotState, len(e.slots))
	copy(slots, e.slots)
	e.slotsMu.RUnlock()

	started := e.started()
	elapsed := time.Duration(0)
	if !started.IsZero() {
		elapsed = time.Since(started)
	}
	bps := e.byteMeter.Update(fs.BytesIn)
	fps := e.fileMeter.Update(int64(st.Done))

	// A paused job transfers nothing, so reporting the tail of a smoothed
	// average would show a speed that is not happening. The meter is
	// re-baselined at the same time so resuming reads from zero rather than
	// ramping down from a stale figure.
	if e.frontier.Paused() {
		e.byteMeter.Reset(fs.BytesIn)
		bps = 0
	}

	snap := Snapshot{
		Name:        e.cfg().Name,
		Destination: e.cfg().Destination,
		Phase:       e.Phase(),
		Stats:       st,
		Fetch:       fs,
		Network:     e.gate.Status(),
		Slots:       slots,
		Queued:      e.frontier.Len(),
		InFlight:    e.frontier.InFlight(),
		Elapsed:     elapsed,
		BytesPerSec: bps,
		FilesPerSec: fps,
		Pass:        int(e.pass.Load()),
		StartedAt:   started,
		MaxFiles:    e.cfg().Limits.MaxFiles,
		MaxBytes:    e.cfg().Limits.MaxBytes.V(),
		RateLimit:   e.client.RateLimit(),
		Paused:      e.frontier.Paused(),
	}
	// The estimate is deliberately based on the queue rather than a guess at
	// total site size, because the total is unknowable until the crawl ends.
	remaining := st.Pending + st.Active
	if fps > 0.01 && remaining > 0 {
		snap.ETA = time.Duration(float64(remaining) / fps * float64(time.Second))
	}
	return snap
}

// ---------------------------------------------------------------- speed

// speedLadder is the set of throughput caps the F7/F8 keys step through, from
// unlimited down to a rate that leaves a home connection usable.
var speedLadder = []int64{
	0, // unlimited — where every job starts
	5 << 20,
	2 << 20,
	1 << 20,
	512 << 10,
	256 << 10,
	128 << 10,
	64 << 10,
	32 << 10,
}

// AdjustRate moves the throughput cap one step (dir > 0 faster, dir < 0 slower)
// and returns a message describing the new setting.
func (e *Engine) AdjustRate(dir int) string {
	cur := e.client.RateLimit()
	idx := 0
	for i, v := range speedLadder {
		if v == cur {
			idx = i
			break
		}
	}
	// The ladder runs fastest-first, so "slower" means moving down it.
	if dir < 0 {
		idx++
	} else {
		idx--
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(speedLadder) {
		idx = len(speedLadder) - 1
	}
	next := speedLadder[idx]
	e.client.SetRateLimit(next)
	if next == 0 {
		e.logf(LevelInfo, "speed limit removed")
		return "speed: unlimited"
	}
	msg := "speed limit " + humanBytes(next) + "/s"
	e.logf(LevelInfo, "%s", msg)
	return "speed: " + humanBytes(next) + "/s"
}

// ---------------------------------------------------------------- localize

// localResolver adapts the journal to the localize package's interfaces.
type localResolver struct {
	st    *state.Store
	rules *rules.Engine
}

// LocalPath finds the file a link points at.
//
// Lookup has to apply the same normalization the crawler applied when it queued
// the URL, or the two never meet. A forum that puts a session id in every link
// is the case that makes this obvious: with those parameters stripped, the page
// is stored under the cleaned key, while the link in the markup still carries
// the full query. Matching only on the raw URL misses every one of them, and
// each miss silently becomes a link back to the live site — a local copy whose
// links all leave.
func (r localResolver) LocalPath(u *url.URL) (string, bool) {
	if r.rules != nil {
		if p, ok := r.st.PathFor(urlx.Key(r.rules.NormalizeForQueue(u))); ok {
			return p, true
		}
	}
	if p, ok := r.st.PathFor(urlx.Key(u)); ok {
		return p, true
	}
	return r.st.PathFor(u.String())
}

func (r localResolver) HasLocal(rel string) bool { return r.st.HasPath(rel) }

// Localize rewrites every downloaded HTML and CSS file so the mirror browses
// offline. It runs after the crawl because a link can only be pointed at a
// local file once that file's final name is known.
func (e *Engine) Localize(ctx context.Context) error {
	// Every document is reconsidered, not only ones never rewritten before.
	// The set of files on disk grows with each run, so a page localized during
	// an earlier pass still points at the live site for everything that had not
	// been downloaded yet. Rewriting is idempotent, so re-running it only ever
	// upgrades those links to local ones.
	var pending int
	targets := e.store.Select(func(en *state.Entry) bool {
		if en.Status != state.Done || en.Path == "" {
			return false
		}
		if !e.rules().IsHTML(en.MediaType) && !e.rules().IsCSS(en.MediaType) {
			return false
		}
		if !en.Localized {
			pending++
		}
		return true
	})
	if len(targets) == 0 {
		e.logf(LevelInfo, "nothing to localize")
		return nil
	}
	// A resumed run that downloaded nothing new has nothing to improve.
	if pending == 0 && e.filesDone.Load() == 0 {
		e.logf(LevelInfo, "links are already up to date")
		return nil
	}
	e.logf(LevelInfo, "localizing %d documents", len(targets))

	res := localResolver{st: e.store, rules: e.rules()}
	opt := localize.Options{
		Options:          e.parseOptions(),
		StripScripts:     e.cfg().Webpage.StripScripts,
		InjectBanner:     e.cfg().Webpage.InjectBanner,
		BannerText:       "Offline archive by Web Vacuum",
		AbsoluteFallback: true,
		EarlySubs:        e.subsEarly,
		LateSubs:         e.subsLate,
	}

	workers := e.cfg().General.Connections
	if workers > 8 {
		workers = 8
	}
	jobs := make(chan state.Entry)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var rewritten, failed, localLinks, offsiteLinks int
	// unresolved counts, per host, the links that had to stay pointing at the
	// live site. When a mirror browses badly this is the number that explains
	// why, so it is reported rather than left for someone to guess at.
	unresolved := map[string]int{}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for en := range jobs {
				if ctx.Err() != nil {
					return
				}
				stats, err := e.localizeOne(en, res, opt)
				if err != nil {
					mu.Lock()
					failed++
					mu.Unlock()
					e.logf(LevelWarn, "localize %s: %v", en.Path, err)
					continue
				}
				mu.Lock()
				rewritten++
				localLinks += stats.local
				offsiteLinks += stats.offsite
				for host, n := range stats.hosts {
					unresolved[host] += n
				}
				mu.Unlock()
			}
		}()
	}
	for _, en := range targets {
		if ctx.Err() != nil {
			break
		}
		jobs <- en
	}
	close(jobs)
	wg.Wait()

	e.logf(LevelInfo, "localized %d documents: %d links now point at local files, %d still point at the site",
		rewritten, localLinks, offsiteLinks)
	if failed > 0 {
		e.logf(LevelWarn, "%d documents could not be rewritten", failed)
	}
	if len(unresolved) > 0 {
		// Naming the hosts turns "some links are wrong" into something
		// actionable: either they were never downloaded, or they belong to a
		// domain the scope excluded.
		type hostCount struct {
			host string
			n    int
		}
		top := make([]hostCount, 0, len(unresolved))
		for h, n := range unresolved {
			top = append(top, hostCount{h, n})
		}
		sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n })
		if len(top) > 5 {
			top = top[:5]
		}
		parts := make([]string, 0, len(top))
		for _, t := range top {
			parts = append(parts, fmt.Sprintf("%s (%d)", t.host, t.n))
		}
		e.logf(LevelInfo, "links left pointing at: %s — these were not downloaded",
			strings.Join(parts, ", "))
	}
	return nil
}

// localizeStats is what one document's rewrite reports back.
type localizeStats struct {
	local   int
	offsite int
	hosts   map[string]int
}

func (e *Engine) localizeOne(en state.Entry, res localize.Resolver, opt localize.Options) (localizeStats, error) {
	var stats localizeStats
	abs, err := pathmap.Local(e.cfg().Destination, en.Path)
	if err != nil {
		return stats, err
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return stats, err
	}
	docURL, err := urlx.Parse(en.URL)
	if err != nil {
		return stats, err
	}

	// The untouched original is kept when asked for, so a rewrite can be undone
	// or re-run against a later, more complete crawl.
	if e.cfg().General.KeepOriginals {
		orig := filepath.Join(e.cfg().WorkDir(), "originals", filepath.FromSlash(en.Path))
		if _, statErr := os.Stat(orig); os.IsNotExist(statErr) {
			if mkErr := os.MkdirAll(filepath.Dir(orig), 0o755); mkErr == nil {
				os.WriteFile(orig, src, 0o644)
			}
		}
	}

	var result *localize.Result
	if e.rules().IsCSS(en.MediaType) {
		result, err = localize.CSS(src, docURL, en.Path, res, opt)
	} else {
		result, err = localize.HTML(src, docURL, en.Path, res, opt)
	}
	if err != nil {
		return stats, err
	}
	if result.Changed {
		if err := writeFileAtomic(abs, result.Content); err != nil {
			return stats, err
		}
	}
	stats.local = result.Rewritten
	stats.offsite = result.Absolutized
	stats.hosts = result.UnresolvedHosts

	en.Localized = true
	return stats, e.store.Put(&en)
}

// ---------------------------------------------------------------- report

// Report is the machine-readable summary written beside the mirror.
type Report struct {
	Name        string        `json:"name"`
	StartURLs   []string      `json:"start_urls"`
	Destination string        `json:"destination"`
	StartedAt   time.Time     `json:"started_at"`
	FinishedAt  time.Time     `json:"finished_at"`
	Duration    string        `json:"duration"`
	Phase       Phase         `json:"phase"`
	StopReason  string        `json:"stop_reason,omitempty"`
	Stats       state.Stats   `json:"stats"`
	Outages     int           `json:"network_outages"`
	Downtime    string        `json:"network_downtime"`
	TopErrors   []ErrorBucket `json:"top_errors,omitempty"`
	Entry       string        `json:"entry_point,omitempty"`
}

// ErrorBucket groups identical failures for the summary.
type ErrorBucket struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
	Sample string `json:"sample_url"`
}

// writeReport saves report.json and an index.html shortcut into the state
// folder, so a finished download explains itself without the tool present.
func (e *Engine) writeReport() error {
	net := e.gate.Status()
	reason, _ := e.stopReason.Load().(string)

	buckets := map[string]*ErrorBucket{}
	for _, en := range e.store.Select(func(en *state.Entry) bool { return en.Status == state.Failed }) {
		key := en.ErrKind + ": " + shortReason(en.Err)
		b, ok := buckets[key]
		if !ok {
			b = &ErrorBucket{Reason: key, Sample: en.URL}
			buckets[key] = b
		}
		b.Count++
	}
	top := make([]ErrorBucket, 0, len(buckets))
	for _, b := range buckets {
		top = append(top, *b)
	}
	sort.Slice(top, func(i, j int) bool { return top[i].Count > top[j].Count })
	if len(top) > 10 {
		top = top[:10]
	}

	started := e.started()
	rep := Report{
		Name:        e.cfg().Name,
		StartURLs:   e.cfg().StartURLs,
		Destination: e.cfg().Destination,
		StartedAt:   started,
		FinishedAt:  time.Now(),
		Duration:    time.Since(started).Round(time.Second).String(),
		Phase:       e.Phase(),
		StopReason:  reason,
		Stats:       e.store.Stats(),
		Outages:     net.TotalOutages,
		Downtime:    net.TotalDowntime.Round(time.Second).String(),
		TopErrors:   top,
		Entry:       e.EntryPoint(),
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.cfg().WorkDir(), "report.json"), data, 0o644)
}

func shortReason(s string) string {
	if i := strings.Index(s, ": "); i >= 0 && i < len(s)-2 {
		s = s[i+2:]
	}
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return s
}

// EntryPoint returns the destination-relative path of the first start URL's
// saved file, which is the file a user should open to browse the mirror.
func (e *Engine) EntryPoint() string {
	for _, raw := range e.cfg().StartURLs {
		u, err := urlx.Parse(raw)
		if err != nil {
			continue
		}
		if p, ok := e.store.PathFor(urlx.Key(u)); ok {
			return p
		}
	}
	return ""
}
