package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
)

// stringList collects a repeatable flag, so --domain can be given many times
// instead of forcing the user to build a comma-separated string.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// optBool distinguishes "not given" from "given as false", which matters when
// flags are layered on top of a config file.
type optBool struct {
	set bool
	val bool
}

func (b *optBool) String() string {
	if !b.set {
		return ""
	}
	return fmt.Sprint(b.val)
}

func (b *optBool) Set(v string) error {
	switch strings.ToLower(v) {
	case "1", "true", "t", "yes", "y", "on", "":
		b.val = true
	case "0", "false", "f", "no", "n", "off":
		b.val = false
	default:
		return fmt.Errorf("invalid boolean %q", v)
	}
	b.set = true
	return nil
}

func (b *optBool) IsBoolFlag() bool { return true }

func (b *optBool) apply(dst *bool) {
	if b.set {
		*dst = b.val
	}
}

// optString / optInt / optDuration / optSize behave the same way for their types.
type optString struct {
	set bool
	val string
}

func (o *optString) String() string { return o.val }
func (o *optString) Set(v string) error {
	o.val, o.set = v, true
	return nil
}
func (o *optString) apply(dst *string) {
	if o.set {
		*dst = o.val
	}
}

type optInt struct {
	set bool
	val int64
}

func (o *optInt) String() string {
	if !o.set {
		return ""
	}
	return fmt.Sprint(o.val)
}
func (o *optInt) Set(v string) error {
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err != nil {
		return fmt.Errorf("invalid number %q", v)
	}
	o.val, o.set = n, true
	return nil
}
func (o *optInt) applyInt(dst *int) {
	if o.set {
		*dst = int(o.val)
	}
}
func (o *optInt) applyInt64(dst *int64) {
	if o.set {
		*dst = o.val
	}
}

type optDuration struct {
	set bool
	val time.Duration
}

func (o *optDuration) String() string {
	if !o.set {
		return ""
	}
	return o.val.String()
}
func (o *optDuration) Set(v string) error {
	d, err := config.ParseDuration(v)
	if err != nil {
		return err
	}
	o.val, o.set = d.D(), true
	return nil
}
func (o *optDuration) apply(dst *config.Duration) {
	if o.set {
		*dst = config.Duration(o.val)
	}
}

type optSize struct {
	set bool
	val config.ByteSize
}

func (o *optSize) String() string {
	if !o.set {
		return ""
	}
	return o.val.String()
}

func (o *optSize) Set(v string) error {
	var b config.ByteSize
	if err := b.UnmarshalJSON([]byte(`"` + strings.TrimSpace(v) + `"`)); err != nil {
		return err
	}
	o.val, o.set = b, true
	return nil
}

func (o *optSize) apply(dst *config.ByteSize) {
	if o.set {
		*dst = o.val
	}
}

// options is every command-line switch, kept in one struct so the same set can
// be registered by more than one subcommand.
type options struct {
	configFile   string
	saveConfig   string
	noGlobalConf bool
	freshConfig  bool

	out  optString
	name optString

	scope       optString
	domains     stringList
	assetHosts  stringList
	blockHosts  stringList
	include     stringList
	exclude     stringList
	includeGlob stringList
	excludeGlob stringList

	levels      optInt
	maxFiles    optInt
	maxSize     optSize
	maxFileSize optSize
	minFileSize optSize
	maxPerHost  optInt
	maxTime     optDuration
	rate        optSize

	connections optInt
	perHost     optInt
	delay       optDuration
	jitter      optDuration
	attempts    optInt
	timeout     optDuration
	userAgent   optString
	header      stringList
	cookies     optString
	cookieJar   optString
	auth        optString
	proxy       optString
	insecure    optBool

	ignoreRobots   optBool
	ignoreNofollow optBool
	noSupporting   optBool
	supporting     optString
	errorPages     optBool
	replace        optString
	noLocalize     optBool
	keepOriginals  optBool
	stripScripts   optBool
	banner         optBool

	typeMode   optString
	categories stringList
	allowExt   stringList
	blockExt   stringList

	sitemap      optBool
	scanScripts  optBool
	scanComments optBool
	followForms  optBool
	ignoreQuery  optBool
	dropParams   stringList
	noResume     optBool

	hostFolders optBool
	flatten     optBool

	render        optBool
	renderAll     optBool
	renderMode    optString
	renderMatch   stringList
	renderSkip    stringList
	renderTabs    optInt
	renderWait    optDuration
	renderIdle    optDuration
	renderTimeout optDuration
	renderScroll  optBool
	renderBlock   stringList
	noFollowReqs  optBool
	chromePath    optString
	chromeURL     optString
	renderHeadful optBool
	noSandbox     optBool
	keepScripts   optBool

	logFile  optString
	logLevel optString

	headless bool
	addr     string
	token    string
	open     bool
	noTUI    bool
	quiet    bool
	yes      bool
}

// register wires every flag onto a FlagSet.
func (o *options) register(fs *flag.FlagSet) {
	fs.StringVar(&o.configFile, "config", "", "load settings from a YAML file")
	fs.StringVar(&o.saveConfig, "save-config", "", "write the resolved settings to a YAML file and exit")
	fs.BoolVar(&o.noGlobalConf, "no-global-config", false, "ignore the automatically discovered configuration files")
	fs.BoolVar(&o.freshConfig, "fresh-settings", false, "ignore the settings saved in the download folder and start from the defaults")

	pair := func(v flag.Value, usage string, names ...string) {
		for _, n := range names {
			fs.Var(v, n, usage)
		}
	}

	pair(&o.out, "destination folder (default: a folder named after the site)", "o", "out", "output")
	pair(&o.name, "job name, used for the default folder and reports", "name")

	pair(&o.scope, "how far to roam: host, host+1, subdomains, directory, rules, none", "scope")
	pair(&o.domains, "also download pages from this domain (repeatable; \".example.com\" matches subdomains)", "d", "domain", "domains")
	pair(&o.assetHosts, "allow images and other files from this domain, but do not crawl its pages", "asset-domain")
	pair(&o.blockHosts, "never touch this domain (wins over everything else)", "block-domain")
	pair(&o.include, "download URLs matching this regex regardless of scope (repeatable)", "include")
	pair(&o.exclude, "never download URLs matching this regex (repeatable, highest priority)", "exclude")
	pair(&o.includeGlob, "like --include but a shell glob, e.g. '*/docs/*'", "include-glob")
	pair(&o.excludeGlob, "like --exclude but a shell glob, e.g. '*?replytocom=*'", "exclude-glob")

	pair(&o.levels, "maximum link depth, 0 for unlimited", "l", "levels", "depth")
	pair(&o.maxFiles, "stop after this many files", "max-files")
	pair(&o.maxSize, "stop after downloading this much in total, e.g. 5GB", "max-size")
	pair(&o.maxFileSize, "skip files larger than this, e.g. 100MB", "max-file-size")
	pair(&o.minFileSize, "skip files smaller than this, e.g. 2KB (good for dropping thumbnails)", "min-file-size")
	pair(&o.maxPerHost, "stop downloading from a host after this many files", "max-files-per-host")
	pair(&o.maxTime, "stop after this much time, e.g. 2h", "max-time")
	pair(&o.rate, "limit total download speed, e.g. 2MB", "rate", "limit-rate")

	pair(&o.connections, "number of simultaneous downloads", "c", "connections")
	pair(&o.perHost, "simultaneous downloads per host", "per-host")
	pair(&o.delay, "minimum wait between requests to one host, e.g. 500ms", "delay")
	pair(&o.jitter, "random extra delay added to --delay", "delay-jitter")
	pair(&o.attempts, "attempts per URL before giving up on it", "attempts", "retries")
	pair(&o.timeout, "per-request timeout", "timeout")
	pair(&o.userAgent, "user agent: a preset name or a full string ("+strings.Join(config.UserAgentNames(), ", ")+")", "u", "user-agent")
	pair(&o.header, "extra request header, 'Name: value' (repeatable)", "H", "header")
	pair(&o.cookies, "cookie header contents, 'a=1; b=2'", "cookie")
	pair(&o.cookieJar, "path to a Netscape cookies.txt exported from your browser", "cookies", "cookie-jar")
	pair(&o.auth, "HTTP basic credentials as user:password", "auth")
	pair(&o.proxy, "proxy URL, e.g. socks5://127.0.0.1:9050", "proxy")
	pair(&o.insecure, "do not verify TLS certificates", "insecure")

	pair(&o.ignoreRobots, "ignore robots.txt and robots meta tags", "ignore-robots")
	pair(&o.ignoreNofollow, "follow links marked rel=nofollow", "ignore-nofollow")
	pair(&o.supporting, "which hosts may serve images/CSS/scripts: none, related, any", "supporting-files")
	pair(&o.noSupporting, "shorthand for --supporting-files=none", "no-supporting-files")
	pair(&o.errorPages, "save the body of error responses too", "error-pages")
	pair(&o.replace, "what to do about files already on disk: never, always, newer", "replace")
	pair(&o.noLocalize, "leave links untouched instead of rewriting them for offline use", "no-localize")
	pair(&o.keepOriginals, "keep an unmodified copy of every page before links are rewritten", "keep-originals")
	pair(&o.stripScripts, "remove <script> tags from saved pages", "strip-scripts")
	pair(&o.banner, "add a small archive banner to saved pages", "banner")

	pair(&o.typeMode, "media type filter: all, allow, disallow", "types")
	pair(&o.categories, "categories the filter applies to ("+strings.Join(config.CategoryNames(), ", ")+")", "category", "categories")
	pair(&o.allowExt, "only download files with these extensions", "allow-ext")
	pair(&o.blockExt, "never download files with these extensions", "block-ext")

	pair(&o.sitemap, "also seed the crawl from sitemap.xml", "sitemap")
	pair(&o.scanScripts, "search JavaScript for URLs using --url-pattern", "scan-scripts")
	pair(&o.scanComments, "search HTML comments for links", "scan-comments")
	pair(&o.followForms, "follow form action URLs", "follow-forms")
	pair(&o.ignoreQuery, "treat URLs that differ only by query string as the same page", "ignore-query")
	pair(&o.dropParams, "strip these query parameters before queueing (repeatable)", "drop-param")
	pair(&o.noResume, "start fresh instead of resuming an interrupted job in the destination", "no-resume")

	pair(&o.render, "run pages through a headless browser when they look JavaScript-built", "render", "js")
	pair(&o.renderAll, "run every page through a headless browser", "render-all")
	pair(&o.renderMode, "when to use a browser: never, auto, always", "render-mode")
	pair(&o.renderMatch, "only render URLs matching this regex (repeatable)", "render-match")
	pair(&o.renderSkip, "never render URLs matching this regex (repeatable)", "render-skip")
	pair(&o.renderTabs, "how many pages to render at once", "render-tabs")
	pair(&o.renderWait, "extra pause after a rendered page settles, e.g. 1s", "render-wait")
	pair(&o.renderIdle, "how long the network must be quiet before a page counts as loaded", "render-idle")
	pair(&o.renderTimeout, "give up rendering a page after this long", "render-timeout")
	pair(&o.renderScroll, "scroll each rendered page to the bottom to trigger lazy-loaded images", "render-scroll")
	pair(&o.renderBlock, "URL wildcard the browser refuses to load, e.g. '*.mp4' (repeatable)", "render-block")
	pair(&o.noFollowReqs, "do not download the extra files a rendered page requests", "no-follow-requests")
	pair(&o.chromePath, "path to the Chrome or Chromium binary", "chrome-path")
	pair(&o.chromeURL, "attach to a browser already running with remote debugging, e.g. http://127.0.0.1:9222", "chrome-url")
	pair(&o.renderHeadful, "show the browser window instead of running it hidden", "render-headful")
	pair(&o.noSandbox, "pass --no-sandbox to the browser (needed when running as root)", "no-sandbox")
	pair(&o.keepScripts, "keep scripts in rendered pages (they normally re-run offline and blank the page)", "render-keep-scripts")

	pair(&o.hostFolders, "give every host its own top-level folder", "host-folders")
	pair(&o.flatten, "put the main site's files straight in the destination folder (default true)", "flatten")

	pair(&o.logFile, "also write the log to this file", "log")
	pair(&o.logLevel, "log level: debug, info, warn, error", "log-level")

	fs.BoolVar(&o.headless, "headless", false, "run without a terminal UI and serve a web interface instead")
	fs.StringVar(&o.addr, "addr", "", "address for the web interface (default 127.0.0.1 on a random port)")
	fs.StringVar(&o.token, "token", "", "require this token to use the web interface (default: generated)")
	fs.BoolVar(&o.open, "open", false, "open the web interface in a browser")
	fs.BoolVar(&o.noTUI, "no-tui", false, "print plain progress lines instead of the full-screen interface")
	fs.BoolVar(&o.quiet, "quiet", false, "only print errors and the final summary")
	fs.BoolVar(&o.yes, "y", false, "do not ask for confirmation")
}

// applyTo layers the command line on top of a config.
func (o *options) applyTo(c *config.Config) error {
	o.out.apply(&c.Destination)
	o.name.apply(&c.Name)

	if o.scope.set {
		c.Scope.Constraint = config.Constraint(strings.ToLower(o.scope.val))
	}
	c.Scope.Hosts = append(c.Scope.Hosts, o.domains...)
	c.Scope.AssetHosts = append(c.Scope.AssetHosts, o.assetHosts...)
	c.Scope.BlockHosts = append(c.Scope.BlockHosts, o.blockHosts...)
	c.Scope.Include = append(c.Scope.Include, o.include...)
	c.Scope.Exclude = append(c.Scope.Exclude, o.exclude...)
	c.Scope.IncludeGlob = append(c.Scope.IncludeGlob, o.includeGlob...)
	c.Scope.ExcludeGlob = append(c.Scope.ExcludeGlob, o.excludeGlob...)
	o.ignoreQuery.apply(&c.Scope.IgnoreQuery)
	c.Scope.DropQueryParams = append(c.Scope.DropQueryParams, o.dropParams...)
	o.scanComments.apply(&c.Scope.ScanComments)

	o.levels.applyInt(&c.Limits.MaxLevels)
	o.maxFiles.applyInt64(&c.Limits.MaxFiles)
	o.maxSize.apply(&c.Limits.MaxBytes)
	o.maxFileSize.apply(&c.Limits.MaxFileSize)
	o.minFileSize.apply(&c.Limits.MinFileSize)
	o.maxPerHost.applyInt64(&c.Limits.MaxPerHost)
	o.maxTime.apply(&c.Limits.MaxDuration)
	o.rate.apply(&c.Limits.MaxRate)

	o.connections.applyInt(&c.General.Connections)
	o.perHost.applyInt(&c.Request.PerHostConns)
	o.delay.apply(&c.Request.Delay)
	o.jitter.apply(&c.Request.DelayJitter)
	o.attempts.applyInt(&c.Request.Attempts)
	o.timeout.apply(&c.Request.Timeout)
	o.userAgent.apply(&c.Request.UserAgent)
	o.cookies.apply(&c.Request.CookiesRaw)
	o.cookieJar.apply(&c.Request.CookieJarFile)
	o.proxy.apply(&c.Request.Proxy)
	o.insecure.apply(&c.Request.InsecureTLS)

	for _, h := range o.header {
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			return fmt.Errorf("--header %q should look like 'Name: value'", h)
		}
		c.Request.Headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	if o.auth.set {
		user, pass, ok := strings.Cut(o.auth.val, ":")
		if !ok {
			return fmt.Errorf("--auth should look like user:password")
		}
		c.Request.Auth = append(c.Request.Auth, config.BasicAuth{Username: user, Password: pass})
	}

	o.ignoreRobots.apply(&c.General.IgnoreRobots)
	o.ignoreNofollow.apply(&c.General.IgnoreNofollow)
	if o.supporting.set {
		c.General.SupportingFiles = config.SupportingMode(strings.ToLower(o.supporting.val))
	}
	if o.noSupporting.set && o.noSupporting.val {
		c.General.SupportingFiles = config.SupportingNone
	}
	o.errorPages.apply(&c.General.DownloadErrorPages)
	o.keepOriginals.apply(&c.General.KeepOriginals)
	if o.replace.set {
		c.General.FileReplacement = config.FileReplacement(strings.ToLower(o.replace.val))
	}
	if o.noLocalize.set && o.noLocalize.val {
		c.General.FileModification = config.ModifyNone
	}
	o.stripScripts.apply(&c.Webpage.StripScripts)
	o.banner.apply(&c.Webpage.InjectBanner)
	o.sitemap.apply(&c.Webpage.UseSitemap)
	o.scanScripts.apply(&c.Webpage.ScanScripts)
	o.followForms.apply(&c.Webpage.FollowForms)

	if o.typeMode.set {
		c.Types.Mode = config.TypeFilterMode(strings.ToLower(o.typeMode.val))
	}
	c.Types.Categories = append(c.Types.Categories, o.categories...)
	c.Types.AllowExtensions = append(c.Types.AllowExtensions, o.allowExt...)
	c.Types.BlockExtensions = append(c.Types.BlockExtensions, o.blockExt...)

	if o.noResume.set && o.noResume.val {
		c.Resilience.ResumeOnStart = false
	}
	if o.renderMode.set {
		c.Render.Mode = config.RenderMode(strings.ToLower(o.renderMode.val))
	}
	if o.render.set && o.render.val {
		c.Render.Mode = config.RenderAuto
	}
	if o.renderAll.set && o.renderAll.val {
		c.Render.Mode = config.RenderAlways
	}
	if (o.render.set && !o.render.val) || (o.renderAll.set && !o.renderAll.val) {
		c.Render.Mode = config.RenderNever
	}
	c.Render.Match = append(c.Render.Match, o.renderMatch...)
	c.Render.Skip = append(c.Render.Skip, o.renderSkip...)
	c.Render.Block = append(c.Render.Block, o.renderBlock...)
	o.renderTabs.applyInt(&c.Render.Tabs)
	o.renderWait.apply(&c.Render.Wait)
	o.renderIdle.apply(&c.Render.Idle)
	o.renderTimeout.apply(&c.Render.Timeout)
	o.renderScroll.apply(&c.Render.Scroll)
	o.chromePath.apply(&c.Render.ChromePath)
	o.chromeURL.apply(&c.Render.RemoteURL)
	o.renderHeadful.apply(&c.Render.Headful)
	o.noSandbox.apply(&c.Render.NoSandbox)
	o.keepScripts.apply(&c.Render.KeepScripts)
	if o.noFollowReqs.set && o.noFollowReqs.val {
		c.Render.FollowRequests = false
	}

	o.hostFolders.apply(&c.Paths.HostFolders)
	// --host-folders is the plain-language way to ask for a folder per domain,
	// which only means anything if the main site stops being flattened away.
	if o.hostFolders.set && o.hostFolders.val {
		c.Paths.FlattenSingleHost = false
	}
	o.flatten.apply(&c.Paths.FlattenSingleHost)

	o.logFile.apply(&c.Log.File)
	o.logLevel.apply(&c.Log.Level)
	return nil
}
