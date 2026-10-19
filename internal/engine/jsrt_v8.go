package engine

// jsrt_v8.go: the V8 implementation of jsRuntime, on the purego shim (internal/v8shim).
//
// Threading. A V8 isolate belongs to the OS thread that created it (rusty_v8 refuses to touch a Global from any
// other thread, and V8 sizes the stack guard from the creating thread). The engine calls a VM from whichever
// goroutine happens to be running, so each v8RT owns an actor: one goroutine locked to one OS thread that creates
// the isolate and runs every call into it. A call from the engine is a hand-off to the actor and back (about
// 30 us); a page load makes a few hundred of those (the event loop pumps in slices, scripts and timers run
// inside them), and everything JS does in between, host calls included, runs on the actor's thread without a
// hand-off.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sweefbrowser/sweef/internal/v8shim"
)

// v8Actor runs functions on one dedicated OS thread.
type v8Actor struct {
	ch   chan func()
	stop sync.Once
}

func newV8Actor() *v8Actor {
	a := &v8Actor{ch: make(chan func())}
	go func() {
		runtime.LockOSThread()
		for f := range a.ch {
			f()
		}
	}()
	return a
}

// do runs f on the actor's thread and waits for it.
func (a *v8Actor) do(f func()) {
	done := make(chan struct{})
	a.ch <- func() {
		defer close(done)
		f()
	}
	<-done
}

func (a *v8Actor) close() { a.stop.Do(func() { close(a.ch) }) }

type v8RT struct {
	j   *jsVM
	act *v8Actor
	iso *v8shim.Isolate
	ctx *v8shim.Context

	heap      uint64
	timeoutNs atomic.Int64
	snap      bool // booted from a startup snapshot
	docURL    string
	modN      int

	hostDispatch hostFn // the host table's dispatcher (registerHost)
	names        []string

	// the evaluation watchdog (actor thread: depth; any thread: the rest)
	depth  int
	lbl    string
	t0     time.Time     // outermost entry start (jsStatsOn)
	nested time.Duration // host and loader time inside it (jsStatsOn)
	timer  *time.Timer
	mu     sync.Mutex
	armed  bool
	fired  bool
	dead   bool // the heap limit was hit: the isolate must not run any more
}

var errV8Dead = errors.New("InternalError: out of memory (JavaScript heap limit reached)")

func (r *v8RT) kind() string { return JSEngineV8 }

// V8's code cache for fetched scripts is opt-in (SWEEF_V8_CODECACHE=on). Measured on the six replay pages it saved
// 50-160 ms warm on three of them and cost 390 ms on nextjs.org/docs: the cache is made by compiling every
// function eagerly in the background, and consuming that much bytecode is not always cheaper than V8's lazy
// compile. A cache made from what a page really ran (after execution) is the follow-up.
var v8NoCodeCache = os.Getenv("SWEEF_V8_CODECACHE") != "on"

// SWEEF_V8_SLOW=<ms> prints every entry into V8 that spent longer than that in the engine itself.
var v8SlowMs, _ = strconv.Atoi(os.Getenv("SWEEF_V8_SLOW"))

// buildV8VM creates an unbound VM on V8: limits set, host object bound, prelude loaded (from the startup
// snapshot when there is one).
func buildV8VM(o Options) (*jsVM, error) {
	mem := o.JSMemoryMiB
	if mem <= 0 {
		mem = 256
	}
	to := time.Duration(o.JSTimeoutMs) * time.Millisecond
	if to <= 0 {
		to = 5 * time.Second
	}
	r := &v8RT{heap: uint64(mem) << 20, act: newV8Actor()}
	// The page's script timeout must not apply to our own prelude.
	r.timeoutNs.Store(int64(60 * time.Second))
	r.timer = time.AfterFunc(time.Hour, r.expire)
	r.timer.Stop()
	j := &jsVM{start: time.Now(), executed: map[int64]bool{}, session: map[string]string{}}
	j.jr, r.j = r, j
	fail := func(err error) (*jsVM, error) {
		r.destroy()
		return nil, err
	}
	if err := j.registerHost(); err != nil {
		return fail(fmt.Errorf("host registration: %w", err))
	}
	if !r.snap {
		if err := r.evalPrelude(); err != nil {
			return fail(fmt.Errorf("prelude: %w", err))
		}
	}
	// Never hand out a half-initialised VM.
	if v, err := r.evalOn(`typeof __swSetDocument === 'function' && typeof __swWsEvent === 'function'`, "sweef:check"); err != nil || v.Kind != v8shim.Bool || v.Int == 0 {
		return fail(fmt.Errorf("prelude did not initialise"))
	}
	r.timeoutNs.Store(int64(to))
	return j, nil
}

// installHost creates the isolate and its context on the actor, binds the dispatcher and, without a snapshot,
// defines __sw.
func (r *v8RT) installHost(j *jsVM, names []string, dispatch hostFn) (err error) {
	blob, _ := v8SnapshotFor(names, dispatch, r.act)
	r.act.do(func() {
		if os.Getenv("SWEEF_V8_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "V8DEBUG new isolate heap=%d snapshot=%d\n", r.heap, len(blob))
		}
		iso, e := v8shim.NewIsolate(v8shim.Options{HeapLimit: r.heap, Snapshot: blob})
		if e != nil && blob != nil {
			blob = nil // a snapshot the library refuses: boot from source
			iso, e = v8shim.NewIsolate(v8shim.Options{HeapLimit: r.heap})
		}
		if e != nil {
			err = e
			return
		}
		v8Active.Store(&blob) // after the fallback above: what the isolate really runs on
		r.iso = iso
		iso.Loader = r.loadModule
		iso.ImportMeta = importMetaURL
		ctx, e := iso.NewContext()
		if e != nil {
			err = e
			return
		}
		r.ctx = ctx
		r.snap = blob != nil
		r.hostDispatch = dispatch
		r.names = names
		if e := ctx.Register("__sw_native", r.hostCall); e != nil {
			err = e
			return
		}
		bind := "__swBind(__sw_native); delete globalThis.__sw_native; delete globalThis.__swBind;"
		if !r.snap {
			bind = hostBindJS(names, nil) + bind
		}
		if _, e := ctx.Eval(bind, "sweef:host"); e != nil {
			err = e
		}
	})
	return err
}

// evalPrelude boots the prelude from source (no snapshot).
func (r *v8RT) evalPrelude() error {
	// the host object is defined by installHost; the prelude proper follows it
	src := v8PreludeSource(nil, nil)
	if i := strings.Index(src, "\n"); i >= 0 {
		src = src[i+1:] // drop hostBindJS: __sw exists already
	}
	_, err := r.evalOn(src, "sweef:prelude")
	return err
}

func (r *v8RT) destroy() {
	if r.act == nil {
		return
	}
	r.act.do(func() {
		r.timer.Stop()
		if r.ctx != nil {
			r.ctx.Close()
		}
		if r.iso != nil {
			r.iso.Close()
		}
	})
	r.act.close()
}

// ---- watchdog ------------------------------------------------------------------------------------------

func (r *v8RT) expire() {
	r.mu.Lock()
	if r.armed {
		r.fired = true
		r.iso.Terminate()
	}
	r.mu.Unlock()
}

// begin arms the evaluation timeout for an outermost entry (actor thread).
func (r *v8RT) begin() {
	if r.depth++; r.depth > 1 {
		return
	}
	if jsStatsOn {
		r.t0, r.nested = time.Now(), 0
	}
	r.mu.Lock()
	r.armed, r.fired = true, false
	r.mu.Unlock()
	r.timer.Reset(time.Duration(r.timeoutNs.Load()))
}

func (r *v8RT) end() {
	if r.depth--; r.depth > 0 {
		return
	}
	if jsStatsOn {
		core := time.Since(r.t0) - r.nested
		jsSt.coreNs.Add(int64(core))
		if v8SlowMs > 0 && core > time.Duration(v8SlowMs)*time.Millisecond {
			fmt.Fprintf(os.Stderr, "V8SLOW %.0fms core (%.0fms total) %s\n", float64(core.Microseconds())/1000, float64(time.Since(r.t0).Microseconds())/1000, r.lbl)
		}
	}
	r.timer.Stop()
	r.mu.Lock()
	r.armed = false
	f := r.fired
	r.fired = false
	r.mu.Unlock()
	if f {
		r.iso.CancelTerminate()
	}
}

// fix turns the shim's termination and out-of-memory statuses into the errors the QuickJS runtime reports.
func (r *v8RT) fix(err error) error {
	var ex *v8shim.Exception
	if errors.As(err, &ex) {
		switch {
		case ex.OOM:
			r.dead = true
			return errV8Dead
		case ex.Terminated:
			return errors.New("InternalError: interrupted (script timed out)")
		}
	}
	return err
}

// ---- host calls ----------------------------------------------------------------------------------------

// hostCall is the one native function of the VM: JS -> Go, on the actor's thread.
func (r *v8RT) hostCall(c *v8shim.Call) {
	if jsStatsOn {
		t0 := time.Now()
		defer func() { r.nested += time.Since(t0) }()
	}
	n := c.N()
	args := make([]any, n)
	for i := 0; i < n; i++ {
		args[i] = hostArg(c, i)
	}
	res, err := r.hostDispatch(args)
	if err != nil {
		c.RetThrow(err.Error())
		return
	}
	if jsStatsOn && n > 0 {
		jsonTopNote(r.names, int(toInt(args[0])), res)
	}
	if err := hostRet(c, res); err != nil {
		c.RetThrow(err.Error())
	}
}

// ---- evaluation ----------------------------------------------------------------------------------------

// evalOn runs a classic script on the actor.
func (r *v8RT) evalOn(src, name string) (v v8shim.Value, err error) {
	r.act.do(func() {
		if r.dead {
			err = errV8Dead
			return
		}
		r.lbl = "eval " + name
		r.begin()
		v, err = r.ctx.Eval(src, name)
		r.end()
		err = r.fix(err)
	})
	return
}

func (r *v8RT) run(j *jsVM, src, name string) (any, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrClosed
	}
	j.curScript = name
	defer func() { j.curScript = "" }()
	v, err := r.evalOn(src, scriptName(name))
	if err != nil {
		j.tab.logConsoleAt("error", jsErrorText(err, name), errLoc(err, name))
		return nil, err
	}
	return fromV8(v), nil
}

func scriptName(name string) string {
	if name == "" {
		return "sweef:script"
	}
	return name
}

func (r *v8RT) call(j *jsVM, fn string, args ...any) (any, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrClosed
	}
	return r.callLocked(fn, args...)
}

func (r *v8RT) callLocked(fn string, args ...any) (res any, err error) {
	cargs := toV8Args(args)
	r.act.do(func() {
		if r.dead {
			err = errV8Dead
			return
		}
		r.lbl = "call " + fn
		r.begin()
		v, e := r.ctx.Call(fn, cargs...)
		r.end()
		if e != nil {
			err = r.fix(e)
			return
		}
		res = fromV8(v)
	})
	return
}

func (r *v8RT) jobs(j *jsVM) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	rejected := false
	r.act.do(func() {
		if r.dead {
			return
		}
		r.lbl = "jobs"
		r.begin()
		r.iso.Microtasks()
		r.iso.Pump()
		rejected = r.iso.PendingRejections() > 0
		r.end()
	})
	if rejected {
		if _, err := r.callLocked("__swFlushRejections"); err != nil {
			j.tab.LogConsole("error", jsErrorText(err, "microtask"))
		}
	}
}

func (r *v8RT) interrupt() {
	if r.iso != nil {
		r.iso.Terminate()
	}
}

func (r *v8RT) closeVM() { r.destroy() }

func (r *v8RT) stats() jsHeapStats {
	var s v8shim.HeapStats
	if r.act != nil && r.iso != nil {
		r.act.do(func() { s = r.iso.HeapStats() })
	}
	return jsHeapStats{Engine: JSEngineV8, HeapUsed: s.Used, HeapSize: s.Total, HeapMax: s.Limit}
}

// ---- agent evaluate ------------------------------------------------------------------------------------

func isSyntaxError(err error) bool {
	var ex *v8shim.Exception
	return errors.As(err, &ex) && strings.HasPrefix(ex.Message, "SyntaxError")
}

func (r *v8RT) evaluate(j *jsVM, expr string) (json.RawMessage, error) {
	src := strings.TrimSpace(expr)
	if src == "" {
		return json.RawMessage("null"), nil
	}
	hasAwait := strings.Contains(src, "await ")
	candidates := []string{}
	if hasAwait {
		candidates = append(candidates, "(async () => { return (\n"+src+"\n); })()", "(async () => {\n"+src+"\n})()")
	}
	candidates = append(candidates, "(\n"+src+"\n)", src)

	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		return nil, ErrClosed
	}
	var err error
	for i, c := range candidates {
		r.act.do(func() {
			if r.dead {
				err = errV8Dead
				return
			}
			r.begin()
			err = r.fix(r.ctx.EvalStash(c, "sweef:evaluate"))
			r.end()
		})
		if err == nil {
			break
		}
		if isSyntaxError(err) && i < len(candidates)-1 {
			continue
		}
		break
	}
	if err != nil {
		j.mu.Unlock()
		return nil, fmt.Errorf("%s", jsErrorText(err, ""))
	}
	v, err := r.callLocked("__swAgentSettle")
	j.mu.Unlock()
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	if async, _ := m["async"].(bool); !async {
		return marshalAgent(m["res"])
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if j.pump(50 * time.Millisecond) {
			return json.RawMessage("null"), nil // navigated away
		}
		pv, perr := j.call("__swAgentPoll")
		if perr != nil {
			return nil, perr
		}
		pm, _ := pv.(map[string]any)
		if done, _ := pm["done"].(bool); done {
			if ok, _ := pm["ok"].(bool); !ok {
				s, _ := pm["err"].(string)
				return nil, fmt.Errorf("%s", s)
			}
			return marshalAgent(pm["res"])
		}
		if time.Now().After(deadline) {
			return nil, errEvalTimeout
		}
	}
}

func marshalAgent(res any) (json.RawMessage, error) {
	b, err := json.Marshal(normalizeJS(res))
	if err != nil {
		return json.RawMessage(fmt.Sprintf("%q", fmt.Sprint(res))), nil
	}
	return b, nil
}

// ---- code cache ----------------------------------------------------------------------------------------

func (r *v8RT) runCached(j *jsVM, src, name string) (any, error) {
	if len(src) < 256 {
		j.bcState = "small"
		return r.run(j, src, name)
	}
	cache := j.tab.eng.v8code
	if v8NoCodeCache {
		cache = nil
	}
	key := codeKey(src)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrClosed
	}
	var blob []byte
	if cache != nil {
		if w := cache.get(key); w != nil {
			if blob = v8CacheUnwrap(w); blob == nil {
				cache.forget(key) // damaged on disk: V8 never sees it
			}
		}
	}
	j.bcState = "miss"
	j.curScript = name
	defer func() { j.curScript = "" }()
	var v v8shim.Value
	var err error
	rejected := false
	r.act.do(func() {
		if r.dead {
			err = errV8Dead
			return
		}
		r.lbl = "script " + name
		r.begin()
		if blob != nil {
			j.bcState = "hit"
			v, rejected, err = r.ctx.EvalCached(src, scriptName(name), blob)
		} else {
			v, err = r.ctx.Eval(src, scriptName(name))
		}
		r.end()
		err = r.fix(err)
	})
	if rejected && cache != nil {
		cache.forget(key)
		j.bcState = "rejected"
	}
	if err != nil {
		j.tab.logConsoleAt("error", jsErrorText(err, name), errLoc(err, name))
		return nil, err
	}
	if blob == nil && cache != nil {
		j.tab.eng.v8pc.submit(src, scriptName(name), key)
	}
	return fromV8(v), nil
}

// ---- modules -------------------------------------------------------------------------------------------

func (r *v8RT) installModuleLoader(j *jsVM, docURL string) {
	j.modDocURL = docURL
	r.docURL = docURL
}

// inlineModuleMark separates the document URL from a counter in the name of an inline module, so every inline
// module has its own identity while its base URL (and import.meta.url) stays the document's.
const inlineModuleMark = "#sweef-module-"

func stripInline(name string) string {
	if i := strings.Index(name, inlineModuleMark); i >= 0 {
		return name[:i]
	}
	return name
}

func importMetaURL(moduleName string) string { return stripInline(moduleName) }

// loadModule is the shim's resolver and loader: static imports and import(). It runs on the actor's thread.
func (r *v8RT) loadModule(spec, referrer string) (name, source string, err error) {
	defer jsStatLoad()()
	if jsStatsOn {
		t0 := time.Now()
		defer func() { r.nested += time.Since(t0) }()
	}
	j := r.j
	abs, err := j.resolveModule(stripInline(referrer), spec, r.docURL)
	if err != nil {
		return "", "", err
	}
	if _, ok := r.iso.ModuleByName(abs); ok {
		return abs, "", nil
	}
	body, status, err := j.tab.fetchResourceKind(abs, KindScript)
	if err != nil {
		return "", "", err
	}
	if status >= 400 {
		return "", "", fmt.Errorf("module %s: HTTP %d", abs, status)
	}
	j.prefetchModuleDeps(abs, body)
	return abs, body, nil
}

func (r *v8RT) runModule(j *jsVM, src, name string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrClosed
	}
	j.syncNet = true // a top-level await inside the module is awaited without an event loop: requests complete in place
	j.curScript = name
	defer func() { j.syncNet, j.curScript = false, "" }()
	named := strings.HasPrefix(name, "http://") || strings.HasPrefix(name, "https://")
	var err error
	r.act.do(func() {
		if r.dead {
			err = errV8Dead
			return
		}
		modName := name
		if !named {
			base := j.curBase
			if base == "" {
				base = r.docURL
			}
			r.modN++
			modName = base + inlineModuleMark + strconv.Itoa(r.modN)
		}
		r.lbl = "module " + name
		r.begin()
		defer func() { r.end() }()
		id, ok := r.iso.ModuleByName(modName)
		if !ok {
			var e error
			if id, e = r.ctx.CompileModule(src, modName); e != nil {
				err = r.fix(e)
				return
			}
		}
		// The module body runs now; what its promises queue stays queued for jobs(), as with QuickJS: a
		// checkpoint here would run the page's whole boot chain with syncNet set, awaiting every request in place.
		_, e := r.ctx.RunModuleDrain(id, false)
		err = r.fix(e)
	})
	if err != nil {
		j.tab.LogConsole("error", jsErrorText(err, name))
		return err
	}
	return nil
}
