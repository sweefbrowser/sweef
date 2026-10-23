// Package rpc exposes the engine over JSON-RPC. It powers:
//
//   - `sweef serve`  — TCP/Unix socket server for SDKs (Node & Python)
//   - stdio loop       — used by the MCP bridge and `sweef call <method>`
//
// Method names mirror CDP namespaces (Page.*, DOM.*, Network.*, Runtime.*)
// so the shim in internal/cdp can translate real DevTools traffic onto these,
// and so Playwright-style SDKs map 1:1 onto our verbs.
package rpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sweefbrowser/sweef/internal/engine"
	fnet "github.com/sweefbrowser/sweef/internal/net"
	"github.com/sweefbrowser/sweef/internal/protocol"
)

type Server struct {
	eng *engine.Engine

	// IdleTimeout, when > 0, makes ServeTCP return after that long without a
	// request (session daemons started by the CLI use it).
	IdleTimeout time.Duration
	// OnShutdown is invoked by the Browser.shutdown method (CLI `close`).
	OnShutdown func()

	lastActivity atomic.Int64

	// BackgroundPump keeps the pages' event loops running between calls for a short while after each call
	// (pumpWindow), so work a page started (a client-side route loading its chunk, a fetch, a delayed render)
	// completes between an agent's commands, as it would in a browser, instead of only while the next command
	// runs. Pages then go quiet: no CPU is spent on animation loops of a page nobody is driving.
	BackgroundPump bool
	drive          sync.RWMutex // calls hold it shared; a background pump holds it alone
	inCall         atomic.Int32
}

// pumpWindow is how long after the last call the background pump keeps pages running.
const pumpWindow = 3 * time.Second

// StartBackgroundPump runs the background pump (see Server.BackgroundPump) until ctx
// ends, for servers that are not started through ServeTCP (the MCP server).
func (s *Server) StartBackgroundPump(ctx context.Context) { go s.backgroundPump(ctx) }

// backgroundPump runs until ctx ends (see Server.BackgroundPump).
func (s *Server) backgroundPump(ctx context.Context) {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	var rest time.Time // no pump before this (duty-cycle limit)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if time.Since(time.Unix(0, s.lastActivity.Load())) > pumpWindow || s.inCall.Load() > 0 {
			continue
		}
		if time.Now().Before(rest) || !s.drive.TryLock() {
			continue
		}
		t0 := time.Now()
		for _, t := range s.eng.ListTabs() {
			t.Settle(15 * time.Millisecond) // returns at once when nothing is due
		}
		s.drive.Unlock()
		// at most a quarter of one core: a page whose every animation frame forces an expensive layout would
		// otherwise keep the daemon busy for the whole window while the agent thinks
		rest = time.Now().Add(3 * time.Since(t0))
	}
}

func NewServer(e *engine.Engine) *Server {
	s := &Server{eng: e}
	s.lastActivity.Store(time.Now().UnixNano())
	return s
}

// ---- dispatch --------------------------------------------------------------

type Params = map[string]any

func str(p Params, k string) string { v, _ := p[k].(string); return v }
func num(p Params, k string) float64 {
	if v, ok := numMs(p[k]); ok {
		return v
	}
	return 0
}
func strOr(p Params, k, def string) string {
	if v, ok := p[k].(string); ok && v != "" {
		return v
	}
	return def
}

func (s *Server) tabID(p Params) (string, error) {
	id := str(p, "tab")
	if id == "" {
		t, err := s.eng.ActiveTab()
		if err != nil {
			return "", err
		}
		return t.ID, nil
	}
	return id, nil
}

// tabOrNew resolves the target tab; if none exists yet it auto-creates one
// (navigating to url when provided). This lets agents call browser_navigate /
// browser_screenshot as the very first tool without an explicit newTab.
func (s *Server) tabOrNew(p Params) (*engine.Tab, error) {
	if str(p, "tab") == "" {
		if len(s.eng.ListTabs()) == 0 {
			u := strOr(p, "url", "about:blank")
			t, _ := s.eng.NewTab(u) // navigation errors are surfaced via logs
			if t != nil {
				return t, nil
			}
		}
	}
	return s.tab(p)
}

func (s *Server) tab(p Params) (*engine.Tab, error) {
	id, err := s.tabID(p)
	if err != nil {
		return nil, err
	}
	return s.eng.Tab(id)
}

// Call executes one method and returns a JSON result or an RpcError.
func (s *Server) Call(method string, raw json.RawMessage) (json.RawMessage, *protocol.RpcError) {
	s.inCall.Add(1)
	s.drive.RLock()
	defer func() {
		s.drive.RUnlock()
		s.inCall.Add(-1)
		s.lastActivity.Store(time.Now().UnixNano()) // the background pump's window starts when the call ends
	}()
	s.lastActivity.Store(time.Now().UnixNano())
	var p Params
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, badReq("params must be an object")
		}
	} else {
		p = Params{}
	}
	res, err := s.dispatch(method, p)
	if err != nil {
		return nil, toRpc(err)
	}
	b, merr := MarshalCompact(res)
	if merr != nil {
		return nil, internal(merr.Error())
	}
	return b, nil
}

// MarshalCompact is json.Marshal without HTML escaping: page HTML, snapshots and
// text are full of <, > and &, and \u003c costs six bytes (and tokens) for one.
func MarshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func snapshotOptions(p Params) engine.SnapshotOptions {
	return engine.SnapshotOptions{
		MaxChars:        int(num(p, "maxChars")),
		Selector:        str(p, "selector"),
		InteractiveOnly: p["interactiveOnly"] == true,
		MaxTextLen:      int(num(p, "maxTextLen")),
		MaxDepth:        int(num(p, "maxDepth")),
	}
}

func (s *Server) dispatch(method string, p Params) (any, error) {
	switch method {
	// ---- Browser / lifecycle ----
	case "Browser.getVersion":
		return map[string]any{"product": "Sweef/0.1", "userAgent": s.eng.Opts().DefaultUserAgent,
			"jsRuntime": "quickjs-es2020", "memoryBudgetMiB": s.eng.Opts().MaxRSSMiB}, nil
	case "Browser.newTab", "Target.createTarget":
		u := str(p, "url")
		t, err := s.eng.NewTab(u)
		if err != nil && t == nil {
			return nil, err
		}
		// NewTab reports navigation errors but still returns a usable tab;
		// swallow the error so agents can inspect logs and retry via Page.navigate.
		return map[string]any{"tab": t.ID, "url": t.URL(), "title": t.Title()}, nil
	case "Browser.closeTab", "Target.closeTarget":
		return map[string]bool{"ok": true}, s.eng.CloseTab(str(p, "tab"))
	case "Browser.listTabs":
		type tb struct {
			ID    string `json:"id"`
			URL   string `json:"url"`
			Title string `json:"title"`
		}
		out := []tb{}
		for _, t := range s.eng.ListTabs() {
			out = append(out, tb{t.ID, t.URL(), t.Title()})
		}
		return out, nil
	case "Browser.shutdown":
		if s.OnShutdown != nil {
			go func() { time.Sleep(50 * time.Millisecond); s.OnShutdown() }()
		}
		return map[string]bool{"ok": true}, nil
	case "Browser.stats":
		// gc: collect first (numbers without garbage); heapProfile: also write a pprof heap profile there
		// (a development aid for memory work, like SWEEF_CPUPROFILE)
		if b, _ := p["gc"].(bool); b || str(p, "heapProfile") != "" {
			runtime.GC()
		}
		if path := str(p, "heapProfile"); path != "" {
			f, err := os.Create(path)
			if err != nil {
				return nil, err
			}
			err = pprof.WriteHeapProfile(f)
			f.Close()
			if err != nil {
				return nil, err
			}
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		rss, ok := engine.ProcessRSS()
		if !ok {
			rss = ms.Sys // no OS figure: what the Go runtime holds (misses the QuickJS heaps)
		}
		return map[string]any{
			"rssMiB":     rss >> 20,
			"goSysMiB":   ms.Sys >> 20,
			"heapMiB":    ms.HeapAlloc >> 20,
			"budgetMiB":  s.eng.Opts().MaxRSSMiB,
			"liveDocs":   fnet.LiveDocs(),
			"goroutines": runtime.NumGoroutine(),
			"tabs":       len(s.eng.ListTabs()),
		}, nil

	// ---- Navigation ----
	case "Page.navigate", "Page.goto":
		// The first navigate opens a blank tab and then navigates it once. Creating the tab with
		// the url and navigating again loaded every page twice (scripts, API calls and analytics
		// all ran twice) and hid errors from the first load.
		t, err := s.tabOrNew(Params{"tab": p["tab"], "url": "about:blank"})
		if err != nil {
			return nil, err
		}
		hdrs := map[string]string{}
		if hm, ok := p["headers"].(map[string]any); ok {
			for k, v := range hm {
				hdrs[k] = fmt.Sprint(v)
			}
		}
		if err := t.Navigate(str(p, "url"), hdrs); err != nil {
			return nil, err
		}
		return map[string]any{"url": t.URL(), "title": t.Title()}, nil
	case "Page.goBack":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, t.GoBack()
	case "Page.goForward":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, t.GoForward()
	case "Page.reload":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, t.Navigate(t.URL(), nil)
	case "Page.title":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"title": t.Title()}, nil
	case "Page.url":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": t.URL()}, nil

	case "Page.frames": // the frames of the page (recursively); a frame's refs in a snapshot start with its ref
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"frames": frameTree(t, "")}, nil
	case "Page.popups": // window.open / link-target decisions of the tab, allowed and blocked
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, e := range t.PopupLog(0) {
			out = append(out, e.Data)
		}
		return map[string]any{"popups": out}, nil

	// ---- Content extraction ----
	case "Page.content", "Page.getHTML":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"html": t.HTML(true)}, nil
	case "Page.textContent":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		txt, err := t.Text(strOr(p, "selector", "body"))
		return map[string]any{"text": txt}, err
	case "Page.snapshot": // aria-style tree for LLM consumption
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return t.Snapshot()
	case "Page.ariaSnapshot": // Playwright-style YAML accessibility tree with [ref=eN] handles
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		yaml, err := t.AriaSnapshotV2(snapshotOptions(p))
		if err != nil {
			return nil, err
		}
		if p["delta"] == true {
			return map[string]any{"yaml": deltaFor(t.ID, yaml)}, nil
		}
		rememberSnap(t.ID, yaml)
		return map[string]any{"yaml": yaml}, nil
	case "Batch.run", "Batch":
		return s.batch(p)
	case "Page.screenshot": // PNG bytes (base64) + structured text render; width/height/dsf per Playwright
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		w := int(num(p, "width"))
		h := int(num(p, "height"))
		dsf := num(p, "deviceScaleFactor")
		format := strOr(p, "type", "png")
		out := map[string]any{}
		if p["noWait"] != true && format == "png" {
			// shoot the page once it stopped moving: sites reveal their content with script animations, and a
			// capture at the end of the load's settle would show the first frames of the intro
			timeout := engine.ShotStableTimeout
			if v, ok := numMs(p["waitMs"]); ok {
				timeout = time.Duration(v) * time.Millisecond
			}
			var quiet time.Duration
			if v, ok := numMs(p["stableMs"]); ok {
				quiet = time.Duration(v) * time.Millisecond
			}
			if !t.SettleVisual(timeout, quiet) {
				out["stable"] = false // still changing when the wait ran out (an endless animation)
			}
		}
		if p["noText"] != true { // the text render costs a layout pass; image-only callers skip it
			out["text"] = t.Screenshot(w)
		}
		if format == "png" {
			var pb []byte
			if p["fullPage"] == true { // one render, not a viewport shot thrown away first
				pb = t.PNGScreenshotFull(w, dsf)
			} else {
				pb = t.PNGScreenshot(w, h, dsf)
			}
			out["pngBase64"] = base64.StdEncoding.EncodeToString(pb)
			out["bytes"] = len(pb)
		}
		return out, nil

	// ---- DOM queries ----
	case "DOM.querySelector":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return t.QuerySelector(str(p, "selector"))
	case "DOM.querySelectorAll":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return t.QuerySelectorAll(str(p, "selector"))

	// ---- Interaction (Playwright-equivalent actions) ----
	case "Input.click", "Page.click":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, t.Click(str(p, "selector"))
	case "Input.fill", "Page.fill":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, t.Fill(str(p, "selector"), str(p, "value"))
	case "Input.press", "Page.press":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, t.Press(str(p, "selector"), strOr(p, "key", "Enter"))

	// ---- JS ----
	case "Runtime.evaluate":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		val, err := t.Evaluate(str(p, "expression"))
		if err != nil {
			return nil, err
		}
		// Every client (session CLI, batch, Node and Python SDKs) reads .result; a bare value made
		// `sweef eval` print null and SDK evaluate() return nothing (Python's .get crashed on it).
		return map[string]any{"result": val}, nil

	// ---- Logs (console / network / state) with cursor pagination ----
	case "Log.getConsole":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"messages": t.ConsoleLog(int64(num(p, "since")))}, nil
	case "Network.getLogs":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"entries": t.NetworkLog(int64(num(p, "since")))}, nil
	case "Page.getStateLog":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		return t.StateLog(int64(num(p, "since"))), nil
	case "Network.har":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		har, err := t.HAR()
		return json.RawMessage(har), err

	// ---- Interception ----
	case "Network.route":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		if err := t.AddRoute(routeFromParams(p)); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "routes": len(t.Routes())}, nil
	case "Network.unrouteAll":
		t, err := s.tab(p)
		if err != nil {
			return nil, err
		}
		t.UnrouteAll()
		t.RemoveRoutes("")
		return map[string]bool{"ok": true}, nil

	default:
		return s.dispatchExt(method, p)
	}
}

func badReq(msg string) *protocol.RpcError {
	return &protocol.RpcError{Code: -32600, Message: msg}
}
func internal(msg string) *protocol.RpcError {
	return &protocol.RpcError{Code: -32603, Message: msg}
}
func toRpc(err error) *protocol.RpcError {
	switch {
	case errors.Is(err, engine.ErrNoTab):
		return &protocol.RpcError{Code: -32001, Message: err.Error()}
	case errors.Is(err, engine.ErrNotFound):
		return &protocol.RpcError{Code: -32004, Message: err.Error()}
	default:
		msg := err.Error()
		code := -32000
		if strings.Contains(msg, "no node matches") || strings.Contains(msg, "vanished") {
			code = -32004
		}
		return &protocol.RpcError{Code: code, Message: msg}
	}
}

// ---- serving ----------------------------------------------------------------

// ServePipe speaks framed protocol over an arbitrary ReadWriteCloser.
// Used for stdio (`sweef mcp`, `sweef call`) and sockets alike.
func (s *Server) ServePipe(ctx context.Context, rw io.ReadWriteCloser, busEvents bool) error {
	conn := protocol.NewConn(rw)
	defer conn.Close()

	var wg sync.WaitGroup
	defer wg.Wait()

	if busEvents {
		id, ch := s.eng.Bus().Subscribe(128)
		defer s.eng.Bus().Unsubscribe(id)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case <-conn.Done():
					return
				case ev, ok := <-ch:
					if !ok {
						return
					}
					data, _ := json.Marshal(ev)
					conn.Write(&protocol.Message{JSONRPC: "2.0", Method: "event." + string(ev.Kind), Result: data})
				}
			}
		}()
	}

	buf := make([]byte, 4096)
	for {
		msg, nb, err := conn.Read(buf)
		buf = nb
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		if msg.Method == "" {
			continue // clients don't send responses on this channel
		}
		if msg.Method == "conn.close" {
			return nil
		}
		res, rerr := s.Call(msg.Method, msg.Params)
		reply := &protocol.Message{JSONRPC: "2.0", ID: msg.ID}
		if rerr != nil {
			reply.Error = rerr
		} else {
			reply.Result = res
		}
		if err := conn.Write(reply); err != nil {
			return err
		}
	}
}

// ServeTCP listens for SDK connections. One goroutine per client; each is a
// few KB of stack until it starts working — fine even at hundreds of agents.
func (s *Server) ServeTCP(ctx context.Context, addr string) error {
	network := "tcp"
	if strings.HasPrefix(addr, "unix:") {
		network, addr = "unix", strings.TrimPrefix(addr, "unix:")
		_ = os.Remove(addr)
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	if network == "unix" {
		_ = os.Chmod(addr, 0o600)
		// remove the socket only while it is still this listener's: once a new daemon has bound the same
		// path (the old one still shutting down), removing it would strand the new one's clients
		if mine, err := os.Stat(addr); err == nil {
			defer func() {
				if cur, err := os.Stat(addr); err == nil && os.SameFile(mine, cur) {
					_ = os.Remove(addr)
				}
			}()
		}
		if ul, ok := ln.(*net.UnixListener); ok {
			ul.SetUnlinkOnClose(false) // the deferred check above decides
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	if s.BackgroundPump {
		go s.backgroundPump(ctx)
	}
	if s.IdleTimeout > 0 {
		go func() {
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if time.Since(time.Unix(0, s.lastActivity.Load())) > s.IdleTimeout {
						cancel()
						return
					}
				}
			}
		}()
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetDeadline(time.Time{}) // long-lived agent sessions: no timeouts
		}
		go s.ServePipe(ctx, c, true)
	}
}

// frameTree lists the frames of t, each with the ref prefix its snapshot refs carry.
func frameTree(t *engine.Tab, prefix string) []map[string]any {
	out := []map[string]any{}
	for _, fr := range t.FrameRefs() {
		ref := prefix + fr.Ref
		out = append(out, map[string]any{"ref": ref, "name": fr.Tab.WindowName(), "url": fr.Tab.URL(), "origin": fr.Tab.OriginOrNull(),
			"sandbox": fr.Tab.Sandboxed(), "frames": frameTree(fr.Tab, ref)})
	}
	return out
}
