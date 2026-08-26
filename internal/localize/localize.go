// Package localize rewrites a downloaded document so it browses offline: every
// link that points at something we downloaded becomes a relative path, and
// everything else becomes an absolute URL so it still works when online.
//
// That second half matters. A half-mirrored site whose missing links are left
// relative produces a folder full of dead ends; making them absolute means the
// archive degrades into "click through to the live site" instead of "404".
package localize

import (
	"bytes"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/xMasterX/web_vacuum/internal/parse"
	"github.com/xMasterX/web_vacuum/internal/pathmap"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

// Resolver maps an absolute URL to the destination-relative path where its
// content was saved.
type Resolver interface {
	// LocalPath returns the saved path for a URL and whether it exists locally.
	LocalPath(abs *url.URL) (string, bool)
}

// LocalIndex is an optional extra a Resolver may implement. It lets a rewrite
// recognize links it already produced, which is what makes running the pass
// twice safe — a resumed job re-localizing a finished page must be a no-op.
type LocalIndex interface {
	// HasLocal reports whether a destination-relative path exists in the mirror.
	HasLocal(rel string) bool
}

// ResolverFunc adapts a function to the Resolver interface.
type ResolverFunc func(*url.URL) (string, bool)

// LocalPath implements Resolver.
func (f ResolverFunc) LocalPath(u *url.URL) (string, bool) { return f(u) }

// Options controls a rewrite pass.
type Options struct {
	parse.Options
	// StripScripts removes <script> elements from the saved page.
	StripScripts bool
	// InjectBanner adds an unobtrusive archive banner to each page.
	InjectBanner bool
	BannerText   string
	// AbsoluteFallback rewrites links we did not download into absolute URLs.
	AbsoluteFallback bool
	// EarlySubs run on the raw bytes before scanning; LateSubs run on the
	// rewritten output. Both are the user's escape hatch for site-specific fixes.
	EarlySubs []Substitution
	LateSubs  []Substitution
}

// Substitution is a compiled find/replace.
type Substitution struct {
	Re      *regexp.Regexp
	Literal string
	Repl    string
}

// Apply runs the substitution over src.
func (s Substitution) Apply(src []byte) []byte {
	if s.Re != nil {
		return s.Re.ReplaceAll(src, []byte(s.Repl))
	}
	if s.Literal == "" {
		return src
	}
	return bytes.ReplaceAll(src, []byte(s.Literal), []byte(s.Repl))
}

// Result reports what a rewrite did.
type Result struct {
	Content []byte
	// Rewritten is the number of links pointed at local files.
	Rewritten int
	// Absolutized is the number of links turned into absolute URLs.
	Absolutized int
	// Unresolved is the number of references that could not be parsed at all.
	Unresolved int
	// UnresolvedHosts counts, per host, the links left pointing at the live
	// site. It is what turns "some links are still wrong" into a specific,
	// answerable question about which domain was never downloaded.
	UnresolvedHosts map[string]int
	Changed         bool
}

// noteUnresolved records a link that had to stay absolute.
func (r *Result) noteUnresolved(u *url.URL) {
	if r.UnresolvedHosts == nil {
		r.UnresolvedHosts = map[string]int{}
	}
	r.UnresolvedHosts[strings.ToLower(u.Hostname())]++
}

// HTML rewrites an HTML document. docURL is where it came from, docRel is where
// it lives inside the destination folder.
func HTML(src []byte, docURL *url.URL, docRel string, res Resolver, opt Options) (*Result, error) {
	out := &Result{Content: src}
	for _, s := range opt.EarlySubs {
		src = s.Apply(src)
	}

	doc := parse.HTML(src, opt.Options)
	base := parse.ResolveBase(docURL, doc.Base)

	var reps []parse.Replacement
	for _, l := range doc.Links {
		if l.Kind == parse.KindBase {
			// Neutralize <base>: "./" means "this document's directory", which
			// is exactly what the rewritten relative links assume.
			reps = append(reps, parse.Replacement{Start: l.Start, End: l.End, Text: "./"})
			out.Changed = true
			continue
		}
		if l.Start >= l.End {
			continue
		}
		if alreadyLocal(l.Value, docRel, res) {
			continue
		}
		abs, ok := urlx.ResolveKeepFragment(base, l.Value)
		if !ok {
			out.Unresolved++
			continue
		}
		text, kind := resolveTarget(abs, docRel, res, opt)
		if text == "" {
			continue
		}
		switch kind {
		case targetLocal:
			out.Rewritten++
		case targetAbsolute:
			out.Absolutized++
			out.noteUnresolved(abs)
		}
		reps = append(reps, parse.Replacement{
			Start: l.Start,
			End:   l.End,
			Text:  encodeFor(l, text),
		})
		out.Changed = true
	}

	if opt.StripScripts {
		for _, r := range parse.ElementRanges(src, "script") {
			reps = append(reps, parse.Replacement{Start: r.Start, End: r.End, Text: ""})
			out.Changed = true
		}
	}

	content := parse.Apply(src, reps)

	if opt.InjectBanner {
		content = injectBanner(content, opt.BannerText, docURL)
		out.Changed = true
	}
	for _, s := range opt.LateSubs {
		content = s.Apply(content)
	}
	out.Content = content
	return out, nil
}

// CSS rewrites a stylesheet's url() and @import references.
func CSS(src []byte, docURL *url.URL, docRel string, res Resolver, opt Options) (*Result, error) {
	out := &Result{Content: src}
	for _, s := range opt.EarlySubs {
		src = s.Apply(src)
	}

	var reps []parse.Replacement
	for _, l := range parse.CSS(src, 0) {
		if alreadyLocal(l.Value, docRel, res) {
			continue
		}
		abs, ok := urlx.ResolveKeepFragment(docURL, l.Value)
		if !ok {
			out.Unresolved++
			continue
		}
		text, kind := resolveTarget(abs, docRel, res, opt)
		if text == "" {
			continue
		}
		switch kind {
		case targetLocal:
			out.Rewritten++
		case targetAbsolute:
			out.Absolutized++
			out.noteUnresolved(abs)
		}
		reps = append(reps, parse.Replacement{
			Start: l.Start,
			End:   l.End,
			Text:  parse.EscapeCSSURL(text),
		})
		out.Changed = true
	}

	content := parse.Apply(src, reps)
	for _, s := range opt.LateSubs {
		content = s.Apply(content)
	}
	out.Content = content
	return out, nil
}

// alreadyLocal reports whether a link's text is a relative path that already
// names a file in the mirror, meaning a previous pass wrote it.
func alreadyLocal(value, docRel string, res Resolver) bool {
	idx, ok := res.(LocalIndex)
	if !ok {
		return false
	}
	v := strings.TrimSpace(value)
	// Only "./" and "../" references can have come from this package, and they
	// must also name a file that exists in the mirror. Requiring both keeps a
	// site's own relative links eligible for rewriting while making a second
	// pass over an already-localized document a no-op.
	if !strings.HasPrefix(v, "./") && !strings.HasPrefix(v, "../") {
		return false
	}
	if i := strings.IndexByte(v, '#'); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return false
	}
	if dec, err := url.PathUnescape(v); err == nil {
		v = dec
	}
	joined := path.Join(path.Dir("/"+docRel), v)
	return idx.HasLocal(strings.TrimPrefix(joined, "/"))
}

type targetKind int

const (
	targetNone targetKind = iota
	targetLocal
	targetAbsolute
)

// resolveTarget decides what a link should become.
func resolveTarget(abs *url.URL, docRel string, res Resolver, opt Options) (string, targetKind) {
	// A fragment on a downloaded page has to survive: /a#top must become
	// ./a.html#top, not ./a.html.
	frag := abs.Fragment
	lookup := *abs
	lookup.Fragment = ""
	lookup.RawFragment = ""

	if rel, ok := res.LocalPath(&lookup); ok {
		link := pathmap.RelativeLink(docRel, rel)
		if frag != "" {
			link += "#" + frag
		}
		return link, targetLocal
	}
	if opt.AbsoluteFallback {
		return abs.String(), targetAbsolute
	}
	return "", targetNone
}

// encodeFor escapes a replacement for the syntax it is being written into.
func encodeFor(l parse.Link, text string) string {
	switch {
	case l.Tag == "css":
		return parse.EscapeCSSURL(text)
	case l.Descriptor != "":
		// srcset candidates keep their descriptor, which lives outside the
		// replaced range, so only the URL is escaped here.
		return parse.EscapeAttr(text)
	default:
		return parse.EscapeAttr(text)
	}
}

// injectBanner puts a small fixed notice at the top of the body so a reader
// months later knows they are looking at an archive and where it came from.
func injectBanner(src []byte, text string, docURL *url.URL) []byte {
	if text == "" {
		text = "Offline archive"
	}
	banner := fmt.Sprintf(
		`<div style="all:initial;display:block;position:relative;z-index:2147483647;`+
			`font:12px/1.6 -apple-system,system-ui,sans-serif;background:#1c1c1e;color:#f2f2f7;`+
			`padding:6px 12px;border-bottom:1px solid #3a3a3c">%s &middot; `+
			`<a href="%s" style="color:#64d2ff">%s</a></div>`,
		htmlEscape(text), htmlEscape(docURL.String()), htmlEscape(docURL.String()))

	lower := bytes.ToLower(src)
	if i := bytes.Index(lower, []byte("<body")); i >= 0 {
		if gt := bytes.IndexByte(src[i:], '>'); gt >= 0 {
			at := i + gt + 1
			var b bytes.Buffer
			b.Write(src[:at])
			b.WriteString(banner)
			b.Write(src[at:])
			return b.Bytes()
		}
	}
	return append([]byte(banner), src...)
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
