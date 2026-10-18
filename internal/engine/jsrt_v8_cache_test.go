package engine

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestV8CodeCacheOptInRoundTripAndDamage(t *testing.T) {
	o := v8Opts(t)
	v8NoCodeCache = false // the code cache is opt-in (SWEEF_V8_CODECACHE=on)
	t.Cleanup(func() { v8NoCodeCache = true })
	o.CacheDir = t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("/p", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><body><script src="/big.js"></script></body></html>`))
	})
	mux.HandleFunc("/big.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("/*" + strings.Repeat("x", 400) + "*/ function f(a){return a*2} document.title='cc'+f(21);"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	visit := func() string {
		e := New(o)
		defer e.Close()
		tab, err := e.NewTab(srv.URL + "/p")
		if err != nil {
			t.Fatal(err)
		}
		if got := v8eval(t, tab, `document.title`); got != "cc42" {
			t.Fatalf("script did not run: %q", got)
		}
		requireV8(t, tab)
		st := tab.js.bcState
		// the producer works in the background: give it time before the engine (and its worker) goes away
		for i := 0; i < 100 && e.v8code.dir != ""; i++ {
			if ents, _ := os.ReadDir(e.v8code.dir); len(ents) > 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		return st
	}
	if st := visit(); st != "miss" {
		t.Fatalf("first visit: %q", st)
	}
	if st := visit(); st != "hit" {
		t.Fatalf("second visit: %q (the cache the first visit produced was not used)", st)
	}
	dirs, _ := os.ReadDir(o.CacheDir)
	for _, d := range dirs {
		if !strings.HasPrefix(d.Name(), "v8code-") {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(o.CacheDir, d.Name()))
		for _, f := range files {
			p := filepath.Join(o.CacheDir, d.Name(), f.Name())
			b, _ := os.ReadFile(p)
			for i := len(b) / 2; i < len(b)/2+64 && i < len(b); i++ {
				b[i] ^= 0xff
			}
			_ = os.WriteFile(p, b, 0o600)
		}
	}
	if st := visit(); st != "miss" {
		t.Fatalf("a damaged cache must be ignored, got %q", st)
	}
}
