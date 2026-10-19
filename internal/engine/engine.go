// Package engine is Sweef's browser core. It is intentionally *not* a fork
// of Blink/WebKit (which cost >100 MiB RSS before loading a page). Instead it
// is an agent-first micro-engine:
//
//   - HTML5-compliant tokenizer/tree builder (golang.org/x/net/html)
//   - CSS selector + cascade resolution for query/computed-style needs
//   - A real JS runtime via embedded QuickJS (cgo, ~1.5 MiB heap idle)
//   - Network stack with cookie jar, cache, HAR capture, request interception
//   - Structured text renderer that fits 320x200 and up; optional PNG blit
//
// Everything is exposed as small goroutine-per-tab state machines so a tab
// costs < 2 MiB instead of Chromium's ~60 MiB renderer process.
package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNoTab    = errors.New("engine: no such tab")
	ErrClosed   = errors.New("engine: closed")
	ErrNotFound = errors.New("engine: node not found")
	ErrTimeout  = errors.New("engine: timeout waiting for condition")
)

// Options tune the whole browser instance for low-memory machines.
type Options struct {
	MaxRSSMiB        int  // soft resident-memory budget (memgov.go): GC pressure, ring trimming and memory returned to the OS above it
	MaxCacheMiB      int  // HTTP memory cache ceiling (default 32)
	DiskCacheMiB     int  // profile-level on-disk HTTP cache cap under CacheDir/http (0 = 100, negative = memory only)
	Headless         bool // always true today; "headful" streams to a thin client
	DisableJS        bool // pure fetch+DOM mode for tiny agents
	ConsoleRingSize  int  // console messages retained per tab
	NetworkRingSize  int  // network events retained per tab
	StateRingSize    int  // actionability/state log entries retained
	DefaultUserAgent string
	ProxyURL         string // HTTP/HTTPS/SOCKS5 proxy for every context ("" = HTTPS_PROXY/HTTP_PROXY/NO_PROXY from the environment, "direct" = none)
	AcceptLanguage   string // Accept-Language of every request (default "en-US,en;q=0.9"); see Engine.SetAcceptLanguage
	ViewportWidth    int    // default viewport width (px) for text rendering
	ViewportHeight   int    // default viewport height (px)
	SettleMs         int    // event-loop budget after actions (default 1500; an action settles for a third of it)
	NavSettleMs      int    // event-loop budget of a headless load: half before the load event, all after (default 600, at most SettleMs)
	SettleQuietMs    int    // a headless settle ends early once only timers are left and the document has not changed for this long (0 = default 300; negative = off, pump to SettleMs)
	NavQuietMs       int    // quiet window of a headless navigation's pre-load and settle pumps: they end once only timers are left and the document has not changed for this long (0 = SettleQuietMs, the library default; > 0 also caps the pre-load pump's window at 10 ms)
	NavPostLoadMs    int    // event-loop budget of a headless load after the load event (0 = NavSettleMs); the page goes on in the background pump and the next command's settle
	JSMemoryMiB      int    // QuickJS heap cap per tab (default DefaultJSMemoryMiB)
	JSEngine         string // JavaScript runtime: "quickjs" (default), "v8" or "auto" (V8 when its library loads, else QuickJS); SWEEF_JS_ENGINE overrides (jsruntime.go)
	JSTimeoutMs      int    // single-eval timeout; a looping script is interrupted (default 30000: a real page's longest tasks, YouTube's first render among them, run past 5 s on a loaded machine, and an interrupted task leaves the page half-built)
	WarmVMs          int    // JS VMs kept pre-built so navigations skip prelude setup (default 1)
	DownloadsDir     string // where downloads are saved (default $TMPDIR/sweef-downloads)
	CacheDir         string // on-disk bytecode cache ("" = $SWEEF_CACHE_DIR or user cache dir; "off" disables)
	StorageDir       string // profile storage root of the default context: IndexedDB, Cache Storage, service workers ("" = memory only; other contexts: ContextOptions.StorageDir)
	StorageQuotaMiB  int    // hard cap per origin for all site storage in MiB (0 = 20% of the budget)
	StorageBudgetMiB int    // global budget of all origins in MiB (0 = 60% of the profile volume's free space, at most 64 GiB); see docs/storage.md
	DisableFrames    bool   // do not load <iframe> documents (frames.go): pages see no content window, as before frames existed
	BlockTrackers    bool   // refuse third-party requests to known ad, analytics and tracking hosts (trackers.go); the agent-facing commands turn it on
	HideScrollbars   bool   // lay out and paint without scrollbars, as headless Chromium under Playwright/CDP does; the agent-facing commands turn it on
	NoAutoplay       bool   // media do not preload or autoplay (the page's own play() still loads them), as under the Data Saver; the agent-facing commands turn it on: a background video cost ~150 MB of decoded frames and a core of CPU for nothing an agent reads
}

func DefaultOptions() Options {
	return Options{
		MaxRSSMiB:        DefaultMaxRSSMiB,
		MaxCacheMiB:      32,
		Headless:         true,
		ConsoleRingSize:  512,
		NetworkRingSize:  512,
		StateRingSize:    256,
		DefaultUserAgent: "Sweef/0.1 (agent; lightweight)",
		ViewportWidth:    1280,
		ViewportHeight:   720,
		SettleMs:         1500,
		JSMemoryMiB:      DefaultJSMemoryMiB,
		JSTimeoutMs:      30000,
		WarmVMs:          1,
	}
}

// EventKind enumerates everything SDKs/MCP can subscribe to.
type EventKind string

const (
	EvNavigated  EventKind = "Page.navigate"
	EvLoad       EventKind = "Page.load"
	EvDomMutated EventKind = "DOM.nodeInserted"
	EvConsole    EventKind = "Log.entry"
	EvRequest    EventKind = "Network.request"
	EvResponse   EventKind = "Network.response"
	EvFailure    EventKind = "Network.failure"
	EvDialog     EventKind = "Page.javascript dialog"
	EvPopup      EventKind = "Page.popup"
	EvCrash      EventKind = "Page.crashed"
	EvDownload   EventKind = "Browser.download"
	EvBinding    EventKind = "Runtime.bindingCalled"
	EvWebSocket  EventKind = "Network.webSocket"
)

// Event is one broadcast entry. Ring buffers store these verbatim so history
// queries are O(n) over a bounded window, never unbounded.
type Event struct {
	SID  int64     `json:"sid"`
	Tab  string    `json:"tab"`
	Kind EventKind `json:"kind"`
	At   time.Time `json:"at"`
	Data any       `json:"data,omitempty"`
}

// Bus fans out events to subscribers (MCP bridge, CDP shim, SDK sockets).
type Bus struct {
	mu   sync.RWMutex
	subs map[uint64]chan Event
	next uint64
}

func NewBus() *Bus { return &Bus{subs: map[uint64]chan Event{}} }

func (b *Bus) Subscribe(depth int) (uint64, <-chan Event) {
	if depth <= 0 {
		depth = 64
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Event, depth)
	b.subs[id] = ch
	return id, ch
}

func (b *Bus) Unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		close(ch)
		delete(b.subs, id)
	}
}

// hasSubscribers reports whether anyone listens (data events are not even built otherwise).
func (b *Bus) hasSubscribers() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs) > 0
}

func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // slow consumer drops; ring buffer still holds history
		}
	}
}

// Engine owns tabs, the event bus, and global resources (cookie jar, cache).
type Engine struct {
	opts Options
	bus  *Bus

	mu     sync.RWMutex
	tabs   map[string]*Tab
	order  []string
	nextID atomic.Int64

	httpClient *http.Client // lazily built: cookie jar + bounded transport
	wire       wireCounters // socket totals of every context (see wire.go)

	// Context isolation (Playwright BrowserContext parity)
	contexts   map[string]*BrowserContext
	defaultCtx *BrowserContext
	jar        *Jar
	metrics    *Metrics
	started    time.Time
	vms        *vmPool
	res        *httpCache
	acceptLang atomic.Value // string: Engine.SetAcceptLanguage
	code       *codeCache
	v8code     *codeCache     // the V8 runtime's code cache (jsrt_v8.go); nil on QuickJS
	v8pc       *v8Precompiler // produces it off the page's thread
	pc         *precompiler   // background compiles into code (precompile.go)

	wm winRegistry // window ids and the embedder's popup / close / focus hooks (windows.go)
	// frameRead decides whether the content of a frame document may be read through the
	// snapshot and acted on through refs (the embedder's per-site rules for agents); nil = all
	frameRead atomic.Pointer[func(url string) bool]

	ctx    context.Context
	cancel context.CancelFunc
	closed atomic.Bool
}

func New(opts Options) *Engine {
	kind, note, jerr := ResolveJSEngine(opts)
	if jerr != nil {
		jsEngineNoteOnce.Do(func() { fmt.Fprintln(os.Stderr, "sweef: "+jerr.Error()) })
	}
	if note != "" {
		jsEngineNoteOnce.Do(func() { fmt.Fprintln(os.Stderr, note) })
	}
	if kind == "" {
		kind = JSEngineQuickJS
	}
	opts.JSEngine = kind // the resolved runtime; "v8" without a loadable library fails each VM creation loudly
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		opts:     opts,
		bus:      NewBus(),
		tabs:     map[string]*Tab{},
		contexts: map[string]*BrowserContext{},
		jar:      NewJar(),
		metrics:  NewMetrics(opts.MaxRSSMiB),
		started:  time.Now(),
		ctx:      ctx,
		cancel:   cancel,
	}
	cacheDir := opts.CacheDir
	if cacheDir == "" {
		cacheDir = defaultCacheDir()
	} else if cacheDir == "off" {
		cacheDir = ""
	}
	e.code = newCodeCache(cacheDir)
	e.pc = newPrecompiler(e.code)
	setPreludeCacheDir(e.code.dir)
	if opts.JSEngine == JSEngineV8 {
		e.pc.off = true
		e.v8code = newCodeCacheAt(cacheDir, "v8code-"+sanitizeVersion(v8CacheTag()), ".v8cc")
		e.v8pc = newV8Precompiler(e.v8code)
		if d := os.Getenv("SWEEF_V8_SNAP_DIR"); d != "" {
			setV8SnapDir(d)
		} else if cacheDir != "" {
			setV8SnapDir(filepath.Join(cacheDir, "v8"))
		}
	}
	e.res = newHTTPCache(opts.MaxCacheMiB, cacheDir, opts.DiskCacheMiB)
	e.vms = newVMPool(opts)
	e.startMemGovernor()
	return e
}

func (e *Engine) Bus() *Bus                { return e.bus }
func (e *Engine) Opts() Options            { return e.opts }
func (e *Engine) Context() context.Context { return e.ctx }

// emit broadcasts an engine-level event (no tab) onto the bus.
func (e *Engine) emit(kind EventKind, data any) {
	e.bus.Publish(Event{Kind: kind, At: time.Now(), Data: data})
}

// now is a clock indirection so tests can pin time.
func (e *Engine) now() time.Time { return time.Now() }

// NewTabInContext creates a tab bound to a specific BrowserContext; tabs
// inherit the context's UA/viewport/cookies and register with its tab list.
func (e *Engine) NewTabInContext(c *BrowserContext, url string) (*Tab, error) {
	if e.closed.Load() {
		return nil, ErrClosed
	}
	e.mu.Lock()
	t := newTab(e, e.newTabID())
	t.ctx = c
	e.tabs[t.ID] = t
	e.order = append(e.order, t.ID)
	e.mu.Unlock()
	// apply context emulation before first navigation
	if o := c.Options(); o.UserAgent != "" || o.ViewportWidth > 0 {
		t.SetViewport(o.ViewportWidth, o.ViewportHeight, o.DeviceScaleFactor, o.Mobile, o.Landscape)
		if o.UserAgent != "" {
			t.SetUserAgent(o.UserAgent)
		}
		if len(o.ExtraHeaders) > 0 {
			t.SetExtraHeaders(o.ExtraHeaders)
		}
		t.SetOffline(o.Offline)
		if o.ColorScheme != "" {
			t.SetColorScheme(o.ColorScheme)
		}
		if len(o.Permissions) > 0 {
			t.GrantPermissions(o.Permissions)
		}
		if o.Geolocation != nil {
			t.SetGeolocation(o.Geolocation[0], o.Geolocation[1])
		}
	}
	if url != "" && url != "about:blank" {
		if err := t.Navigate(url, nil); err != nil {
			return t, err
		}
	} else {
		t.SetDocument("<html><head><title>New Tab</title></head><body></body></html>", "about:blank")
	}
	return t, nil
}

func (e *Engine) newTabID() string {
	return fmt.Sprintf("tab-%d", e.nextID.Add(1))
}

// NewTab creates a blank tab. Cost target: < 2 MiB resident.
func (e *Engine) NewTab(url string) (*Tab, error) {
	if e.closed.Load() {
		return nil, ErrClosed
	}
	e.mu.Lock()
	t := newTab(e, e.newTabID())
	e.tabs[t.ID] = t
	e.order = append(e.order, t.ID)
	e.mu.Unlock()
	if url != "" && url != "about:blank" {
		if err := t.Navigate(url, nil); err != nil {
			return t, err
		}
	} else {
		t.SetDocument("<html><head><title>New Tab</title></head><body></body></html>", "about:blank")
	}
	return t, nil
}

func (e *Engine) Tab(id string) (*Tab, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	t, ok := e.tabs[id]
	if !ok {
		return nil, ErrNoTab
	}
	return t, nil
}

// ActiveTab returns the most recently created tab (single-window semantics).
func (e *Engine) ActiveTab() (*Tab, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(e.order) == 0 {
		return nil, ErrNoTab
	}
	return e.tabs[e.order[len(e.order)-1]], nil
}

func (e *Engine) ListTabs() []*Tab {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*Tab, 0, len(e.order))
	for _, id := range e.order {
		out = append(out, e.tabs[id])
	}
	return out
}

func (e *Engine) CloseTab(id string) error {
	e.mu.Lock()
	t, ok := e.tabs[id]
	if ok {
		delete(e.tabs, id)
		for i, o := range e.order {
			if o == id {
				e.order = append(e.order[:i], e.order[i+1:]...)
				break
			}
		}
	}
	e.mu.Unlock()
	if !ok {
		return ErrNoTab
	}
	t.close()
	return nil
}

func (e *Engine) Close() error {
	if !e.closed.CompareAndSwap(false, true) {
		return nil
	}
	e.pc.close()
	if e.v8pc != nil {
		e.v8pc.close()
	}
	e.mu.Lock()
	tabs := make([]*Tab, 0, len(e.tabs))
	for _, t := range e.tabs {
		tabs = append(tabs, t)
	}
	e.tabs = map[string]*Tab{}
	e.order = nil
	ctxs := make([]*BrowserContext, 0, len(e.contexts))
	for _, c := range e.contexts {
		ctxs = append(ctxs, c)
	}
	e.contexts = map[string]*BrowserContext{}
	e.defaultCtx = nil
	e.mu.Unlock()
	for _, t := range tabs {
		t.close()
	}
	for _, c := range ctxs {
		c.siteClose()
	}
	e.cancel()
	e.vms.close()
	e.res.close()
	// keep-alive connections would otherwise outlive the engine (their reader
	// goroutines wait for IdleConnTimeout)
	e.mu.Lock()
	hc := e.httpClient
	e.mu.Unlock()
	sweep := func() {
		if hc != nil {
			hc.CloseIdleConnections()
		}
		for _, c := range ctxs {
			c.closeIdle()
		}
	}
	if hc != nil || len(ctxs) > 0 {
		sweep()
		// Requests that were in flight at Close (favicons, subresources) turn idle only when they
		// finish, after the sweep above; sweep again so their connections do not linger either.
		for _, d := range []time.Duration{300 * time.Millisecond, 1500 * time.Millisecond} {
			time.AfterFunc(d, sweep)
		}
	}
	return nil
}

// Reload re-fetches the current URL in place (page.reload equivalent).
