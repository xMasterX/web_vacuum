package main

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/parse"
	"github.com/xMasterX/web_vacuum/internal/state"
)

// cmdCheck inspects a finished download and reports links that will not work
// offline.
//
// It exists because "some links are wrong" is impossible to act on. This walks
// the saved files, follows every link the way a browser would, and says which
// ones lead nowhere — and which file they are in.
func cmdCheck(args []string) error {
	fs := newFlags("check")
	verbose := fs.Bool("all", false, "list every broken link instead of a sample")
	limit := fs.Int("limit", 25, "how many examples to show")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	dest, err := resolveDestination(firstArg(fs.Args()))
	if err != nil {
		return err
	}

	var (
		files        int
		notLocalized int
		totalLinks   int
		localOK      int
		localBroke   int
		siteAbs      int
		remote       int
		examples     []brokenLink
		byFile       = map[string]int{}
	)

	// The journal knows which saved files are pages. Guessing from the file
	// extension would miss every board that serves its pages from a .php URL,
	// which is most of them.
	st, err := state.Open(config.WorkDirIn(dest))
	if err != nil {
		return err
	}
	defer st.Close()

	pages := st.Select(func(en *state.Entry) bool {
		if en.Status != state.Done || en.Path == "" || en.Alias {
			return false
		}
		mt := strings.ToLower(en.MediaType)
		return strings.Contains(mt, "html") || strings.HasSuffix(mt, "+xml")
	})

	for _, en := range pages {
		rel := en.Path
		p := filepath.Join(dest, filepath.FromSlash(rel))
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		files++
		// A page the rewrite pass never reached is a different problem from a
		// page whose links could not be matched, and the fix is different too,
		// so the two are counted apart.
		if !en.Localized {
			notLocalized++
		}

		doc := parse.HTML(data, parse.Options{ScanSrcset: true, ScanInlineCSS: true, FollowMeta: true})
		for _, l := range doc.Links {
			if l.Kind == parse.KindBase || l.Value == "" {
				continue
			}
			v := strings.TrimSpace(l.Value)
			if strings.HasPrefix(v, "#") || strings.HasPrefix(v, "data:") ||
				strings.HasPrefix(v, "javascript:") || strings.HasPrefix(v, "mailto:") {
				continue
			}
			totalLinks++

			switch {
			case strings.Contains(v, "://"):
				// Left pointing at the live site: works online, and is the
				// intended outcome for anything not downloaded.
				remote++

			case strings.HasPrefix(v, "/"):
				// A site-absolute path resolves against the filesystem root
				// when opened locally, so it can never work.
				siteAbs++
				byFile[rel]++
				examples = append(examples, brokenLink{rel, v, "site-absolute path — resolves outside the download"})

			default:
				target := v
				if i := strings.IndexAny(target, "#?"); i >= 0 {
					// A query string on a local path means the link was never
					// rewritten: saved files fold the query into the filename.
					if target[i] == '?' {
						localBroke++
						byFile[rel]++
						examples = append(examples, brokenLink{rel, v, "still carries a query string — this link was never rewritten"})
						continue
					}
					target = target[:i]
				}
				if dec, derr := url.PathUnescape(target); derr == nil {
					target = dec
				}
				joined := path.Join(path.Dir("/"+rel), target)
				if _, serr := os.Stat(filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(joined, "/")))); serr != nil {
					localBroke++
					byFile[rel]++
					examples = append(examples, brokenLink{rel, v, "points at a file that is not in the download"})
					continue
				}
				localOK++
			}
		}
	}

	fmt.Printf("%s\n\n", dest)
	fmt.Printf("  %d pages checked, %d links\n\n", files, totalLinks)
	fmt.Printf("  %6d  work offline\n", localOK)
	fmt.Printf("  %6d  point at the live site (not downloaded)\n", remote)
	fmt.Printf("  %6d  broken: relative links leading nowhere\n", localBroke)
	fmt.Printf("  %6d  broken: site-absolute paths\n", siteAbs)

	if notLocalized > 0 {
		fmt.Printf("\n  %d of %d pages were never rewritten at all.\n", notLocalized, files)
		fmt.Printf("  That is the cause of most of the broken links above, and it is\n")
		fmt.Printf("  fixed without downloading anything again:\n\n")
		fmt.Printf("      webvacuum localize %s\n", dest)
	}

	broken := localBroke + siteAbs
	if broken == 0 {
		fmt.Printf("\n  Every local link resolves.\n")
		return nil
	}

	fmt.Printf("\n  WORST PAGES\n")
	type fc struct {
		file string
		n    int
	}
	worst := make([]fc, 0, len(byFile))
	for f, n := range byFile {
		worst = append(worst, fc{f, n})
	}
	sort.Slice(worst, func(i, j int) bool { return worst[i].n > worst[j].n })
	for i, w := range worst {
		if i >= 10 {
			break
		}
		fmt.Printf("    %4d  %s\n", w.n, w.file)
	}

	show := *limit
	if *verbose {
		show = len(examples)
	}
	fmt.Printf("\n  EXAMPLES\n")
	for i, ex := range examples {
		if i >= show {
			fmt.Printf("    … and %d more (use --all to see them)\n", len(examples)-show)
			break
		}
		fmt.Printf("    in %s\n      %s\n      %s\n", ex.file, ex.link, ex.why)
	}

	fmt.Printf("\n  Run 'webvacuum localize %s' to rewrite links again,\n", dest)
	fmt.Printf("  or 'webvacuum resume %s' to download what is missing first.\n", dest)
	return nil
}

// brokenLink is one link that will not work when the copy is opened locally.
type brokenLink struct {
	file string
	link string
	why  string
}
