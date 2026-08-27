package rules

import (
	"net/url"
	"testing"

	"github.com/xMasterX/web_vacuum/internal/config"
	"github.com/xMasterX/web_vacuum/internal/urlx"
)

func engineFor(t *testing.T, tweak func(*config.Config)) *Engine {
	t.Helper()
	cfg := config.Default()
	cfg.StartURLs = []string{"https://forum.example.com/"}
	cfg.Destination = t.TempDir()
	if tweak != nil {
		tweak(cfg)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func check(t *testing.T, e *Engine, raw string, role Role) Decision {
	t.Helper()
	u, err := urlx.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	from, _ := urlx.Parse("https://forum.example.com/thread.php?t=1")
	return e.CheckURL(u, role, 1, from)
}

// TestSupportingFileScope is the setting that decides whether an old forum
// downloads cleanly or spends hours timing out. Posts accumulate links to image
// hosts that stopped existing years ago; each one is an asset, and letting
// assets through from anywhere means chasing all of them.
func TestSupportingFileScope(t *testing.T) {
	cases := []struct {
		mode  config.SupportingMode
		url   string
		allow bool
		why   string
	}{
		// The site's own files are always fine.
		{config.SupportingNone, "https://forum.example.com/img/logo.png", true, "same host"},
		{config.SupportingRelated, "https://forum.example.com/img/logo.png", true, "same host"},

		// A sister host on the same domain is the usual static/CDN split.
		{config.SupportingRelated, "https://static.example.com/style.css", true, "same registrable domain"},
		{config.SupportingNone, "https://static.example.com/style.css", false, "off-host, mode none"},

		// A genuinely third-party host is what causes the trouble.
		{config.SupportingRelated, "https://dead-image-host.example/pic.jpg", false, "third party"},
		{config.SupportingNone, "https://dead-image-host.example/pic.jpg", false, "third party"},
		{config.SupportingAny, "https://dead-image-host.example/pic.jpg", true, "explicitly allowed"},
	}

	for _, c := range cases {
		e := engineFor(t, func(cfg *config.Config) { cfg.General.SupportingFiles = c.mode })
		d := check(t, e, c.url, RoleAsset)
		if d.Allow != c.allow {
			t.Errorf("mode=%s %s: allow=%v want %v (%s) reason=%q",
				c.mode, c.url, d.Allow, c.allow, c.why, d.Reason)
		}
	}
}

// TestAssetDomainOptsInOneHost checks the opt-in path, which is only meaningful
// once the blanket allowance is switched off: with the default "any" every
// asset host is already permitted.
func TestAssetDomainOptsInOneHost(t *testing.T) {
	e := engineFor(t, func(cfg *config.Config) {
		cfg.General.SupportingFiles = config.SupportingNone
		cfg.Scope.AssetHosts = []string{"images.example-cdn.com"}
	})
	if d := check(t, e, "https://images.example-cdn.com/pic.jpg", RoleAsset); !d.Allow {
		t.Errorf("named asset host should be allowed: %s", d.Reason)
	}
	// It is allowed for files only; its pages are not crawled.
	if d := check(t, e, "https://images.example-cdn.com/gallery.html", RolePage); d.Allow {
		t.Error("an asset host's pages should not be crawled")
	}
	if d := check(t, e, "https://other-cdn.example/pic.jpg", RoleAsset); d.Allow {
		t.Error("an unnamed host should still be refused")
	}
}

// TestPagesStayOnTheStartingHost is the plain expectation: one domain means one
// domain, and adding a second means exactly two.
func TestPagesStayOnTheStartingHost(t *testing.T) {
	e := engineFor(t, nil)
	if d := check(t, e, "https://forum.example.com/index.php", RolePage); !d.Allow {
		t.Errorf("own host refused: %s", d.Reason)
	}
	for _, off := range []string{
		"https://other.example.com/page.html",
		"https://forum.example.com.evil.test/page.html",
		"https://sub.forum.example.com/page.html",
	} {
		if d := check(t, e, off, RolePage); d.Allow {
			t.Errorf("%s should be out of scope", off)
		}
	}

	two := engineFor(t, func(cfg *config.Config) {
		cfg.Scope.Hosts = []string{"images.example-cdn.com"}
	})
	if d := check(t, two, "https://images.example-cdn.com/album.html", RolePage); !d.Allow {
		t.Errorf("the second domain should be crawlable: %s", d.Reason)
	}
	if d := check(t, two, "https://third.example/page.html", RolePage); d.Allow {
		t.Error("a third domain should still be refused")
	}
}

func TestExcludeBeatsEverything(t *testing.T) {
	e := engineFor(t, func(cfg *config.Config) {
		cfg.Scope.Exclude = []string{`/(login|logout|newreply)`}
		cfg.Scope.Include = []string{`.*`}
		cfg.Scope.Hosts = []string{"*"}
	})
	if d := check(t, e, "https://forum.example.com/login.php", RolePage); d.Allow {
		t.Error("exclude must win over include and an allowed host")
	}
	if d := check(t, e, "https://forum.example.com/showthread.php?t=1", RolePage); !d.Allow {
		t.Errorf("unrelated page wrongly excluded: %s", d.Reason)
	}
}

func TestSpiderTrapsAreRefused(t *testing.T) {
	e := engineFor(t, nil)
	if d := check(t, e, "https://forum.example.com/a/b/a/b/a/b/a/b/", RolePage); d.Allow {
		t.Error("a repeating path should be treated as a trap")
	}
}

// TestDefaultKeepsAssetsCloseToTheSite pins the default down.
//
// A mirror has to render, so files from the site's own domain family are
// fetched without being asked for. Genuinely third-party hosts are not: on any
// site with years of history those are mostly status badges and image hosts
// that stopped answering long ago, and chasing them is what makes a crawl look
// stalled. Naming a host with --asset-domain, or choosing "any", opts in.
func TestDefaultKeepsAssetsCloseToTheSite(t *testing.T) {
	e := engineFor(t, nil)

	if d := check(t, e, "https://static.example.com/app.css", RoleAsset); !d.Allow {
		t.Errorf("a sister host on the same domain should be fetched: %s", d.Reason)
	}
	if d := check(t, e, "https://cdn.unrelated-host.example/app.css", RoleAsset); d.Allow {
		t.Error("a third-party host should need to be asked for")
	}
	if d := check(t, e, "https://mystatus.skype.com/smallicon/someone", RoleAsset); d.Allow {
		t.Error("a third-party badge image should not be queued")
	}
	// Pages are a different matter again: scope still means scope.
	if d := check(t, e, "https://cdn.unrelated-host.example/index.html", RolePage); d.Allow {
		t.Error("a third-party page must be out of scope")
	}
}

// TestInScopeMarksThirdPartyHosts checks the signal the fetcher uses to decide
// how patient to be with a host.
func TestInScopeMarksThirdPartyHosts(t *testing.T) {
	e := engineFor(t, func(cfg *config.Config) {
		cfg.Scope.Hosts = []string{"images.example-cdn.com"}
	})
	for _, in := range []string{
		"https://forum.example.com/x",
		"https://images.example-cdn.com/y.png",
	} {
		if !e.InScope(mustURL(t, in)) {
			t.Errorf("%s should count as in scope", in)
		}
	}
	for _, out := range []string{
		"https://mystatus.skype.com/smallicon/someone",
		"https://tracker.example.net/pixel.gif",
	} {
		if e.InScope(mustURL(t, out)) {
			t.Errorf("%s should count as third party", out)
		}
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := urlx.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u
}

// TestProhibitedAgreesWithCheckURL keeps the two apart from drifting.
//
// Prohibited exists so the deny-only rules can be applied a second time, just
// before a queued URL is fetched. That is only sound while it stays a faithful
// prefix of CheckURL: a rule added to one and not the other would either let
// something through the second pass that the first refused, or — worse — start
// deleting queued work that the settings actually allow.
//
// Whenever Prohibited says no, CheckURL must say no for the same reason. The
// converse does not hold: CheckURL denies plenty of URLs on the permissive
// half of its logic, which Prohibited deliberately does not consider.
func TestProhibitedAgreesWithCheckURL(t *testing.T) {
	e := engineFor(t, func(c *config.Config) {
		c.Scope.Exclude = []string{`/(login|logout|newreply)`}
		c.Scope.BlockHosts = []string{"ads.example.net"}
		c.Scope.MaxURLLength = 120
		c.Scope.MaxPathDepth = 4
		c.Scope.MaxRepeatSegment = 2
		c.Limits.MaxLevels = 3
		c.Types.BlockExtensions = []string{".exe"}
		// An include pattern wide enough to allow everything, so that any
		// disagreement below can only come from the deny-only half.
		c.Scope.Include = []string{".*"}
	})

	urls := []string{
		"https://forum.example.com/showthread.php?t=1",
		"https://forum.example.com/login.php",
		"https://forum.example.com/newreply.php?p=9",
		"https://ads.example.net/banner.gif",
		"https://forum.example.com/setup.exe",
		"https://forum.example.com/a/b/c/d/e/f/deep.html",
		"https://forum.example.com/a/a/a/a/loop.html",
		"https://forum.example.com/x?q=" + longString(200),
		"https://forum.example.com/images/logo.png",
	}

	for _, raw := range urls {
		u, err := urlx.Parse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		for _, role := range []Role{RolePage, RoleAsset} {
			for _, depth := range []int{0, 1, 9} {
				d, forbidden := e.Prohibited(u, role, depth)
				if !forbidden {
					continue
				}
				full := e.CheckURL(u, role, depth, nil)
				if full.Allow {
					t.Errorf("Prohibited denied %s (role %v, depth %d) as %q but CheckURL allowed it",
						raw, role, depth, d.Reason)
				}
				if full.Reason != d.Reason {
					t.Errorf("%s (role %v, depth %d): Prohibited says %q, CheckURL says %q",
						raw, role, depth, d.Reason, full.Reason)
				}
			}
		}
	}
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
