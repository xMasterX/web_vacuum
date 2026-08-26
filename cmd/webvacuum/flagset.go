package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// flagSet wraps flag.FlagSet to print help that groups related options, since
// a flat alphabetical list of sixty flags is unusable.
type flagSet struct {
	*flag.FlagSet
	name string
}

func newFlags(name string) *flagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	f := &flagSet{FlagSet: fs, name: name}
	fs.Usage = f.printUsage
	return f
}

// groups orders the option list the way someone reading it would expect.
var groups = []struct {
	title string
	names []string
}{
	{"Where it goes", []string{"o", "out", "output", "name", "config", "save-config"}},
	{"What to download", []string{
		"scope", "d", "domain", "domains", "asset-domain", "block-domain",
		"include", "exclude", "include-glob", "exclude-glob",
		"l", "levels", "depth", "sitemap", "ignore-query", "drop-param",
	}},
	{"How much", []string{
		"max-files", "max-size", "max-file-size", "min-file-size",
		"max-files-per-host", "max-time", "rate", "limit-rate",
		"types", "category", "categories", "allow-ext", "block-ext",
	}},
	{"How to connect", []string{
		"c", "connections", "per-host", "delay", "delay-jitter", "attempts",
		"retries", "timeout", "u", "user-agent", "H", "header", "cookie",
		"cookies", "cookie-jar", "auth", "proxy", "insecure",
	}},
	{"Politeness and fidelity", []string{
		"ignore-robots", "ignore-nofollow", "supporting-files", "no-supporting-files", "error-pages",
		"replace", "no-localize", "keep-originals", "strip-scripts", "banner",
		"scan-scripts", "scan-comments", "follow-forms",
	}},
	{"JavaScript rendering (needs Chrome or Chromium)", []string{
		"render", "js", "render-all", "render-mode", "render-match", "render-skip",
		"render-tabs", "render-wait", "render-idle", "render-timeout", "render-scroll",
		"render-block", "no-follow-requests", "chrome-path", "chrome-url",
		"render-headful", "no-sandbox", "render-keep-scripts",
	}},
	{"Interface", []string{"headless", "addr", "token", "open", "no-tui", "quiet", "y", "log", "log-level", "no-resume"}},
}

func (f *flagSet) printUsage() {
	w := os.Stderr
	fmt.Fprintf(w, "Web Vacuum — %s\n\nUSAGE\n  webvacuum %s [options]\n\n", summaryFor(f.name), f.name)

	seen := map[string]bool{}
	all := map[string]*flag.Flag{}
	f.VisitAll(func(fl *flag.Flag) { all[fl.Name] = fl })

	for _, g := range groups {
		var lines []string
		for _, n := range g.names {
			fl, ok := all[n]
			if !ok || seen[n] {
				continue
			}
			seen[n] = true
			lines = append(lines, formatFlag(fl))
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s\n", g.title)
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
		fmt.Fprintln(w)
	}

	var rest []string
	for n, fl := range all {
		if !seen[n] {
			rest = append(rest, formatFlag(fl))
		}
	}
	if len(rest) > 0 {
		sort.Strings(rest)
		fmt.Fprintln(w, "Other")
		for _, l := range rest {
			fmt.Fprintln(w, l)
		}
	}
}

func formatFlag(fl *flag.Flag) string {
	dash := "-"
	if len(fl.Name) > 1 {
		dash = "--"
	}
	name := dash + fl.Name
	def := fl.DefValue
	if def != "" && def != "false" && def != "0" {
		name += "=" + def
	}
	usage := strings.ReplaceAll(fl.Usage, "\n", " ")
	if len(name) >= 26 {
		return fmt.Sprintf("  %s\n%s%s", name, strings.Repeat(" ", 28), usage)
	}
	return fmt.Sprintf("  %-26s%s", name, usage)
}

func summaryFor(name string) string {
	for _, c := range commands() {
		if c.name == name {
			return c.summary
		}
	}
	return "download a site"
}

// Parse accepts options and positional arguments in any order.
//
// Go's flag package stops parsing at the first non-flag argument, which would
// make the most natural invocation — "webvacuum example.com --levels 3" —
// silently treat the options as URLs. Reordering first means the command line
// behaves the way every other tool's does.
func (f *flagSet) Parse(args []string) error {
	return f.FlagSet.Parse(reorderArgs(f.FlagSet, args))
}

// reorderArgs moves options ahead of positional arguments, keeping each
// option's value attached to it.
func reorderArgs(fs *flag.FlagSet, args []string) []string {
	var opts, positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]

		// Everything after "--" is positional by convention.
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		// A lone "-" and anything not starting with "-" is positional. A
		// negative number is too, so "--levels -1" keeps working.
		if len(a) < 2 || a[0] != '-' || isNegativeNumber(a) {
			positional = append(positional, a)
			continue
		}

		opts = append(opts, a)
		name := strings.TrimLeft(a, "-")
		if strings.ContainsRune(name, '=') {
			continue // value is already attached
		}
		fl := fs.Lookup(name)
		if fl == nil || isBoolFlag(fl) {
			continue // no separate value to consume
		}
		if i+1 < len(args) {
			i++
			opts = append(opts, args[i])
		}
	}
	return append(opts, positional...)
}

func isBoolFlag(fl *flag.Flag) bool {
	b, ok := fl.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func isNegativeNumber(s string) bool {
	if len(s) < 2 || s[0] != '-' {
		return false
	}
	for _, r := range s[1:] {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}
