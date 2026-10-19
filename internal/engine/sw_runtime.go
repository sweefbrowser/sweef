package engine

// sw_runtime.go: the VM a service worker runs in.
//
// A worker runs in an ordinary script VM bound to a hidden tab (never listed by the
// engine, never navigated): the page prelude supplies fetch, Request/Response, timers,
// caches, indexedDB and console, and sw_scope.js turns the global into a
// ServiceWorkerGlobalScope. Because the hidden tab has no controller, the worker's own
// fetch() goes to the network and is never routed back to itself.
//
// One goroutine per running worker pumps the VM (timers, promise jobs, request
// completions) and runs the Go closures posted to it; events are dispatched by posting
// a closure that calls __swSwDispatch and waiting for the worker to report back through
// __sw.swEventDone.

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	fnet "github.com/sweefbrowser/sweef/internal/net"
	"github.com/sweefbrowser/sweef/internal/zpack"
)

//go:embed sw_scope.js.z
var swScopeJSZ string

var swScopeLazy zpack.Lazy

var errSWStopped = errors.New("service worker stopped")

type swEventResult struct {
	err  string
	meta map[string]any
	body string
}

type swRuntime struct {
	m   *swManager
	w   *swWorker
	tab *Tab
	j   *jsVM

	wakeCh chan struct{}
	taskMu sync.Mutex
	tasks  []func()
	quit   chan struct{}
	done   chan struct{}

	evMu     sync.Mutex
	evs      map[int64]chan swEventResult
	evSeq    int64
	stopOnce sync.Once
}

// runtime returns the running VM of a worker, if any.
func (w *swWorker) runtime() *swRuntime {
	w.rtMu.Lock()
	defer w.rtMu.Unlock()
	return w.rt
}

// runtimeFor starts the worker's VM on first use and evaluates its script.
func (m *swManager) runtimeFor(w *swWorker) (*swRuntime, error) {
	w.rtMu.Lock()
	defer w.rtMu.Unlock()
	if w.rt != nil {
		return w.rt, nil
	}
	if w.rtErr != nil {
		return nil, w.rtErr
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed || w.state == "redundant" {
		return nil, errSWStopped
	}
	rt, err := m.startRuntime(w)
	if err != nil {
		w.rtErr = err
		return nil, err
	}
	w.rt = rt
	return rt, nil
}

func (w *swWorker) stopRuntime() {
	w.rtMu.Lock()
	rt := w.rt
	w.rt = nil
	if w.rtErr == nil {
		w.rtErr = errSWStopped
	}
	w.rtMu.Unlock()
	if rt != nil {
		rt.stop()
	}
}

const emptyWorkerDoc = "<!doctype html><html><head></head><body></body></html>"

func (m *swManager) startRuntime(w *swWorker) (*swRuntime, error) {
	e := m.c.eng
	t := newTab(e, "sw"+fmt.Sprint(w.id))
	t.ctx = m.c
	t.sw.noNav = true
	t.url = parseURLQuiet(w.scriptURL)
	t.bc = partCtxFor(w.reg.origin)
	doc, err := fnet.ParseDocument(emptyWorkerDoc, w.scriptURL)
	if err != nil {
		return nil, err
	}
	t.dom = doc
	j, err := buildQJSVM(e.opts) // service workers run on QuickJS whatever the pages use (V8 is phase 2b)
	if err != nil {
		return nil, err
	}
	rt := &swRuntime{m: m, w: w, tab: t, j: j, wakeCh: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{}), evs: map[int64]chan swEventResult{}}
	t.sw.rt = rt
	j.mu.Lock()
	j.tab, j.currentDoc, j.start = t, doc, time.Now()
	j.mu.Unlock()
	t.mu.Lock()
	t.js = j
	t.mu.Unlock()
	fail := func(err error) (*swRuntime, error) {
		t.close()
		return nil, err
	}
	j.installModuleLoader(w.scriptURL)
	if _, err := j.call("__swSetDocument", w.scriptURL, "complete"); err != nil {
		return fail(err)
	}
	wake := func() { rt.nudge() }
	t.onWake.Store(&wake)
	info, _ := json.Marshal(map[string]any{"scriptURL": w.scriptURL})
	if _, err := j.run(strings.Replace(swScopeLazy.Get(swScopeJSZ), "__SW_INFO__", string(info), 1), "service worker scope"); err != nil {
		return fail(err)
	}
	if w.typ == "module" {
		err = j.runModule(w.source, w.scriptURL)
	} else {
		_, err = j.run(w.source+"\n//# sourceURL="+w.scriptURL, w.scriptURL)
	}
	if err != nil {
		return fail(fmt.Errorf("script error: %w", err))
	}
	go rt.loop()
	return rt, nil
}

func parseURLQuiet(raw string) *url.URL {
	u, _ := url.Parse(raw)
	return u
}

func (rt *swRuntime) nudge() {
	select {
	case rt.wakeCh <- struct{}{}:
	default:
	}
}

// post queues a closure for the worker's goroutine.
func (rt *swRuntime) post(f func()) {
	rt.taskMu.Lock()
	rt.tasks = append(rt.tasks, f)
	rt.taskMu.Unlock()
	rt.nudge()
}

func (rt *swRuntime) loop() {
	defer close(rt.done)
	for {
		select {
		case <-rt.quit:
			return
		default:
		}
		rt.taskMu.Lock()
		tasks := rt.tasks
		rt.tasks = nil
		rt.taskMu.Unlock()
		for _, f := range tasks {
			f()
		}
		now := time.Now()
		_, finished := rt.j.pumpUntil(now.Add(time.Hour), now.Add(15*time.Millisecond))
		rt.taskMu.Lock()
		more := len(rt.tasks) > 0
		rt.taskMu.Unlock()
		if more {
			continue
		}
		var timer <-chan time.Time
		if !finished {
			d := time.Duration(rt.j.wakeMs * float64(time.Millisecond))
			if d <= 0 {
				continue
			}
			timer = time.After(d)
		}
		select {
		case <-rt.quit:
			return
		case <-rt.wakeCh:
		case <-timer:
		}
	}
}

func (rt *swRuntime) stop() {
	rt.stopOnce.Do(func() {
		close(rt.quit)
		rt.nudge()
		<-rt.done
		rt.tab.close()
	})
}

// dispatch fires an event in the worker and waits until its promises settle.
func (rt *swRuntime) dispatch(kind string, payload any, timeout time.Duration) (swEventResult, error) {
	pj := ""
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return swEventResult{}, err
		}
		pj = string(b)
	}
	rt.evMu.Lock()
	rt.evSeq++
	id := rt.evSeq
	ch := make(chan swEventResult, 1)
	rt.evs[id] = ch
	rt.evMu.Unlock()
	rt.post(func() {
		if _, err := rt.j.call("__swSwDispatch", kind, id, pj); err != nil {
			rt.finish(id, swEventResult{err: err.Error()})
		}
	})
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.err != "" {
			return res, errors.New(res.err)
		}
		return res, nil
	case <-timer.C:
		rt.evMu.Lock()
		delete(rt.evs, id)
		rt.evMu.Unlock()
		return swEventResult{}, fmt.Errorf("the %s event timed out", kind)
	case <-rt.quit:
		return swEventResult{}, errSWStopped
	case <-rt.m.quit:
		return swEventResult{}, errSWStopped
	}
}

// finish reports an event's outcome (called from __sw.swEventDone inside the VM).
func (rt *swRuntime) finish(id int64, res swEventResult) {
	rt.evMu.Lock()
	ch := rt.evs[id]
	delete(rt.evs, id)
	rt.evMu.Unlock()
	if ch != nil {
		ch <- res
	}
}
