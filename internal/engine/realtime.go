package engine

// realtime.go — WebSockets and the per-VM asynchronous event queue.
//
// Sockets, workers, frames, storage and fetch produce events asynchronously while page JavaScript may be
// running, so they push onto a small queue that the event-loop pump drains into the VM. Socket frames are also
// logged to the network ring (kind Network.webSocket) so agents can inspect realtime traffic the same way
// they inspect requests. Workers live in workers.go, nested browsing contexts in frames.go.

import (
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed dom_prelude_ext.js.z
var domPreludeExtZ string

type asyncEvent struct {
	kind   string // ws | worker
	id     int64
	typ    string
	data   string
	binary bool
	code   int
	reason string
	clean  bool // ws close: the closing handshake completed
}

// realtime holds a page VM's sockets, workers and their pending events.
type realtime struct {
	imgPending  atomic.Int32 // detached image requests in flight (imgload.go)
	httpPending atomic.Int32 // fetch/XHR requests in flight (asynchttp.go)
	http        map[int64]httpOutcome
	httpAt      map[int64]httpStart // start of every unanswered request
	mu          sync.Mutex
	queue       []asyncEvent
	nextID      int64
	sockets     map[int64]*wsClient
	cancelled   map[int64]bool // sockets close()d while still connecting
	bg          *vmBG          // workers, channels and locks this VM owns (workers.go)
	closers     []func()       // feature packages (webrtc.go) register their teardown here
}

// asyncHandlers let a feature deliver its own queued events without editing
// drainAsync: registerAsyncHandler("kind", fn) from an init() in the feature's file.
var asyncHandlers = map[string]func(j *jsVM, e asyncEvent){}

func registerAsyncHandler(kind string, f func(j *jsVM, e asyncEvent)) { asyncHandlers[kind] = f }

func (r *realtime) addCloser(f func()) {
	r.mu.Lock()
	r.closers = append(r.closers, f)
	r.mu.Unlock()
}

func (j *jsVM) rt() *realtime {
	j.rtOnce.Do(func() {
		j.realtime = &realtime{sockets: map[int64]*wsClient{}}
	})
	return j.realtime
}

func (r *realtime) push(e asyncEvent) {
	r.mu.Lock()
	r.queue = append(r.queue, e)
	r.mu.Unlock()
}

func (r *realtime) take() []asyncEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.queue
	r.queue = nil
	return q
}

func (r *realtime) pending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queue) > 0
}

func (r *realtime) id() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	return r.nextID
}

// closeAll tears down sockets and workers when the page goes away.
func (r *realtime) closeAll() {
	r.mu.Lock()
	socks, closers, bg := r.sockets, r.closers, r.bg
	r.sockets, r.closers, r.bg = map[int64]*wsClient{}, nil, nil
	r.mu.Unlock()
	for _, f := range closers {
		f()
	}
	for _, c := range socks {
		c.Abort()
	}
	if bg != nil {
		bg.closeAll()
	}
}

// ---- WebSocket ------------------------------------------------------------------

func (j *jsVM) wsLog(dir, rawurl, data string, binary bool) {
	if len(data) > 2048 {
		data = data[:2048] + "…"
	}
	ev := map[string]any{"direction": dir, "url": rawurl, "binary": binary, "data": data}
	j.tab.netRing.Push(Event{Tab: j.tab.ID, Kind: EvWebSocket, At: time.Now(), Data: ev})
	j.tab.emit(EvWebSocket, ev)
}

func (j *jsVM) wsOpen(rawurl, protocols string) int64 {
	r := j.rt()
	id := r.id()
	t := j.tab
	origin := t.docOrigin()
	if !strings.HasPrefix(origin, "http") {
		origin = "http://localhost"
	}
	go func() {
		if t.IsOfflineMode() {
			r.push(asyncEvent{kind: "ws", id: id, typ: "error"})
			r.push(asyncEvent{kind: "ws", id: id, typ: "close", code: 1006, reason: "offline"})
			return
		}
		u, err := url.Parse(rawurl)
		if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
			if err == nil {
				err = errors.New("invalid WebSocket URL")
			}
			r.push(asyncEvent{kind: "ws", id: id, typ: "error"})
			r.push(asyncEvent{kind: "ws", id: id, typ: "close", code: 1006, reason: err.Error()})
			return
		}
		var protos []string
		if protocols != "" {
			protos = strings.Split(protocols, ",")
		}
		hdr := http.Header{}
		hdr.Set("User-Agent", t.UserAgent())
		hdr.Set("Pragma", "no-cache")
		hdr.Set("Cache-Control", "no-cache")
		if al := t.Context().AcceptLanguage(); al != "" {
			hdr.Set("Accept-Language", al)
		}
		httpURL := strings.Replace(rawurl, "ws", "http", 1)
		if ck := t.Context().jar.Header(httpURL, t.cookieScopeFor(httpURL, KindXHR).query(false)); ck != "" {
			hdr.Set("Cookie", ck)
		}
		t.touchNet()
		conn, resp, err := t.eng.dialWebSocket(t.eng.ctx, t.Context(), u, origin, protos, hdr)
		if resp != nil { // cookies of the handshake response are stored whether or not the upgrade worked
			host := strings.ToLower(u.Hostname())
			for _, sch := range resp.Header.Values("Set-Cookie") {
				if ck := parseCookie(sch, t.eng.now()); ck != nil {
					t.Context().storeCookie(ck, host, t.cookieScopeFor(httpURL, KindXHR))
				}
			}
			if resp.Body != nil {
				resp.Body.Close()
			}
		}
		if err != nil {
			j.wsLog("error", rawurl, err.Error(), false)
			r.push(asyncEvent{kind: "ws", id: id, typ: "error"})
			r.push(asyncEvent{kind: "ws", id: id, typ: "close", code: 1006, reason: ""})
			return
		}
		r.mu.Lock()
		if r.cancelled[id] { // close() while connecting: the connection is dropped as soon as it exists
			delete(r.cancelled, id)
			r.mu.Unlock()
			conn.Abort()
			r.push(asyncEvent{kind: "ws", id: id, typ: "error"})
			r.push(asyncEvent{kind: "ws", id: id, typ: "close", code: 1006})
			return
		}
		r.sockets[id] = conn
		r.mu.Unlock()
		j.wsLog("open", rawurl, conn.Protocol(), false)
		r.push(asyncEvent{kind: "ws", id: id, typ: "open", data: conn.Protocol()})
		for {
			msg, bin, err := conn.Read()
			if err != nil {
				r.mu.Lock()
				delete(r.sockets, id)
				r.mu.Unlock()
				oc := wsOutcome(err)
				j.wsLog("close", rawurl, fmt.Sprintf("%d %s", oc.Code, oc.Reason), false)
				if oc.Code == 1006 {
					r.push(asyncEvent{kind: "ws", id: id, typ: "error"})
				}
				r.push(asyncEvent{kind: "ws", id: id, typ: "close", code: oc.Code, reason: oc.Reason, clean: oc.Clean})
				return
			}
			if bin {
				b64 := base64.StdEncoding.EncodeToString(msg)
				j.wsLog("receive", rawurl, b64, true)
				r.push(asyncEvent{kind: "ws", id: id, typ: "message", data: b64, binary: true})
			} else {
				j.wsLog("receive", rawurl, string(msg), false)
				r.push(asyncEvent{kind: "ws", id: id, typ: "message", data: string(msg)})
			}
		}
	}()
	return id
}

func (j *jsVM) wsSend(id int64, data string, isBinary bool) error {
	r := j.rt()
	r.mu.Lock()
	conn := r.sockets[id]
	r.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("InvalidStateError: socket %d is not open", id)
	}
	j.wsLog("send", "", data, isBinary)
	if isBinary {
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return err
		}
		return conn.Send(b, true)
	}
	return conn.Send([]byte(data), false)
}

// wsBuffered is WebSocket.bufferedAmount.
func (j *jsVM) wsBuffered(id int64) int64 {
	r := j.rt()
	r.mu.Lock()
	conn := r.sockets[id]
	r.mu.Unlock()
	if conn == nil {
		return 0
	}
	return conn.BufferedAmount()
}

// wsClose starts the closing handshake; the close event arrives when the server answers (or after a timeout).
func (j *jsVM) wsClose(id int64, code int, reason string) {
	r := j.rt()
	r.mu.Lock()
	conn := r.sockets[id]
	r.mu.Unlock()
	if conn != nil {
		conn.Close(code, reason)
		return
	}
	// closed before the connection was open: the attempt is dropped, the page sees an error and a 1006 close
	r.mu.Lock()
	if r.cancelled == nil {
		r.cancelled = map[int64]bool{}
	}
	r.cancelled[id] = true
	r.mu.Unlock()
}

// drainAsync hands queued socket and worker events to page JavaScript.
// Returns true when anything was delivered.
func (j *jsVM) drainAsync() bool {
	if j.realtime == nil {
		return false
	}
	evs := j.realtime.take()
	for _, e := range evs {
		switch e.kind {
		case "ws":
			if _, err := j.call("__swWsEvent", e.id, e.typ, e.data, e.binary, e.code, e.reason, e.clean); err != nil {
				j.tab.LogConsole("error", jsErrorText(err, "websocket "+e.typ))
			}
		case "http":
			if _, err := j.call("__swHttpEvent", e.id); err != nil {
				j.tab.LogConsole("error", jsErrorText(err, "request callback"))
			}
		case "cap":
			if _, err := j.call("__swCapEvent", e.id, e.typ, e.data); err != nil {
				j.tab.LogConsole("error", jsErrorText(err, "capture "+e.typ))
			}
		case "img":
			if _, err := j.call("__swImgEvent", e.id, e.typ, e.code, atoiSafe(e.data)); err != nil {
				j.tab.LogConsole("error", jsErrorText(err, "image "+e.typ))
			}
		default:
			if h := asyncHandlers[e.kind]; h != nil {
				h(j, e)
			}
		}
	}
	return len(evs) > 0
}

// hasLiveWork reports queued events, requests in flight, nested documents that are still loading.
func (j *jsVM) hasLiveWork() bool {
	if j.realtime == nil {
		return false
	}
	r := j.realtime
	if r.imgPending.Load() > 0 || r.httpPending.Load() > 0 {
		return true // a detached image or a page request is on its way: the settle waits for its outcome
	}
	r.mu.Lock()
	n := len(r.queue)
	r.mu.Unlock()
	if n > 0 {
		return true // events waiting
	}
	return j.bgLiveWork()
}

var _ = url.Parse

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
