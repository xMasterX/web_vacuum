package crawl

import (
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/rules"
)

// same compares two settings sections by their serialized form rather than
// structurally.
//
// A struct comparison would call a nil slice and an empty slice different, and
// settings routinely make that round trip on their way through YAML or JSON.
// That produces a change report full of things nobody changed, which is worse
// than no report at all.
func same(a, b any) bool {
	ya, err1 := yaml.Marshal(a)
	yb, err2 := yaml.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ya) == string(yb)
}

// ReconfigureResult reports what a settings change actually did.
//
// Being explicit matters: some settings cannot take effect on a job already
// underway, and silently ignoring them would leave someone believing they had
// changed something they had not.
type ReconfigureResult struct {
	// Applied names the settings that changed and are now in force.
	Applied []string
	// Ignored names settings that differ but cannot change on a running job,
	// with the reason.
	Ignored []string
}

// Changed reports whether anything took effect.
func (r ReconfigureResult) Changed() bool { return len(r.Applied) > 0 }

// Summary is a one-line description for the log and the UI.
func (r ReconfigureResult) Summary() string {
	switch {
	case len(r.Applied) == 0 && len(r.Ignored) == 0:
		return "no settings changed"
	case len(r.Applied) == 0:
		return "nothing could be changed while running: " + strings.Join(r.Ignored, "; ")
	case len(r.Ignored) == 0:
		return "applied " + strings.Join(r.Applied, ", ")
	default:
		return "applied " + strings.Join(r.Applied, ", ") +
			" — ignored " + strings.Join(r.Ignored, "; ")
	}
}

// fixedSettings are the ones that decide where files land. Changing them on a
// running job would send new files somewhere else while everything already
// downloaded stayed put, leaving a mirror that is half one layout and half
// another and whose links cannot all be right.
func fixedSettings(old, next *config.Config) []string {
	var ignored []string
	if next.Destination != old.Destination {
		ignored = append(ignored, "destination (start a new job to change where files go)")
	}
	if !same(next.StartURLs, old.StartURLs) {
		ignored = append(ignored, "start URLs (start a new job to change them)")
	}
	if !same(next.Paths, old.Paths) {
		ignored = append(ignored, "path layout (it would relocate files already downloaded)")
	}
	if next.Request.Proxy != old.Request.Proxy {
		ignored = append(ignored, "proxy (it is bound to the open connections)")
	}
	if next.Request.CookieJarFile != old.Request.CookieJarFile {
		ignored = append(ignored, "cookie jar file (already loaded)")
	}
	return ignored
}

// Reconfigure adopts new settings on a running job.
//
// Everything that can safely change does so immediately: how many connections
// to use, how fast to go, which URLs are in scope, what the limits are. What
// cannot change is reported rather than quietly dropped.
func (e *Engine) Reconfigure(next *config.Config) (ReconfigureResult, error) {
	e.reconfigureMu.Lock()
	defer e.reconfigureMu.Unlock()

	var res ReconfigureResult
	old := e.cfg()

	// Work on a copy so a rejected change cannot leave the caller's struct half
	// applied, and normalize it so the same validation a fresh job gets applies.
	candidate := next.Clone()
	candidate.Destination = old.Destination
	candidate.StartURLs = append([]string(nil), old.StartURLs...)
	candidate.Paths = old.Paths
	candidate.Request.Proxy = old.Request.Proxy
	candidate.Request.CookieJarFile = old.Request.CookieJarFile
	if err := candidate.Normalize(); err != nil {
		return res, err
	}
	res.Ignored = fixedSettings(old, next)

	// Scope rules are rebuilt before anything is swapped in, so a bad pattern
	// leaves the running job untouched.
	newRules, err := rules.New(candidate)
	if err != nil {
		return res, fmt.Errorf("scope rules: %w", err)
	}

	if !same(candidate.Scope, old.Scope) {
		res.Applied = append(res.Applied, "scope")
	}
	if !same(candidate.Types, old.Types) {
		res.Applied = append(res.Applied, "file types")
	}
	if !same(candidate.Limits, old.Limits) {
		res.Applied = append(res.Applied, "limits")
	}
	if candidate.General.Connections != old.General.Connections {
		res.Applied = append(res.Applied,
			fmt.Sprintf("connections %d→%d", old.General.Connections, candidate.General.Connections))
	}
	if candidate.Request.PerHostConns != old.Request.PerHostConns {
		res.Applied = append(res.Applied,
			fmt.Sprintf("per-host %d→%d", old.Request.PerHostConns, candidate.Request.PerHostConns))
	}
	if candidate.Request.Delay != old.Request.Delay {
		res.Applied = append(res.Applied, "delay "+candidate.Request.Delay.String())
	}
	if candidate.Limits.MaxRate != old.Limits.MaxRate {
		rate := "unlimited"
		if candidate.Limits.MaxRate > 0 {
			rate = candidate.Limits.MaxRate.String() + "/s"
		}
		res.Applied = append(res.Applied, "speed "+rate)
	}
	if candidate.Request.UserAgent != old.Request.UserAgent {
		res.Applied = append(res.Applied, "user agent")
	}
	if candidate.Request.Timeout != old.Request.Timeout {
		res.Applied = append(res.Applied, "timeout "+candidate.Request.Timeout.String())
	}
	if candidate.Request.Attempts != old.Request.Attempts {
		res.Applied = append(res.Applied, fmt.Sprintf("attempts %d", candidate.Request.Attempts))
	}
	if candidate.Request.OffsiteTimeout != old.Request.OffsiteTimeout {
		res.Applied = append(res.Applied, "off-site timeout "+candidate.Request.OffsiteTimeout.String())
	}
	if candidate.General.IgnoreRobots != old.General.IgnoreRobots {
		res.Applied = append(res.Applied, "robots.txt handling")
	}
	if candidate.General.SupportingFiles != old.General.SupportingFiles {
		res.Applied = append(res.Applied, "supporting files "+string(candidate.General.SupportingFiles))
	}
	if candidate.General.FileReplacement != old.General.FileReplacement {
		res.Applied = append(res.Applied, "file replacement "+string(candidate.General.FileReplacement))
	}
	if !same(candidate.Webpage, old.Webpage) {
		res.Applied = append(res.Applied, "page scanning")
	}
	if !same(candidate.Render, old.Render) {
		res.Applied = append(res.Applied, "rendering")
		res.Ignored = append(res.Ignored,
			"a browser already started keeps its launch options until the job ends")
	}
	if candidate.Log.Level != old.Log.Level {
		res.Applied = append(res.Applied, "log level "+candidate.Log.Level)
	}

	// Whether to apply is decided by comparing the whole configuration, not by
	// the list of labels above. That list exists to describe the change in
	// words, and a hand-written list of fields will always eventually miss one;
	// when it does, the change must still take effect rather than being
	// silently dropped for want of a label.
	if same(candidate, old) {
		res.Applied = nil
		return res, nil
	}
	if len(res.Applied) == 0 {
		res.Applied = append(res.Applied, "settings")
	}

	// Everything below this point succeeds; the validation that could fail is
	// already done.
	e.client.Reconfigure(candidate)
	e.rulesPtr.Store(newRules)
	e.cfgPtr.Store(candidate)
	e.setWorkerCount(candidate.General.Connections)

	if err := candidate.Save(filepath.Join(candidate.WorkDir(), "config.yaml")); err != nil {
		e.logf(LevelWarn, "could not save the changed settings: %v", err)
	}
	e.logf(LevelInfo, "settings changed: %s", res.Summary())
	e.bus.publish(Event{Kind: EventPhase, Level: LevelInfo, Message: "settings changed: " + res.Summary()})
	return res, nil
}

// SetConnections is the shortcut the interfaces use for the one setting people
// reach for most while watching a download run.
func (e *Engine) SetConnections(n int) int {
	if n < 1 {
		n = 1
	}
	if n > config.MaxConnections {
		n = config.MaxConnections
	}
	next := e.cfg().Clone()
	next.General.Connections = n
	// Per-host stays at or below the total, or the number would not mean what
	// it says.
	if next.Request.PerHostConns > n {
		next.Request.PerHostConns = n
	}
	if _, err := e.Reconfigure(next); err != nil {
		e.logf(LevelWarn, "could not change connections: %v", err)
		return e.cfg().General.Connections
	}
	return n
}

// setWorkerCount changes how many workers should be running. Workers above the
// new count retire once they finish what they are holding, so a reduction never
// abandons a download in progress.
func (e *Engine) setWorkerCount(n int) {
	if n < 1 {
		n = 1
	}
	e.desiredWorkers.Store(int64(n))
	e.ensureSlots(n)
	// Waking the frontier lets workers that are parked waiting for work notice
	// they are no longer wanted.
	e.frontier.Wake()
}

// ensureSlots grows the display slots to cover a raised connection count.
func (e *Engine) ensureSlots(n int) {
	if n > config.MaxConnections {
		n = config.MaxConnections
	}
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	for len(e.slots) < n {
		e.slots = append(e.slots, SlotState{ID: len(e.slots) + 1})
	}
}

// trimSlots drops display rows left behind by a higher connection count, so the
// connections pane shows the number of connections actually asked for rather
// than the largest number ever asked for.
//
// Only idle rows at the end go. Workers are given the lowest free slot, so the
// busy ones cluster at the front and a row still showing a transfer is never
// removed from under it.
func (e *Engine) trimSlots(n int) {
	if n < 1 {
		n = 1
	}
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	for len(e.slots) > n && !e.slots[len(e.slots)-1].Busy {
		e.slots = e.slots[:len(e.slots)-1]
	}
}
