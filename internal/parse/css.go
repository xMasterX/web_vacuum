package parse

import (
	"strings"
)

// CSS finds every URL reference in a stylesheet and reports its byte range so
// the rewriter can swap in a local path. It handles the three forms that
// actually appear in the wild: url(...), @import "..." and @import url(...).
//
// The scanner is deliberately token-agnostic — it walks the text, skipping
// comments and strings, rather than building a parse tree. Stylesheets in the
// wild are full of hacks and vendor syntax that a strict parser rejects, and a
// rejected stylesheet means a mirror that renders unstyled.
func CSS(src []byte, base int) []Link {
	var out []Link
	s := string(src)
	i := 0
	n := len(s)

	for i < n {
		switch {
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return out
			}
			i += 2 + end + 2

		case s[i] == '"' || s[i] == '\'':
			i = skipString(s, i)

		case (s[i] == 'u' || s[i] == 'U') && hasFoldPrefix(s[i:], "url("):
			j := i + 4
			for j < n && isCSSSpace(s[j]) {
				j++
			}
			if j >= n {
				return out
			}
			var vStart, vEnd int
			if s[j] == '"' || s[j] == '\'' {
				q := s[j]
				j++
				vStart = j
				for j < n && s[j] != q {
					if s[j] == '\\' {
						j++
					}
					j++
				}
				vEnd = min(j, n)
				if j < n {
					j++
				}
			} else {
				vStart = j
				for j < n && s[j] != ')' {
					j++
				}
				vEnd = j
				// Unquoted url() values may carry trailing whitespace.
				for vEnd > vStart && isCSSSpace(s[vEnd-1]) {
					vEnd--
				}
			}
			if val := strings.TrimSpace(s[vStart:vEnd]); val != "" && !isCSSNonURL(val) {
				out = append(out, Link{
					Value: unescapeCSS(val),
					Kind:  KindStyle,
					Tag:   "css",
					Attr:  "url",
					Start: base + vStart,
					End:   base + vEnd,
				})
			}
			i = j

		case s[i] == '@' && hasFoldPrefix(s[i:], "@import"):
			j := i + len("@import")
			for j < n && isCSSSpace(j2byte(s, j)) {
				j++
			}
			// "@import url(...)" is handled by the url( branch on the next pass.
			if j < n && (s[j] == '"' || s[j] == '\'') {
				q := s[j]
				j++
				vStart := j
				for j < n && s[j] != q {
					if s[j] == '\\' {
						j++
					}
					j++
				}
				vEnd := min(j, n)
				if val := strings.TrimSpace(s[vStart:vEnd]); val != "" {
					out = append(out, Link{
						Value: unescapeCSS(val),
						Kind:  KindStyle,
						Tag:   "css",
						Attr:  "import",
						Start: base + vStart,
						End:   base + vEnd,
					})
				}
				if j < n {
					j++
				}
			}
			i = j

		default:
			i++
		}
	}
	return out
}

func j2byte(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func skipString(s string, i int) int {
	q := s[i]
	i++
	for i < len(s) {
		if s[i] == '\\' {
			i += 2
			continue
		}
		if s[i] == q {
			return i + 1
		}
		i++
	}
	return i
}

func hasFoldPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return strings.EqualFold(s[:len(prefix)], prefix)
}

func isCSSSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

// isCSSNonURL filters values that look like a url() argument but are not a
// fetchable resource.
func isCSSNonURL(v string) bool {
	lv := strings.ToLower(v)
	return strings.HasPrefix(lv, "data:") ||
		strings.HasPrefix(lv, "about:") ||
		strings.HasPrefix(lv, "#") ||
		strings.HasPrefix(lv, "var(") ||
		lv == "none"
}

// unescapeCSS resolves the backslash escapes CSS permits inside strings.
func unescapeCSS(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
			b.WriteByte(v[i])
			continue
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// EscapeCSSURL quotes a replacement path for use inside url(...).
func EscapeCSSURL(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\A `)
	return r.Replace(s)
}
