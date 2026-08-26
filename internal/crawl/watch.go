package crawl

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/xMasterX/web_vacuum/internal/config"
)

// watchInterval is how often the job's settings file is checked. Polling beats
// a filesystem-notification dependency here: the file is one known path, edits
// are rare, and a two-second delay after saving is imperceptible.
const watchInterval = 2 * time.Second

// watchConfig reloads the job's config.yaml whenever it changes on disk, so
// editing the file is a way to steer a running download rather than something
// that only matters next time.
//
// The engine writes that same file when settings change from a UI, so the
// watcher must not treat its own writes as edits — hence comparing content
// rather than timestamps.
func (e *Engine) watchConfig(ctx context.Context) func() {
	path := filepath.Join(e.cfg().WorkDir(), "config.yaml")

	lastSeen, _ := os.ReadFile(path)
	done := make(chan struct{})

	go func() {
		t := time.NewTicker(watchInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				data, err := os.ReadFile(path)
				if err != nil || len(data) == 0 {
					continue
				}
				if string(data) == string(lastSeen) {
					continue
				}
				lastSeen = data

				next, err := config.Parse(data)
				if err != nil {
					e.logf(LevelWarn, "the settings file has an error, keeping the current settings: %v", err)
					continue
				}
				res, err := e.Reconfigure(next)
				if err != nil {
					e.logf(LevelWarn, "the edited settings were rejected, keeping the current ones: %v", err)
					continue
				}
				if res.Changed() {
					// Reconfigure saves the normalized form, so pick that up as
					// the new baseline instead of reloading it as a fresh edit.
					lastSeen, _ = os.ReadFile(path)
				}
			}
		}
	}()

	var stopped bool
	return func() {
		if !stopped {
			stopped = true
			close(done)
		}
	}
}
