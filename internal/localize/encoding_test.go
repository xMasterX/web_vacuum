package localize

import (
	"bytes"
	"net/url"
	"strings"
	"testing"

	"github.com/xMasterX/web_vacuum/internal/parse"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

var win1251 = []byte("\xd0\xf3\xf1\xf1\xea\xe8\xe9 \xf2\xe5\xea\xf1\xf2")

// TestRewritingALegacyEncodedPage is the whole point of working on bytes rather
// than decoded text: an archive of an old site has to come back byte-identical
// apart from the links, and the code that finds those links must not assume the
// page is UTF-8.
func TestRewritingALegacyEncodedPage(t *testing.T) {
	cyr := string(win1251)
	var b bytes.Buffer
	b.WriteString("<html><head><title>" + cyr + "</title>")
	b.WriteString(`<style>body{background:url(bg.png)}</style></head><body>`)
	for i := 0; i < 500; i++ {
		b.WriteString(cyr)
		b.WriteString(`<a href="/thread.php?t=1">` + cyr + `</a>`)
		b.WriteString(`<img src="/i/pic.png">`)
	}
	b.WriteString("</body></html>")
	src := b.Bytes()

	m := mirror{
		"https://forum.example/thread.php?t=1": "thread.php@t=1.html",
		"https://forum.example/i/pic.png":      "i/pic.png",
		"https://forum.example/bg.png":         "bg.png",
	}
	docURL, _ := urlx.Parse("https://forum.example/index.html")

	opt := baseOpts()
	opt.InjectBanner = true // exercises the other offset-sensitive path
	res, err := HTML(src, docURL, "index.html", m, opt)
	if err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := res.Content

	if !bytes.Contains(out, []byte(`href="./thread.php@t=1.html"`)) {
		t.Error("links were not rewritten in a legacy-encoded page")
	}
	if !bytes.Contains(out, []byte(`src="./i/pic.png"`)) {
		t.Error("images were not rewritten")
	}
	// The original bytes must survive untouched around the links.
	if got := bytes.Count(out, win1251); got < 1000 {
		t.Errorf("legacy bytes were mangled: %d occurrences survived, expected at least 1000", got)
	}
	if bytes.Contains(out, []byte("�")) {
		t.Error("the document was re-encoded; replacement characters appeared")
	}
}

// TestLocalizeSurvivesHostileDocuments throws malformed and non-UTF-8 documents
// at the rewriter. None may panic.
func TestLocalizeSurvivesHostileDocuments(t *testing.T) {
	cyr := string(win1251)
	docURL, _ := urlx.Parse("https://forum.example/dir/page.html")
	m := mirror{"https://forum.example/dir/x.html": "dir/x.html"}

	for _, bad := range []string{
		"", "<html>", "<body", "<body>" + cyr, cyr,
		`<base href="` + cyr + `">`,
		`<a href="` + cyr + `">x</a>`,
		`<style>` + cyr + `url(`,
		`<meta http-equiv=refresh content="` + cyr + `url=`,
		`<img srcset="` + cyr + ` 1x,`,
		"<body>" + strings.Repeat(cyr+`<a href="x.html">`, 200),
	} {
		opt := baseOpts()
		opt.InjectBanner = true
		opt.StripScripts = true
		if _, err := HTML([]byte(bad), docURL, "dir/page.html", m, opt); err != nil {
			t.Errorf("HTML(%q): %v", bad, err)
		}
		if _, err := CSS([]byte(bad), docURL, "dir/page.css", m, opt); err != nil {
			t.Errorf("CSS(%q): %v", bad, err)
		}
	}
}

var _ = parse.AsciiLower
var _ = url.Parse
