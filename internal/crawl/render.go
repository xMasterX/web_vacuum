package crawl

import (
	"bytes"
	"context"
	"net/url"
	"regexp"
	"sync"

	"github.com/xMasterX/web_vacuum/internal/parse"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/render"
	"github.com/xMasterX/web_vacuum/internal/rules"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

// renderer owns the browser for a job. It is started lazily, on the first page
// that actually needs it, so a crawl configured with "auto" that never meets a
// JavaScript-built page never pays for launching Chrome.
type renderer struct {
	cfg   *config.RenderConfig
	match []*regexp.Regexp
	skip  []*regexp.Regexp

	once    sync.Once
	browser *render.Browser
	startEr error

	// failures counts consecutive render failures. Sustained failure means the
	// browser has become unusable, and falling back to plain HTTP for the rest
	// of the crawl beats stalling on every page.
	mu        sync.Mutex
	failures  int
	disabled  bool
	rendered  int
	fallbacks int
}

func newRenderer(cfg *config.RenderConfig) (*renderer, error) {
	r := &renderer{cfg: cfg}
	for _, p := range cfg.Match {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		r.match = append(r.match, re)
	}
	for _, p := range cfg.Skip {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		r.skip = append(r.skip, re)
	}
	return r, nil
}

// wants decides whether a page should go through the browser. The raw body is
// consulted for "auto", because the only reliable signal that a page needs
// JavaScript is that its markup contains almost nothing.
func (r *renderer) wants(u *url.URL, body []byte) bool {
	r.mu.Lock()
	if r.disabled {
		r.mu.Unlock()
		return false
	}
	r.mu.Unlock()

	full := u.String()
	for _, re := range r.skip {
		if re.MatchString(full) {
			return false
		}
	}
	for _, re := range r.match {
		if re.MatchString(full) {
			return true
		}
	}
	switch r.cfg.Mode {
	case config.RenderAlways:
		return true
	case config.RenderAuto:
		// With an explicit match list, "auto" means those URLs and nothing else.
		if len(r.match) > 0 {
			return false
		}
		return LooksJSDriven(body)
	}
	return false
}

// start brings the browser up once, sharing the outcome with every worker.
func (r *renderer) start(ctx context.Context) (*render.Browser, error) {
	r.once.Do(func() {
		r.browser, r.startEr = render.Launch(ctx, render.Options{
			ExecPath:     r.cfg.ChromePath,
			RemoteURL:    r.cfg.RemoteURL,
			Tabs:         r.cfg.Tabs,
			Headful:      r.cfg.Headful,
			NoSandbox:    r.cfg.NoSandbox,
			WindowWidth:  r.cfg.WindowWidth,
			WindowHeight: r.cfg.WindowHeight,
		})
	})
	return r.browser, r.startEr
}

func (r *renderer) close() {
	if r.browser != nil {
		r.browser.Close()
	}
}

// noteFailure records a failed render and disables the browser after a run of
// them, so one broken browser cannot stall an entire crawl.
func (r *renderer) noteFailure() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures++
	r.fallbacks++
	if r.failures >= 5 && !r.disabled {
		r.disabled = true
		return true
	}
	return false
}

func (r *renderer) noteSuccess() {
	r.mu.Lock()
	r.failures = 0
	r.rendered++
	r.mu.Unlock()
}

func (r *renderer) stats() (rendered, fallbacks int, disabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rendered, r.fallbacks, r.disabled
}

// emptyMountPoints are the container elements single-page frameworks leave in
// otherwise empty markup.
var emptyMountPoints = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<div[^>]+id=["']?(root|app|__next|___gatsby|application)["']?[^>]*>\s*</div>`),
	regexp.MustCompile(`(?is)<div[^>]+data-reactroot`),
	regexp.MustCompile(`(?is)<[^>]+ng-app`),
	regexp.MustCompile(`(?is)<noscript>[^<]*(enable|turn on)[^<]*javascript`),
}

// rawTextStrippers remove elements whose contents are not visible text. Go's
// regexp engine has no backreferences, so each element gets its own pattern
// rather than one clever expression.
var rawTextStrippers = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>`),
	regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style\s*>`),
	regexp.MustCompile(`(?is)<template\b[^>]*>.*?</template\s*>`),
	regexp.MustCompile(`(?is)<svg\b[^>]*>.*?</svg\s*>`),
	regexp.MustCompile(`(?is)<noscript\b[^>]*>.*?</noscript\s*>`),
	regexp.MustCompile(`(?is)<head\b[^>]*>.*?</head\s*>`),
	regexp.MustCompile(`(?is)<!--.*?-->`),
}

var (
	anyTag          = regexp.MustCompile(`(?s)<[^>]*>`)
	whitespaceRunes = regexp.MustCompile(`\s+`)
)

// LooksJSDriven reports whether a document appears to be an empty shell that
// builds itself in the browser.
//
// The test is deliberately conservative: rendering is expensive, so a page is
// only sent to the browser when its markup carries so little text that a reader
// would see a blank page. A content-bearing page that also happens to use
// JavaScript is left alone.
func LooksJSDriven(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// Only used for a containment check, but folding ASCII avoids re-encoding a
	// whole page that is not UTF-8 just to look for one tag.
	lower := parse.AsciiLower(body)
	if !bytes.Contains(lower, []byte("<script")) {
		// Without script there is nothing for a browser to run that a parser
		// would miss.
		return false
	}

	stripped := body
	for _, re := range rawTextStrippers {
		stripped = re.ReplaceAll(stripped, []byte(" "))
	}
	text := anyTag.ReplaceAll(stripped, []byte(" "))
	text = whitespaceRunes.ReplaceAll(text, []byte(" "))
	visible := len(bytes.TrimSpace(text))

	for _, re := range emptyMountPoints {
		if re.Match(body) {
			return true
		}
	}
	// A page whose entire visible text would fit in a tweet, yet which ships
	// script, is almost certainly assembled client-side.
	return visible < 200
}

// renderPage runs a page through the browser and returns the rendered markup
// plus every URL the page requested. A nil result means the caller should carry
// on with the bytes the server sent.
func (e *Engine) renderPage(ctx context.Context, slot int, u *url.URL) (*render.Result, bool) {
	if e.renderer == nil {
		return nil, false
	}
	e.setSlotActivity(slot, "rendering")

	browser, err := e.renderer.start(ctx)
	if err != nil {
		if e.renderer.noteFailure() {
			e.logf(LevelWarn, "JavaScript rendering is unavailable (%v); continuing without it", err)
		}
		return nil, false
	}

	req := render.Request{
		URL:           u.String(),
		UserAgent:     config.ResolveUserAgent(e.cfg().Request.UserAgent),
		Wait:          e.cfg().Render.Wait.D(),
		IdleWindow:    e.cfg().Render.Idle.D(),
		Timeout:       e.cfg().Render.Timeout.D(),
		Scroll:        e.cfg().Render.Scroll,
		ScrollLimit:   e.cfg().Render.ScrollLimit,
		BlockPatterns: e.cfg().Render.Block,
		Headers:       e.cfg().Request.Headers,
	}
	// A logged-in session has to carry over, or rendering a members-only site
	// captures the login page instead of the content.
	if jar := e.client.Jar(); jar != nil {
		req.Cookies = jar.Cookies(u)
	}

	res, err := browser.Render(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false
		}
		if e.renderer.noteFailure() {
			e.logf(LevelWarn, "JavaScript rendering keeps failing (%v); continuing without it", err)
		} else {
			e.logf(LevelDebug, "render %s: %v", u, err)
		}
		return nil, false
	}
	e.renderer.noteSuccess()
	return res, true
}

// queueRendered adds the URLs a rendered page requested. These are subresources
// the markup never mentions, which is the single biggest thing a plain download
// misses on a modern site.
func (e *Engine) queueRendered(item *Item, docURL *url.URL, res *render.Result) int {
	if res == nil || !e.cfg().Render.FollowRequests {
		return 0
	}
	added := 0
	referer := docURL.String()
	for _, raw := range res.Requests {
		abs, err := urlx.Parse(raw)
		if err != nil {
			continue
		}
		if abs.String() == docURL.String() {
			continue
		}
		role := rules.GuessRole(abs)
		depth := item.Depth
		if role == rules.RolePage {
			depth++
		}
		if e.enqueue(abs, depth, role, referer, docURL, false) {
			added++
		}
	}
	return added
}

// renderTimeoutNote keeps the log honest about pages that never went quiet.
func renderTimeoutNote(res *render.Result) string {
	if res != nil && res.TimedOut {
		return " (page never went idle; captured as-is)"
	}
	return ""
}

// stripScripts removes script elements from a rendered document.
//
// A rendered page is a snapshot of a DOM that scripts already finished
// building. Leaving those scripts in place means they run again when the file
// is opened, wipe the content they previously produced, and fail to rebuild it
// because the API calls they depend on no longer resolve. Removing them is what
// turns a render into an archive.
func stripScripts(src []byte) []byte {
	ranges := parse.ElementRanges(src, "script")
	if len(ranges) == 0 {
		return src
	}
	reps := make([]parse.Replacement, 0, len(ranges))
	for _, r := range ranges {
		reps = append(reps, parse.Replacement{Start: r.Start, End: r.End, Text: ""})
	}
	return parse.Apply(src, reps)
}
