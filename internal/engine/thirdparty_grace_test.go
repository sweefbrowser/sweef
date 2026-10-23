package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A request to another site (error reporting, analytics) that hangs must not hold a headless load for the whole
// httpGrace; a request to the page's own site still does, because the page may need its answer.
func TestThirdPartyHangDoesNotHoldTheLoad(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, "{}")
	}))
	defer slow.Close()
	page := func(api string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<!doctype html><body><p>hello</p><script>fetch(%q).then(function(){ document.title = 'answered'; });</script>`, api)
		}))
	}
	load := func(url string) (time.Duration, string) {
		o := DefaultOptions()
		o.CacheDir = "off"
		o.NavSettleMs, o.NavPostLoadMs, o.NavQuietMs = 300, 100, 60
		e := New(o)
		defer e.Close()
		t0 := time.Now()
		tab, err := e.NewTab(url)
		if err != nil {
			t.Fatal(err)
		}
		return time.Since(t0), tab.Title()
	}

	// the page is on localhost, the slow API on 127.0.0.1: two sites
	pg := page(slow.URL + "/api")
	defer pg.Close()
	d, title := load(strings.Replace(pg.URL, "127.0.0.1", "localhost", 1))
	if d > 2*time.Second || title == "answered" {
		t.Errorf("third-party hang held the load for %v (title %q), want it released after ~%v", d, title, httpGraceThirdParty)
	}

	// the same API on the page's own site is waited for
	own := page(slow.URL + "/api")
	defer own.Close()
	d, title = load(own.URL)
	if d < 2500*time.Millisecond || title != "answered" {
		t.Errorf("first-party request: load took %v with title %q, want it to wait for the answer", d, title)
	}
}
