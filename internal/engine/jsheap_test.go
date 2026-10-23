package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// bigJSON builds a JSON array of about n bytes of small objects, the shape of the data tables
// that large documentation sites ship and parse in the page (MDN's browser-compat data).
func bigJSON(n int) []byte {
	var sb strings.Builder
	sb.Grow(n + 256)
	sb.WriteByte('[')
	for i := 0; sb.Len() < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"id":%d,"name":"feature-%d","browser":"chrome","version_added":"%d","flags":[],"notes":["row %d"],"support":{"a":true,"b":null}}`, i, i, i%120, i)
	}
	sb.WriteByte(']')
	return []byte(sb.String())
}

func heapServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/data.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(payload)
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><head><title>big</title></head><body><h1 id="h">loading</h1><div id="out"></div><script>
fetch('/data.json').then(r => r.text()).then(txt => {
  const rows = JSON.parse(txt);
  // keep a derived copy alive too, as table renderers do
  const byName = {};
  for (const r of rows) byName[r.name] = r;
  document.getElementById('h').textContent = 'rows ' + rows.length;
  const last = rows[rows.length - 1];
  document.getElementById('out').textContent = 'last ' + last.name + ' ' + Object.keys(byName).length;
}).catch(e => { document.getElementById('h').textContent = 'ERR ' + e; });
</script></body></html>`)
	})
	mux.HandleFunc("/runaway", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><head><title>runaway</title></head><body><p id="p">before</p><script>
var keep = [];
try { for (;;) keep.push(new Array(100000).fill(1.5)); } catch (e) { window.caught = String(e); }
document.title = 'survived';
</script></body></html>`)
	})
	mux.HandleFunc("/strings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><head><title>strings</title></head><body><script>
var o = {};
for (var i = 0; ; i++) o['key' + i] = { a: i, b: 'v' + i };
</script></body></html>`)
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><head><title>fine</title></head><body><p>hello</p></body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func heapEngine(t *testing.T, mutate func(*Options)) *Engine {
	t.Helper()
	if _, err := os.Stat("dom_prelude.js"); err != nil {
		t.Skip("prelude missing")
	}
	o := DefaultOptions()
	o.SettleMs = 4000
	if mutate != nil {
		mutate(&o)
	}
	e := New(o)
	t.Cleanup(func() { e.Close() })
	return e
}

func pageText(tab *Tab) string {
	y, _ := tab.AriaSnapshotYAML()
	return y
}

// A page that downloads and parses a ~40 MB JSON array must render at the default heap cap. At the
// old 256 MiB cap the same page came out as "InternalError: out of memory" (the MDN compat tables did).
func TestJSLargeJSONRendersAtDefaultCap(t *testing.T) {
	if DefaultJSMemoryMiB < 1024 {
		t.Fatalf("default JS heap cap %d MiB is below the 1 GiB the large-page work needs", DefaultJSMemoryMiB)
	}
	rows := bigJSON(40 << 20)
	srv := heapServer(t, rows)
	e := heapEngine(t, nil)
	tab, err := e.NewTab(srv.URL + "/big")
	if err != nil {
		t.Fatalf("nav: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	var y string
	for time.Now().Before(deadline) {
		y = pageText(tab)
		if strings.Contains(y, "last feature-") || strings.Contains(y, "ERR") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if strings.Contains(y, "out of memory") || strings.Contains(consoleText(tab), "out of memory") {
		t.Fatalf("out of memory at the default cap:\n%s\n%s", y, consoleText(tab))
	}
	if !strings.Contains(y, "rows ") || !strings.Contains(y, "last feature-") {
		t.Fatalf("content not rendered:\n%s\nconsole:\n%s", y, consoleText(tab))
	}
}

// The cap is the guard: the same page at a deliberately small cap (64 MiB; the old 256 MiB default also failed it) fails cleanly (the tab stays usable)
// instead of taking the process down.
func TestJSLargeJSONHitsAnExplicitSmallCap(t *testing.T) {
	srv := heapServer(t, bigJSON(40<<20))
	e := heapEngine(t, func(o *Options) { o.JSMemoryMiB = 64 })
	tab, err := e.NewTab(srv.URL + "/big")
	if err != nil {
		t.Fatalf("nav: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	y := pageText(tab)
	if strings.Contains(y, "last feature-") {
		t.Fatalf("a 64 MiB cap did not stop a ~40 MB JSON parse:\n%s", y)
	}
	if _, err := e.NewTab(srv.URL + "/ok"); err != nil {
		t.Fatalf("engine unusable after the cap was hit: %v", err)
	}
}

// A script that allocates in a loop is stopped (by the heap cap or the script timeout, whichever is
// first at the default size), its tab stays readable, and other tabs and later navigations are
// unaffected. The page's own JavaScript is not expected to recover: once the heap is full the VM
// cannot even run a catch block.
func TestJSRunawayAllocationIsStopped(t *testing.T) {
	srv := heapServer(t, []byte("[]"))
	e := heapEngine(t, func(o *Options) { o.JSTimeoutMs = 5000 })
	other, err := e.NewTab(srv.URL + "/ok")
	if err != nil {
		t.Fatalf("nav: %v", err)
	}
	for _, path := range []string{"/runaway", "/strings"} {
		start := time.Now()
		tab, err := e.NewTab(srv.URL + path)
		if err != nil {
			t.Fatalf("%s: nav: %v", path, err)
		}
		d := time.Since(start)
		t.Logf("%s stopped after %v", path, d.Round(10*time.Millisecond))
		if d > 25*time.Second {
			t.Fatalf("%s: allocation loop took %v to stop", path, d)
		}
		if c := consoleText(tab); !strings.Contains(c, `"level":"error"`) {
			// with enough headroom left, the page's own catch block runs: the out-of-memory error reached the script
			if path != "/runaway" || !strings.Contains(mustEval(t, tab, `String(window.caught)`), "out of memory") {
				t.Fatalf("%s: the stopped script was not reported as an error:\n%s", path, c)
			}
		}
		if path == "/runaway" && !strings.Contains(pageText(tab), "before") {
			t.Fatalf("%s: document unreadable after the failed script:\n%s", path, pageText(tab))
		}
	}
	if other.Title() != "fine" || !strings.Contains(pageText(other), "hello") {
		t.Fatalf("unrelated tab changed: %q\n%s", other.Title(), pageText(other))
	}
	if got := mustEval(t, other, `document.querySelector('p').textContent`); got != `"hello"` {
		t.Fatalf("unrelated tab unusable: %s", got)
	}
	if tab, err := e.NewTab(srv.URL + "/ok"); err != nil || tab.Title() != "fine" {
		t.Fatalf("engine unusable after runaways: %v", err)
	}
}
