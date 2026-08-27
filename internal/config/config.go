// Package config holds every knob the crawler exposes. One Config fully
// describes a job: it is what the CLI flags, the YAML file and the web UI all
// produce, and what gets checkpointed next to the download so a job can be
// resumed months later with identical behaviour.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------- enums

// FileReplacement decides what happens when a file already exists on disk.
type FileReplacement string

const (
	ReplaceNever  FileReplacement = "never"  // keep what is on disk, skip the fetch
	ReplaceAlways FileReplacement = "always" // always re-download
	ReplaceNewer  FileReplacement = "newer"  // conditional GET, replace only if changed
)

// FileModification decides post-download processing.
type FileModification string

const (
	ModifyNone     FileModification = "none"     // byte-identical mirror, links untouched
	ModifyLocalize FileModification = "localize" // rewrite links to relative local paths
)

// Constraint is the coarse scope rule applied before the include/exclude lists.
type Constraint string

const (
	ConstraintNone       Constraint = "none"       // follow every link, anywhere
	ConstraintHost       Constraint = "host"       // exactly the starting host(s)
	ConstraintHostPlus1  Constraint = "host+1"     // starting host, plus one hop off-site
	ConstraintSubdomains Constraint = "subdomains" // the registrable domain and any subdomain
	ConstraintDirectory  Constraint = "directory"  // at or below the starting directory
	ConstraintRules      Constraint = "rules"      // only the include/exclude lists decide
)

// TypeFilterMode selects how the media-type filter behaves.
type TypeFilterMode string

const (
	TypesAll      TypeFilterMode = "all"      // download every media type
	TypesAllow    TypeFilterMode = "allow"    // download only the listed categories/types
	TypesDisallow TypeFilterMode = "disallow" // download everything except those listed
)

// SupportingMode decides which hosts may serve the images, stylesheets, scripts
// and fonts a page needs to render.
type SupportingMode string

const (
	// SupportingNone fetches supporting files only from hosts already in scope.
	SupportingNone SupportingMode = "none"
	// SupportingRelated also allows hosts sharing the site's registrable domain,
	// which covers the usual static.example.com / cdn.example.com split.
	SupportingRelated SupportingMode = "related"
	// SupportingAny allows supporting files from anywhere. It makes a modern
	// site on a third-party CDN render perfectly and an old forum crawl badly:
	// years of posts leave behind status badges, counters and image hosts that
	// no longer answer, and every one of them has to be tried before it can be
	// written off.
	SupportingAny SupportingMode = "any"
)

// LoginMode controls how HTTP authentication challenges are handled.
type LoginMode string

const (
	LoginNever  LoginMode = "never"  // never answer a 401, record the error
	LoginAuto   LoginMode = "auto"   // answer with configured credentials when present
	LoginPrompt LoginMode = "prompt" // ask interactively (TUI only), else behave as auto
)

// DefaultConnections and MaxConnections bound the size of the worker pool.
// They live here because the crawler, the CLI and the two interfaces all have
// to agree on them, and a second copy would eventually disagree.
const (
	DefaultConnections = 8
	MaxConnections     = 256
)

// ---------------------------------------------------------------- sections

// GeneralConfig mirrors the top-level behaviour switches.
type GeneralConfig struct {
	IgnoreRobots   bool `yaml:"ignore_robots" json:"ignore_robots"`
	IgnoreNofollow bool `yaml:"ignore_nofollow" json:"ignore_nofollow"`
	// IncludeSupportingFiles is the old on/off switch, kept so configs saved by
	// earlier runs still load. SupportingFiles supersedes it.
	IncludeSupportingFiles bool             `yaml:"include_supporting_files" json:"include_supporting_files"`
	SupportingFiles        SupportingMode   `yaml:"supporting_files" json:"supporting_files"`
	AlwaysDownloadHTMLCSS  bool             `yaml:"always_download_html_css" json:"always_download_html_css"`
	DownloadErrorPages     bool             `yaml:"download_error_pages" json:"download_error_pages"`
	FileReplacement        FileReplacement  `yaml:"file_replacement" json:"file_replacement"`
	FileModification       FileModification `yaml:"file_modification" json:"file_modification"`
	Connections            int              `yaml:"connections" json:"connections"`
	Login                  LoginMode        `yaml:"login" json:"login"`
	// KeepOriginals stores an untouched copy of every localized document under
	// .webvacuum/originals so localization can be re-run or undone.
	KeepOriginals bool `yaml:"keep_originals" json:"keep_originals"`
	// LocalizeEvery rewrites links periodically during a long download instead
	// of only at the end. A crawl that ends unexpectedly — a crash, a power
	// cut, a machine rebooted from under it — otherwise leaves a folder whose
	// every link still points at the live site, which is a poor return for
	// hours of downloading. Zero rewrites only at the end.
	LocalizeEvery Duration `yaml:"localize_every" json:"localize_every"`
}

// LimitConfig bounds the size of a crawl. Zero always means "no limit".
type LimitConfig struct {
	MaxLevels   int      `yaml:"max_levels" json:"max_levels"`
	MaxFiles    int64    `yaml:"max_files" json:"max_files"`
	MaxBytes    ByteSize `yaml:"max_bytes" json:"max_bytes"`
	MaxDuration Duration `yaml:"max_duration" json:"max_duration"`
	MinFileSize ByteSize `yaml:"min_file_size" json:"min_file_size"`
	MaxFileSize ByteSize `yaml:"max_file_size" json:"max_file_size"`
	MaxPerHost  int64    `yaml:"max_files_per_host" json:"max_files_per_host"`
	// MaxRate throttles total download throughput; 0 disables throttling.
	MaxRate ByteSize `yaml:"max_rate" json:"max_rate"`
}

// BasicAuth is one set of credentials scoped to a host (empty host = any).
type BasicAuth struct {
	Host     string `yaml:"host" json:"host"`
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"password"`
}

// RequestConfig covers everything about the HTTP conversation itself.
type RequestConfig struct {
	UserAgent      string            `yaml:"user_agent" json:"user_agent"`
	Attempts       int               `yaml:"attempts" json:"attempts"`
	Timeout        Duration          `yaml:"timeout" json:"timeout"`
	Delay          Duration          `yaml:"delay" json:"delay"`
	DelayJitter    Duration          `yaml:"delay_jitter" json:"delay_jitter"`
	PerHostConns   int               `yaml:"connections_per_host" json:"connections_per_host"`
	Headers        map[string]string `yaml:"headers" json:"headers"`
	SendReferer    bool              `yaml:"send_referer" json:"send_referer"`
	CookieJarFile  string            `yaml:"cookie_jar" json:"cookie_jar"`
	CookiesRaw     string            `yaml:"cookies" json:"cookies"`
	Auth           []BasicAuth       `yaml:"auth" json:"auth"`
	Proxy          string            `yaml:"proxy" json:"proxy"`
	InsecureTLS    bool              `yaml:"insecure_tls" json:"insecure_tls"`
	FollowRedirect int               `yaml:"max_redirects" json:"max_redirects"`
	HTTP2          bool              `yaml:"http2" json:"http2"`

	// OffsiteTimeout and OffsiteAttempts apply to hosts that are not part of
	// the crawl — the third-party images and scripts a page happens to
	// reference. They are optional decoration, so they get a short wait and few
	// tries instead of holding a connection for minutes while a host that died
	// years ago fails to answer.
	OffsiteTimeout  Duration `yaml:"offsite_timeout" json:"offsite_timeout"`
	OffsiteAttempts int      `yaml:"offsite_attempts" json:"offsite_attempts"`
}

// ScopeConfig decides which URLs are in bounds. This is the section the CLI
// exposes most aggressively, because picking the right domains is the single
// most common thing a user needs to get right.
type ScopeConfig struct {
	Constraint Constraint `yaml:"constraint" json:"constraint"`
	// Hosts is the allow-list of hosts treated as "in scope" in addition to the
	// hosts of the start URLs. A leading dot ("­.example.com") or a "*." prefix
	// matches every subdomain. This is the easy answer to "forum images live on
	// a different domain".
	Hosts []string `yaml:"hosts" json:"hosts"`
	// AssetHosts are hosts allowed only for supporting files (images, css, js),
	// never crawled for further pages.
	AssetHosts []string `yaml:"asset_hosts" json:"asset_hosts"`
	// BlockHosts always wins over everything else.
	BlockHosts []string `yaml:"block_hosts" json:"block_hosts"`

	Include []string `yaml:"include" json:"include"`
	Exclude []string `yaml:"exclude" json:"exclude"`
	// IncludeGlob / ExcludeGlob accept shell-style globs against the full URL,
	// for people who do not want to write regexes.
	IncludeGlob []string `yaml:"include_glob" json:"include_glob"`
	ExcludeGlob []string `yaml:"exclude_glob" json:"exclude_glob"`

	IgnoreQuery      bool     `yaml:"ignore_query" json:"ignore_query"`
	DropQueryParams  []string `yaml:"drop_query_params" json:"drop_query_params"`
	SkipQueryURLs    bool     `yaml:"skip_query_urls" json:"skip_query_urls"`
	AmbiguousAsDir   bool     `yaml:"treat_ambiguous_as_folders" json:"treat_ambiguous_as_folders"`
	ScanComments     bool     `yaml:"scan_comments" json:"scan_comments"`
	DownloadPDFLinks bool     `yaml:"download_links_in_pdfs" json:"download_links_in_pdfs"`
	// MaxURLLength drops absurdly long URLs (usually calendar/session traps).
	MaxURLLength int `yaml:"max_url_length" json:"max_url_length"`
	// MaxPathDepth drops URLs nested deeper than N path segments; 0 = unlimited.
	MaxPathDepth int `yaml:"max_path_depth" json:"max_path_depth"`
	// MaxRepeatSegment catches /a/a/a/... spider traps.
	MaxRepeatSegment int `yaml:"max_repeat_segment" json:"max_repeat_segment"`
}

// TypeReplacement forces a media type for URLs matching a pattern, for servers
// that hand out text/plain for stylesheets and similar nonsense.
type TypeReplacement struct {
	Pattern string `yaml:"pattern" json:"pattern"`
	Type    string `yaml:"type" json:"type"`
}

// TypeConfig is the media-type filter.
type TypeConfig struct {
	Mode         TypeFilterMode    `yaml:"mode" json:"mode"`
	Categories   []string          `yaml:"categories" json:"categories"`
	Custom       []string          `yaml:"custom" json:"custom"`
	HTMLTypes    []string          `yaml:"html_types" json:"html_types"`
	CSSTypes     []string          `yaml:"css_types" json:"css_types"`
	Replacements []TypeReplacement `yaml:"replacements" json:"replacements"`
	// Extensions is a convenience filter on the URL extension, applied before a
	// request is even made (so it saves bandwidth where media type cannot).
	AllowExtensions []string `yaml:"allow_extensions" json:"allow_extensions"`
	BlockExtensions []string `yaml:"block_extensions" json:"block_extensions"`
}

// PathRewrite renames files on disk via regex substitution.
type PathRewrite struct {
	Pattern     string `yaml:"pattern" json:"pattern"`
	Replacement string `yaml:"replacement" json:"replacement"`
}

// PathConfig controls how a URL becomes a path on disk.
type PathConfig struct {
	// HostFolders puts each host in its own top-level folder. Off means every
	// host's files are merged into the destination root.
	HostFolders bool `yaml:"host_folders" json:"host_folders"`
	// FlattenSingleHost drops the folder for the one host being mirrored, so a
	// single-site download opens at <destination>/index.html instead of
	// <destination>/example.com/index.html. Turn it off to always get a folder
	// per domain, keeping the layout identical whether one host or five end up
	// in the mirror.
	FlattenSingleHost bool          `yaml:"flatten_single_host" json:"flatten_single_host"`
	ReplaceSpecial    bool          `yaml:"replace_special" json:"replace_special"`
	IgnoreContentDisp bool          `yaml:"ignore_filename_in_headers" json:"ignore_filename_in_headers"`
	IndexFile         string        `yaml:"index_file" json:"index_file"`
	MaxSegmentLength  int           `yaml:"max_segment_length" json:"max_segment_length"`
	MaxPathLength     int           `yaml:"max_path_length" json:"max_path_length"`
	Rewrites          []PathRewrite `yaml:"rewrites" json:"rewrites"`
	// AppendExtension adds an extension derived from the media type when the URL
	// has none, so a browser opens the file correctly from disk.
	AppendExtension bool `yaml:"append_extension" json:"append_extension"`
	// LowercasePaths avoids collisions on case-insensitive filesystems.
	LowercasePaths bool `yaml:"lowercase" json:"lowercase"`
}

// Substitution is a text find/replace applied to documents.
type Substitution struct {
	Pattern     string `yaml:"pattern" json:"pattern"`
	Replacement string `yaml:"replacement" json:"replacement"`
	Literal     bool   `yaml:"literal" json:"literal"`
}

// WebpageConfig covers HTML parsing and link discovery.
type WebpageConfig struct {
	Encoding string `yaml:"encoding" json:"encoding"`
	// DataAttributes are extra `data-*` attributes scanned for URLs.
	DataAttributes []string `yaml:"data_attributes" json:"data_attributes"`
	// ExtraAttributes are arbitrary attribute names scanned for URLs.
	ExtraAttributes []string `yaml:"extra_attributes" json:"extra_attributes"`
	// URLExtraction are regexes whose first capture group is treated as a URL,
	// run over HTML and (when ScanScripts is on) inline/external JavaScript.
	URLExtraction     []string `yaml:"url_extraction" json:"url_extraction"`
	ScanScripts       bool     `yaml:"scan_scripts" json:"scan_scripts"`
	ScanSrcset        bool     `yaml:"scan_srcset" json:"scan_srcset"`
	ScanInlineCSS     bool     `yaml:"scan_inline_css" json:"scan_inline_css"`
	FollowMetaRefresh bool     `yaml:"follow_meta_refresh" json:"follow_meta_refresh"`
	FollowForms       bool     `yaml:"follow_forms" json:"follow_forms"`
	// UseSitemap seeds the crawl from /sitemap.xml and any sitemap named in robots.txt.
	UseSitemap bool `yaml:"use_sitemap" json:"use_sitemap"`

	EarlySubstitutions []Substitution `yaml:"early_substitutions" json:"early_substitutions"`
	LateSubstitutions  []Substitution `yaml:"late_substitutions" json:"late_substitutions"`

	// StripScripts removes <script> elements from saved pages, which makes many
	// archived sites behave better offline.
	StripScripts bool `yaml:"strip_scripts" json:"strip_scripts"`
	// InjectBanner adds a small offline-archive banner to saved pages.
	InjectBanner bool `yaml:"inject_banner" json:"inject_banner"`
}

// ResilienceConfig decides how a job behaves when the network misbehaves.
//
// The defaults here never abandon a download. Transport failures park the crawl
// in an offline state that keeps probing until the connection returns, rather
// than marking URLs failed and stopping. Archiving a large site is measured in
// hours, and in that time a connection will drop; needing someone present to
// notice and restart makes the whole exercise impractical.
type ResilienceConfig struct {
	// NeverGiveUp keeps the job alive through any number of network failures.
	NeverGiveUp bool `yaml:"never_give_up" json:"never_give_up"`
	// StopAfterConsecutiveFailures aborts the job after N back-to-back failures.
	// Only consulted when NeverGiveUp is false. 0 = never abort.
	StopAfterConsecutiveFailures int `yaml:"stop_after_consecutive_failures" json:"stop_after_consecutive_failures"`

	// OfflineProbeURLs are hit to decide whether the machine is back online.
	// Empty means probe the hosts being crawled instead of a third party.
	OfflineProbeURLs []string `yaml:"offline_probe_urls" json:"offline_probe_urls"`
	OfflineMinWait   Duration `yaml:"offline_min_wait" json:"offline_min_wait"`
	OfflineMaxWait   Duration `yaml:"offline_max_wait" json:"offline_max_wait"`

	// RetryBaseDelay/RetryMaxDelay bound the per-URL exponential backoff.
	RetryBaseDelay Duration `yaml:"retry_base_delay" json:"retry_base_delay"`
	RetryMaxDelay  Duration `yaml:"retry_max_delay" json:"retry_max_delay"`
	// RetryStatuses are HTTP status codes worth retrying.
	RetryStatuses []int `yaml:"retry_statuses" json:"retry_statuses"`
	// HonorRetryAfter obeys the Retry-After header on 429/503.
	HonorRetryAfter bool `yaml:"honor_retry_after" json:"honor_retry_after"`
	// ThrottleOn429 halves per-host concurrency after repeated rate limiting and
	// recovers slowly, instead of hammering until the host bans the client.
	ThrottleOn429 bool `yaml:"throttle_on_429" json:"throttle_on_429"`

	// RetryPasses re-queues everything that permanently failed, up to N extra
	// sweeps once the main frontier drains. This catches transient server-side
	// problems that outlived the per-URL attempts.
	RetryPasses     int      `yaml:"retry_passes" json:"retry_passes"`
	RetryPassDelay  Duration `yaml:"retry_pass_delay" json:"retry_pass_delay"`
	CheckpointEvery Duration `yaml:"checkpoint_every" json:"checkpoint_every"`
	// ResumeOnStart picks up an interrupted job in the destination automatically.
	ResumeOnStart bool `yaml:"resume_on_start" json:"resume_on_start"`
}

// RenderMode decides when a page is handed to a real browser.
type RenderMode string

const (
	// RenderNever fetches HTML over plain HTTP and never starts a browser.
	RenderNever RenderMode = "never"
	// RenderAuto starts a browser only for pages whose source markup carries
	// almost no content, which is the signature of a JavaScript-built page.
	RenderAuto RenderMode = "auto"
	// RenderAlways renders every HTML page.
	RenderAlways RenderMode = "always"
)

// RenderConfig controls JavaScript rendering through a headless browser.
//
// This is off by default and deliberately so: it is far slower and heavier than
// fetching HTML, and most sites do not need it. It exists for the ones that
// serve an empty shell and build the page in the browser, where a plain
// download produces a folder of blank pages.
type RenderConfig struct {
	Mode RenderMode `yaml:"mode" json:"mode"`
	// Match renders only URLs matching these regexes. Setting it implies that
	// rendering is wanted, so the mode does not also have to be changed.
	Match []string `yaml:"match" json:"match"`
	// Skip never renders URLs matching these, even in "always" mode.
	Skip []string `yaml:"skip" json:"skip"`

	// Tabs is how many pages render at once. Kept separate from the crawl's
	// connection count because a browser tab costs orders of magnitude more
	// than an HTTP request.
	Tabs int `yaml:"tabs" json:"tabs"`
	// Wait is an extra pause after the page settles, for work no event
	// announces.
	Wait Duration `yaml:"wait" json:"wait"`
	// Idle is how long the network must be quiet before a page counts as done.
	Idle    Duration `yaml:"idle" json:"idle"`
	Timeout Duration `yaml:"timeout" json:"timeout"`

	// Scroll walks each page to the bottom before capturing, which is what
	// triggers lazy-loaded images on galleries and forums.
	Scroll      bool `yaml:"scroll" json:"scroll"`
	ScrollLimit int  `yaml:"scroll_limit" json:"scroll_limit"`

	// FollowRequests queues everything the page asked the network for. This is
	// the main reason rendering finds files a parser cannot: images, fonts and
	// API responses that appear in no markup.
	FollowRequests bool `yaml:"follow_requests" json:"follow_requests"`
	// Block lists URL wildcards the browser refuses to load, for skipping heavy
	// media that does not change the DOM.
	Block []string `yaml:"block" json:"block"`

	// KeepScripts leaves the page's own scripts in the saved copy. Off by
	// default, and that default matters: the browser has already built the DOM,
	// so letting the same scripts run again offline makes them tear it back
	// down, re-request an API that is not there, and leave a blank page. The
	// rendered markup is the artifact worth keeping.
	KeepScripts bool `yaml:"keep_scripts" json:"keep_scripts"`

	ChromePath   string `yaml:"chrome_path" json:"chrome_path"`
	RemoteURL    string `yaml:"remote_url" json:"remote_url"`
	Headful      bool   `yaml:"headful" json:"headful"`
	NoSandbox    bool   `yaml:"no_sandbox" json:"no_sandbox"`
	WindowWidth  int    `yaml:"window_width" json:"window_width"`
	WindowHeight int    `yaml:"window_height" json:"window_height"`
}

// Enabled reports whether any rendering could happen.
func (r RenderConfig) Enabled() bool {
	return r.Mode == RenderAuto || r.Mode == RenderAlways || len(r.Match) > 0
}

// LogConfig controls on-disk logging.
type LogConfig struct {
	File     string `yaml:"file" json:"file"`
	Level    string `yaml:"level" json:"level"`
	LogHTTP  bool   `yaml:"log_http" json:"log_http"`
	MaxLines int    `yaml:"ui_scrollback" json:"ui_scrollback"`
}

// ---------------------------------------------------------------- root

// Config is a complete job description.
type Config struct {
	Name        string   `yaml:"name" json:"name"`
	StartURLs   []string `yaml:"start_urls" json:"start_urls"`
	Destination string   `yaml:"destination" json:"destination"`

	General    GeneralConfig    `yaml:"general" json:"general"`
	Limits     LimitConfig      `yaml:"limits" json:"limits"`
	Request    RequestConfig    `yaml:"request" json:"request"`
	Scope      ScopeConfig      `yaml:"scope" json:"scope"`
	Types      TypeConfig       `yaml:"types" json:"types"`
	Paths      PathConfig       `yaml:"paths" json:"paths"`
	Webpage    WebpageConfig    `yaml:"webpage" json:"webpage"`
	Render     RenderConfig     `yaml:"render" json:"render"`
	Resilience ResilienceConfig `yaml:"resilience" json:"resilience"`
	Log        LogConfig        `yaml:"log" json:"log"`
}

// Default returns a configuration tuned for the common case: mirror one site,
// stay on its host, keep every asset it needs, and never stop because the wifi
// dropped.
func Default() *Config {
	return &Config{
		General: GeneralConfig{
			IncludeSupportingFiles: true,
			SupportingFiles:        SupportingRelated,
			FileReplacement:        ReplaceNewer,
			LocalizeEvery:          Duration(5 * time.Minute),
			FileModification:       ModifyLocalize,
			Connections:            8,
			Login:                  LoginAuto,
		},
		Limits: LimitConfig{
			MaxLevels: 0,
		},
		Request: RequestConfig{
			UserAgent:       "chrome",
			Attempts:        10,
			Timeout:         Duration(45 * time.Second),
			Delay:           0,
			PerHostConns:    4,
			SendReferer:     true,
			FollowRedirect:  12,
			HTTP2:           true,
			OffsiteTimeout:  Duration(8 * time.Second),
			OffsiteAttempts: 2,
			Headers:         map[string]string{},
		},
		Scope: ScopeConfig{
			Constraint:       ConstraintHost,
			MaxURLLength:     2000,
			MaxRepeatSegment: 3,
		},
		Types: TypeConfig{
			Mode:      TypesAll,
			HTMLTypes: DefaultHTMLTypes(),
			CSSTypes:  DefaultCSSTypes(),
		},
		Paths: PathConfig{
			HostFolders:       true,
			FlattenSingleHost: true,
			ReplaceSpecial:    true,
			IndexFile:         "index.html",
			MaxSegmentLength:  120,
			MaxPathLength:     3800,
			AppendExtension:   true,
		},
		Webpage: WebpageConfig{
			ScanSrcset:        true,
			ScanInlineCSS:     true,
			FollowMetaRefresh: true,
			ScanScripts:       false,
			UseSitemap:        false,
		},
		Resilience: ResilienceConfig{
			NeverGiveUp:     true,
			OfflineMinWait:  Duration(2 * time.Second),
			OfflineMaxWait:  Duration(2 * time.Minute),
			RetryBaseDelay:  Duration(2 * time.Second),
			RetryMaxDelay:   Duration(5 * time.Minute),
			RetryStatuses:   []int{408, 425, 429, 500, 502, 503, 504, 522, 524},
			HonorRetryAfter: true,
			ThrottleOn429:   true,
			RetryPasses:     2,
			RetryPassDelay:  Duration(30 * time.Second),
			CheckpointEvery: Duration(5 * time.Second),
			ResumeOnStart:   true,
		},
		Render: RenderConfig{
			Mode:           RenderNever,
			Tabs:           2,
			Wait:           Duration(500 * time.Millisecond),
			Idle:           Duration(800 * time.Millisecond),
			Timeout:        Duration(45 * time.Second),
			ScrollLimit:    40000,
			FollowRequests: true,
			WindowWidth:    1366,
			WindowHeight:   900,
		},
		Log: LogConfig{
			Level:    "info",
			MaxLines: 2000,
		},
	}
}

// ---------------------------------------------------------------- validation

// Normalize fills in derived defaults and returns an error for anything that
// cannot produce a working crawl.
// dedupeOnce removes repeated entries from a list of rules, keeping the first
// of each and the order they were written in.
func dedupeOnce(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// dedupeLists collapses repeated entries in every list of rules.
//
// The lists are built by appending: settings read from a file first, then
// anything named on the command line, so that -d and --exclude add to what is
// configured rather than replacing it. That is the right behaviour for a single
// run and the wrong one when the file being read is itself the product of an
// earlier run — which is exactly what a resumed job reads. Passing the same
// --exclude on each of five runs would otherwise leave five copies of it, and
// the list grows for as long as the job is worked on.
//
// Collapsing them is safe because every one of these lists is a set in all but
// type: a pattern that matches, matches once, and a host named twice is not
// more allowed than a host named once.
func (c *Config) dedupeLists() {
	c.StartURLs = dedupeOnce(c.StartURLs)
	c.Scope.Hosts = dedupeOnce(c.Scope.Hosts)
	c.Scope.AssetHosts = dedupeOnce(c.Scope.AssetHosts)
	c.Scope.BlockHosts = dedupeOnce(c.Scope.BlockHosts)
	c.Scope.Include = dedupeOnce(c.Scope.Include)
	c.Scope.Exclude = dedupeOnce(c.Scope.Exclude)
	c.Scope.IncludeGlob = dedupeOnce(c.Scope.IncludeGlob)
	c.Scope.ExcludeGlob = dedupeOnce(c.Scope.ExcludeGlob)
	c.Scope.DropQueryParams = dedupeOnce(c.Scope.DropQueryParams)
	c.Types.Categories = dedupeOnce(c.Types.Categories)
	c.Types.AllowExtensions = dedupeOnce(c.Types.AllowExtensions)
	c.Types.BlockExtensions = dedupeOnce(c.Types.BlockExtensions)
	c.Render.Match = dedupeOnce(c.Render.Match)
	c.Render.Skip = dedupeOnce(c.Render.Skip)
	c.Render.Block = dedupeOnce(c.Render.Block)
}

func (c *Config) Normalize() error {
	c.dedupeLists()
	if len(c.StartURLs) == 0 {
		return fmt.Errorf("no start URL given")
	}
	for i, raw := range c.StartURLs {
		s := strings.TrimSpace(raw)
		if s == "" {
			return fmt.Errorf("start URL %d is empty", i+1)
		}
		if !strings.Contains(s, "://") {
			s = defaultScheme(s) + "://" + s
		}
		u, err := url.Parse(s)
		if err != nil {
			return fmt.Errorf("start URL %q: %w", raw, err)
		}
		if u.Host == "" {
			return fmt.Errorf("start URL %q has no host", raw)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("start URL %q: unsupported scheme %q", raw, u.Scheme)
		}
		c.StartURLs[i] = u.String()
	}

	if c.Name == "" {
		if u, err := url.Parse(c.StartURLs[0]); err == nil {
			c.Name = u.Hostname()
		} else {
			c.Name = "site"
		}
	}
	if c.Destination == "" {
		c.Destination = filepath.Join(".", sanitizeName(c.Name))
	}
	abs, err := filepath.Abs(c.Destination)
	if err != nil {
		return fmt.Errorf("destination: %w", err)
	}
	c.Destination = abs

	if c.General.Connections <= 0 {
		c.General.Connections = DefaultConnections
	}
	if c.General.Connections > MaxConnections {
		c.General.Connections = MaxConnections
	}
	if c.Request.PerHostConns <= 0 {
		c.Request.PerHostConns = c.General.Connections
	}
	if c.Request.Attempts <= 0 {
		c.Request.Attempts = 1
	}
	if c.Request.Timeout <= 0 {
		c.Request.Timeout = Duration(45 * time.Second)
	}
	if c.Request.FollowRedirect <= 0 {
		c.Request.FollowRedirect = 12
	}
	if c.Request.OffsiteTimeout <= 0 {
		c.Request.OffsiteTimeout = Duration(8 * time.Second)
	}
	if c.Request.OffsiteAttempts <= 0 {
		c.Request.OffsiteAttempts = 2
	}
	// Waiting longer on an off-site file than on the site itself makes no sense.
	if c.Request.OffsiteTimeout > c.Request.Timeout {
		c.Request.OffsiteTimeout = c.Request.Timeout
	}
	if c.Request.OffsiteAttempts > c.Request.Attempts {
		c.Request.OffsiteAttempts = c.Request.Attempts
	}
	if c.Request.Headers == nil {
		c.Request.Headers = map[string]string{}
	}

	switch c.General.FileReplacement {
	case ReplaceNever, ReplaceAlways, ReplaceNewer:
	case "":
		c.General.FileReplacement = ReplaceNewer
	default:
		return fmt.Errorf("file_replacement: unknown value %q", c.General.FileReplacement)
	}
	switch c.General.FileModification {
	case ModifyNone, ModifyLocalize:
	case "":
		c.General.FileModification = ModifyLocalize
	default:
		return fmt.Errorf("file_modification: unknown value %q", c.General.FileModification)
	}
	switch c.General.SupportingFiles {
	case SupportingNone, SupportingRelated, SupportingAny:
	case "":
		// Fall back to the older boolean so a job saved by a previous version
		// resumes with the behaviour it was started with.
		if c.General.IncludeSupportingFiles {
			c.General.SupportingFiles = SupportingRelated
		} else {
			c.General.SupportingFiles = SupportingNone
		}
	default:
		return fmt.Errorf("supporting_files: unknown value %q (use none, related or any)", c.General.SupportingFiles)
	}
	c.General.IncludeSupportingFiles = c.General.SupportingFiles != SupportingNone

	switch c.General.Login {
	case LoginNever, LoginAuto, LoginPrompt:
	case "":
		c.General.Login = LoginAuto
	default:
		return fmt.Errorf("login: unknown value %q", c.General.Login)
	}
	switch c.Scope.Constraint {
	case ConstraintNone, ConstraintHost, ConstraintHostPlus1, ConstraintSubdomains, ConstraintDirectory, ConstraintRules:
	case "":
		c.Scope.Constraint = ConstraintHost
	default:
		return fmt.Errorf("scope.constraint: unknown value %q", c.Scope.Constraint)
	}
	switch c.Types.Mode {
	case TypesAll, TypesAllow, TypesDisallow:
	case "":
		c.Types.Mode = TypesAll
	default:
		return fmt.Errorf("types.mode: unknown value %q", c.Types.Mode)
	}
	for _, cat := range c.Types.Categories {
		if _, ok := Categories[strings.ToLower(cat)]; !ok {
			return fmt.Errorf("types.categories: unknown category %q (known: %s)",
				cat, strings.Join(CategoryNames(), ", "))
		}
	}

	if len(c.Types.HTMLTypes) == 0 {
		c.Types.HTMLTypes = DefaultHTMLTypes()
	}
	if len(c.Types.CSSTypes) == 0 {
		c.Types.CSSTypes = DefaultCSSTypes()
	}
	if c.Paths.IndexFile == "" {
		c.Paths.IndexFile = "index.html"
	}
	if !c.Paths.HostFolders {
		// With no per-host folders there is nothing to flatten, and leaving the
		// flag set would make the two settings look like they disagree.
		c.Paths.FlattenSingleHost = false
	}
	if c.Paths.MaxSegmentLength <= 0 || c.Paths.MaxSegmentLength > 200 {
		c.Paths.MaxSegmentLength = 120
	}
	if c.Paths.MaxPathLength <= 0 {
		c.Paths.MaxPathLength = 3800
	}
	if c.Resilience.OfflineMinWait <= 0 {
		c.Resilience.OfflineMinWait = Duration(2 * time.Second)
	}
	if c.Resilience.OfflineMaxWait < c.Resilience.OfflineMinWait {
		c.Resilience.OfflineMaxWait = Duration(2 * time.Minute)
	}
	if c.Resilience.RetryBaseDelay <= 0 {
		c.Resilience.RetryBaseDelay = Duration(2 * time.Second)
	}
	if c.Resilience.RetryMaxDelay < c.Resilience.RetryBaseDelay {
		c.Resilience.RetryMaxDelay = Duration(5 * time.Minute)
	}
	if len(c.Resilience.RetryStatuses) == 0 {
		c.Resilience.RetryStatuses = []int{408, 425, 429, 500, 502, 503, 504, 522, 524}
	}
	if c.Resilience.CheckpointEvery <= 0 {
		c.Resilience.CheckpointEvery = Duration(5 * time.Second)
	}
	if c.Log.MaxLines <= 0 {
		c.Log.MaxLines = 2000
	}

	switch c.Render.Mode {
	case RenderNever, RenderAuto, RenderAlways:
	case "":
		c.Render.Mode = RenderNever
	default:
		return fmt.Errorf("render.mode: unknown value %q (use never, auto or always)", c.Render.Mode)
	}
	// Naming URLs to render is a clear statement of intent; honouring it
	// without also demanding a mode change avoids a confusing silent no-op.
	if len(c.Render.Match) > 0 && c.Render.Mode == RenderNever {
		c.Render.Mode = RenderAuto
	}
	if c.Render.Tabs <= 0 {
		c.Render.Tabs = 2
	}
	if c.Render.Tabs > 32 {
		c.Render.Tabs = 32
	}
	if c.Render.Timeout <= 0 {
		c.Render.Timeout = Duration(45 * time.Second)
	}
	if c.Render.Idle <= 0 {
		c.Render.Idle = Duration(800 * time.Millisecond)
	}
	if c.Render.ScrollLimit <= 0 {
		c.Render.ScrollLimit = 40000
	}
	for _, group := range []struct {
		label string
		pats  []string
	}{{"render.match", c.Render.Match}, {"render.skip", c.Render.Skip}} {
		for _, p := range group.pats {
			if _, err := regexp.Compile(p); err != nil {
				return fmt.Errorf("%s: bad regex %q: %w", group.label, p, err)
			}
		}
	}

	// Compile every user-supplied regex up front so a typo fails at start-up
	// rather than 40 000 files into a crawl.
	for _, group := range []struct {
		label string
		pats  []string
	}{
		{"scope.include", c.Scope.Include},
		{"scope.exclude", c.Scope.Exclude},
		{"webpage.url_extraction", c.Webpage.URLExtraction},
	} {
		for _, p := range group.pats {
			if _, err := regexp.Compile(p); err != nil {
				return fmt.Errorf("%s: bad regex %q: %w", group.label, p, err)
			}
		}
	}
	for _, r := range c.Paths.Rewrites {
		if _, err := regexp.Compile(r.Pattern); err != nil {
			return fmt.Errorf("paths.rewrites: bad regex %q: %w", r.Pattern, err)
		}
	}
	for _, r := range c.Types.Replacements {
		if _, err := regexp.Compile(r.Pattern); err != nil {
			return fmt.Errorf("types.replacements: bad regex %q: %w", r.Pattern, err)
		}
	}
	for _, s := range append(append([]Substitution{}, c.Webpage.EarlySubstitutions...), c.Webpage.LateSubstitutions...) {
		if !s.Literal {
			if _, err := regexp.Compile(s.Pattern); err != nil {
				return fmt.Errorf("webpage substitutions: bad regex %q: %w", s.Pattern, err)
			}
		}
	}
	if c.Request.Proxy != "" {
		if _, err := url.Parse(c.Request.Proxy); err != nil {
			return fmt.Errorf("request.proxy: %w", err)
		}
	}
	return nil
}

// StartHosts returns the hostnames of the start URLs, de-duplicated.
func (c *Config) StartHosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range c.StartURLs {
		u, err := url.Parse(s)
		if err != nil {
			continue
		}
		h := strings.ToLower(u.Hostname())
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// WorkDir is where state, logs and originals live, inside the destination so a
// download folder is fully self-describing and portable.
func (c *Config) WorkDir() string { return WorkDirIn(c.Destination) }

// WorkDirName is the folder holding a download's journal, config and report.
const WorkDirName = ".webvacuum"

// WorkDirIn returns the state folder for a destination.
func WorkDirIn(dest string) string { return filepath.Join(dest, WorkDirName) }

// IsDownloadFolder reports whether a directory holds a download.
func IsDownloadFolder(dest string) bool {
	fi, err := os.Stat(WorkDirIn(dest))
	return err == nil && fi.IsDir()
}

// ---------------------------------------------------------------- io

// Load reads a YAML config, applying it on top of Default so partial files work.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Merge decodes a YAML file on top of an existing config, leaving any field the
// file does not mention untouched. This is what layers a machine-wide default
// under a project file under the command line.
func Merge(c *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && err.Error() != "EOF" {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// DefaultPaths lists the configuration files loaded automatically, lowest
// precedence first. Missing files are skipped.
//
// A file beside the binary makes a portable install self-contained: drop the
// binary and its settings onto a server together. The user-level file is the
// usual place for personal defaults, and the working-directory file lets one
// folder carry its own.
func DefaultPaths() []string {
	var out []string

	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		out = append(out, filepath.Join(filepath.Dir(exe), "webvacuum.yaml"))
	}
	if dir, err := os.UserConfigDir(); err == nil {
		out = append(out, filepath.Join(dir, "webvacuum", "config.yaml"))
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(wd, "webvacuum.yaml"))
	}

	// The same path can appear twice when the binary sits in the working
	// directory; loading it twice would be confusing rather than harmful.
	seen := map[string]bool{}
	uniq := out[:0]
	for _, p := range out {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		uniq = append(uniq, p)
	}
	return uniq
}

// LoadLayered applies every discovered default file, then an explicit one if
// given. It returns the config and the paths actually read, so the caller can
// show the user where the settings came from.
func LoadLayered(explicit string, skipDefaults bool) (*Config, []string, error) {
	c := Default()
	var used []string

	if !skipDefaults {
		for _, p := range DefaultPaths() {
			if err := Merge(c, p); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, used, err
			}
			used = append(used, p)
		}
	}
	if explicit != "" {
		if err := Merge(c, explicit); err != nil {
			return nil, used, err
		}
		used = append(used, explicit)
	}
	return c, used, nil
}

// Parse decodes YAML on top of the defaults.
func Parse(data []byte) (*Config, error) {
	c := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && err.Error() != "EOF" {
		return nil, err
	}
	return c, nil
}

// Save writes the config as YAML, creating parent directories as needed.
func (c *Config) Save(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Clone returns a deep copy, used when the UI edits a running job's settings.
func (c *Config) Clone() *Config {
	data, err := yaml.Marshal(c)
	if err != nil {
		cp := *c
		return &cp
	}
	out := Default()
	if err := yaml.Unmarshal(data, out); err != nil {
		cp := *c
		return &cp
	}
	return out
}

// defaultScheme picks the scheme for an address typed without one. HTTPS is
// right for anything on the public web, but a loopback address or an explicit
// non-standard port is almost always a local server speaking plain HTTP, and
// guessing wrong there produces a confusing TLS error instead of a download.
func defaultScheme(host string) string {
	h := strings.ToLower(host)
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	name, port, hasPort := strings.Cut(h, ":")
	if name == "localhost" || name == "127.0.0.1" || name == "::1" || name == "0.0.0.0" ||
		strings.HasSuffix(name, ".local") || strings.HasSuffix(name, ".localhost") {
		return "http"
	}
	if hasPort {
		switch port {
		case "443", "8443", "4443":
			return "https"
		case "":
		default:
			return "http"
		}
	}
	return "https"
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeName(s string) string {
	s = unsafeName.ReplaceAllString(s, "_")
	s = strings.Trim(s, "._-")
	if s == "" {
		s = "site"
	}
	return s
}
