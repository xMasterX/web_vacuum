package crawl

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forumServer imitates the shape of an old board: a session id welded onto
// every link, pages addressed by query string, and attachments served from a
// script with no file extension anywhere in the URL.
func forumServer(t *testing.T) *httptest.Server {
	t.Helper()
	const sid = "165b18c64b861e0a715b0a04ef7c4224"
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body>
<a href="/forumdisplay.php?s=%s&f=33">Forum 33</a>
<a href="/forumdisplay.php?s=%s&f=8">Forum 8</a>
</body></html>`, sid, sid)
	})
	mux.HandleFunc("/forumdisplay.php", func(w http.ResponseWriter, r *http.Request) {
		f := r.URL.Query().Get("f")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><h1>Forum %s</h1>
<a href="/showthread.php?s=%s&t=%s01">A thread</a>
<a href="/?s=%s">Back</a>
</body></html>`, f, sid, f, sid)
	})
	mux.HandleFunc("/showthread.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><p>a post</p>
<a href="/attachment.php?s=%s&attachmentid=91">photo.jpg</a>
<img src="/attachment.php?s=%s&attachmentid=92&thumb=1">
<a href="/attachment.php?s=%s&attachmentid=93">manual.pdf</a>
</body></html>`, sid, sid, sid)
	})
	mux.HandleFunc("/attachment.php", func(w http.ResponseWriter, r *http.Request) {
		// Attachments carry no extension in the URL; only the response says
		// what they are, and old boards often add Content-Disposition too.
		switch r.URL.Query().Get("attachmentid") {
		case "93":
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition", `attachment; filename="manual.pdf"`)
			w.Write([]byte("%PDF-1.4 fake"))
		default:
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("\xff\xd8\xff fake jpeg"))
		}
	})
	return srv
}

// TestForumLinksResolveLocallyWithSessionIDs is the failure that makes a mirror
// worthless: pages are downloaded, but every link still points at the live site
// because the link carries a session id the crawler stripped before queueing.
func TestForumLinksResolveLocallyWithSessionIDs(t *testing.T) {
	srv := forumServer(t)
	dest := t.TempDir()

	cfg := testConfig(t, srv.URL+"/", dest)
	cfg.Scope.DropQueryParams = []string{"s", "sid", "PHPSESSID"}
	e := runEngine(t, cfg)
	defer e.Close()

	home, err := os.ReadFile(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(home)

	if strings.Contains(body, srv.URL) {
		t.Errorf("the home page still links to the live site:\n%s", body)
	}
	for _, want := range []string{
		`href="./forumdisplay.php@f=33.html"`,
		`href="./forumdisplay.php@f=8.html"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home page missing %s\n%s", want, body)
		}
	}

	// And the same has to hold deeper in, where the session id also appeared.
	forum, err := os.ReadFile(filepath.Join(dest, "forumdisplay.php@f=33.html"))
	if err != nil {
		t.Fatalf("forum page missing: %v", err)
	}
	if strings.Contains(string(forum), srv.URL) {
		t.Errorf("a forum page still links to the live site:\n%s", forum)
	}
	if !strings.Contains(string(forum), `href="./index.html"`) {
		t.Errorf("the 'back' link was not localized:\n%s", forum)
	}
}

// TestAttachmentsAreDownloadedByDefault covers the files people actually want
// from a forum: the photos and documents attached to posts, which are served by
// a script and named only by the response headers.
func TestAttachmentsAreDownloadedByDefault(t *testing.T) {
	srv := forumServer(t)
	dest := t.TempDir()

	cfg := testConfig(t, srv.URL+"/", dest)
	cfg.Scope.DropQueryParams = []string{"s"}
	e := runEngine(t, cfg)
	defer e.Close()

	var found []string
	err := filepath.Walk(dest, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.Contains(path, ".webvacuum") {
			return nil
		}
		found = append(found, strings.TrimPrefix(path, dest+"/"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var jpegs, pdfs int
	for _, f := range found {
		switch {
		case strings.HasSuffix(f, ".jpg"):
			jpegs++
		case strings.HasSuffix(f, ".pdf"):
			pdfs++
		}
	}
	if jpegs < 2 {
		t.Errorf("expected the attached images, got %v", found)
	}
	if pdfs < 1 {
		t.Errorf("expected the attached document, got %v", found)
	}

	// The saved thread must point at those local files, not back at the board.
	thread, err := os.ReadFile(filepath.Join(dest, "showthread.php@t=3301.html"))
	if err != nil {
		t.Fatalf("thread page missing (found %v): %v", found, err)
	}
	if strings.Contains(string(thread), srv.URL) {
		t.Errorf("attachment links still point at the live site:\n%s", thread)
		// Dumping what the crawler recorded turns "a link is wrong" into the
		// specific mismatch between the key a link resolves to and the key a
		// file was stored under.
		for _, en := range e.Store().Select(nil) {
			t.Logf("  %-8s alias=%-5v path=%-40q key=%s err=%q", en.Status, en.Alias, en.Path, en.Key, en.Err)
		}
	}
}

// TestAttachmentsCanBeTurnedOff shows the setting is there when a text-only
// copy of a huge board is what you want.
func TestAttachmentsCanBeTurnedOff(t *testing.T) {
	srv := forumServer(t)
	dest := t.TempDir()

	cfg := testConfig(t, srv.URL+"/", dest)
	cfg.Scope.DropQueryParams = []string{"s"}
	cfg.Types.Mode = "disallow"
	cfg.Types.Categories = []string{"images", "documents"}
	e := runEngine(t, cfg)
	defer e.Close()

	err := filepath.Walk(dest, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.Contains(path, ".webvacuum") {
			return nil
		}
		if strings.HasSuffix(path, ".jpg") || strings.HasSuffix(path, ".pdf") {
			t.Errorf("attachment downloaded despite being disallowed: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRedirectTargetsResolveLocally covers a way links quietly stayed pointing
// at the live site: a page fetched through a redirect is saved under the name
// of where it landed, while the crawler recorded it under what was asked for.
// A link elsewhere on the site pointing straight at the landing URL then found
// nothing, and was rewritten back to the internet even though the file was
// sitting right there.
func TestRedirectTargetsResolveLocally(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		// The first link goes through a redirect; the second names the landing
		// page directly. Both have to end up pointing at the same local file.
		fmt.Fprint(w, `<html><body>
<a href="/go?to=thread">via redirect</a>
<a href="/thread.html">direct</a>
</body></html>`)
	})
	mux.HandleFunc("/go", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/thread.html", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/thread.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><h1>the thread</h1></body></html>`)
	})

	dest := t.TempDir()
	e := runEngine(t, testConfig(t, srv.URL+"/", dest))
	defer e.Close()

	home, err := os.ReadFile(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(home)
	if strings.Contains(body, srv.URL) {
		t.Errorf("a link was left pointing at the live site:\n%s", body)
	}
	if !strings.Contains(body, `href="./thread.html"`) {
		t.Errorf("the direct link should resolve to the downloaded file:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(dest, "thread.html")); err != nil {
		t.Errorf("the redirect target was not saved: %v", err)
	}

	// Aliases point at a file counted once; they must not inflate the totals.
	st := e.Store().Stats()
	if st.Done > 3 {
		t.Errorf("alias entries leaked into the counts: %d done", st.Done)
	}
}
