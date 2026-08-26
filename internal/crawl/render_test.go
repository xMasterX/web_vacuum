package crawl

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/render"
)

// jsSite serves a page whose entire content — a link and two images — is
// created by script, so a plain download captures an empty shell.
func jsSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><head><title>App</title></head>
<body><div id="root"></div>
<script>
  document.getElementById('root').innerHTML =
    '<a href="/built-by-js.html">deeper</a><img src="/js-image.png">';
  fetch('/api/data.json');
</script></body></html>`)
	})
	mux.HandleFunc("/built-by-js.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><body><h1>Only reachable through rendering</h1>
<a href="/second-level.html">further</a></body></html>`)
	})
	mux.HandleFunc("/second-level.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><body><p>second level</p></body></html>`)
	})
	mux.HandleFunc("/js-image.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG\r\n\x1a\nJS"))
	})
	mux.HandleFunc("/api/data.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"loaded":"by fetch"}`)
	})
	return srv
}

// TestRenderingFindsWhatPlainDownloadCannot is the case for the feature: the
// same site is crawled twice, and the difference is the point.
func TestRenderingFindsWhatPlainDownloadCannot(t *testing.T) {
	if _, err := render.FindChrome(""); err != nil {
		t.Skipf("no browser available: %v", err)
	}
	srv := jsSite(t)

	// Without rendering, the crawler sees an empty shell.
	plainDest := t.TempDir()
	plain := runEngine(t, testConfig(t, srv.URL+"/", plainDest))
	defer plain.Close()

	for _, unreachable := range []string{"built-by-js.html", "js-image.png"} {
		if _, err := os.Stat(filepath.Join(plainDest, unreachable)); err == nil {
			t.Errorf("%s should be unreachable without rendering", unreachable)
		}
	}

	// With rendering, the script-built link is followed, its own links are
	// followed in turn, and the files the page fetched are downloaded.
	renderDest := t.TempDir()
	cfg := testConfig(t, srv.URL+"/", renderDest)
	cfg.Render.Mode = config.RenderAuto
	cfg.Render.Tabs = 1
	cfg.Render.NoSandbox = true
	cfg.Render.Wait = config.Duration(500 * time.Millisecond)
	cfg.Render.Idle = config.Duration(500 * time.Millisecond)
	cfg.Render.Timeout = config.Duration(60 * time.Second)
	cfg.Render.FollowRequests = true

	e := runEngine(t, cfg)
	defer e.Close()

	for _, want := range []string{
		"built-by-js.html",  // link that exists only after script runs
		"second-level.html", // proves the rendered page was itself rescanned
		"js-image.png",      // image inserted by script
		"api/data.json",     // fetched by script, in no markup at all
	} {
		if _, err := os.Stat(filepath.Join(renderDest, want)); err != nil {
			t.Errorf("rendering should have found %s: %v", want, err)
		}
	}

	// The saved home page must contain the built DOM, not the empty shell.
	home, err := os.ReadFile(filepath.Join(renderDest, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(home), "built-by-js") {
		t.Errorf("the saved page is still the pre-render shell:\n%s", home)
	}

	rendered, fallbacks, disabled := e.renderer.stats()
	t.Logf("rendered %d page(s), %d fallbacks, disabled=%v", rendered, fallbacks, disabled)
	if rendered == 0 {
		t.Error("no page was actually rendered")
	}
}

func TestLooksJSDriven(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"empty react root", `<html><body><div id="root"></div><script src="/a.js"></script></body></html>`, true},
		{"empty app mount", `<html><body><div id="app"></div><script>go()</script></body></html>`, true},
		{"noscript plea", `<html><body><noscript>Please enable JavaScript</noscript><script>x()</script><p>hi</p></body></html>`, true},
		{"no script at all", `<html><body><div id="root"></div></body></html>`, false},
		{"real content with script", `<html><body><script src="/analytics.js"></script>` +
			`<article><h1>A real page</h1><p>` + longText + `</p></article></body></html>`, false},
		{"empty document", "", false},
	}
	for _, c := range cases {
		if got := LooksJSDriven([]byte(c.body)); got != c.want {
			t.Errorf("%s: LooksJSDriven = %v, want %v", c.name, got, c.want)
		}
	}
}

var longText = func() string {
	s := ""
	for i := 0; i < 40; i++ {
		s += "This paragraph carries genuine article text. "
	}
	return s
}()

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && stringIndex(haystack, needle) >= 0
}

func stringIndex(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
