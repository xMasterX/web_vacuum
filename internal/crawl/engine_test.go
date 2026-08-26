package crawl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/state"
)

// fakeSite is a small but structurally realistic site: nested pages, an asset
// on another host, a stylesheet with its own references, a spider trap, and a
// page that only the sitemap knows about.
func fakeSite(t *testing.T) (*httptest.Server, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var cdnHits atomic.Int32

	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHits.Add(1)
		switch r.URL.Path {
		case "/img/remote.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG\r\n\x1a\nREMOTE"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cdn.Close)

	mux := http.NewServeMux()
	main := httptest.NewUnstartedServer(mux)
	main.Start()
	t.Cleanup(main.Close)

	page := func(body string) string {
		return "<!doctype html><html><head><link rel=stylesheet href=\"/css/site.css\"></head><body>" + body + "</body></html>"
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, page(`
<h1>Home</h1>
<a href="/about">About</a>
<a href="/docs/">Docs</a>
<a href="/missing-page">Broken</a>
<a href="/trap/a/a/a/a/a/a">Trap</a>
<img src="/img/local.png">
<img src="`+cdn.URL+`/img/remote.png">
<a href="https://elsewhere.invalid/offsite">Offsite</a>`))
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, page(`<h1>About</h1><a href="/">Home</a><a href="/docs/deep/page.html">Deep</a>`))
	})
	mux.HandleFunc("/docs/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, page(`<h1>Docs</h1><a href="deep/page.html">Deep</a><a href="../about">Up</a>`))
	})
	mux.HandleFunc("/docs/deep/page.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, page(`<h1>Deep</h1><a href="../../">Root</a><img src="../../img/local.png">`))
	})
	mux.HandleFunc("/orphan", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, page(`<h1>Orphan</h1>`))
	})
	mux.HandleFunc("/css/site.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		fmt.Fprint(w, "body{background:url(../img/bg.jpg)}\n@import \"print.css\";")
	})
	mux.HandleFunc("/css/print.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		fmt.Fprint(w, "@media print{body{color:#000}}")
	})
	mux.HandleFunc("/img/local.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG\r\n\x1a\nLOCAL"))
	})
	mux.HandleFunc("/img/bg.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("\xff\xd8\xffBG"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0"?><urlset><url><loc>%s/orphan</loc></url></urlset>`, main.URL)
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "User-agent: *\nDisallow: /private/\nSitemap: %s/sitemap.xml\n", main.URL)
	})
	return main, cdn, &cdnHits
}

// hostFolder is the folder name the mapper gives a server's origin.
func hostFolder(serverURL string) string {
	hostPort := strings.TrimPrefix(serverURL, "http://")
	return strings.ReplaceAll(hostPort, ":", "_")
}

func testConfig(t *testing.T, start, dest string) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.StartURLs = []string{start}
	cfg.Destination = dest
	cfg.General.Connections = 4
	cfg.Request.Attempts = 2
	cfg.Request.UserAgent = "webvacuum"
	cfg.Resilience.RetryBaseDelay = config.Duration(time.Millisecond)
	cfg.Resilience.RetryMaxDelay = config.Duration(5 * time.Millisecond)
	cfg.Resilience.RetryPasses = 0
	cfg.Resilience.OfflineMinWait = config.Duration(5 * time.Millisecond)
	cfg.Resilience.OfflineMaxWait = config.Duration(20 * time.Millisecond)
	return cfg
}

func runEngine(t *testing.T, cfg *config.Config) *Engine {
	t.Helper()
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := e.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return e
}

func TestCrawlProducesBrowsableMirror(t *testing.T) {
	main, cdn, cdnHits := fakeSite(t)
	dest := t.TempDir()

	cfg := testConfig(t, main.URL+"/", dest)
	cfg.Webpage.UseSitemap = true
	e := runEngine(t, cfg)
	defer e.Close()

	// Every page and asset reachable from the home page must exist on disk.
	for _, rel := range []string{
		"index.html",
		"about.html",
		"docs/index.html",
		"docs/deep/page.html",
		"css/site.css",
		"css/print.css",
		"img/local.png",
		"img/bg.jpg",
		"orphan.html",
	} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}

	// Supporting files are fetched from other hosts by default, and land under
	// their own host folder.
	if cdnHits.Load() == 0 {
		t.Error("the off-host image was never requested")
	}
	cdnDir := hostFolder(cdn.URL)
	if _, err := os.Stat(filepath.Join(dest, cdnDir, "img", "remote.png")); err != nil {
		t.Errorf("off-host asset not saved: %v", err)
	}

	index, err := os.ReadFile(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)

	// Links to downloaded pages become relative paths.
	for _, want := range []string{
		`href="./about.html"`,
		`href="./docs/index.html"`,
		`src="./img/local.png"`,
		`href="./css/site.css"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %s", want)
		}
	}
	// The off-host image is relative too, pointing into the host folder.
	if !strings.Contains(html, "/img/remote.png") || !strings.Contains(html, cdnDir) {
		t.Errorf("off-host image not localized:\n%s", html)
	}
	// Links we could not download stay absolute so they still work online.
	if !strings.Contains(html, `href="https://elsewhere.invalid/offsite"`) {
		t.Errorf("off-site link should have stayed absolute:\n%s", html)
	}
	if !strings.Contains(html, main.URL+"/missing-page") {
		t.Errorf("broken link should have been absolutized:\n%s", html)
	}

	// A page reached through a relative link from a nested directory must point back
	// correctly.
	deep, err := os.ReadFile(filepath.Join(dest, "docs", "deep", "page.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(deep), `src="../../img/local.png"`) {
		t.Errorf("nested relative asset link wrong:\n%s", deep)
	}

	// The stylesheet's own references are rewritten as well.
	css, err := os.ReadFile(filepath.Join(dest, "css", "site.css"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), "url(../img/bg.jpg)") {
		t.Errorf("css url() not localized: %s", css)
	}
	if !strings.Contains(string(css), `"./print.css"`) {
		t.Errorf("css @import not localized: %s", css)
	}
}

func TestSpiderTrapAndScopeAreEnforced(t *testing.T) {
	main, _, _ := fakeSite(t)
	dest := t.TempDir()
	e := runEngine(t, testConfig(t, main.URL+"/", dest))
	defer e.Close()

	for _, en := range e.Store().Select(nil) {
		if strings.Contains(en.URL, "/trap/a/a/a") && en.Status == state.Done {
			t.Errorf("spider trap was crawled: %s", en.URL)
		}
		if strings.Contains(en.URL, "elsewhere.invalid") && en.Status == state.Done {
			t.Errorf("off-site page was downloaded: %s", en.URL)
		}
	}
}

func TestBrokenLinksAreRecordedNotFatal(t *testing.T) {
	main, _, _ := fakeSite(t)
	dest := t.TempDir()
	e := runEngine(t, testConfig(t, main.URL+"/", dest))
	defer e.Close()

	st := e.Store().Stats()
	if st.Done < 6 {
		t.Fatalf("only %d files saved, expected the crawl to complete", st.Done)
	}
	found := false
	for _, en := range e.Store().Select(func(en *state.Entry) bool { return en.Status == state.Failed }) {
		if strings.Contains(en.URL, "/missing-page") && en.HTTPCode == 404 {
			found = true
		}
	}
	if !found {
		t.Error("the 404 link should have been recorded as a failure")
	}
	if e.Phase() != PhaseDone {
		t.Errorf("phase = %s, want done", e.Phase())
	}
}

func TestResumeSkipsCompletedWork(t *testing.T) {
	main, _, _ := fakeSite(t)
	dest := t.TempDir()

	cfg := testConfig(t, main.URL+"/", dest)
	first := runEngine(t, cfg)
	firstStats := first.Store().Stats()
	first.Close()

	// A second run with "never replace" must not re-download anything.
	cfg2 := testConfig(t, main.URL+"/", dest)
	cfg2.General.FileReplacement = config.ReplaceNever
	second, err := New(cfg2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer second.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := second.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := second.client.Stats().Requests; got > 4 {
		t.Errorf("resume made %d requests; it should have reused the mirror", got)
	}
	if second.Store().Stats().Done != firstStats.Done {
		t.Errorf("resumed run has %d done, first had %d",
			second.Store().Stats().Done, firstStats.Done)
	}
}

func TestExtraDomainIsAllowedForPages(t *testing.T) {
	main, cdn, _ := fakeSite(t)
	dest := t.TempDir()

	cfg := testConfig(t, main.URL+"/", dest)
	cfg.Scope.Hosts = []string{strings.TrimPrefix(cdn.URL, "http://")}
	e := runEngine(t, cfg)
	defer e.Close()

	if _, err := os.Stat(filepath.Join(dest, hostFolder(cdn.URL), "img", "remote.png")); err != nil {
		t.Errorf("second domain not mirrored: %v", err)
	}
}

func TestRobotsIsHonouredAndCanBeIgnored(t *testing.T) {
	var privateHits atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow: /private/\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<a href="/private/secret.html">s</a>`)
	})
	mux.HandleFunc("/private/secret.html", func(w http.ResponseWriter, r *http.Request) {
		privateHits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "secret")
	})

	e := runEngine(t, testConfig(t, srv.URL+"/", t.TempDir()))
	e.Close()
	if privateHits.Load() != 0 {
		t.Error("robots.txt was ignored by default")
	}

	cfg := testConfig(t, srv.URL+"/", t.TempDir())
	cfg.General.IgnoreRobots = true
	e2 := runEngine(t, cfg)
	e2.Close()
	if privateHits.Load() == 0 {
		t.Error("ignore_robots did not take effect")
	}
}

func TestMaxLevelsLimitsDepth(t *testing.T) {
	main, _, _ := fakeSite(t)
	dest := t.TempDir()
	cfg := testConfig(t, main.URL+"/", dest)
	cfg.Limits.MaxLevels = 1
	e := runEngine(t, cfg)
	defer e.Close()

	// Depth 1 means the home page plus what it links to, and the home page's
	// own assets regardless of depth.
	if _, err := os.Stat(filepath.Join(dest, "about.html")); err != nil {
		t.Errorf("depth-1 page missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "img", "local.png")); err != nil {
		t.Errorf("home page asset missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "docs", "deep", "page.html")); err == nil {
		t.Error("depth-2 page should not have been downloaded")
	}
}

func TestExcludePatternWins(t *testing.T) {
	main, _, _ := fakeSite(t)
	dest := t.TempDir()
	cfg := testConfig(t, main.URL+"/", dest)
	cfg.Scope.Exclude = []string{`/docs/`}
	e := runEngine(t, cfg)
	defer e.Close()

	if _, err := os.Stat(filepath.Join(dest, "docs", "index.html")); err == nil {
		t.Error("excluded path was downloaded")
	}
	if _, err := os.Stat(filepath.Join(dest, "about.html")); err != nil {
		t.Errorf("unrelated page was wrongly excluded: %v", err)
	}
}
