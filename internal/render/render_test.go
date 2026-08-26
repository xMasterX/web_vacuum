package render

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// requireChrome skips rather than fails when no browser is installed: rendering
// is an optional feature and its absence must not break a normal test run.
func requireChrome(t *testing.T) string {
	t.Helper()
	exe, err := FindChrome("")
	if err != nil {
		t.Skipf("no browser available: %v", err)
	}
	return exe
}

func TestRenderRunsJavaScript(t *testing.T) {
	requireChrome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			// Nothing useful exists in the source: the content, a link and an
			// image are all created by script, and one image arrives later
			// still. This is the shape of site the renderer exists for.
			fmt.Fprint(w, `<!doctype html><html><head><title>App</title></head>
<body><div id="app">loading…</div>
<script>
  document.getElementById('app').innerHTML =
    '<h1>Rendered heading</h1><a href="/generated-page.html">generated link</a>' +
    '<img src="/generated.png">';
  setTimeout(function () {
    var i = document.createElement('img');
    i.src = '/deferred.png';
    document.body.appendChild(i);
  }, 250);
</script></body></html>`)
		case "/generated.png", "/deferred.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG\r\n\x1a\nX"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := Launch(ctx, Options{Tabs: 1, NoSandbox: true})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer b.Close()
	t.Logf("browser: %s", b.Version(ctx))

	res, err := b.Render(ctx, Request{
		URL:        srv.URL + "/",
		Wait:       600 * time.Millisecond,
		IdleWindow: 400 * time.Millisecond,
		Timeout:    60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if !strings.Contains(res.HTML, "Rendered heading") {
		t.Errorf("script-generated content missing from the DOM:\n%s", truncate(res.HTML, 600))
	}
	if !strings.Contains(res.HTML, "/generated-page.html") {
		t.Errorf("script-generated link missing, so the crawler could never follow it")
	}
	if res.Status != 200 {
		t.Errorf("Status = %d, want 200", res.Status)
	}
	if !strings.Contains(res.FinalURL, srv.URL) {
		t.Errorf("FinalURL = %q", res.FinalURL)
	}

	// The request list is what lets the crawler find files that never appear in
	// any markup.
	var sawGenerated, sawDeferred bool
	for _, u := range res.Requests {
		if strings.HasSuffix(u, "/generated.png") {
			sawGenerated = true
		}
		if strings.HasSuffix(u, "/deferred.png") {
			sawDeferred = true
		}
	}
	if !sawGenerated {
		t.Errorf("script-loaded image not reported: %v", res.Requests)
	}
	if !sawDeferred {
		t.Errorf("deferred image not reported; the settle window may be too short: %v", res.Requests)
	}
	t.Logf("rendered in %s, %d requests observed", res.Elapsed.Round(time.Millisecond), len(res.Requests))
}

func TestRenderScrollTriggersLazyLoading(t *testing.T) {
	requireChrome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/lazy") {
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG\r\n\x1a\nX"))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		// Images load only once they scroll into view, which is how nearly
		// every image-heavy forum and gallery behaves.
		fmt.Fprint(w, `<!doctype html><html><body>
<div style="height:4000px">tall</div>
<img data-src="/lazy-1.png" class="lazy"><div style="height:2000px"></div>
<img data-src="/lazy-2.png" class="lazy">
<script>
  var io = new IntersectionObserver(function (entries) {
    entries.forEach(function (e) {
      if (e.isIntersecting) { e.target.src = e.target.dataset.src; io.unobserve(e.target); }
    });
  });
  document.querySelectorAll('.lazy').forEach(function (el) { io.observe(el); });
</script></body></html>`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	b, err := Launch(ctx, Options{Tabs: 1, NoSandbox: true})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer b.Close()

	withoutScroll, err := b.Render(ctx, Request{URL: srv.URL + "/", IdleWindow: 400 * time.Millisecond})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	withScroll, err := b.Render(ctx, Request{
		URL: srv.URL + "/", Scroll: true,
		IdleWindow: 400 * time.Millisecond, Wait: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Render with scroll: %v", err)
	}

	count := func(r *Result) int {
		n := 0
		for _, u := range r.Requests {
			if strings.Contains(u, "/lazy-") {
				n++
			}
		}
		return n
	}
	plain, scrolled := count(withoutScroll), count(withScroll)
	t.Logf("lazy images seen: %d without scrolling, %d with", plain, scrolled)
	if scrolled <= plain {
		t.Errorf("scrolling should have revealed more lazy images (%d vs %d)", scrolled, plain)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
