package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// The preload scanner follows the static imports of a module script as soon as its source arrives, not when the
// module runs: with a slow classic script ahead of it in the document, the whole graph is already downloaded by
// the time that script finishes.
func TestPreloadScannerFollowsModuleGraph(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]time.Time{}
	var graphSeenWhileHeld bool
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><head><script src="/slow.js"></script><script type="module" src="/a.js"></script></head><body>x</body></html>`)
	})
	mux.HandleFunc("/slow.js", func(w http.ResponseWriter, r *http.Request) {
		// hold the blocking script until the module graph has been requested (or give up)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			_, b := seen["/b.js"]
			_, c := seen["/c.js"]
			if b && c {
				graphSeenWhileHeld = true
			}
			mu.Unlock()
			if b && c {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "text/javascript")
		fmt.Fprint(w, "window.slowDone = 1;")
	})
	js := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[r.URL.Path] = time.Now()
			mu.Unlock()
			w.Header().Set("Content-Type", "text/javascript")
			fmt.Fprint(w, body)
		}
	}
	mux.HandleFunc("/a.js", js(`import { b } from "./b.js"; window.result = b + 1;`))
	mux.HandleFunc("/b.js", js(`import { c } from "./c.js"; export const b = c + 1;`))
	mux.HandleFunc("/c.js", js(`export const c = 1;`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := New(DefaultOptions())
	defer e.Close()
	tab, err := e.NewTab("about:blank")
	if err != nil {
		t.Fatal(err)
	}
	if err := tab.Navigate(srv.URL+"/", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	held := graphSeenWhileHeld
	mu.Unlock()
	if !held {
		t.Fatal("b.js and c.js were not requested while the blocking script was still loading: the module graph is only followed once the module runs")
	}
	got, err := tab.Evaluate(`window.result`)
	if err != nil || string(got) != "3" {
		t.Fatalf("window.result = %s, err %v (the module graph must still run)", got, err)
	}
}
