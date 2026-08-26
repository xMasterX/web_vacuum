package crawl

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
)

// TestEverySettingChangeSticks guards against the failure where a change is
// accepted, reported as fine, and then quietly discarded because the code that
// decides whether anything changed did not know about that particular field.
//
// Each case sets one value and reads it back off the running engine.
func TestEverySettingChangeSticks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>hi</body></html>")
	}))
	defer srv.Close()

	e, err := New(testConfig(t, srv.URL+"/", t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	cases := []struct {
		name  string
		apply func(*config.Config)
		check func(*config.Config) string
		want  string
	}{
		{"timeout", func(c *config.Config) { c.Request.Timeout = config.Duration(5 * time.Second) },
			func(c *config.Config) string { return c.Request.Timeout.String() }, "5s"},
		{"attempts", func(c *config.Config) { c.Request.Attempts = 3 },
			func(c *config.Config) string { return fmt.Sprint(c.Request.Attempts) }, "3"},
		{"offsite timeout", func(c *config.Config) { c.Request.OffsiteTimeout = config.Duration(2 * time.Second) },
			func(c *config.Config) string { return c.Request.OffsiteTimeout.String() }, "2s"},
		{"connections", func(c *config.Config) { c.General.Connections = 5 },
			func(c *config.Config) string { return fmt.Sprint(c.General.Connections) }, "5"},
		{"per host", func(c *config.Config) { c.Request.PerHostConns = 2 },
			func(c *config.Config) string { return fmt.Sprint(c.Request.PerHostConns) }, "2"},
		{"delay", func(c *config.Config) { c.Request.Delay = config.Duration(750 * time.Millisecond) },
			func(c *config.Config) string { return c.Request.Delay.String() }, "750ms"},
		{"speed limit", func(c *config.Config) { c.Limits.MaxRate = config.ByteSize(512 * 1024) },
			func(c *config.Config) string { return c.Limits.MaxRate.String() }, "512KB"},
		{"max levels", func(c *config.Config) { c.Limits.MaxLevels = 4 },
			func(c *config.Config) string { return fmt.Sprint(c.Limits.MaxLevels) }, "4"},
		{"max file size", func(c *config.Config) { c.Limits.MaxFileSize = config.ByteSize(10 << 20) },
			func(c *config.Config) string { return c.Limits.MaxFileSize.String() }, "10MB"},
		{"min file size", func(c *config.Config) { c.Limits.MinFileSize = config.ByteSize(2 << 10) },
			func(c *config.Config) string { return c.Limits.MinFileSize.String() }, "2KB"},
		{"user agent", func(c *config.Config) { c.Request.UserAgent = "firefox" },
			func(c *config.Config) string { return c.Request.UserAgent }, "firefox"},
		{"supporting files", func(c *config.Config) { c.General.SupportingFiles = config.SupportingNone },
			func(c *config.Config) string { return string(c.General.SupportingFiles) }, "none"},
		{"replacement", func(c *config.Config) { c.General.FileReplacement = config.ReplaceAlways },
			func(c *config.Config) string { return string(c.General.FileReplacement) }, "always"},
		{"ignore robots", func(c *config.Config) { c.General.IgnoreRobots = true },
			func(c *config.Config) string { return fmt.Sprint(c.General.IgnoreRobots) }, "true"},
		{"ignore nofollow", func(c *config.Config) { c.General.IgnoreNofollow = true },
			func(c *config.Config) string { return fmt.Sprint(c.General.IgnoreNofollow) }, "true"},
		{"render mode", func(c *config.Config) { c.Render.Mode = config.RenderAuto },
			func(c *config.Config) string { return string(c.Render.Mode) }, "auto"},
		{"render scroll", func(c *config.Config) { c.Render.Scroll = true },
			func(c *config.Config) string { return fmt.Sprint(c.Render.Scroll) }, "true"},
		{"drop parameters", func(c *config.Config) { c.Scope.DropQueryParams = []string{"sid", "s"} },
			func(c *config.Config) string { return fmt.Sprint(c.Scope.DropQueryParams) }, "[sid s]"},
		{"log level", func(c *config.Config) { c.Log.Level = "debug" },
			func(c *config.Config) string { return c.Log.Level }, "debug"},
		{"scan comments", func(c *config.Config) { c.Scope.ScanComments = true },
			func(c *config.Config) string { return fmt.Sprint(c.Scope.ScanComments) }, "true"},
		{"sitemap", func(c *config.Config) { c.Webpage.UseSitemap = true },
			func(c *config.Config) string { return fmt.Sprint(c.Webpage.UseSitemap) }, "true"},
	}

	for _, c := range cases {
		next := e.Config().Clone()
		c.apply(next)

		res, err := e.Reconfigure(next)
		if err != nil {
			t.Errorf("%s: Reconfigure: %v", c.name, err)
			continue
		}
		if !res.Changed() {
			t.Errorf("%s: reported no change", c.name)
			continue
		}
		if got := c.check(e.Config()); got != c.want {
			t.Errorf("%s: value reverted — got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestUnchangedSettingsReportNoChange keeps the other half honest: re-applying
// the same settings must not look like an edit.
func TestUnchangedSettingsReportNoChange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html></html>")
	}))
	defer srv.Close()

	e, err := New(testConfig(t, srv.URL+"/", t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	res, err := e.Reconfigure(e.Config().Clone())
	if err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if res.Changed() {
		t.Errorf("re-applying the same settings reported a change: %v", res.Applied)
	}
}
