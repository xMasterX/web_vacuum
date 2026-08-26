package config

import (
	"sort"
	"strings"
)

// Categories groups media types into the buckets the filter UI exposes.
// Membership is matched by exact media type or by a "type/" prefix.
var Categories = map[string][]string{
	"images":    {"image/"},
	"audio":     {"audio/", "application/ogg"},
	"video":     {"video/", "application/x-mpegurl", "application/vnd.apple.mpegurl", "application/dash+xml"},
	"archives":  {"application/zip", "application/x-tar", "application/gzip", "application/x-7z-compressed", "application/x-rar-compressed", "application/vnd.rar", "application/x-bzip2", "application/x-xz"},
	"documents": {"application/pdf", "application/msword", "application/rtf", "text/plain", "application/vnd.openxmlformats-officedocument", "application/vnd.ms-", "application/vnd.oasis.opendocument", "application/epub+zip"},
	"fonts":     {"font/", "application/font-woff", "application/vnd.ms-fontobject", "application/x-font-ttf"},
	"scripts":   {"application/javascript", "text/javascript", "application/x-javascript", "application/ecmascript", "application/json", "application/wasm"},
	"styles":    {"text/css"},
	"data":      {"application/json", "application/xml", "text/xml", "text/csv"},
	"binaries":  {"application/octet-stream", "application/x-msdownload", "application/x-executable", "application/x-apple-diskimage", "application/vnd.debian.binary-package"},
}

// CategoryNames lists categories in a stable order for help text and the UI.
func CategoryNames() []string {
	out := make([]string, 0, len(Categories))
	for k := range Categories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CategoryMatches reports whether a media type falls in the named category.
func CategoryMatches(category, mediaType string) bool {
	prefixes, ok := Categories[strings.ToLower(category)]
	if !ok {
		return false
	}
	mt := strings.ToLower(mediaType)
	for _, p := range prefixes {
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(mt, p) {
				return true
			}
		} else if mt == p || strings.HasPrefix(mt, p) {
			return true
		}
	}
	return false
}

// DefaultHTMLTypes are the media types scanned for links.
func DefaultHTMLTypes() []string {
	return []string{
		"text/html",
		"application/xhtml+xml",
		"application/xhtml",
		"text/x-server-parsed-html",
	}
}

// DefaultCSSTypes are the media types parsed for url() and @import.
func DefaultCSSTypes() []string {
	return []string{"text/css"}
}

// userAgents are the presets selectable by short name. Anything not listed is
// used verbatim, so a custom string still works.
var userAgents = map[string]string{
	"chrome":     "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
	"chrome-win": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
	"firefox":    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:127.0) Gecko/20100101 Firefox/127.0",
	"safari":     "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15",
	"edge":       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0",
	"ios":        "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
	"android":    "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36",
	"googlebot":  "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
	"curl":       "curl/8.6.0",
	"webvacuum":  "webvacuum/1.0 (+https://github.com/xMasterX/web_vacuum)",
	"none":       "",
}

// UserAgentNames lists preset names for help text and the UI.
func UserAgentNames() []string {
	out := make([]string, 0, len(userAgents))
	for k := range userAgents {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ResolveUserAgent maps a preset name to its header value, or returns the input
// unchanged when it is already a full user-agent string.
func ResolveUserAgent(s string) string {
	if v, ok := userAgents[strings.ToLower(strings.TrimSpace(s))]; ok {
		return v
	}
	return s
}

// extensionByType supplies a file extension for media types whose URLs carry
// none, so the mirror opens correctly from disk.
var extensionByType = map[string]string{
	"text/html":                     ".html",
	"application/xhtml+xml":         ".xhtml",
	"text/css":                      ".css",
	"text/javascript":               ".js",
	"application/javascript":        ".js",
	"application/x-javascript":      ".js",
	"application/json":              ".json",
	"application/manifest+json":     ".webmanifest",
	"application/xml":               ".xml",
	"text/xml":                      ".xml",
	"text/plain":                    ".txt",
	"text/csv":                      ".csv",
	"image/jpeg":                    ".jpg",
	"image/png":                     ".png",
	"image/gif":                     ".gif",
	"image/webp":                    ".webp",
	"image/avif":                    ".avif",
	"image/svg+xml":                 ".svg",
	"image/x-icon":                  ".ico",
	"image/vnd.microsoft.icon":      ".ico",
	"image/bmp":                     ".bmp",
	"image/tiff":                    ".tiff",
	"audio/mpeg":                    ".mp3",
	"audio/ogg":                     ".ogg",
	"audio/wav":                     ".wav",
	"audio/webm":                    ".weba",
	"audio/aac":                     ".aac",
	"audio/flac":                    ".flac",
	"video/mp4":                     ".mp4",
	"video/webm":                    ".webm",
	"video/ogg":                     ".ogv",
	"video/quicktime":               ".mov",
	"video/x-matroska":              ".mkv",
	"application/pdf":               ".pdf",
	"application/zip":               ".zip",
	"application/gzip":              ".gz",
	"application/x-tar":             ".tar",
	"application/x-7z-compressed":   ".7z",
	"application/vnd.rar":           ".rar",
	"application/epub+zip":          ".epub",
	"font/woff":                     ".woff",
	"font/woff2":                    ".woff2",
	"font/ttf":                      ".ttf",
	"font/otf":                      ".otf",
	"application/vnd.ms-fontobject": ".eot",
	"application/wasm":              ".wasm",
	"application/rss+xml":           ".rss",
	"application/atom+xml":          ".atom",
	"application/x-mpegurl":         ".m3u8",
	"application/vnd.apple.mpegurl": ".m3u8",
}

// ExtensionForType returns a leading-dot extension for a media type, or "".
func ExtensionForType(mediaType string) string {
	return extensionByType[strings.ToLower(strings.TrimSpace(mediaType))]
}

// typeByExtension is the reverse lookup, used to guess a type before a response
// arrives so extension filters can skip a request entirely.
var typeByExtension = func() map[string]string {
	m := map[string]string{}
	for t, e := range extensionByType {
		if _, taken := m[e]; !taken {
			m[e] = t
		}
	}
	// Disambiguate the reverse mappings that matter.
	m[".html"] = "text/html"
	m[".htm"] = "text/html"
	m[".js"] = "text/javascript"
	m[".mjs"] = "text/javascript"
	m[".jpg"] = "image/jpeg"
	m[".jpeg"] = "image/jpeg"
	m[".xml"] = "application/xml"
	m[".gz"] = "application/gzip"
	m[".tgz"] = "application/gzip"
	m[".mp3"] = "audio/mpeg"
	m[".m4a"] = "audio/mp4"
	m[".m4v"] = "video/mp4"
	m[".avi"] = "video/x-msvideo"
	m[".wmv"] = "video/x-ms-wmv"
	m[".flv"] = "video/x-flv"
	m[".doc"] = "application/msword"
	m[".docx"] = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	m[".xls"] = "application/vnd.ms-excel"
	m[".xlsx"] = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	m[".ppt"] = "application/vnd.ms-powerpoint"
	m[".pptx"] = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	m[".exe"] = "application/x-msdownload"
	m[".dmg"] = "application/x-apple-diskimage"
	m[".deb"] = "application/vnd.debian.binary-package"
	m[".rpm"] = "application/x-rpm"
	m[".iso"] = "application/x-iso9660-image"
	m[".bz2"] = "application/x-bzip2"
	m[".xz"] = "application/x-xz"
	m[".rar"] = "application/vnd.rar"
	m[".webmanifest"] = "application/manifest+json"
	return m
}()

// TypeForExtension guesses a media type from a leading-dot extension.
func TypeForExtension(ext string) string {
	return typeByExtension[strings.ToLower(ext)]
}
