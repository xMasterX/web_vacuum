// Package pathmap turns URLs into local file paths. The mapping has to be
// deterministic (so resuming a job reuses the same files), reversible enough to
// rewrite links, and safe on every filesystem the tool might run on.
package pathmap

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

// Mapper converts URLs to destination-relative slash-separated paths.
type Mapper struct {
	cfg       *config.PathConfig
	indexFile string
	appendExt bool
	rewrites  []compiledRewrite
	// singleHost, when set, is the one host whose files go straight into the
	// destination root instead of a per-host folder.
	singleHost string
}

type compiledRewrite struct {
	re   *regexp.Regexp
	repl string
}

// New builds a Mapper. hostFolderExempt names the origin ("host" or
// "host:port") that should not get its own folder (empty to always use host
// folders). The port matters: two servers on one host are two different sites.
func New(cfg *config.PathConfig, hostFolderExempt string) (*Mapper, error) {
	m := &Mapper{
		cfg:        cfg,
		indexFile:  cfg.IndexFile,
		appendExt:  cfg.AppendExtension,
		singleHost: strings.ToLower(hostFolderExempt),
	}
	if m.indexFile == "" {
		m.indexFile = "index.html"
	}
	for _, r := range cfg.Rewrites {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("path rewrite %q: %w", r.Pattern, err)
		}
		m.rewrites = append(m.rewrites, compiledRewrite{re: re, repl: r.Replacement})
	}
	return m, nil
}

// Hint carries what the response told us, which can change the filename.
type Hint struct {
	MediaType string // e.g. "text/html"
	Filename  string // from Content-Disposition, may be empty
	IsHTML    bool
}

// Map returns the destination-relative path for a URL, using slashes as the
// separator regardless of platform. Callers resolve collisions themselves via
// Disambiguate, because only the crawler knows what is already claimed.
func (m *Mapper) Map(u *url.URL, h Hint) string {
	u = urlx.Canonical(u)

	var parts []string
	if m.cfg.HostFolders {
		origin := strings.ToLower(u.Host)
		if m.singleHost == "" || origin != m.singleHost {
			hostDir := strings.ToLower(u.Hostname())
			if p := u.Port(); p != "" {
				hostDir += "_" + p
			}
			parts = append(parts, m.sanitizeSegment(hostDir))
		}
	}

	segs := urlx.Segments(u)
	dirSegs := segs
	file := ""
	if !urlx.LooksLikeDirectory(u) && len(segs) > 0 {
		dirSegs = segs[:len(segs)-1]
		file = segs[len(segs)-1]
	}
	for _, s := range dirSegs {
		parts = append(parts, m.sanitizeSegment(decodeSegment(s)))
	}

	// A Content-Disposition filename wins over the URL's last segment, because
	// it is what the server actually wants the file called.
	if !m.cfg.IgnoreContentDisp && h.Filename != "" {
		file = filepath.Base(filepath.FromSlash(h.Filename))
	} else {
		file = decodeSegment(file)
	}

	if file == "" {
		// Directory-style URL: the response becomes the folder's index file.
		file = m.indexFile
	}

	file = m.sanitizeSegment(file)
	// Extension first, then the query suffix, so "/p.php?id=7" serving HTML
	// becomes "p.php@id=7.html" and stays openable by double-clicking it.
	file = m.applyExtension(file, h)
	file = m.applyQuerySuffix(file, u)

	parts = append(parts, file)
	rel := strings.Join(parts, "/")
	rel = m.applyRewrites(rel)
	rel = m.enforceLength(rel)
	if m.cfg.LowercasePaths {
		rel = strings.ToLower(rel)
	}
	return rel
}

// decodeSegment percent-decodes a path segment so "My%20Page" becomes
// "My Page" on disk, which is what a user browsing the folder expects.
func decodeSegment(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

// reservedWindows are device names that cannot be used as a filename on
// Windows even with an extension, so a Linux-made mirror stays portable.
var reservedWindows = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// lookalikes stand in for characters that are illegal or awkward in paths when
// ReplaceSpecial is off, keeping filenames readable instead of a row of "_".
var lookalikes = map[rune]rune{
	'/': '∕', ':': '：', '*': '＊', '?': '？',
	'"': '”', '<': '＜', '>': '＞', '|': '｜',
	'\\': '⧵', '#': '＃', '%': '％',
}

func (m *Mapper) sanitizeSegment(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			// Control characters are never written.
			continue
		case strings.ContainsRune(`/\:*?"<>|#%`, r):
			if m.cfg.ReplaceSpecial {
				b.WriteRune('_')
			} else if alt, ok := lookalikes[r]; ok {
				b.WriteRune(alt)
			} else {
				b.WriteRune('_')
			}
		case unicode.IsSpace(r) && r != ' ':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()

	// Trailing dots and spaces are silently stripped by Windows, which would
	// desynchronize the mirror from the rewritten links.
	out = strings.TrimRight(out, ". ")
	out = strings.TrimLeft(out, " ")
	if out == "" || out == "." || out == ".." {
		out = "_"
	}
	base := out
	if i := strings.Index(base, "."); i > 0 {
		base = base[:i]
	}
	if reservedWindows[strings.ToLower(base)] {
		out = "_" + out
	}
	return m.truncateSegment(out)
}

// truncateSegment keeps a segment inside the filesystem's per-name byte limit,
// preserving the extension and appending a hash so distinct names stay distinct.
func (m *Mapper) truncateSegment(s string) string {
	max := m.cfg.MaxSegmentLength
	if max <= 0 {
		max = 120
	}
	if utf8.RuneCountInString(s) <= max && len(s) <= 200 {
		return s
	}
	ext := path.Ext(s)
	if len(ext) > 12 {
		ext = ""
	}
	stem := strings.TrimSuffix(s, ext)
	sum := shortHash(s)
	keep := max - len(ext) - len(sum) - 1
	if keep < 8 {
		keep = 8
	}
	r := []rune(stem)
	if len(r) > keep {
		r = r[:keep]
	}
	return string(r) + "-" + sum + ext
}

func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// applyQuerySuffix folds a query string into the filename so that /p?id=1 and
// /p?id=2 are two files. Long or messy queries collapse to a stable hash.
func (m *Mapper) applyQuerySuffix(file string, u *url.URL) string {
	if u.RawQuery == "" {
		return file
	}
	q := u.RawQuery
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.' || r == '=' || r == '&' || r == ',':
			return r
		default:
			return '-'
		}
	}, q)
	if len(safe) > 48 {
		safe = "h" + shortHash(q)
	}
	ext := path.Ext(file)
	if len(ext) > 12 {
		ext = ""
	}
	stem := strings.TrimSuffix(file, ext)
	return stem + "@" + safe + ext
}

// applyExtension gives the file an extension matching its media type when the
// URL supplied none or a misleading one, so the browser renders it correctly
// from the local filesystem.
func (m *Mapper) applyExtension(file string, h Hint) string {
	if !m.appendExt || h.MediaType == "" {
		return file
	}
	want := config.ExtensionForType(h.MediaType)
	if want == "" {
		return file
	}
	have := strings.ToLower(path.Ext(file))
	if have == want {
		return file
	}
	// An HTML response served at /page.php must become page.php.html, or the
	// browser will offer to download it instead of rendering it.
	if h.IsHTML {
		if have == ".html" || have == ".htm" || have == ".xhtml" {
			return file
		}
		return file + want
	}
	if have == "" {
		return file + want
	}
	// A plausible existing extension is left alone: servers mislabel types far
	// more often than URLs lie about them.
	if config.TypeForExtension(have) != "" {
		return file
	}
	return file + want
}

func (m *Mapper) applyRewrites(rel string) string {
	for _, r := range m.rewrites {
		if r.re.MatchString(rel) {
			rel = r.re.ReplaceAllString(rel, r.repl)
		}
	}
	rel = strings.TrimLeft(path.Clean("/"+rel), "/")
	if rel == "." || rel == "" {
		rel = "index.html"
	}
	return rel
}

// enforceLength keeps the whole relative path under the configured budget by
// collapsing middle directories into a hash.
func (m *Mapper) enforceLength(rel string) string {
	max := m.cfg.MaxPathLength
	if max <= 0 {
		max = 3800
	}
	if len(rel) <= max {
		return rel
	}
	parts := strings.Split(rel, "/")
	if len(parts) < 3 {
		return rel
	}
	head, file := parts[0], parts[len(parts)-1]
	return head + "/_deep/" + shortHash(rel) + "/" + file
}

// Disambiguate returns a variant of rel that taken() reports as free. It
// inserts "~1", "~2"... before the extension, which keeps the file openable.
func Disambiguate(rel string, taken func(string) bool) string {
	if !taken(rel) {
		return rel
	}
	ext := path.Ext(rel)
	if len(ext) > 12 {
		ext = ""
	}
	stem := strings.TrimSuffix(rel, ext)
	for i := 1; i < 10000; i++ {
		cand := fmt.Sprintf("%s~%d%s", stem, i, ext)
		if !taken(cand) {
			return cand
		}
	}
	return fmt.Sprintf("%s~%s%s", stem, shortHash(rel), ext)
}

// RelativeLink returns the href that, placed inside document `from`, points at
// `to`. Both are destination-relative slash paths. The result is always
// relative so the mirror works from any folder, and from a file:// URL.
func RelativeLink(from, to string) string {
	fromDir := path.Dir("/" + from)
	rel, err := filepath.Rel(filepath.FromSlash(fromDir), filepath.FromSlash("/"+to))
	if err != nil {
		return "./" + to
	}
	out := filepath.ToSlash(rel)
	if out == "." {
		out = path.Base(to)
	}
	if !strings.HasPrefix(out, ".") {
		out = "./" + out
	}
	return escapeLink(out)
}

// escapeLink percent-encodes the characters that would break an href while
// leaving slashes and ordinary punctuation readable.
func escapeLink(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r == '/' || r == '.' || r == '-' || r == '_' || r == '~':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ':
			b.WriteString("%20")
		case r == '?' || r == '#' || r == '%' || r == '&' || r == '+' || r == '\\':
			b.WriteString(url.PathEscape(string(r)))
		case r < 0x80:
			b.WriteRune(r)
		default:
			b.WriteString(url.PathEscape(string(r)))
		}
	}
	return b.String()
}

// Local converts a destination-relative slash path into an OS path under dest.
// Mapped paths cannot contain ".." because sanitizeSegment rewrites it, but
// this is the single place every write goes through, so it checks anyway.
func Local(dest, rel string) (string, error) {
	joined := filepath.Join(dest, filepath.FromSlash(rel))
	clean := filepath.Clean(dest)
	if joined != clean && !strings.HasPrefix(joined, clean+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to write %q outside %q", rel, dest)
	}
	return joined, nil
}
