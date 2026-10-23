package engine

// asynchttp.go: fetch(), XMLHttpRequest and sendBeacon without parking the page's
// JavaScript on the network.
//
// The page VM is single-threaded and every host call runs inside it, so a request
// made synchronously from a host call (what fetch and async XHR used to do) holds
// the tab for its whole duration: a page that asks for ten resources got them one
// after the other, and a hanging request (an ad or telemetry ping that takes 30 s
// to time out, on MDN) froze the tab and every input for 30 s. Now the request
// runs on its own goroutine; its outcome is queued for the event loop like a
// socket frame (asyncEvent kind "http") and delivered to the page's callback by the
// pump. Several requests overlap, and nothing waits for a slow one.
//
// Not asynchronous: synchronous XMLHttpRequest (the page asked to wait), and
// anything made while a module script is being evaluated (QuickJS awaits a module's
// top-level promise in a loop that has no event loop to deliver results to, so a
// request issued under it must complete in place or the await never ends).

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// httpGrace is how long a headless pump waits for a page request that is still in flight when its settle
// budget is spent, counted from the request's start. A slow API answer belongs in the settled page; an ad ping
// that hangs for 30 s does not hold it.
var httpGrace = 4 * time.Second

// httpGraceThirdParty is the same wait for a request to another site than the page's: error reporters, analytics and
// ad pings, which nothing on the page depends on. An API on a CDN answers well inside it; a telemetry endpoint that
// hangs (or is blocked on the way) no longer holds a headless load for the full httpGrace.
var httpGraceThirdParty = 800 * time.Millisecond

// httpStart is when an unanswered page request began and whether it went to another site.
type httpStart struct {
	at    time.Time
	third bool
}

func init() {
	registerHostExtension("asynchttp", func(j *jsVM) map[string]hostFn {
		return map[string]hostFn{"httpAsync": j.hostHTTPAsync, "httpTake": j.hostHTTPTake}
	})
}

type httpOutcome struct {
	res map[string]any
	err string
}

var httpToken atomic.Int64

// hostHTTPAsync(method, url, headers, body, beacon): returns a token, or the response itself when the request had
// to be made in place (see the file comment).
func (j *jsVM) hostHTTPAsync(a []any) (any, error) {
	hdr := map[string]string{}
	if m, ok := normalizeJS(argAt(a, 2)).(map[string]any); ok { // a JS object arrives as *quickjs.Object
		for k, v := range m {
			hdr[k] = fmt.Sprint(v)
		}
	}
	method, raw, body, beacon := argS(a, 0), argS(a, 1), argS(a, 3), argB(a, 4)
	if argB(a, 5) { // the body is binary, sent as base64
		raw64, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			return nil, fmt.Errorf("TypeError: bad request body")
		}
		body = string(raw64)
	}
	t := j.tab
	if j.syncNet && !beacon {
		return t.httpForScript(method, raw, hdr, body)
	}
	token := httpToken.Add(1)
	r := j.rt()
	// a speculative request (a router prefetch of the links on the page) is answered like any other, but the
	// settle does not wait for it: nothing on the page changes until the user follows the link
	counted := !beacon && !isPrefetchRequest(hdr)
	if counted {
		r.httpPending.Add(1)
		r.mu.Lock()
		if r.httpAt == nil {
			r.httpAt = map[int64]httpStart{}
		}
		r.httpAt[token] = httpStart{at: time.Now(), third: thirdParty(t.URL(), raw)}
		r.mu.Unlock()
	}
	go func() {
		res, err := t.httpForScript(method, raw, hdr, body)
		if beacon {
			return // fire and forget: nothing waits for it and nobody reads the answer
		}
		out := httpOutcome{res: res}
		if err != nil {
			out.err = err.Error()
		}
		r.mu.Lock()
		if r.http == nil {
			r.http = map[int64]httpOutcome{}
		}
		r.http[token] = out
		delete(r.httpAt, token)
		r.queue = append(r.queue, asyncEvent{kind: "http", id: token})
		r.mu.Unlock()
		if counted {
			r.httpPending.Add(-1)
		}
		t.wake()
	}()
	return token, nil
}

// isPrefetchRequest reports whether page code marked a request as speculative: Next.js's router prefetch
// (Next-Router-Prefetch), or a Purpose / Sec-Purpose of prefetch.
func isPrefetchRequest(hdr map[string]string) bool {
	for k, v := range hdr {
		switch strings.ToLower(k) {
		case "next-router-prefetch":
			return strings.TrimSpace(v) != "" && strings.TrimSpace(v) != "0"
		case "purpose", "sec-purpose", "x-purpose", "x-moz":
			if strings.Contains(strings.ToLower(v), "prefetch") {
				return true
			}
		}
	}
	return false
}

func argAt(a []any, i int) any {
	if i < len(a) {
		return a[i]
	}
	return nil
}

// hostHTTPTake hands the outcome of a finished request to its callback.
func (j *jsVM) hostHTTPTake(a []any) (any, error) {
	r := j.rt()
	token := argI(a, 0)
	r.mu.Lock()
	out, ok := r.http[token]
	delete(r.http, token)
	r.mu.Unlock()
	if !ok {
		return map[string]any{"error": "no such request"}, nil
	}
	if out.err != "" {
		return map[string]any{"error": strings.TrimSpace(out.err)}, nil
	}
	return out.res, nil
}

// httpInFlightBefore reports whether a page request that started before cutoff (zero = any) and is younger
// than httpGrace is unanswered. The
// settle pump passes its deadline: a request the page had already sent when the budget ran out gets its grace,
// but one started afterwards does not extend the wait, or a page that keeps issuing requests (Next.js prefetches
// every link it renders, one batch after another; a poller) would hold the load until it stops on its own.
func (j *jsVM) httpInFlightBefore(cutoff time.Time) bool {
	r := j.realtime
	if r == nil || r.httpPending.Load() == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.httpAt {
		grace := httpGrace
		if h.third {
			grace = httpGraceThirdParty
		}
		if (cutoff.IsZero() || h.at.Before(cutoff)) && time.Since(h.at) < grace {
			return true
		}
	}
	return false
}
