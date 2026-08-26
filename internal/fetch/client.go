// Package fetch performs HTTP requests on behalf of the crawler. It owns
// retries, per-host politeness, cookies, authentication and the interaction
// with the offline gate, so the crawl engine only ever sees "here is a
// response" or "here is a final error".
package fetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/netwatch"
)

// Kind classifies a failure so the crawler can decide whether to record it as
// permanent, queue it for a later pass, or ignore it.
type Kind int

const (
	KindNone Kind = iota
	// KindTransport is a network-level failure: DNS, refused, reset, timeout.
	KindTransport
	// KindHTTP is a server response that is not success and not worth retrying.
	KindHTTP
	// KindTooLarge means the response exceeded the configured size cap.
	KindTooLarge
	// KindRobots means robots.txt forbade the request.
	KindRobots
	// KindCanceled means the job was stopped.
	KindCanceled
)

func (k Kind) String() string {
	switch k {
	case KindTransport:
		return "network"
	case KindHTTP:
		return "http"
	case KindTooLarge:
		return "too-large"
	case KindRobots:
		return "robots"
	case KindCanceled:
		return "canceled"
	}
	return "ok"
}

// Error is a fetch failure with enough context to display and to retry later.
type Error struct {
	URL      string
	Kind     Kind
	Status   int
	Attempts int
	Err      error
	// After carries a Retry-After hint from the server, when it sent one.
	After time.Duration
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s: HTTP %d %s", e.URL, e.Status, http.StatusText(e.Status))
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.URL, e.Err)
	}
	return e.URL + ": failed"
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether a later pass has any chance of succeeding.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case KindTransport:
		return true
	case KindHTTP:
		return e.Status == 0 || e.Status >= 500 || e.Status == 408 || e.Status == 429
	}
	return false
}

// Request is one thing to fetch.
type Request struct {
	URL     *url.URL
	Referer string
	Method  string
	// ETag and LastModified enable conditional GET so an interrupted mirror can
	// be refreshed without re-downloading unchanged files.
	ETag         string
	LastModified string
	// Head asks for a HEAD request first when the caller only needs metadata.
	NoBody bool
	// Offsite marks a file that belongs to a host outside the crawl — a
	// third-party image or script a page happens to reference. Those get a
	// short wait and few attempts: they are optional, and an old site is full
	// of references to hosts that stopped answering years ago.
	Offsite bool
	// OnPhase reports what the request is doing. Without it a connection shows
	// "connecting" for the whole time it spends queued behind a host's rate
	// limit or backing off from a retry, which reads as a stall.
	OnPhase func(phase string)
}

// phase reports progress when the caller asked to hear about it.
func (r Request) phase(format string, args ...any) {
	if r.OnPhase == nil {
		return
	}
	if len(args) == 0 {
		r.OnPhase(format)
		return
	}
	r.OnPhase(fmt.Sprintf(format, args...))
}

// Response is a successful fetch. Body must always be closed by the caller:
// closing releases the host's connection slot.
type Response struct {
	URL           *url.URL
	FinalURL      *url.URL
	Status        int
	Header        http.Header
	Body          io.ReadCloser
	MediaType     string
	Charset       string
	Filename      string
	ContentLength int64
	NotModified   bool
	Redirects     []*url.URL
	Attempts      int
	Elapsed       time.Duration
}

// Stats are cumulative counters for the UI.
type Stats struct {
	Requests    int64
	Retries     int64
	BytesIn     int64
	Redirects   int64
	Conditional int64
	NotModified int64
}

// Client is the crawler's HTTP front end.
type Client struct {
	cfgPtr atomic.Pointer[config.Config]
	gate   *netwatch.Gate
	hc     *http.Client
	hosts  *hostLimiter
	rate   *rateLimiter
	pause  *pauseGate

	// settingsMu guards the fields derived from the config, which can be
	// replaced while requests are in flight.
	settingsMu  sync.RWMutex
	userAgent   string
	retryStatus map[int]bool

	stats struct {
		requests    atomic.Int64
		retries     atomic.Int64
		bytesIn     atomic.Int64
		redirects   atomic.Int64
		conditional atomic.Int64
		notModified atomic.Int64
	}

	authMu sync.RWMutex
	auth   []config.BasicAuth
}

// New builds a Client from a normalized Config.
func New(cfg *config.Config, gate *netwatch.Gate) (*Client, error) {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{
		Timeout:   20 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   cfg.Request.PerHostConns + 2,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		ForceAttemptHTTP2:     cfg.Request.HTTP2,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: cfg.Request.InsecureTLS},
	}
	if cfg.Request.Proxy != "" {
		pu, err := url.Parse(cfg.Request.Proxy)
		if err != nil {
			return nil, fmt.Errorf("proxy: %w", err)
		}
		tr.Proxy = http.ProxyURL(pu)
	}

	// The hosts a job was pointed at are expected to work, so an outage that
	// begins before anything has succeeded is still recognized as an outage
	// rather than a dead address. Establishing this here means every Client
	// gets it, not only the ones the crawl engine builds.
	gate.TrustHosts(cfg.StartHosts()...)

	c := &Client{
		gate:        gate,
		userAgent:   config.ResolveUserAgent(cfg.Request.UserAgent),
		retryStatus: map[int]bool{},
		auth:        append([]config.BasicAuth(nil), cfg.Request.Auth...),
	}
	c.cfgPtr.Store(cfg)
	for _, s := range cfg.Resilience.RetryStatuses {
		c.retryStatus[s] = true
	}

	maxRedirects := cfg.Request.FollowRedirect
	c.hc = &http.Client{
		Transport: tr,
		Jar:       jar,
		Timeout:   0, // per-request contexts carry the deadline instead
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			// Carry our headers across the redirect; net/http drops custom ones
			// when the host changes.
			for k, v := range via[0].Header {
				if _, ok := req.Header[k]; !ok && !strings.EqualFold(k, "Authorization") {
					req.Header[k] = v
				}
			}
			return nil
		},
	}

	c.hosts = newHostLimiter(
		cfg.Request.PerHostConns,
		cfg.Request.Delay.D(),
		cfg.Request.DelayJitter.D(),
		cfg.Resilience.ThrottleOn429,
	)
	c.rate = newRateLimiter(cfg.Limits.MaxRate.V())
	c.pause = newPauseGate()

	if err := c.loadCookies(); err != nil {
		return nil, err
	}
	return c, nil
}

// cfg returns the settings currently in force.
func (c *Client) cfg() *config.Config { return c.cfgPtr.Load() }

// Reconfigure adopts new settings without disturbing requests already running.
// Anything fixed at construction — the transport, the cookie jar, the proxy —
// keeps its original value; those need a new Client, and the caller is told so.
func (c *Client) Reconfigure(cfg *config.Config) {
	c.settingsMu.Lock()
	c.userAgent = config.ResolveUserAgent(cfg.Request.UserAgent)
	c.retryStatus = map[int]bool{}
	for _, s := range cfg.Resilience.RetryStatuses {
		c.retryStatus[s] = true
	}
	c.settingsMu.Unlock()

	c.authMu.Lock()
	c.auth = append([]config.BasicAuth(nil), cfg.Request.Auth...)
	c.authMu.Unlock()

	c.hosts.setBase(cfg.Request.PerHostConns, cfg.Request.Delay.D(), cfg.Request.DelayJitter.D())
	c.rate.SetRate(cfg.Limits.MaxRate.V())
	c.cfgPtr.Store(cfg)
}

// userAgentValue reads the current identification string.
func (c *Client) userAgentValue() string {
	c.settingsMu.RLock()
	defer c.settingsMu.RUnlock()
	return c.userAgent
}

func (c *Client) shouldRetryStatus(code int) bool {
	c.settingsMu.RLock()
	defer c.settingsMu.RUnlock()
	return c.retryStatus[code]
}

// requestTimeout is how long to wait for this particular request's headers.
func (c *Client) requestTimeout(req Request) time.Duration {
	cfg := c.cfg()
	if req.Offsite {
		if t := cfg.Request.OffsiteTimeout.D(); t > 0 {
			return t
		}
	}
	return cfg.Request.Timeout.D()
}

// HostKey is the pacing bucket for a URL: scheme, host and port.
func HostKey(u *url.URL) string { return u.Scheme + "://" + u.Host }

// Do fetches a URL, retrying according to the resilience settings. It returns
// a Response whose Body the caller must close, or an *Error.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	method := req.Method
	if method == "" {
		if req.NoBody {
			method = http.MethodHead
		} else {
			method = http.MethodGet
		}
	}
	target := req.URL.String()
	hostKey := HostKey(req.URL)

	attempts := c.cfg().Request.Attempts
	if req.Offsite && c.cfg().Request.OffsiteAttempts > 0 {
		attempts = c.cfg().Request.OffsiteAttempts
	}
	if attempts < 1 {
		attempts = 1
	}
	// networkWaits are retries caused by the network being down rather than by
	// anything wrong with this URL, so they do not consume the URL's budget.
	const maxNetworkWaits = 100000

	attempt := 0
	networkWaits := 0
	start := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			return nil, &Error{URL: target, Kind: KindCanceled, Err: err, Attempts: attempt}
		}
		// Park here for as long as the network is down. This is what turns a
		// dropped connection into a pause instead of a failed job.
		if !c.gate.Online() {
			req.phase("waiting for network")
		}
		if err := c.gate.Wait(ctx); err != nil {
			return nil, &Error{URL: target, Kind: KindCanceled, Err: err, Attempts: attempt}
		}

		attempt++
		if attempt > 1 {
			req.phase("retry %d", attempt)
		} else {
			req.phase("queued")
		}
		release, err := c.hosts.acquire(ctx, hostKey)
		if err != nil {
			return nil, &Error{URL: target, Kind: KindCanceled, Err: err, Attempts: attempt}
		}

		resp, ferr := c.attempt(ctx, method, req)
		c.stats.requests.Add(1)

		if ferr == nil {
			// Success path: the host slot stays held until the body is closed.
			c.gate.ReportSuccess(req.URL.Hostname())
			c.hosts.reward(hostKey)
			resp.Attempts = attempt
			resp.Elapsed = time.Since(start)
			resp.Body = &guardedBody{
				rc:      resp.Body,
				release: release,
				onRead:  func(n int) { c.stats.bytesIn.Add(int64(n)) },
				rate:    c.rate,
				pause:   c.pause,
				limit:   c.bodyLimit(resp.MediaType),
			}
			return resp, nil
		}
		release()

		fe, _ := ferr.(*Error)
		if fe == nil {
			fe = &Error{URL: target, Kind: KindTransport, Err: ferr}
		}
		switch fe.Kind {
		case KindCanceled:
			fe.Attempts = attempt
			return nil, fe

		case KindTransport:
			wentOffline := c.gate.ReportFailure(req.URL.Hostname(), fe.Err)
			if c.cfg().Resilience.NeverGiveUp && (wentOffline || !c.gate.Online()) {
				// The problem is the network, not this URL. Give the attempt
				// back and wait for the gate on the next loop.
				attempt--
				networkWaits++
				if networkWaits > maxNetworkWaits {
					fe.Attempts = attempt
					return nil, fe
				}
				continue
			}

		case KindHTTP:
			if fe.Status == http.StatusTooManyRequests || fe.Status == http.StatusServiceUnavailable {
				c.hosts.penalize(hostKey, retryAfter(fe))
			}
			if !c.shouldRetryStatus(fe.Status) {
				fe.Attempts = attempt
				return nil, fe
			}
		default:
			fe.Attempts = attempt
			return nil, fe
		}

		if attempt >= attempts {
			fe.Attempts = attempt
			return nil, fe
		}
		c.stats.retries.Add(1)
		wait := c.backoff(attempt, fe)
		if wait > time.Second {
			req.phase("waiting %s to retry", wait.Round(time.Second))
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, &Error{URL: target, Kind: KindCanceled, Err: err, Attempts: attempt}
		}
	}
}

// attempt performs one request with no retry logic.
func (c *Client) attempt(ctx context.Context, method string, req Request) (*Response, error) {
	target := req.URL.String()

	reqCtx := ctx
	var cancel context.CancelFunc
	if t := c.requestTimeout(req); t > 0 {
		// The deadline covers headers only; the body reader gets its own
		// budget so a big slow file is not killed mid-download.
		reqCtx, cancel = context.WithCancel(ctx)
		timer := time.AfterFunc(t, cancel)
		defer timer.Stop()
	}

	req.phase("connecting")
	hr, err := http.NewRequestWithContext(reqCtx, method, target, nil)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, &Error{URL: target, Kind: KindHTTP, Err: err}
	}
	c.applyHeaders(hr, req)

	resp, err := c.hc.Do(hr)
	if err == nil {
		req.phase("receiving")
	}
	if err != nil {
		if cancel != nil {
			cancel()
		}
		if ctx.Err() != nil {
			return nil, &Error{URL: target, Kind: KindCanceled, Err: ctx.Err()}
		}
		kind := KindHTTP
		if netwatch.IsTransportError(err) {
			kind = KindTransport
		}
		return nil, &Error{URL: target, Kind: kind, Err: unwrapURLError(err)}
	}

	out := &Response{
		URL:           req.URL,
		FinalURL:      resp.Request.URL,
		Status:        resp.StatusCode,
		Header:        resp.Header,
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
		NotModified:   resp.StatusCode == http.StatusNotModified,
	}
	if out.FinalURL != nil && out.FinalURL.String() != target {
		c.stats.redirects.Add(1)
		out.Redirects = append(out.Redirects, out.FinalURL)
	}
	out.MediaType, out.Charset = parseContentType(resp.Header.Get("Content-Type"))
	out.Filename = contentDispositionFilename(resp.Header.Get("Content-Disposition"))

	if out.NotModified {
		c.stats.notModified.Add(1)
		resp.Body.Close()
		out.Body = io.NopCloser(strings.NewReader(""))
		return out, nil
	}

	if resp.StatusCode == http.StatusUnauthorized && c.cfg().General.Login != config.LoginNever {
		if user, pass, ok := c.credentialsFor(req.URL); ok {
			resp.Body.Close()
			if cancel != nil {
				cancel()
			}
			return c.attemptWithAuth(ctx, method, req, user, pass)
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if !c.cfg().General.DownloadErrorPages {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if cancel != nil {
				cancel()
			}
			return nil, &Error{URL: target, Kind: KindHTTP, Status: resp.StatusCode,
				After: ParseRetryAfter(resp.Header.Get("Retry-After")),
				Err:   errors.New(http.StatusText(resp.StatusCode))}
		}
		// Error pages are kept when asked for, but a retryable status still
		// gets retried first.
		if c.shouldRetryStatus(resp.StatusCode) {
			resp.Body.Close()
			if cancel != nil {
				cancel()
			}
			return nil, &Error{URL: target, Kind: KindHTTP, Status: resp.StatusCode,
				After: ParseRetryAfter(resp.Header.Get("Retry-After")),
				Err:   errors.New(http.StatusText(resp.StatusCode))}
		}
	}
	return out, nil
}

// attemptWithAuth repeats a request carrying basic credentials.
func (c *Client) attemptWithAuth(ctx context.Context, method string, req Request, user, pass string) (*Response, error) {
	hr, err := http.NewRequestWithContext(ctx, method, req.URL.String(), nil)
	if err != nil {
		return nil, &Error{URL: req.URL.String(), Kind: KindHTTP, Err: err}
	}
	c.applyHeaders(hr, req)
	hr.SetBasicAuth(user, pass)

	resp, err := c.hc.Do(hr)
	if err != nil {
		kind := KindHTTP
		if netwatch.IsTransportError(err) {
			kind = KindTransport
		}
		return nil, &Error{URL: req.URL.String(), Kind: kind, Err: unwrapURLError(err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &Error{URL: req.URL.String(), Kind: KindHTTP, Status: resp.StatusCode,
			After: ParseRetryAfter(resp.Header.Get("Retry-After")),
			Err:   errors.New(http.StatusText(resp.StatusCode))}
	}
	out := &Response{
		URL: req.URL, FinalURL: resp.Request.URL, Status: resp.StatusCode,
		Header: resp.Header, Body: resp.Body, ContentLength: resp.ContentLength,
	}
	out.MediaType, out.Charset = parseContentType(resp.Header.Get("Content-Type"))
	out.Filename = contentDispositionFilename(resp.Header.Get("Content-Disposition"))
	return out, nil
}

func (c *Client) applyHeaders(hr *http.Request, req Request) {
	if ua := c.userAgentValue(); ua != "" {
		hr.Header.Set("User-Agent", ua)
	}
	hr.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	hr.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// Explicitly asking for identity would defeat compression; net/http adds
	// gzip and decodes transparently, which is what we want.
	if c.cfg().Request.SendReferer && req.Referer != "" {
		hr.Header.Set("Referer", req.Referer)
	}
	for k, v := range c.cfg().Request.Headers {
		hr.Header.Set(k, v)
	}
	if req.ETag != "" {
		hr.Header.Set("If-None-Match", req.ETag)
		c.stats.conditional.Add(1)
	}
	if req.LastModified != "" {
		hr.Header.Set("If-Modified-Since", req.LastModified)
		c.stats.conditional.Add(1)
	}
	if user, pass, ok := c.credentialsFor(req.URL); ok && c.cfg().General.Login == config.LoginAuto {
		hr.SetBasicAuth(user, pass)
	}
}

func (c *Client) credentialsFor(u *url.URL) (string, string, bool) {
	if u.User != nil {
		pass, _ := u.User.Password()
		return u.User.Username(), pass, true
	}
	c.authMu.RLock()
	defer c.authMu.RUnlock()
	host := strings.ToLower(u.Hostname())
	for _, a := range c.auth {
		if a.Username == "" {
			continue
		}
		if a.Host == "" || strings.EqualFold(a.Host, host) || strings.HasSuffix(host, strings.TrimPrefix(strings.ToLower(a.Host), "*")) {
			return a.Username, a.Password, true
		}
	}
	return "", "", false
}

// AddCredentials registers credentials at runtime, used by the interactive
// login prompt and the web UI.
func (c *Client) AddCredentials(host, user, pass string) {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	c.auth = append(c.auth, config.BasicAuth{Host: host, Username: user, Password: pass})
}

// bodyLimit is the byte cap for a response body, or 0 for unlimited. HTML and
// CSS are never capped because the mirror's structure depends on them.
func (c *Client) bodyLimit(mediaType string) int64 {
	if c.cfg().Limits.MaxFileSize <= 0 {
		return 0
	}
	mt := strings.ToLower(mediaType)
	for _, t := range append(append([]string{}, c.cfg().Types.HTMLTypes...), c.cfg().Types.CSSTypes...) {
		if strings.EqualFold(t, mt) {
			return 0
		}
	}
	return c.cfg().Limits.MaxFileSize.V()
}

// backoff computes the wait before the next attempt, honouring Retry-After.
func (c *Client) backoff(attempt int, fe *Error) time.Duration {
	if c.cfg().Resilience.HonorRetryAfter {
		if d := retryAfter(fe); d > 0 {
			if maxd := c.cfg().Resilience.RetryMaxDelay.D(); d > maxd {
				return maxd
			}
			return d
		}
	}
	base := c.cfg().Resilience.RetryBaseDelay.D()
	if base <= 0 {
		base = 2 * time.Second
	}
	d := base
	for i := 1; i < attempt && d < c.cfg().Resilience.RetryMaxDelay.D(); i++ {
		d *= 2
	}
	if maxd := c.cfg().Resilience.RetryMaxDelay.D(); maxd > 0 && d > maxd {
		d = maxd
	}
	return d
}

func retryAfter(fe *Error) time.Duration {
	if fe == nil {
		return 0
	}
	return fe.After
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ParseRetryAfter reads the header in both its seconds and HTTP-date forms.
func ParseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func parseContentType(v string) (mediaType, charset string) {
	if v == "" {
		return "", ""
	}
	mt, params, err := mime.ParseMediaType(v)
	if err != nil {
		if i := strings.Index(v, ";"); i > 0 {
			return strings.ToLower(strings.TrimSpace(v[:i])), ""
		}
		return strings.ToLower(strings.TrimSpace(v)), ""
	}
	return strings.ToLower(mt), params["charset"]
}

func contentDispositionFilename(v string) string {
	if v == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	if f := params["filename*"]; f != "" {
		return f
	}
	return params["filename"]
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// Stats returns a snapshot of the counters.
func (c *Client) Stats() Stats {
	return Stats{
		Requests:    c.stats.requests.Load(),
		Retries:     c.stats.retries.Load(),
		BytesIn:     c.stats.bytesIn.Load(),
		Redirects:   c.stats.redirects.Load(),
		Conditional: c.stats.conditional.Load(),
		NotModified: c.stats.notModified.Load(),
	}
}

// SetPaused holds or releases every transfer in progress, so pausing takes
// effect immediately rather than once the current files finish.
func (c *Client) SetPaused(paused bool) { c.pause.Set(paused) }

// ReleasePause permanently opens the pause gate, so a stopping job cannot leave
// a reader parked forever.
func (c *Client) ReleasePause() { c.pause.Release() }

// SetRateLimit changes the total throughput cap while the crawl runs; 0 lifts
// it entirely.
func (c *Client) SetRateLimit(bytesPerSec int64) { c.rate.SetRate(bytesPerSec) }

// RateLimit returns the current throughput cap, 0 meaning unlimited.
func (c *Client) RateLimit() int64 { return c.rate.Rate() }

// HostPacing exposes per-host pacing for the UI.
func (c *Client) HostPacing() map[string]HostPace { return c.hosts.snapshot() }

// SetCrawlDelay raises a host's minimum delay, used for robots.txt Crawl-delay.
func (c *Client) SetCrawlDelay(u *url.URL, d time.Duration) { c.hosts.setDelay(HostKey(u), d) }

// Jar exposes the cookie jar so the web UI can show or seed a session.
func (c *Client) Jar() http.CookieJar { return c.hc.Jar }

// loadCookies seeds the jar from a raw "a=1; b=2" string or a Netscape-format
// cookies.txt, which is how most people export a logged-in session.
func (c *Client) loadCookies() error {
	if c.cfg().Request.CookiesRaw != "" {
		for _, s := range c.cfg().StartURLs {
			u, err := url.Parse(s)
			if err != nil {
				continue
			}
			var cookies []*http.Cookie
			for _, part := range strings.Split(c.cfg().Request.CookiesRaw, ";") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				eq := strings.IndexByte(part, '=')
				if eq <= 0 {
					continue
				}
				cookies = append(cookies, &http.Cookie{
					Name:  strings.TrimSpace(part[:eq]),
					Value: strings.TrimSpace(part[eq+1:]),
					Path:  "/",
				})
			}
			if len(cookies) > 0 {
				c.hc.Jar.SetCookies(u, cookies)
			}
		}
	}
	if c.cfg().Request.CookieJarFile != "" {
		return c.loadCookieFile(c.cfg().Request.CookieJarFile)
	}
	return nil
}

// guardedBody releases the host slot and accounts bytes when the body closes.
type guardedBody struct {
	rc      io.ReadCloser
	release func()
	onRead  func(int)
	rate    *rateLimiter
	pause   *pauseGate
	limit   int64
	read    int64
	once    sync.Once
	err     error
}

// ErrTooLarge is returned from a body read that exceeds the size cap.
var ErrTooLarge = errors.New("response exceeded the maximum file size")

func (g *guardedBody) Read(p []byte) (int, error) {
	if g.err != nil {
		return 0, g.err
	}
	if g.pause != nil {
		g.pause.Wait()
	}
	if g.limit > 0 && g.read >= g.limit {
		g.err = ErrTooLarge
		return 0, g.err
	}
	if g.limit > 0 && int64(len(p)) > g.limit-g.read {
		p = p[:g.limit-g.read]
	}
	n, err := g.rc.Read(p)
	if n > 0 {
		g.read += int64(n)
		if g.onRead != nil {
			g.onRead(n)
		}
		if g.rate != nil {
			g.rate.take(n)
		}
	}
	return n, err
}

func (g *guardedBody) Close() error {
	var err error
	g.once.Do(func() {
		err = g.rc.Close()
		if g.release != nil {
			g.release()
		}
	})
	return err
}
