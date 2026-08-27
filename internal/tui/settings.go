package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/xMasterX/web_vacuum/internal/config"
)

// fieldKind decides how a setting is edited: typed into, toggled, or cycled.
type fieldKind int

const (
	fieldText fieldKind = iota
	fieldInt
	fieldDuration
	fieldSize
	fieldBool
	fieldEnum
	fieldList
	fieldReadOnly
)

// settingField describes one editable setting.
//
// Settings are described rather than hand-rendered so the list, the editor and
// the validation all stay in step: adding a knob means adding one entry, not
// touching three places that can drift apart.
type settingField struct {
	label string
	help  string
	kind  fieldKind
	enum  []string
	get   func(*config.Config) string
	set   func(*config.Config, string) error
}

func (f settingField) editable() bool { return f.kind != fieldReadOnly }

// ---------------------------------------------------------------- helpers

func setInt(dst *int, v string, min, max int) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("expected a whole number")
	}
	if n < min || (max > 0 && n > max) {
		return fmt.Errorf("must be between %d and %d", min, max)
	}
	*dst = n
	return nil
}

func setInt64(dst *int64, v string) error {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return fmt.Errorf("expected a whole number, 0 for no limit")
	}
	*dst = n
	return nil
}

func setDuration(dst *config.Duration, v string) error {
	v = strings.TrimSpace(v)
	if v == "" || v == "0" {
		*dst = 0
		return nil
	}
	d, err := config.ParseDuration(v)
	if err != nil {
		return err
	}
	*dst = d
	return nil
}

func setSize(dst *config.ByteSize, v string) error {
	v = strings.TrimSpace(v)
	if v == "" || v == "0" || strings.EqualFold(v, "unlimited") {
		*dst = 0
		return nil
	}
	var b config.ByteSize
	if err := b.UnmarshalJSON([]byte(`"` + v + `"`)); err != nil {
		return fmt.Errorf("expected something like 500KB, 10MB or 2GB")
	}
	*dst = b
	return nil
}

func showSize(b config.ByteSize) string {
	if b <= 0 {
		return "unlimited"
	}
	return b.String()
}

func showInt(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

func showInt64(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	return strconv.FormatInt(n, 10)
}

func showBool(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "on", "1", "y":
		return true
	}
	return false
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinList(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return strings.Join(v, ", ")
}

// ---------------------------------------------------------------- fields

// settingFields is the full list shown in the Setup pane, ordered by how often
// someone reaches for them while a download is running.
func settingFields() []settingField {
	return []settingField{
		{
			label: "connections", kind: fieldInt,
			help: "how many downloads run at once",
			get:  func(c *config.Config) string { return strconv.Itoa(c.General.Connections) },
			set:  func(c *config.Config, v string) error { return setInt(&c.General.Connections, v, 1, 256) },
		},
		{
			label: "per host", kind: fieldInt,
			help: "how many at once against a single server",
			get:  func(c *config.Config) string { return strconv.Itoa(c.Request.PerHostConns) },
			set:  func(c *config.Config, v string) error { return setInt(&c.Request.PerHostConns, v, 1, 256) },
		},
		{
			label: "delay", kind: fieldDuration,
			help: "minimum wait between requests to one server",
			get:  func(c *config.Config) string { return c.Request.Delay.String() },
			set:  func(c *config.Config, v string) error { return setDuration(&c.Request.Delay, v) },
		},
		{
			label: "speed limit", kind: fieldSize,
			help: "cap on total download speed per second",
			get:  func(c *config.Config) string { return showSize(c.Limits.MaxRate) },
			set:  func(c *config.Config, v string) error { return setSize(&c.Limits.MaxRate, v) },
		},
		{
			label: "scope", kind: fieldEnum,
			enum: []string{"host", "subdomains", "host+1", "directory", "rules", "none"},
			help: "how far the crawl is allowed to roam",
			get:  func(c *config.Config) string { return string(c.Scope.Constraint) },
			set: func(c *config.Config, v string) error {
				c.Scope.Constraint = config.Constraint(v)
				return nil
			},
		},
		{
			label: "extra domains", kind: fieldList,
			help: "other domains to crawl fully, comma separated",
			get:  func(c *config.Config) string { return joinList(c.Scope.Hosts) },
			set: func(c *config.Config, v string) error {
				c.Scope.Hosts = splitList(v)
				return nil
			},
		},
		{
			label: "asset domains", kind: fieldList,
			help: "domains allowed for files only, never crawled for pages",
			get:  func(c *config.Config) string { return joinList(c.Scope.AssetHosts) },
			set: func(c *config.Config, v string) error {
				c.Scope.AssetHosts = splitList(v)
				return nil
			},
		},
		{
			label: "blocked domains", kind: fieldList,
			help: "never contacted, whatever else allows them",
			get:  func(c *config.Config) string { return joinList(c.Scope.BlockHosts) },
			set: func(c *config.Config, v string) error {
				c.Scope.BlockHosts = splitList(v)
				return nil
			},
		},
		{
			label: "exclude", kind: fieldList,
			help: "regexps; drops matching URLs already queued too. a plain word works",
			get:  func(c *config.Config) string { return joinList(c.Scope.Exclude) },
			set: func(c *config.Config, v string) error {
				c.Scope.Exclude = splitList(v)
				return nil
			},
		},
		{
			label: "include", kind: fieldList,
			help: "regular expressions that override the scope",
			get:  func(c *config.Config) string { return joinList(c.Scope.Include) },
			set: func(c *config.Config, v string) error {
				c.Scope.Include = splitList(v)
				return nil
			},
		},
		{
			label: "drop parameters", kind: fieldList,
			help: "query parameters stripped before queueing, e.g. session ids",
			get:  func(c *config.Config) string { return joinList(c.Scope.DropQueryParams) },
			set: func(c *config.Config, v string) error {
				c.Scope.DropQueryParams = splitList(v)
				return nil
			},
		},
		{
			label: "supporting files", kind: fieldEnum,
			enum: []string{"any", "related", "none"},
			help: "which hosts may serve images, styles and scripts",
			get:  func(c *config.Config) string { return string(c.General.SupportingFiles) },
			set: func(c *config.Config, v string) error {
				c.General.SupportingFiles = config.SupportingMode(v)
				return nil
			},
		},
		{
			label: "max levels", kind: fieldInt,
			help: "how many links deep to follow, 0 for no limit",
			get:  func(c *config.Config) string { return showInt(c.Limits.MaxLevels) },
			set:  func(c *config.Config, v string) error { return setInt(&c.Limits.MaxLevels, v, 0, 0) },
		},
		{
			label: "max files", kind: fieldInt,
			help: "stop after this many files, 0 for no limit",
			get:  func(c *config.Config) string { return showInt64(c.Limits.MaxFiles) },
			set:  func(c *config.Config, v string) error { return setInt64(&c.Limits.MaxFiles, v) },
		},
		{
			label: "max total size", kind: fieldSize,
			help: "stop after downloading this much",
			get:  func(c *config.Config) string { return showSize(c.Limits.MaxBytes) },
			set:  func(c *config.Config, v string) error { return setSize(&c.Limits.MaxBytes, v) },
		},
		{
			label: "max file size", kind: fieldSize,
			help: "skip files larger than this",
			get:  func(c *config.Config) string { return showSize(c.Limits.MaxFileSize) },
			set:  func(c *config.Config, v string) error { return setSize(&c.Limits.MaxFileSize, v) },
		},
		{
			label: "min file size", kind: fieldSize,
			help: "skip files smaller than this, good for dropping thumbnails",
			get:  func(c *config.Config) string { return showSize(c.Limits.MinFileSize) },
			set:  func(c *config.Config, v string) error { return setSize(&c.Limits.MinFileSize, v) },
		},
		{
			label: "user agent", kind: fieldText,
			help: "a preset name (" + strings.Join(config.UserAgentNames(), ", ") + ") or a full string",
			get:  func(c *config.Config) string { return c.Request.UserAgent },
			set: func(c *config.Config, v string) error {
				v = strings.TrimSpace(v)
				if v == "" {
					return fmt.Errorf("cannot be empty; use 'none' to send no user agent")
				}
				c.Request.UserAgent = v
				return nil
			},
		},
		{
			label: "attempts", kind: fieldInt,
			help: "tries per URL before giving up on it",
			get:  func(c *config.Config) string { return strconv.Itoa(c.Request.Attempts) },
			set:  func(c *config.Config, v string) error { return setInt(&c.Request.Attempts, v, 1, 1000) },
		},
		{
			label: "timeout", kind: fieldDuration,
			help: "how long to wait for a server to respond",
			get:  func(c *config.Config) string { return c.Request.Timeout.String() },
			set:  func(c *config.Config, v string) error { return setDuration(&c.Request.Timeout, v) },
		},
		{
			label: "ignore robots.txt", kind: fieldBool,
			help: "download paths the site asks crawlers to skip",
			get:  func(c *config.Config) string { return showBool(c.General.IgnoreRobots) },
			set: func(c *config.Config, v string) error {
				c.General.IgnoreRobots = parseBool(v)
				return nil
			},
		},
		{
			label: "ignore nofollow", kind: fieldBool,
			help: "follow links marked rel=nofollow",
			get:  func(c *config.Config) string { return showBool(c.General.IgnoreNofollow) },
			set: func(c *config.Config, v string) error {
				c.General.IgnoreNofollow = parseBool(v)
				return nil
			},
		},
		{
			label: "replace files", kind: fieldEnum,
			enum: []string{"newer", "never", "always"},
			help: "what to do about files already downloaded",
			get:  func(c *config.Config) string { return string(c.General.FileReplacement) },
			set: func(c *config.Config, v string) error {
				c.General.FileReplacement = config.FileReplacement(v)
				return nil
			},
		},
		{
			label: "render javascript", kind: fieldEnum,
			enum: []string{"never", "auto", "always"},
			help: "use a headless browser for pages built by script",
			get:  func(c *config.Config) string { return string(c.Render.Mode) },
			set: func(c *config.Config, v string) error {
				c.Render.Mode = config.RenderMode(v)
				return nil
			},
		},
		{
			label: "render scrolling", kind: fieldBool,
			help: "scroll rendered pages to trigger lazy-loaded images",
			get:  func(c *config.Config) string { return showBool(c.Render.Scroll) },
			set: func(c *config.Config, v string) error {
				c.Render.Scroll = parseBool(v)
				return nil
			},
		},
		{
			label: "log level", kind: fieldEnum,
			enum: []string{"info", "debug", "warn", "error"},
			help: "how much detail goes into the log",
			get:  func(c *config.Config) string { return c.Log.Level },
			set: func(c *config.Config, v string) error {
				c.Log.Level = v
				return nil
			},
		},

		// Below here are the settings a running job cannot change; they are
		// shown because they are what someone checks first when something looks
		// wrong, and hiding them would just send them to the config file.
		{
			label: "start urls", kind: fieldReadOnly,
			help: "fixed for this job",
			get:  func(c *config.Config) string { return joinList(c.StartURLs) },
		},
		{
			label: "destination", kind: fieldReadOnly,
			help: "fixed for this job",
			get:  func(c *config.Config) string { return c.Destination },
		},
		{
			label: "user agent sent", kind: fieldReadOnly,
			help: "the full header value the preset above resolves to",
			get:  func(c *config.Config) string { return config.ResolveUserAgent(c.Request.UserAgent) },
		},
		{
			label: "settings file", kind: fieldReadOnly,
			help: "edit this file and the change is picked up within seconds",
			get:  func(c *config.Config) string { return c.WorkDir() + "/config.yaml" },
		},
	}
}

// cycleEnum returns the next value in a list, wrapping around.
func cycleEnum(values []string, current string, step int) string {
	if len(values) == 0 {
		return current
	}
	idx := 0
	for i, v := range values {
		if v == current {
			idx = i
			break
		}
	}
	idx = (idx + step + len(values)) % len(values)
	return values[idx]
}
