package engine

// pwa.go: the background-capable web platform (Notifications, Web Push, Background Sync, Periodic Background
// Sync, Badging, Web Locks) on top of the service workers. One pwaState per BrowserContext.
//
//	pwa.go          options the embedder supplies, permission state and prompts, user activation, the bridge to JS
//	pwa_sync.go     Background Sync and Periodic Background Sync scheduling
//	pwa_locks.go    Web Locks
//	platformjs/pwa.js   the script-facing API (Notification, PushManager, SyncManager, navigator.locks, ...)
//
// The embedder (internal/app) configures a context with SetPWA. A context that was never configured behaves like
// a private window: Notification.permission is "denied", push cannot subscribe, sync and periodic sync do not
// fire. Web Locks and the badge work everywhere because they involve no permission.

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Permission names the engine asks the embedder about.
const (
	PermNotifications = "notifications"
	PermPush          = "push"
	PermPeriodicSync  = "periodic-background-sync"
)

// PermissionRequest is a question to the user.
type PermissionRequest struct {
	Origin     string
	Permission string // PermNotifications or PermPush
	TabID      string
	Gesture    bool // the page had a user gesture shortly before asking
}

// Badge is the state of an origin's app badge.
type Badge struct {
	Set   bool  // a badge is shown
	Count int64 // 0 with Set true is the plain "flag" badge
}

// PWAClock abstracts time for the schedulers so tests can control it.
type PWAClock interface {
	Now() time.Time
	// AfterFunc calls f once after d on its own goroutine (a fake clock may call it synchronously from Advance,
	// so f must not block) and returns a function that cancels it.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) func() bool {
	t := time.AfterFunc(d, f)
	return t.Stop
}

// SyncPolicy tunes Background Sync; the zero value is Chromium's.
type SyncPolicy struct {
	MaxAttempts   int           // attempts before a registration is dropped (3)
	InitialDelay  time.Duration // wait after the first failure (5 minutes)
	DelayFactor   float64       // growth of the wait per failure (3)
	PeriodicFloor time.Duration // least interval between two periodicsync events of an origin (12 hours)
	EventTimeout  time.Duration // how long a sync event may run (5 minutes)
}

func (p SyncPolicy) withDefaults() SyncPolicy {
	if p.MaxAttempts == 0 {
		p.MaxAttempts = 3
	}
	if p.InitialDelay == 0 {
		p.InitialDelay = 5 * time.Minute
	}
	if p.DelayFactor == 0 {
		p.DelayFactor = 3
	}
	if p.PeriodicFloor == 0 {
		p.PeriodicFloor = 12 * time.Hour
	}
	if p.EventTimeout == 0 {
		p.EventTimeout = 5 * time.Minute
	}
	return p
}

// PWAOptions connects a BrowserContext to the application around it.
type PWAOptions struct {
	// Permission reports "granted", "denied" or "prompt" for an origin and one of the Perm* names. Nil: "denied".
	Permission func(origin, name string) string
	// Request asks the user and returns the new state ("granted", "denied", or "prompt" when the user dismissed
	// the question). It runs on its own goroutine and may block until the user answers. Nil: nothing can be asked.
	Request func(ctx context.Context, r PermissionRequest) string
	// OpenWindow opens url in a new tab and returns it (clients.openWindow). Nil: openWindow rejects.
	OpenWindow func(url string) (*Tab, error)
	// FocusTab brings a tab to the front (WindowClient.focus, notification clicks).
	FocusTab func(*Tab)
	// NavigateTab navigates a tab (WindowClient.navigate); nil uses Tab.Navigate.
	NavigateTab func(t *Tab, url string) error
	// TabState reports whether a tab is the visible one and has focus (WindowClient.visibilityState / focused).
	TabState func(*Tab) (visible, focused bool)
	// DoNotDisturb, when it returns true, keeps notifications from being shown: the API behaves as if they were.
	DoNotDisturb func() bool
	// OnBadge is told when an origin's app badge changes.
	OnBadge func(origin string, b Badge)
	// Online reports connectivity; nil means online. SetOnline on the context can also be used.
	Online func() bool

	Clock        PWAClock
	Sync         SyncPolicy
	EventTimeout time.Duration // how long push and notification events may run (30 seconds)
	Logf         func(format string, args ...any)
}

// pwaState is the per-context state.
type pwaState struct {
	c       *BrowserContext
	enabled atomic.Bool
	o       PWAOptions
	clock   PWAClock
	pol     SyncPolicy
	online  atomic.Bool // set by SetOnline; ANDed with o.Online
	offline atomic.Bool
	// announced is the connectivity pages were last told about
	announced atomic.Bool

	mu       sync.Mutex
	subs     map[*jsVM]string // documents and workers following permission and connectivity changes
	badges   map[string]Badge
	data     map[string]*pwaOrigin // persisted per-origin data (pwa.json)
	inflight map[string][]chan string
	openOK   map[string]time.Time // registration key -> time until which clients.openWindow/focus are allowed
	closed   bool
	quit     chan struct{}
	wg       sync.WaitGroup

	locks *lockMgr
}

type pwaHolder struct {
	once sync.Once
	p    atomic.Pointer[pwaState]
}

// pwa returns the context's state, creating it on first use.
func (c *BrowserContext) pwa() *pwaState {
	c.pwah.once.Do(func() {
		st := &pwaState{c: c, clock: realClock{}, subs: map[*jsVM]string{},
			badges: map[string]Badge{}, data: map[string]*pwaOrigin{}, inflight: map[string][]chan string{}, openOK: map[string]time.Time{},
			quit: make(chan struct{})}
		st.online.Store(true)
		st.announced.Store(true)
		st.pol = SyncPolicy{}.withDefaults()
		st.locks = newLockMgr()
		c.pwah.p.Store(st)
	})
	return c.pwah.p.Load()
}

func (c *BrowserContext) pwaIfStarted() *pwaState { return c.pwah.p.Load() }

// SetPWA connects the context to the application. It may be called once, before pages that use these APIs load.
func (c *BrowserContext) SetPWA(o PWAOptions) {
	st := c.pwa()
	st.mu.Lock()
	if st.enabled.Load() {
		st.mu.Unlock()
		return
	}
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.EventTimeout == 0 {
		o.EventTimeout = 30 * time.Second
	}
	st.o, st.clock, st.pol = o, o.Clock, o.Sync.withDefaults()
	st.enabled.Store(true)
	st.mu.Unlock()
	st.startSync()
}

// PWA returns the control handle of the context.
func (c *BrowserContext) PWA() *PWA { return &PWA{c.pwa()} }

// PWA is what the application uses to tell the engine about changes, and to inspect what it holds.
type PWA struct{ st *pwaState }

// PermissionChanged tells open pages and workers of origin ("" = all) that a permission changed, so
// Notification.permission, PermissionStatus.onchange and the push permission state follow.
func (p *PWA) PermissionChanged(origin string) { p.st.permissionChanged(origin) }

// SetOnline sets connectivity: pending background syncs fire when it comes back, pages get online/offline events.
func (p *PWA) SetOnline(on bool) { p.st.setOnline(on) }

// Badge returns the app badge of an origin.
func (p *PWA) Badge(origin string) Badge {
	p.st.mu.Lock()
	defer p.st.mu.Unlock()
	return p.st.badges[origin]
}

// Badges returns every origin that has a badge.
func (p *PWA) Badges() map[string]Badge {
	p.st.mu.Lock()
	defer p.st.mu.Unlock()
	out := map[string]Badge{}
	for k, v := range p.st.badges {
		out[k] = v
	}
	return out
}

// ClearOrigin forgets what the engine holds for origin: push subscriptions (unregistered at the service),
// background sync registrations, the badge, shown notifications. "Clear browsing data" calls it.
func (p *PWA) ClearOrigin(origin string) { p.st.clearOrigin(origin) }

// ---- shutdown --------------------------------------------------------------------------------------------------------

func (c *BrowserContext) pwaShutdown() {
	st := c.pwaIfStarted()
	if st == nil {
		return
	}
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	st.closed = true
	close(st.quit)
	st.mu.Unlock()
	st.stopSync()
	st.locks.closeAll()
	st.wg.Wait()
}

// goJob runs f on a goroutine that shutdown waits for.
func (st *pwaState) goJob(f func()) {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	st.wg.Add(1)
	st.mu.Unlock()
	go func() {
		defer st.wg.Done()
		f()
	}()
}

// ---- permissions --------------------------------------------------------------------------------------------------------

func normPerm(s string) string {
	switch s {
	case "granted", "denied":
		return s
	}
	return "prompt"
}

// permission is the state of a permission for an origin: "granted", "denied" or "prompt".
func (st *pwaState) permission(origin, name string) string {
	if !st.enabled.Load() || !potentiallyTrustworthy(origin+"/") {
		return "denied"
	}
	switch name {
	case PermNotifications, PermPush:
		if st.o.Permission == nil {
			return "denied"
		}
		return normPerm(st.o.Permission(origin, PermNotifications))
	case "background-sync":
		return "granted"
	case PermClipboardRead:
		if st.o.Permission == nil {
			return "denied"
		}
		return normPerm(st.o.Permission(origin, PermClipboardRead))
	case PermPeriodicSync:
		if st.o.Permission == nil {
			return "denied"
		}
		if s := st.o.Permission(origin, PermPeriodicSync); s == "granted" {
			return "granted"
		}
		return "denied" // never prompts: it follows the site's engagement
	}
	return "prompt"
}

// request runs the permission flow of Notification.requestPermission() and PushManager.subscribe(). The result is
// "granted", "denied" or "default" (nothing was decided: no gesture, or the user dismissed the question).
func (st *pwaState) request(origin, name, tabID string, gesture bool) string {
	if name == PermPush {
		name = PermNotifications // one permission covers notifications and push
	}
	cur := st.permission(origin, name)
	if cur != "prompt" {
		return cur
	}
	if st.o.Request == nil {
		return "denied"
	}
	if !gesture {
		// Quiet UI: a page that asks without a user gesture is not allowed to put a question in front of the user.
		return "default"
	}
	key := origin + "|" + name
	ch := make(chan string, 1)
	st.mu.Lock()
	if w, busy := st.inflight[key]; busy {
		st.inflight[key] = append(w, ch)
		st.mu.Unlock()
		return <-ch
	}
	st.inflight[key] = []chan string{ch}
	st.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-st.quit:
			cancel()
		case <-ctx.Done():
		}
	}()
	res := normPerm(st.o.Request(ctx, PermissionRequest{Origin: origin, Permission: name, TabID: tabID, Gesture: gesture}))
	cancel()
	if res == "prompt" {
		res = "default"
	}
	st.mu.Lock()
	waiters := st.inflight[key]
	delete(st.inflight, key)
	st.mu.Unlock()
	for _, w := range waiters {
		w <- res
	}
	return res
}

func (st *pwaState) permissionChanged(origin string) {
	st.mu.Lock()
	var vms []*jsVM
	for j, o := range st.subs {
		if origin == "" || o == origin {
			vms = append(vms, j)
		}
	}
	st.mu.Unlock()
	for _, j := range vms {
		pushPWA(j, map[string]any{"t": "perm"})
	}
	if origin == "" {
		return
	}
	if st.permission(origin, PermPush) != "granted" {
	}
}

func (st *pwaState) setOnline(on bool) {
	st.offline.Store(!on)
	changed := st.announced.Swap(on) != on
	if on {
		st.syncOnline() // also when we were never told it went away: PWAOptions.Online may have said so
	}
	if !changed {
		return
	}
	for _, t := range st.c.eng.ListTabs() {
		if t.Context() != st.c {
			continue
		}
		t.mu.RLock()
		j := t.js
		t.mu.RUnlock()
		if j != nil {
			pushPWA(j, map[string]any{"t": "online", "online": on})
		}
	}
}

func (st *pwaState) isOnline() bool {
	if st.offline.Load() {
		return false
	}
	if f := st.o.Online; f != nil {
		return f()
	}
	return true
}

// ---- user activation ------------------------------------------------------------------------------------------------

// pwaTab is the part of a tab that the background APIs need.
type pwaTab struct{ activatedAt atomic.Int64 }

// transientActivation is how long a user gesture counts (Chromium: 5 seconds).
const transientActivation = 5 * time.Second

func (p *pwaTab) activate() { p.activatedAt.Store(time.Now().UnixNano()) }

func (p *pwaTab) active() bool {
	at := p.activatedAt.Load()
	return at != 0 && time.Since(time.Unix(0, at)) < transientActivation
}

// MarkUserActivation records a trusted user input (a click or key press delivered by the window shell). Page scripts
// cannot call it, which is why the application reports input here instead of the page's event handlers doing so.
func (t *Tab) MarkUserActivation() { t.pw.activate() }

// UserActive reports whether the tab's page had a user gesture within the last five seconds.
func (t *Tab) UserActive() bool { return t.pw.active() }

// ---- JS bridge --------------------------------------------------------------------------------------------------------

func init() {
	registerHostExtension("pwa", func(j *jsVM) map[string]hostFn { return (&pwaHost{j: j}).fns() })
	registerAsyncHandler("pwa", func(j *jsVM, e asyncEvent) {
		if _, err := j.call("__pwaEvent", e.data); err != nil && j.tab != nil {
			j.tab.LogConsole("error", jsErrorText(err, "pwa event"))
		}
		j.jobs()
	})
	registerCloseHook(func(j *jsVM) {
		if j.tab == nil {
			return
		}
		st := j.tab.Context().pwaIfStarted()
		if st == nil {
			return
		}
		st.mu.Lock()
		delete(st.subs, j)
		st.mu.Unlock()
		st.dropVM(j)
	})
}

// pushPWA delivers a JSON message to a VM's event loop.
func pushPWA(j *jsVM, msg map[string]any) {
	b, err := json.Marshal(msg)
	if err != nil || j == nil {
		return
	}
	j.rt().push(asyncEvent{kind: "pwa", typ: "event", data: string(b)})
	if t := j.tab; t != nil {
		t.wake()
	}
}

// dropVM releases what a closed VM owned.
func (st *pwaState) dropVM(j *jsVM) {
	st.locks.dropVM(j)
}

type pwaHost struct{ j *jsVM }

func (h *pwaHost) st() *pwaState { return h.j.tab.Context().pwa() }

func (h *pwaHost) origin() string {
	if h.j.tab == nil {
		return ""
	}
	return siteOriginOf(h.j.tab.URL())
}

// secure reports whether the VM's document or worker is a secure context.
func (h *pwaHost) secure() bool {
	return h.j.tab != nil && h.origin() != "" && potentiallyTrustworthy(h.j.tab.URL())
}

func (h *pwaHost) worker() *swRuntime {
	if h.j.tab == nil {
		return nil
	}
	return h.j.tab.sw.rt
}

// reply sends the outcome of an asynchronous host call back to the VM.
func (h *pwaHost) reply(token int64, v any, err error) {
	if err != nil {
		pushPWA(h.j, errToResult(token, err))
		return
	}
	pushPWA(h.j, map[string]any{"t": "result", "token": token, "ok": true, "value": v})
}

func (h *pwaHost) fns() map[string]hostFn {
	f := map[string]hostFn{
		"pwaSecure": func(a []any) (any, error) { return h.secure(), nil },
		"pwaPrivate": func(a []any) (any, error) {
			return !h.st().enabled.Load(), nil
		},
		"pwaSubscribe": func(a []any) (any, error) {
			if h.j.tab == nil {
				return nil, nil
			}
			st := h.st()
			st.mu.Lock()
			st.subs[h.j] = h.origin()
			st.mu.Unlock()
			return nil, nil
		},
		"pwaPermission": func(a []any) (any, error) {
			if h.j.tab == nil {
				return "denied", nil
			}
			return h.st().permission(h.origin(), argS(a, 0)), nil
		},
		"pwaRequestPermission": func(a []any) (any, error) {
			token, name := argI(a, 0), argS(a, 1)
			t := h.j.tab
			if t == nil || !h.secure() {
				h.reply(token, "denied", nil)
				return nil, nil
			}
			st := h.st()
			origin, gesture := h.origin(), t.UserActive()
			st.goJob(func() { h.reply(token, st.request(origin, name, t.ID, gesture), nil) })
			return nil, nil
		},
		"pwaOnline": func(a []any) (any, error) {
			if h.j.tab == nil {
				return true, nil
			}
			return !h.j.tab.IsOfflineMode() && h.st().isOnline(), nil
		},
		"pwaUserActive": func(a []any) (any, error) { return h.j.tab != nil && h.j.tab.UserActive(), nil },
		"pwaBadge": func(a []any) (any, error) {
			// pwaBadge(kind, n): kind "set" with n >= 0 (0 = flag), kind "clear"
			if h.j.tab == nil || !h.secure() {
				return nil, nil
			}
			h.st().setBadge(h.origin(), argS(a, 0) == "set", argI(a, 1))
			return nil, nil
		},
	}
	h.syncFns(f)
	h.notifyFns(f)
	h.pushFns(f)
	h.locksFns(f)
	h.clientFns(f)
	return f
}

func (st *pwaState) setBadge(origin string, set bool, n int64) {
	b := Badge{}
	if set {
		b = Badge{Set: true, Count: n}
	}
	st.mu.Lock()
	old := st.badges[origin]
	if set {
		st.badges[origin] = b
	} else {
		delete(st.badges, origin)
	}
	cb := st.o.OnBadge
	st.mu.Unlock()
	if cb != nil && old != b {
		cb(origin, b)
	}
}

// ---- per-origin persisted data ----------------------------------------------------------------------------------------

// pwaOrigin is what is kept on disk for an origin (<origin storage dir>/pwa.json).
type pwaOrigin struct {
	Sync     []*syncReg     `json:"sync,omitempty"`
	Periodic []*periodicReg `json:"periodic,omitempty"`
}

func (st *pwaState) originFile(origin string) string {
	root := st.c.SiteStorage()
	if !root.Persistent() {
		return ""
	}
	dir := filepath.Dir(root.Origin(origin).SWDir())
	if dir == "" || dir == "." {
		return ""
	}
	return filepath.Join(dir, "pwa.json")
}

// originData returns the origin's data, reading it from disk the first time (st.mu is not held).
func (st *pwaState) originData(origin string) *pwaOrigin {
	st.mu.Lock()
	d := st.data[origin]
	st.mu.Unlock()
	if d != nil {
		return d
	}
	d = &pwaOrigin{}
	if p := st.originFile(origin); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, d)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if cur := st.data[origin]; cur != nil {
		return cur
	}
	st.data[origin] = d
	// Whoever loads an origin's data first arms its persisted registrations: the start-up scan may not have listed the
	// origin yet (or may run after a page already asked), and a registration nobody armed would never fire.
	st.armLocked(origin, d)
	return d
}

// saveOrigin writes the origin's data. The caller holds st.mu (the structures are shared with the schedulers).
func (st *pwaState) saveOriginLocked(origin string) {
	d := st.data[origin]
	p := st.originFile(origin)
	if d == nil || p == "" {
		return
	}
	if len(d.Sync) == 0 && len(d.Periodic) == 0 {
		_ = os.Remove(p)
		return
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil || os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
		st.c.SiteStorage().Origin(origin).Touch()
	}
}

// allOrigins lists the origins that have persisted data.
func (st *pwaState) allOrigins() []string {
	var out []string
	seen := map[string]bool{}
	for _, o := range st.c.SiteStorage().Origins() {
		if p := st.originFile(o); p != "" {
			if _, err := os.Stat(p); err == nil && !seen[o] {
				seen[o] = true
				out = append(out, o)
			}
		}
	}
	st.mu.Lock()
	for o := range st.data {
		if !seen[o] {
			out = append(out, o)
		}
	}
	st.mu.Unlock()
	return out
}

func (st *pwaState) clearOrigin(origin string) {
	if origin == "" { // every origin
		for _, o := range st.allOrigins() {
			st.clearOrigin(o)
		}
		st.mu.Lock()
		var badged []string
		for o := range st.badges {
			badged = append(badged, o)
		}
		st.mu.Unlock()
		for _, o := range badged {
			st.setBadge(o, false, 0)
		}
		return
	}
	st.clearSync(origin)
	st.setBadge(origin, false, 0)
	st.mu.Lock()
	delete(st.data, origin)
	st.mu.Unlock()
}

// registrationGone is called when a service worker registration is unregistered: its push subscription and its
// sync registrations go with it.
func (c *BrowserContext) pwaRegistrationGone(origin, scope string) {
	st := c.pwaIfStarted()
	if st == nil {
		return
	}
	st.dropRegistration(origin, scope)
}

func (st *pwaState) dropRegistration(origin, scope string) {
	st.dropSyncOf(origin, scope)
}

// pwaOpenAllowed marks the window in which a worker may open or focus windows (during a notification click).
func (st *pwaState) allowOpen(regKey string, d time.Duration) {
	st.mu.Lock()
	st.openOK[regKey] = time.Now().Add(d)
	st.mu.Unlock()
}

func (st *pwaState) openAllowed(regKey string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return time.Now().Before(st.openOK[regKey])
}

func originHost(origin string) string {
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
}
