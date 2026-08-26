package render

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Request is one page to render.
type Request struct {
	URL string
	// Cookies seed the browser so a logged-in session carries over from the
	// crawler's jar.
	Cookies []*http.Cookie
	// Headers are applied to every request the page makes.
	Headers map[string]string
	// UserAgent overrides the browser's own identification.
	UserAgent string
	// Wait is an extra pause after the page settles, for animations and
	// deferred work that no event announces.
	Wait time.Duration
	// IdleWindow is how long the network must be quiet before the page counts
	// as settled.
	IdleWindow time.Duration
	// Timeout bounds the whole render.
	Timeout time.Duration
	// Scroll walks to the bottom of the page first, which is what triggers the
	// lazy-loading that hides most images on modern sites.
	Scroll bool
	// ScrollLimit caps how far the scroll goes, so an infinite feed terminates.
	ScrollLimit int
	// BlockPatterns are URL wildcards the browser refuses to load, for skipping
	// heavy media that does not affect the DOM.
	BlockPatterns []string
}

// Result is what a render produced.
type Result struct {
	// HTML is the serialized DOM after scripts have run.
	HTML string
	// FinalURL is where the page ended up after redirects and history changes.
	FinalURL string
	// Status is the main document's HTTP status, or 0 if it was not observed.
	Status int
	// Requests are every URL the page asked for. This is the real prize: it
	// surfaces images, fonts and API responses that appear nowhere in the
	// source HTML.
	Requests []string
	// Elapsed is how long the render took.
	Elapsed time.Duration
	// TimedOut reports that the page never went quiet and was captured anyway,
	// which is normal for sites with polling or open sockets.
	TimedOut bool
}

// Render loads a URL in a fresh tab and returns the resulting DOM.
func (b *Browser) Render(ctx context.Context, req Request) (*Result, error) {
	if req.Timeout <= 0 {
		req.Timeout = 45 * time.Second
	}
	if req.IdleWindow <= 0 {
		req.IdleWindow = 700 * time.Millisecond
	}
	if req.ScrollLimit <= 0 {
		req.ScrollLimit = 40000
	}

	// One tab at a time per slot: Chrome will happily accept a hundred tabs and
	// then thrash.
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	start := time.Now()

	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := b.conn.call(ctx, "", "Target.createTarget", map[string]any{
		"url": "about:blank",
	}, &created); err != nil {
		return nil, err
	}
	defer func() {
		// The tab is closed on a fresh context: the render's own context is
		// usually already expired by the time cleanup runs.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		b.conn.call(cleanup, "", "Target.closeTarget", map[string]any{"targetId": created.TargetID}, nil)
		cancel()
	}()

	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.conn.call(ctx, "", "Target.attachToTarget", map[string]any{
		"targetId": created.TargetID,
		"flatten":  true,
	}, &attached); err != nil {
		return nil, err
	}
	sid := attached.SessionID

	events, unsubscribe := b.conn.subscribe(sid, 4096)
	defer unsubscribe()

	if err := b.prepare(ctx, sid, req); err != nil {
		return nil, err
	}

	if err := b.conn.call(ctx, sid, "Page.navigate", map[string]any{"url": req.URL}, nil); err != nil {
		return nil, err
	}

	res := &Result{}
	timedOut := b.waitForSettle(ctx, events, res, req.IdleWindow)
	res.TimedOut = timedOut

	if req.Scroll {
		b.scrollThrough(ctx, sid, req.ScrollLimit)
	}
	if req.Wait > 0 {
		select {
		case <-time.After(req.Wait):
		case <-ctx.Done():
		}
	}

	// Draining whatever arrived during the scroll and the extra wait keeps
	// lazy-loaded URLs in the result.
	b.drain(events, res)

	html, finalURL, err := b.capture(ctx, sid)
	if err != nil {
		return nil, err
	}
	res.HTML = html
	if finalURL != "" {
		res.FinalURL = finalURL
	}
	res.Elapsed = time.Since(start)
	res.Requests = dedupe(res.Requests)
	return res, nil
}

// prepare enables the domains the render depends on and applies per-request
// overrides before navigation starts.
func (b *Browser) prepare(ctx context.Context, sid string, req Request) error {
	if err := b.conn.call(ctx, sid, "Page.enable", nil, nil); err != nil {
		return err
	}
	if err := b.conn.call(ctx, sid, "Network.enable", nil, nil); err != nil {
		return err
	}
	if req.UserAgent != "" {
		b.conn.call(ctx, sid, "Network.setUserAgentOverride", map[string]any{"userAgent": req.UserAgent}, nil)
	}
	if len(req.Headers) > 0 {
		headers := map[string]any{}
		for k, v := range req.Headers {
			headers[k] = v
		}
		b.conn.call(ctx, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": headers}, nil)
	}
	if len(req.BlockPatterns) > 0 {
		b.conn.call(ctx, sid, "Network.setBlockedURLs", map[string]any{"urls": req.BlockPatterns}, nil)
	}
	if len(req.Cookies) > 0 {
		cookies := make([]map[string]any, 0, len(req.Cookies))
		for _, c := range req.Cookies {
			entry := map[string]any{"name": c.Name, "value": c.Value, "url": req.URL}
			if c.Domain != "" {
				entry["domain"] = c.Domain
				delete(entry, "url")
			}
			if c.Path != "" {
				entry["path"] = c.Path
			}
			entry["secure"] = c.Secure
			entry["httpOnly"] = c.HttpOnly
			cookies = append(cookies, entry)
		}
		b.conn.call(ctx, sid, "Network.setCookies", map[string]any{"cookies": cookies}, nil)
	}
	return nil
}

// waitForSettle follows the page's network activity until it has been quiet for
// idleWindow, which approximates "the page has finished building itself" far
// better than the load event alone on a JavaScript-driven site.
func (b *Browser) waitForSettle(ctx context.Context, events <-chan event, res *Result, idleWindow time.Duration) bool {
	inFlight := 0
	loaded := false

	idle := time.NewTimer(idleWindow)
	defer idle.Stop()
	stopIdle := func() {
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
	}
	stopIdle()

	armIdle := func() {
		stopIdle()
		if loaded && inFlight <= 0 {
			idle.Reset(idleWindow)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return true

		case <-idle.C:
			return false

		case ev, ok := <-events:
			if !ok {
				return false
			}
			switch ev.Method {
			case "Network.requestWillBeSent":
				var p struct {
					Request struct {
						URL string `json:"url"`
					} `json:"request"`
					RedirectResponse *struct {
						URL string `json:"url"`
					} `json:"redirectResponse"`
					Type string `json:"type"`
				}
				if json.Unmarshal(ev.Params, &p) == nil {
					if p.Request.URL != "" {
						res.Requests = append(res.Requests, p.Request.URL)
					}
					if p.RedirectResponse != nil {
						// A redirect completes one request and starts another,
						// so the in-flight count stays balanced.
						inFlight--
					}
					inFlight++
				}
				stopIdle()

			case "Network.loadingFinished", "Network.loadingFailed":
				inFlight--
				armIdle()

			case "Network.responseReceived":
				var p struct {
					Type     string `json:"type"`
					Response struct {
						URL    string `json:"url"`
						Status int    `json:"status"`
					} `json:"response"`
				}
				if json.Unmarshal(ev.Params, &p) == nil && p.Type == "Document" && res.Status == 0 {
					res.Status = p.Response.Status
					res.FinalURL = p.Response.URL
				}

			case "Page.loadEventFired":
				loaded = true
				armIdle()
			}
		}
	}
}

// drain collects any events that queued up while the renderer was scrolling or
// waiting, without blocking.
func (b *Browser) drain(events <-chan event, res *Result) {
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Method != "Network.requestWillBeSent" {
				continue
			}
			var p struct {
				Request struct {
					URL string `json:"url"`
				} `json:"request"`
			}
			if json.Unmarshal(ev.Params, &p) == nil && p.Request.URL != "" {
				res.Requests = append(res.Requests, p.Request.URL)
			}
		default:
			return
		}
	}
}

// scrollJS walks the page to the bottom in viewport-sized steps and returns
// once it stops growing, then restores the scroll position so the captured DOM
// looks like a fresh visit.
const scrollJS = `new Promise(resolve => {
  const limit = %d;
  let y = 0, stable = 0, lastHeight = 0;
  const step = () => {
    const h = Math.max(document.body ? document.body.scrollHeight : 0,
                       document.documentElement ? document.documentElement.scrollHeight : 0);
    if (h === lastHeight) { stable++; } else { stable = 0; lastHeight = h; }
    if (y >= h || y >= limit || stable > 3) {
      window.scrollTo(0, 0);
      resolve(h);
      return;
    }
    y += window.innerHeight || 800;
    window.scrollTo(0, y);
    setTimeout(step, 120);
  };
  step();
})`

func (b *Browser) scrollThrough(ctx context.Context, sid string, limit int) {
	b.conn.call(ctx, sid, "Runtime.evaluate", map[string]any{
		"expression":    fmt.Sprintf(scrollJS, limit),
		"awaitPromise":  true,
		"returnByValue": true,
	}, nil)
}

// capture serializes the live DOM, which is the whole point: this is the markup
// after scripts have run, not the bytes the server sent.
func (b *Browser) capture(ctx context.Context, sid string) (string, string, error) {
	var out struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	expr := `(function () {
  var doctype = '';
  if (document.doctype) {
    doctype = '<!DOCTYPE ' + document.doctype.name +
      (document.doctype.publicId ? ' PUBLIC "' + document.doctype.publicId + '"' : '') +
      (!document.doctype.publicId && document.doctype.systemId ? ' SYSTEM' : '') +
      (document.doctype.systemId ? ' "' + document.doctype.systemId + '"' : '') + '>\n';
  }
  return JSON.stringify({
    html: doctype + (document.documentElement ? document.documentElement.outerHTML : ''),
    url: location.href
  });
})()`
	if err := b.conn.call(ctx, sid, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
	}, &out); err != nil {
		return "", "", err
	}
	if out.ExceptionDetails != nil {
		return "", "", fmt.Errorf("reading the page: %s", out.ExceptionDetails.Text)
	}

	var raw string
	if err := json.Unmarshal(out.Result.Value, &raw); err != nil {
		return "", "", fmt.Errorf("reading the page: unexpected result")
	}
	var payload struct {
		HTML string `json:"html"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", "", fmt.Errorf("reading the page: %w", err)
	}
	return payload.HTML, payload.URL, nil
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		// Inline data and blob URLs are content, not addresses to fetch.
		if strings.HasPrefix(s, "data:") || strings.HasPrefix(s, "blob:") {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
