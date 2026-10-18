package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sweefbrowser/sweef/internal/v8shim"
)

// v8Opts returns options that run pages on V8, or skips the test when the library is not available
// (SWEEF_V8_LIB, or next to the test binary). SWEEF_V8_REQUIRE=1 turns the skip into a failure.
func v8Opts(t *testing.T) Options {
	t.Helper()
	if err := v8shim.Load(); err != nil {
		if os.Getenv("SWEEF_V8_REQUIRE") != "" {
			t.Fatal(err)
		}
		t.Skip("V8 library not available: ", err)
	}
	o := DefaultOptions()
	o.JSEngine = JSEngineV8
	return o
}

func v8Tab(t *testing.T, o Options, url string) (*Engine, *Tab) {
	t.Helper()
	e := New(o)
	t.Cleanup(func() { e.Close() })
	tab, err := e.NewTab(url)
	if err != nil {
		t.Fatalf("navigate %s: %v", url, err)
	}
	if url != "about:blank" {
		requireV8(t, tab)
	}
	return e, tab
}

func requireV8(t *testing.T, tab *Tab) {
	t.Helper()
	if tab.js == nil || tab.js.jr.kind() != JSEngineV8 {
		t.Fatalf("tab is not on the V8 runtime")
	}
}

func v8eval(t *testing.T, tab *Tab, expr string) string {
	t.Helper()
	b, err := tab.Evaluate(expr)
	if err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		return s
	}
	return string(b)
}

func v8Server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	js := func(path, body string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = w.Write([]byte(body))
		})
	}
	page := func(path, body string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(body))
		})
	}
	page("/mods", `<!doctype html><html><body><p id="o"></p>
<script type="module">
import { a, metaUrl } from './lib/a.js';
import dflt, * as ns from './lib/b.js';
const dyn = await import('./lib/c.js');
document.getElementById('o').textContent = [a, dflt, ns.named, metaUrl.split('/').slice(-2).join('/'), dyn.c, import.meta.url.split('/').pop()].join('|');
</script>
<script>window.classicRan = typeof document;</script></body></html>`)
	js("/lib/a.js", `import { b } from './b.js'; export const a = 'A' + b; export const metaUrl = import.meta.url;`)
	js("/lib/b.js", `export const b = 'B'; export const named = 'N'; export default 'D';`)
	js("/lib/c.js", `export const c = 'C-' + (await Promise.resolve('tla'));`)
	page("/reject", `<!doctype html><html><body><script>
window.seen = [];
window.addEventListener('unhandledrejection', function (e) { window.seen.push(String(e.reason)); e.preventDefault(); });
Promise.reject('boom');
Promise.reject('caught-in-time').catch(function () {});
Promise.resolve().then(function () { return Promise.reject('chained'); });
</script></body></html>`)
	page("/timers", `<!doctype html><html><body><script>
window.log = [];
setTimeout(function () { log.push('t0'); }, 0);
setTimeout(function () { log.push('t20'); }, 20);
Promise.resolve().then(function () { log.push('micro'); });
queueMicrotask(function () { log.push('qm'); });
requestAnimationFrame(function () { log.push('raf'); });
log.push('sync');
</script></body></html>`)
	page("/loop", `<!doctype html><html><head><title>Loop</title></head><body><p>alive</p><script>while (true) {}</script><script>document.title = 'after';</script></body></html>`)
	page("/oom", `<!doctype html><html><head><title>Oom</title></head><body><p>alive</p><script>var keep = []; for (;;) keep.push(new Array(1e6).fill(1));</script><script>document.title = 'after';</script></body></html>`)
	page("/big", `<!doctype html><html><body><script>
fetch('/bigjson').then(function (r) { return r.arrayBuffer(); }).then(function (ab) {
  var s = new TextDecoder().decode(ab); var o = JSON.parse(s);
  document.title = 'n=' + o.items.length + ' bytes=' + ab.byteLength + ' last=' + o.items[o.items.length - 1].t;
});
</script></body></html>`)
	mux.HandleFunc("/bigjson", func(w http.ResponseWriter, r *http.Request) {
		var sb strings.Builder
		sb.WriteString(`{"items":[`)
		for i := 0; i < 20000; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(`{"i":` + itoa(i) + `,"t":"é€ item ` + itoa(i) + `"}`)
		}
		sb.WriteString(`]}`)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(sb.String()))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestV8ModulesImportMetaAndDynamicImport(t *testing.T) {
	o := v8Opts(t)
	srv := v8Server(t)
	_, tab := v8Tab(t, o, srv.URL+"/mods")
	tab.Settle(500 * time.Millisecond)
	got := v8eval(t, tab, `document.getElementById('o').textContent`)
	if got != "AB|D|N|lib/a.js|C-tla|mods" {
		t.Fatalf("module graph result %q", got)
	}
	if v := v8eval(t, tab, `window.classicRan`); v != "object" {
		t.Fatalf("classic script after a module: %q", v)
	}
}

func TestV8UnhandledRejectionEvents(t *testing.T) {
	o := v8Opts(t)
	srv := v8Server(t)
	_, tab := v8Tab(t, o, srv.URL+"/reject")
	tab.Settle(300 * time.Millisecond)
	got := v8eval(t, tab, `window.seen.join(',')`)
	if !strings.Contains(got, "boom") || !strings.Contains(got, "chained") {
		t.Fatalf("unhandledrejection events: %q", got)
	}
	if strings.Contains(got, "caught-in-time") {
		t.Fatalf("a rejection handled in time was reported: %q", got)
	}
}

func TestV8TimersAndMicrotaskOrder(t *testing.T) {
	o := v8Opts(t)
	srv := v8Server(t)
	_, tab := v8Tab(t, o, srv.URL+"/timers")
	tab.Settle(500 * time.Millisecond)
	got := v8eval(t, tab, `log.join(',')`)
	if !strings.HasPrefix(got, "sync,micro,qm,") || !strings.Contains(got, "t0") || !strings.HasSuffix(got, "t20") {
		t.Fatalf("event loop order %q", got)
	}
}

func TestV8InfiniteLoopAndHeapLimitDoNotKillTheTab(t *testing.T) {
	o := v8Opts(t)
	o.JSTimeoutMs = 300
	o.JSMemoryMiB = 48
	srv := v8Server(t)
	_, tab := v8Tab(t, o, srv.URL+"/loop")
	if got := v8eval(t, tab, `document.title`); got != "after" {
		t.Fatalf("script after an interrupted loop did not run: %q", got)
	}
	_, tab2 := v8Tab(t, o, srv.URL+"/oom")
	// the heap limit ends the script (and the isolate); the page's text must still be readable
	if txt, err := tab2.Text("body"); err != nil || !strings.Contains(txt, "alive") {
		t.Fatalf("page text after the heap limit: %q %v", txt, err)
	}
}

func TestV8LargeBodiesCrossTheBridgeIntact(t *testing.T) {
	o := v8Opts(t)
	srv := v8Server(t)
	_, tab := v8Tab(t, o, srv.URL+"/big")
	tab.Settle(2 * time.Second)
	got := v8eval(t, tab, `document.title`)
	if !strings.HasPrefix(got, "n=20000 bytes=") || !strings.HasSuffix(got, "last=é€ item 19999") {
		t.Fatalf("large fetch body: %q", got)
	}
}

func TestV8HostValuesRoundTrip(t *testing.T) {
	o := v8Opts(t)
	_, tab := v8Tab(t, o, "about:blank")
	tab.SetDocument(`<!doctype html><body><div id="d"></div></body>`, "https://example.test/")
	v8eval(t, tab, `1`) // the VM is created on first use
	requireV8(t, tab)
	checks := map[string]string{
		`(function(){ var d = document.getElementById('d'); var s = '€'.repeat(100000) + '\u{1F600}'; d.setAttribute('data-x', s); return d.getAttribute('data-x') === s })()`: "true",
		`(function(){ var d = document.getElementById('d'); d.textContent = 'a\u0000b'; return d.textContent.length })()`:                                                      "3",
		`(function(){ localStorage.setItem('k', 'v'.repeat(500000)); return localStorage.getItem('k').length })()`:                                                             "500000",
		`(function(){ var e = document.createElement('span'); e.id = 'x'; document.body.appendChild(e); return document.getElementById('x') === e })()`:                        "true",
		`(function(){ try { __sw.nodeType(); return 'ok' } catch (e) { return e.name } })()`:                                                                                   "ok",
		`(function(){ var u = new TextEncoder().encode('héllo \u{1F600}'); return Array.from(u).join(',') })()`:                                                                "104,195,169,108,108,111,32,240,159,152,128",
		`(function(){ function f(n) { return n ? f(n - 1) + 1 : 0 } try { return f(1e7) } catch (e) { return e instanceof RangeError } })()`:                                   "true",
		`new TextDecoder().decode(new Uint8Array([104,195,169,0xff,111]))`:                                                                                                     "hé�o",
	}
	for expr, want := range checks {
		if got := v8eval(t, tab, expr); got != want {
			t.Errorf("%s\n got %q want %q", expr, got, want)
		}
	}
}

func TestV8EvaluateShapes(t *testing.T) {
	o := v8Opts(t)
	_, tab := v8Tab(t, o, "about:blank")
	tab.SetDocument(`<!doctype html><body><p id="p">hi</p></body>`, "https://example.test/")
	v8eval(t, tab, `1`) // the VM is created on first use
	requireV8(t, tab)
	cases := map[string]string{
		`1 + 2`:                    `3`,
		`'str'`:                    `"str"`,
		`({a: [1, 2, {b: null}]})`: `{"a":[1,2,{"b":null}]}`,
		`undefined`:                `null`,
		`await Promise.resolve(7)`: `7`,
		`(async () => { await 0; return 'x' })()`:                                  `"x"`,
		`new Promise(function (r) { setTimeout(function () { r('later') }, 30) })`: `"later"`,
		`let q = 5; q * 2`: `10`,
	}
	for expr, want := range cases {
		b, err := tab.Evaluate(expr)
		if err != nil || string(b) != want {
			t.Errorf("evaluate %q = %s, %v; want %s", expr, b, err, want)
		}
	}
	if _, err := tab.Evaluate(`document.getElementById('p').foo.bar`); err == nil {
		t.Error("a TypeError must be reported")
	}
	if _, err := tab.Evaluate(`Promise.reject(new Error('nope'))`); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("a rejected promise must be reported with its message: %v", err)
	}
	if _, err := tab.Evaluate(`this is not javascript`); err == nil || !strings.Contains(err.Error(), "SyntaxError") {
		t.Errorf("a syntax error must be reported: %v", err)
	}
}

// A snapshot-less isolate must not share a process with isolates on a snapshot (v8shim.Options), so this runs
// in a child process of its own.
func TestV8BootsWithoutASnapshot(t *testing.T) {
	if os.Getenv("SWEEF_V8_CHILD") == "" {
		o := v8Opts(t)
		_ = o
		cmd := exec.Command(os.Args[0], "-test.run=^TestV8BootsWithoutASnapshot$", "-test.count=1")
		cmd.Env = append(os.Environ(), "SWEEF_V8_CHILD=1", "SWEEF_V8_SNAPSHOT=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		return
	}
	o := v8Opts(t)
	_, tab := v8Tab(t, o, "about:blank")
	tab.SetDocument(`<!doctype html><body><h1>x</h1></body>`, "https://example.test/")
	v8eval(t, tab, `1`)
	requireV8(t, tab)
	if got := v8eval(t, tab, `document.querySelector('h1').textContent + typeof __swSetDocument`); got != "xfunction" {
		t.Fatalf("snapshot-less boot: %q", got)
	}
	if v8Active.Load() == nil || *v8Active.Load() != nil {
		t.Fatalf("the child must have run without a snapshot")
	}
}

func TestV8UnsupportedFeaturesFailSoftly(t *testing.T) {
	o := v8Opts(t)
	_, tab := v8Tab(t, o, "about:blank")
	tab.SetDocument(`<!doctype html><body><canvas id="c" width="4" height="4"></canvas></body>`, "https://example.test/")
	v8eval(t, tab, `1`) // the VM is created on first use
	requireV8(t, tab)
	// none of these may panic or wedge the VM; each is either supported by V8 itself or reports an error
	for _, expr := range []string{
		`typeof WebAssembly.instantiate`,
		`typeof SharedArrayBuffer`,
		`(function(){ var c = document.getElementById('c').getContext('2d'); c.fillStyle = '#f00'; c.fillRect(0, 0, 4, 4); return c.getImageData(0, 0, 1, 1).data[0] })()`,
		`typeof Worker`,
	} {
		if _, err := tab.Evaluate(expr); err != nil {
			t.Logf("%s -> %v (allowed: reported, not crashed)", expr, err)
		}
	}
	if got := v8eval(t, tab, `1 + 1`); got != "2" {
		t.Fatalf("VM unusable after touching unsupported features: %q", got)
	}
}
