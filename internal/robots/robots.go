// Package robots implements just enough of the Robots Exclusion Protocol to be
// a well-behaved client: User-agent grouping, Allow/Disallow with wildcards,
// Crawl-delay and Sitemap discovery. Fetching is left to the caller so the
// crawler's retry and offline handling apply to robots.txt too.
package robots

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Group is the set of rules that apply to one user-agent token.
type Group struct {
	Agents     []string
	Rules      []Rule
	CrawlDelay time.Duration
}

// Rule is a single Allow or Disallow line.
type Rule struct {
	Path  string
	Allow bool
	// specificity is the pattern length, used for the longest-match rule.
	specificity int
}

// Robots is a parsed robots.txt.
type Robots struct {
	groups   []Group
	Sitemaps []string
	// missing means there was no robots.txt (404), which permits everything.
	missing bool
}

// AllowAll returns a permissive Robots, used when robots.txt is absent or
// unreadable, or when the user has chosen to ignore exclusions.
func AllowAll() *Robots { return &Robots{missing: true} }

// Parse reads a robots.txt body. Unknown directives are ignored, as the spec
// requires, so a future directive never breaks a crawl.
func Parse(body string) *Robots {
	r := &Robots{}
	var cur *Group
	// sawRule tracks whether the current group already has rules; a new
	// User-agent line after rules starts a fresh group.
	sawRule := false

	for _, raw := range strings.Split(body, "\n") {
		line := raw
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		field := strings.ToLower(strings.TrimSpace(line[:colon]))
		value := strings.TrimSpace(line[colon+1:])

		switch field {
		case "user-agent", "useragent":
			if cur == nil || sawRule {
				r.groups = append(r.groups, Group{})
				cur = &r.groups[len(r.groups)-1]
				sawRule = false
			}
			cur.Agents = append(cur.Agents, strings.ToLower(value))

		case "disallow", "allow":
			if cur == nil {
				// Rules before any User-agent line are treated as applying to
				// everyone, which is how most clients read malformed files.
				r.groups = append(r.groups, Group{Agents: []string{"*"}})
				cur = &r.groups[len(r.groups)-1]
			}
			sawRule = true
			// "Disallow:" with an empty value means allow everything.
			if field == "disallow" && value == "" {
				continue
			}
			cur.Rules = append(cur.Rules, Rule{
				Path:        value,
				Allow:       field == "allow",
				specificity: len(value),
			})

		case "crawl-delay":
			if cur != nil {
				if f, err := strconv.ParseFloat(value, 64); err == nil && f > 0 {
					cur.CrawlDelay = time.Duration(f * float64(time.Second))
				}
			}

		case "sitemap":
			if value != "" {
				r.Sitemaps = append(r.Sitemaps, value)
			}
		}
	}
	return r
}

// group picks the most specific matching group for a user agent: an exact
// token match beats "*", and a longer token beats a shorter one.
func (r *Robots) group(userAgent string) *Group {
	if r == nil || r.missing || len(r.groups) == 0 {
		return nil
	}
	ua := strings.ToLower(userAgent)
	var best *Group
	bestLen := -1
	for i := range r.groups {
		for _, tok := range r.groups[i].Agents {
			match := false
			switch {
			case tok == "*":
				match = true
			case strings.Contains(ua, tok):
				match = true
			}
			if !match {
				continue
			}
			l := len(tok)
			if tok == "*" {
				l = 0
			}
			if l > bestLen {
				bestLen = l
				best = &r.groups[i]
			}
		}
	}
	return best
}

// Allowed reports whether a path (with query string, as sent on the wire) may
// be fetched. The longest matching pattern wins; Allow wins ties, which is the
// behaviour both Google and the RFC specify.
func (r *Robots) Allowed(userAgent, pathWithQuery string) bool {
	g := r.group(userAgent)
	if g == nil || len(g.Rules) == 0 {
		return true
	}
	if pathWithQuery == "" {
		pathWithQuery = "/"
	}
	matches := make([]Rule, 0, 4)
	for _, rule := range g.Rules {
		if matchPattern(rule.Path, pathWithQuery) {
			matches = append(matches, rule)
		}
	}
	if len(matches) == 0 {
		return true
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].specificity != matches[j].specificity {
			return matches[i].specificity > matches[j].specificity
		}
		return matches[i].Allow && !matches[j].Allow
	})
	return matches[0].Allow
}

// CrawlDelay returns the delay requested for this user agent, or 0.
func (r *Robots) CrawlDelay(userAgent string) time.Duration {
	if g := r.group(userAgent); g != nil {
		return g.CrawlDelay
	}
	return 0
}

// matchPattern implements robots.txt globbing: "*" matches any run of
// characters and a trailing "$" anchors the end of the path.
func matchPattern(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(s, part) {
				return false
			}
			pos = len(part)
			continue
		}
		idx := strings.Index(s[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	if anchored {
		// With no wildcard the whole string must equal the pattern; with one,
		// the tail must land exactly at the end.
		if !strings.Contains(pattern, "*") {
			return s == pattern
		}
		return pos == len(s)
	}
	return true
}

// Cache holds one parsed robots.txt per host, fetched at most once.
type Cache struct {
	mu    sync.Mutex
	byKey map[string]*entry
}

type entry struct {
	once sync.Once
	r    *Robots
}

// NewCache returns an empty cache.
func NewCache() *Cache { return &Cache{byKey: map[string]*entry{}} }

// Get returns the robots.txt for a host, calling fetch at most once per host
// even under concurrent access. fetch returns the body, or an error meaning
// "treat as absent".
func (c *Cache) Get(hostKey string, fetch func() (string, error)) *Robots {
	c.mu.Lock()
	e, ok := c.byKey[hostKey]
	if !ok {
		e = &entry{}
		c.byKey[hostKey] = e
	}
	c.mu.Unlock()

	e.once.Do(func() {
		body, err := fetch()
		if err != nil {
			e.r = AllowAll()
			return
		}
		e.r = Parse(body)
	})
	return e.r
}
