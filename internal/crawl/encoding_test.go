package crawl

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var win1251 = []byte("\xd0\xf3\xf1\xf1\xea\xe8\xe9 \xf2\xe5\xea\xf1\xf2")

// TestCrawlingALegacyEncodedForum is the end-to-end version of the crash: a
// board served in windows-1251, large enough that any offset drift between the
// document and a re-encoded copy of it runs past the end of the buffer.
func TestCrawlingALegacyEncodedForum(t *testing.T) {
	cyr := string(win1251)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	page := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		fmt.Fprintf(w, `<html><head><meta http-equiv="Content-Type" content="text/html; charset=windows-1251">
<style>body{background:url(/bg.png)}</style></head><body>%s</body></html>`, body)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		var b bytes.Buffer
		// Enough legacy bytes that a three-times inflation would overshoot.
		for i := 0; i < 3000; i++ {
			b.WriteString(cyr)
			if i%100 == 0 {
				fmt.Fprintf(&b, `<a href="/thread.php?t=%d">%s</a>`, i, cyr)
				b.WriteString(`<style>.a{background:url(/i/x.gif)}</style>`)
				b.WriteString(`<script>var x=1;</script>`)
			}
		}
		page(w, b.String())
	})
	mux.HandleFunc("/thread.php", func(w http.ResponseWriter, r *http.Request) {
		page(w, cyr+`<a href="/">`+cyr+`</a><img src="/i/x.gif">`)
	})
	for _, img := range []string{"/bg.png", "/i/x.gif"} {
		mux.HandleFunc(img, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG\r\n\x1a\nX"))
		})
	}

	dest := t.TempDir()
	e := runEngine(t, testConfig(t, srv.URL+"/", dest))
	defer e.Close()

	st := e.Store().Stats()
	if st.Done < 5 {
		t.Fatalf("only %d files saved; the crawl did not get going", st.Done)
	}

	home, err := os.ReadFile(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(home, []byte(`href="./thread.php@t=0.html"`)) {
		t.Errorf("links were not rewritten in a legacy-encoded page")
	}
	// The archive must be byte-faithful outside the links it rewrote.
	if n := bytes.Count(home, win1251); n < 2000 {
		t.Errorf("legacy bytes were mangled: %d survived", n)
	}
	if bytes.Contains(home, []byte("�")) {
		t.Error("the page was re-encoded; replacement characters appeared")
	}
}

// TestJSDetectionHandlesLegacyEncoding covers the render heuristic, which also
// folds case over raw page bytes.
func TestJSDetectionHandlesLegacyEncoding(t *testing.T) {
	cyr := string(win1251)
	// A real page with plenty of text is not a JavaScript shell, whatever
	// encoding it is in.
	rich := "<html><body><script src=/a.js></script><article>" +
		strings.Repeat(cyr+" ", 200) + "</article></body></html>"
	if LooksJSDriven([]byte(rich)) {
		t.Error("a content-bearing legacy-encoded page was mistaken for a shell")
	}
	shell := `<html><body><div id="root"></div><script src="/app.js"></script></body></html>`
	if !LooksJSDriven([]byte(shell)) {
		t.Error("an empty mount point should still be detected")
	}
}
