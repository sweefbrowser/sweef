package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"

	fnet "github.com/sweefbrowser/sweef/internal/net"
	"github.com/sweefbrowser/sweef/internal/raster"
)

// Ring is a fixed-capacity circular buffer of events. Bounded memory is the
// whole point: an agent that never reads logs still cannot OOM the browser.
type Ring struct {
	mu    sync.Mutex
	items []Event
	size  int
	head  int
	n     int
	seq   int64
}

func NewRing(size int) *Ring {
	if size <= 0 {
		size = 128
	}
	return &Ring{items: make([]Event, size), size: size}
}

func (r *Ring) Push(e Event) Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e.SID = r.seq
	i := (r.head + r.n) % r.size
	r.items[i] = e
	if r.n == r.size {
		r.head = (r.head + 1) % r.size // overwrite oldest
	} else {
		r.n++
	}
	return e
}

// Since returns all events with SID > sinceSID, oldest first.
func (r *Ring) Since(sinceSID int64) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, 0, r.n)
	for i := 0; i < r.n; i++ {
		e := r.items[(r.head+i)%r.size]
		if e.SID > sinceSID {
			out = append(out, e)
		}
	}
	return out
}

func (r *Ring) All() []Event { return r.Since(0) }

// Seq is the SID of the newest entry (0 for none).
func (r *Ring) Seq() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// Trim halves the live window — called by the memory governor under pressure.
func (r *Ring) Trim() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n > 1 {
		drop := r.n / 2
		for i := 0; i < drop; i++ { // clear the slots too, or the events they hold stay reachable
			r.items[(r.head+i)%r.size] = Event{}
		}
		r.head = (r.head + drop) % r.size
		r.n -= drop
	}
}

// NodeInfo is the serialisable handle the outside world uses to address DOM
// nodes. BackendNodeID is stable for the lifetime of the document tree.
type NodeInfo struct {
	NodeID    int64             `json:"nodeId"`
	BackendID int64             `json:"backendNodeId"`
	Name      string            `json:"name"`         // tag name, lowercased
	ID        string            `json:"id,omitempty"` // id attribute
	Type      string            `json:"type,omitempty"`
	Role      string            `json:"role,omitempty"`
	Text      string            `json:"text,omitempty"` // accessible-ish label
	Href      string            `json:"href,omitempty"`
	Attrs     map[string]string `json:"attrs,omitempty"`
	Visible   bool              `json:"visible"`
	Disabled  bool              `json:"disabled"`
	Checked   bool              `json:"checked,omitempty"`
	Box       [4]int            `json:"box,omitempty"` // x,y,w,h in layout px (approx)
	Focusable bool              `json:"focusable"`
}

// Tab is one browsing context. Goroutine-light: no dedicated thread, just
// state + a mutex; JS evaluation happens on demand.
type Tab struct {
	ID         string
	incrMu     sync.Mutex
	incr       *raster.Canvas // pixels of the last incremental paint (see incremental.go)
	lastPaint  atomic.Pointer[PaintInfo]
	paintedGen atomic.Uint64 // document generation of the last PaintRGBAOpts (Dirty)
	layoutN    atomic.Int64  // style+layout runs of the render pipeline (LayoutStats)
	layoutNs   atomic.Int64  // their total time in nanoseconds
	mu         sync.RWMutex
	eng        *Engine
	ctx        *BrowserContext // isolation context (nil = engine default)
	title      string
	url        *url.URL
	html       string
	dom        *fnet.Doc                   // parsed tree held by our DOM layer
	kbdFocus   atomic.Pointer[kbdFocusRec] // focused element of a page without a VM (kbdfocus.go)
	scripts    []*CompiledScript

	consoleRing *Ring
	netRing     *Ring
	stateRing   *Ring
	dialogRing  *Ring
	devHL       atomic.Int64 // node highlighted by Developer Tools (0 none)
	dev         *dtLog       // Developer Tools network recorder (devtools_net.go); shared with frames and worker tabs

	js      *jsVM // per-document JavaScript VM (nil when JS is disabled)
	history []*url.URL
	hIdx    int

	closed atomic.Bool

	// Emulation state (Playwright page.* setters)
	vpW, vpH    int
	dsf         float64
	mobile      bool
	landscape   bool
	userAgent   string
	geo         *[2]float64
	permissions map[string]bool
	docPolicy   atomic.Value // string: the document's Permissions-Policy header
	offline     bool
	colorScheme string
	extraHdrs   map[string]string

	initScripts []*CompiledScript
	bindings    []string
	scrollHook  atomic.Pointer[func(ScrollRequest)] // who moves the viewport when page script scrolls it (scrollapi.go)
	selHook     atomic.Pointer[func()]              // told when the selection changes (selection.go)
	quiet       quietState                          // changes that repaint part of the page (quiet.go)

	// Dialog response armed by the agent before the dialog fires
	dialogAccept bool
	dialogText   string

	// Interception hooks (route.fulfill / route.abort semantics)
	interceptMu sync.RWMutex
	routes      []*fnet.Route
	routes2     routeTable // RouteRule interception (fulfill/abort/continue)

	lastNet        atomic.Int64 // unix nanos of the last request start (networkidle)
	bindingReplies map[string]string
	bindingRing    *Ring
	downloads      []Download

	ds tabDS  // Data Saver state (datasaver.go)
	sw tabSW  // service worker control of the document (sw_intercept.go)
	pw pwaTab // user activation, for the permission and window rules of pwa.go

	bc    *browsingCtx // nested browsing context or worker tab: its place in the tree and its partition (partition.go)
	winID int64        // identity of this tab's window for cross-window references (frames.go)
	wk    *bgWorker    // set on the hidden tab of a worker (workers.go)
	fr    frameSet     // child frames of the document and, for a frame, its pump (frames.go)

	isoState atomic.Pointer[isolation] // COOP/COEP state of the current document (coi.go)

	navSite atomic.Pointer[string] // schemeful site of the document being loaded (partition.go)

	asyncImg atomic.Bool // layout starts image fetches instead of waiting for them (SetAsyncImages)
	// scriptGeom > 0 while a host call of the page's script runs: a layout it forces (getBoundingClientRect,
	// offsetWidth, an observer) does not wait for the network, as in a browser (scriptForcedLayout)
	scriptGeom atomic.Int32
	// fullInputSettle: input actions settle like other actions (SetFullInputSettle)
	fullInputSettle atomic.Bool
	imgSem          chan struct{}             // images fetched at once
	preloadSem      chan struct{}             // requests the preload scanner has in flight
	imgTimer        atomic.Bool               // a relayout request for newly arrived images is scheduled
	onUpdate        atomic.Pointer[func()]    // the window's "repaint me" callback (SetUpdateHook)
	onWake          atomic.Pointer[func()]    // the window's "give the event loop a slice" callback (SetWakeHook)
	pl              atomic.Pointer[progLoad]  // load work still pending after NavigateProgressive (progload.go)
	plMu            sync.Mutex                // one LoadStep at a time
	progMode        atomic.Bool               // the current navigation is progressive: page-initiated navigations are too
	tr              atomic.Pointer[loadTrace] // timeline of the current navigation (loadtrace.go)
	traceNet        atomic.Bool               // record DNS/connect/TLS/first byte of every request
	win             winState                  // frames, opener/popups, messaging (windows.go, frames.go)
}

func newTab(e *Engine, id string) *Tab {
	t := &Tab{
		winID:       winSeq.Add(1),
		ID:          id,
		eng:         e,
		consoleRing: NewRing(e.opts.ConsoleRingSize),
		netRing:     NewRing(e.opts.NetworkRingSize),
		stateRing:   NewRing(e.opts.StateRingSize),
		dialogRing:  NewRing(64),
		dev:         newDtLog(),
		bindingRing: NewRing(256),
		vpW:         e.opts.ViewportWidth,
		vpH:         e.opts.ViewportHeight,
		dsf:         1,
		imgSem:      make(chan struct{}, 8),
		preloadSem:  make(chan struct{}, prefetchConns),
	}
	if t.vpW == 0 {
		t.vpW = 1280
	}
	if t.vpH == 0 {
		t.vpH = 720
	}
	winReg.Store(t.winID, t)
	t.win.popups = NewRing(64)
	return t
}

// Context returns the tab's browser context (creating the default lazily).
func (t *Tab) Context() *BrowserContext {
	if t.ctx != nil {
		return t.ctx
	}
	return t.eng.DefaultContext()
}

func (t *Tab) close() {
	if t.closed.CompareAndSwap(false, true) {
		t.closeFrames()
		winReg.Delete(t.winID)
		t.swDocumentGone()
		dropBitmapSources(t.ID)
		if env := t.ds.env.Load(); env != nil {
			env.fonts.close() // releases the document's web font set
		}
		t.mu.Lock()
		j := t.js
		t.js = nil
		t.mu.Unlock()
		j.close()
	}
}

func (t *Tab) URL() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.url == nil {
		return "about:blank"
	}
	return CleanTabURL(t.url.String())
}

// Title reads the live <title> so changes made by page scripts are visible.
func (t *Tab) Title() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.dom != nil {
		if s := t.dom.Title(); s != "" || t.title == "" {
			return CleanTabTitle(s)
		}
	}
	return CleanTabTitle(t.title)
}

// badTabRune reports the characters tab-state consumers must never see in a
// title or URL: C0 controls, DEL, C1 controls and the Unicode line and
// paragraph separators. (Anthropic's browser_state block rejects a request
// whose tab title holds one with HTTP 400, and they break line-based output.)
func badTabRune(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029
}

// CleanTabTitle is document.title made safe for tab-state output: the
// characters badTabRune rejects become spaces, then whitespace is stripped and
// collapsed as document.title does.
func CleanTabTitle(s string) string {
	if strings.IndexFunc(s, badTabRune) < 0 && !strings.Contains(s, "  ") && strings.TrimSpace(s) == s {
		return s
	}
	s = strings.Map(func(r rune) rune {
		if badTabRune(r) {
			return ' '
		}
		return r
	}, s)
	return fnet.StripCollapseASCIIWhitespace(s)
}

// CleanTabURL percent-encodes the characters badTabRune rejects (a URL
// String() already escapes them; this guards the other ways a URL reaches
// tab state).
func CleanTabURL(s string) string {
	if strings.IndexFunc(s, badTabRune) < 0 {
		return s
	}
	var sb strings.Builder
	for _, r := range s {
		if !badTabRune(r) {
			sb.WriteRune(r)
			continue
		}
		var buf [4]byte
		for _, b := range buf[:utf8.EncodeRune(buf[:], r)] {
			fmt.Fprintf(&sb, "%%%02X", b)
		}
	}
	return sb.String()
}

func (t *Tab) emit(kind EventKind, data any) {
	if t.bc != nil {
		return // frames and workers are not tabs of the engine: their events belong to the page that owns them
	}
	t.eng.bus.Publish(Event{Tab: t.ID, Kind: kind, At: time.Now(), Data: data})
}

// Navigate fetches url and installs the resulting document. This is the
// equivalent of Playwright's page.goto with waitUntil baked in.
func (t *Tab) Navigate(rawurl string, headers map[string]string) error {
	return t.navigate(rawurl, headers, false)
}

// NavigateProgressive is Navigate for a window that shows the page while it
// loads (the GUI). It returns as soon as a first frame can be painted: the
// document is fetched and parsed, its render-blocking stylesheets are in and the
// scripts that were already downloaded and are quick have run. Everything else
// (the remaining scripts, DOMContentLoaded, load, the settle pump, and with
// SetAsyncImages the images) is left pending: the caller drives it with
// LoadStep between paints and input, or runs it out with LoadFinish.
// Headless callers (CLI, RPC, MCP, the agent) use Navigate, which returns only
// when the page has settled.
func (t *Tab) NavigateProgressive(rawurl string, headers map[string]string) error {
	return t.navigate(rawurl, headers, true)
}

func (t *Tab) navigate(rawurl string, headers map[string]string, progressive bool) error {
	if t.sw.noNav {
		return fmt.Errorf("navigation is not available here")
	}
	// a path-only URL (snapshots show same-origin links that way) is relative to the current page
	if r := strings.TrimSpace(rawurl); (strings.HasPrefix(r, "/") && !strings.HasPrefix(r, "//")) || strings.HasPrefix(r, "?") {
		if cur, err := url.Parse(t.URL()); err == nil && (cur.Scheme == "http" || cur.Scheme == "https") {
			if abs, err := cur.Parse(r); err == nil {
				rawurl = abs.String()
			}
		}
	}
	u, err := url.Parse(rawurl)
	if err != nil {
		return fmt.Errorf("bad url %q: %w", rawurl, err)
	}
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	if t.IsOfflineMode() {
		err := fmt.Errorf("offline mode: navigation to %s blocked", rawurl)
		t.netRing.Push(Event{Tab: t.ID, Kind: EvFailure, At: time.Now(), Data: map[string]string{"url": rawurl, "error": err.Error()}})
		return err
	}
	t.resetDataForNavigation()
	t.resetTrace()
	t0 := time.Now()
	if t.bc == nil {
		t.setNavSite(u.String())
		defer t.setNavSite("") // however it ends, the document's own URL is the page again
	}
	res, err := t.fetch(u, headers)
	t.tspan("doc.fetch", u.String(), t0)
	if errors.Is(err, errDownloaded) {
		return nil // the response was saved as a download; the page stays
	}
	if err != nil {
		t.netRing.Push(Event{Tab: t.ID, Kind: EvFailure, At: time.Now(), Data: map[string]string{"url": rawurl, "error": err.Error()}})
		t.emit(EvFailure, map[string]any{"url": rawurl, "error": err.Error()})
		return err
	}
	if t.isFrame() {
		if blocked, why := t.frameAncestorsBlocked(res.header, res.finalURL); blocked {
			t.LogConsole("error", fmt.Sprintf("Refused to display '%s' in a frame because it set '%s'.", res.finalURL, why))
			t.fr.blocked.Store(true)
			t.fr.opaque.Store(true)
			t.setIsolation(nil, "about:blank")
			t.setDocument("<!doctype html><html><head></head><body></body></html>", "about:blank", false)
			return nil
		}
		if blocked, why := t.frameCOEPBlocked(res.header, res.finalURL); blocked {
			t.LogConsole("error", fmt.Sprintf("Refused to display '%s' in a frame because it lacks %s.", res.finalURL, why))
			t.fr.blocked.Store(true)
			t.fr.opaque.Store(true)
			t.setIsolation(nil, "about:blank")
			t.setDocument("<!doctype html><html><head></head><body></body></html>", "about:blank", false)
			return nil
		}
		t.fr.blocked.Store(false)
	}
	t.commitCOOP(coopHeader(res.header), res.finalURL)
	t.setIsolation(res.header, res.finalURL)
	t.setDocument(res.body, res.finalURL, progressive)
	if !progressive && !t.isFrame() {
		t.WaitFrames(frameWaitBudget) // a headless caller reads the page right after: let its frames load
	}
	t.mu.Lock()
	// history bookkeeping (goBack/goForward)
	if t.hIdx+1 < len(t.history) {
		t.history = t.history[:t.hIdx+1]
	}
	t.history = append(t.history, u)
	t.hIdx = len(t.history) - 1
	t.mu.Unlock()
	t.emit(EvNavigated, map[string]any{"url": res.finalURL, "status": res.status})
	return nil
}

type fetchResult struct {
	body     string
	finalURL string
	status   int
	har      fnet.RequestRecord
	header   http.Header // response headers (frame checks)
}

func (t *Tab) fetch(u *url.URL, headers map[string]string) (*fetchResult, error) {
	if strings.EqualFold(u.Scheme, "about") && strings.EqualFold(strings.TrimSpace(u.Opaque), "blank") {
		// about:blank is an empty document, not a request
		return &fetchResult{body: "<!doctype html><html><head></head><body></body></html>", finalURL: "about:blank", status: 200}, nil
	}
	if u.Scheme == "data" {
		mt, data, err := decodeDataURL(u.String())
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		h.Set("Content-Type", mt)
		if isDownloadResponse(h) {
			t.SaveDownloadBytes("download", mt, data, u.String())
			return nil, errDownloaded
		}
		return &fetchResult{body: string(data), finalURL: u.String(), status: 200}, nil
	}
	t.swPrepareNavigation(u.String())
	route := t.routedRequestNoBlock // the top-level document is never blocked; a frame's is a subresource
	if t.isFrame() {
		route = t.routedRequest
	}
	synth, method, target, hdrs, _, err := route("GET", u.String(), headers, "")
	if err != nil {
		t.dev.begin(t.documentKind(), "GET", u.String(), headers, "").fail(err.Error())
		t.netRing.Push(Event{Tab: t.ID, Kind: EvFailure, At: time.Now(), Data: map[string]string{"url": u.String(), "error": err.Error()}})
		return nil, err
	}
	if synth == nil && method == "GET" { // a service worker may answer the navigation
		sres, handled, serr := t.swNavigation(target, hdrs)
		if handled {
			if serr != nil {
				t.netRing.Push(Event{Tab: t.ID, Kind: EvFailure, At: time.Now(), Data: map[string]string{"url": target, "error": serr.Error()}})
				return nil, serr
			}
			synth = sres
		}
	}
	rec := fnet.RequestRecord{Method: method, URL: target}
	start := time.Now()
	var res *http.Response
	var docMI *meterInfo
	var dr *dtRec
	if synth != nil {
		res = synth
		rec.Headers = map[string]string{"x-sweef-route": "fulfill"}
		dr = t.dev.begin(t.documentKind(), method, target, rec.Headers, "")
	} else {
		req, err := http.NewRequestWithContext(t.eng.ctx, method, target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", t.UserAgent())
		t.setDocumentHeaders(req.Header, req.URL)
		for k, v := range t.ExtraHeaders() {
			req.Header.Set(k, v)
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		pol := t.policy()
		rec.Headers = flattenHeaders(req.Header)
		t.netRing.Push(Event{Tab: t.ID, Kind: EvRequest, At: time.Now(), Data: rec})
		t.emit(EvRequest, rec)
		dr = t.dev.begin(t.documentKind(), method, target, rec.Headers, "")
		t.touchNet()
		mreq, mi := withMeter(req, t.Context(), t, t.documentKind(), pol)
		if res, err = t.eng.clientFor(t.Context()).Do(mreq); err != nil {
			dr.fail(err.Error())
			return nil, err
		}
		docMI = mi
	}
	if isDownloadResponse(res.Header) && res.StatusCode < 400 {
		defer res.Body.Close()
		dr.done(res.StatusCode, res.Header, "", 0, nil, target)
		t.saveDownload(u.String(), suggestedName(res.Header, u.String(), ""), res.Header.Get("Content-Type"), res.Body)
		return nil, errDownloaded
	}
	defer res.Body.Close()
	var body []byte
	limit := int64(8 << 20) // 8 MiB per doc — protects 50 MB machines
	var b []byte
	if isHTMLResponse(res.Header) && !t.IsOfflineMode() {
		finalURL := target
		if res.Request != nil && res.Request.URL != nil {
			finalURL = res.Request.URL.String()
		}
		if t.bc == nil {
			t.setNavSite(finalURL) // a redirect may have moved the document to another site
		}
		b, err = readWithPreloadScan(t, io.LimitReader(res.Body, limit), finalURL)
	} else {
		b, err = io.ReadAll(io.LimitReader(res.Body, limit))
	}
	if err != nil {
		dr.fail(err.Error())
		return nil, err
	}
	body = b
	rec.Status = res.StatusCode
	rec.FinalURL = target
	if res.Request != nil && res.Request.URL != nil {
		rec.FinalURL = res.Request.URL.String()
	}
	rec.DurationMs = time.Since(start).Milliseconds()
	rec.ResponseHeaders = flattenHeaders(res.Header)
	if docMI != nil {
		if st := docMI.cacheState(); st != "" {
			h := make(map[string]string, len(rec.Headers)+1)
			for k, v := range rec.Headers {
				h[k] = v
			}
			h["x-sweef-cache"] = st
			rec.Headers = h
		}
	}
	t.netRing.Push(Event{Tab: t.ID, Kind: EvResponse, At: time.Now(), Data: rec})
	t.emit(EvResponse, rec)
	{
		var xfer int64
		cs := rec.Headers["x-sweef-cache"]
		if docMI != nil {
			xfer = docMI.hdrBytes.Load() + docMI.rawBytes.Load()
			if docMI.fromCache.Load() {
				xfer = 0
			}
		}
		dr.done(res.StatusCode, res.Header, cs, xfer, body, rec.FinalURL)
	}
	status, text := res.StatusCode, string(body)
	return &fetchResult{body: text, finalURL: rec.FinalURL, status: status, har: rec, header: res.Header}, nil
}

func flattenHeaders(h map[string][]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

// SetDocument parses html, rebuilds the DOM, runs init scripts + page scripts,
// fires load.
func (t *Tab) SetDocument(htmlSrc, finalURL string) {
	t.setIsolation(nil, finalURL)
	t.setDocument(htmlSrc, finalURL, false)
}

func (t *Tab) setDocument(htmlSrc, finalURL string, progressive bool) {
	t.closeFrames() // the frames of the document being replaced
	tp := time.Now()
	doc, err := fnet.ParseDocument(htmlSrc, finalURL)
	t.tspan("doc.parse", itoa(len(htmlSrc))+" bytes", tp)
	if err != nil {
		t.consoleRing.Push(Event{Tab: t.ID, Kind: EvConsole, At: time.Now(),
			Data: map[string]string{"level": "error", "text": "parse failure: " + err.Error()}})
		return
	}
	// forward DOM mutations from the doc layer onto the bus (EvDomMutated)
	doc.SetMutationListener(func(m fnet.Mutation) {
		t.stateRing.Push(Event{Tab: t.ID, Kind: EvDomMutated, At: time.Now(), Data: m})
		t.emit(EvDomMutated, m)
		t.frameMutation(m)
	})
	if w, _, _, _, _ := t.Viewport(); w > 0 {
		doc.SetLayoutWidth(w)
	}
	t.pl.Store(nil) // load work of the previous document is void
	t.installRender(doc)
	if !progressive {
		if env := t.ds.env.Load(); env != nil && env.doc == doc {
			env.prefetchDocImagesEarly()
		}
	}
	ts := time.Now()
	t.applyStyles(doc)
	t.tspan("styles.ready", "", ts)
	t.closeFrames() // the frames of the document that is being replaced
	t.mu.Lock()
	t.html = htmlSrc
	t.dom = doc
	t.title = doc.Title()
	u, _ := url.Parse(finalURL)
	t.url = u
	t.scripts = compileScripts(doc.Scripts())
	initScripts := append([]*CompiledScript(nil), t.initScripts...)
	old := t.js
	t.js = nil
	t.mu.Unlock()
	dropBitmapSourcesExcept(t.ID, doc)
	t.swDocumentCommitted(finalURL)
	// Freeing every object the previous page allocated takes milliseconds;
	// nothing can reach the old VM any more, so do it off the critical path.
	if old != nil {
		go old.close()
	}

	// Pages with no scripts, no inline handlers and no init scripts get no
	// VM at all: there is nothing to run, and a navigation stays at the cost
	// of a fetch and a parse. A VM is created on demand by Evaluate.
	if !t.eng.opts.DisableJS && t.scriptsAllowed() && (len(initScripts) > 0 || doc.NeedsScript()) {
		j := newJSVM(t, doc, finalURL)
		t.mu.Lock()
		t.js = j
		t.mu.Unlock()
		if j != nil {
			// addInitScript sources run BEFORE any page script, on every
			// navigation — the guarantee Playwright gives for hooking timers/fetch.
			for _, s := range initScripts {
				_, _ = j.run(s.Source, s.Name)
			}
			t.scanFrames() // the document is parsed: its frames start loading while its scripts run
			pl := newProgLoad(t, j, doc, finalURL, progressive)
			t.pl.Store(pl)
			if progressive {
				pl.prepaint()
				if t.pl.Load() == pl {
					t.tmark("first-paint-ready", "")
					return // the rest is driven by LoadStep
				}
				return
			}
			pl.run()
			return
		}
	}
	t.scanFrames()
	t.finishLoad(finalURL)
}

// finishLoad announces the load of the current document.
func (t *Tab) finishLoad(finalURL string) {
	t.stateRing.Push(Event{Tab: t.ID, Kind: EvLoad, At: time.Now(), Data: map[string]any{"url": finalURL, "title": t.Title()}})
	t.emit(EvLoad, map[string]any{"url": finalURL, "title": t.Title()})
}

// ---- public surface used by RPC/MCP handlers -----------------------------

func (t *Tab) HTML(pretty bool) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.dom == nil {
		return t.html
	}
	if pretty {
		var buf bytes.Buffer
		t.dom.Serialize(&buf)
		return buf.String()
	}
	return t.html
}

func (t *Tab) QuerySelector(sel string) (*NodeInfo, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.dom == nil {
		return nil, ErrNotFound
	}
	info, err := t.dom.QuerySelector(sel)
	if err != nil {
		return nil, err
	}
	return infoToNode(info), nil
}

func (t *Tab) QuerySelectorAll(sel string) ([]*NodeInfo, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.dom == nil {
		return nil, ErrNotFound
	}
	infos, err := t.dom.QuerySelectorAll(sel)
	if err != nil {
		return nil, err
	}
	out := make([]*NodeInfo, 0, len(infos))
	for _, i := range infos {
		out = append(out, infoToNode(i))
	}
	return out, nil
}

// Text returns innerText-like extraction for agents (structured, whitespace-normalised).
func (t *Tab) Text(sel string) (string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.dom == nil {
		return "", ErrNotFound
	}
	return t.dom.Text(sel)
}

// Snapshot returns an accessibility-style tree (roles/names/states) which is
// what AI agents should read instead of raw HTML — far fewer tokens.
func (t *Tab) Snapshot() ([]*NodeInfo, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.dom == nil {
		return nil, ErrNotFound
	}
	infos, err := t.dom.AriaSnapshot()
	if err != nil {
		return nil, err
	}
	out := make([]*NodeInfo, 0, len(infos))
	for _, i := range infos {
		out = append(out, infoToNode(i))
	}
	return out, nil
}

// SnapshotOptions mirrors net.SnapshotOptions for callers outside the engine.
type SnapshotOptions = fnet.SnapshotOptions

// AriaSnapshotV2 renders the Playwright-format accessibility tree with options.
func (t *Tab) AriaSnapshotV2(o SnapshotOptions) (string, error) {
	t.settle(0) // deliver queued socket/worker events and due timers first
	if o.Frame == nil {
		o.Frame = func(n *html.Node) (string, []string) { return t.snapshotFrame(n, o) }
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.dom == nil {
		return "", ErrNotFound
	}
	return t.dom.AriaSnapshotV2(o)
}

func (t *Tab) ConsoleLog(sinceSID int64) []Event { return t.consoleRing.Since(sinceSID) }
func (t *Tab) NetworkLog(sinceSID int64) []Event { return t.netRing.Since(sinceSID) }
func (t *Tab) StateLog(sinceSID int64) []Event   { return t.stateRing.Since(sinceSID) }

func (t *Tab) LogConsole(level, text string) { t.logConsoleAt(level, text, "") }

// logConsoleAt is LogConsole with the place the call came from ("url:line:col"), which Developer Tools shows.
func (t *Tab) logConsoleAt(level, text, loc string) {
	d := map[string]string{"level": level, "text": text}
	if loc != "" {
		d["loc"] = loc
	}
	e := t.consoleRing.Push(Event{Tab: t.ID, Kind: EvConsole, At: time.Now(), Data: d})
	t.emit(EvConsole, e.Data)
}

// Evaluate runs JS in the tab's VM, awaits a returned promise, and returns
// the JSON-encoded completion value. DOM nodes serialise as {nodeType,
// tagName, id, ref}.
func (t *Tab) Evaluate(expr string) (json.RawMessage, error) {
	if stageTrace {
		n0, d0 := t.LayoutStats()
		t0 := time.Now()
		defer func() {
			n1, d1 := t.LayoutStats()
			stageLog("eval", t.ID, time.Since(t0), "layouts=%d layout=%.2fms expr=%.40q %s", n1-n0, ms(d1-d0), expr, hostStatTake())
		}()
	}
	j, err := t.ensureJS()
	if err != nil {
		return nil, err
	}
	r, err := j.evaluate(expr)
	if strings.HasPrefix(expr, "__sweefScrollTo(") {
		t.scrollFromEval(expr) // the app's scroll notification doubles as the lazy-image trigger
	}
	return r, err
}

// ensureJS returns the tab's VM, binding one to the current document when the
// page loaded without scripts (lazy VM).
func (t *Tab) ensureJS() (*jsVM, error) { return t.ensureJSFor(false) }

// ensureFrameJS is ensureJS for a same-origin script reaching into this frame (contentWindow, frameElement). A frame
// sandboxed without allow-scripts still has a window object the embedder can read and call (a page that parks an
// <iframe sandbox="allow-same-origin" src="about:blank"> to borrow its History object does exactly that); only the
// frame's own scripts and inline handlers stay blocked, which the VM is told about.
func (t *Tab) ensureFrameJS() (*jsVM, error) { return t.ensureJSFor(true) }

func (t *Tab) ensureJSFor(remote bool) (*jsVM, error) {
	// the document can be replaced while the VM is built (a frame finishing its load): bind to the new one
	for try := 0; ; try++ {
		j, err := t.ensureJSOnce(remote)
		if err != errDocChanged || try >= 2 {
			return j, err
		}
	}
}

var errDocChanged = fmt.Errorf("document changed")

func (t *Tab) ensureJSOnce(remote bool) (*jsVM, error) {
	t.mu.RLock()
	j, d, u := t.js, t.dom, t.url
	t.mu.RUnlock()
	if j != nil {
		return j, nil
	}
	if t.eng.opts.DisableJS {
		return nil, fmt.Errorf("javascript is disabled (--no-js)")
	}
	blocked := !t.scriptsAllowed()
	if blocked && !remote {
		return nil, fmt.Errorf("scripts are blocked by the frame's sandbox")
	}
	if d == nil {
		return nil, fmt.Errorf("no document loaded")
	}
	docURL := "about:blank"
	if u != nil {
		docURL = u.String()
	}
	j = newJSVM(t, d, docURL)
	if j == nil {
		return nil, fmt.Errorf("javascript engine unavailable")
	}
	_, _ = j.call("__swSetDocument", docURL, "complete")
	if blocked {
		_, _ = j.call("__swBlockScripts")
	}
	t.mu.Lock()
	if t.js == nil && t.dom == d {
		t.js = j
	} else {
		other := t.js
		t.mu.Unlock()
		j.close()
		if other == nil {
			return nil, errDocChanged
		}
		return other, nil
	}
	t.mu.Unlock()
	return j, nil
}

// Click performs a semantic click on selector: focus, dispatch mousedown/up/click,
// follow link href or submit button form (Playwright actionability minus pixels).

// Fill sets a control's value with auto-wait and dispatches input/change
// into page JavaScript (Playwright fill semantics).
func (t *Tab) Fill(sel, value string) error { return t.Fill2(sel, value) }

// SubmitFormFor submits the form enclosing sel (GET navigates with the query,
// POST sends application/x-www-form-urlencoded) and installs the response.
func (t *Tab) SubmitFormFor(sel string) error {
	if c, in, ok := t.routeFrameSel(sel); ok {
		return c.SubmitFormFor(in)
	}
	if err := frameRefUnresolved(sel); err != nil {
		return err
	}
	t.mu.RLock()
	d := t.dom
	t.mu.RUnlock()
	if d == nil {
		return ErrNotFound
	}
	method, action, values, err := d.FormFor(sel)
	if err != nil {
		return err
	}
	bid, _, _ := d.BackendByEngine(sel)
	return t.submitFormTargeted(d, bid, method, action, values)
}

// SubmitFormBackend submits the form with (or enclosing) the given node id.
func (t *Tab) SubmitFormBackend(id int64) error {
	t.mu.RLock()
	d := t.dom
	t.mu.RUnlock()
	if d == nil {
		return ErrNotFound
	}
	method, action, values, err := d.FormForBackend(id)
	if err != nil {
		return err
	}
	return t.submitFormTargeted(d, id, method, action, values)
}

func (t *Tab) submitForm(method, action string, values map[string]string) error {
	u, _ := url.Parse(t.URL())
	if action != "" {
		au, err := url.Parse(action)
		if err != nil {
			return err
		}
		u = u.ResolveReference(au)
	}
	if !strings.EqualFold(method, "post") {
		q := u.Query()
		for k, v := range values {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
		return t.Navigate(u.String(), nil)
	}
	return t.postForm(u.String(), values)
}

func (t *Tab) postForm(rawurl string, values map[string]string) error {
	form := url.Values{}
	for k, v := range values {
		form.Set(k, v)
	}
	encoded := form.Encode()
	synth, method, target, hdrs, newBody, err := t.routedRequestNoBlock("POST", rawurl, nil, encoded)
	if err != nil {
		return err
	}
	if synth != nil {
		b, _ := io.ReadAll(synth.Body)
		t.SetDocument(string(b), rawurl)
		return nil
	}
	rawurl = target
	req, err := http.NewRequestWithContext(t.eng.ctx, method, rawurl, strings.NewReader(newBody))
	if err != nil {
		return err
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	t.resetDataForNavigation()
	t.touchNet()
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", t.UserAgent())
	t.setDocumentHeaders(req.Header, req.URL)
	for k, v := range t.ExtraHeaders() {
		req.Header.Set(k, v)
	}
	rec := fnet.RequestRecord{Method: "POST", URL: rawurl, Headers: flattenHeaders(req.Header)}
	t.netRing.Push(Event{Tab: t.ID, Kind: EvRequest, At: time.Now(), Data: rec})
	start := time.Now()
	req, _ = withMeter(req, t.Context(), t, KindDocument, t.policy())
	res, err := t.eng.clientFor(t.Context()).Do(req)
	if err != nil {
		t.netRing.Push(Event{Tab: t.ID, Kind: EvFailure, At: time.Now(), Data: map[string]string{"url": rawurl, "error": err.Error()}})
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	rec.Status = res.StatusCode
	rec.FinalURL = res.Request.URL.String()
	rec.DurationMs = time.Since(start).Milliseconds()
	rec.ResponseHeaders = flattenHeaders(res.Header)
	t.netRing.Push(Event{Tab: t.ID, Kind: EvResponse, At: time.Now(), Data: rec})
	final := res.Request.URL.String()
	t.commitCOOP(coopHeader(res.Header), final)
	t.SetDocument(string(b), final)
	t.mu.Lock()
	if fu, err := url.Parse(final); err == nil {
		if t.hIdx+1 < len(t.history) {
			t.history = t.history[:t.hIdx+1]
		}
		t.history = append(t.history, fu)
		t.hIdx = len(t.history) - 1
	}
	t.mu.Unlock()
	t.emit(EvNavigated, map[string]any{"url": final, "status": res.StatusCode})
	return nil
}

// Reload re-navigates to the current URL, bypassing cached state — like
// Playwright's page.reload().
func (t *Tab) Reload() error {
	t.mu.RLock()
	u := t.url
	t.mu.RUnlock()
	return t.Navigate(u.String(), map[string]string{"Cache-Control": "no-cache"})
}

func (t *Tab) GoBack() error    { return t.traverse(-1) }
func (t *Tab) GoForward() error { return t.traverse(+1) }

func (t *Tab) traverse(delta int) error {
	t.mu.RLock()
	next := t.hIdx + delta
	if next < 0 || next >= len(t.history) {
		t.mu.RUnlock()
		return nil // no-op like real browsers
	}
	u := t.history[next].String()
	t.hIdx = next
	t.mu.RUnlock()
	return t.Navigate(u, nil)
}

// Route installs an interception rule (glob on URL). handler may return a mock
// response body/status or abort the request.
func (t *Tab) Route(glob string, handler func(*http.Request) (int, string, bool)) {
	t.interceptMu.Lock()
	t.routes = append(t.routes, fnet.NewRoute(glob, handler))
	t.interceptMu.Unlock()
}

// Unroute removes one previously installed rule by its glob pattern.
func (t *Tab) Unroute(glob string) {
	t.interceptMu.Lock()
	var keep []*fnet.Route
	for _, r := range t.routes {
		if r.Pattern != glob {
			keep = append(keep, r)
		}
	}
	t.routes = keep
	t.interceptMu.Unlock()
}

func (t *Tab) UnrouteAll() {
	t.interceptMu.Lock()
	t.routes = nil
	t.interceptMu.Unlock()
}

// Screenshot renders the current document as structured text — the agent-first
// equivalent of a screenshot (token-cheap, deterministic). width<=0 uses the
// context viewport width.
func (t *Tab) Screenshot(width int) string {
	t.mu.RLock()
	dom := t.dom
	if width <= 0 {
		width = t.vpW
	}
	t.mu.RUnlock()
	if width <= 0 {
		width = 80
	}
	if dom == nil {
		return ""
	}
	return dom.RenderText(width)
}

// HAR exports the network ring in Chrome HAR 1.2 format.
func (t *Tab) HAR() ([]byte, error) {
	evs := t.netRing.All()
	type log struct {
		Version string               `json:"version"`
		Entries []fnet.RequestRecord `json:"entries"`
	}
	wrapper := struct {
		Log log `json:"log"`
	}{Log: log{Version: "1.2"}}
	for _, e := range evs {
		if e.Kind == EvResponse {
			if rec, ok := e.Data.(fnet.RequestRecord); ok {
				wrapper.Log.Entries = append(wrapper.Log.Entries, rec)
			}
		}
	}
	return json.Marshal(wrapper)
}

// infoToNode converts a DOM-layer Info into the engine's wire NodeInfo.
func infoToNode(i *fnet.Info) *NodeInfo {
	return &NodeInfo{
		NodeID:    i.NodeID,
		BackendID: i.BackendID,
		Name:      i.Name,
		ID:        i.ID,
		Type:      i.Type,
		Role:      i.Role,
		Text:      i.Text,
		Href:      i.Href,
		Visible:   i.Visible,
		Disabled:  i.Disabled,
		Checked:   i.Checked,
		Focusable: i.Focusable,
	}
}

// CompiledScript is a script ready for the JS runtime.
type CompiledScript struct {
	Source string
	Name   string
}

func compileScripts(srcs []fnet.Script) []*CompiledScript {
	out := make([]*CompiledScript, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, &CompiledScript{Source: s.Source, Name: s.Name})
	}
	return out
}

// interceptGet evaluates installed route handlers for a GET; returns
// (statusCode, body, matched). code==0 with matched==true means abort.
func (t *Tab) interceptGet(rawurl string) (int, string, bool) {
	t.interceptMu.RLock()
	routes := append([]*fnet.Route(nil), t.routes...)
	t.interceptMu.RUnlock()
	if len(routes) == 0 {
		return 0, "", false
	}
	req, _ := http.NewRequest(http.MethodGet, rawurl, nil)
	for _, r := range routes {
		if code, body, ok := fnet.MatchRoute(r, req); ok {
			return code, body, true
		}
	}
	return 0, "", false
}

// Dialogs records a JS dialog and applies the armed response like Playwright's
// page.on("dialog"). Returns whether it was accepted.
func (t *Tab) HandleDialog(d Dialog) (bool, string) {
	t.mu.Lock()
	acc, txt := t.dialogAccept, t.dialogText
	t.dialogAccept, t.dialogText = true, ""
	t.mu.Unlock()
	if d.Kind == "alert" || d.Kind == "beforeunload" {
		acc = true
	}
	t.dialogRing.Push(Event{Tab: t.ID, Kind: EvDialog, At: time.Now(),
		Data: map[string]any{"kind": d.Kind, "message": d.Message, "accepted": acc, "reply": txt}})
	t.emit(EvDialog, map[string]any{"dialog": d, "accepted": acc, "text": txt})
	return acc, txt
}

func (t *Tab) ArmDialogResponse(accept bool, promptText string) {
	t.mu.Lock()
	t.dialogAccept = accept
	t.dialogText = promptText
	t.mu.Unlock()
}

func (t *Tab) DialogLog(sinceSID int64) []Event { return t.dialogRing.Since(sinceSID) }

// Emulation setters/getters (Playwright parity).
func (t *Tab) SetViewport(w, h int, dsf float64, mobile, landscape bool) {
	t.mu.Lock()
	if w > 0 {
		t.vpW = w
	}
	if h > 0 {
		t.vpH = h
	}
	if dsf > 0 {
		t.dsf = dsf
	}
	t.mobile, t.landscape = mobile, landscape
	if t.dom != nil {
		t.dom.SetLayoutWidth(t.vpW)
	}
	t.mu.Unlock()
	t.stateRing.Push(Event{Tab: t.ID, Kind: EvDomMutated, At: time.Now(),
		Data: map[string]any{"action": "setViewport", "width": t.vpW, "height": t.vpH, "mobile": mobile}})
}

func (t *Tab) Viewport() (w, h int, dsf float64, mobile, landscape bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.vpW, t.vpH, t.dsf, t.mobile, t.landscape
}

func (t *Tab) SetUserAgent(ua string) { t.mu.Lock(); t.userAgent = ua; t.mu.Unlock() }

func (t *Tab) UserAgent() string {
	t.mu.RLock()
	ua := t.userAgent
	t.mu.RUnlock()
	if ua != "" {
		return ua
	}
	return t.eng.opts.DefaultUserAgent
}

func (t *Tab) SetGeolocation(lat, lng float64) {
	t.mu.Lock()
	t.geo = &[2]float64{lat, lng}
	t.mu.Unlock()
}

func (t *Tab) Geolocation() [2]float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.geo == nil {
		return [2]float64{}
	}
	return *t.geo
}

func (t *Tab) GrantPermissions(perms []string) {
	t.mu.Lock()
	if t.permissions == nil {
		t.permissions = map[string]bool{}
	}
	for _, p := range perms {
		t.permissions[p] = true
	}
	t.mu.Unlock()
}

func (t *Tab) PermissionsList() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []string
	for p, ok := range t.permissions {
		if ok {
			out = append(out, p)
		}
	}
	return out
}

func (t *Tab) SetOffline(on bool) {
	t.mu.Lock()
	t.offline = on
	t.mu.Unlock()
	t.stateRing.Push(Event{Tab: t.ID, Kind: EvDomMutated, At: time.Now(),
		Data: map[string]any{"action": "setOffline", "offline": on}})
}

func (t *Tab) IsOfflineMode() bool { t.mu.RLock(); defer t.mu.RUnlock(); return t.offline }

func (t *Tab) SetColorScheme(s string) { t.mu.Lock(); t.colorScheme = s; t.mu.Unlock() }

func (t *Tab) ColorSchemeName() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.colorScheme == "" {
		return "light"
	}
	return t.colorScheme
}

func (t *Tab) SetExtraHeaders(h map[string]string) { t.mu.Lock(); t.extraHdrs = h; t.mu.Unlock() }

func (t *Tab) ExtraHeaders() map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := map[string]string{}
	for k, v := range t.extraHdrs {
		out[k] = v
	}
	return out
}

func (t *Tab) AddInitScript(src string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Init scripts apply from the next navigation on, as in Playwright.
	id := fmt.Sprintf("init-%d", len(t.initScripts)+1)
	t.initScripts = append(t.initScripts, &CompiledScript{Source: src, Name: id})
	return id
}

func (t *Tab) RemoveInitScript(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var keep []*CompiledScript
	for _, s := range t.initScripts {
		if s.Name != id {
			keep = append(keep, s)
		}
	}
	t.initScripts = keep
}

func (t *Tab) ExposeBinding(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, b := range t.bindings {
		if b == name {
			return nil
		}
	}
	t.bindings = append(t.bindings, name)
	if j := t.js; j != nil {
		go j.installBindings() // define it in the live page too (outside t.mu)
	}
	return nil
}

func (t *Tab) Bindings() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]string(nil), t.bindings...)
}

func isHTMLResponse(h http.Header) bool {
	ct := strings.ToLower(h.Get("Content-Type"))
	return ct == "" || strings.Contains(ct, "html")
}

// readWithPreloadScan reads the document body and lets the preload scanner see it as it arrives.
func readWithPreloadScan(t *Tab, r io.Reader, finalURL string) ([]byte, error) {
	sc := t.startPreloadScan(finalURL)
	var buf bytes.Buffer
	chunk := make([]byte, 16<<10)
	var rerr error
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			sc.Write(chunk[:n])
		}
		if err != nil {
			if err != io.EOF {
				rerr = err
			}
			break
		}
	}
	sc.Close()
	if rerr != nil {
		return nil, rerr
	}
	return buf.Bytes(), nil
}
