// Package webui serves a small web interface so a job can be set up, watched
// and controlled from a browser. It exists for the case the terminal UI cannot
// cover: starting a long download on a headless server, closing the laptop, and
// checking on it from a phone hours later.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/crawl"
	"github.com/xMasterX/web_vacuum/internal/state"
)

//go:embed assets/*
var assets embed.FS

// Options configures the server.
type Options struct {
	// Addr is host:port. Empty means 127.0.0.1 on a random free port, which is
	// the safe default: reachable from the machine, not from the network.
	Addr string
	// Token is required on every request. Empty generates one.
	Token string
	// Engine is an already-running job. Nil means the interface starts idle and
	// offers a form to create one.
	Engine *crawl.Engine
	// Template supplies the defaults shown in that form.
	Template *config.Config
	// Open launches a browser once the listener is up.
	Open bool
}

// Server is the web interface.
type Server struct {
	opt      Options
	token    string
	ln       net.Listener
	mux      *http.ServeMux
	template *config.Config

	mu      sync.RWMutex
	engine  *crawl.Engine
	cancel  context.CancelFunc
	running bool
	lastErr string
}

// New binds the listener immediately so the address can be printed before
// Serve blocks.
func New(opt Options) (*Server, error) {
	addr := opt.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	// A bare port or a bare host are both things people type; accept them.
	if !strings.Contains(addr, ":") {
		if _, err := strconv.Atoi(addr); err == nil {
			addr = "127.0.0.1:" + addr
		} else {
			addr += ":0"
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("web interface: %w", err)
	}

	token := opt.Token
	if token == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			ln.Close()
			return nil, err
		}
		token = hex.EncodeToString(buf)
	}

	tmpl := opt.Template
	if tmpl == nil {
		tmpl = config.Default()
	}

	s := &Server{
		opt:      opt,
		token:    token,
		ln:       ln,
		mux:      http.NewServeMux(),
		engine:   opt.Engine,
		template: tmpl,
		running:  opt.Engine != nil,
	}
	s.routes()
	return s, nil
}

// URL is the address to open, including the access token.
func (s *Server) URL() string {
	host := s.ln.Addr().String()
	if tcp, ok := s.ln.Addr().(*net.TCPAddr); ok {
		ip := tcp.IP.String()
		if tcp.IP.IsUnspecified() {
			ip = "127.0.0.1"
		}
		host = net.JoinHostPort(ip, strconv.Itoa(tcp.Port))
	}
	return fmt.Sprintf("http://%s/?t=%s", host, s.token)
}

// Serve runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	srv := &http.Server{
		Handler:      s.mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // the event stream is deliberately long-lived
		IdleTimeout:  120 * time.Second,
	}
	if s.opt.Open {
		go openBrowser(s.URL())
	}

	errc := make(chan error, 1)
	go func() {
		err := srv.Serve(s.ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
		return nil
	case err := <-errc:
		return err
	}
}

// ---------------------------------------------------------------- routing

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.guard(s.handleIndex))
	s.mux.HandleFunc("/app.js", s.guard(s.handleAsset("assets/app.js", "text/javascript")))
	s.mux.HandleFunc("/app.css", s.guard(s.handleAsset("assets/app.css", "text/css")))
	s.mux.HandleFunc("/api/snapshot", s.guard(s.handleSnapshot))
	s.mux.HandleFunc("/api/events", s.guard(s.handleEvents))
	s.mux.HandleFunc("/api/control", s.guard(s.handleControl))
	s.mux.HandleFunc("/api/start", s.guard(s.handleStart))
	s.mux.HandleFunc("/api/list", s.guard(s.handleList))
	s.mux.HandleFunc("/api/config", s.guard(s.handleConfig))
	s.mux.HandleFunc("/api/settings", s.guard(s.handleSettings))
	s.mux.HandleFunc("/files/", s.guard(s.handleFiles))
}

// guard enforces the access token. The token may arrive as a query parameter
// (so the printed URL just works) or as a cookie (so later requests are clean).
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("t")
		if got == "" {
			if c, err := r.Cookie("webvacuum"); err == nil {
				got = c.Value
			}
		}
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "invalid or missing access token", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: "webvacuum", Value: s.token, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 86400 * 7,
		})
		// The interface is local-only by default and embeds no third-party
		// resources, so a strict policy costs nothing.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next(w, r)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) handleAsset(name, mime string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := assets.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mime+"; charset=utf-8")
		w.Write(data)
	}
}

// ---------------------------------------------------------------- api

type snapshotResponse struct {
	// UserAgentResolved is the full header value the configured preset expands
	// to, shown because the preset name alone answers no useful question.
	UserAgentResolved string          `json:"user_agent_resolved,omitempty"`
	Running           bool            `json:"running"`
	Snapshot          *crawl.Snapshot `json:"snapshot,omitempty"`
	Config            *config.Config  `json:"config,omitempty"`
	Template          *config.Config  `json:"template,omitempty"`
	Paused            bool            `json:"paused"`
	Entry             string          `json:"entry,omitempty"`
	Error             string          `json:"error,omitempty"`
	Presets           presets         `json:"presets"`
}

type presets struct {
	UserAgents  []string `json:"user_agents"`
	Categories  []string `json:"categories"`
	Constraints []string `json:"constraints"`
	Supporting  []string `json:"supporting"`
	Replacement []string `json:"replacement"`
	RenderModes []string `json:"render_modes"`
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	e := s.currentEngine()
	resp := snapshotResponse{
		Template: s.template,
		Presets: presets{
			UserAgents:  config.UserAgentNames(),
			Categories:  config.CategoryNames(),
			Constraints: []string{"host", "subdomains", "host+1", "directory", "rules", "none"},
			Supporting:  []string{"any", "related", "none"},
			Replacement: []string{"newer", "never", "always"},
			RenderModes: []string{"never", "auto", "always"},
		},
	}
	s.mu.RLock()
	resp.Error = s.lastErr
	s.mu.RUnlock()

	if e != nil {
		snap := e.Snapshot()
		resp.Running = true
		resp.Snapshot = &snap
		resp.Config = e.Config()
		resp.Paused = e.Paused()
		resp.Entry = e.EntryPoint()
		resp.UserAgentResolved = config.ResolveUserAgent(e.Config().Request.UserAgent)
	}
	writeJSON(w, resp)
}

// handleEvents streams events with Server-Sent Events, which needs no client
// library and reconnects on its own.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	e := s.currentEngine()
	if e == nil {
		// Idle: hold the connection open with comments so the browser's
		// EventSource does not reconnect in a tight loop.
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-t.C:
				fmt.Fprint(w, ": idle\n\n")
				flusher.Flush()
			}
		}
	}

	events, unsub := e.Subscribe(512)
	defer unsub()

	for _, ev := range e.Recent() {
		writeSSE(w, "event", ev)
	}
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			writeSSE(w, "event", ev)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Action string `json:"action"`
		Value  int    `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	e := s.currentEngine()
	if e == nil {
		http.Error(w, "no job is running", http.StatusConflict)
		return
	}
	msg := ""
	switch body.Action {
	case "pause":
		e.Pause()
		msg = "paused"
	case "resume":
		e.Resume()
		msg = "resumed"
	case "toggle":
		if e.TogglePause() {
			msg = "paused"
		} else {
			msg = "resumed"
		}
	case "stop":
		e.Stop("stopped from the web interface")
		msg = "stopping"
	case "faster":
		msg = e.AdjustRate(1)
	case "slower":
		msg = e.AdjustRate(-1)
	case "connections":
		msg = fmt.Sprintf("%d connections", e.SetConnections(body.Value))
	case "localize":
		msg = e.LocalizeNow()
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": msg})
}

// handleStart creates and runs a job from settings posted by the browser.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		http.Error(w, "a job is already running", http.StatusConflict)
		return
	}
	s.mu.Unlock()

	cfg := s.template.Clone()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(cfg); err != nil {
		http.Error(w, "could not read the settings: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := cfg.Normalize(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	e, err := crawl.New(cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.engine = e
	s.cancel = cancel
	s.running = true
	s.lastErr = ""
	s.mu.Unlock()

	go func() {
		defer cancel()
		if err := e.Run(ctx); err != nil {
			s.mu.Lock()
			s.lastErr = err.Error()
			s.mu.Unlock()
		}
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	writeJSON(w, map[string]string{"status": "started", "destination": cfg.Destination})
}

// handleList returns queue or failure rows for the browser's tables.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	e := s.currentEngine()
	if e == nil {
		writeJSON(w, []state.Entry{})
		return
	}
	want := r.URL.Query().Get("status")
	limit := 500
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 5000 {
		limit = n
	}
	q := strings.ToLower(r.URL.Query().Get("q"))

	rows := e.Store().Select(func(en *state.Entry) bool {
		switch want {
		case "failed":
			if en.Status != state.Failed {
				return false
			}
		case "queued":
			if en.Status != state.Pending && en.Status != state.Active {
				return false
			}
		case "done":
			if en.Status != state.Done {
				return false
			}
		case "skipped":
			if en.Status != state.Skipped {
				return false
			}
		}
		if q != "" && !strings.Contains(strings.ToLower(en.URL), q) {
			return false
		}
		return true
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	writeJSON(w, rows)
}

// handleSettings applies a settings change to the running job.
//
// The browser posts the same shape the YAML file uses, which is decoded over a
// copy of the current settings. Reusing that one representation everywhere —
// file, API, saved job — means the web form cannot drift out of step with what
// the crawler actually understands.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	e := s.currentEngine()
	if e == nil {
		http.Error(w, "no job is running", http.StatusConflict)
		return
	}

	next := e.Config().Clone()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(next); err != nil {
		http.Error(w, "could not read the settings: "+err.Error(), http.StatusBadRequest)
		return
	}
	res, err := e.Reconfigure(next)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"status":  res.Summary(),
		"applied": res.Applied,
		"ignored": res.Ignored,
		"config":  e.Config(),
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if e := s.currentEngine(); e != nil {
		writeJSON(w, e.Config())
		return
	}
	writeJSON(w, s.template)
}

// handleFiles serves the mirror itself, so the download can be checked in the
// same browser tab that is watching it.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	e := s.currentEngine()
	if e == nil {
		http.Error(w, "no job", http.StatusNotFound)
		return
	}
	root := e.Config().Destination
	rel := strings.TrimPrefix(r.URL.Path, "/files/")
	if rel == "" {
		// Bare /files/ opens whatever the crawl started from, so the link in
		// the interface lands on the site's front page.
		if entry := e.EntryPoint(); entry != "" {
			rel = entry
		} else {
			rel = "index.html"
		}
	}
	if decoded, err := url.PathUnescape(rel); err == nil {
		rel = decoded
	}

	// The content is served directly rather than through http.FileServer,
	// which canonicalizes ".../index.html" into a redirect to "./" and so
	// cannot serve a mirror mounted under a prefix.
	full, err := safeJoin(root, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if fi.IsDir() {
		idx := filepath.Join(full, "index.html")
		df, derr := os.Open(idx)
		if derr != nil {
			http.NotFound(w, r)
			return
		}
		defer df.Close()
		dfi, derr := df.Stat()
		if derr != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, idx, dfi.ModTime(), df)
		return
	}
	http.ServeContent(w, r, full, fi.ModTime(), f)
}

// safeJoin resolves a URL-supplied relative path inside root, refusing any
// result that would land outside it.
func safeJoin(root, rel string) (string, error) {
	clean := filepath.Clean("/" + filepath.FromSlash(rel))
	full := filepath.Join(root, clean)
	rootClean := filepath.Clean(root)
	if full != rootClean && !strings.HasPrefix(full, rootClean+string(filepath.Separator)) {
		return "", errors.New("path escapes the download folder")
	}
	return full, nil
}

func (s *Server) currentEngine() *crawl.Engine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.engine
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.Encode(v)
}

func writeSSE(w http.ResponseWriter, event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

// openBrowser is best-effort; failing to open one is never an error worth
// stopping for, since the URL is printed anyway.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	exec.Command(cmd, append(args, url)...).Start()
}
