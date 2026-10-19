package engine

// frames_host.go: the host functions behind frame_prelude.js. Windows are addressed by Tab.winID; a script can
// only reach windows of its own page tree (same top-level tab), and can touch the insides of one only when the
// two documents have the same, non-opaque origin.

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	fnet "github.com/sweefbrowser/sweef/internal/net"
)

//go:embed frame_prelude.js.z
var framePreludeJSZ string

func init() {
	registerPreludeZ("frames", framePreludeJSZ)
	registerHostExtension("frames", func(j *jsVM) map[string]hostFn { return (&frHost{j: j}).fns() })
	registerAsyncHandler("frame", func(j *jsVM, e asyncEvent) {
		if _, err := j.call("__swFrameEvent", e.data); err != nil && j.tab != nil {
			j.tab.LogConsole("error", jsErrorText(err, "frame event"))
		}
		j.jobs()
	})
}

// pushFrame queues an event for a VM's frame_prelude.js.
func pushFrame(j *jsVM, msg map[string]any) {
	b, err := json.Marshal(msg)
	if err != nil || j == nil {
		return
	}
	j.rt().push(asyncEvent{kind: "frame", typ: "event", data: string(b)})
	if t := j.tab; t != nil {
		t.wake()
	}
}

var errFrameBusy = errors.New("TimeoutError: the other window is busy")

// callWait calls a prelude function of j, waiting up to d for the VM to be free. Two windows calling into each
// other at the same moment would otherwise wait for each other forever.
func (j *jsVM) callWait(d time.Duration, fn string, args ...any) (any, error) {
	deadline := time.Now().Add(d)
	for !j.mu.TryLock() {
		if time.Now().After(deadline) {
			return nil, errFrameBusy
		}
		time.Sleep(200 * time.Microsecond)
	}
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrClosed
	}
	return j.jr.callLocked(fn, args...)
}

func (t *Tab) scriptsAllowed() bool {
	return t.bc == nil || t.bc.sandbox&sbSandboxed == 0 || t.bc.sandbox&sbScripts != 0
}

// windowName is the browsing context name (the iframe's name attribute until the page changes it).
func (t *Tab) windowName() string {
	if t.bc != nil {
		return t.bc.name
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.win.name
}

type frHost struct{ j *jsVM }

func (h *frHost) me() *Tab { return h.j.tab }

// reach resolves a window id to a Tab of the caller's own page tree.
func (h *frHost) reach(win int64) *Tab {
	me := h.me()
	if me == nil {
		return nil
	}
	v, ok := winReg.Load(win)
	if !ok {
		return nil
	}
	t := v.(*Tab)
	if t.closed.Load() {
		return nil
	}
	a, b := me.topTab(), t.topTab()
	if a == nil || b == nil || (a != b && !windowsRelated(a, b)) {
		return nil // another page tree, unless it is the opener or a popup of this one (windows.go)
	}
	return t
}

// canonOrigin drops default ports so that "http://a:80" and "http://a" compare equal.
func canonOrigin(o string) string {
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return o
	}
	h := strings.ToLower(u.Hostname())
	p := u.Port()
	if (u.Scheme == "http" && p == "80") || (u.Scheme == "https" && p == "443") {
		p = ""
	}
	if p != "" {
		h += ":" + p
	}
	return strings.ToLower(u.Scheme) + "://" + h
}

func sameOriginTabs(a, b *Tab) bool {
	oa, ob := a.docOrigin(), b.docOrigin()
	return oa != "" && ob != "" && canonOrigin(oa) == canonOrigin(ob)
}

func originOrNull(t *Tab) string {
	if o := t.docOrigin(); o != "" {
		return canonOrigin(o)
	}
	return "null"
}

func (h *frHost) fns() map[string]hostFn {
	return map[string]hostFn{
		"frSelf": func(a []any) (any, error) {
			t := h.me()
			if t == nil {
				return map[string]any{"win": 0, "parent": 0, "top": 0, "framed": false}, nil
			}
			out := map[string]any{"win": t.winID, "parent": int64(0), "top": t.winID, "framed": false}
			if t.isFrame() {
				out["parent"], out["top"], out["framed"] = t.bc.parent.winID, t.bc.top.winID, true
			}
			return out, nil
		},
		"frOf": func(a []any) (any, error) {
			t := h.me()
			if t == nil {
				return int64(0), nil
			}
			if k := t.ensureFrame(argI(a, 0)); k != nil {
				return k.winID, nil
			}
			return int64(0), nil
		},
		"frChildren": func(a []any) (any, error) {
			t := h.reach(argI(a, 0))
			out := []any{}
			if t == nil {
				return out, nil
			}
			for _, k := range t.FrameTabs() {
				out = append(out, k.winID)
			}
			return out, nil
		},
		"frInfo": func(a []any) (any, error) {
			t := h.reach(argI(a, 0))
			if t == nil {
				return map[string]any{"closed": true, "parent": int64(0), "top": int64(0), "length": 0}, nil
			}
			out := map[string]any{"closed": t.WindowClosed(), "parent": int64(0), "top": t.topTab().winID, "length": len(t.FrameTabs()), "opener": int64(0)}
			if o := t.Opener(); o != nil {
				out["opener"] = o.winID
			}
			if t.isFrame() {
				out["parent"] = t.bc.parent.winID
				// a window whose <iframe> was removed from the document is closed
				_ = t.bc.parent.withDoc(func(d *fnet.Doc) error {
					out["closed"] = out["closed"] == true || !d.IsConnected(t.bc.elem)
					return nil
				})
			}
			return out, nil
		},
		"frAccess": func(a []any) (any, error) {
			t := h.reach(argI(a, 0))
			return t != nil && sameOriginTabs(h.me(), t), nil
		},
		"frPost": func(a []any) (any, error) {
			me := h.me()
			t := h.reach(argI(a, 0))
			if t == nil || me == nil {
				return nil, nil
			}
			want := argS(a, 2)
			src, dst := originOrNull(me), originOrNull(t)
			switch want {
			case "*":
			case "/":
				if src != dst || dst == "null" {
					return nil, nil
				}
			default:
				u, err := url.Parse(want)
				if err != nil || u.Scheme == "" || u.Host == "" {
					return nil, jsErr("SyntaxError", "Failed to execute 'postMessage' on 'Window': Invalid target origin '%s' in a call to 'postMessage'.", want)
				}
				if canonOrigin(want) != dst {
					return nil, nil // the target is not at that origin: the message is dropped, and the sender cannot tell
				}
			}
			t.mu.RLock()
			tj := t.js
			t.mu.RUnlock()
			if tj == nil {
				return nil, nil
			}
			pushFrame(tj, map[string]any{"t": "msg", "src": me.winID, "origin": src, "data": argS(a, 1), "ports": portsArg(argAt(a, 3))})
			return nil, nil
		},
		"frOp": func(a []any) (any, error) {
			me := h.me()
			t := h.reach(argI(a, 0))
			if t == nil || me == nil {
				return nil, jsErr("InvalidStateError", "The window is gone.")
			}
			if t != me && !sameOriginTabs(me, t) {
				return nil, jsErr("SecurityError", "Blocked a frame with origin \"%s\" from accessing a cross-origin frame.", originOrNull(me))
			}
			tj, err := t.ensureFrameJS()
			if err != nil {
				return nil, jsErr("InvalidAccessError", "The frame has no scripting context: %v", err)
			}
			res, err := tj.callWait(3*time.Second, "__swRemote", argS(a, 1), argI(a, 2), argS(a, 3), argS(a, 4), argI(a, 5))
			if err != nil {
				if errors.Is(err, errFrameBusy) {
					return nil, err
				}
				return nil, jsErr("InvalidStateError", "%v", err)
			}
			s, _ := res.(string)
			return s, nil
		},
		"frElement": func(a []any) (any, error) {
			me := h.me()
			if me == nil || !me.isFrame() {
				return "{\"k\":\"j\",\"s\":\"null\"}", nil
			}
			p := me.bc.parent
			if !sameOriginTabs(me, p) {
				return "{\"k\":\"j\",\"s\":\"null\"}", nil
			}
			pj, err := p.ensureFrameJS()
			if err != nil {
				return nil, jsErr("InvalidAccessError", "%v", err)
			}
			res, err := pj.callWait(3*time.Second, "__swRemoteElem", me.bc.elem)
			if err != nil {
				return nil, err
			}
			s, _ := res.(string)
			return s, nil
		},
		"frCallback": func(a []any) (any, error) {
			t := h.reach(argI(a, 0))
			if t == nil {
				return nil, nil
			}
			t.mu.RLock()
			tj := t.js
			t.mu.RUnlock()
			if tj != nil {
				pushFrame(tj, map[string]any{"t": "cb", "id": argI(a, 1), "args": argS(a, 2)})
			}
			return nil, nil
		},
		"frNav": func(a []any) (any, error) {
			me := h.me()
			t := h.reach(argI(a, 0))
			if t == nil || me == nil {
				return nil, nil
			}
			// a sandboxed frame needs allow-top-navigation to navigate its top-level page
			if t == t.topTab() && me != t && me.bc != nil && me.bc.sandbox&sbSandboxed != 0 && me.bc.sandbox&(sbTopNav|sbTopNavUser) == 0 {
				me.LogConsole("error", "Unsafe attempt to navigate the top-level page from a sandboxed frame: the sandbox lacks allow-top-navigation.")
				return nil, nil
			}
			target := argS(a, 1)
			base := me.URL()
			if abs, err := resolveURL(base, target); err == nil {
				target = abs
			}
			lower := strings.ToLower(target)
			if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "data:") {
				return nil, nil
			}
			if t.isFrame() {
				go func() { _ = t.Navigate(target, nil) }()
				return nil, nil
			}
			if me != t && !navSchemeAllowed(me.URL(), target) {
				return nil, nil
			}
			me.queueNavigation(target, navSpec{}, t) // a window is navigated by the goroutine that owns it
			return nil, nil
		},
		"frName": func(a []any) (any, error) {
			me := h.me()
			if me == nil {
				return "", nil
			}
			switch argS(a, 0) {
			case "":
				return me.windowName(), nil
			case "setName":
				me.setWindowName(argS(a, 1))
			case "open": // window.open(url, target, features): the window id it refers to, or 0
				if w := me.openFromScript(argS(a, 1), argS(a, 2), argS(a, 3)); w != nil {
					return w.winID, nil
				}
				return int64(0), nil
			case "close":
				t := h.reach(argI(a, 1))
				if t == nil {
					return false, nil
				}
				if t == me.Top() || t == me || (t.win.scriptOpened && t.win.opener.Load() == me.Top()) {
					return t.closeFromScript(), nil
				}
				return false, nil
			case "focus":
				if t := h.reach(argI(a, 1)); t != nil {
					t.focusFromScript()
				}
			case "nav": // an anchor or form with a target other than _self
				rel, tg := argS(a, 3), argS(a, 2)
				me.navigateTarget(navSpec{rawURL: argS(a, 1), target: tg, noOpener: anchorNoOpener(tg, rel), noReferrer: relHas(rel, "noreferrer")})
				return true, nil
			}
			return nil, nil
		},
		// ---- Storage Access API ----
		"saHas": func(a []any) (any, error) { return h.me() != nil && h.me().hasStorageAccess(), nil },
		"saRequest": func(a []any) (any, error) {
			if h.me() == nil {
				return nil, jsErr("NotAllowedError", "no document")
			}
			return nil, h.me().requestStorageAccess()
		},
	}
}
