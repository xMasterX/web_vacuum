package crawl

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// TestAURLLinkedFromManyPagesIsDownloadedOnce covers a race that does not look
// like one from the outside.
//
// Deciding whether a URL is new used to be a lookup followed, separately, by an
// insert. On a forum every page links to the same handful of things — the logo,
// the stylesheet, the same attachment quoted in two threads — and several
// workers scan several pages at the same moment. Two of them could both find no
// record and both queue it.
//
// The visible result was not a duplicate download. It was a missing file: both
// downloads wrote to one path, and the one that finished second renamed a
// temporary file the first had already moved, failed, and recorded the URL as
// failed — so the pages linking to it kept pointing at the live site.
//
// The pages here are all released at the same instant, so the workers scan them
// together and every shared link is discovered many times at once.
func TestAURLLinkedFromManyPagesIsDownloadedOnce(t *testing.T) {
	const pages, shared = 24, 24

	var hits sync.Map // url -> *atomic.Int32
	var barrier sync.WaitGroup
	barrier.Add(pages)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>")
		for i := 0; i < pages; i++ {
			fmt.Fprintf(w, `<a href="/page/%d.html">p%d</a>`, i, i)
		}
		fmt.Fprint(w, "</body></html>")
	})
	mux.HandleFunc("/page/", func(w http.ResponseWriter, r *http.Request) {
		// Hold every page until they can all be served together, so the workers
		// scan them at the same moment and race to queue the shared links.
		barrier.Done()
		barrier.Wait()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>")
		for i := 0; i < shared; i++ {
			fmt.Fprintf(w, `<img src="/shared/%d.gif">`, i)
		}
		fmt.Fprint(w, "</body></html>")
	})
	mux.HandleFunc("/shared/", func(w http.ResponseWriter, r *http.Request) {
		c, _ := hits.LoadOrStore(r.URL.Path, &atomic.Int32{})
		c.(*atomic.Int32).Add(1)
		w.Header().Set("Content-Type", "image/gif")
		w.Write([]byte("GIF89a"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig(t, srv.URL+"/", t.TempDir())
	// Every page has to be in flight at once for the barrier to release.
	cfg.General.Connections = pages
	cfg.Request.PerHostConns = pages

	runEngine(t, cfg)

	worst, worstURL := int32(0), ""
	hits.Range(func(k, v any) bool {
		if n := v.(*atomic.Int32).Load(); n > worst {
			worst, worstURL = n, k.(string)
		}
		return true
	})
	if worst > 1 {
		t.Errorf("%s was downloaded %d times; a URL linked from many pages must be queued once", worstURL, worst)
	}
	if worst == 0 {
		t.Error("the shared files were never downloaded; the test proved nothing")
	}
}
