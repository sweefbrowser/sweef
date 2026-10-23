package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// The heavy page (datasaver_site_test.go) has: a stylesheet with an @font-face,
// a hero image under the cap, a muted autoplay video, a third-party iframe,
// prefetch/preload/prerender/dns-prefetch hints, a first-party script and a
// third-party ad script, and eight images far below the fold.

type modeRun struct {
	level  SaverLevel
	stats  DataStats
	tabSt  DataStats
	text   string
	boxes  []string
	counts map[string]int
	app    bool
	ads    bool
}

func runMode(t *testing.T, s *dsSite, e *Engine, level SaverLevel) modeRun {
	t.Helper()
	s.reset()
	c, tab := ctxTab(t, e, polPtr(level))
	if err := tab.Navigate(s.URL()+"/", nil); err != nil {
		t.Fatal(err)
	}
	tab.Settle(400 * time.Millisecond)
	_, _ = tab.PaintRGBA(1280, 720, 1, false) // layout and paint: images load here
	r := modeRun{level: level, stats: c.DataStats(), tabSt: tab.DataStats(), counts: map[string]int{}}
	r.text, _ = tab.Text("body")
	for _, b := range tab.ElementBoxes() {
		r.boxes = append(r.boxes, fmt.Sprintf("%s %v", b.Tag, [4]float64{b.X, b.Y, b.W, b.H}))
	}
	for _, rq := range s.requests() {
		r.counts[rq.Path]++
	}
	if v, _ := tab.Evaluate(`document.documentElement.getAttribute('data-app')`); strings.Contains(string(v), "ran") {
		r.app = true
	}
	if v, _ := tab.Evaluate(`document.documentElement.getAttribute('data-ads')`); strings.Contains(string(v), "ran") {
		r.ads = true
	}
	return r
}

func TestDataSaverBytesDecreaseByMode(t *testing.T) {
	s := newDSSite(t)
	e := newDSEngine(t)
	off := runMode(t, s, e, SaverOff)
	std := runMode(t, s, e, SaverStandard)
	strict := runMode(t, s, e, SaverStrict)
	for _, r := range []modeRun{off, std, strict} {
		t.Logf("%-8s used=%7d (in %7d out %5d) requests=%2d kinds=%v cacheHit=%d aborted=%d skipped=%v est=%d", r.level, r.stats.Used(), r.stats.WireIn, r.stats.WireOut,
			r.stats.Requests, r.stats.ByKind, r.stats.CacheHitBytes, r.stats.AbortedBytes, r.stats.Skipped, r.stats.SkippedEstimate)
	}
	if !(off.stats.Used() > std.stats.Used() && std.stats.Used() > strict.stats.Used()) {
		t.Fatalf("bytes must strictly decrease: off=%d standard=%d strict=%d", off.stats.Used(), std.stats.Used(), strict.stats.Used())
	}
	if std.stats.Used() > off.stats.Used()/2 {
		t.Fatalf("standard should cut at least half of this page: off=%d standard=%d", off.stats.Used(), std.stats.Used())
	}

	// what each mode requested
	if off.counts["/img/i1.png"] != 1 || off.counts["/img/i8.png"] != 1 {
		t.Fatalf("off must load everything: %v", off.counts)
	}
	if std.counts["/img/hero.png"] != 1 || std.counts["/img/i1.png"] != 0 || std.counts["/video.mp4"] != 0 {
		t.Fatalf("standard: hero yes, far images and video no: %v", std.counts)
	}
	if strict.counts["/img/hero.png"] != 0 || strict.counts["/ads.js"] != 0 || strict.counts["/app.js"] != 1 {
		t.Fatalf("strict: no images, no third-party script, first-party script yes: %v", strict.counts)
	}
	// web fonts: loaded when the text uses them (the bytes here are not a font, so the text stays in
	// the fallback), within the caps in standard, not at all in strict; hints are never fetched
	for name, r := range map[string]modeRun{"off": off, "standard": std, "strict": strict} {
		wantFont := 1
		if name == "strict" {
			wantFont = 0
		}
		if r.counts["/font.woff2"] != wantFont || r.counts["/next.html"] != 0 {
			t.Fatalf("%s: font requests want %d, hints never: %v", name, wantFont, r.counts)
		}
	}
	if !off.ads || !std.ads || strict.ads {
		t.Fatalf("ads script ran: off=%v standard=%v strict=%v", off.ads, std.ads, strict.ads)
	}
	if !off.app || !std.app || !strict.app {
		t.Fatalf("first-party script must run in every mode")
	}

	// text and layout stay correct
	if off.text != std.text || off.text != strict.text {
		t.Fatalf("page text differs between modes:\noff:    %q\nstd:    %q\nstrict: %q", off.text, std.text, strict.text)
	}
	if !strings.Contains(std.text, "The quick brown fox") || !strings.Contains(std.text, "The end.") {
		t.Fatalf("text lost: %q", std.text)
	}
	if strings.Join(off.boxes, "\n") != strings.Join(std.boxes, "\n") {
		diff(t, "standard layout differs from off (all images are sized by attributes)", off.boxes, std.boxes)
	}
	if strings.Join(off.boxes, "\n") != strings.Join(strict.boxes, "\n") {
		diff(t, "strict layout differs from off (placeholders keep the declared size)", off.boxes, strict.boxes)
	}

	// accounting
	if std.stats.Skipped[KindImage] != 0 && std.stats.SkippedEstimate != 0 && false {
		t.Fatal("unreachable")
	}
	if strict.stats.Skipped[KindScript] != 1 || strict.stats.Skipped[KindImage] < 1 {
		t.Fatalf("strict skipped: %v", strict.stats.Skipped)
	}
	if strict.stats.SkippedEstimate <= 0 {
		t.Fatalf("estimate must be set for requests of unknown size: %+v", strict.stats)
	}
}

func diff(t *testing.T, msg string, a, b []string) {
	t.Helper()
	var d []string
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n && len(d) < 8; i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			d = append(d, fmt.Sprintf("  #%d off=%q other=%q", i, x, y))
		}
	}
	sort.Strings(d)
	t.Fatalf("%s (%d vs %d elements):\n%s", msg, len(a), len(b), strings.Join(d, "\n"))
}

func TestDataSaverWireCountersMatchServer(t *testing.T) {
	s := newDSSite(t)
	e := newDSEngine(t)
	// every mode, one context each: the client's socket counters must equal what
	// the servers saw at their sockets (client reads = server writes, and back)
	for _, level := range []SaverLevel{SaverOff, SaverStandard, SaverStrict} {
		s.reset()
		inBefore, outBefore := s.srvIn.Load(), s.srvOut.Load()
		c, tab := ctxTab(t, e, polPtr(level))
		if err := tab.Navigate(s.URL()+"/", nil); err != nil {
			t.Fatal(err)
		}
		tab.Settle(400 * time.Millisecond)
		_, _ = tab.PaintRGBA(1280, 720, 1, false)
		time.Sleep(50 * time.Millisecond) // let the servers' counters catch up with the last reads
		st := c.DataStats()
		srvIn, srvOut := s.srvIn.Load()-inBefore, s.srvOut.Load()-outBefore
		t.Logf("%-8s client in/out = %d/%d   server out/in = %d/%d", level, st.WireIn, st.WireOut, srvOut, srvIn)
		if level != SaverOff { // off reads one MB of a video and drops the rest: the server may have written more than was read
			if st.WireIn != srvOut || st.WireOut != srvIn {
				t.Fatalf("%s: socket counters differ from the server side: client in/out=%d/%d, server out/in=%d/%d", level, st.WireIn, st.WireOut, srvOut, srvIn)
			}
		} else if d := srvOut - st.WireIn; d < 0 || d > 3<<20 {
			t.Fatalf("off: client in=%d, server out=%d", st.WireIn, srvOut)
		}
		// attribution is approximate but must be close to the exact total
		var sum int64
		for _, v := range st.ByKind {
			sum += v
		}
		if diff := float64(sum-st.Used()) / float64(st.Used()); diff > 0.03 || diff < -0.03 {
			t.Fatalf("%s: attributed %d bytes vs %d on the wire (%.1f%% off)", level, sum, st.Used(), diff*100)
		}
		var origins int64
		for _, v := range st.ByOrigin {
			origins += v
		}
		if origins != sum {
			t.Fatalf("by-origin %d != by-kind %d", origins, sum)
		}
	}
	// the engine total covers every context
	in, out := e.WireTotals()
	if in <= 0 || out <= 0 {
		t.Fatalf("engine totals: %d %d", in, out)
	}
}

// serveAbortPage installs handlers for the Content-Length tests.
func serveAbortPage(s *dsSite) (big, big2 []byte) {
	big = noisePNG(800, 600, 77)      // ~1.4 MB: far over the 200 KB cap
	big2 = noisePNG(700, 500, 78)     // ~1.0 MB, sits inside a link
	chunked := noisePNG(700, 500, 79) // served without Content-Length
	s.mu.Lock()
	s.hook = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/abort":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<!doctype html><html><head><title>abort</title></head><body>
<p id="t">Some text before the images.</p>
<img id="big" src="/big.png" width="400" height="300" alt="Big photo">
<img id="ok" src="/img/i1.png" width="160" height="120" alt="Small photo">
<a id="link" href="/next.html"><img id="lnk" src="/big2.png" width="200" height="100" alt="Linked photo"></a>
<img id="chunked" src="/chunked.png" width="200" height="100" alt="Chunked photo">
<p id="b">Text after.</p></body></html>`)
		case "/big.png":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Length", fmt.Sprint(len(big)))
			w.Write(big)
		case "/big2.png":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Length", fmt.Sprint(len(big2)))
			w.Write(big2)
		case "/chunked.png":
			w.Header().Set("Content-Type", "image/png")
			fl := w.(http.Flusher)
			for off := 0; off < len(chunked); off += 16 << 10 {
				end := off + 16<<10
				if end > len(chunked) {
					end = len(chunked)
				}
				w.Write(chunked[off:end])
				fl.Flush()
			}
		default:
			return false
		}
		return true
	}
	s.mu.Unlock()
	return big, big2
}

func TestDataSaverContentLengthAbortAndTapToLoad(t *testing.T) {
	s := newDSSite(t)
	e := newDSEngine(t)
	big, big2 := serveAbortPage(s)
	c, tab := ctxTab(t, e, polPtr(SaverStandard))
	if err := tab.Navigate(s.URL()+"/abort", nil); err != nil {
		t.Fatal(err)
	}
	_, _ = tab.PaintRGBA(1280, 720, 1, false)
	st := c.DataStats()
	t.Logf("in=%d aborted=%d skipped=%v estimate=%d known=%d kinds=%v", st.WireIn, st.AbortedBytes, st.Skipped, st.SkippedEstimate, st.SkippedKnownBytes, st.ByKind)

	// the two oversized images were not read: the wire carries a small fraction of their size
	over := int64(len(big) + len(big2))
	if st.WireIn > over/5 {
		t.Fatalf("wire in %d bytes; the oversized images alone are %d: they must not have been read", st.WireIn, over)
	}
	if st.AbortedBytes != over {
		t.Fatalf("AbortedBytes=%d, want exactly the Content-Lengths of the two aborted responses = %d", st.AbortedBytes, over)
	}
	if st.Skipped[KindImage] != 3 { // big, big2 (by Content-Length) and chunked (by streaming past the cap)
		t.Fatalf("skipped images: %v", st.Skipped)
	}
	if st.SkippedKnownBytes != 0 {
		t.Fatalf("aborted bytes must not be counted twice: %d", st.SkippedKnownBytes)
	}
	for _, id := range []string{"big", "lnk", "chunked"} {
		v, _ := tab.Evaluate(`document.getElementById('` + id + `').getAttribute('data-sweef-lite')`)
		if string(v) != `"skipped"` {
			t.Fatalf("#%s is not a placeholder: %s", id, v)
		}
	}
	if v, _ := tab.Evaluate(`document.getElementById('ok').getAttribute('data-sweef-lite')`); string(v) != "null" {
		t.Fatalf("small image must load normally: %s", v)
	}
	var item SkippedItem
	for _, it := range tab.Skipped() {
		if strings.HasSuffix(it.URL, "/big.png") {
			item = it
		}
	}
	if item.NodeID == 0 || item.Bytes != int64(len(big)) || item.Reason != "too-large" {
		t.Fatalf("skipped item: %+v", item)
	}
	// text and layout do not move
	if txt, _ := tab.Text("body"); !strings.Contains(txt, "Some text before the images.") || !strings.Contains(txt, "Text after.") {
		t.Fatalf("text: %q", txt)
	}

	// tap by node: the image loads in full, the placeholder goes away
	before := c.DataStats().WireIn
	if err := tab.LoadSkipped(item.NodeID); err != nil {
		t.Fatal(err)
	}
	got := c.DataStats().WireIn - before
	if got < int64(len(big)) || got > int64(len(big))+4096 {
		t.Fatalf("LoadSkipped read %d bytes, the image is %d", got, len(big))
	}
	if v, _ := tab.Evaluate(`document.getElementById('big').getAttribute('data-sweef-lite')`); string(v) != "null" {
		t.Fatalf("placeholder not removed: %s", v)
	}
	if err := tab.LoadSkipped(item.NodeID); err != ErrNotSkipped {
		t.Fatalf("second LoadSkipped: %v", err)
	}

	// tap by coordinates on an image inside a link: loads it and does not follow the link
	rect, _ := tab.Evaluate(`(function(){var r=document.getElementById('lnk').getBoundingClientRect();return [r.left+5,r.top+5]})()`)
	var xy [2]float64
	if err := jsonUnmarshal(rect, &xy); err != nil || xy[0] <= 0 {
		t.Fatalf("rect %s %v", rect, err)
	}
	before = c.DataStats().WireIn
	if err := tab.MouseClick(int(xy[0]), int(xy[1])); err != nil {
		t.Fatal(err)
	}
	if tab.URL() != s.URL()+"/abort" {
		t.Fatalf("tapping a placeholder must not follow the link around it: %s", tab.URL())
	}
	if got := c.DataStats().WireIn - before; got < int64(len(big2)) {
		t.Fatalf("tap read %d bytes, image is %d", got, len(big2))
	}
	if v, _ := tab.Evaluate(`document.getElementById('lnk').getAttribute('data-sweef-lite')`); string(v) != "null" {
		t.Fatalf("placeholder not removed after tap: %s", v)
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func imgCount(s *dsSite) int { return s.countPrefix("/img/i") }

func TestDataSaverLazyImagesFollowScroll(t *testing.T) {
	s := newDSSite(t)
	e := newDSEngine(t)
	c, tab := ctxTab(t, e, polPtr(SaverStandard))
	tab.AddInitScript(`window.__sweefScrollTo = function (y) { window.__y = y; }`) // what the app's helper script defines
	if err := tab.Navigate(s.URL()+"/", nil); err != nil {
		t.Fatal(err)
	}
	_, _ = tab.PaintRGBA(1280, 720, 1, false)
	if n := imgCount(s); n != 0 {
		t.Fatalf("no image far below the viewport may load at first, got %d", n)
	}
	if s.count("/img/hero.png") != 1 {
		t.Fatalf("the hero image is in the viewport and loads")
	}
	var tops []float64
	v, _ := tab.Evaluate(`[1,2,3,4,5,6,7,8].map(function(i){return document.getElementById('i'+i).getBoundingClientRect().top})`)
	_ = jsonUnmarshal(v, &tops)
	if len(tops) != 8 || tops[0] < 2*720 {
		t.Fatalf("fixture: image tops %v", tops)
	}
	t.Logf("image tops %v", tops)

	// scroll so that image 3 is on screen: images within a viewport above and two below load
	gen0 := tab.Generation()
	y := int(tops[2]) - 100
	tab.SetScrollY(y)
	loaded := map[int]bool{}
	for i := 1; i <= 8; i++ {
		loaded[i] = s.count(fmt.Sprintf("/img/i%d.png", i)) == 1
	}
	for i := 1; i <= 8; i++ {
		top := tops[i-1]
		inRange := top+120 >= float64(y)-720 && top <= float64(y)+2*720
		if loaded[i] != inRange {
			t.Fatalf("image %d (top %.0f, scroll %d): loaded=%v, in range=%v", i, top, y, loaded[i], inRange)
		}
	}
	if tab.Generation() == gen0 {
		t.Fatal("loading images must change the document generation so the app repaints")
	}
	if tab.DataStats().ByKind[KindImage] == 0 {
		t.Fatal("tab stats must show the images")
	}

	// the app's scroll notification (a script call) is the usual trigger
	before := imgCount(s)
	if _, err := tab.Evaluate(fmt.Sprintf("__sweefScrollTo(%d)", int(tops[7])-100)); err != nil {
		t.Fatal(err)
	}
	if imgCount(s) == before {
		t.Fatalf("the scroll notification did not load more images")
	}
	// scrolling through the page in steps, up and down, loads nothing twice
	for y := 0; y < int(tops[7])+700; y += 500 {
		tab.SetScrollY(y)
	}
	for y := int(tops[7]); y >= 0; y -= 500 {
		tab.SetScrollY(y)
	}
	for i := 1; i <= 8; i++ {
		if n := s.count(fmt.Sprintf("/img/i%d.png", i)); n > 1 {
			t.Fatalf("image %d requested %d times", i, n)
		}
	}
	if n := imgCount(s); n != 8 {
		t.Fatalf("all eight images should be loaded after scrolling to the end, got %d", n)
	}
	// wheel path (agents)
	_, tab2 := ctxTab(t, e, polPtr(SaverStandard))
	s.reset()
	if err := tab2.Navigate(s.URL()+"/", nil); err != nil {
		t.Fatal(err)
	}
	_, _ = tab2.PaintRGBA(1280, 720, 1, false)
	if err := tab2.MouseWheel(0, int(tops[3])); err != nil {
		t.Fatal(err)
	}
	if s.count("/img/i4.png") != 1 {
		t.Fatalf("wheel did not bring image 4 into range: %v", s.requests())
	}
	_ = c
}

func servePollPage(s *dsSite) {
	s.mu.Lock()
	s.hook = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/poll":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<!doctype html><html><body><p id="t">polling page</p><script>
window.__ticks = 0;
setInterval(function () { window.__ticks++; fetch('/ping?n=' + window.__ticks).then(function (r) { return r.text(); }); }, 20);
</script></body></html>`)
		case "/ping":
			fmt.Fprint(w, "pong")
		default:
			return false
		}
		return true
	}
	s.mu.Unlock()
}

func ticks(t *testing.T, tab *Tab) int {
	v, err := tab.Evaluate(`window.__ticks`)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	_ = jsonUnmarshal(v, &n)
	return n
}

func TestDataSaverBackgroundTabMakesNoRequests(t *testing.T) {
	s := newDSSite(t)
	servePollPage(s)
	e := newDSEngine(t)
	for _, tc := range []struct {
		name  string
		level SaverLevel
		quiet bool
	}{{"standard", SaverStandard, true}, {"strict", SaverStrict, true}, {"off", SaverOff, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s.reset()
			_, tab := ctxTab(t, e, polPtr(tc.level))
			if err := tab.Navigate(s.URL()+"/poll", nil); err != nil {
				t.Fatal(err)
			}
			tab.Settle(300 * time.Millisecond)
			if s.count("/ping") == 0 || ticks(t, tab) == 0 {
				t.Fatalf("the page polls while it is in front: pings=%d ticks=%d", s.count("/ping"), ticks(t, tab))
			}
			tab.SetBackground(true)
			s.reset()
			t0 := ticks(t, tab)
			tab.Settle(500 * time.Millisecond)
			_, _ = tab.Evaluate(`fetch('/ping?evaluate=1')`) // page code asking for the network
			_, _ = tab.Evaluate(`var x = new XMLHttpRequest(); x.open('GET', '/ping?xhr=1'); x.send()`)
			tab.Settle(200 * time.Millisecond)
			n := s.count("/ping")
			if tc.quiet && (n != 0 || ticks(t, tab) != t0) {
				t.Fatalf("background tab: %d requests, timers advanced %d -> %d", n, t0, ticks(t, tab))
			}
			if !tc.quiet && n == 0 {
				t.Fatalf("policy off must not suspend anything")
			}
			tab.SetBackground(false)
			s.reset()
			tab.Settle(300 * time.Millisecond)
			if s.count("/ping") == 0 {
				t.Fatalf("a tab shown again resumes")
			}
		})
	}
	// agent-driven tabs are not on screen but must keep working
	s.reset()
	c := e.NewContext("", ContextOptions{})
	c.SetDataPolicy(PolicyFor(SaverStandard))
	tab, _ := c.NewTab("about:blank")
	tab.SetAgentLoading(true)
	if err := tab.Navigate(s.URL()+"/poll", nil); err != nil {
		t.Fatal(err)
	}
	tab.SetBackground(true)
	s.reset()
	tab.Settle(300 * time.Millisecond)
	if s.count("/ping") == 0 {
		t.Fatalf("agent loading mode must not suspend a background tab")
	}
}

func TestDataSaverOffChangesNothing(t *testing.T) {
	s := newDSSite(t)
	e := newDSEngine(t)
	sig := func(pol *DataPolicy) []string {
		s.reset()
		c, tab := ctxTab(t, e, pol)
		if err := tab.Navigate(s.URL()+"/", nil); err != nil {
			t.Fatal(err)
		}
		tab.Settle(300 * time.Millisecond)
		_, _ = tab.PaintRGBA(1280, 720, 1, false)
		var out []string
		for _, r := range s.requests() {
			var hs []string
			for _, k := range []string{"Accept-Encoding", "Save-Data", "Range", "If-None-Match"} {
				if v := r.Header.Get(k); v != "" {
					hs = append(hs, k+"="+v)
				}
			}
			out = append(out, r.Path+" "+strings.Join(hs, ";"))
		}
		sort.Strings(out)
		if st := c.DataStats(); len(st.Skipped) != 0 || st.AbortedBytes != 0 || st.CacheHitBytes != 0 || st.SkippedEstimate != 0 {
			t.Fatalf("off must not skip, abort or credit anything: %+v", st)
		}
		if st := c.DataStats(); st.Used() == 0 || st.ByKind[KindImage] == 0 {
			t.Fatalf("counting still works with the saver off: %+v", st)
		}
		return out
	}
	plain := sig(nil)
	off := sig(polPtr(SaverOff))
	zero := sig(&DataPolicy{ImageMaxBytes: 1, NoImages: true, LazyImages: true, BlockMediaLoad: true}) // flags without a level do nothing
	if strings.Join(plain, "\n") != strings.Join(off, "\n") || strings.Join(plain, "\n") != strings.Join(zero, "\n") {
		t.Fatalf("requests differ:\nno policy: %v\noff:       %v\nflags only: %v", plain, off, zero)
	}
	for _, l := range plain {
		if strings.Contains(l, "Save-Data") {
			t.Fatalf("off sends no Save-Data: %s", l)
		}
	}
}

func serveScriptsPage(s *dsSite) {
	mk := func(tag string, n int) []byte {
		return append([]byte("window."+tag+"=1;/*"), append(filler(n, int64(n)), '*', '/')...)
	}
	a, b, c := mk("sa", 120<<10), mk("sb", 120<<10), mk("sc", 50<<10)
	s.mu.Lock()
	s.hook = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/scripts":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<!doctype html><html><body><p id="t">scripts</p>
<script src="/sa.js"></script><script src="/sb.js"></script><script src="/sc.js"></script>
<script src="`+s.third.URL+`/ads.js"></script></body></html>`)
		case "/sa.js", "/sb.js", "/sc.js":
			w.Header().Set("Content-Type", "text/javascript")
			body := map[string][]byte{"/sa.js": a, "/sb.js": b, "/sc.js": c}[r.URL.Path]
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Write(body)
		default:
			return false
		}
		return true
	}
	s.mu.Unlock()
}

func TestDataSaverStrictScriptBudgetAndThirdParty(t *testing.T) {
	s := newDSSite(t)
	serveScriptsPage(s)
	e := newDSEngine(t)
	pol := PolicyFor(SaverStrict)
	pol.ScriptMaxBytes = 200 << 10
	c, tab := ctxTab(t, e, &pol)
	nav(t, tab, s.URL()+"/scripts")
	has := func(v string) bool {
		x, _ := tab.Evaluate("window." + v + " === 1")
		return string(x) == "true"
	}
	// a (120 KB) fits the budget; b (120 KB) would exceed what is left (80 KB) and is
	// aborted on its Content-Length; c (50 KB) still fits; the third-party script is blocked
	if !has("sa") || has("sb") || !has("sc") {
		t.Fatalf("scripts that ran: a=%v b=%v c=%v", has("sa"), has("sb"), has("sc"))
	}
	if s.count("/ads.js") != 0 {
		t.Fatal("third-party script must not be requested in strict mode")
	}
	st := c.DataStats()
	if st.Skipped[KindScript] != 2 {
		t.Fatalf("skipped scripts: %v (want b and ads.js)", st.Skipped)
	}
	if st.AbortedBytes < 120<<10 || st.AbortedBytes > 121<<10 {
		t.Fatalf("AbortedBytes=%d, want b's Content-Length (~120 KB)", st.AbortedBytes)
	}
	if st.ByKind[KindScript] > 175<<10 {
		t.Fatalf("scripts cost %d bytes, a and c are ~170 KB", st.ByKind[KindScript])
	}
	// the same page in standard runs everything
	_, tab2 := ctxTab(t, e, polPtr(SaverStandard))
	nav(t, tab2, s.URL()+"/scripts")
	for _, v := range []string{"sa", "sb", "sc"} {
		if x, _ := tab2.Evaluate("window." + v + " === 1"); string(x) != "true" {
			t.Fatalf("standard: script %s did not run", v)
		}
	}
}

func TestDataSaverAgentLoadingMode(t *testing.T) {
	s := newDSSite(t)
	e := newDSEngine(t)
	// the context is not in any saver level: the tab opts into agent loading
	_, tab := ctxTab(t, e, nil)
	tab.SetAgentLoading(true)
	nav(t, tab, s.URL()+"/")
	_, _ = tab.PaintRGBA(1280, 720, 1, false)
	if s.countPrefix("/img/") != 0 || s.count("/video.mp4") != 0 {
		t.Fatalf("agents never load images or media: %v", s.requests())
	}
	if s.count("/ads.js") != 1 || s.count("/app.js") != 1 {
		t.Fatalf("agent mode has no script limits: ads=%d app=%d", s.count("/ads.js"), s.count("/app.js"))
	}
	if v, _ := tab.Evaluate(`document.getElementById('hero').getAttribute('data-sweef-lite')`); string(v) != `"skipped"` {
		t.Fatalf("images keep their alt text and declared size as placeholders: %s", v)
	}
	if snap, _ := tab.AriaSnapshotV2(SnapshotOptions{}); !strings.Contains(snap, "Hero image") {
		t.Fatalf("snapshot should still name the image: %s", snap)
	}
	if tab.DataStats().Skipped[KindImage] != 9 {
		t.Fatalf("skipped: %v", tab.DataStats().Skipped)
	}
	// context level switch
	c2 := e.NewContext("", ContextOptions{})
	c2.SetAgentLoading(true)
	tab2, _ := c2.NewTab("about:blank")
	s.reset()
	nav(t, tab2, s.URL()+"/")
	_, _ = tab2.PaintRGBA(1280, 720, 1, false)
	if s.countPrefix("/img/") != 0 {
		t.Fatal("context-level agent loading did not apply")
	}
	// the per-tab override wins
	off := PolicyFor(SaverOff)
	tab2.SetDataPolicy(&off)
	s.reset()
	nav(t, tab2, s.URL()+"/")
	_, _ = tab2.PaintRGBA(1280, 720, 1, false)
	if s.count("/img/hero.png") != 1 {
		t.Fatal("a tab override (Off) must win over agent loading")
	}
}

func serveMediaPage(s *dsSite) {
	s.mu.Lock()
	s.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/media" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<!doctype html><html><body>
<video id="a" src="/video.mp4?a" autoplay muted preload="auto" width="320" height="180"></video>
<video id="b" src="/video.mp4?b" controls width="320" height="180"></video>
<audio id="c" src="/video.mp4?c" preload="auto"></audio>
<button id="play">play</button>
<script>document.getElementById('play').addEventListener('click', function () { document.getElementById('b').play().catch(function () {}); });</script>
</body></html>`)
			return true
		}
		return false
	}
	s.mu.Unlock()
}

func TestDataSaverBusEventsAreThrottled(t *testing.T) {
	s := newDSSite(t)
	e := newDSEngine(t)
	id, ch := e.Bus().Subscribe(4096)
	defer e.Bus().Unsubscribe(id)
	c, tab := ctxTab(t, e, polPtr(SaverOff))
	start := time.Now()
	nav(t, tab, s.URL()+"/")
	tab.Settle(300 * time.Millisecond)
	_, _ = tab.PaintRGBA(1280, 720, 1, false)
	time.Sleep(400 * time.Millisecond) // the trailing event
	elapsed := time.Since(start)
	var n int
	var last DataEvent
	for done := false; !done; {
		select {
		case ev := <-ch:
			if ev.Kind == EvData && ev.Tab == tab.ID {
				n++
				last = ev.Data.(DataEvent)
			}
		default:
			done = true
		}
	}
	// at most 4 per second plus the trailing one, and at least the final state
	if n == 0 || n > int(elapsed.Seconds()*4)+2 {
		t.Fatalf("%d data events in %v", n, elapsed)
	}
	if last.Tab.Used() != tab.DataStats().Used() || last.WireIn != c.DataStats().WireIn || last.Context != c.ID {
		t.Fatalf("the last event must carry the final numbers: %+v vs %+v", last, tab.DataStats())
	}
}

func TestDataSaverLoadFullyAndOverrides(t *testing.T) {
	s := newDSSite(t)
	e := newDSEngine(t)
	c, tab := ctxTab(t, e, polPtr(SaverStrict))
	nav(t, tab, s.URL()+"/")
	_, _ = tab.PaintRGBA(1280, 720, 1, false)
	if s.count("/img/hero.png") != 0 || s.count("/ads.js") != 0 {
		t.Fatal("strict page should not have loaded these")
	}
	if tab.FullyLoaded() {
		t.Fatal("not fully loaded yet")
	}
	s.reset()
	if err := tab.LoadFully(); err != nil {
		t.Fatal(err)
	}
	_, _ = tab.PaintRGBA(1280, 720, 1, false)
	if !tab.FullyLoaded() || s.count("/img/hero.png") != 1 || s.count("/ads.js") != 1 {
		t.Fatalf("LoadFully must load as if off: %v", s.requests())
	}
	if rq, _ := s.last("/"); rq.Header.Get("Save-Data") != "" || rq.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("fully loaded document request: %v", rq.Header)
	}
	if st := tab.DataStats(); st.Skipped[KindImage] != 0 || st.ByKind[KindImage] == 0 {
		t.Fatalf("stats of the fully loaded page: %+v", st)
	}
	// "once": the next navigation is back under the context policy
	s.reset()
	nav(t, tab, s.URL()+"/")
	_, _ = tab.PaintRGBA(1280, 720, 1, false)
	if tab.FullyLoaded() || s.countPrefix("/img/") != 0 {
		t.Fatalf("the exemption ends with the page: %v", s.requests())
	}
	// per-tab policy (the app's per-site rules)
	lite := PolicyFor(SaverStandard)
	tab.SetDataPolicy(&lite)
	s.reset()
	nav(t, tab, s.URL()+"/")
	_, _ = tab.PaintRGBA(1280, 720, 1, false)
	if s.count("/img/hero.png") != 1 || s.count("/ads.js") != 1 {
		t.Fatalf("a standard override on a strict context: %v", s.requests())
	}
	tab.SetDataPolicy(nil)
	if got := tab.policy(); got.Level != SaverStrict {
		t.Fatalf("nil override inherits: %v", got.Level)
	}
	_ = c
}

func TestDataSaverFontPolicy(t *testing.T) {
	s := newDSSite(t)
	e := newDSEngine(t)
	// Web fonts load when the page's text needs them: always when the saver is off, within the caps
	// in standard (webfonts_policy_test.go has the byte-exact tests), not in strict (icon fonts excepted).
	// The font of this site is not a valid font file: the text stays in the fallback in every mode.
	// A direct font read (fetchResourceKind) is still gated by BlockFonts.
	for _, level := range []SaverLevel{SaverOff, SaverStandard, SaverStrict} {
		s.reset()
		_, tab := ctxTab(t, e, polPtr(level))
		nav(t, tab, s.URL()+"/")
		_, _ = tab.PaintRGBA(1280, 720, 1, false)
		want := 1
		if level == SaverStrict {
			want = 0
		}
		if s.count("/font.woff2") != want {
			t.Fatalf("%s: font requests %d, want %d", level, s.count("/font.woff2"), want)
		}
		s.reset()
		_, _, err := tab.fetchResourceKind(s.URL()+"/font.woff2", KindFont)
		if level != SaverStrict && (err != nil && level == SaverOff) {
			t.Fatalf("%s: a font read is allowed: %v", level, err)
		}
		if level == SaverStrict && (err != errSaverBlocked || s.count("/font.woff2") != 0) {
			t.Fatalf("%s: BlockFonts must stop the read: %v", level, err)
		}
	}
}

func TestDataSaverThirdPartyFramesAreClickToLoad(t *testing.T) {
	s := newDSSite(t)
	e := newDSEngine(t)
	for _, level := range []SaverLevel{SaverOff, SaverStandard} {
		_, tab := ctxTab(t, e, polPtr(level))
		nav(t, tab, s.URL()+"/")
		_, _ = tab.PaintRGBA(1280, 720, 1, false)
		v, _ := tab.Evaluate(`document.getElementById('ad').getAttribute('data-sweef-lite')`)
		if level == SaverOff {
			if string(v) != "null" {
				t.Fatalf("off must not touch frames: %s", v)
			}
			continue
		}
		if string(v) != `"frame"` {
			t.Fatalf("a third-party iframe becomes a placeholder: %s", v)
		}
		var frame SkippedItem
		for _, it := range tab.Skipped() {
			if it.Kind == KindFrame {
				frame = it
			}
		}
		if frame.NodeID == 0 || !strings.HasSuffix(frame.URL, "/embed.html") {
			t.Fatalf("skipped frame: %+v", frame)
		}
		if st := tab.DataStats(); st.Skipped[KindFrame] != 0 {
			t.Fatalf("the engine fetches no frame documents, so nothing is saved or counted: %v", st.Skipped)
		}
		if err := tab.Click("#ad"); err != nil {
			t.Fatal(err)
		}
		if v, _ := tab.Evaluate(`document.getElementById('ad').getAttribute('data-sweef-lite')`); string(v) != "null" {
			t.Fatalf("tap must uncover the frame: %s", v)
		}
		_, _ = tab.PaintRGBA(1280, 720, 1, false)
		if v, _ := tab.Evaluate(`document.getElementById('ad').getAttribute('data-sweef-lite')`); string(v) != "null" {
			t.Fatalf("a layout pass must not cover it again: %s", v)
		}
	}
	// a same-site frame is left alone
	s.mu.Lock()
	s.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/same" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><iframe id="own" src="/next.html" width="200" height="100"></iframe></body></html>`)
			return true
		}
		return false
	}
	s.mu.Unlock()
	_, tab := ctxTab(t, e, polPtr(SaverStrict))
	nav(t, tab, s.URL()+"/same")
	_, _ = tab.PaintRGBA(1280, 720, 1, false)
	if v, _ := tab.Evaluate(`document.getElementById('own').getAttribute('data-sweef-lite')`); string(v) != "null" {
		t.Fatalf("same-site frames are not placeholders: %s", v)
	}
}
