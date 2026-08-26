package pathmap

import (
	"strings"
	"testing"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

func newMapper(t *testing.T, tweak func(*config.PathConfig)) *Mapper {
	t.Helper()
	c := config.Default().Paths
	if tweak != nil {
		tweak(&c)
	}
	m, err := New(&c, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestMapBasics(t *testing.T) {
	m := newMapper(t, nil)
	cases := []struct {
		url  string
		hint Hint
		want string
	}{
		{"https://example.com/", Hint{MediaType: "text/html", IsHTML: true}, "example.com/index.html"},
		{"https://example.com/about", Hint{MediaType: "text/html", IsHTML: true}, "example.com/about.html"},
		{"https://example.com/about/", Hint{MediaType: "text/html", IsHTML: true}, "example.com/about/index.html"},
		{"https://example.com/a/b.png", Hint{MediaType: "image/png"}, "example.com/a/b.png"},
		{"https://example.com/style.css", Hint{MediaType: "text/css"}, "example.com/style.css"},
		{"https://example.com/p.php?id=7", Hint{MediaType: "text/html", IsHTML: true}, "example.com/p.php@id=7.html"},
		{"https://example.com:8080/x", Hint{MediaType: "text/html", IsHTML: true}, "example.com_8080/x.html"},
		{"https://example.com/My%20Page", Hint{MediaType: "text/html", IsHTML: true}, "example.com/My Page.html"},
	}
	for _, c := range cases {
		u, err := urlx.Parse(c.url)
		if err != nil {
			t.Fatalf("parse %s: %v", c.url, err)
		}
		if got := m.Map(u, c.hint); got != c.want {
			t.Errorf("Map(%s) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestMapSanitizesDangerousNames(t *testing.T) {
	m := newMapper(t, nil)
	// Encoded traversal is resolved by URL canonicalization (as a browser would),
	// so it addresses /etc/passwd on the site and stays inside the host folder.
	u, _ := urlx.Parse("https://example.com/a/%2e%2e%2f%2e%2e%2fetc%2fpasswd")
	got := m.Map(u, Hint{MediaType: "text/plain"})
	if got != "example.com/etc/passwd.txt" {
		t.Fatalf("unexpected mapping: %q", got)
	}
	// Whatever a URL contains, the mapped path must stay inside the destination.
	hostile := []string{
		"https://example.com/x/%2E%2E",
		"https://example.com/..%2f..%2f..%2fetc/passwd",
		"https://example.com/a/%00/b",
		"https://example.com/%2e%2e/%2e%2e/",
		"https://example.com/con/aux/nul.html",
		"https://example.com/a b/c:d*e?f|g.html",
		"https://example.com/....//....//x",
	}
	for _, raw := range hostile {
		hu, err := urlx.Parse(raw)
		if err != nil {
			continue
		}
		rel := m.Map(hu, Hint{MediaType: "text/html", IsHTML: true})
		if _, err := Local("/tmp/dest", rel); err != nil {
			t.Errorf("Map(%s) -> %q escaped the destination: %v", raw, rel, err)
		}
		for _, seg := range strings.Split(rel, "/") {
			if seg == ".." || seg == "." || seg == "" {
				t.Errorf("Map(%s) -> %q contains an unsafe segment %q", raw, rel, seg)
			}
		}
	}
	u2, _ := urlx.Parse("https://example.com/con")
	if got := m.Map(u2, Hint{MediaType: "text/html", IsHTML: true}); got != "example.com/_con.html" {
		t.Fatalf("reserved name not escaped: %q", got)
	}
}

func TestMapIsStable(t *testing.T) {
	m := newMapper(t, nil)
	u, _ := urlx.Parse("https://example.com/a/b/c?x=1&y=2")
	first := m.Map(u, Hint{MediaType: "text/html", IsHTML: true})
	for i := 0; i < 5; i++ {
		if got := m.Map(u, Hint{MediaType: "text/html", IsHTML: true}); got != first {
			t.Fatalf("mapping not deterministic: %q vs %q", got, first)
		}
	}
}

func TestLongSegmentsAreTruncatedDistinctly(t *testing.T) {
	m := newMapper(t, nil)
	long := ""
	for i := 0; i < 400; i++ {
		long += "a"
	}
	u1, _ := urlx.Parse("https://example.com/" + long + "1.html")
	u2, _ := urlx.Parse("https://example.com/" + long + "2.html")
	p1 := m.Map(u1, Hint{MediaType: "text/html", IsHTML: true})
	p2 := m.Map(u2, Hint{MediaType: "text/html", IsHTML: true})
	if p1 == p2 {
		t.Fatal("distinct long URLs collapsed to the same path")
	}
	for _, p := range []string{p1, p2} {
		if len(p) > 200 {
			t.Fatalf("segment not truncated: %d bytes", len(p))
		}
	}
}

func TestDisambiguate(t *testing.T) {
	taken := map[string]bool{"a/b.html": true, "a/b~1.html": true}
	got := Disambiguate("a/b.html", func(s string) bool { return taken[s] })
	if got != "a/b~2.html" {
		t.Fatalf("Disambiguate = %q, want a/b~2.html", got)
	}
}

func TestRelativeLink(t *testing.T) {
	cases := []struct{ from, to, want string }{
		{"example.com/index.html", "example.com/about.html", "./about.html"},
		{"example.com/a/b.html", "example.com/c.html", "../c.html"},
		{"example.com/a/b.html", "cdn.example.net/img.png", "../../cdn.example.net/img.png"},
		{"example.com/index.html", "example.com/my page.html", "./my%20page.html"},
	}
	for _, c := range cases {
		if got := RelativeLink(c.from, c.to); got != c.want {
			t.Errorf("RelativeLink(%q,%q) = %q, want %q", c.from, c.to, got, c.want)
		}
	}
}

func TestLocalRefusesEscape(t *testing.T) {
	if _, err := Local("/tmp/dest", "../../etc/passwd"); err == nil {
		t.Fatal("Local must refuse a path that escapes the destination")
	}
	got, err := Local("/tmp/dest", "example.com/a.html")
	if err != nil || got != "/tmp/dest/example.com/a.html" {
		t.Fatalf("Local = %q, %v", got, err)
	}
}
