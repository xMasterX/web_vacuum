// Package rules decides whether a URL is in scope and whether a response is
// worth keeping. Scope decisions are made twice: once before the request (from
// the URL alone) and once after the headers arrive (media type, size), so the
// crawler wastes as little bandwidth as possible.
package rules

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

// Role describes why a URL was queued, because supporting files are allowed
// off-site while pages usually are not.
type Role int

const (
	// RolePage is a document that will be scanned for further links.
	RolePage Role = iota
	// RoleAsset is a supporting file: image, stylesheet, script, font, media.
	RoleAsset
)

func (r Role) String() string {
	if r == RoleAsset {
		return "asset"
	}
	return "page"
}

// Decision is the outcome of a scope check.
type Decision struct {
	Allow  bool
	Reason string
}

func allow() Decision { return Decision{Allow: true} }
func deny(f string, a ...any) Decision {
	return Decision{Allow: false, Reason: fmt.Sprintf(f, a...)}
}

// Engine holds the compiled rules for one job.
type Engine struct {
	cfg *config.Config

	include []*regexp.Regexp
	exclude []*regexp.Regexp

	typeRepl []typeRule

	// startOrigins is keyed on "host:port" (with the default port omitted).
	// The port is part of the identity of a site: example.com:8080 is not the
	// same site as example.com, and treating them as one silently pulls an
	// unrelated server into the crawl.
	startOrigins map[string]bool
	startHosts   map[string]bool
	startDomain  map[string]bool
	startURLs    []*url.URL

	allowExt map[string]bool
	blockExt map[string]bool
}

type typeRule struct {
	re   *regexp.Regexp
	mime string
}

// New compiles a rules Engine from a normalized Config.
func New(cfg *config.Config) (*Engine, error) {
	e := &Engine{
		cfg:          cfg,
		startOrigins: map[string]bool{},
		startHosts:   map[string]bool{},
		startDomain:  map[string]bool{},
		allowExt:     map[string]bool{},
		blockExt:     map[string]bool{},
	}
	for _, s := range cfg.StartURLs {
		u, err := urlx.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("start URL %q: %w", s, err)
		}
		e.startURLs = append(e.startURLs, u)
		h := strings.ToLower(u.Hostname())
		e.startOrigins[strings.ToLower(u.Host)] = true
		e.startHosts[h] = true
		e.startDomain[urlx.RegistrableDomain(h)] = true
	}
	var err error
	if e.include, err = compileAll(cfg.Scope.Include, globsToRegex(cfg.Scope.IncludeGlob)); err != nil {
		return nil, fmt.Errorf("scope.include: %w", err)
	}
	if e.exclude, err = compileAll(cfg.Scope.Exclude, globsToRegex(cfg.Scope.ExcludeGlob)); err != nil {
		return nil, fmt.Errorf("scope.exclude: %w", err)
	}
	for _, r := range cfg.Types.Replacements {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("types.replacements %q: %w", r.Pattern, err)
		}
		e.typeRepl = append(e.typeRepl, typeRule{re: re, mime: r.Type})
	}
	for _, x := range cfg.Types.AllowExtensions {
		e.allowExt[normExt(x)] = true
	}
	for _, x := range cfg.Types.BlockExtensions {
		e.blockExt[normExt(x)] = true
	}
	return e, nil
}

func normExt(x string) string {
	x = strings.ToLower(strings.TrimSpace(x))
	if x != "" && !strings.HasPrefix(x, ".") {
		x = "." + x
	}
	return x
}

func compileAll(groups ...[]string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, g := range groups {
		for _, p := range g {
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("bad pattern %q: %w", p, err)
			}
			out = append(out, re)
		}
	}
	return out, nil
}

// globsToRegex converts shell-style globs to anchored regexes so users can
// write "*/wp-admin/*" instead of learning regex syntax.
func globsToRegex(globs []string) []string {
	out := make([]string, 0, len(globs))
	for _, g := range globs {
		var b strings.Builder
		b.WriteString("^")
		for _, r := range g {
			switch r {
			case '*':
				b.WriteString(".*")
			case '?':
				b.WriteString(".")
			default:
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		b.WriteString("$")
		out = append(out, b.String())
	}
	return out
}

// ---------------------------------------------------------------- URL scope

// Prohibited applies the deny-only half of the scope rules: the prohibitions
// that no include pattern, allowed host or constraint can override.
//
// It is split out of CheckURL because these are the only rules that are safe to
// apply a second time. Everything CheckURL does after this point answers "what
// permits this URL", and those answers depend on context a queued item no
// longer carries — chiefly the page that linked to it, which the host+1 rule
// needs and which is not restored on resume. The prohibitions depend on nothing
// but the URL itself, so re-running them can only reach the same verdict the
// current settings would have reached at discovery time.
//
// The second application happens just before a queued URL is fetched, so that
// tightening the exclusions mid-run clears out what is already queued instead
// of only affecting links not yet found.
func (e *Engine) Prohibited(u *url.URL, role Role, depth int) (Decision, bool) {
	s := e.cfg.Scope
	full := u.String()

	if len(s.BlockHosts) > 0 && urlx.AnyHostMatchesURL(s.BlockHosts, u) {
		return deny("host is blocked"), true
	}
	for _, re := range e.exclude {
		if re.MatchString(full) {
			return deny("matched exclude %s", re.String()), true
		}
	}
	if s.MaxURLLength > 0 && len(full) > s.MaxURLLength {
		return deny("URL longer than %d characters", s.MaxURLLength), true
	}
	if s.SkipQueryURLs && u.RawQuery != "" {
		return deny("URL has a query string"), true
	}
	if s.MaxPathDepth > 0 && len(urlx.Segments(u)) > s.MaxPathDepth {
		return deny("deeper than %d path segments", s.MaxPathDepth), true
	}
	if s.MaxRepeatSegment > 0 && urlx.MaxRepeatedSegment(u) > s.MaxRepeatSegment {
		return deny("looks like a spider trap (repeated path segment)"), true
	}
	if e.cfg.Limits.MaxLevels > 0 && depth > e.cfg.Limits.MaxLevels {
		return deny("beyond level %d", e.cfg.Limits.MaxLevels), true
	}
	if ext := urlx.Extension(u); ext != "" {
		if e.blockExt[ext] {
			return deny("blocked extension %s", ext), true
		}
		if len(e.allowExt) > 0 && role == RoleAsset && !e.allowExt[ext] {
			return deny("extension %s not in the allow list", ext), true
		}
	}
	return allow(), false
}

// CheckURL decides whether a URL should be requested at all.
//
// Order of decision, highest priority first:
//  1. blocked hosts            — always wins
//  2. exclude patterns         — an explicit "no" beats every "yes"
//  3. structural traps         — absurd length/depth/repetition
//  4. include patterns         — an explicit "yes" beats the constraint
//  5. extra allowed hosts      — the multi-domain escape hatch
//  6. asset hosts (assets only)
//  7. supporting-file rule     — off-site assets when enabled
//  8. the coarse constraint    — host / subdomains / directory / ...
func (e *Engine) CheckURL(u *url.URL, role Role, depth int, from *url.URL) Decision {
	s := e.cfg.Scope
	host := strings.ToLower(u.Hostname())
	full := u.String()

	if d, forbidden := e.Prohibited(u, role, depth); forbidden {
		return d
	}

	for _, re := range e.include {
		if re.MatchString(full) {
			return allow()
		}
	}
	if urlx.AnyHostMatchesURL(s.Hosts, u) {
		return allow()
	}
	if role == RoleAsset && urlx.AnyHostMatchesURL(s.AssetHosts, u) {
		return allow()
	}
	if role == RoleAsset {
		// Supporting files are what make an offline copy render, but "from
		// anywhere" is a trap on any site old enough to have accumulated links
		// to image hosts that no longer exist. Related hosts are allowed by
		// default; genuinely third-party ones need naming.
		switch e.cfg.General.SupportingFiles {
		case config.SupportingAny:
			return allow()
		case config.SupportingRelated:
			for start := range e.startHosts {
				if urlx.SameSite(start, host) {
					return allow()
				}
			}
		}
	}

	switch s.Constraint {
	case config.ConstraintNone:
		return allow()

	case config.ConstraintRules:
		if len(e.include) == 0 && len(s.Hosts) == 0 {
			return deny("constraint is 'rules' but no include patterns are set")
		}
		return deny("did not match any include rule")

	case config.ConstraintHost:
		if e.startOrigins[strings.ToLower(u.Host)] {
			return allow()
		}
		return deny("%s is outside the starting host(s)", u.Host)

	case config.ConstraintSubdomains:
		if e.startDomain[urlx.RegistrableDomain(host)] {
			return allow()
		}
		return deny("host %s is outside %s", host, joinKeys(e.startDomain))

	case config.ConstraintHostPlus1:
		if e.startOrigins[strings.ToLower(u.Host)] {
			return allow()
		}
		// One hop off-site: allowed only when the linking page was on-site,
		// and the result is never crawled further for pages.
		if from != nil && e.startOrigins[strings.ToLower(from.Host)] {
			return allow()
		}
		return deny("%s is more than one hop off-site", u.Host)

	case config.ConstraintDirectory:
		for _, su := range e.startURLs {
			if urlx.UnderDirectory(su, u) {
				return allow()
			}
		}
		return deny("outside the starting directory")
	}
	return deny("no rule allowed this URL")
}

// InScope reports whether a host is part of the crawl itself rather than a
// third party a page happens to reference. Off-site hosts are given a shorter
// leash: they are optional, and waiting on one as long as on the site being
// mirrored is what makes a crawl appear to stall.
func (e *Engine) InScope(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if e.startOrigins[strings.ToLower(u.Host)] || e.startHosts[host] {
		return true
	}
	if urlx.AnyHostMatchesURL(e.cfg.Scope.Hosts, u) || urlx.AnyHostMatchesURL(e.cfg.Scope.AssetHosts, u) {
		return true
	}
	if e.cfg.Scope.Constraint == config.ConstraintSubdomains {
		return e.startDomain[urlx.RegistrableDomain(host)]
	}
	return false
}

// CrawlableForPages reports whether pages found at this URL should be followed.
// Under host+1 an off-site page is downloaded but never expanded, which is what
// stops "one external link" from turning into a crawl of the whole web.
func (e *Engine) CrawlableForPages(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	origin := strings.ToLower(u.Host)
	extra := urlx.AnyHostMatchesURL(e.cfg.Scope.Hosts, u)
	switch e.cfg.Scope.Constraint {
	case config.ConstraintNone, config.ConstraintRules:
		return true
	case config.ConstraintHostPlus1:
		return e.startOrigins[origin] || extra
	case config.ConstraintSubdomains:
		return e.startDomain[urlx.RegistrableDomain(host)] || extra
	case config.ConstraintDirectory:
		for _, su := range e.startURLs {
			if urlx.UnderDirectory(su, u) {
				return true
			}
		}
		return extra
	default:
		return e.startOrigins[origin] || extra
	}
}

func joinKeys(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, ", ")
}

// ---------------------------------------------------------------- media type

// ResolveMediaType applies the configured overrides, then falls back to the
// server's header, then to a guess from the URL extension.
func (e *Engine) ResolveMediaType(u *url.URL, headerType string) string {
	full := u.String()
	for _, r := range e.typeRepl {
		if r.re.MatchString(full) {
			return strings.ToLower(r.mime)
		}
	}
	mt := strings.ToLower(strings.TrimSpace(headerType))
	if i := strings.Index(mt, ";"); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	if mt == "" || mt == "application/octet-stream" || mt == "binary/octet-stream" {
		if guess := config.TypeForExtension(urlx.Extension(u)); guess != "" {
			return guess
		}
	}
	if mt == "" {
		return "application/octet-stream"
	}
	return mt
}

// IsHTML reports whether a media type should be scanned for links.
func (e *Engine) IsHTML(mt string) bool {
	return containsFold(e.cfg.Types.HTMLTypes, mt)
}

// IsCSS reports whether a media type should be parsed as a stylesheet.
func (e *Engine) IsCSS(mt string) bool {
	return containsFold(e.cfg.Types.CSSTypes, mt)
}

// IsText reports whether a document is safe to rewrite as text.
func (e *Engine) IsText(mt string) bool {
	if e.IsHTML(mt) || e.IsCSS(mt) {
		return true
	}
	return strings.HasPrefix(mt, "text/") ||
		strings.HasSuffix(mt, "+xml") ||
		strings.Contains(mt, "javascript") ||
		mt == "application/json"
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), s) {
			return true
		}
	}
	return false
}

// CheckResponse decides whether to keep a response body given its media type
// and (possibly unknown, i.e. -1) length. HTML and CSS bypass the type and size
// filters entirely: without them the mirror has no structure.
func (e *Engine) CheckResponse(u *url.URL, mediaType string, size int64) Decision {
	if e.IsHTML(mediaType) || e.IsCSS(mediaType) {
		return allow()
	}
	lim := e.cfg.Limits
	if size >= 0 {
		if lim.MinFileSize > 0 && size < lim.MinFileSize.V() {
			return deny("smaller than the %s minimum", lim.MinFileSize)
		}
		if lim.MaxFileSize > 0 && size > lim.MaxFileSize.V() {
			return deny("larger than the %s maximum", lim.MaxFileSize)
		}
	}

	t := e.cfg.Types
	switch t.Mode {
	case config.TypesAll:
		return allow()
	case config.TypesAllow:
		if e.typeListed(mediaType) {
			return allow()
		}
		return deny("media type %s is not in the allow list", mediaType)
	case config.TypesDisallow:
		if e.typeListed(mediaType) {
			return deny("media type %s is in the disallow list", mediaType)
		}
		return allow()
	}
	return allow()
}

func (e *Engine) typeListed(mediaType string) bool {
	for _, c := range e.cfg.Types.Categories {
		if config.CategoryMatches(c, mediaType) {
			return true
		}
	}
	for _, custom := range e.cfg.Types.Custom {
		custom = strings.ToLower(strings.TrimSpace(custom))
		if custom == "" {
			continue
		}
		if strings.HasSuffix(custom, "/*") {
			if strings.HasPrefix(mediaType, strings.TrimSuffix(custom, "*")) {
				return true
			}
		} else if custom == mediaType {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- helpers

// GuessRole infers whether a URL is a page or a supporting file from its
// extension, used when the referring markup does not make it obvious.
func GuessRole(u *url.URL) Role {
	ext := urlx.Extension(u)
	switch ext {
	case "", ".html", ".htm", ".xhtml", ".php", ".asp", ".aspx", ".jsp", ".cgi", ".shtml", ".do", ".action":
		return RolePage
	}
	return RoleAsset
}

// NormalizeForQueue applies the query-stripping options before a URL is keyed,
// so tracking parameters do not create duplicate copies of the same page.
func (e *Engine) NormalizeForQueue(u *url.URL) *url.URL {
	s := e.cfg.Scope
	if s.IgnoreQuery && u.RawQuery != "" {
		c := *u
		c.RawQuery = ""
		c.ForceQuery = false
		u = &c
	} else if len(s.DropQueryParams) > 0 {
		u = urlx.StripQueryParams(u, s.DropQueryParams)
	}
	if s.AmbiguousAsDir && !urlx.LooksLikeDirectory(u) && path.Ext(u.Path) == "" {
		c := *u
		c.Path += "/"
		u = &c
	}
	return urlx.Canonical(u)
}
