# Web Vacuum

Archive a website before it disappears.

Web Vacuum was built for the sites that are quietly fading out: old forums
running software nobody has updated in fifteen years, hobby sites on hosting
that will lapse when someone stops paying, communities whose archive exists in
exactly one place. It downloads the pages along with the stylesheets, scripts,
images and attachments they need, then rewrites every link so the copy browses
locally exactly as the original did — years after the original is gone.

That target shapes the whole tool. Old sites are slow, half-broken, and full of
links to hosts that stopped answering a decade ago. They put a session id in
every URL. They take hours to copy, over connections that drop. Anything built
for the modern web tends to give up on them.

Written in Go, so it cross-compiles to macOS, Linux and Windows from one source
tree, with no runtime to install on the machine you leave it running on.

```
webvacuum example.com
```

That downloads the whole site into `./example.com`, rewrites the links, and
prints the file to open. Press `F9` to pause, `F10` to quit. Run the same
command again to pick up where it stopped.

> **Built with AI assistance.** This tool was written with heavy use of an AI
> coding assistant. It has a substantial test suite covering the parts that
> matter — link rewriting, scope rules, resume, network outages — and it has
> been used on real sites, but it has not had years of exposure to the strange
> things the web does. **Expect bugs.** Check an archive with `webvacuum check`
> before trusting it, keep the original source if the site is still up, and
> please report anything that looks wrong.

## What it does for an old site

**It does not give up when the network does.** A dropped connection is not a
failure, it is a pause. Transport errors park the crawl in a "waiting for
network" state, probe until connectivity returns, and then carry on from the
same place. Nothing is abandoned, and no URL burns its retry budget on an
outage that was never its fault. Leaving a large download running overnight on
a flaky link is the case this was built for.

**Extra domains are one flag.** Forums that keep their images on a separate
host are the normal case, not an edge case:

```
webvacuum forum.example.com -d images.example-cdn.com
```

`-d` is repeatable, and a leading dot matches subdomains (`-d .static.example.com`).
Use `--asset-domain` to allow files from a host without crawling its pages.

**Stopping is safe.** Every URL's state lives in an append-only journal inside
the download folder, checkpointed as the crawl runs. Ctrl-C, a closed laptop,
or a power cut all resume cleanly.

**It runs headless.** Start it on a server, close the terminal, check on it
from a phone:

```
webvacuum example.com --headless
```

It prints a URL with an access token. The web interface shows live progress,
lets you pause, stop and change speed, and serves the downloaded copy so you
can click through what you have so far. `webvacuum serve` starts the interface
with no job configured, so the whole thing can be set up from a browser.

**Unreachable hosts cost two requests, not thousands.** Any site with years of
history accumulates links to image hosts that no longer resolve. A host that has
never once answered is written off after two failures and everything else queued
for it is skipped without touching the network — and those failures are never
mistaken for the network itself going down.

**It can run the JavaScript.** Sites that ship an empty shell and build the
page in the browser download as blank files with any ordinary mirroring tool.
Point it at a real browser instead:

```
webvacuum example.com --render
```

## Installing

```
go build -o webvacuum ./cmd/webvacuum
```

Cross-compiling for a Linux server from a Mac:

```
GOOS=linux GOARCH=amd64 go build -o webvacuum-linux ./cmd/webvacuum
```

## The terminal interface

`htop`-style, with a function-key bar that responds to both keys and mouse
clicks.

```
F1 Help   F2 Setup   F3 Filter   F4 Queue   F5 Errors   F6 Links
F7 Slower F8 Faster  F9 Pause    F10 Quit
```

`F6` rewrites links across everything downloaded so far, without waiting for the
crawl to finish. The log, and the other panes, are also on `Tab` and `1`–`5`.

The header shows files saved, queue depth, failures, speed, elapsed time, an
ETA, and how many network outages the job has already survived. Below it, one
row per connection shows what each is fetching, the way `htop` shows cores.
Tabs (`1`–`5`, or `Tab`) switch between the overview, the queue, the failures,
the log and the settings. `/` filters the current list.

It falls back to plain line-oriented output automatically when stdout is not a
terminal, so it behaves in cron jobs and pipes.

## Common jobs

```sh
# Just this section of a site, four links deep
webvacuum example.com/docs/ --scope directory --levels 4

# A site plus its subdomains, skipping the parts that are not worth having
webvacuum example.com --scope subdomains \
  --exclude '/(login|logout|cart|\?replytocom=)' \
  --exclude '\.(zip|iso|dmg)$'

# Be gentle with a small server
webvacuum example.com --connections 2 --per-host 1 --delay 1s --rate 500KB

# Images only, ignoring thumbnails
webvacuum example.com --types allow --category images --min-file-size 20KB

# Behind a login, using cookies exported from your browser
webvacuum example.com --cookies ~/Downloads/cookies.txt

# Retry only what failed, keeping everything already downloaded
webvacuum retry ./example.com

# Rewrite links again without touching the network
webvacuum localize ./example.com
```

## Choosing the scope

`--scope` is the coarse rule; `--include` and `--exclude` refine it.

| Value | Meaning |
|---|---|
| `host` | only the host you started on (**default**) |
| `subdomains` | that domain and any subdomain |
| `host+1` | the host, plus one hop to other sites (those pages are saved but not expanded) |
| `directory` | only at or below the starting path |
| `rules` | nothing but `--include` decides |
| `none` | follow every link anywhere — asks for confirmation first |

Decisions are made in this order, highest priority first:

1. `--block-domain`
2. `--exclude` / `--exclude-glob`
3. structural traps (absurd length, depth, or a repeating path segment)
4. `--include` / `--include-glob`
5. `-d` / `--domain`
6. `--asset-domain`, for supporting files
7. supporting files from anywhere, unless `--no-supporting-files`
8. `--scope`

An explicit exclude always wins. An explicit include beats the scope.

Rules 1 to 3 are the prohibitions, and they are applied twice: once when a link
is found and again just before the URL is actually requested. That second pass
is what makes an exclusion added mid-run worth adding. Thirty thousand URLs into
a forum you notice that everything matching `printthread` is junk; if the rule
only applied to links not yet discovered, all thirty thousand already in the
queue would be downloaded anyway. Instead they are dropped as the queue reaches
them, and the log says which rule dropped them.

A plain word is a valid regular expression, so `printthread` on its own works.
Start URLs are never dropped this way — they enter the queue without consulting
the rules, so a pattern that happens to match the starting page narrows the
crawl rather than ending it.

Rules 4 to 8 are not re-applied, because they depend on context a queued URL no
longer carries: the host+1 rule needs to know which page linked to it, and that
is not restored when a job resumes.

## Changing settings while it runs

Nothing needs stopping to be adjusted. Press `F2` for the Setup pane, move with
the arrow keys, press Enter to edit a value or flip a yes/no, and use the left
and right arrows to step through the choices for a setting. Changes take effect
on the running job immediately.

The same settings are editable in the web interface under the Settings tab, and
the header carries a `−`/`+` stepper for the connection count, which is the one
people reach for most.

Editing `<destination>/.webvacuum/config.yaml` in a text editor works too: the
file is re-read within a couple of seconds and applied. Whichever route you
take, the file and the running job stay in step.

Those changes survive the run. Every edit is written to that same file, and
running the command again picks it up as the starting point, so the connection
count you turned down and the pattern you added stay where you put them instead
of reverting to the defaults. Flags given on the new run still win over the
saved values, and `--fresh-settings` ignores the file and starts from the
defaults.

A repeatable option — `--exclude`, `--drop-param`, `-d` and the rest —
**replaces** the saved list rather than adding to it. Repeating it inside a
single run still builds the whole list, so `--exclude a --exclude b` means
exactly those two and nothing else. Giving it an empty value clears the list:

```sh
webvacuum forum.example.com --exclude ''
```

Without that rule, passing the same `--exclude` on each of five runs would leave
five copies of it, and there would be no way to take a pattern back once it had
been saved.

A few settings cannot change on a job already underway — the destination, the
start URLs, the path layout — because they decide where files land, and moving
that target mid-run would leave half a mirror in one shape and half in another.
Those are shown but locked, and an attempt to change them says so rather than
being quietly dropped.

**Speed and pausing.** Every job starts unlimited. `F7` steps the cap down
(5 MB/s, 2, 1, 512 KB…) and `F8` steps it back up; the current cap sits in the
header next to the live speed. Pausing stops transfers already in progress
rather than only refusing to start new ones, so throughput goes to zero at once
and resuming continues the same files from where they stopped.

## Rewriting links without waiting

Links are rewritten automatically **every five minutes** during a download and
again when the job ends — including when it is stopped or interrupted. A crawl
that dies unexpectedly still leaves a folder that browses, rather than one whose
every link points back at the live site.

To do it on demand:

```sh
webvacuum localize ./example.com     # no network use at all
```

Press `F6` (or `L`) in the terminal interface, or use **Rewrite links** in the
web interface, to run a pass on a job that is still downloading. Nothing pauses;
the pass reads what is on disk while the download continues around it.

Rewriting only ever improves a link. A target that had not been downloaded when
the last pass ran becomes local as soon as it arrives, so running it early costs
nothing. `general.localize_every: 0` turns the periodic pass off. On a very
large mirror the interval stretches itself, so passes never take a meaningful
share of the crawl's time.

To find out whether it worked:

```sh
webvacuum check ./example.com
```

That follows every link the way a browser would and reports which lead nowhere,
in which file, and why — including how many pages were never rewritten at all.

## Configuration files

Settings are read from these places, each overriding the one before:

1. `webvacuum.yaml` next to the binary — drop the binary and its settings on a
   server together and it is self-contained
2. `~/.config/webvacuum/config.yaml` (`%AppData%\webvacuum\config.yaml` on
   Windows) — your personal defaults
3. `webvacuum.yaml` in the current directory
4. `--config <file>`
5. command-line flags

`--no-global-config` skips the automatic ones. Whichever files were used are
listed in the plan printed before a download starts, so nothing is mysterious.

For anything you run more than once:

```sh
webvacuum config example.com --save-config job.yaml
webvacuum --config job.yaml
```

Any flag overrides the matching value in the file. Every job also writes its
resolved settings to `<destination>/.webvacuum/config.yaml` — inside the
download, not next to the binary — which is what `resume` reads, so a download
folder is self-describing and portable.

## Inside a download folder

```
example.com/
  index.html               the mirror, browsable by opening this file
  css/  img/  docs/ ...
  .webvacuum/
    config.yaml            the exact settings used
    journal.jsonl          every URL and its state — this is what resume reads
    report.json            counts, timing, outages, most common failures
    originals/             untouched copies, with --keep-originals
```

`webvacuum status ./example.com` summarizes it; add `--failures` to list every
URL that failed and why.

**Folder layout.** When one site is being mirrored, its files sit directly in
the destination, so the page to open is `<destination>/index.html`. Any
additional host gets its own folder named after it (`images.example-cdn.com/`,
or `127.0.0.1_8080/` when a port is involved). Pass `--host-folders` to give
every host a folder including the main one, which keeps the layout identical no
matter how many hosts a crawl ends up touching.

**Interrupting is safe at any moment.** The journal is checkpointed as the crawl
runs, and a `kill -9` mid-download loses at most the few seconds since the last
flush. Partial files are written to `.part` and renamed only once complete, so a
truncated file never masquerades as a finished one. `webvacuum resume <folder>`
picks up from there and re-downloads nothing it already has.

## How links are rewritten

Anything that was downloaded becomes a relative path, so the mirror works from
any folder and over `file://`. Anything that was **not** downloaded is rewritten
to an absolute URL, so a partial archive degrades into "click through to the
live site" rather than a folder full of dead links.

Handled: `href`/`src`/`data`/`poster`/`action`, `srcset` with its descriptors
preserved, `<base href>` (neutralized so relative links keep working), CSS
`url()` and `@import` in files, `<style>` blocks and `style` attributes,
`<meta http-equiv=refresh>`, fragments (`/page#section` → `./page.html#section`),
and `data-*` attributes you name with `--url-attribute`.

Rewriting is byte-surgical: the document is scanned for attribute positions and
only those spans are replaced. The markup is never re-serialized, so the archive
stays byte-identical to the original except for the links themselves — including
pages that are not valid HTML and pages that are not UTF-8. Running the pass
twice is a no-op.

## JavaScript rendering

Off by default, because it is much slower and heavier than fetching HTML and
most sites do not need it. When a site *does* need it, nothing else works.

```sh
# Render pages that look JavaScript-built, leave the rest alone
webvacuum example.com --render

# Render everything
webvacuum example.com --render-all

# Render only the parts that need it
webvacuum example.com --render-match '/app/' --render-match '/dashboard/'

# Gallery or forum with lazy-loaded images
webvacuum gallery.example.com --render --render-scroll
```

`--render` (mode `auto`) sends a page to the browser only when its markup
carries almost no visible text but does ship script — the signature of a page
assembled client-side. A content-bearing page that merely uses JavaScript is
fetched normally, so a mostly-static site pays almost nothing.

What rendering adds:

- **Script-generated links are followed.** A link that only exists after
  `innerHTML` runs is invisible to a parser; the crawler sees it and keeps going
  from there, exactly as with any other link.
- **Everything the page requested is downloaded.** The browser reports every URL
  the page asked for, so images inserted by script, fonts, and `fetch()`ed API
  responses all get saved even though they appear in no markup.
- **`--render-scroll` walks each page to the bottom**, which is what triggers
  lazy-loaded images. On a gallery this is the difference between a handful of
  images and all of them.

The rendered DOM is what gets saved, and the page's own scripts are removed from
it. That default matters: the browser already built the page, so letting the
same scripts run again offline makes them tear it down, re-request an API that
is no longer there, and leave you looking at a blank file. Pass
`--render-keep-scripts` if you want them anyway.

Requirements and knobs:

| Flag | Purpose |
|---|---|
| `--chrome-path` | point at a browser if it is somewhere unusual |
| `--chrome-url` | attach to a browser already running with remote debugging |
| `--render-tabs` | how many pages render at once (default 2) |
| `--render-wait` / `--render-idle` | how long to let a page settle |
| `--render-block` | URL wildcards the browser refuses to load, e.g. `'*.mp4'` |
| `--no-sandbox` | needed when running as root, which is normal in containers |
| `--render-headful` | show the window, to watch what a site actually does |

Any Chrome, Chromium, Edge or Brave install works; the binary is found
automatically, or set `SITEDUMPER_CHROME`. If no browser is present, or it keeps
failing, the crawl says so once and carries on with plain HTTP rather than
stopping.

## What the interfaces show

Both interfaces report the same numbers, because both are views over one engine.

The connection rows are the per-download equivalent of `htop`'s per-core meters:
each shows what stage the request is at (queued, connecting, receiving,
rendering, waiting to retry, waiting for network), the status code once headers
arrive, bytes against the expected total, that transfer's own speed, and how
long it has been going. A connection that has stalled is coloured differently
from one that is merely slow.

The log is columned rather than prose — time, status, duration, size, media
type, path — so a run can be scanned for the slow, the large and the failed
without reading it line by line. Retried URLs carry a `×n` marker and rendered
pages a `js` one.

The web interface is dark by default, with a toggle in the header. It is meant
to sit open on a second screen for hours, and a bright panel doing that is
tiring.

## Politeness

`robots.txt`, robots meta tags and `rel="nofollow"` are honoured by default,
along with `Crawl-delay`. `--ignore-robots` and `--ignore-nofollow` turn that
off for sites you own or have permission to archive.

`Retry-After` is obeyed. Repeated `429`s shrink the per-host connection count
and lengthen the delay automatically, recovering slowly afterwards, so a long
crawl throttles itself instead of getting banned.

## Testing

```
go test ./...
```

Covers URL canonicalization and path mapping (including that no crafted URL can
write outside the destination), scope rules, the HTML and CSS parsers, link
rewriting, `robots.txt` matching, the retry and outage behaviour against a
server that drops connections mid-download, and full crawls of a fake site.

Two of those are worth naming, because they are the claims the tool lives or
dies by:

- **Transitive closure.** A fixture is built so the only route to page N is a
  link inside page N-1, sixty pages deep, with further branches reachable only
  through an iframe, a `meta refresh`, an image map and a chain of CSS
  `@import`s. The test asserts every one of them was reached, that a second
  allowed domain was crawled onward rather than merely touched, and that a third
  domain was never contacted at all.
- **Rendering.** The same JavaScript-built site is crawled twice. Without
  rendering the crawler correctly finds nothing beyond the shell; with it, the
  script-generated link, the page *that* page links to, the script-inserted
  image and the `fetch()`ed JSON are all downloaded.

The rendering tests skip themselves when no browser is installed, so a machine
without Chrome still gets a clean run.
