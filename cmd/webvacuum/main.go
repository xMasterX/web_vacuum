// Command webvacuum mirrors websites for offline browsing.
//
// It downloads pages together with the stylesheets, scripts, images and fonts
// they need, then rewrites every link so the copy browses locally exactly like
// the original. A dropped connection pauses the job and resumes it on its own,
// and stopping at any point leaves a journal that a later run picks up.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/xMasterX/web_vacuum/internal/config"
)

const version = "1.0.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "webvacuum: %v\n", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

type command struct {
	name    string
	summary string
	run     func(args []string) error
}

func commands() []command {
	return []command{
		{"dump", "download a site (the default when you pass a URL)", cmdDump},
		{"resume", "continue an interrupted download in a folder", cmdResume},
		{"serve", "run the web interface and set up jobs from a browser", cmdServe},
		{"localize", "rewrite links in an existing download without fetching anything", cmdLocalize},
		{"status", "show what happened in a download folder", cmdStatus},
		{"check", "find links in a download that will not work offline", cmdCheck},
		{"retry", "re-try just the failed URLs in a folder", cmdRetry},
		{"config", "print a commented configuration file you can edit", cmdConfig},
		{"version", "print the version", cmdVersion},
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return errUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return nil
	case "-v", "--version":
		return cmdVersion(nil)
	}

	for _, c := range commands() {
		if c.name == args[0] {
			return c.run(args[1:])
		}
	}
	// Anything that is not a known subcommand is treated as a URL, so the
	// shortest useful invocation is "webvacuum example.com".
	return cmdDump(args)
}

func usage(w *os.File) {
	fmt.Fprintf(w, `Web Vacuum %s — mirror a website for offline browsing

USAGE
  webvacuum <url> [options]           download a site
  webvacuum <command> [options]

COMMANDS
`, version)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprint(w, `
GETTING STARTED
  webvacuum example.com
      Mirror the whole site into ./example.com and rewrite links so it
      browses offline. Press F9 to pause, F10 to quit; run it again to resume.

  webvacuum forum.example.com -d images.example-cdn.com
      Stay on the forum, but also follow images hosted on a second domain.
      Repeat -d for as many domains as you need.

  webvacuum example.com --exclude '/(login|logout|cart)' --levels 4
      Skip URLs matching a pattern and stop four links deep.

  webvacuum example.com --headless
      No terminal UI: start a small web interface, print its address, and
      keep downloading after you close the terminal.

  webvacuum resume ./example.com
      Continue where an interrupted run left off.

Run 'webvacuum <command> -h' for the full option list.
`)
}

func cmdVersion(_ []string) error {
	fmt.Printf("Web Vacuum %s\n", version)
	return nil
}

// configSources records which configuration files were read, so the plan shown
// before a download can say where its settings came from.
var configSources []string

// buildConfig assembles a Config from the discovered and explicit config files,
// the command line, and the positional URLs.
func buildConfig(o *options, urls []string) (*config.Config, error) {
	cfg, used, err := config.LoadLayered(o.configFile, o.noGlobalConf)
	if err != nil {
		return nil, err
	}
	configSources = used
	if len(urls) > 0 {
		cfg.StartURLs = urls
	}
	if err := o.applyTo(cfg); err != nil {
		return nil, err
	}
	if len(cfg.StartURLs) == 0 {
		return nil, fmt.Errorf("no URL given\n\ntry: webvacuum example.com")
	}
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// newFlagSet builds a FlagSet whose error output points at the right command.
func newFlagSet(name string) (*flagSet, *options) {
	fs := newFlags(name)
	o := &options{}
	o.register(fs.FlagSet)
	return fs, o
}

// signalContext cancels on Ctrl-C and on SIGTERM, so a job stopped by a
// service manager still checkpoints its journal.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
		// A second signal means the user is impatient; leave immediately.
		<-ch
		os.Exit(130)
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel()
	}
}

// describePlan renders the settings that most often surprise people, so a
// mistake is visible before a long download rather than after it.
func describePlan(cfg *config.Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  from     %s\n", strings.Join(cfg.StartURLs, "\n           "))
	fmt.Fprintf(&b, "  into     %s\n", cfg.Destination)

	scope := string(cfg.Scope.Constraint)
	switch cfg.Scope.Constraint {
	case config.ConstraintHost:
		scope = "this host only (" + strings.Join(cfg.StartHosts(), ", ") + ")"
	case config.ConstraintSubdomains:
		scope = "this domain and its subdomains"
	case config.ConstraintDirectory:
		scope = "this directory and below"
	case config.ConstraintHostPlus1:
		scope = "this host, plus one hop off-site"
	case config.ConstraintNone:
		scope = "anywhere (no limit — this can be very large)"
	}
	fmt.Fprintf(&b, "  scope    %s\n", scope)
	if len(cfg.Scope.Hosts) > 0 {
		fmt.Fprintf(&b, "  also     %s\n", strings.Join(cfg.Scope.Hosts, ", "))
	}
	assets := map[config.SupportingMode]string{
		config.SupportingNone:    "only from the hosts above",
		config.SupportingRelated: "from the hosts above and their own subdomains",
		config.SupportingAny:     "from anywhere (every third-party host a page links to)",
	}[cfg.General.SupportingFiles]
	if len(cfg.Scope.AssetHosts) > 0 {
		assets += ", plus " + strings.Join(cfg.Scope.AssetHosts, ", ")
	}
	fmt.Fprintf(&b, "  assets   %s\n", assets)
	if len(cfg.Scope.Exclude) > 0 || len(cfg.Scope.ExcludeGlob) > 0 {
		fmt.Fprintf(&b, "  skip     %s\n", strings.Join(append(append([]string{}, cfg.Scope.Exclude...), cfg.Scope.ExcludeGlob...), ", "))
	}

	limits := []string{}
	if cfg.Limits.MaxLevels > 0 {
		limits = append(limits, fmt.Sprintf("%d levels", cfg.Limits.MaxLevels))
	}
	if cfg.Limits.MaxFiles > 0 {
		limits = append(limits, fmt.Sprintf("%d files", cfg.Limits.MaxFiles))
	}
	if cfg.Limits.MaxBytes > 0 {
		limits = append(limits, cfg.Limits.MaxBytes.String()+" total")
	}
	if cfg.Limits.MaxDuration > 0 {
		limits = append(limits, cfg.Limits.MaxDuration.String())
	}
	if len(limits) == 0 {
		limits = append(limits, "none — the whole site")
	}
	fmt.Fprintf(&b, "  limits   %s\n", strings.Join(limits, ", "))
	fmt.Fprintf(&b, "  network  %d connections, %d per host, %s delay, %d attempts per URL\n",
		cfg.General.Connections, cfg.Request.PerHostConns, cfg.Request.Delay, cfg.Request.Attempts)

	robots := "honoured"
	if cfg.General.IgnoreRobots {
		robots = "IGNORED"
	}
	localize := "links rewritten for offline browsing"
	if cfg.General.FileModification == config.ModifyNone {
		localize = "links left untouched"
	}
	fmt.Fprintf(&b, "  also     robots.txt %s, %s\n", robots, localize)
	if cfg.Render.Enabled() {
		what := "pages that look JavaScript-built"
		if cfg.Render.Mode == config.RenderAlways {
			what = "every page"
		}
		if len(cfg.Render.Match) > 0 {
			what = "pages matching " + strings.Join(cfg.Render.Match, ", ")
		}
		extra := ""
		if cfg.Render.Scroll {
			extra = ", scrolling for lazy-loaded images"
		}
		fmt.Fprintf(&b, "  browser  rendering %s in %d tab(s)%s\n", what, cfg.Render.Tabs, extra)
	}
	if len(configSources) > 0 {
		fmt.Fprintf(&b, "  config   %s\n", strings.Join(configSources, "\n           "))
	}
	return b.String()
}

// listDestination finds a download folder from an argument, defaulting to the
// current directory when it already looks like one.
func resolveDestination(arg string) (string, error) {
	if arg == "" {
		arg = "."
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", err
	}
	if config.IsDownloadFolder(abs) {
		return abs, nil
	}
	// Be forgiving about being handed the state folder itself.
	if filepath.Base(abs) == config.WorkDirName {
		return filepath.Dir(abs), nil
	}
	return "", fmt.Errorf("%s is not a Web Vacuum download folder (no %s inside)", abs, config.WorkDirName)
}

// loadJobConfig reads the config saved inside a download folder.
func loadJobConfig(dest string) (*config.Config, error) {
	path := filepath.Join(config.WorkDirIn(dest), "config.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	cfg.Destination = dest
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
