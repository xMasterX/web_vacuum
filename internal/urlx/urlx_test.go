package urlx

import "testing"

func TestCanonical(t *testing.T) {
	cases := []struct{ in, want string }{
		{"HTTP://Example.COM:80/a/./b?q=1#frag", "http://example.com/a/b?q=1"},
		{"https://example.com.", "https://example.com/"},
		{"https://example.com/a/../b", "https://example.com/b"},
		{"https://example.com/a/", "https://example.com/a/"},
		{"https://example.com", "https://example.com/"},
	}
	for _, c := range cases {
		u, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got := u.String(); got != c.want {
			t.Errorf("Canonical(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestKeySortsQuery(t *testing.T) {
	a, _ := Parse("https://example.com/p?b=2&a=1")
	b, _ := Parse("https://example.com/p?a=1&b=2")
	if Key(a) != Key(b) {
		t.Fatalf("query order changed the key: %q vs %q", Key(a), Key(b))
	}
}

func TestResolveRejectsNonFetchable(t *testing.T) {
	base, _ := Parse("https://example.com/dir/page.html")
	for _, ref := range []string{"#top", "javascript:void(0)", "mailto:a@b.c", "data:text/plain,hi", "tel:+1", ""} {
		if _, ok := Resolve(base, ref); ok {
			t.Errorf("Resolve(%q) should be rejected", ref)
		}
	}
	got, ok := Resolve(base, "../img/x.png")
	if !ok || got.String() != "https://example.com/img/x.png" {
		t.Fatalf("Resolve relative failed: %v %v", got, ok)
	}
	got, ok = Resolve(base, "//cdn.example.net/x.js")
	if !ok || got.String() != "https://cdn.example.net/x.js" {
		t.Fatalf("Resolve protocol-relative failed: %v %v", got, ok)
	}
}

func TestHostPattern(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"example.com", "example.com", true},
		{"example.com", "www.example.com", false},
		{".example.com", "www.example.com", true},
		{".example.com", "example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*", "anything.test", true},
		{"example.com", "notexample.com", false},
	}
	for _, c := range cases {
		if got := HostPattern(c.pattern, c.host); got != c.want {
			t.Errorf("HostPattern(%q,%q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
}

func TestRegistrableDomain(t *testing.T) {
	cases := []struct{ in, want string }{
		{"www.bbc.co.uk", "bbc.co.uk"},
		{"a.b.example.com", "example.com"},
		{"192.168.1.1", "192.168.1.1"},
	}
	for _, c := range cases {
		if got := RegistrableDomain(c.in); got != c.want {
			t.Errorf("RegistrableDomain(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMaxRepeatedSegment(t *testing.T) {
	u, _ := Parse("https://example.com/a/b/a/b/a/b/")
	if got := MaxRepeatedSegment(u); got != 3 {
		t.Fatalf("MaxRepeatedSegment = %d, want 3", got)
	}
}
