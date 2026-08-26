package localize

import (
	"net/url"
	"strings"
	"testing"

	"github.com/xMasterX/web_vacuum/internal/parse"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

// mirror is a stand-in for the crawler's URL to local-path index.
type mirror map[string]string

func (m mirror) LocalPath(u *url.URL) (string, bool) {
	p, ok := m[u.String()]
	return p, ok
}

func (m mirror) HasLocal(rel string) bool {
	for _, v := range m {
		if v == rel {
			return true
		}
	}
	return false
}

func baseOpts() Options {
	return Options{
		Options: parse.Options{
			ScanSrcset:    true,
			ScanInlineCSS: true,
			FollowMeta:    true,
		},
		AbsoluteFallback: true,
	}
}

func TestHTMLBecomesBrowsableOffline(t *testing.T) {
	src := []byte(`<!doctype html>
<html><head>
<link rel="stylesheet" href="/css/site.css">
</head><body>
<a href="/about">About</a>
<a href="/deep/page.html#section">Deep</a>
<a href="https://other.example/thing">Offsite</a>
<img src="/img/logo.png" srcset="/img/logo.png 1x, /img/logo@2x.png 2x">
<div style="background:url(/img/bg.jpg)"></div>
</body></html>`)

	m := mirror{
		"https://example.com/css/site.css":    "example.com/css/site.css",
		"https://example.com/about":           "example.com/about.html",
		"https://example.com/deep/page.html":  "example.com/deep/page.html",
		"https://example.com/img/logo.png":    "example.com/img/logo.png",
		"https://example.com/img/logo@2x.png": "example.com/img/logo@2x.png",
		"https://example.com/img/bg.jpg":      "example.com/img/bg.jpg",
	}
	docURL, _ := urlx.Parse("https://example.com/index.html")
	res, err := HTML(src, docURL, "example.com/index.html", m, baseOpts())
	if err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := string(res.Content)

	for _, want := range []string{
		`href="./css/site.css"`,
		`href="./about.html"`,
		`href="./deep/page.html#section"`,
		`src="./img/logo.png"`,
		`./img/logo@2x.png 2x`,
		`url(./img/bg.jpg)`,
		`href="https://other.example/thing"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %s\n---\n%s", want, out)
		}
	}
	if res.Rewritten != 7 {
		t.Errorf("Rewritten = %d, want 7", res.Rewritten)
	}
	if res.Absolutized != 1 {
		t.Errorf("Absolutized = %d, want 1", res.Absolutized)
	}
	// The srcset descriptors must survive the rewrite.
	if !strings.Contains(out, "1x,") || !strings.Contains(out, "2x") {
		t.Errorf("srcset descriptors lost:\n%s", out)
	}
}

func TestRelativeLinksFromNestedPages(t *testing.T) {
	src := []byte(`<a href="../../index.html">home</a><img src="pic.png">`)
	m := mirror{
		"https://example.com/index.html":  "example.com/index.html",
		"https://example.com/a/b/pic.png": "example.com/a/b/pic.png",
	}
	docURL, _ := urlx.Parse("https://example.com/a/b/page.html")
	res, _ := HTML(src, docURL, "example.com/a/b/page.html", m, baseOpts())
	out := string(res.Content)
	if !strings.Contains(out, `href="../../index.html"`) {
		t.Errorf("upward link wrong:\n%s", out)
	}
	if !strings.Contains(out, `src="./pic.png"`) {
		t.Errorf("sibling link wrong:\n%s", out)
	}
}

func TestBaseHrefIsNeutralized(t *testing.T) {
	src := []byte(`<html><head><base href="https://example.com/app/"></head>
<body><a href="page.html">p</a></body></html>`)
	m := mirror{"https://example.com/app/page.html": "example.com/app/page.html"}
	docURL, _ := urlx.Parse("https://example.com/other/doc.html")
	res, _ := HTML(src, docURL, "example.com/other/doc.html", m, baseOpts())
	out := string(res.Content)
	if !strings.Contains(out, `<base href="./">`) {
		t.Errorf("base not neutralized:\n%s", out)
	}
	// The link was authored relative to the base, so it must resolve through it
	// and then be re-expressed relative to where the document actually sits.
	if !strings.Contains(out, `href="../app/page.html"`) {
		t.Errorf("base-relative link wrong:\n%s", out)
	}
}

func TestMissingTargetsBecomeAbsolute(t *testing.T) {
	src := []byte(`<a href="/not-downloaded">x</a>`)
	docURL, _ := urlx.Parse("https://example.com/index.html")
	res, _ := HTML(src, docURL, "example.com/index.html", mirror{}, baseOpts())
	if !strings.Contains(string(res.Content), `href="https://example.com/not-downloaded"`) {
		t.Errorf("missing target not absolutized:\n%s", res.Content)
	}
}

func TestMetaRefreshIsRewritten(t *testing.T) {
	src := []byte(`<meta http-equiv="refresh" content="3; url=/next">`)
	m := mirror{"https://example.com/next": "example.com/next.html"}
	docURL, _ := urlx.Parse("https://example.com/index.html")
	res, _ := HTML(src, docURL, "example.com/index.html", m, baseOpts())
	out := string(res.Content)
	if !strings.Contains(out, `content="3; url=./next.html"`) {
		t.Errorf("meta refresh not rewritten:\n%s", out)
	}
}

func TestCSSRewrite(t *testing.T) {
	src := []byte(`@import "reset.css";
body { background: url('../img/bg.jpg'); }
@font-face { src: url(fonts/f.woff2) format("woff2"); }
.x { background: url(https://cdn.example/missing.png); }`)
	m := mirror{
		"https://example.com/css/reset.css":     "example.com/css/reset.css",
		"https://example.com/img/bg.jpg":        "example.com/img/bg.jpg",
		"https://example.com/css/fonts/f.woff2": "example.com/css/fonts/f.woff2",
	}
	docURL, _ := urlx.Parse("https://example.com/css/site.css")
	res, err := CSS(src, docURL, "example.com/css/site.css", m, baseOpts())
	if err != nil {
		t.Fatalf("CSS: %v", err)
	}
	out := string(res.Content)
	for _, want := range []string{
		`@import "./reset.css"`,
		`url('../img/bg.jpg')`,
		`url(./fonts/f.woff2)`,
		`url(https://cdn.example/missing.png)`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CSS output missing %s\n---\n%s", want, out)
		}
	}
}

func TestStripScripts(t *testing.T) {
	opt := baseOpts()
	opt.StripScripts = true
	src := []byte(`<body><p>keep</p><script>evil()</script><script src="a.js"></script></body>`)
	docURL, _ := urlx.Parse("https://example.com/i.html")
	res, _ := HTML(src, docURL, "example.com/i.html", mirror{}, opt)
	out := string(res.Content)
	if strings.Contains(out, "script") {
		t.Errorf("scripts not stripped:\n%s", out)
	}
	if !strings.Contains(out, "<p>keep</p>") {
		t.Errorf("content lost:\n%s", out)
	}
}

func TestRewriteIsIdempotent(t *testing.T) {
	src := []byte(`<a href="/about">a</a><img src="/i.png">`)
	m := mirror{
		"https://example.com/about": "example.com/about.html",
		"https://example.com/i.png": "example.com/i.png",
	}
	docURL, _ := urlx.Parse("https://example.com/index.html")
	first, _ := HTML(src, docURL, "example.com/index.html", m, baseOpts())
	second, _ := HTML(first.Content, docURL, "example.com/index.html", m, baseOpts())
	if string(first.Content) != string(second.Content) {
		t.Fatalf("second pass changed the document:\n%s\n---\n%s", first.Content, second.Content)
	}
}

func TestNonUTF8BytesSurvive(t *testing.T) {
	// A latin-1 page: the high bytes must pass through untouched while the
	// ASCII href is still rewritten.
	src := []byte("<html><body><p>caf\xe9</p><a href=\"/x\">l\xe0</a></body></html>")
	m := mirror{"https://example.com/x": "example.com/x.html"}
	docURL, _ := urlx.Parse("https://example.com/i.html")
	res, _ := HTML(src, docURL, "example.com/i.html", m, baseOpts())
	if !strings.Contains(string(res.Content), `href="./x.html"`) {
		t.Errorf("link not rewritten:\n%q", res.Content)
	}
	if !strings.Contains(string(res.Content), "caf\xe9") {
		t.Errorf("high bytes were mangled:\n%q", res.Content)
	}
}
