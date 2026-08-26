package parse

import (
	"regexp"
	"strings"
	"testing"
)

const page = `<!doctype html>
<html><head>
<meta charset="utf-8">
<base href="/app/">
<title>Test Page</title>
<link rel="stylesheet" href="style.css">
<link rel="preconnect" href="https://fonts.example">
<meta http-equiv="refresh" content="5; url=/next.html">
<style>body{background:url("bg.png");}@import "more.css";</style>
</head><body>
<a href="/about">About</a>
<a href="ext.html" rel="nofollow">No</a>
<img src="a.png" srcset="a-1x.png 1x, a-2x.png 2x" alt="x">
<div style="background-image:url(inline.gif)"></div>
<script src="app.js"></script>
<script>var api = "/api/data.json";</script>
<!-- <a href="/hidden.html"> -->
<iframe src="frame.html"></iframe>
<form action="/search"></form>
</body></html>`

func opts() Options {
	return Options{
		ScanSrcset:    true,
		ScanInlineCSS: true,
		FollowMeta:    true,
		ScanComments:  true,
		ScanScripts:   true,
		URLPatterns:   []*regexp.Regexp{regexp.MustCompile(`"(/[^"]*\.json)"`)},
	}
}

func TestHTMLFindsEveryReference(t *testing.T) {
	doc := HTML([]byte(page), opts())
	if doc.Base != "/app/" {
		t.Errorf("Base = %q", doc.Base)
	}
	if doc.Charset != "utf-8" {
		t.Errorf("Charset = %q", doc.Charset)
	}
	if doc.Title != "Test Page" {
		t.Errorf("Title = %q", doc.Title)
	}

	got := map[string]bool{}
	for _, l := range doc.Links {
		got[l.Value] = true
	}
	want := []string{
		"style.css", "/next.html", "bg.png", "more.css",
		"/about", "ext.html", "a.png", "a-1x.png", "a-2x.png",
		"inline.gif", "app.js", "/api/data.json", "frame.html", "/search",
		"/hidden.html",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing link %q", w)
		}
	}
	// A preconnect hint is a declaration, not a resource to download.
	if got["https://fonts.example"] {
		t.Error("rel=preconnect should not be treated as a resource")
	}
}

func TestNofollowIsFlagged(t *testing.T) {
	doc := HTML([]byte(page), opts())
	for _, l := range doc.Links {
		if l.Value == "ext.html" && !l.NoFollow {
			t.Fatal("rel=nofollow not detected")
		}
		if l.Value == "/about" && l.NoFollow {
			t.Fatal("plain link marked nofollow")
		}
	}
}

func TestOffsetsPointAtTheValue(t *testing.T) {
	src := []byte(page)
	doc := HTML(src, opts())
	for _, l := range doc.Links {
		if l.Start >= l.End || l.End > len(src) {
			t.Fatalf("bad range for %q: %d..%d", l.Value, l.Start, l.End)
		}
		got := string(src[l.Start:l.End])
		// The stored value is unescaped and trimmed; the span is the raw text.
		if !strings.Contains(l.Value, strings.TrimSpace(got)) && !strings.Contains(got, l.Value) {
			t.Errorf("range for %q covers %q", l.Value, got)
		}
	}
}

func TestSrcsetKeepsDescriptors(t *testing.T) {
	links := Srcset("a.png 1x, b.png 2x,  c.png 640w", 0, "img", "srcset")
	if len(links) != 3 {
		t.Fatalf("got %d candidates, want 3", len(links))
	}
	if links[0].Value != "a.png" || links[0].Descriptor != "1x" {
		t.Errorf("candidate 0 = %+v", links[0])
	}
	if links[2].Value != "c.png" || links[2].Descriptor != "640w" {
		t.Errorf("candidate 2 = %+v", links[2])
	}
}

func TestCSSFindsURLs(t *testing.T) {
	css := `
/* url(commented.png) should be ignored */
@import url(reset.css);
@import "typography.css";
body { background: url('bg.jpg') no-repeat; }
.icon { background-image: url(data:image/png;base64,AAA); }
.b { background: url( spaced.png ); }
@font-face { src: url("f.woff2") format("woff2"); }
`
	var vals []string
	for _, l := range CSS([]byte(css), 0) {
		vals = append(vals, l.Value)
	}
	want := []string{"reset.css", "typography.css", "bg.jpg", "spaced.png", "f.woff2"}
	if len(vals) != len(want) {
		t.Fatalf("got %v, want %v", vals, want)
	}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("value %d = %q, want %q", i, vals[i], want[i])
		}
	}
}

func TestCSSIgnoresCommentedAndDataURLs(t *testing.T) {
	for _, l := range CSS([]byte(`/* url(x.png) */ a{background:url(data:image/gif;base64,Zm8=)}`), 0) {
		t.Errorf("unexpected link %q", l.Value)
	}
}

func TestApplyReplacesBackToFront(t *testing.T) {
	src := []byte("aaa BBB ccc DDD eee")
	got := Apply(src, []Replacement{
		{Start: 4, End: 7, Text: "X"},
		{Start: 12, End: 15, Text: "YYYYY"},
	})
	if string(got) != "aaa X ccc YYYYY eee" {
		t.Fatalf("Apply = %q", got)
	}
}

func TestElementRanges(t *testing.T) {
	src := []byte(`<p>a</p><script>x</script>b<script src="y"></script>`)
	rs := ElementRanges(src, "script")
	if len(rs) != 2 {
		t.Fatalf("got %d ranges, want 2", len(rs))
	}
	if string(src[rs[0].Start:rs[0].End]) != "<script>x</script>" {
		t.Errorf("range 0 = %q", src[rs[0].Start:rs[0].End])
	}
	if string(src[rs[1].Start:rs[1].End]) != `<script src="y"></script>` {
		t.Errorf("range 1 = %q", src[rs[1].Start:rs[1].End])
	}
}

func TestMalformedHTMLDoesNotPanic(t *testing.T) {
	for _, bad := range []string{
		`<a href=unquoted.html>x`,
		`<img src="a.png`,
		`<<<>>><a href='b.html'>`,
		`<a href = "  c.html  ">`,
		"<a href=\"d.html\"\n\tclass=x>",
		`<div style="background:url(`,
	} {
		HTML([]byte(bad), opts())
	}
}

func TestUnquotedAndSpacedAttributes(t *testing.T) {
	doc := HTML([]byte(`<a href=plain.html>x</a><a href = " spaced.html ">y</a>`), opts())
	if len(doc.Links) != 2 {
		t.Fatalf("got %d links: %+v", len(doc.Links), doc.Links)
	}
	if doc.Links[0].Value != "plain.html" {
		t.Errorf("unquoted value = %q", doc.Links[0].Value)
	}
}
