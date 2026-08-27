package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/crawl"
	"github.com/xMasterX/web_vacuum/internal/netwatch"
	"github.com/xMasterX/web_vacuum/internal/state"
)

// headerHeight is the number of rows the fixed header occupies.
const headerHeight = 7

// Colours are adaptive so the interface stays readable on light terminals,
// which most terminal dashboards get wrong.
var (
	cAccent  = lipgloss.AdaptiveColor{Light: "#005f87", Dark: "#5fd7ff"}
	cGood    = lipgloss.AdaptiveColor{Light: "#006400", Dark: "#87d787"}
	cWarn    = lipgloss.AdaptiveColor{Light: "#875f00", Dark: "#ffd75f"}
	cBad     = lipgloss.AdaptiveColor{Light: "#870000", Dark: "#ff8787"}
	cDim     = lipgloss.AdaptiveColor{Light: "#6c6c6c", Dark: "#8a8a8a"}
	cBarFill = lipgloss.AdaptiveColor{Light: "#00875f", Dark: "#5fd75f"}

	stTitle = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	stDim   = lipgloss.NewStyle().Foreground(cDim)
	stGood  = lipgloss.NewStyle().Foreground(cGood)
	stWarn  = lipgloss.NewStyle().Foreground(cWarn)
	stBad   = lipgloss.NewStyle().Foreground(cBad)
	stKey   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#000000"}).
		Background(cAccent).Bold(true)
	stLabel  = lipgloss.NewStyle().Foreground(cDim)
	stTabOn  = lipgloss.NewStyle().Bold(true).Foreground(cAccent).Underline(true)
	stTabOff = lipgloss.NewStyle().Foreground(cDim)
)

// View renders the whole screen.
func (m *Model) View() string {
	if m.width == 0 {
		return "starting…"
	}
	if m.showHelp {
		return m.viewHelp()
	}

	var b strings.Builder
	b.WriteString(m.viewHeader())
	b.WriteString("\n")
	b.WriteString(m.viewTabs())
	b.WriteString("\n")

	body := ""
	switch m.tab {
	case TabOverview:
		body = m.viewOverview()
	case TabQueue:
		body = m.viewQueue()
	case TabErrors:
		body = m.viewErrors()
	case TabLog:
		body = m.viewLog()
	case TabSettings:
		body = m.viewSettings()
	}
	b.WriteString(body)

	// Pad so the function bar always sits on the last row.
	used := lipgloss.Height(b.String())
	for i := used; i < m.height-1; i++ {
		b.WriteString("\n")
	}
	b.WriteString(m.viewBar())
	return b.String()
}

// ---------------------------------------------------------------- header

func (m *Model) viewHeader() string {
	s := m.snap
	st := s.Stats

	title := stTitle.Render("Web Vacuum") + stDim.Render("  "+s.Name)
	phase := m.renderPhase()

	done := st.Done
	total := st.Total
	pct := 0.0
	if total > 0 {
		pct = float64(done) / float64(total)
	}

	barW := m.width - 24
	if barW < 10 {
		barW = 10
	}
	progress := fmt.Sprintf("%s %s",
		meter(barW, pct, cBarFill),
		stDim.Render(fmt.Sprintf("%d/%d", done, total)))

	line1 := lipgloss.JoinHorizontal(lipgloss.Left, title, "  ", phase)

	line2 := strings.Join([]string{
		kv("saved", stGood.Render(fmt.Sprint(st.Done))),
		kv("queued", fmt.Sprint(st.Pending+st.Active)),
		kv("failed", failStyle(st.Failed).Render(fmt.Sprint(st.Failed))),
		kv("skipped", fmt.Sprint(st.Skipped)),
		kv("size", humanBytes(st.Bytes)),
	}, "   ")

	speed := humanBytes(int64(s.BytesPerSec)) + "/s"
	if s.RateLimit > 0 {
		// The cap is only worth showing once one exists; saying "unlimited" on
		// every run would be noise.
		speed += stDim.Render(" of " + humanBytes(s.RateLimit) + "/s")
	}
	line3 := strings.Join([]string{
		kv("speed", speed),
		kv("files/s", fmt.Sprintf("%.1f", s.FilesPerSec)),
		kv("elapsed", short(s.Elapsed)),
		kv("eta", etaText(s)),
		kv("conns", fmt.Sprintf("%d/%d", s.InFlight, len(s.Slots))),
	}, "   ")

	line4 := strings.Join([]string{
		kv("requests", fmt.Sprint(s.Fetch.Requests)),
		kv("retries", fmt.Sprint(s.Fetch.Retries)),
		kv("outages", outageStyle(s.Network.TotalOutages).Render(fmt.Sprint(s.Network.TotalOutages))),
		kv("downtime", short(s.Network.TotalDowntime)),
		kv("dest", truncLeft(s.Destination, maxInt(12, m.width-64))),
	}, "   ")

	return strings.Join([]string{line1, progress, line2, line3, line4}, "\n")
}

func (m *Model) renderPhase() string {
	s := m.snap
	switch s.Phase {
	case crawl.PhaseOffline:
		txt := "WAITING FOR NETWORK"
		if s.Network.NextProbeIn > 0 {
			txt += fmt.Sprintf(" — retry in %s", short(s.Network.NextProbeIn.Round(time.Second)))
		}
		if s.Network.Attempts > 0 {
			txt += fmt.Sprintf(" (probe %d)", s.Network.Attempts)
		}
		return stWarn.Bold(true).Render("● " + txt)
	case crawl.PhasePaused:
		return stWarn.Bold(true).Render("❚❚ PAUSED")
	case crawl.PhaseDone:
		return stGood.Bold(true).Render("✔ DONE")
	case crawl.PhaseStopped:
		return stWarn.Bold(true).Render("■ STOPPED")
	case crawl.PhaseFailed:
		return stBad.Bold(true).Render("✖ FAILED")
	case crawl.PhaseRetrying:
		return stWarn.Render(fmt.Sprintf("↻ RETRY PASS %d", s.Pass))
	case crawl.PhaseLocalizing:
		return stAccentBold().Render("⇄ LOCALIZING LINKS")
	default:
		return stGood.Render("▶ " + strings.ToUpper(string(s.Phase)))
	}
}

func stAccentBold() lipgloss.Style { return lipgloss.NewStyle().Bold(true).Foreground(cAccent) }

func failStyle(n int) lipgloss.Style {
	if n > 0 {
		return stBad
	}
	return stDim
}

func outageStyle(n int) lipgloss.Style {
	if n > 0 {
		return stWarn
	}
	return stDim
}

func etaText(s crawl.Snapshot) string {
	if s.Phase == crawl.PhaseDone || s.Phase == crawl.PhaseStopped {
		return "—"
	}
	if s.ETA <= 0 {
		return "…"
	}
	return short(s.ETA.Round(time.Second))
}

// ---------------------------------------------------------------- tabs

func (m *Model) viewTabs() string {
	var parts []string
	for t := Tab(0); t < tabCount; t++ {
		label := fmt.Sprintf(" %d %s ", int(t)+1, t.String())
		if t == m.tab {
			parts = append(parts, stTabOn.Render(label))
		} else {
			parts = append(parts, stTabOff.Render(label))
		}
	}
	line := strings.Join(parts, stDim.Render("│"))
	if m.filter != "" || m.filtering {
		cursor := ""
		if m.filtering {
			cursor = "▏"
		}
		line += "   " + stLabel.Render("filter: ") + stAccentBold().Render(m.filter+cursor)
	}
	if m.status != "" && time.Since(m.statusAt) < 3*time.Second {
		line += "   " + stWarn.Render(m.status)
	}
	return line
}

// tabAt maps an x coordinate on the tab strip back to a tab.
func (m *Model) tabAt(x int) (Tab, bool) {
	pos := 0
	for t := Tab(0); t < tabCount; t++ {
		w := len(fmt.Sprintf(" %d %s ", int(t)+1, t.String()))
		if x >= pos && x < pos+w {
			return t, true
		}
		pos += w + 1 // the separator
	}
	return 0, false
}

// ---------------------------------------------------------------- overview

func (m *Model) viewOverview() string {
	var b strings.Builder

	// A header makes the numbers readable; without it a row of figures is a
	// puzzle rather than a status display.
	b.WriteString(stLabel.Render(fmt.Sprintf(
		"CONNECTIONS   %-13s %4s %10s %11s %7s  %s",
		"PHASE", "CODE", "SIZE", "SPEED", "TIME", "URL")) + "\n")

	urlW := maxInt(20, m.width-64)
	for _, s := range m.snap.Slots {
		id := stDim.Render(fmt.Sprintf("%2d", s.ID))
		if !s.Busy {
			b.WriteString(fmt.Sprintf("%s %s %s\n", id, meter(12, 0, cBarFill), stDim.Render("idle")))
			continue
		}

		frac := 0.0
		if s.Total > 0 {
			frac = float64(s.Bytes) / float64(s.Total)
			if frac > 1 {
				frac = 1
			}
		} else if s.Bytes > 0 {
			// Without a Content-Length there is no progress to show, so the bar
			// reads as activity rather than a proportion.
			frac = 0.5
		}

		size := humanBytes(s.Bytes)
		if s.Total > 0 {
			size = humanBytes(s.Bytes) + "/" + humanBytes(s.Total)
		}
		speed := "-"
		if s.Speed > 0 {
			speed = humanBytes(int64(s.Speed)) + "/s"
		}
		elapsed := "-"
		if !s.Started.IsZero() {
			elapsed = shortMillis(time.Since(s.Started))
		}

		b.WriteString(fmt.Sprintf("%s %s %-13s %s %10s %11s %7s  %s\n",
			id,
			meter(12, frac, cBarFill),
			stDim.Render(trunc(s.Activity, 13)),
			statusCell(s.Status),
			stDim.Render(size),
			speedStyle(s.Speed).Render(speed),
			stDim.Render(elapsed),
			trunc(s.URL, urlW)))
	}

	b.WriteString("\n" + stLabel.Render("RECENT") + "\n")
	rows := m.listHeight() - len(m.snap.Slots) - 3
	if rows < 2 {
		rows = 2
	}
	start := maxInt(0, len(m.logs)-rows)
	for _, l := range m.logs[start:] {
		b.WriteString(renderLogLine(l, m.width) + "\n")
	}
	return b.String()
}

// speedStyle highlights a transfer that has effectively stalled, which is
// otherwise easy to miss among healthy ones.
func speedStyle(bps float64) lipgloss.Style {
	switch {
	case bps <= 0:
		return stDim
	case bps < 8*1024:
		return stWarn
	default:
		return lipgloss.NewStyle()
	}
}

// ---------------------------------------------------------------- lists

func (m *Model) viewQueue() string {
	rows := make([]string, 0, len(m.queue))
	for _, e := range m.queue {
		if !m.matchesFilter(e.URL) {
			continue
		}
		mark := stDim.Render("·")
		if e.Status == state.Active {
			mark = stGood.Render("▸")
		}
		rows = append(rows, fmt.Sprintf("%s %s %s  %s",
			mark, stDim.Render(fmt.Sprintf("L%-2d", e.Depth)),
			stDim.Render(fmt.Sprintf("%-5s", e.Role)), trunc(e.URL, m.width-16)))
	}
	return m.paged("QUEUE", rows, "the queue is empty")
}

func (m *Model) viewErrors() string {
	rows := make([]string, 0, len(m.errors))
	for _, e := range m.errors {
		if !m.matchesFilter(e.URL + " " + e.Err) {
			continue
		}
		code := e.ErrKind
		if e.HTTPCode > 0 {
			code = fmt.Sprint(e.HTTPCode)
		}
		rows = append(rows, fmt.Sprintf("%s %s  %s",
			stBad.Render(fmt.Sprintf("%-8s", code)),
			trunc(e.URL, maxInt(20, m.width/2)),
			stDim.Render(trunc(e.Err, maxInt(10, m.width/2-12)))))
	}
	hint := "no failures"
	if len(m.errors) > 0 {
		hint = "no failures match the filter"
	}
	return m.paged("FAILURES", rows, hint)
}

func (m *Model) viewLog() string {
	rows := make([]string, 0, len(m.logs))
	for _, l := range m.logs {
		if !m.matchesFilter(l.text) {
			continue
		}
		rows = append(rows, renderLogLine(l, m.width))
	}
	// The log is most useful pinned to the newest line unless scrolled away.
	if m.scroll[TabLog] == 0 {
		m.scroll[TabLog] = maxInt(0, len(rows)-m.listHeight())
	}
	return m.paged("LOG", rows, "nothing logged yet")
}

// renderLogLine lays a download out in columns — time, status, duration, size,
// type, path — so a run can be scanned for the slow, the large and the failed
// without reading every line.
func renderLogLine(l logLine, width int) string {
	ts := stDim.Render(l.at.Format("15:04:05"))

	switch l.ev.Kind {
	case crawl.EventFetched:
		code := statusCell(l.ev.Status)
		dur := stDim.Render(fmt.Sprintf("%7s", shortMillis(l.ev.Duration)))
		size := stDim.Render(fmt.Sprintf("%9s", humanBytes(l.ev.Size)))
		kind := stDim.Render(fmt.Sprintf("%-11s", shortMediaType(l.ev.MediaType)))
		mark := " "
		if l.ev.Rendered {
			mark = stAccentBold().Render("js")
		}
		if l.ev.Attempts > 1 {
			mark = stWarn.Render(fmt.Sprintf("×%d", l.ev.Attempts))
		}
		rest := maxInt(10, width-56)
		return fmt.Sprintf("%s %s %s %s %s %-2s %s",
			ts, code, dur, size, kind, mark, trunc(l.ev.Path, rest))

	case crawl.EventFailed:
		code := statusCell(l.ev.Status)
		rest := maxInt(10, width-24)
		msg := l.ev.Message
		if i := strings.Index(msg, ": "); i > 0 && strings.Contains(msg[:i], "://") {
			msg = msg[i+2:]
		}
		return fmt.Sprintf("%s %s %s  %s",
			ts, code, stBad.Render(trunc(l.ev.URL, rest/2)), stDim.Render(trunc(msg, rest/2)))
	}

	style := stDim
	switch l.level {
	case crawl.LevelWarn:
		style = stWarn
	case crawl.LevelError:
		style = stBad
	case crawl.LevelInfo:
		style = lipgloss.NewStyle()
	}
	return ts + " " + style.Render(trunc(l.text, maxInt(10, width-10)))
}

// statusCell colours an HTTP status by class, which is the fastest way to spot
// trouble in a scrolling list.
func statusCell(code int) string {
	if code == 0 {
		return stDim.Render("  - ")
	}
	txt := fmt.Sprintf("%4d", code)
	switch {
	case code >= 200 && code < 300:
		return stGood.Render(txt)
	case code == 304:
		return stDim.Render(txt)
	case code >= 300 && code < 400:
		return stWarn.Render(txt)
	default:
		return stBad.Render(txt)
	}
}

// shortMediaType trims a media type to the part that identifies it.
func shortMediaType(mt string) string {
	if mt == "" {
		return ""
	}
	if i := strings.IndexByte(mt, '/'); i >= 0 {
		sub := mt[i+1:]
		switch mt[:i] {
		case "image", "video", "audio", "font":
			return mt[:i] + "/" + sub
		}
		sub = strings.TrimPrefix(sub, "x-")
		sub = strings.TrimPrefix(sub, "vnd.")
		return sub
	}
	return mt
}

// shortMillis keeps sub-second timings readable instead of showing "0s".
func shortMillis(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return short(d)
	}
}

// paged renders a scrollable list with a header and position indicator.
func (m *Model) paged(title string, rows []string, empty string) string {
	h := m.listHeight()
	off := m.scroll[m.tab]
	if off > maxInt(0, len(rows)-h) {
		off = maxInt(0, len(rows)-h)
		m.scroll[m.tab] = off
	}

	var b strings.Builder
	pos := ""
	if len(rows) > h {
		pos = stDim.Render(fmt.Sprintf("   %d–%d of %d", off+1, minInt(off+h, len(rows)), len(rows)))
	} else if len(rows) > 0 {
		pos = stDim.Render(fmt.Sprintf("   %d", len(rows)))
	}
	b.WriteString(stLabel.Render(title) + pos + "\n")

	if len(rows) == 0 {
		b.WriteString(stDim.Render("  " + empty))
		return b.String()
	}
	end := minInt(off+h, len(rows))
	for _, r := range rows[off:end] {
		b.WriteString(r + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------- settings

// viewSettings renders the Setup pane as an editable list. Values change on the
// running job the moment they are accepted, so the pane doubles as the record
// of what the job is currently doing.
func (m *Model) viewSettings() string {
	cfg := m.engine.Config()
	var b strings.Builder

	hint := "↑↓ choose · Enter edit · ← → change · Esc cancel"
	b.WriteString(stLabel.Render("SETTINGS") + stDim.Render("   "+hint) + "\n")

	h := m.listHeight() - 2
	if h < 3 {
		h = 3
	}
	off := m.scroll[TabSettings]
	if off > maxInt(0, len(m.fields)-h) {
		off = maxInt(0, len(m.fields)-h)
	}
	end := minInt(off+h, len(m.fields))

	labelW := 18
	valueW := maxInt(20, m.width-labelW-6)

	for i := off; i < end; i++ {
		f := m.fields[i]
		selected := i == m.settingsIdx

		marker := "  "
		label := stLabel.Render(fmt.Sprintf("%-*s", labelW, f.label))
		if selected {
			marker = stAccentBold().Render("▸ ")
			label = stAccentBold().Render(fmt.Sprintf("%-*s", labelW, f.label))
		}

		var value string
		switch {
		case selected && m.editing:
			// A visible caret is the only thing distinguishing an empty field
			// being edited from an empty field sitting there.
			value = stAccentBold().Render(trunc(m.editBuf, valueW-1) + "▏")
		case !f.editable():
			value = stDim.Render(trunc(f.get(cfg), valueW))
		case f.kind == fieldEnum && selected:
			value = renderEnum(f.enum, f.get(cfg), valueW)
		default:
			value = trunc(displayValue(f, cfg), valueW)
		}

		b.WriteString(marker + label + " " + value + "\n")
	}

	// The help for the selected field sits at the bottom rather than beside
	// every row, which would make the list unreadable.
	b.WriteString("\n")
	if m.settingsIdx < len(m.fields) {
		f := m.fields[m.settingsIdx]
		if m.editErr != "" {
			b.WriteString(stBad.Render("  " + m.editErr))
		} else {
			b.WriteString(stDim.Render("  " + f.help))
		}
	}
	return b.String()
}

// displayValue softens settings that are switched off, so a screen full of
// "unlimited" and "no" reads as background rather than as content.
func displayValue(f settingField, cfg *config.Config) string {
	v := f.get(cfg)
	switch v {
	case "", "unlimited", "no", "never", "0s":
		return stDim.Render(orDash(v))
	}
	return v
}

// renderEnum shows the options for the selected setting with the current one
// highlighted, which saves having to remember what the choices are.
func renderEnum(values []string, current string, width int) string {
	var parts []string
	for _, v := range values {
		if v == current {
			parts = append(parts, stAccentBold().Render("["+v+"]"))
		} else {
			parts = append(parts, stDim.Render(v))
		}
	}
	return trunc(strings.Join(parts, " "), width+len(values)*12)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func zeroAsUnlimited(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	return fmt.Sprint(n)
}

func boolText(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}

// ---------------------------------------------------------------- help

func (m *Model) viewHelp() string {
	lines := []string{
		stTitle.Render("Web Vacuum — keys"),
		"",
		"  " + stKey.Render(" F1 ") + "  help (any key closes)",
		"  " + stKey.Render(" F2 ") + "  settings",
		"  " + stKey.Render(" F3 ") + "  search the list on screen (/ also works, Esc clears)",
		"  " + stKey.Render(" F4 ") + "  queue        " + stKey.Render(" F5 ") + "  failures",
		"  " + stKey.Render(" F6 ") + "  rewrite links now, without waiting for the download to finish (L)",
		"  " + stKey.Render(" F7 ") + "  slow down    " + stKey.Render(" F8 ") + "  speed up",
		"  " + stKey.Render(" F9 ") + "  pause / resume  (Space or p)",
		"  " + stKey.Render(" F10") + "  quit and save progress  (q)",
		"",
		"  Tab           switch pane          1–5   jump to a pane",
		"  ↑ ↓ / j k     scroll               PgUp / PgDn   page",
		"  g / G         top / bottom         wheel  scroll",
		"",
		stTitle.Render("  Settings (F2)"),
		"  ↑ ↓           choose a setting",
		"  Enter         edit it, or flip a yes/no",
		"  ← →           step through the choices for a setting",
		"  Esc           cancel without changing anything",
		"",
		stDim.Render("  Changes apply to the running job straight away and are saved"),
		stDim.Render("  to the job's config.yaml. Editing that file directly works too:"),
		stDim.Render("  it is re-read within a couple of seconds."),
		"",
		stDim.Render("  F3 only hides rows on screen; it downloads nothing differently."),
		stDim.Render("  To stop URLs being downloaded, put a pattern in the 'exclude'"),
		stDim.Render("  setting: it drops matching URLs already queued as well as ones"),
		stDim.Render("  not yet found. A plain word works; it is matched as a regex."),
		"",
		stDim.Render("  Links are also rewritten automatically every few minutes and when"),
		stDim.Render("  the job ends, so an interrupted download still browses offline."),
		"",
		stDim.Render("  Quitting saves the journal; running the same command again resumes."),
		stDim.Render("  A dropped connection pauses the job and resumes it automatically."),
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------- bar

// viewBar renders the htop-style function-key bar and records each button's
// horizontal extent so clicks can be routed back to it.
func (m *Model) viewBar() string {
	var b strings.Builder
	x := 0
	for i := range m.buttons {
		btn := &m.buttons[i]
		label := btn.label
		if btn.action == actPause && m.engine.Paused() {
			label = "Resume"
		}
		keyPart := stKey.Render(btn.key)
		labelPart := stLabel.Render(label + " ")

		btn.x0 = x
		x += len(btn.key) + len(label) + 1
		btn.x1 = x

		b.WriteString(keyPart)
		b.WriteString(labelPart)
	}
	line := b.String()
	if pad := m.width - x; pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return line
}

// ---------------------------------------------------------------- helpers

// meter draws a proportional bar, the visual idiom htop uses for load.
func meter(width int, frac float64, color lipgloss.TerminalColor) string {
	if width < 3 {
		width = 3
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	inner := width - 2
	filled := int(frac * float64(inner))
	fill := lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("│", filled))
	rest := stDim.Render(strings.Repeat("·", inner-filled))
	return stDim.Render("[") + fill + rest + stDim.Render("]")
}

func kv(k, v string) string { return stLabel.Render(k+" ") + v }

func trunc(s string, w int) string {
	if w <= 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}

// truncLeft keeps the end of a string, which is what matters for a file path.
func truncLeft(s string, w int) string {
	if w <= 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return "…" + string(r[len(r)-w+1:])
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTP"[exp])
}

func short(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	mn := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, mn)
	case mn > 0:
		return fmt.Sprintf("%dm%02ds", mn, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = netwatch.Online
