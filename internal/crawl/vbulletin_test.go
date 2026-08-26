package crawl

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestVBulletinStyleForumLocalizes reproduces the shape of a real board: the
// site lives under a subdirectory, every link is relative and carries a session
// id, and the markup uses &amp; between parameters.
//
// A link left untouched is the worst outcome of the three possible ones. An
// absolute link still works; a relative one resolves against the folder it was
// opened from and goes nowhere at all.
func TestVBulletinStyleForumLocalizes(t *testing.T) {
	const sid = "7f3fb3360bc5c18fd30a918c2d592c07"
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Every link form a board of this vintage produces.
	index := fmt.Sprintf(`<!doctype html>
<html><head><title>Forums</title></head><body>
<a href="forumdisplay.php?s=%[1]s&amp;f=8">Relative with &amp;amp;</a>
<a href="forumdisplay.php?s=%[1]s&f=9">Relative with bare &amp;</a>
<a href="/forumaml/forumdisplay.php?s=%[1]s&amp;f=10">Site-absolute</a>
<a href='forumdisplay.php?s=%[1]s&amp;f=11'>Single quoted</a>
<a href="./forumdisplay.php?s=%[1]s&amp;f=12">Dot-slash prefixed</a>
<a href="%[2]s/forumaml/forumdisplay.php?s=%[1]s&amp;f=13">Fully qualified</a>
</body></html>`, sid, srv.URL)

	mux.HandleFunc("/forumaml/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		switch {
		case strings.HasSuffix(r.URL.Path, "/forumaml/"), strings.HasSuffix(r.URL.Path, "index.php"):
			fmt.Fprint(w, index)
		case strings.HasSuffix(r.URL.Path, "forumdisplay.php"):
			f := r.URL.Query().Get("f")
			fmt.Fprintf(w, `<html><body><h1>Forum %s</h1>
<a href="index.php?s=%s">Back to the index</a></body></html>`, f, sid)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/forumaml/", http.StatusFound)
	})

	dest := t.TempDir()
	cfg := testConfig(t, srv.URL+"/forumaml/", dest)
	cfg.Scope.DropQueryParams = []string{"s", "sid", "PHPSESSID"}
	e := runEngine(t, cfg)
	defer e.Close()

	var pages []string
	filepath.Walk(dest, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".html") && !strings.Contains(p, ".webvacuum") {
			pages = append(pages, strings.TrimPrefix(p, dest+"/"))
		}
		return nil
	})
	t.Logf("saved pages: %v", pages)

	// The start URL is a directory, so it is saved as that folder's index.
	home := filepath.Join(dest, "forumaml", "index.html")
	body, err := os.ReadFile(home)
	if err != nil {
		t.Fatalf("home page not found among %v: %v", pages, err)
	}
	html := string(body)

	// Any surviving session id means a link was never touched, because the
	// crawler strips that parameter from everything it queues.
	leftover := regexp.MustCompile(`href=["'][^"']*` + sid)
	if m := leftover.FindAllString(html, -1); len(m) > 0 {
		t.Errorf("%d link(s) were never rewritten:\n  %s\n---\n%s",
			len(m), strings.Join(m, "\n  "), html)
	}

	for f := 8; f <= 13; f++ {
		want := fmt.Sprintf("forumdisplay.php@f=%d.html", f)
		if !strings.Contains(html, want) {
			t.Errorf("no link to %s in the saved page", want)
		}
	}
}
