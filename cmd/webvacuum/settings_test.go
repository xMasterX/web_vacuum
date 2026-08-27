package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xMasterX/web_vacuum/internal/config"
)

// savedJob writes a job folder holding settings a previous run would have saved
// after someone edited them from the Setup pane.
func savedJob(t *testing.T, body string) string {
	t.Helper()
	dest := t.TempDir()
	dir := config.WorkDirIn(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dest
}

// resolved runs the command far enough to settle every setting and returns the
// configuration it would have crawled with. --save-config stops it there, so no
// request is ever made.
func resolved(t *testing.T, args ...string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "resolved.yaml")
	if err := run(append(args, "--save-config", out)); err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const editedSettings = `
start_urls:
  - http://example.com/
general:
  connections: 17
request:
  user_agent: marker-agent
  timeout: 5s
scope:
  exclude:
    - printthread
`

// TestRepeatRunKeepsSettingsEditedLastTime covers the whole point of being able
// to edit settings while a job runs.
//
// Re-running the same command is the documented way to resume, and the settings
// changed from the Setup pane are written to the job's own config.yaml as they
// are made. If the next run rebuilds its settings from the defaults and the
// command line, every one of those edits is thrown away — the connection count
// goes back to eight, the timeout back to forty-five seconds, and the exclusion
// added after watching the queue fill with junk is simply gone.
func TestRepeatRunKeepsSettingsEditedLastTime(t *testing.T) {
	dest := savedJob(t, editedSettings)
	got := resolved(t, "http://example.com/", "-o", dest)

	for _, want := range []string{"connections: 17", "user_agent: marker-agent", "timeout: 5s", "printthread"} {
		if !strings.Contains(got, want) {
			t.Errorf("resolved settings lost %q; the last run's edits were discarded", want)
		}
	}
}

// TestFlagsGivenNowBeatSettingsFromLastTime keeps the saved settings a starting
// point rather than a cage: naming a value on the command line still wins, and
// only the values not named are inherited.
func TestFlagsGivenNowBeatSettingsFromLastTime(t *testing.T) {
	dest := savedJob(t, editedSettings)
	got := resolved(t, "http://example.com/", "-o", dest, "--connections", "3")

	if !strings.Contains(got, "connections: 3") {
		t.Error("--connections did not override the saved value")
	}
	if !strings.Contains(got, "user_agent: marker-agent") {
		t.Error("a setting nobody mentioned on the command line should still be inherited")
	}
}

// TestFreshSettingsIgnoresTheSavedFile is the way back out, for when the saved
// settings are the problem rather than the point.
func TestFreshSettingsIgnoresTheSavedFile(t *testing.T) {
	dest := savedJob(t, editedSettings)
	got := resolved(t, "http://example.com/", "-o", dest, "--fresh-settings")

	if strings.Contains(got, "marker-agent") || strings.Contains(got, "printthread") {
		t.Error("--fresh-settings still picked up the saved settings")
	}
	if !strings.Contains(got, "connections: 8") {
		t.Error("--fresh-settings should fall back to the defaults")
	}
}

// TestUnreadableSavedSettingsDoNotStopTheJob keeps a damaged file — a run cut
// short mid-write, say — from turning into a refusal to download anything. The
// defaults are a usable answer and the download is what was asked for.
func TestUnreadableSavedSettingsDoNotStopTheJob(t *testing.T) {
	dest := savedJob(t, "general: [this is not, a mapping")
	got := resolved(t, "http://example.com/", "-o", dest)

	if !strings.Contains(got, "connections: 8") {
		t.Error("a corrupt saved config should leave the defaults in place, not fail")
	}
}
