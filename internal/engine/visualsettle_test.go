package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func visualTab(t *testing.T, html string) *Tab {
	t.Helper()
	o := DefaultOptions()
	o.CacheDir = "off"
	e := New(o)
	t.Cleanup(func() { e.Close() })
	tab, err := e.NewTab("about:blank")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, html)
	}))
	t.Cleanup(srv.Close)
	if err := tab.Navigate(srv.URL+"/", nil); err != nil {
		t.Fatal(err)
	}
	return tab
}

// A page that reveals itself with a script animation (an opacity rewritten every frame, after a delay) is still
// in its intro when a headless load returns, because style writes do not hold that settle. SettleVisual waits it out.
func TestSettleVisualWaitsForAScriptAnimation(t *testing.T) {
	tab := visualTab(t, `<!doctype html><body><h1 id=h style="opacity:0">Hello</h1><script>
var started = Date.now();
setTimeout(function step() {
  var p = Math.min(1, (Date.now() - started - 400) / 700);
  if (p < 0) p = 0;
  document.getElementById('h').style.opacity = String(p);
  if (p < 1) requestAnimationFrame(step);
}, 400);
</script>`)
	t0 := time.Now()
	if !tab.SettleVisual(3*time.Second, 0) {
		t.Fatal("a finite animation never went still within 3 s")
	}
	if d := time.Since(t0); d > 2500*time.Millisecond {
		t.Errorf("waited %v for a 1.1 s animation", d)
	}
	if v := mustEval(t, tab, `document.getElementById('h').style.opacity`); v != `"1"` {
		t.Errorf("opacity after SettleVisual = %s, want the finished animation (\"1\")", v)
	}
}

// An endless animation (a spinner) never goes still: the wait gives up at its timeout and says so.
func TestSettleVisualGivesUpOnAnEndlessAnimation(t *testing.T) {
	tab := visualTab(t, `<!doctype html><body><div id=s>x</div><script>
var a = 0;
(function f() { a++; document.getElementById('s').style.transform = 'rotate(' + a + 'deg)'; requestAnimationFrame(f); })();
</script>`)
	t0 := time.Now()
	if tab.SettleVisual(600*time.Millisecond, 0) {
		t.Fatal("an endless animation reported as still")
	}
	if d := time.Since(t0); d < 550*time.Millisecond || d > 1500*time.Millisecond {
		t.Errorf("gave up after %v, want about the 600 ms timeout", d)
	}
}

// A page without script cannot animate, so a screenshot does not wait at all, and timeout 0 turns the wait off.
func TestSettleVisualIsFreeWhenNothingCanMove(t *testing.T) {
	tab := visualTab(t, `<!doctype html><body><p>static</p>`)
	t0 := time.Now()
	if !tab.SettleVisual(3*time.Second, 0) {
		t.Fatal("a static page reported as moving")
	}
	if d := time.Since(t0); d > 100*time.Millisecond {
		t.Errorf("a page with no script waited %v", d)
	}
	busy := visualTab(t, `<!doctype html><body><div id=s>x</div><script>
(function f() { document.getElementById('s').style.opacity = String(Math.random()); requestAnimationFrame(f); })();
</script>`)
	t0 = time.Now()
	busy.SettleVisual(0, 0)
	if d := time.Since(t0); d > 100*time.Millisecond {
		t.Errorf("timeout 0 waited %v", d)
	}
}

// A reveal that finishes while a ticker keeps animating for ever: the wait ends once only the ticker moves,
// well before the timeout, with the reveal done.
func TestSettleVisualIgnoresAnAmbientAnimation(t *testing.T) {
	tab := visualTab(t, `<!doctype html><body><div id=tick>ticker</div><h1 id=h style="opacity:0">Hello</h1><script>
var a = 0;
(function f() { a++; document.getElementById('tick').style.transform = 'translateX(' + (a % 200) + 'px)'; requestAnimationFrame(f); })();
var started = Date.now();
setTimeout(function step() {
  var p = Math.min(1, (Date.now() - started - 300) / 600);
  if (p < 0) p = 0;
  document.getElementById('h').style.opacity = String(p);
  if (p < 1) requestAnimationFrame(step);
}, 300);
</script>`)
	t0 := time.Now()
	if !tab.SettleVisual(3*time.Second, 0) {
		t.Fatal("a page whose only motion is an endless ticker never counted as settled within 3 s")
	}
	if d := time.Since(t0); d > 2800*time.Millisecond {
		t.Errorf("waited %v: the ticker held the wait to its timeout", d)
	}
	if v := mustEval(t, tab, `document.getElementById('h').style.opacity`); v != `"1"` {
		t.Errorf("reveal unfinished when the wait ended: opacity %s", v)
	}
}

// A staggered reveal starts a new element every 600 ms. It is not an ambient animation however long it runs:
// the wait goes on until the last element has finished.
func TestSettleVisualWaitsForAStaggeredReveal(t *testing.T) {
	tab := visualTab(t, `<!doctype html><body><p id=e0 style="opacity:0">0</p><p id=e1 style="opacity:0">1</p><p id=e2 style="opacity:0">2</p><p id=e3 style="opacity:0">3</p><script>
var t0 = Date.now();
function fade(id, delay) {
  setTimeout(function step() {
    var p = Math.min(1, (Date.now() - t0 - delay) / 500);
    if (p < 0) p = 0;
    document.getElementById(id).style.opacity = String(p);
    if (p < 1) requestAnimationFrame(step);
  }, delay);
}
for (var i = 0; i < 4; i++) fade('e' + i, 400 + i * 600);
</script>`)
	tab.SettleVisual(10*time.Second, 0)
	if v := mustEval(t, tab, `document.getElementById('e3').style.opacity`); v != `"1"` {
		t.Errorf("last element of a staggered reveal at opacity %s when the wait ended", v)
	}
}
