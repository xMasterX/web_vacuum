// Package parse extracts URLs from documents and rewrites them in place.
//
// Rewriting is byte-surgical rather than tree-based: the document is tokenized
// to find attribute values, but everything outside the spans being replaced is
// copied through untouched. Re-rendering a parsed tree would "fix" the markup,
// and an archive that silently differs from the original is a worse archive.
package parse

import (
	"bytes"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// Kind says what a link is for, which decides whether the crawler follows it as
// a page or fetches it as a supporting file.
type Kind int

const (
	// KindHyperlink is a navigable link: <a href>, <area href>.
	KindHyperlink Kind = iota
	// KindResource is a file the page needs to render.
	KindResource
	// KindFrame is an embedded document, crawled as a page but never navigated to.
	KindFrame
	// KindMeta is a <meta http-equiv=refresh> target.
	KindMeta
	// KindForm is a <form action>.
	KindForm
	// KindBase is <base href>; it changes resolution and is removed on localize.
	KindBase
	// KindStyle is a URL found inside CSS (inline attribute, <style>, or a file).
	KindStyle
	// KindScript is a URL recovered from JavaScript by a user pattern.
	KindScript
)

func (k Kind) String() string {
	switch k {
	case KindHyperlink:
		return "link"
	case KindResource:
		return "resource"
	case KindFrame:
		return "frame"
	case KindMeta:
		return "meta"
	case KindForm:
		return "form"
	case KindBase:
		return "base"
	case KindStyle:
		return "style"
	case KindScript:
		return "script"
	}
	return "?"
}

// Link is one URL reference found in a document, with the byte range of the
// text that has to change when the document is localized.
type Link struct {
	// Value is the reference as the author wrote it, HTML-unescaped.
	Value string
	Kind  Kind
	Tag   string
	Attr  string
	// Start and End bound the replaceable text inside the source bytes.
	Start, End int
	// NoFollow is set for rel="nofollow" hyperlinks.
	NoFollow bool
	// Descriptor is the trailing size/density part of a srcset candidate, which
	// must be preserved when the URL is replaced.
	Descriptor string
}

// Options controls what the HTML scanner looks at.
type Options struct {
	ScanComments   bool
	ScanSrcset     bool
	ScanInlineCSS  bool
	ScanScripts    bool
	FollowMeta     bool
	FollowForms    bool
	DataAttributes []string
	ExtraAttrs     []string
	// URLPatterns have their first capture group treated as a URL. They run
	// over script bodies and, when ScanComments is on, over comments.
	URLPatterns []*regexp.Regexp
}

// urlAttrs maps tag/attribute pairs to their kind. Anything not listed here is
// not a URL as far as HTML is concerned.
var urlAttrs = map[string]map[string]Kind{
	"a":          {"href": KindHyperlink, "ping": KindResource},
	"area":       {"href": KindHyperlink},
	"link":       {"href": KindResource, "imagesrcset": KindResource},
	"img":        {"src": KindResource, "longdesc": KindResource, "usemap": KindResource},
	"script":     {"src": KindResource},
	"iframe":     {"src": KindFrame, "srcdoc": KindFrame},
	"frame":      {"src": KindFrame, "longdesc": KindResource},
	"embed":      {"src": KindResource},
	"object":     {"data": KindResource, "codebase": KindResource},
	"applet":     {"code": KindResource, "codebase": KindResource, "archive": KindResource},
	"source":     {"src": KindResource},
	"track":      {"src": KindResource},
	"audio":      {"src": KindResource},
	"video":      {"src": KindResource, "poster": KindResource},
	"input":      {"src": KindResource, "formaction": KindForm},
	"button":     {"formaction": KindForm},
	"form":       {"action": KindForm},
	"blockquote": {"cite": KindResource},
	"q":          {"cite": KindResource},
	"del":        {"cite": KindResource},
	"ins":        {"cite": KindResource},
	"base":       {"href": KindBase},
	"body":       {"background": KindResource},
	"table":      {"background": KindResource},
	"td":         {"background": KindResource},
	"th":         {"background": KindResource},
	"menuitem":   {"icon": KindResource},
	"html":       {"manifest": KindResource},
}

// srcsetAttrs are attributes holding a comma-separated candidate list.
var srcsetAttrs = map[string]bool{"srcset": true, "imagesrcset": true, "data-srcset": true}

// Document is the result of scanning an HTML file.
type Document struct {
	Links []Link
	// Base is the value of <base href>, if the document declares one.
	Base string
	// Charset is the encoding declared in the document, if any.
	Charset string
	// NoIndex reports a robots meta tag asking not to index; NoFollowAll reports
	// one asking not to follow links.
	NoIndex     bool
	NoFollowAll bool
	// Title is used only for display in the UI.
	Title string
}

// HTML scans an HTML document and returns every URL reference with its byte
// range, in document order.
func HTML(src []byte, opt Options) *Document {
	return scanHTML(src, opt, true)
}

// scanHTML does the work; nested controls whether commented-out markup is
// scanned recursively, which must not recurse a second time.
func scanHTML(src []byte, opt Options, nested bool) *Document {
	doc := &Document{}
	z := html.NewTokenizer(bytes.NewReader(src))
	offset := 0
	inTitle := false

	for {
		tt := z.Next()
		raw := z.Raw()
		start := offset
		offset += len(raw)

		if tt == html.ErrorToken {
			break
		}

		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, attrs := scanTag(src[start:start+len(raw)], start)
			lower := strings.ToLower(name)
			if lower == "title" {
				inTitle = true
			}
			doc.collectTag(src, lower, attrs, opt)

		case html.EndTagToken:
			if n, _ := z.TagName(); strings.EqualFold(string(n), "title") {
				inTitle = false
			}

		case html.TextToken:
			if inTitle && doc.Title == "" {
				doc.Title = strings.TrimSpace(html.UnescapeString(string(raw)))
			}

		case html.CommentToken:
			if opt.ScanComments && nested {
				// Markup is routinely parked inside comments for one browser or
				// another to pick up, so the comment body is scanned as HTML
				// rather than merely regex-searched. Offsets shift by the
				// opening delimiter so rewriting still lands correctly.
				body := raw
				inner := start
				if bytes.HasPrefix(body, []byte("<!--")) {
					inner += 4
					body = body[4:]
					body = bytes.TrimSuffix(body, []byte("-->"))
				}
				sub := scanHTML(body, opt, false)
				for _, l := range sub.Links {
					l.Start += inner
					l.End += inner
					doc.Links = append(doc.Links, l)
				}
				doc.Links = append(doc.Links, patternLinks(body, inner, opt.URLPatterns, KindHyperlink)...)
			}
		}
	}

	// Script and style bodies need their own pass: the tokenizer hands them
	// over as opaque text, so they are located separately by tag boundaries.
	if opt.ScanInlineCSS {
		doc.Links = append(doc.Links, scanElementBodies(src, "style", func(body []byte, off int) []Link {
			return CSS(body, off)
		})...)
	}
	if opt.ScanScripts && len(opt.URLPatterns) > 0 {
		doc.Links = append(doc.Links, scanElementBodies(src, "script", func(body []byte, off int) []Link {
			return patternLinks(body, off, opt.URLPatterns, KindScript)
		})...)
	}

	sort.SliceStable(doc.Links, func(i, j int) bool { return doc.Links[i].Start < doc.Links[j].Start })
	return doc
}

// collectTag turns one start tag's attributes into Links.
func (doc *Document) collectTag(src []byte, tag string, attrs []rawAttr, opt Options) {
	byName := map[string]rawAttr{}
	for _, a := range attrs {
		byName[a.name] = a
	}

	// rel controls both the kind of a <link> and nofollow on an <a>.
	rel := strings.ToLower(byName["rel"].value)
	nofollow := strings.Contains(rel, "nofollow")

	if tag == "meta" {
		doc.collectMeta(byName, opt)
		return
	}

	table := urlAttrs[tag]
	for _, a := range attrs {
		kind, known := table[a.name]

		switch {
		case known:
		case opt.ScanSrcset && srcsetAttrs[a.name]:
			kind = KindResource
		case a.name == "style" && opt.ScanInlineCSS:
			// Inline style attributes can carry url() references.
			for _, l := range CSS([]byte(a.value), a.valueStart) {
				l.Tag, l.Attr = tag, "style"
				doc.Links = append(doc.Links, l)
			}
			continue
		case strings.HasPrefix(a.name, "data-") && containsFold(opt.DataAttributes, a.name):
			kind = KindResource
		case containsFold(opt.ExtraAttrs, a.name):
			kind = KindResource
		default:
			continue
		}

		if a.value == "" {
			continue
		}
		if srcsetAttrs[a.name] {
			if opt.ScanSrcset {
				doc.Links = append(doc.Links, Srcset(a.value, a.valueStart, tag, a.name)...)
			}
			continue
		}
		if tag == "iframe" && a.name == "srcdoc" {
			// srcdoc is a whole nested document; its links are relative to the
			// parent, but rewriting inside an escaped attribute is a trap, so
			// it is reported for discovery only.
			continue
		}
		if tag == "link" && a.name == "href" && rel != "" && isNonFetchableRel(rel) {
			continue
		}

		doc.Links = append(doc.Links, Link{
			Value:    a.value,
			Kind:     kind,
			Tag:      tag,
			Attr:     a.name,
			Start:    a.valueStart,
			End:      a.valueEnd,
			NoFollow: nofollow,
		})
		if kind == KindBase && doc.Base == "" {
			doc.Base = a.value
		}
	}
}

// collectMeta handles <meta charset>, robots directives and refresh redirects.
func (doc *Document) collectMeta(byName map[string]rawAttr, opt Options) {
	if cs, ok := byName["charset"]; ok && doc.Charset == "" {
		doc.Charset = strings.TrimSpace(cs.value)
	}
	equiv := strings.ToLower(strings.TrimSpace(byName["http-equiv"].value))
	name := strings.ToLower(strings.TrimSpace(byName["name"].value))
	content, hasContent := byName["content"]

	if equiv == "content-type" && hasContent && doc.Charset == "" {
		// The same care as elsewhere: the offset is applied to the original.
		if i := strings.Index(AsciiLowerString(content.value), "charset="); i >= 0 {
			doc.Charset = strings.TrimSpace(content.value[i+len("charset="):])
		}
	}
	if (name == "robots" || name == "googlebot") && hasContent {
		v := strings.ToLower(content.value)
		if strings.Contains(v, "noindex") {
			doc.NoIndex = true
		}
		if strings.Contains(v, "nofollow") {
			doc.NoFollowAll = true
		}
	}
	if equiv == "refresh" && hasContent && opt.FollowMeta {
		// content is "5; url=/next" — only the URL part is a link.
		if start, end, val := metaRefreshURL(content.value); val != "" {
			doc.Links = append(doc.Links, Link{
				Value: val,
				Kind:  KindMeta,
				Tag:   "meta",
				Attr:  "content",
				Start: content.valueStart + start,
				End:   content.valueStart + end,
			})
		}
	}
}

// metaRefreshURL locates the URL inside a refresh directive and returns its
// offsets within the attribute value.
func metaRefreshURL(content string) (int, int, string) {
	// Length-preserving: the index found here is used to slice content itself,
	// and an attribute value in a legacy encoding would otherwise shift it.
	lower := AsciiLowerString(content)
	i := strings.Index(lower, "url=")
	if i < 0 {
		return 0, 0, ""
	}
	start := i + 4
	if start > len(content) {
		return 0, 0, ""
	}
	for start < len(content) && (content[start] == ' ' || content[start] == '\t') {
		start++
	}
	end := len(content)
	if start < len(content) && (content[start] == '\'' || content[start] == '"') {
		q := content[start]
		start++
		if j := strings.IndexByte(content[start:], q); j >= 0 {
			end = start + j
		}
	} else {
		if j := strings.IndexAny(content[start:], " \t;"); j >= 0 {
			end = start + j
		}
	}
	if start >= end {
		return 0, 0, ""
	}
	return start, end, strings.TrimSpace(content[start:end])
}

// isNonFetchableRel reports rel values whose href should not be downloaded,
// because they are declarations rather than resources.
func isNonFetchableRel(rel string) bool {
	for _, r := range strings.Fields(rel) {
		switch r {
		case "dns-prefetch", "preconnect", "alternate", "canonical", "pingback",
			"webmention", "search", "profile", "author", "license", "me",
			"nofollow", "noopener", "noreferrer", "external":
			continue
		default:
			return false
		}
	}
	return rel != ""
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), s) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- raw tag scan

type rawAttr struct {
	name       string
	value      string
	valueStart int
	valueEnd   int
	quoted     bool
}

// scanTag parses a raw start tag's bytes into attributes with absolute offsets
// for their values. The tokenizer already validated the tag's shape, so this
// only has to handle well-formed input plus the usual unquoted-value sloppiness.
func scanTag(raw []byte, base int) (string, []rawAttr) {
	i := 0
	if i < len(raw) && raw[i] == '<' {
		i++
	}
	if i < len(raw) && raw[i] == '/' {
		i++
	}
	nameStart := i
	for i < len(raw) && !isSpaceByte(raw[i]) && raw[i] != '>' && raw[i] != '/' {
		i++
	}
	name := strings.ToLower(string(raw[nameStart:i]))

	var attrs []rawAttr
	for i < len(raw) {
		for i < len(raw) && isSpaceByte(raw[i]) {
			i++
		}
		if i >= len(raw) || raw[i] == '>' {
			break
		}
		if raw[i] == '/' {
			i++
			continue
		}
		aStart := i
		for i < len(raw) && !isSpaceByte(raw[i]) && raw[i] != '=' && raw[i] != '>' && raw[i] != '/' {
			i++
		}
		aName := strings.ToLower(string(raw[aStart:i]))
		if aName == "" {
			i++
			continue
		}
		for i < len(raw) && isSpaceByte(raw[i]) {
			i++
		}
		if i >= len(raw) || raw[i] != '=' {
			attrs = append(attrs, rawAttr{name: aName, valueStart: base + i, valueEnd: base + i})
			continue
		}
		i++ // '='
		for i < len(raw) && isSpaceByte(raw[i]) {
			i++
		}
		if i >= len(raw) {
			break
		}
		var vStart, vEnd int
		quoted := false
		if raw[i] == '"' || raw[i] == '\'' {
			q := raw[i]
			i++
			vStart = i
			for i < len(raw) && raw[i] != q {
				i++
			}
			vEnd = i
			if i < len(raw) {
				i++
			}
			quoted = true
		} else {
			vStart = i
			for i < len(raw) && !isSpaceByte(raw[i]) && raw[i] != '>' {
				i++
			}
			vEnd = i
		}
		attrs = append(attrs, rawAttr{
			name:       aName,
			value:      html.UnescapeString(string(raw[vStart:vEnd])),
			valueStart: base + vStart,
			valueEnd:   base + vEnd,
			quoted:     quoted,
		})
	}
	return name, attrs
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

// AsciiLower returns a lowercased copy that is byte-for-byte the same length as
// the input.
//
// bytes.ToLower must not be used where the result's offsets are applied back to
// the original: it decodes UTF-8, and every byte that is not valid UTF-8 comes
// back as a three-byte replacement character. A page in windows-1251 — which is
// most of the old web, and nearly all old forums — grows by several bytes per
// Cyrillic character, so an index taken from the lowercased copy lands past the
// end of the original and the slice panics.
//
// Tag and attribute names are ASCII, so folding only ASCII is all that case
// insensitivity requires here.
// AsciiLowerString is the string form, with the same guarantee and for the same
// reason: strings.ToLower re-encodes, so an index taken from its result cannot
// be applied to the original.
func AsciiLowerString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

func AsciiLower(src []byte) []byte {
	out := make([]byte, len(src))
	for i, b := range src {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		out[i] = b
	}
	return out
}

// scanElementBodies finds the text content of every <tag>...</tag> pair and
// hands it to fn with its absolute offset.
func scanElementBodies(src []byte, tag string, fn func([]byte, int) []Link) []Link {
	var out []Link
	open := []byte("<" + tag)
	closeTag := []byte("</" + tag)
	lower := AsciiLower(src)
	pos := 0
	for {
		i := bytes.Index(lower[pos:], open)
		if i < 0 {
			break
		}
		i += pos
		// Confirm this is the tag and not a prefix of a longer name.
		after := i + len(open)
		if after < len(src) && !isSpaceByte(src[after]) && src[after] != '>' && src[after] != '/' {
			pos = after
			continue
		}
		gt := bytes.IndexByte(src[i:], '>')
		if gt < 0 {
			break
		}
		bodyStart := i + gt + 1
		j := bytes.Index(lower[bodyStart:], closeTag)
		if j < 0 {
			break
		}
		bodyEnd := bodyStart + j
		out = append(out, fn(src[bodyStart:bodyEnd], bodyStart)...)
		pos = bodyEnd
	}
	return out
}

// patternLinks runs user-supplied regexes and reports the first capture group
// of each match as a link, with offsets so it can be rewritten.
func patternLinks(body []byte, base int, patterns []*regexp.Regexp, kind Kind) []Link {
	var out []Link
	for _, re := range patterns {
		for _, m := range re.FindAllSubmatchIndex(body, -1) {
			s, e := m[0], m[1]
			if len(m) >= 4 && m[2] >= 0 {
				s, e = m[2], m[3]
			}
			val := strings.TrimSpace(string(body[s:e]))
			if val == "" {
				continue
			}
			out = append(out, Link{
				Value: val,
				Kind:  kind,
				Tag:   "script",
				Attr:  "",
				Start: base + s,
				End:   base + e,
			})
		}
	}
	return out
}

// Srcset splits a srcset attribute into candidates, each with its own byte
// range so descriptors like "2x" and "640w" survive rewriting.
func Srcset(value string, base int, tag, attr string) []Link {
	var out []Link
	pos := 0
	for pos < len(value) {
		for pos < len(value) && (value[pos] == ',' || isSpaceByte(value[pos])) {
			pos++
		}
		if pos >= len(value) {
			break
		}
		urlStart := pos
		for pos < len(value) && !isSpaceByte(value[pos]) && value[pos] != ',' {
			pos++
		}
		urlEnd := pos
		// Anything up to the next comma is this candidate's descriptor.
		descStart := pos
		for pos < len(value) && value[pos] != ',' {
			pos++
		}
		desc := strings.TrimSpace(value[descStart:pos])
		if urlEnd > urlStart {
			out = append(out, Link{
				Value:      value[urlStart:urlEnd],
				Kind:       KindResource,
				Tag:        tag,
				Attr:       attr,
				Start:      base + urlStart,
				End:        base + urlEnd,
				Descriptor: desc,
			})
		}
	}
	return out
}

// ---------------------------------------------------------------- rewriting

// Replacement is an instruction to swap one byte range for new text.
type Replacement struct {
	Start, End int
	Text       string
}

// Apply performs replacements on src. Ranges must not overlap; they are sorted
// and applied back to front so earlier offsets stay valid.
func Apply(src []byte, reps []Replacement) []byte {
	if len(reps) == 0 {
		return src
	}
	sorted := append([]Replacement(nil), reps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })

	var out bytes.Buffer
	out.Grow(len(src) + 64*len(sorted))
	last := 0
	for _, r := range sorted {
		if r.Start < last || r.Start > len(src) || r.End > len(src) || r.Start > r.End {
			continue
		}
		out.Write(src[last:r.Start])
		out.WriteString(r.Text)
		last = r.End
	}
	out.Write(src[last:])
	return out.Bytes()
}

// EscapeAttr escapes a replacement value for use inside an HTML attribute.
func EscapeAttr(s string) string {
	r := strings.NewReplacer(`&`, "&amp;", `"`, "&quot;", `'`, "&#39;", `<`, "&lt;", `>`, "&gt;")
	return r.Replace(s)
}

// ResolveBase returns the effective base URL for a document, honouring a
// <base href> when the document declares one.
func ResolveBase(docURL *url.URL, baseHref string) *url.URL {
	if baseHref == "" {
		return docURL
	}
	b, err := url.Parse(strings.TrimSpace(baseHref))
	if err != nil {
		return docURL
	}
	return docURL.ResolveReference(b)
}

// Range is a byte span covering a whole element, open tag through close tag.
type Range struct{ Start, End int }

// ElementRanges finds the full extent of every <tag>...</tag> element,
// including the tags themselves. Used to strip elements from a saved document.
func ElementRanges(src []byte, tag string) []Range {
	var out []Range
	open := []byte("<" + tag)
	closeTag := []byte("</" + tag)
	lower := AsciiLower(src)
	pos := 0
	for {
		i := bytes.Index(lower[pos:], open)
		if i < 0 {
			return out
		}
		i += pos
		after := i + len(open)
		if after < len(src) && !isSpaceByte(src[after]) && src[after] != '>' && src[after] != '/' {
			pos = after
			continue
		}
		gt := bytes.IndexByte(src[i:], '>')
		if gt < 0 {
			return out
		}
		// A self-closing or void form has no matching end tag.
		bodyStart := i + gt + 1
		j := bytes.Index(lower[bodyStart:], closeTag)
		if j < 0 {
			out = append(out, Range{Start: i, End: bodyStart})
			return out
		}
		endStart := bodyStart + j
		endGt := bytes.IndexByte(src[endStart:], '>')
		if endGt < 0 {
			out = append(out, Range{Start: i, End: endStart})
			return out
		}
		out = append(out, Range{Start: i, End: endStart + endGt + 1})
		pos = endStart + endGt + 1
	}
}
