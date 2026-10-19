package engine

// bgvm.go: a script VM that runs on its own goroutine, bound to a hidden tab.
//
// Dedicated and shared workers (workers.go) and nested browsing contexts
// (frames.go) all need the same thing a service worker already has: a complete
// page VM (timers, promises, fetch, IndexedDB, Cache Storage, OPFS ...) that the
// page's own event loop does not pump. One goroutine per runtime drains the
// VM's event queue (timers, promise jobs, request completions, storage
// notifications, messages) and runs the Go closures posted to it, so a worker
// never touches the queue of the page that created it.
//
// The hidden tab is never listed or navigated by the user. It shares the
// console and network rings of the tab that owns it (a worker's fetches and
// console output show up with the page's), the context's cookie jar and
// storage, and carries the partition of its owner (see partition.go).

import (
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	fnet "github.com/sweefbrowser/sweef/internal/net"
)

type bgRuntime struct {
	tab *Tab
	j   *jsVM

	wakeCh chan struct{}
	taskMu sync.Mutex
	tasks  []func()
	quit   chan struct{}
	done   chan struct{}

	started  atomic.Bool
	stopped  atomic.Bool
	stopOnce sync.Once

	cur func() *jsVM // (frames) the VM of the tab's current document; nil = j

	tick func() // (frames) called after every slice of the loop: repaint the embedding page when the frame changed
}

// vm is the VM to pump now.
func (rt *bgRuntime) vm() *jsVM {
	if rt.cur != nil {
		return rt.cur()
	}
	return rt.j
}

var bgSeq atomic.Int64

// newBGRuntime builds a hidden tab and a VM for it. owner supplies the context, the log rings and the partition;
// docURL is the URL the VM's document reports (a worker's script URL, or the creator's URL for a blob worker).
func (e *Engine) newBGRuntime(owner *Tab, docURL string) (*bgRuntime, error) {
	t := newTab(e, fmt.Sprintf("bg%d", bgSeq.Add(1)))
	t.ctx = owner.Context()
	t.sw.noNav = true
	u, err := url.Parse(docURL)
	if err != nil {
		return nil, err
	}
	t.url = u
	t.consoleRing, t.netRing, t.dev = owner.consoleRing, owner.netRing, owner.dev
	t.inheritContext(owner)
	t.inheritIsolation(owner)
	doc, err := fnet.ParseDocument(emptyWorkerDoc, docURL)
	if err != nil {
		return nil, err
	}
	t.dom = doc
	j := e.vms.takeQJS() // workers run on QuickJS whatever the pages use (V8 workers are phase 2b)
	if j == nil {
		if j, err = buildQJSVM(e.opts); err != nil {
			return nil, err
		}
	}
	e.vms.refill()
	rt := &bgRuntime{tab: t, j: j, wakeCh: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{})}
	j.mu.Lock()
	j.tab, j.currentDoc, j.start = t, doc, time.Now()
	j.mu.Unlock()
	t.mu.Lock()
	t.js = j
	t.mu.Unlock()
	wake := func() { rt.nudge() }
	t.onWake.Store(&wake)
	j.installModuleLoader(docURL)
	if _, err := j.call("__swSetDocument", docURL, "complete"); err != nil {
		t.close()
		return nil, err
	}
	_, _ = j.call("__swCoiBind")
	return rt, nil
}

func (rt *bgRuntime) nudge() {
	select {
	case rt.wakeCh <- struct{}{}:
	default:
	}
}

// post queues a closure for the runtime's goroutine. After stop it is dropped.
func (rt *bgRuntime) post(f func()) {
	if rt.stopped.Load() {
		return
	}
	rt.taskMu.Lock()
	rt.tasks = append(rt.tasks, f)
	rt.taskMu.Unlock()
	rt.nudge()
}

// start launches the pump goroutine (after the script ran).
func (rt *bgRuntime) start() {
	if rt.started.CompareAndSwap(false, true) {
		go rt.loop()
	}
}

func (rt *bgRuntime) loop() {
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
			select {
			case <-rt.quit:
				return
			default:
			}
		}
		now := time.Now()
		j := rt.vm()
		finished := true
		if j != nil {
			_, finished = j.pumpUntil(now.Add(time.Hour), now.Add(15*time.Millisecond))
		}
		if rt.tick != nil {
			rt.tick()
		}
		rt.taskMu.Lock()
		more := len(rt.tasks) > 0
		rt.taskMu.Unlock()
		if more {
			continue
		}
		var timer <-chan time.Time
		if !finished {
			d := time.Duration(j.wakeMs * float64(time.Millisecond))
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

// stop ends the goroutine and closes the VM. It waits for a script that is running (bounded by the eval
// timeout), so callers on a page's thread use stopAsync.
func (rt *bgRuntime) stop() {
	rt.stopOnce.Do(func() {
		rt.stopped.Store(true)
		close(rt.quit)
		rt.j.sabInterrupt() // a worker blocked in Atomics.wait must come back to notice
		rt.j.wasmAbort()    // and so must one that is inside a wasm thread: its shared memories are released now
		rt.nudge()
		if rt.started.Load() {
			<-rt.done
		}
		rt.tab.close()
	})
}

func (rt *bgRuntime) stopAsync() {
	rt.stopped.Store(true)
	go rt.stop()
}
