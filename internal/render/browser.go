package render

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Options configures the browser.
type Options struct {
	// ExecPath is an explicit browser binary. Empty searches the usual places.
	ExecPath string
	// RemoteURL attaches to a browser someone else is running, given either its
	// http://host:port debugging endpoint or a ws:// URL. Useful for a
	// container that keeps Chrome in a sidecar.
	RemoteURL string
	// Tabs bounds how many pages render at once. Chrome is far heavier than an
	// HTTP connection, so this is deliberately separate from the crawl's
	// connection count.
	Tabs int
	// Headful runs a visible window, which is the only practical way to watch
	// what a site is doing to you.
	Headful bool
	// NoSandbox is required when running as root, which is common in containers.
	NoSandbox    bool
	UserAgent    string
	Proxy        string
	InsecureTLS  bool
	WindowWidth  int
	WindowHeight int
	ExtraArgs    []string
	// StartTimeout bounds how long to wait for the browser to come up.
	StartTimeout time.Duration
}

// Browser is a running (or attached) headless browser.
type Browser struct {
	opt     Options
	cmd     *exec.Cmd
	conn    *conn
	tmpDir  string
	slots   chan struct{}
	closeMu sync.Mutex
	closed  bool
	// launched distinguishes a browser we started from one we attached to, so
	// Close never kills something that was not ours.
	launched bool
}

// chromeCandidates lists where a Chrome-family browser usually lives. Any
// Chromium build works; the protocol is the same.
func chromeCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
		}
	case "windows":
		var out []string
		for _, base := range []string{
			os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData"),
		} {
			if base == "" {
				continue
			}
			out = append(out,
				filepath.Join(base, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(base, `Chromium\Application\chrome.exe`),
				filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
			)
		}
		return out
	default:
		return []string{
			"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable",
			"/usr/bin/chromium", "/usr/bin/chromium-browser",
			"/usr/bin/microsoft-edge", "/usr/bin/brave-browser",
			"/snap/bin/chromium", "/opt/google/chrome/chrome",
		}
	}
}

// FindChrome locates a usable browser binary, or returns an error naming what
// was tried so the user knows what to install or point at.
func FindChrome(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("browser not found at %s", explicit)
		}
		return explicit, nil
	}
	if env := os.Getenv("SITEDUMPER_CHROME"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env, nil
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome", "msedge"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	for _, p := range chromeCandidates() {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no Chrome or Chromium found — install one, " +
		"or point at it with --chrome-path (or the SITEDUMPER_CHROME environment variable)")
}

// Launch starts a browser, or attaches to one if RemoteURL is set.
func Launch(ctx context.Context, opt Options) (*Browser, error) {
	if opt.Tabs <= 0 {
		opt.Tabs = 2
	}
	if opt.StartTimeout <= 0 {
		opt.StartTimeout = 45 * time.Second
	}
	if opt.WindowWidth <= 0 {
		opt.WindowWidth = 1366
	}
	if opt.WindowHeight <= 0 {
		opt.WindowHeight = 900
	}

	b := &Browser{opt: opt, slots: make(chan struct{}, opt.Tabs)}

	if opt.RemoteURL != "" {
		wsURL, err := resolveRemote(ctx, opt.RemoteURL)
		if err != nil {
			return nil, err
		}
		if err := b.dial(ctx, wsURL); err != nil {
			return nil, err
		}
		return b, nil
	}

	exe, err := FindChrome(opt.ExecPath)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "webvacuum-chrome-")
	if err != nil {
		return nil, err
	}
	b.tmpDir = tmp

	args := []string{
		"--remote-debugging-port=0",
		"--user-data-dir=" + tmp,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-client-side-phishing-detection",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-sync",
		"--disable-extensions",
		"--disable-popup-blocking",
		"--disable-prompt-on-repost",
		"--metrics-recording-only",
		"--no-service-autorun",
		"--password-store=basic",
		"--use-mock-keychain",
		"--mute-audio",
		"--hide-scrollbars",
		fmt.Sprintf("--window-size=%d,%d", opt.WindowWidth, opt.WindowHeight),
	}
	if !opt.Headful {
		args = append(args, "--headless=new")
	}
	if opt.NoSandbox {
		args = append(args, "--no-sandbox", "--disable-setuid-sandbox", "--disable-dev-shm-usage")
	}
	if opt.InsecureTLS {
		args = append(args, "--ignore-certificate-errors")
	}
	if opt.Proxy != "" {
		args = append(args, "--proxy-server="+opt.Proxy)
	}
	if opt.UserAgent != "" {
		args = append(args, "--user-agent="+opt.UserAgent)
	}
	args = append(args, opt.ExtraArgs...)
	args = append(args, "about:blank")

	cmd := exec.Command(exe, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		os.RemoveAll(tmp)
		return nil, fmt.Errorf("starting %s: %w", exe, err)
	}
	b.cmd = cmd
	b.launched = true

	wsURL, err := waitForDevTools(ctx, stderr, tmp, opt.StartTimeout)
	if err != nil {
		b.Close()
		return nil, err
	}
	if err := b.dial(ctx, wsURL); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

func (b *Browser) dial(ctx context.Context, wsURL string) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 20 * time.Second,
		ReadBufferSize:   64 * 1024,
		WriteBufferSize:  64 * 1024,
	}
	ws, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("connecting to the browser at %s: %w", wsURL, err)
	}
	b.conn = newConn(ws)
	return nil
}

// waitForDevTools reads the endpoint from Chrome's stderr, falling back to the
// DevToolsActivePort file it writes into the profile directory.
func waitForDevTools(ctx context.Context, stderr io.Reader, profileDir string, timeout time.Duration) (string, error) {
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if i := strings.Index(line, "ws://"); i >= 0 {
				found <- strings.TrimSpace(line[i:])
				// Keep draining so the pipe never fills and blocks the browser.
				for sc.Scan() {
				}
				return
			}
		}
	}()

	deadline := time.After(timeout)
	poll := time.NewTicker(150 * time.Millisecond)
	defer poll.Stop()

	for {
		select {
		case url := <-found:
			return url, nil
		case <-poll.C:
			if url := readActivePort(profileDir); url != "" {
				return url, nil
			}
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("the browser did not report a debugging endpoint within %s", timeout)
		}
	}
}

func readActivePort(profileDir string) string {
	data, err := os.ReadFile(filepath.Join(profileDir, "DevToolsActivePort"))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		return ""
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("ws://127.0.0.1:%d%s", port, strings.TrimSpace(lines[1]))
}

// resolveRemote turns a user-supplied endpoint into a browser websocket URL.
func resolveRemote(ctx context.Context, raw string) (string, error) {
	if strings.HasPrefix(raw, "ws://") || strings.HasPrefix(raw, "wss://") {
		return raw, nil
	}
	base := raw
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	base = strings.TrimSuffix(base, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("asking %s for its debugging endpoint: %w", base, err)
	}
	defer resp.Body.Close()

	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", fmt.Errorf("reading the debugging endpoint from %s: %w", base, err)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("%s did not report a debugging endpoint", base)
	}
	return v.WebSocketDebuggerURL, nil
}

// Close shuts the browser down and removes its temporary profile.
func (b *Browser) Close() error {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true

	if b.conn != nil {
		// Asking politely lets Chrome flush and exit; the kill below is the
		// backstop for a browser that has stopped listening.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		b.conn.call(ctx, "", "Browser.close", nil, nil)
		cancel()
		b.conn.close()
	}
	if b.launched && b.cmd != nil && b.cmd.Process != nil {
		done := make(chan struct{})
		go func() { b.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			b.cmd.Process.Kill()
			<-done
		}
	}
	if b.tmpDir != "" {
		os.RemoveAll(b.tmpDir)
	}
	return nil
}

// Version reports what the browser identifies itself as, for the log.
func (b *Browser) Version(ctx context.Context) string {
	var v struct {
		Product string `json:"product"`
	}
	if err := b.conn.call(ctx, "", "Browser.getVersion", nil, &v); err != nil {
		return "unknown browser"
	}
	return v.Product
}
