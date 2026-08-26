package robots

import "testing"

const sample = `
# comment
User-agent: *
Disallow: /private/
Allow: /private/public.html
Crawl-delay: 2

User-agent: BadBot
Disallow: /

Sitemap: https://example.com/sitemap.xml
`

func TestAllowed(t *testing.T) {
	r := Parse(sample)
	cases := []struct {
		ua, path string
		want     bool
	}{
		{"webvacuum", "/index.html", true},
		{"webvacuum", "/private/secret.html", false},
		{"webvacuum", "/private/public.html", true},
		{"BadBot/1.0", "/index.html", false},
	}
	for _, c := range cases {
		if got := r.Allowed(c.ua, c.path); got != c.want {
			t.Errorf("Allowed(%q,%q) = %v, want %v", c.ua, c.path, got, c.want)
		}
	}
	if len(r.Sitemaps) != 1 {
		t.Fatalf("sitemaps = %v", r.Sitemaps)
	}
	if r.CrawlDelay("webvacuum").Seconds() != 2 {
		t.Fatalf("crawl delay = %v", r.CrawlDelay("webvacuum"))
	}
}

func TestWildcardAndAnchor(t *testing.T) {
	r := Parse("User-agent: *\nDisallow: /*.pdf$\nDisallow: /tmp/*/cache\n")
	if r.Allowed("x", "/a/b.pdf") {
		t.Error("*.pdf$ should be disallowed")
	}
	if !r.Allowed("x", "/a/b.pdf?v=1") {
		t.Error("anchored rule should not match a query suffix")
	}
	if r.Allowed("x", "/tmp/z/cache/x") {
		t.Error("middle wildcard should match")
	}
}

func TestMissingRobotsAllowsAll(t *testing.T) {
	if !AllowAll().Allowed("x", "/anything") {
		t.Error("absent robots.txt must allow everything")
	}
}
