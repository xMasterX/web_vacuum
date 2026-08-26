package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/crawl"
)

// runPlain is the interface for pipes, cron jobs and CI: line-oriented output
// that stays readable in a log file, with a periodic one-line progress summary.
func runPlain(ctx context.Context, e *crawl.Engine, o *options) error {
	fmt.Print(describePlan(e.Config()))
	fmt.Println()

	events, unsub := e.Subscribe(4096)
	defer unsub()

	errc := make(chan error, 1)
	go func() { errc <- e.Run(ctx) }()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			printEvent(ev, o.quiet)

		case <-ticker.C:
			if !o.quiet {
				s := e.Snapshot()
				fmt.Printf("[%s] %d saved, %d queued, %d failed, %s, %s/s\n",
					short(s.Elapsed), s.Stats.Done, s.Stats.Pending+s.Stats.Active,
					s.Stats.Failed, humanBytes(s.Stats.Bytes), humanBytes(int64(s.BytesPerSec)))
			}

		case err := <-errc:
			printSummary(e)
			return err
		}
	}
}

func printEvent(ev crawl.Event, quiet bool) {
	switch ev.Kind {
	case crawl.EventFailed:
		fmt.Fprintf(os.Stderr, "failed  %s  %s\n", ev.URL, ev.Message)
	case crawl.EventNetwork:
		fmt.Fprintf(os.Stderr, "network %s\n", ev.Message)
	case crawl.EventFetched:
		if !quiet {
			fmt.Printf("saved   %s\n", ev.Path)
		}
	case crawl.EventLog:
		if ev.Level == crawl.LevelError || ev.Level == crawl.LevelWarn {
			fmt.Fprintf(os.Stderr, "%s\n", ev.Message)
		} else if !quiet {
			fmt.Printf("%s\n", ev.Message)
		}
	}
}

// drainLog consumes events so the bus never blocks when no interface is
// rendering them, and surfaces the ones that matter.
func drainLog(e *crawl.Engine, quiet bool) {
	events, unsub := e.Subscribe(4096)
	defer unsub()
	for ev := range events {
		switch ev.Kind {
		case crawl.EventFailed, crawl.EventNetwork:
			printEvent(ev, quiet)
		case crawl.EventLog:
			if ev.Level == crawl.LevelWarn || ev.Level == crawl.LevelError {
				printEvent(ev, quiet)
			}
		}
	}
}

// printSummary is the last thing a run prints: what was saved, what went wrong,
// and the one path the user actually wants — the file to open.
func printSummary(e *crawl.Engine) {
	s := e.Snapshot()
	st := s.Stats

	fmt.Printf("\n%s\n", strings.Repeat("─", 60))
	fmt.Printf("  %s in %s\n\n", strings.ToUpper(string(s.Phase)), short(s.Elapsed))
	fmt.Printf("  saved     %d files, %s\n", st.Done, humanBytes(st.Bytes))
	if st.Failed > 0 {
		fmt.Printf("  failed    %d  (webvacuum retry %s)\n", st.Failed, e.Config().Destination)
	}
	if st.Pending > 0 {
		fmt.Printf("  queued    %d  (webvacuum resume %s)\n", st.Pending, e.Config().Destination)
	}
	if st.Skipped > 0 {
		fmt.Printf("  skipped   %d  (out of scope or filtered)\n", st.Skipped)
	}
	if s.Network.TotalOutages > 0 {
		fmt.Printf("  network   survived %d outage(s), %s spent waiting\n",
			s.Network.TotalOutages, short(s.Network.TotalDowntime))
	}
	if entry := e.EntryPoint(); entry != "" {
		fmt.Printf("\n  Open: %s\n", filepath.Join(e.Config().Destination, filepath.FromSlash(entry)))
	}
	fmt.Printf("%s\n", strings.Repeat("─", 60))
}

func short(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// marshalConfig renders a config as YAML with a short explanatory header, so a
// generated file is something a person can actually edit.
func marshalConfig(cfg *config.Config) ([]byte, error) {
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	header := `# Web Vacuum configuration
#
# Run with:  webvacuum --config this-file.yaml
# Any command-line flag overrides the matching value here.
#
# scope.constraint decides how far the crawl roams:
#   host        only the host in start_urls          (the default)
#   subdomains  that domain and any subdomain
#   host+1      the host, plus one hop to other sites
#   directory   only at or below the starting path
#   rules       nothing but scope.include decides
#   none        follow every link anywhere           (rarely what you want)
#
# scope.hosts adds whole domains to the crawl — this is how you pull in a
# forum's separate image host. A leading dot matches subdomains:
#   hosts: [images.example-cdn.com, .static.example.com]
#
# scope.exclude wins over everything; entries are regular expressions.
#
# Zero means "no limit" everywhere in the limits section.

`
	return append([]byte(header), body...), nil
}
