package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/crawl"
	"github.com/xMasterX/web_vacuum/internal/state"
	"github.com/xMasterX/web_vacuum/internal/tui"
	"github.com/xMasterX/web_vacuum/internal/webui"
)

// cmdDump is the main command: crawl a site.
func cmdDump(args []string) error {
	fs, o := newFlagSet("dump")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	cfg, err := buildConfig(o, fs.Args())
	if err != nil {
		return err
	}
	if cfg, err = adoptSavedSettings(cfg, o); err != nil {
		return err
	}

	if o.saveConfig != "" {
		if err := cfg.Save(o.saveConfig); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", o.saveConfig)
		return nil
	}

	// A scope of "none" will happily try to download the internet, so it is the
	// one setting worth confirming before a long unattended run.
	if cfg.Scope.Constraint == config.ConstraintNone && !o.yes && isTerminal() {
		fmt.Print(describePlan(cfg))
		fmt.Print("\nThis will follow links to any site it finds. Continue? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if !strings.HasPrefix(strings.ToLower(answer), "y") {
			return fmt.Errorf("cancelled")
		}
	}

	return runJob(cfg, o)
}

// adoptSavedSettings makes a repeat run pick up where the last one left off in
// settings as well as in progress.
//
// Running the same command again is the documented way to resume, and a job
// that has been running for hours has usually had its settings adjusted from
// the Setup pane by then — the connection count turned down, a pattern added to
// the exclusions, a timeout shortened. Those changes are written to the job's
// own config.yaml as they are made. Without this, the next run rebuilds its
// settings from the defaults and the command line alone, so every one of those
// adjustments is silently discarded and has to be made again.
//
// The saved settings are the base and this invocation's flags are layered on
// top, exactly as the resume command does it, so a run can still be given a
// different speed or scope on the way in. The start URLs come from the command
// line when any were given, since naming a URL is as explicit as passing a
// flag. --fresh-settings opts out and starts from the defaults.
func adoptSavedSettings(cfg *config.Config, o *options) (*config.Config, error) {
	if o.freshConfig {
		return cfg, nil
	}
	path := filepath.Join(config.WorkDirIn(cfg.Destination), "config.yaml")
	if _, err := os.Stat(path); err != nil {
		return cfg, nil
	}
	saved, err := config.Load(path)
	if err != nil {
		// A corrupt or unreadable file must not stop a download that would
		// otherwise run; the defaults are a usable answer.
		fmt.Fprintf(os.Stderr, "webvacuum: ignoring %s: %v\n", path, err)
		return cfg, nil
	}
	if len(cfg.StartURLs) > 0 {
		saved.StartURLs = cfg.StartURLs
	}
	saved.Destination = cfg.Destination
	if err := o.applyTo(saved); err != nil {
		return nil, err
	}
	if err := saved.Normalize(); err != nil {
		return nil, err
	}
	configSources = append(configSources, path+" (settings saved by the last run)")
	return saved, nil
}

// cmdResume continues an interrupted job using the config saved beside it.
func cmdResume(args []string) error {
	fs, o := newFlagSet("resume")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	dir := ""
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	dest, err := resolveDestination(dir)
	if err != nil {
		return err
	}
	cfg, err := loadJobConfig(dest)
	if err != nil {
		return err
	}
	// Flags given now override the saved settings, so a job can be resumed with
	// a different speed or scope.
	if err := o.applyTo(cfg); err != nil {
		return err
	}
	cfg.Destination = dest
	if err := cfg.Normalize(); err != nil {
		return err
	}
	return runJob(cfg, o)
}

// cmdRetry re-queues only what failed, without touching anything already saved.
func cmdRetry(args []string) error {
	fs, o := newFlagSet("retry")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	dest, err := resolveDestination(firstArg(fs.Args()))
	if err != nil {
		return err
	}
	cfg, err := loadJobConfig(dest)
	if err != nil {
		return err
	}
	if err := o.applyTo(cfg); err != nil {
		return err
	}
	cfg.Destination = dest
	// Keeping what is already on disk is the whole point of a retry pass.
	cfg.General.FileReplacement = config.ReplaceNever
	if cfg.Resilience.RetryPasses < 1 {
		cfg.Resilience.RetryPasses = 1
	}
	if err := cfg.Normalize(); err != nil {
		return err
	}
	return runJob(cfg, o)
}

// cmdLocalize rewrites links in an existing download without any network use.
func cmdLocalize(args []string) error {
	fs, o := newFlagSet("localize")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	dest, err := resolveDestination(firstArg(fs.Args()))
	if err != nil {
		return err
	}
	cfg, err := loadJobConfig(dest)
	if err != nil {
		return err
	}
	if err := o.applyTo(cfg); err != nil {
		return err
	}
	cfg.Destination = dest
	cfg.General.FileModification = config.ModifyLocalize
	if err := cfg.Normalize(); err != nil {
		return err
	}

	e, err := crawl.New(cfg)
	if err != nil {
		return err
	}
	defer e.Close()

	ctx, stop := signalContext()
	defer stop()

	// Re-localizing means starting from the originals when they were kept,
	// otherwise the already-rewritten files are simply left alone.
	go drainLog(e, o.quiet)
	if err := e.Localize(ctx); err != nil {
		return err
	}
	if entry := e.EntryPoint(); entry != "" {
		fmt.Printf("\nOpen: %s\n", filepath.Join(dest, filepath.FromSlash(entry)))
	}
	return nil
}

// cmdStatus prints what a download folder contains.
func cmdStatus(args []string) error {
	fs := newFlags("status")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	failures := fs.Bool("failures", false, "list every failed URL")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	dest, err := resolveDestination(firstArg(fs.Args()))
	if err != nil {
		return err
	}

	st, err := state.Open(config.WorkDirIn(dest))
	if err != nil {
		return err
	}
	defer st.Close()
	stats := st.Stats()

	if *asJSON {
		data, err := os.ReadFile(filepath.Join(config.WorkDirIn(dest), "report.json"))
		if err == nil {
			os.Stdout.Write(data)
			return nil
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(stats)
	}

	fmt.Printf("%s\n\n", dest)
	fmt.Printf("  saved     %d files, %s\n", stats.Done, humanBytes(stats.Bytes))
	fmt.Printf("  queued    %d\n", stats.Pending+stats.Active)
	fmt.Printf("  failed    %d\n", stats.Failed)
	fmt.Printf("  skipped   %d\n", stats.Skipped)
	fmt.Printf("  localized %d\n", stats.Localized)

	if data, err := os.ReadFile(filepath.Join(config.WorkDirIn(dest), "report.json")); err == nil {
		var rep crawl.Report
		if json.Unmarshal(data, &rep) == nil {
			fmt.Printf("\n  last run  %s, took %s\n", rep.FinishedAt.Format(time.RFC1123), rep.Duration)
			if rep.Outages > 0 {
				fmt.Printf("  network   %d outage(s), %s waiting\n", rep.Outages, rep.Downtime)
			}
			if rep.StopReason != "" {
				fmt.Printf("  stopped   %s\n", rep.StopReason)
			}
			if len(rep.TopErrors) > 0 {
				fmt.Printf("\n  most common failures\n")
				for _, b := range rep.TopErrors {
					fmt.Printf("    %5d  %s\n", b.Count, b.Reason)
				}
			}
			if rep.Entry != "" {
				fmt.Printf("\n  open: %s\n", filepath.Join(dest, filepath.FromSlash(rep.Entry)))
			}
		}
	}

	if *failures {
		fmt.Printf("\nFAILED URLS\n")
		for _, e := range st.Select(func(e *state.Entry) bool { return e.Status == state.Failed }) {
			code := e.ErrKind
			if e.HTTPCode > 0 {
				code = fmt.Sprint(e.HTTPCode)
			}
			fmt.Printf("  %-8s %s\n           %s\n", code, e.URL, e.Err)
		}
	}
	if stats.Pending > 0 {
		fmt.Printf("\n%d URLs are still queued — run 'webvacuum resume %s' to continue.\n", stats.Pending, dest)
	}
	return nil
}

// cmdConfig prints a starting configuration file.
func cmdConfig(args []string) error {
	fs, o := newFlagSet("config")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	cfg := config.Default()
	if fs.NArg() > 0 {
		cfg.StartURLs = fs.Args()
	} else {
		cfg.StartURLs = []string{"https://example.com/"}
	}
	if err := o.applyTo(cfg); err != nil {
		return err
	}
	if err := cfg.Normalize(); err != nil {
		return err
	}
	out := o.saveConfig
	if out == "" {
		out = "-"
	}
	if out == "-" {
		data, err := marshalConfig(cfg)
		if err != nil {
			return err
		}
		os.Stdout.Write(data)
		return nil
	}
	if err := cfg.Save(out); err != nil {
		return err
	}
	fmt.Printf("wrote %s — edit it, then run: webvacuum --config %s\n", out, out)
	return nil
}

// cmdServe starts the web interface with no job configured yet, so a job can be
// created from a browser on a machine with no terminal attached.
func cmdServe(args []string) error {
	fs, o := newFlagSet("serve")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	base := config.Default()
	if fs.NArg() > 0 {
		base.StartURLs = fs.Args()
	}
	if err := o.applyTo(base); err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	srv, err := webui.New(webui.Options{
		Addr:     o.addr,
		Token:    o.token,
		Template: base,
		Open:     o.open,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Web Vacuum — web interface\n\n  %s\n\n", srv.URL())
	fmt.Printf("Leave this running (or use nohup / systemd / tmux); the download\ncontinues as long as the process is alive.\nPress Ctrl-C to stop.\n\n")
	return srv.Serve(ctx)
}

// ---------------------------------------------------------------- runner

// runJob starts an engine and attaches whichever interface was asked for.
func runJob(cfg *config.Config, o *options) error {
	e, err := crawl.New(cfg)
	if err != nil {
		return err
	}
	defer e.Close()

	ctx, stop := signalContext()
	defer stop()

	if o.headless {
		return runHeadless(ctx, e, o)
	}
	if o.noTUI || o.quiet || !isTerminal() {
		return runPlain(ctx, e, o)
	}
	return runTUI(ctx, e)
}

func runTUI(ctx context.Context, e *crawl.Engine) error {
	m := tui.New(ctx, e)
	p := tea.NewProgram(m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithContext(ctx),
	)

	errc := make(chan error, 1)
	go func() { errc <- e.Run(ctx) }()

	if _, err := p.Run(); err != nil {
		e.Stop("terminal interface exited")
	}
	// The engine keeps running until it notices the stop; waiting for it means
	// the journal is checkpointed before the process exits.
	if err := <-errc; err != nil {
		return err
	}
	printSummary(e)
	return nil
}

func runHeadless(ctx context.Context, e *crawl.Engine, o *options) error {
	srv, err := webui.New(webui.Options{
		Addr:   o.addr,
		Token:  o.token,
		Engine: e,
		Open:   o.open,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Web Vacuum is downloading %s\n\n  watch it at %s\n\n", e.Config().Name, srv.URL())
	fmt.Printf("Safe to leave running in the background. Press Ctrl-C to stop.\n\n")

	errc := make(chan error, 1)
	go func() { errc <- e.Run(ctx) }()
	go drainLog(e, o.quiet)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	select {
	case err := <-errc:
		printSummary(e)
		if err != nil {
			e.Stop("job failed")
			return err
		}
		// The whole point of headless mode is that nobody is watching. The
		// interface stays up after the job finishes so the results are still
		// there whenever someone does look, and so the mirror can be browsed
		// through /files/. Ctrl-C is what ends it.
		fmt.Printf("The web interface is still running at %s\nPress Ctrl-C when you are done.\n\n", srv.URL())
		select {
		case <-ctx.Done():
			return nil
		case err := <-serveErr:
			return err
		}
	case err := <-serveErr:
		e.Stop("web interface stopped")
		<-errc
		return err
	}
}

func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// isTerminal reports whether stdout is a terminal, which decides between the
// full-screen interface and plain log lines. Checking the file mode avoids a
// dependency and behaves correctly for pipes, files and cron.
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
