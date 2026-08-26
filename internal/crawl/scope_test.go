package crawl

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/xMasterX/web_vacuum/internal/config"
)

// Note: the none/related/any distinction is exercised in internal/rules, where
// real domain names can be used. Every httptest server shares the hostname
// 127.0.0.1, so "a different domain" cannot be expressed here.

func TestHostHealthWritesOffDeadHostsOnly(t *testing.T) {
	h := newHostHealth()

	// A host that never answers is written off after the threshold.
	if h.failure("dead.example", "unreachable") {
		t.Error("one failure should not be enough to write a host off")
	}
	if !h.failure("dead.example", "unreachable") {
		t.Error("the second failure should write the host off")
	}
	if dead, _ := h.dead("dead.example"); !dead {
		t.Error("host should be marked dead")
	}
	// Once written off it stays written off without re-reporting.
	if h.failure("dead.example", "unreachable") {
		t.Error("an already-dead host should not be reported again")
	}

	// A host that has worked is never written off, however badly it behaves.
	h.success("flaky.example")
	for i := 0; i < 20; i++ {
		if h.failure("flaky.example", "unreachable") {
			t.Fatal("a host that has answered before must not be written off")
		}
	}
	if dead, _ := h.dead("flaky.example"); dead {
		t.Error("flaky host should not be dead")
	}

	// And a success clears a host that was on its way out.
	h.failure("recovering.example", "unreachable")
	h.success("recovering.example")
	if dead, _ := h.dead("recovering.example"); dead {
		t.Error("a host that answered should be cleared")
	}
	if h.deadCount() != 1 {
		t.Errorf("deadCount = %d, want 1", h.deadCount())
	}
}

func TestSupportingFilesNoneKeepsEverythingOnSite(t *testing.T) {
	var offsite atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offsite.Add(1)
		w.Header().Set("Content-Type", "text/css")
		fmt.Fprint(w, "body{color:red}")
	}))
	defer cdn.Close()

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><head><link rel=stylesheet href="%s/x.css"></head><body>hi</body></html>`, cdn.URL)
	}))
	defer site.Close()

	cfg := testConfig(t, site.URL+"/", t.TempDir())
	cfg.General.SupportingFiles = config.SupportingNone
	e := runEngine(t, cfg)
	defer e.Close()

	if n := offsite.Load(); n != 0 {
		t.Errorf("supporting_files: none still fetched %d off-site file(s)", n)
	}
}
