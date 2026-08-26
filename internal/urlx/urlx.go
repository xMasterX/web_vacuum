// Package urlx normalizes and compares URLs. Every URL that enters the crawler
// passes through Canonical so the frontier, the on-disk state and the link
// rewriter all agree on what counts as "the same page".
package urlx

import (
	"net"
	"net/url"
	"path"
	"sort"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Canonical returns a cleaned absolute URL suitable for fetching: lowercase
// scheme and host, no default port, no fragment, normalized path, and empty
// query removed. It deliberately does NOT reorder query parameters, because
// some servers are order-sensitive; use Key for deduplication instead.
func Canonical(u *url.URL) *url.URL {
	c := *u
	c.Scheme = strings.ToLower(c.Scheme)
	c.Fragment = ""
	c.RawFragment = ""

	host := strings.ToLower(c.Hostname())
	port := c.Port()
	if (c.Scheme == "http" && port == "80") || (c.Scheme == "https" && port == "443") {
		port = ""
	}
	// Trim a trailing dot from fully-qualified names so "example.com." and
	// "example.com" do not become two separate mirrors.
	host = strings.TrimSuffix(host, ".")
	if port != "" {
		c.Host = net.JoinHostPort(host, port)
	} else {
		c.Host = host
	}

	if c.Opaque == "" {
		c.Path = cleanPath(c.Path)
		c.RawPath = ""
	}
	if c.RawQuery == "" {
		c.ForceQuery = false
	}
	return &c
}

// cleanPath resolves "." and ".." and guarantees a leading slash while keeping
// a meaningful trailing slash (which decides index.html placement later).
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	trailing := strings.HasSuffix(p, "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	cp := path.Clean(p)
	if cp == "." {
		cp = "/"
	}
	if trailing && !strings.HasSuffix(cp, "/") {
		cp += "/"
	}
	return cp
}

// Key returns the deduplication key for a URL: canonical form with query
// parameters sorted, so ?a=1&b=2 and ?b=2&a=1 are recognized as one resource.
func Key(u *url.URL) string {
	c := Canonical(u)
	if c.RawQuery != "" {
		if q, err := url.ParseQuery(c.RawQuery); err == nil {
			keys := make([]string, 0, len(q))
			for k := range q {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var b strings.Builder
			for _, k := range keys {
				vs := append([]string(nil), q[k]...)
				sort.Strings(vs)
				for _, v := range vs {
					if b.Len() > 0 {
						b.WriteByte('&')
					}
					b.WriteString(url.QueryEscape(k))
					b.WriteByte('=')
					b.WriteString(url.QueryEscape(v))
				}
			}
			c.RawQuery = b.String()
		}
	}
	return c.String()
}

// Parse parses a possibly scheme-less string into an absolute URL.
func Parse(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "://") && !strings.HasPrefix(raw, "//") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	return Canonical(u), nil
}

// Resolve turns a possibly relative reference found in a document into an
// absolute canonical URL. It returns ok=false for references that are not
// fetchable resources (fragments, javascript:, mailto:, data:, about:, and any
// other non-http scheme).
func Resolve(base *url.URL, ref string) (*url.URL, bool) {
	ref = strings.TrimSpace(ref)
	// Strip the control characters browsers ignore inside URL attributes.
	ref = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, ref)
	if ref == "" || strings.HasPrefix(ref, "#") {
		return nil, false
	}
	if i := strings.Index(ref, ":"); i > 0 && !strings.ContainsAny(ref[:i], "/?#") {
		switch strings.ToLower(ref[:i]) {
		case "http", "https":
		default:
			return nil, false
		}
	}
	r, err := url.Parse(ref)
	if err != nil {
		return nil, false
	}
	abs := base.ResolveReference(r)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return nil, false
	}
	if abs.Host == "" {
		return nil, false
	}
	return Canonical(abs), true
}

// ResolveKeepFragment is Resolve but preserves the fragment. Link rewriting
// needs it: "/page#section" must become "./page.html#section", or every
// in-page anchor in the archive stops working.
func ResolveKeepFragment(base *url.URL, ref string) (*url.URL, bool) {
	abs, ok := Resolve(base, ref)
	if !ok {
		return nil, false
	}
	if i := strings.IndexByte(ref, '#'); i >= 0 {
		frag := ref[i+1:]
		if frag != "" {
			c := *abs
			c.Fragment = frag
			return &c, true
		}
	}
	return abs, true
}

// RegistrableDomain returns the eTLD+1 ("bbc.co.uk" for "www.bbc.co.uk"),
// falling back to the host itself for IP literals and unknown suffixes.
func RegistrableDomain(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return ""
	}
	if net.ParseIP(host) != nil {
		return host
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || d == "" {
		return host
	}
	return d
}

// SameSite reports whether two hosts share a registrable domain.
func SameSite(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	ra, rb := RegistrableDomain(a), RegistrableDomain(b)
	return ra != "" && ra == rb
}

// HostPattern matches a host against a user-supplied pattern. Bare hosts match
// exactly; a leading "." or "*." also matches every subdomain; "*" matches any
// host. Matching is case-insensitive.
func HostPattern(pattern, host string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if pattern == "" || host == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		pattern = pattern[1:] // leaves ".example.com"
	}
	if strings.HasPrefix(pattern, ".") {
		suffix := pattern
		return host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix)
	}
	return host == pattern
}

// HostMatchesURL matches a pattern against a URL's host, trying both the bare
// hostname and the host:port form so "example.com:8080" works as written.
func HostMatchesURL(pattern string, u *url.URL) bool {
	if HostPattern(pattern, u.Hostname()) {
		return true
	}
	if strings.Contains(pattern, ":") {
		return strings.EqualFold(strings.TrimSpace(pattern), u.Host)
	}
	return false
}

// AnyHostMatchesURL reports whether any pattern matches the URL's host.
func AnyHostMatchesURL(patterns []string, u *url.URL) bool {
	for _, p := range patterns {
		if HostMatchesURL(p, u) {
			return true
		}
	}
	return false
}

// AnyHostPattern reports whether host matches any pattern in the list.
func AnyHostPattern(patterns []string, host string) bool {
	for _, p := range patterns {
		if HostPattern(p, host) {
			return true
		}
	}
	return false
}

// DirPrefix returns the directory portion of a URL path, always ending in "/".
func DirPrefix(u *url.URL) string {
	p := u.Path
	if p == "" {
		return "/"
	}
	if strings.HasSuffix(p, "/") {
		return p
	}
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "/"
	}
	return p[:i+1]
}

// UnderDirectory reports whether u lives at or below the directory of base on
// the same host.
func UnderDirectory(base, u *url.URL) bool {
	if !strings.EqualFold(base.Hostname(), u.Hostname()) {
		return false
	}
	return strings.HasPrefix(u.Path, DirPrefix(base))
}

// Segments splits a URL path into its non-empty components.
func Segments(u *url.URL) []string {
	p := strings.Trim(u.Path, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// MaxRepeatedSegment returns the highest number of times any single path
// segment repeats, which is a cheap and effective spider-trap signal
// ("/a/b/a/b/a/b/..." from a broken relative link).
func MaxRepeatedSegment(u *url.URL) int {
	counts := map[string]int{}
	best := 0
	for _, s := range Segments(u) {
		counts[s]++
		if counts[s] > best {
			best = counts[s]
		}
	}
	return best
}

// StripQueryParams removes the named parameters (case-insensitive) and returns
// a new URL. Used to drop tracking noise that would otherwise fork the crawl.
func StripQueryParams(u *url.URL, names []string) *url.URL {
	if len(names) == 0 || u.RawQuery == "" {
		return u
	}
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[strings.ToLower(n)] = true
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return u
	}
	changed := false
	for k := range q {
		if drop[strings.ToLower(k)] {
			delete(q, k)
			changed = true
		}
	}
	if !changed {
		return u
	}
	c := *u
	c.RawQuery = q.Encode()
	return &c
}

// Extension returns the lowercase extension of the URL's last path segment,
// including the dot, or "" when there is none.
func Extension(u *url.URL) string {
	p := u.Path
	if strings.HasSuffix(p, "/") {
		return ""
	}
	base := path.Base(p)
	i := strings.LastIndex(base, ".")
	if i <= 0 || i == len(base)-1 {
		return ""
	}
	return strings.ToLower(base[i:])
}

// LooksLikeDirectory reports whether the URL has no filename component, meaning
// the response should be saved as an index file inside a folder.
func LooksLikeDirectory(u *url.URL) bool {
	return u.Path == "" || strings.HasSuffix(u.Path, "/")
}
