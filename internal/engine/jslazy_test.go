package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const lazySSR = `<!doctype html><html><head><title>served</title></head><body>
<h1>Docs</h1><p>%s</p>
<a href="/a">One</a> <a href="/b">Two</a> <a href="/c">Three</a>
<button id="b">Go</button>
<script src="/app.js"></script></body></html>`

// the bundle arrives after the first-frame step, as a framework bundle does
const lazyBundle = `document.title = 'hydrated';
document.getElementById('b').addEventListener('click', function () { document.title = 'clicked'; });`

const lazyShell = `<!doctype html><html><head><title>shell</title></head><body><div id="root"></div>
<script>document.getElementById('root').innerHTML = '<h1>Rendered by script</h1>'; document.title = 'rendered';</script></body></html>`

func lazyTab(t *testing.T, mode, page string) *Tab {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app.js" {
			time.Sleep(150 * time.Millisecond)
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprint(w, lazyBundle)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if strings.Contains(page, "%s") {
			fmt.Fprintf(w, page, strings.Repeat("Server rendered text that an agent reads without any script. ", 12))
			return
		}
		fmt.Fprint(w, page)
	}))
	t.Cleanup(srv.Close)
	o := DefaultOptions()
	o.CacheDir = "off"
	o.JSMode = mode
	e := New(o)
	t.Cleanup(func() { e.Close() })
	tab, err := e.NewTab(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

// A server-rendered page is returned without running its scripts, reads leave it that way, and the first action
// (or an evaluate) runs them before it goes on.
func TestLazyJSDefersAServerRenderedPage(t *testing.T) {
	tab := lazyTab(t, "lazy", lazySSR)
	if !tab.JSDeferred() {
		t.Fatal("a server-rendered page was not deferred")
	}
	if got := tab.Title(); got != "served" {
		t.Errorf("title %q before the scripts ran, want the served one", got)
	}
	if y, err := tab.AriaSnapshotV2(SnapshotOptions{}); err != nil || !strings.Contains(y, "Server rendered text") || !strings.Contains(y, `link "One"`) {
		t.Fatalf("snapshot of the served page: %v\n%s", err, y)
	}
	if !tab.JSDeferred() {
		t.Error("a snapshot woke the page")
	}
	if err := tab.Click("#b"); err != nil {
		t.Fatal(err)
	}
	if tab.JSDeferred() {
		t.Error("a click did not run the held scripts")
	}
	if got := tab.Title(); got != "clicked" {
		t.Errorf("title %q after the click, want the handler the held script installed (\"clicked\")", got)
	}
}

func TestLazyJSEvaluateAndScreenshotWakeThePage(t *testing.T) {
	tab := lazyTab(t, "lazy", lazySSR)
	if v := mustEval(t, tab, `document.title`); v != `"hydrated"` {
		t.Errorf("evaluate saw %s, want the page after its scripts ran", v)
	}
	shot := lazyTab(t, "lazy", lazySSR)
	_ = shot.PNGScreenshot(0, 0, 1)
	if shot.JSDeferred() {
		t.Error("a screenshot of a deferred page did not wake it")
	}
}

// A client-rendered shell has nothing to read until its scripts run, so they are not deferred; and the default mode never defers.
func TestLazyJSLeavesShellsAndTheDefaultAlone(t *testing.T) {
	shell := lazyTab(t, "lazy", lazyShell)
	if shell.JSDeferred() || shell.Title() != "rendered" {
		t.Errorf("client-rendered shell: deferred=%v title=%q, want its scripts run at load", shell.JSDeferred(), shell.Title())
	}
	eager := lazyTab(t, "", lazySSR)
	if eager.JSDeferred() || eager.Title() != "hydrated" {
		t.Errorf("default mode: deferred=%v title=%q, want the scripts run at load", eager.JSDeferred(), eager.Title())
	}
}

// Navigating away from a deferred page drops the held load.
func TestLazyJSNavigationClearsTheHold(t *testing.T) {
	tab := lazyTab(t, "lazy", lazySSR)
	if !tab.JSDeferred() {
		t.Fatal("not deferred")
	}
	if err := tab.Navigate("about:blank", nil); err != nil {
		t.Fatal(err)
	}
	if tab.JSDeferred() {
		t.Error("the hold outlived the document")
	}
	time.Sleep(20 * time.Millisecond)
}
