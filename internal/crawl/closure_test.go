package crawl

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xMasterX/web_vacuum/internal/state"
)

// TestCrawlReachesFullTransitiveClosure is the guarantee the whole tool rests
// on: every page reachable by following links from the start URL is downloaded,
// pages discovered that way are themselves scanned for more links, and the loop
// continues until nothing new is left — while everything out of scope is
// refused.
//
// The fixture is built so that no page can be found by guessing URLs: the only
// route to page N is a link inside page N-1, so a crawler that fails to rescan
// what it downloaded will visibly stop partway.
func TestCrawlReachesFullTransitiveClosure(t *testing.T) {
	const chainLen = 60

	var mainURL, allowedURL, blockedURL string
	var requested sync.Map

	// A chain: index -> c/1 -> c/2 -> ... -> c/60, each link discoverable only
	// by parsing the page before it.
	mux := http.NewServeMux()
	main := httptest.NewServer(mux)
	defer main.Close()
	mainURL = main.URL

	allowedMux := http.NewServeMux()
	allowed := httptest.NewServer(allowedMux)
	defer allowed.Close()
	allowedURL = allowed.URL

	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store("blocked"+r.URL.Path, true)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>should never be downloaded</body></html>`)
	}))
	defer blocked.Close()
	blockedURL = blocked.URL

	page := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<!doctype html><html><body>%s</body></html>", body)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		requested.Store("/", true)
		// The start page exercises every link-bearing construct at once, so a
		// parser regression shows up as a missing branch rather than silence.
		page(w, fmt.Sprintf(`
<a href="/c/1.html">chain</a>
<iframe src="/frame.html"></iframe>
<map><area href="/area.html"></map>
<a href="%s/other.html">second domain</a>
<a href="%s/secret.html">blocked domain</a>
<img src="/i/pic.png">
<link rel="stylesheet" href="/s/main.css">
`, allowedURL, blockedURL))
	})

	for i := 1; i <= chainLen; i++ {
		i := i
		mux.HandleFunc(fmt.Sprintf("/c/%d.html", i), func(w http.ResponseWriter, r *http.Request) {
			requested.Store(r.URL.Path, true)
			if i == chainLen {
				page(w, "<p>end of the chain</p>")
				return
			}
			page(w, fmt.Sprintf(`<a href="/c/%d.html">next</a>`, i+1))
		})
	}

	mux.HandleFunc("/frame.html", func(w http.ResponseWriter, r *http.Request) {
		requested.Store("/frame.html", true)
		page(w, `<a href="/from-frame.html">deeper</a>`)
	})
	mux.HandleFunc("/from-frame.html", func(w http.ResponseWriter, r *http.Request) {
		requested.Store("/from-frame.html", true)
		page(w, `<meta http-equiv="refresh" content="0; url=/from-refresh.html">`)
	})
	mux.HandleFunc("/from-refresh.html", func(w http.ResponseWriter, r *http.Request) {
		requested.Store("/from-refresh.html", true)
		page(w, `<p>reached through a meta refresh</p>`)
	})
	mux.HandleFunc("/area.html", func(w http.ResponseWriter, r *http.Request) {
		requested.Store("/area.html", true)
		page(w, `<p>reached through an image map</p>`)
	})
	mux.HandleFunc("/i/pic.png", func(w http.ResponseWriter, r *http.Request) {
		requested.Store("/i/pic.png", true)
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG\r\n\x1a\nX"))
	})
	mux.HandleFunc("/s/main.css", func(w http.ResponseWriter, r *http.Request) {
		requested.Store("/s/main.css", true)
		w.Header().Set("Content-Type", "text/css")
		// A stylesheet pulls in another stylesheet and an image, both of which
		// have to be followed for the mirror to render.
		fmt.Fprint(w, `@import "sub.css"; body{background:url(/i/bg.jpg)}`)
	})
	mux.HandleFunc("/s/sub.css", func(w http.ResponseWriter, r *http.Request) {
		requested.Store("/s/sub.css", true)
		w.Header().Set("Content-Type", "text/css")
		fmt.Fprint(w, `.x{background:url(/i/deep.gif)}`)
	})
	for _, img := range []string{"/i/bg.jpg", "/i/deep.gif"} {
		img := img
		mux.HandleFunc(img, func(w http.ResponseWriter, r *http.Request) {
			requested.Store(img, true)
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("\xff\xd8\xffX"))
		})
	}

	// The second domain has its own two-page chain, so allowing a domain has to
	// mean crawling it, not merely fetching the one linked page.
	allowedMux.HandleFunc("/other.html", func(w http.ResponseWriter, r *http.Request) {
		requested.Store("allowed/other.html", true)
		page(w, `<a href="/other-2.html">deeper on the second domain</a>`)
	})
	allowedMux.HandleFunc("/other-2.html", func(w http.ResponseWriter, r *http.Request) {
		requested.Store("allowed/other-2.html", true)
		page(w, `<p>second domain, second level</p>`)
	})

	dest := t.TempDir()
	cfg := testConfig(t, mainURL+"/", dest)
	cfg.Scope.Hosts = []string{strings.TrimPrefix(allowedURL, "http://")}
	e := runEngine(t, cfg)
	defer e.Close()

	// Everything reachable must have been requested.
	var missing []string
	want := []string{"/", "/frame.html", "/from-frame.html", "/from-refresh.html",
		"/area.html", "/i/pic.png", "/s/main.css", "/s/sub.css", "/i/bg.jpg", "/i/deep.gif",
		"allowed/other.html", "allowed/other-2.html"}
	for i := 1; i <= chainLen; i++ {
		want = append(want, fmt.Sprintf("/c/%d.html", i))
	}
	for _, w := range want {
		if _, ok := requested.Load(w); !ok {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the crawl never reached %d of %d targets: %v", len(missing), len(want), missing)
	}

	// The blocked domain must never have been touched at all.
	requested.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok && strings.HasPrefix(s, "blocked") {
			t.Errorf("out-of-scope domain was requested: %s", s)
		}
		return true
	})

	// And the far end of the chain must actually be on disk. Allowing a second
	// domain means every host gets its own folder, including the main one.
	last := filepath.Join(dest, hostFolder(mainURL), "c", fmt.Sprintf("%d.html", chainLen))
	body, err := os.ReadFile(last)
	if err != nil {
		t.Fatalf("last page of the chain missing: %v", err)
	}
	if !strings.Contains(string(body), "end of the chain") {
		t.Errorf("last page has unexpected content: %s", body)
	}

	st := e.Store().Stats()
	if st.Failed != 0 {
		for _, en := range e.Store().Select(func(en *state.Entry) bool { return en.Status == state.Failed }) {
			t.Logf("failed: %s — %s", en.URL, en.Err)
		}
		t.Errorf("%d URLs failed; a complete crawl of this fixture has none", st.Failed)
	}
	if st.Done < len(want) {
		t.Errorf("saved %d files, expected at least %d", st.Done, len(want))
	}
	t.Logf("closure: %d saved, %d skipped (out of scope), %d failed", st.Done, st.Skipped, st.Failed)
}
