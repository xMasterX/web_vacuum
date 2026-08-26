// Package tui is the terminal interface: an htop-style live view of a running
// crawl, with a function-key bar that responds to both keystrokes and mouse
// clicks, and panes for the queue, the failures and the log.
package tui

import (
	"context"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xMasterX/web_vacuum/internal/crawl"
	"github.com/xMasterX/web_vacuum/internal/state"
)

// Tab is one pane of the interface.
type Tab int

const (
	TabOverview Tab = iota
	TabQueue
	TabErrors
	TabLog
	TabSettings
	tabCount
)

func (t Tab) String() string {
	switch t {
	case TabOverview:
		return "Overview"
	case TabQueue:
		return "Queue"
	case TabErrors:
		return "Errors"
	case TabLog:
		return "Log"
	case TabSettings:
		return "Settings"
	}
	return "?"
}

// button is one entry in the function-key bar. Storing its rendered extent lets
// a mouse click be mapped back to the action without re-deriving the layout.
type button struct {
	key    string
	label  string
	action action
	x0, x1 int
}

type action int

const (
	actHelp action = iota
	actSettings
	actFilter
	actQueue
	actErrors
	actLog
	actLocalize
	actSlower
	actFaster
	actPause
	actQuit
	actNextTab
)

// logLine is one event kept for the Log pane. The whole event is retained
// rather than a formatted string, so the view can lay the parts out in columns
// and colour them independently.
type logLine struct {
	at    time.Time
	level crawl.Level
	ev    crawl.Event
	text  string
}

// Model is the bubbletea model.
type Model struct {
	engine *crawl.Engine
	ctx    context.Context
	cancel context.CancelFunc

	width, height int
	tab           Tab
	snap          crawl.Snapshot

	logs    []logLine
	maxLogs int

	// scroll is the first visible row of the active pane's list.
	scroll    map[Tab]int
	filter    string
	filtering bool
	showHelp  bool

	// Settings-pane editing state.
	fields      []settingField
	settingsIdx int
	editing     bool
	editBuf     string
	editErr     string

	queue  []state.Entry
	errors []state.Entry
	// listRefresh throttles the journal scans that back the Queue and Errors
	// panes; they are expensive on a large crawl and only need to look live.
	lastList time.Time

	buttons []button
	events  <-chan crawl.Event
	unsub   func()

	quitting bool
	// finished is set when the engine reports the job is over, so the UI can
	// stay open showing the result rather than vanishing.
	finished bool
	status   string
	statusAt time.Time
}

// New builds a Model bound to a running engine.
func New(ctx context.Context, e *crawl.Engine) *Model {
	ctx, cancel := context.WithCancel(ctx)
	events, unsub := e.Subscribe(1024)
	m := &Model{
		engine:  e,
		ctx:     ctx,
		cancel:  cancel,
		tab:     TabOverview,
		maxLogs: 2000,
		scroll:  map[Tab]int{},
		events:  events,
		unsub:   unsub,
	}
	for _, ev := range e.Recent() {
		m.appendEvent(ev)
	}
	m.fields = settingFields()
	m.buttons = []button{
		{key: "F1", label: "Help", action: actHelp},
		{key: "F2", label: "Setup", action: actSettings},
		{key: "F3", label: "Filter", action: actFilter},
		{key: "F4", label: "Queue", action: actQueue},
		{key: "F5", label: "Errors", action: actErrors},
		{key: "F6", label: "Links", action: actLocalize},
		{key: "F7", label: "Slower", action: actSlower},
		{key: "F8", label: "Faster", action: actFaster},
		{key: "F9", label: "Pause", action: actPause},
		{key: "F10", label: "Quit", action: actQuit},
	}
	return m
}

// tickMsg drives the refresh loop.
type tickMsg time.Time

// eventMsg carries one engine event into the update loop.
type eventMsg crawl.Event

// eventsClosedMsg means the engine stopped publishing.
type eventsClosedMsg struct{}

func tick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func waitForEvent(ch <-chan crawl.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return eventMsg(ev)
	}
}

// Init starts the refresh and event pumps.
func (m *Model) Init() tea.Cmd {
	m.snap = m.engine.Snapshot()
	return tea.Batch(tick(), waitForEvent(m.events))
}

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		m.snap = m.engine.Snapshot()
		m.refreshLists()
		if m.snap.Phase == crawl.PhaseDone || m.snap.Phase == crawl.PhaseStopped || m.snap.Phase == crawl.PhaseFailed {
			m.finished = true
		}
		return m, tick()

	case eventMsg:
		m.appendEvent(crawl.Event(msg))
		return m, waitForEvent(m.events)

	case eventsClosedMsg:
		m.finished = true
		return m, nil

	case tea.MouseMsg:
		return m, m.handleMouse(msg)

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) appendEvent(ev crawl.Event) {
	text := m.formatEvent(ev)
	if text == "" {
		return
	}
	level := ev.Level
	if level == "" {
		level = crawl.LevelInfo
	}
	m.logs = append(m.logs, logLine{at: ev.Time, level: level, ev: ev, text: text})
	if len(m.logs) > m.maxLogs {
		m.logs = m.logs[len(m.logs)-m.maxLogs:]
	}
}

// formatEvent produces the plain text used for filtering and for the fallback
// rendering; the columned form is built in the view.
func (m *Model) formatEvent(ev crawl.Event) string {
	switch ev.Kind {
	case crawl.EventFetched:
		if ev.Message == "unchanged" {
			return "unchanged " + ev.URL
		}
		return ev.Path + " " + ev.MediaType
	case crawl.EventFailed:
		return ev.URL + " " + ev.Message
	case crawl.EventSkipped:
		// Skips are frequent and mostly uninteresting; they live in the Queue
		// pane rather than flooding the log.
		return ""
	case crawl.EventNetwork, crawl.EventLog, crawl.EventPhase:
		return ev.Message
	case crawl.EventFinished:
		return "job " + string(ev.Phase)
	}
	return ev.Message
}

// refreshLists reloads the Queue and Errors panes at a human-visible rate.
func (m *Model) refreshLists() {
	if time.Since(m.lastList) < 700*time.Millisecond {
		return
	}
	m.lastList = time.Now()
	st := m.engine.Store()

	if m.tab == TabQueue {
		m.queue = st.Select(func(e *state.Entry) bool {
			return e.Status == state.Pending || e.Status == state.Active
		})
		if len(m.queue) > 5000 {
			m.queue = m.queue[:5000]
		}
	}
	if m.tab == TabErrors {
		m.errors = st.Select(func(e *state.Entry) bool { return e.Status == state.Failed })
		sort.SliceStable(m.errors, func(i, j int) bool { return m.errors[i].UpdatedAt > m.errors[j].UpdatedAt })
		if len(m.errors) > 5000 {
			m.errors = m.errors[:5000]
		}
	}
}

// ---------------------------------------------------------------- input

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	if m.editing {
		return m.handleEditKey(msg)
	}
	if m.tab == TabSettings && m.handleSettingsKey(msg) {
		return nil
	}
	if m.filtering {
		switch msg.Type {
		case tea.KeyEnter, tea.KeyEsc:
			m.filtering = false
		case tea.KeyBackspace:
			if n := len(m.filter); n > 0 {
				m.filter = m.filter[:n-1]
			}
		case tea.KeyCtrlU:
			m.filter = ""
		case tea.KeyRunes, tea.KeySpace:
			m.filter += string(msg.Runes)
			if msg.Type == tea.KeySpace {
				m.filter += " "
			}
		}
		return nil
	}
	if m.showHelp {
		m.showHelp = false
		return nil
	}

	switch msg.String() {
	case "f1", "?", "h":
		return m.run(actHelp)
	case "f2":
		return m.run(actSettings)
	case "f3", "/":
		return m.run(actFilter)
	case "f4":
		return m.run(actQueue)
	case "f5":
		return m.run(actErrors)
	case "f6":
		return m.run(actLocalize)
	case "L", "ctrl+l":
		return m.run(actLocalize)
	case "f7", "-":
		return m.run(actSlower)
	case "f8", "+", "=":
		return m.run(actFaster)
	case "f9", "p", " ":
		return m.run(actPause)
	case "f10", "q", "ctrl+c":
		return m.run(actQuit)
	case "tab", "right", "l":
		m.tab = (m.tab + 1) % tabCount
		return nil
	case "shift+tab", "left":
		m.tab = (m.tab + tabCount - 1) % tabCount
		return nil
	case "1":
		m.tab = TabOverview
	case "2":
		m.tab = TabQueue
	case "3":
		m.tab = TabErrors
	case "4":
		m.tab = TabLog
	case "5":
		m.tab = TabSettings
	case "up", "k":
		m.scrollBy(-1)
	case "down", "j":
		m.scrollBy(1)
	case "pgup", "ctrl+b":
		m.scrollBy(-m.listHeight())
	case "pgdown", "ctrl+f":
		m.scrollBy(m.listHeight())
	case "home", "g":
		m.scroll[m.tab] = 0
	case "end", "G":
		m.scroll[m.tab] = 1 << 30
	case "esc":
		m.filter = ""
	}
	return nil
}

func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.scrollBy(-3)
		return nil
	case tea.MouseButtonWheelDown:
		m.scrollBy(3)
		return nil
	}
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return nil
	}
	// The function-key bar occupies the last row; a click there activates the
	// button whose rendered span contains the cursor.
	if msg.Y == m.height-1 {
		for _, b := range m.buttons {
			if msg.X >= b.x0 && msg.X < b.x1 {
				return m.run(b.action)
			}
		}
		return nil
	}
	// The tab strip is the second row.
	if msg.Y == 1 {
		if t, ok := m.tabAt(msg.X); ok {
			m.tab = t
		}
	}
	return nil
}

func (m *Model) run(a action) tea.Cmd {
	switch a {
	case actHelp:
		m.showHelp = true
	case actSettings:
		m.tab = TabSettings
	case actFilter:
		m.filtering = true
	case actQueue:
		m.tab = TabQueue
	case actErrors:
		m.tab = TabErrors
	case actLog:
		m.tab = TabLog
	case actLocalize:
		// Showing the log makes the result visible; the pass reports there.
		m.tab = TabLog
		m.flash(m.engine.LocalizeNow())
	case actPause:
		if m.engine.TogglePause() {
			m.flash("paused")
		} else {
			m.flash("resumed")
		}
	case actSlower:
		m.flash(m.engine.AdjustRate(-1))
	case actFaster:
		m.flash(m.engine.AdjustRate(1))
	case actQuit:
		m.quitting = true
		m.engine.Stop("quit requested from the terminal")
		m.unsub()
		m.cancel()
		return tea.Quit
	case actNextTab:
		m.tab = (m.tab + 1) % tabCount
	}
	return nil
}

func (m *Model) flash(msg string) {
	if msg == "" {
		return
	}
	m.status = msg
	m.statusAt = time.Now()
}

func (m *Model) scrollBy(n int) {
	m.scroll[m.tab] += n
	if m.scroll[m.tab] < 0 {
		m.scroll[m.tab] = 0
	}
}

// listHeight is how many rows the active pane's list can show.
func (m *Model) listHeight() int {
	h := m.height - headerHeight - 4
	if h < 3 {
		h = 3
	}
	return h
}

// matchesFilter reports whether a row survives the current filter text.
func (m *Model) matchesFilter(s string) bool {
	if m.filter == "" {
		return true
	}
	return strings.Contains(strings.ToLower(s), strings.ToLower(m.filter))
}

// ---------------------------------------------------------------- settings

// handleSettingsKey drives the Setup pane. It returns true when it consumed the
// key, so the general bindings still work for everything it does not use.
func (m *Model) handleSettingsKey(msg tea.KeyMsg) bool {
	if len(m.fields) == 0 {
		return false
	}
	switch msg.String() {
	case "up", "k":
		m.moveSetting(-1)
		return true
	case "down", "j":
		m.moveSetting(1)
		return true
	case "home", "g":
		m.settingsIdx = 0
		m.scroll[TabSettings] = 0
		return true
	case "end", "G":
		m.settingsIdx = len(m.fields) - 1
		return true
	case "pgup":
		m.moveSetting(-m.listHeight())
		return true
	case "pgdown":
		m.moveSetting(m.listHeight())
		return true

	case "enter", "e":
		return m.beginEdit()

	case "left", "h":
		return m.cycleSetting(-1)
	case "right", "l":
		return m.cycleSetting(1)
	}
	return false
}

func (m *Model) moveSetting(delta int) {
	m.settingsIdx += delta
	if m.settingsIdx < 0 {
		m.settingsIdx = 0
	}
	if m.settingsIdx >= len(m.fields) {
		m.settingsIdx = len(m.fields) - 1
	}
	// Keep the selection inside the visible window.
	h := m.listHeight()
	if m.settingsIdx < m.scroll[TabSettings] {
		m.scroll[TabSettings] = m.settingsIdx
	}
	if m.settingsIdx >= m.scroll[TabSettings]+h {
		m.scroll[TabSettings] = m.settingsIdx - h + 1
	}
	m.editErr = ""
}

// beginEdit starts editing the selected field. Booleans and enumerations have
// no text worth typing, so they change in place instead.
func (m *Model) beginEdit() bool {
	f := m.fields[m.settingsIdx]
	if !f.editable() {
		m.editErr = "this cannot be changed while the job is running"
		return true
	}
	switch f.kind {
	case fieldBool:
		return m.applySetting(f, showBool(!parseBool(f.get(m.engine.Config()))))
	case fieldEnum:
		return m.cycleSetting(1)
	}
	m.editing = true
	m.editBuf = f.get(m.engine.Config())
	m.editErr = ""
	return true
}

func (m *Model) cycleSetting(step int) bool {
	f := m.fields[m.settingsIdx]
	switch {
	case !f.editable():
		return false
	case f.kind == fieldEnum:
		return m.applySetting(f, cycleEnum(f.enum, f.get(m.engine.Config()), step))
	case f.kind == fieldBool:
		return m.applySetting(f, showBool(!parseBool(f.get(m.engine.Config()))))
	}
	return false
}

func (m *Model) handleEditKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEnter:
		f := m.fields[m.settingsIdx]
		m.editing = false
		m.applySetting(f, m.editBuf)
	case tea.KeyEsc:
		m.editing = false
		m.editErr = ""
	case tea.KeyBackspace:
		if n := len(m.editBuf); n > 0 {
			m.editBuf = m.editBuf[:n-1]
		}
	case tea.KeyCtrlU:
		m.editBuf = ""
	case tea.KeySpace:
		m.editBuf += " "
	case tea.KeyRunes:
		m.editBuf += string(msg.Runes)
	}
	return nil
}

// applySetting writes one field into a copy of the settings and hands the whole
// thing to the engine, which validates it and reports what it could and could
// not do. Editing a copy means a rejected value never half-lands.
func (m *Model) applySetting(f settingField, value string) bool {
	next := m.engine.Config().Clone()
	if err := f.set(next, value); err != nil {
		m.editErr = err.Error()
		return true
	}
	res, err := m.engine.Reconfigure(next)
	if err != nil {
		m.editErr = err.Error()
		return true
	}
	m.editErr = ""
	if res.Changed() {
		m.flash(f.label + " → " + value)
	}
	return true
}
