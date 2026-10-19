package engine

// jsvm.go — the per-tab JavaScript virtual machine.
//
// Engine: modernc.org/quickjs, QuickJS transpiled to pure Go (ES2023, no
// CGO). The Go DOM stays the source of truth; dom_prelude.js implements the
// browser API in JavaScript on top of the host object `__sw` registered here.
// The contract between the two sides is documented in jsbridge.md.
//
// Threading: a quickjs VM is not goroutine-safe, so every entry into the VM
// goes through jsVM.mu. Host functions run while that lock is held and may
// take Tab.mu for DOM access, so code that holds Tab.mu must never call into
// the VM (see Tab.SetDocument and actions.go, which release it first).

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fnet "github.com/sweefbrowser/sweef/internal/net"
	"github.com/sweefbrowser/sweef/internal/v8shim"
	"github.com/sweefbrowser/sweef/internal/zpack"
	"modernc.org/quickjs"
)

//go:embed dom_prelude.js.z
var domPreludeZ string

// Extension points. Feature packages (canvas, WebGL, media, ...) add their
// browser API in their own files without touching the shared lists:
//
//	func init() {
//	    registerPrelude("canvas", canvasPreludeJS)           // evaluated after the base prelude
//	    registerHostExtension("canvas", func(j *jsVM) map[string]hostFn { ... })
//	}
//
// Host functions appear on the page's __sw object (e.g. __sw.canvasFill).
type hostFn = func(a []any) (any, error)

var (
	extMu          sync.Mutex
	preludeExtras  = map[string]string{} // name -> source
	preludeZExtras = map[string]string{} // name -> deflated source (zpack blob), for the preludes embedded as .js.z
	hostExtensions = map[string]func(j *jsVM) map[string]hostFn{}
)

func registerPrelude(name, src string) {
	extMu.Lock()
	preludeExtras[name] = src
	extMu.Unlock()
}

// registerPreludeZ is registerPrelude for a prelude embedded deflated (the NAME.js.z files, see internal/zpack and
// docs/development.md "Binary size budget"). It is inflated only when the prelude has to be compiled, which a warm
// bytecode cache avoids.
func registerPreludeZ(name, z string) {
	extMu.Lock()
	preludeZExtras[name] = z
	extMu.Unlock()
}

func registerHostExtension(name string, f func(j *jsVM) map[string]hostFn) {
	extMu.Lock()
	hostExtensions[name] = f
	extMu.Unlock()
}

// preludeNames is every registered extra in name order, so the bytecode cache key is stable. extMu is held.
func preludeNames() []string {
	names := make([]string, 0, len(preludeExtras)+len(preludeZExtras))
	for n := range preludeExtras {
		names = append(names, n)
	}
	for n := range preludeZExtras {
		if _, dup := preludeExtras[n]; !dup {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// fullPrelude is the base prelude, the WebSocket/Worker layer and every
// registered extra, in name order so the bytecode cache key is stable.
func fullPrelude() string {
	extMu.Lock()
	defer extMu.Unlock()
	var b strings.Builder
	b.WriteString(zpack.String(domPreludeZ))
	b.WriteString("\n")
	b.WriteString(zpack.String(domPreludeExtZ))
	for _, n := range preludeNames() {
		b.WriteString("\n;/* extension: " + n + " */\n")
		if src, ok := preludeExtras[n]; ok {
			b.WriteString(src)
		} else {
			b.WriteString(zpack.String(preludeZExtras[n]))
		}
	}
	if !preludeStripsSource() {
		b.WriteString("\n")
		b.WriteString(nativeToStringJS)
	}
	return b.String()
}

// The platform the preludes build prints as built-in code, "function f() { [native code] }", the way a browser's
// platform does. Pages tell their own code from the platform's that way: Optimizely's "Cleanse" deletes every
// non-native accessor from HTMLElement.prototype (so element.style was gone on the-internet.herokuapp.com and
// jQuery failed to start), and lodash, core-js and others test for native implementations the same way. The
// prelude is compiled without its source text (QuickJS then prints functions as native code, and the bytecode
// is smaller); where the C API is out of reach (preludeStripsSource false) nativeToStringJS does it instead.
//
// nativeToStringJS runs after every prelude: the platform the preludes built (constructors, prototype methods,
// accessors, functions on globals such as console or CSS) prints as built-in code, "function f() { [native
// code] }", the way a browser's platform does. Pages tell their own code from the platform's that way: Optimizely's
// "Cleanse" deletes every non-native accessor from HTMLElement.prototype (so element.style was gone on
// the-internet.herokuapp.com and jQuery failed to start), and lodash, core-js and others test for native
// implementations the same way. Functions the page defines keep their source.
const nativeToStringJS = `;(function (g) {
  var nat = new WeakSet(), FP = Function.prototype, orig = FP.toString;
  var gopn = Object.getOwnPropertyNames, gopd = Object.getOwnPropertyDescriptor, gpo = Object.getPrototypeOf;
  function fn(f) { if (typeof f === 'function') nat.add(f); }
  function props(o) {
    var ks; try { ks = gopn(o); } catch (e) { return; }
    for (var i = 0; i < ks.length; i++) {
      var d; try { d = gopd(o, ks[i]); } catch (e) { continue; }
      if (!d) continue;
      if (d.get) fn(d.get);
      if (d.set) fn(d.set);
      if (typeof d.value === 'function') fn(d.value);
    }
  }
  var seen = new WeakSet();
  function obj(o) {
    for (; o && (typeof o === 'object' || typeof o === 'function') && o !== Object.prototype && o !== FP; o = gpo(o)) {
      if (seen.has(o)) return;
      seen.add(o);
      props(o);
      if (typeof o === 'function' && o.prototype && typeof o.prototype === 'object') obj(o.prototype);
    }
  }
  var ks = gopn(g);
  for (var i = 0; i < ks.length; i++) {
    var d; try { d = gopd(g, ks[i]); } catch (e) { continue; }
    if (!d) continue;
    if (d.get) fn(d.get);
    if (d.set) fn(d.set);
    var v = d.value;
    if (v && (typeof v === 'function' || typeof v === 'object') && v !== g) { fn(v); obj(v); }
  }
  obj(gpo(g));
  props(Object.prototype); props(FP);
  var ts = function toString() {
    if (nat.has(this)) { var n = this.name; return 'function ' + (typeof n === 'string' ? n : '') + '() { [native code] }'; }
    return orig.call(this);
  };
  nat.add(ts);
  Object.defineProperty(FP, 'toString', { value: ts, writable: true, configurable: true, enumerable: false });
})(globalThis);
`

// preludeKey names the prelude for the bytecode cache without inflating it: it hashes what is embedded (the
// deflated blobs, which are committed files and only change when the source does) instead of the 1.2 MB of source,
// so a warm start neither inflates nor hashes the text.
func preludeKey() [32]byte {
	extMu.Lock()
	defer extMu.Unlock()
	h := sha256.New()
	part := func(tag, name, data string) {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", tag, name, len(data))
		h.Write([]byte(data))
	}
	part("z", "dom", domPreludeZ)
	part("z", "domext", domPreludeExtZ)
	if preludeStripsSource() {
		part("strip", "source", "")
	} else {
		part("r", "native-tostring", nativeToStringJS)
	}
	for _, n := range preludeNames() {
		if src, ok := preludeExtras[n]; ok {
			part("r", n, src)
		} else {
			part("z", n, preludeZExtras[n])
		}
	}
	var k [32]byte
	h.Sum(k[:0])
	return k
}

// The prelude is compiled to QuickJS bytecode once per process; loading
// bytecode into a fresh VM takes ~3 ms against ~40 ms to parse the source,
// which is most of the cost of a navigation.
var (
	preludeOnce     sync.Once
	preludeBytecode []byte
	preludeDiskMu   sync.Mutex
	preludeDiskDir  string
)

// setPreludeCacheDir lets the first engine point the prelude cache at disk.
func setPreludeCacheDir(dir string) {
	preludeDiskMu.Lock()
	if preludeDiskDir == "" {
		preludeDiskDir = dir
	}
	preludeDiskMu.Unlock()
}

func compiledPrelude() []byte {
	preludeOnce.Do(func() {
		preludeDiskMu.Lock()
		dir := preludeDiskDir
		preludeDiskMu.Unlock()
		var file string
		if dir != "" {
			key := preludeKey()
			file = filepath.Join(dir, "prelude-"+hex.EncodeToString(key[:8])+".qjsbc")
			if b, err := os.ReadFile(file); err == nil && len(b) > 0 {
				preludeBytecode = b
				return
			}
		}
		if b := embeddedPrelude(); b != nil { // built in (prelude_embed.go)
			preludeBytecode = b
			return
		}
		vm, err := quickjs.NewVM()
		if err != nil {
			return
		}
		defer vm.Close()
		restore := stripSource(vm)
		bc, err := vm.Compile(fullPrelude(), quickjs.EvalGlobal)
		restore()
		if err == nil {
			preludeBytecode = bc
			if file != "" {
				tmp := file + ".tmp"
				if os.WriteFile(tmp, bc, 0o600) == nil {
					_ = os.Rename(tmp, file)
				}
			}
		}
	})
	return preludeBytecode
}

type jsVM struct {
	quietOverride time.Duration // when > 0, a shorter quiet window for this pump (Tab.navQuiet); set by the headless loader

	modDocURL string      // the document URL bare module specifiers resolve against (installModuleLoader)
	fullPump  atomic.Bool // RunFor: pump for the whole duration (no quiet early stop)
	holdBG    atomic.Bool // explicit waits also wait on background (media clock) timers
	wakeMs    float64     // set by pumpUntil when a slice ends with a timer pending: ms until it is due
	bcState   string      // bytecode cache outcome of the last runCached (hit, miss, small), for the load timeline
	scriptCap time.Time   // pumpUntil stops draining page-inserted scripts at this instant (zero = drain them all)
	modMu     sync.Mutex
	modSeen   map[string]bool   // modules whose download prefetchModuleDeps started
	syncNet   bool              // a module script is being evaluated: requests are made in place (asynchttp.go)
	curBase   string            // URL of the script being run: relative imports resolve against it
	curScript string            // name of the script whose top level is executing, for console locations (Developer Tools)
	importMap map[string]string // <script type=importmap> entries
	mu        sync.Mutex
	jr        jsRuntime   // the engine running this VM (jsruntime.go): QuickJS or V8
	vm        *quickjs.VM // the QuickJS VM; nil when rt is V8 (QuickJS-only features check it)
	cv        *cvHost     // the canvas host of this VM, when the canvas extension is installed
	tab       *Tab
	start     time.Time
	closed    bool

	capture *capHost // getUserMedia & co (capture_host.go)
	cvh     *cvHost  // 2D canvas host (canvas.go), reachable for VideoFrame and ImageBitmap interop
	rtc     *rtcHost // peer connections (webrtc.go), reachable for track processors

	rtOnce   sync.Once
	realtime *realtime // WebSockets and Workers (lazily created)

	// closeHooks run after the QuickJS VM is closed: feature files hang their native teardown here (wasm.go).
	closeHooks []func()

	clMu    sync.Mutex
	cluster *agentCluster // SharedArrayBuffer agent cluster (sab.go)
	sabv    *vmSAB
	wasmh   *wasmHost
	wch     *wcHost  // WebCodecs (webcodecs.go)
	rech    *recHost // MediaRecorder (mediarecorder.go)

	closerMu sync.Mutex
	closers  []func() // run first when the VM closes (host extensions, media players, audio streams); they must not call into the VM

	pendingNav  string
	scriptQueue []int64        // script element ids inserted by page code
	executed    map[int64]bool // scripts already run
	session     map[string]string
	currentDoc  *fnet.Doc
}

// buildQJSVM creates an unbound QuickJS VM: limits set, host object registered, prelude
// evaluated. Evaluating the 3,000-line prelude is most of the cost of a
// JavaScript navigation (~40-60 ms), so the engine keeps a pool of these
// built ahead of time (see vmPool) and a navigation only binds one.
func buildQJSVM(o Options) (*jsVM, error) {
	vm, err := quickjs.NewVM()
	if err != nil {
		return nil, err
	}
	mem := o.JSMemoryMiB
	if mem <= 0 {
		mem = DefaultJSMemoryMiB
	}
	vm.SetMemoryLimit(uintptr(mem) << 20)
	vm.SetMaxStackSize(1 << 20)
	// Loading the prelude allocates a few MiB of long-lived objects and almost no garbage, but QuickJS's
	// cycle collector would still run several times on the way (its threshold starts at 256 KiB), each pass
	// marking everything allocated so far. Measured: about 1.3 ms of the ~25 ms build, more as the prelude
	// grows. Turn the collector off for the build and give it a normal pace afterwards.
	vm.SetGCThreshold(1 << 30)
	to := time.Duration(o.JSTimeoutMs) * time.Millisecond
	if to <= 0 {
		to = 5 * time.Second
	}
	// The page's script timeout must not apply to our own prelude: on a slow
	// or loaded machine it can exceed a short page budget.
	vm.SetEvalTimeout(60 * time.Second)
	defer vm.SetEvalTimeout(to)
	j := &jsVM{vm: vm, start: time.Now(), executed: map[int64]bool{}, session: map[string]string{}}
	j.jr = &qjsRT{vm: vm}
	if err := j.registerHost(); err != nil {
		vm.Close()
		return nil, fmt.Errorf("host registration: %w", err)
	}
	loaded := false
	if bc := compiledPrelude(); bc != nil {
		if _, err := vm.EvalBytecode(bc); err == nil {
			loaded = true
		}
	}
	if !loaded {
		restore := stripSource(vm)
		_, err := vm.Eval(fullPrelude(), quickjs.EvalGlobal)
		restore()
		if err != nil {
			vm.Close()
			return nil, fmt.Errorf("prelude: %w", err)
		}
	}
	// Never hand out a half-initialised VM.
	if ok, err := vm.Eval(`typeof __swSetDocument === 'function' && typeof __swWsEvent === 'function'`, quickjs.EvalGlobal); err != nil || ok != true {
		vm.Close()
		return nil, fmt.Errorf("prelude did not initialise")
	}
	vm.SetGCThreshold(10 << 20)
	return j, nil
}

// newJSVM binds a VM (from the warm pool when one is ready) to the tab's
// current document. Returns nil (and logs) when no VM can be created, in
// which case the tab works in fetch+DOM mode.
func newJSVM(t *Tab, doc *fnet.Doc, docURL string) *jsVM {
	j := t.eng.vms.takeWait(500 * time.Millisecond)
	if j == nil {
		var err error
		if j, err = buildVM(t.eng.opts); err != nil {
			t.LogConsole("error", "sweef: cannot create JS VM: "+err.Error())
			return nil
		}
	}
	t.eng.vms.refill()
	j.mu.Lock()
	j.tab, j.currentDoc, j.start = t, doc, time.Now()
	j.mu.Unlock()
	j.installModuleLoader(docURL)
	if _, err := j.call("__swSetDocument", docURL, "loading"); err != nil {
		t.LogConsole("error", "sweef: __swSetDocument failed: "+err.Error())
	}
	_, _ = j.call("__swCoiBind")
	j.installBindings()
	if t.IsBackground() {
		_, _ = j.call("__swSetHidden", true, true) // a page opened or reloaded in a background tab starts hidden
	}
	if t.isFrame() {
		_, _ = j.call("__swFrameInit", t.windowName())
	}
	return j
}

// vmPool keeps a few VMs built ahead of time so navigations never pay for
// prelude evaluation on the critical path. Refills run in the background
// between agent commands; a VM is used for exactly one document, because
// page scripts leave globals behind.
type vmPool struct {
	mu       sync.Mutex
	ready    []*jsVM
	size     int
	building int // builders running
	opts     Options
	closed   bool
	built    chan struct{} // closed and replaced whenever a builder finishes (takeWait)
}

func newVMPool(o Options) *vmPool {
	size := o.WarmVMs
	if o.DisableJS {
		size = 0
	}
	p := &vmPool{size: size, opts: o, built: make(chan struct{})}
	p.refill()
	return p
}

func (p *vmPool) take() *jsVM {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ready) == 0 {
		return nil
	}
	j := p.ready[len(p.ready)-1]
	p.ready = p.ready[:len(p.ready)-1]
	return j
}

// takeQJS is take for a caller that needs a QuickJS VM (workers): V8 VMs stay in the pool for pages.
func (p *vmPool) takeQJS() *jsVM {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.ready) - 1; i >= 0; i-- {
		if p.ready[i].vm != nil {
			j := p.ready[i]
			p.ready = append(p.ready[:i], p.ready[i+1:]...)
			return j
		}
	}
	return nil
}

// takeWait is take, but when no VM is ready and one is being built it waits for that build (up to max)
// rather than building another beside it: right after the engine starts its first builds are under way, and
// a third build competing with them for the cores made the first eval of a fresh session wait longer.
func (p *vmPool) takeWait(max time.Duration) *jsVM {
	if p == nil {
		return nil
	}
	deadline := time.Now().Add(max)
	for {
		p.mu.Lock()
		if n := len(p.ready); n > 0 {
			j := p.ready[n-1]
			p.ready = p.ready[:n-1]
			p.mu.Unlock()
			return j
		}
		if p.building == 0 || p.closed {
			p.mu.Unlock()
			return nil
		}
		ch := p.built
		p.mu.Unlock()
		left := time.Until(deadline)
		if left <= 0 {
			return nil
		}
		select {
		case <-ch:
		case <-time.After(left):
			return nil
		}
	}
}

func (p *vmPool) refill() {
	if p == nil {
		return
	}
	// One builder per missing VM, in parallel: a VM takes ~40 ms to build, and a
	// script driving pages back to back (an SDK flow, a batch) can use VMs faster
	// than one builder makes them, which put the build back on the critical path.
	p.mu.Lock()
	defer p.mu.Unlock()
	for !p.closed && len(p.ready)+p.building < p.size {
		p.building++
		go func() {
			j, err := buildVM(p.opts)
			p.mu.Lock()
			p.building--
			close(p.built)
			p.built = make(chan struct{})
			if err != nil || p.closed || len(p.ready) >= p.size {
				p.mu.Unlock()
				if j != nil {
					j.close()
				}
				return
			}
			p.ready = append(p.ready, j)
			p.mu.Unlock()
		}()
	}
}

func (p *vmPool) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	ready := p.ready
	p.ready = nil
	p.mu.Unlock()
	for _, j := range ready {
		j.close()
	}
}

func (j *jsVM) close() {
	if j == nil {
		return
	}
	uninstallBytecodeLoader(j)
	// A script that is blocked (Atomics.wait, a wasm thread waiting on a futex) holds j.mu and would hold up this
	// close for ever: wake it first, without the lock.
	j.sabInterrupt()
	j.wasmAbort()
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	j.closed = true
	j.closerMu.Lock()
	cl := j.closers
	j.closers = nil
	j.closerMu.Unlock()
	for _, f := range cl {
		f()
	}
	if j.realtime != nil {
		j.realtime.closeAll()
	}
	runCloseHooks(j)
	if j.capture != nil {
		j.capture.closeAll()
	}
	j.jr.closeVM()
	for _, h := range j.closeHooks {
		h()
	}
	j.closeHooks = nil
}

// onClose registers f to run when the VM closes.
func (j *jsVM) onClose(f func()) {
	j.closerMu.Lock()
	defer j.closerMu.Unlock()
	j.closers = append(j.closers, f)
}

func (j *jsVM) nowMs() float64 { return float64(time.Since(j.start).Microseconds()) / 1000 }

// ---- VM entry points (all take j.mu) -----------------------------------------

// run evaluates classic script source. Errors are logged to the console ring
// with the script name, like a browser's uncaught-exception report.
func (j *jsVM) qjsRun(src, name string) (any, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrClosed
	}
	j.curScript = name
	defer func() { j.curScript = "" }()
	r, err := j.vm.Eval(src, quickjs.EvalGlobal)
	if err != nil {
		j.tab.logConsoleAt("error", jsErrorText(err, name), errLoc(err, name))
	}
	return r, err
}

// runCached executes classic script source through the process-wide code
// cache: large scripts are compiled to bytecode once and replayed after that.
func (j *jsVM) qjsRunCached(src, name string) (any, error) {
	if len(src) < 256 {
		j.bcState = "small"
		return j.run(src, name)
	}
	key := codeKey(src)
	cache := j.tab.eng.code
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrClosed
	}
	bc := cache.get(key)
	fromCache := bc != nil
	j.bcState = "miss"
	if fromCache {
		j.bcState = "hit"
	} else if bc = j.tab.eng.pc.wait(key); bc != nil {
		fromCache, j.bcState = true, "precompiled"
	}
	if bc == nil {
		var err error
		if bc, err = j.vm.Compile(src, quickjs.EvalGlobal); err != nil {
			j.tab.LogConsole("error", jsErrorText(err, name))
			return nil, err
		}
		cache.put(key, bc)
	}
	j.curScript = name
	defer func() { j.curScript = "" }()
	r, err := j.vm.EvalBytecode(bc)
	if err != nil {
		var qe *quickjs.Error
		if fromCache && !errors.As(err, &qe) {
			// unreadable cache entry: drop it and run from source
			cache.forget(key)
			r, err = j.vm.Eval(src, quickjs.EvalGlobal)
		}
		if err != nil {
			j.tab.logConsoleAt("error", jsErrorText(err, name), errLoc(err, name))
		}
	}
	return r, err
}

func (j *jsVM) qjsRunModule(src, name string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrClosed
	}
	j.syncNet = true // a top-level await inside the module is awaited without an event loop: requests complete in place
	j.curScript = name
	defer func() { j.syncNet, j.curScript = false, "" }()
	named := strings.HasPrefix(name, "http://") || strings.HasPrefix(name, "https://")
	if named {
		src = withImportMetaURL(src, name)
	}
	if len(src) < 256 && !named {
		j.bcState = "small"
		if _, err := j.vm.Eval(src, quickjs.EvalModule); err != nil {
			j.tab.LogConsole("error", jsErrorText(err, name))
			return err
		}
		return nil
	}
	// like classic scripts, modules replay compiled bytecode on a repeat visit (GitHub's code view is 780 KB of
	// module source: 3 s of compiling a second time otherwise)
	cache := j.tab.eng.code
	key := codeKey("\x00module\x00" + src)
	if named {
		key = codeKey("\x00module\x00" + name + "\x00" + src) // the bytecode carries the module's name
	}
	bc := cache.get(key)
	fromCache := bc != nil
	j.bcState = "miss"
	if fromCache {
		j.bcState = "hit"
	} else if named {
		if bc = j.tab.eng.pc.wait(key); bc != nil {
			fromCache, j.bcState = true, "precompiled"
		}
	}
	if bc == nil && named {
		// compiled and run under its URL: its relative imports, import.meta.url and its identity in the module
		// map are right (qjsnamed.go)
		if b, err, ok := runModuleNamed(j.vm, src, name); ok {
			if len(b) > 0 {
				cache.put(key, b)
			}
			if err != nil {
				j.tab.LogConsole("error", jsErrorText(err, name))
			}
			return err
		}
	}
	if bc == nil {
		var err error
		if bc, err = j.vm.Compile(src, quickjs.EvalModule); err != nil {
			j.tab.LogConsole("error", jsErrorText(err, name))
			return err
		}
		cache.put(key, bc)
	}
	_, err := j.vm.EvalBytecode(bc)
	if err != nil {
		var qe *quickjs.Error
		if fromCache && !errors.As(err, &qe) {
			cache.forget(key) // unreadable cache entry: run from source
			_, err = j.vm.Eval(src, quickjs.EvalModule)
		}
		if err != nil {
			j.tab.LogConsole("error", jsErrorText(err, name))
			return err
		}
	}
	return nil
}

// errLoc is the place an error was thrown, "name:line:col": the innermost frame of its stack, in the script that was
// running. A function of another script called from there is attributed to this one; the stack text says what it is.
func errLoc(err error, name string) string {
	var ex *v8shim.Exception
	if errors.As(err, &ex) {
		return ex.Location
	}
	var qe *quickjs.Error
	if name == "" || !errors.As(err, &qe) {
		return ""
	}
	for _, ln := range strings.Split(qe.Stack, "\n") {
		tag := "<eval>:"
		i := strings.Index(ln, tag)
		if i < 0 {
			tag = name + ":" // a module compiled under its URL (qjsnamed.go)
			i = strings.Index(ln, tag)
		}
		if i >= 0 {
			rest := strings.TrimRight(ln[i+len(tag):], ") ")
			if rest != "" && rest[0] >= '0' && rest[0] <= '9' {
				return name + ":" + rest
			}
		}
	}
	return ""
}

func jsErrorText(err error, name string) string {
	var ex *v8shim.Exception
	if errors.As(err, &ex) {
		s := ex.Message
		if st := strings.TrimRight(ex.Stack, "\n"); st != "" {
			if i := strings.IndexByte(st, '\n'); i >= 0 && strings.TrimSpace(st[:i]) == strings.TrimSpace(ex.Message) {
				st = st[i+1:] // V8's stack starts with the message line
			}
			if st != "" && st != ex.Message {
				s += "\n" + st
			}
		}
		if name != "" {
			s += "\n    in " + name
		}
		return s
	}
	var qe *quickjs.Error
	if errors.As(err, &qe) {
		s := qe.Name + ": " + qe.Message
		if qe.Stack != "" {
			s += "\n" + strings.TrimRight(qe.Stack, "\n")
		}
		if name != "" {
			s += "\n    in " + name
		}
		return s
	}
	if name != "" {
		return err.Error() + " (in " + name + ")"
	}
	return err.Error()
}

// call invokes a global prelude function and returns its JSON-ish result.
func (j *jsVM) qjsCall(fn string, args ...any) (any, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrClosed
	}
	return j.vm.Call(fn, args...)
}

// jobs drains the microtask queue.
func (j *jsVM) qjsJobs() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	for i := 0; i < 64; i++ {
		n, err := j.vm.ExecutePendingJobs()
		if err != nil {
			j.tab.LogConsole("error", jsErrorText(err, "microtask"))
		}
		if n == 0 {
			return
		}
	}
}

// dispatch fires a DOM event through the prelude; true means the default
// action should proceed (preventDefault was not called).
func (j *jsVM) dispatch(id int64, typ string, init map[string]any) bool {
	if j == nil {
		return true
	}
	switch typ {
	case "click", "mousedown", "pointerdown", "keydown", "touchstart", "input":
		if t := j.tab; t != nil && t.bc == nil { // the person used this top-level page: its site counts as visited
			t.Context().noteInteraction(schemefulSite(t.URL()))
		}
	}
	if init == nil {
		init = map[string]any{}
	}
	r, err := j.call("__swDispatch", id, typ, init)
	if err != nil {
		j.tab.LogConsole("error", jsErrorText(err, "dispatch "+typ))
		return true
	}
	b, ok := r.(bool)
	return !ok || b
}

func truthyAny(v any) bool {
	switch x := v.(type) {
	case nil, quickjs.Undefined:
		return false
	case bool:
		return x
	case int:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	}
	return true
}

// ---- event loop ----------------------------------------------------------------

// pump runs queued scripts, microtasks and due timers until the page is idle
// or the budget is spent. It performs a navigation requested by page code
// (location.href = …, form.submit()) and returns true when one happened.
func (j *jsVM) pump(budget time.Duration) bool {
	nav, _ := j.pumpUntil(time.Now().Add(budget), time.Time{})
	return nav
}

// pumpUntil is pump with an absolute settle deadline and an optional soft
// deadline for the slice (zero = none). With a slice the call never sleeps and
// never keeps draining queued scripts past it: it returns finished=false, with
// the delay until the next timer in j.wakeMs (0 = run again at once), and the
// caller (the progressive loader, which interleaves the page's work with input
// and painting) comes back later. finished=true means the page is idle or the
// settle deadline passed.
func (j *jsVM) pumpUntil(deadline, soft time.Time) (navigated, finished bool) {
	if j == nil {
		return false, true
	}
	j.wakeMs = 0
	// quiet: headless pumps stop once the document stopped changing and only timers remain (Tab.settleQuiet)
	var quiet time.Duration
	var lastGen uint64
	lastChange := time.Now()
	if soft.IsZero() && j.tab != nil && !j.tab.progMode.Load() && !j.fullPump.Load() {
		quiet = j.tab.settleQuiet()
		if q := j.quietOverride; q > 0 && (quiet == 0 || q < quiet) {
			quiet = q
		}
		lastGen = j.docGen()
	}
	var tTimers, tSleep, tDrain, tObs time.Duration
	var why [5]int // SWEEF_STAGES: what kept the pump going (scripts, delivered, live work, http, document changes)
	t0 := time.Now()
	if stageTrace {
		defer func() {
			if time.Since(t0) > 50*time.Millisecond {
				stageLog("pump", "", time.Since(t0), "quiet=%v more=%d delivered=%d live=%d http=%d docchg=%d timers=%.0fms sleep=%.0fms drain=%.0fms jobs+observers=%.0fms", quiet, why[0], why[1], why[2], why[3], why[4], ms(tTimers), ms(tSleep), ms(tDrain), ms(tObs))
			}
		}()
	}
	for {
		td := time.Now()
		j.drainScripts(soft)
		j.jobs()
		delivered := j.drainAsync()
		tDrain += time.Since(td)
		next := float64(-1)
		if j.tab == nil || !j.tab.suspended() {
			// A background tab under the Data Saver runs no timers (no polling, no
			// animation frames) and no workers; promise jobs and results already
			// delivered still flow, so an explicit evaluate does not hang.
			// a batch of due timers stops at the deadline: on a page whose animation frames each force a
			// layout, one batch could otherwise run for many seconds past the settle budget
			var budget float64
			if !deadline.IsZero() {
				budget = math.Max(1, float64(time.Until(deadline))/float64(time.Millisecond))
			}
			tt := time.Now()
			next = j.runTimers(budget)
			tTimers += time.Since(tt)
		}
		to := time.Now()
		if j.drainAsync() {
			delivered = true
		}
		j.jobs()
		j.flushObservers()
		tObs += time.Since(to)
		if nav := j.takeNav(); nav != "" {
			if strings.HasPrefix(nav, "form://") {
				id, _ := strconv.ParseInt(strings.TrimPrefix(nav, "form://"), 10, 64)
				_ = j.tab.SubmitFormBackend(id)
			} else if j.tab.progMode.Load() {
				_ = j.tab.NavigateProgressive(nav, nil)
			} else {
				_ = j.tab.Navigate(nav, nil)
			}
			return true, true
		}
		j.mu.Lock()
		more := len(j.scriptQueue) > 0
		if more && !j.scriptCap.IsZero() && !time.Now().Before(j.scriptCap) {
			more = false // load must not wait for a long chain of lazily loaded scripts; the settle pump drains them
		}
		j.mu.Unlock()
		sliceOver := !soft.IsZero() && !time.Now().Before(soft)
		httpWait := soft.IsZero() && (j.tab == nil || !j.tab.progMode.Load()) && j.httpInFlightBefore(deadline) // a window's tab never waits on the network
		if more || ((delivered || j.hasLiveWork()) && time.Now().Before(deadline)) || httpWait {
			if stageTrace {
				switch {
				case more:
					why[0]++
				case delivered:
					why[1]++
				case httpWait:
					why[3]++
				default:
					why[2]++
				}
			}
			if sliceOver {
				return false, false
			}
			if !more && !delivered {
				// only waiting for something in flight (a socket frame, a worker message, a detached image)
				if !soft.IsZero() {
					j.wakeMs = 2
					return false, false
				}
				time.Sleep(time.Millisecond)
				tSleep += time.Millisecond
			}
			continue
		}
		if next < 0 {
			return false, true
		}
		if quiet > 0 {
			if g := j.docGen(); g != lastGen {
				lastGen, lastChange = g, time.Now()
				why[4]++
			} else if time.Since(lastChange) >= quiet && !j.tab.framesBusy() { // a frame still loading is not quiet
				return false, true
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, true
		}
		wait := time.Duration(next) * time.Millisecond
		if wait > remaining {
			wait = remaining
		}
		if !soft.IsZero() {
			j.wakeMs = float64(wait) / float64(time.Millisecond)
			return false, false
		}
		if wait > 30*time.Millisecond {
			wait = 30 * time.Millisecond
		}
		if wait > 0 {
			time.Sleep(wait)
			tSleep += wait
		}
	}
}

// RunFor runs the page's event loop for d (timers, animation frames, network callbacks) without the early
// stop of a settle: the "wait <ms>" of an agent, after which an animation or a delayed render has progressed
// as far as it would have in a browser.
func (t *Tab) RunFor(d time.Duration) {
	t.runTasks()
	if t.beforeDCL() {
		time.Sleep(d)
		return
	}
	t.mu.RLock()
	j := t.js
	t.mu.RUnlock()
	if j == nil {
		time.Sleep(d)
		return
	}
	j.fullPump.Store(true)
	defer j.fullPump.Store(false)
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		j.pump(time.Until(end))
		if rest := time.Until(end); rest > 0 {
			time.Sleep(min(rest, 10*time.Millisecond)) // idle page: nothing due yet
		}
	}
}

// docGen is the content mutation counter of the tab's document (0 without one): style attribute writes do not
// move it (fnet.Doc.ContentGen), so an endless animation does not keep a settle from going quiet.
func (j *jsVM) docGen() uint64 {
	t := j.tab
	t.mu.RLock()
	d := t.dom
	t.mu.RUnlock()
	if d == nil {
		return 0
	}
	return d.ContentGen()
}

// Settle pumps the page's event loop for up to d (exported for RPC waits).
func (t *Tab) Settle(d time.Duration) {
	t.runTasks()
	if t.beforeDCL() {
		return
	}
	t.mu.RLock()
	j := t.js
	t.mu.RUnlock()
	if j != nil {
		j.holdBG.Store(true)
		defer j.holdBG.Store(false)
	}
	t.settle(d)
}

// beforeDCL: a progressive load has not reached DOMContentLoaded yet. Timers and
// queued work must not run ahead of the page's own scripts, so the post-action
// pumps wait until LoadStep gets there.
func (t *Tab) beforeDCL() bool {
	p := t.pl.Load()
	return p != nil && p.stage < stPre
}

// settle is the short pump used after every action.
func (t *Tab) settle(d time.Duration) {
	t.runTasks() // navigations other windows asked this one for
	if t.beforeDCL() {
		return
	}
	t.mu.RLock()
	j := t.js
	t.mu.RUnlock()
	if j != nil {
		j.pump(d)
	}
}

func (j *jsVM) runTimers(budgetMs float64) float64 {
	r, err := j.call("__swRunTimers", j.nowMs(), j.holdBG.Load(), budgetMs)
	if err != nil {
		j.tab.LogConsole("error", jsErrorText(err, "timers"))
		return -1
	}
	switch v := r.(type) {
	case int:
		return float64(v)
	case float64:
		return v
	}
	return -1
}

func (j *jsVM) flushObservers() {
	_, _ = j.call("__swFlushMutationObservers")
}

func (j *jsVM) takeNav() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := j.pendingNav
	j.pendingNav = ""
	return n
}

func (j *jsVM) lifecycle(name string) {
	if _, err := j.call("__swFireLifecycle", name); err != nil {
		j.tab.LogConsole("error", jsErrorText(err, name))
	}
}

// ---- evaluation for agents ------------------------------------------------------

var errEvalTimeout = errors.New("evaluate: promise did not settle in time")

// evaluate runs an agent expression, awaits a returned promise, and returns the
// JSON-serialised completion value.
func (j *jsVM) qjsEvaluate(expr string) (json.RawMessage, error) {
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
	var val quickjs.Value
	var err error
	for i, c := range candidates {
		val, err = j.vm.EvalValue(c, quickjs.EvalGlobal)
		if err == nil {
			break
		}
		var qe *quickjs.Error
		if errors.As(err, &qe) && qe.Name == "SyntaxError" && i < len(candidates)-1 {
			continue
		}
		break
	}
	if err != nil {
		j.mu.Unlock()
		return nil, fmt.Errorf("%s", jsErrorText(err, ""))
	}
	// Release the value under the VM lock: a frame's event loop (frames.go) and the
	// window that drives its tab may use the same VM from different goroutines, and
	// QuickJS's allocator is not safe against a concurrent free. No return path
	// below holds j.mu, so locking here cannot deadlock.
	defer func() {
		j.mu.Lock()
		if !j.closed {
			val.Free()
		}
		j.mu.Unlock()
	}()

	// Await thenables.
	isPromise, _ := j.vm.Call("__swIsThenable", val)
	settled := false
	var result any
	var rejected error
	if b, _ := isPromise.(bool); b {
		_, terr := val.Then(func(v quickjs.Value) {
			settled = true
			result, _ = j.serializeLocked(v)
		}, func(v quickjs.Value) {
			settled = true
			rejected = fmt.Errorf("%s", j.errorStringLocked(v))
		})
		if terr != nil {
			j.mu.Unlock()
			return nil, terr
		}
		j.mu.Unlock()
		deadline := time.Now().Add(10 * time.Second)
		for !settled {
			if j.pump(50 * time.Millisecond) {
				return json.RawMessage("null"), nil // navigated away
			}
			j.mu.Lock()
			done := settled
			j.mu.Unlock()
			if done {
				break
			}
			if time.Now().After(deadline) {
				return nil, errEvalTimeout
			}
		}
		if rejected != nil {
			return nil, rejected
		}
		b, err := json.Marshal(result)
		return b, err
	}
	result, err = j.serializeLocked(val)
	j.mu.Unlock()
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(result)
	if err != nil {
		return json.RawMessage(fmt.Sprintf("%q", fmt.Sprint(result))), nil
	}
	return b, nil
}

// serializeLocked converts a JS value into plain Go data through the prelude's
// __swSerialize (DOM nodes, functions, cycles handled). Caller holds j.mu.
func (j *jsVM) serializeLocked(v quickjs.Value) (any, error) {
	r, err := j.vm.Call("__swSerialize", v)
	if err != nil {
		a, aerr := v.Any()
		if aerr != nil {
			return nil, err
		}
		return normalizeJS(a), nil
	}
	return normalizeJS(r), nil
}

func (j *jsVM) errorStringLocked(v quickjs.Value) string {
	if e := quickjs.ErrorFromValue(v); e != nil {
		return e.Name + ": " + e.Message
	}
	r, err := j.vm.Call("String", v)
	if err != nil {
		return "rejected"
	}
	return fmt.Sprint(r)
}

// normalizeJS turns quickjs result types into json-friendly Go values.
func normalizeJS(v any) any {
	switch x := v.(type) {
	case quickjs.Undefined, quickjs.Unsupported:
		return nil
	case *quickjs.Object:
		var out any
		b, err := x.MarshalJSON()
		if err != nil || json.Unmarshal(b, &out) != nil {
			return x.String()
		}
		return out
	case []any:
		for i := range x {
			x[i] = normalizeJS(x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = normalizeJS(x[k])
		}
		return x
	}
	return v
}

// ---- module loader ---------------------------------------------------------------

func (j *jsVM) qjsInstallModuleLoader(docURL string) {
	j.modDocURL = docURL
	j.vm.SetModuleLoader(func(vm *quickjs.VM, name string) (string, error) {
		return j.loadModuleSource(name)
	}, func(_ *quickjs.VM, base, name string) (string, error) {
		return j.resolveModule(base, name, docURL)
	})
	// the same loader, reading precompiled bytecode when there is some (qjsloader.go)
	installBytecodeLoader(j)
}

// loadModuleSource fetches the module name and returns the source to compile (with its import.meta.url).
func (j *jsVM) loadModuleSource(name string) (string, error) {
	defer jsStatLoad()()
	if !strings.HasPrefix(name, "http://") && !strings.HasPrefix(name, "https://") {
		return "", fmt.Errorf("cannot resolve bare module specifier %q", name)
	}
	// the other import() calls already queued (Promise.all over a bundler's chunks) download now, in
	// parallel, instead of one round trip each as QuickJS gets to their jobs
	for _, d := range pendingDynamicImports(j.vm) {
		if u, err := j.resolveModule(d[0], d[1], j.modDocURL); err == nil {
			j.prefetchModule(u)
		}
	}
	body, status, err := j.tab.fetchResourceKind(name, KindScript)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("module %s: HTTP %d", name, status)
	}
	j.prefetchModuleDeps(name, body)
	return withImportMetaURL(body, name), nil
}

// resolveModule is the module name a specifier imported from base resolves to.
func (j *jsVM) resolveModule(base, name, docURL string) (string, error) {
	if strings.HasPrefix(name, "http://") || strings.HasPrefix(name, "https://") {
		return name, nil
	}
	b := base
	if !strings.HasPrefix(b, "http://") && !strings.HasPrefix(b, "https://") {
		// the importing script has no module name of its own (QuickJS names every evaluated source "<eval>"):
		// relative specifiers resolve against the script being run, as the browser does, else the document
		b = j.curBase
		if !strings.HasPrefix(b, "http://") && !strings.HasPrefix(b, "https://") {
			b = j.tab.URL()
			if b == "about:blank" {
				b = docURL
			}
		}
	}
	if strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
		return resolveURL(b, name)
	}
	if u, ok := j.mapImport(name); ok {
		return u, nil
	}
	return "", fmt.Errorf("bare module specifier %q is not supported (no import map entry)", name)
}

// addImportMap reads a <script type="importmap"> body: its "imports" table maps bare specifiers (and
// specifier prefixes ending in "/") to URLs, resolved against the document. Scopes are not supported.
func (j *jsVM) addImportMap(src, docURL string) {
	var m struct {
		Imports map[string]string `json:"imports"`
	}
	if json.Unmarshal([]byte(src), &m) != nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.importMap == nil {
		j.importMap = map[string]string{}
	}
	for k, v := range m.Imports {
		if abs, err := resolveURL(docURL, v); err == nil {
			j.importMap[k] = abs
		}
	}
}

// mapImport resolves a bare specifier through the import map: an exact entry, else the longest prefix entry.
func (j *jsVM) mapImport(spec string) (string, bool) {
	if u, ok := j.importMap[spec]; ok {
		return u, true
	}
	best := ""
	for k := range j.importMap {
		if strings.HasSuffix(k, "/") && strings.HasPrefix(spec, k) && len(k) > len(best) {
			best = k
		}
	}
	if best != "" {
		return j.importMap[best] + spec[len(best):], true
	}
	return "", false
}

// ---- host object -----------------------------------------------------------------

func argI(a []any, i int) int64 {
	if i >= len(a) {
		return 0
	}
	switch v := a[i].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

func argS(a []any, i int) string {
	if i >= len(a) || a[i] == nil {
		return ""
	}
	if s, ok := a[i].(string); ok {
		return s
	}
	return fmt.Sprint(a[i])
}

func argB(a []any, i int) bool {
	if i >= len(a) {
		return false
	}
	return truthyAny(a[i])
}

func idsToAny(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func (j *jsVM) withDoc(fn func(d *fnet.Doc) (any, error)) (any, error) {
	t := j.tab
	if t == nil { // pooled VM not bound to a tab yet
		return nil, fmt.Errorf("no document")
	}
	t.mu.RLock()
	d := t.dom
	t.mu.RUnlock()
	if d == nil {
		return nil, fmt.Errorf("no document")
	}
	return fn(d)
}

func (j *jsVM) registerHost() error {
	type h struct {
		name string
		fn   func(a []any) (any, error)
	}
	doc := func(fn func(d *fnet.Doc, a []any) (any, error)) func([]any) (any, error) {
		return func(a []any) (any, error) {
			return j.withDoc(func(d *fnet.Doc) (any, error) { return fn(d, a) })
		}
	}
	hs := []h{
		// tree
		{"docId", doc(func(d *fnet.Doc, a []any) (any, error) { return d.DocID(), nil })},
		{"docGen", doc(func(d *fnet.Doc, a []any) (any, error) { return int64(d.MutationGen() & (1<<52 - 1)), nil })},
		{"treeGen", doc(func(d *fnet.Doc, a []any) (any, error) { return int64(d.TreeGen() & (1<<52 - 1)), nil })},
		{"nodeType", doc(func(d *fnet.Doc, a []any) (any, error) { return d.NodeType(argI(a, 0)), nil })},
		{"tagName", doc(func(d *fnet.Doc, a []any) (any, error) { return d.TagName(argI(a, 0)), nil })},
		{"namespace", doc(func(d *fnet.Doc, a []any) (any, error) { return d.Namespace(argI(a, 0)), nil })},
		{"parent", doc(func(d *fnet.Doc, a []any) (any, error) { return d.ParentID(argI(a, 0)), nil })},
		{"children", doc(func(d *fnet.Doc, a []any) (any, error) { return idsToAny(d.ChildIDs(argI(a, 0))), nil })},
		{"firstChild", doc(func(d *fnet.Doc, a []any) (any, error) { return d.FirstChildID(argI(a, 0)), nil })},
		{"lastChild", doc(func(d *fnet.Doc, a []any) (any, error) { return d.LastChildID(argI(a, 0)), nil })},
		{"nextSibling", doc(func(d *fnet.Doc, a []any) (any, error) { return d.NextSiblingID(argI(a, 0)), nil })},
		{"prevSibling", doc(func(d *fnet.Doc, a []any) (any, error) { return d.PrevSiblingID(argI(a, 0)), nil })},
		{"createElement", doc(func(d *fnet.Doc, a []any) (any, error) { return d.CreateElement(argS(a, 0), argS(a, 1)), nil })},
		{"createText", doc(func(d *fnet.Doc, a []any) (any, error) { return d.CreateText(argS(a, 0)), nil })},
		{"createComment", doc(func(d *fnet.Doc, a []any) (any, error) { return d.CreateComment(argS(a, 0)), nil })},
		{"createFragment", doc(func(d *fnet.Doc, a []any) (any, error) { return d.CreateFragment(), nil })},
		{"appendChild", doc(func(d *fnet.Doc, a []any) (any, error) {
			p, c := argI(a, 0), argI(a, 1)
			if err := d.AppendChild(p, c); err != nil {
				return nil, err
			}
			j.noteInserted(d, c)
			j.tab.frameInserted(d, c)
			return c, nil
		})},
		{"insertBefore", doc(func(d *fnet.Doc, a []any) (any, error) {
			p, c := argI(a, 0), argI(a, 1)
			if err := d.InsertBefore(p, c, argI(a, 2)); err != nil {
				return nil, err
			}
			j.noteInserted(d, c)
			j.tab.frameInserted(d, c)
			return c, nil
		})},
		{"removeChild", doc(func(d *fnet.Doc, a []any) (any, error) {
			err := d.RemoveChild(argI(a, 0), argI(a, 1))
			go j.tab.pruneFrames()
			return argI(a, 1), err
		})},
		{"cloneNode", doc(func(d *fnet.Doc, a []any) (any, error) { return d.CloneNode(argI(a, 0), argB(a, 1)), nil })},
		{"attached", doc(func(d *fnet.Doc, a []any) (any, error) { return d.IsConnected(argI(a, 0)), nil })},
		{"refOf", doc(func(d *fnet.Doc, a []any) (any, error) { return d.RefForBackend(argI(a, 0)), nil })},
		// content
		{"nodeValue", doc(func(d *fnet.Doc, a []any) (any, error) { return d.NodeValue(argI(a, 0)), nil })},
		{"setNodeValue", doc(func(d *fnet.Doc, a []any) (any, error) { d.SetNodeValue(argI(a, 0), argS(a, 1)); return nil, nil })},
		{"textContent", doc(func(d *fnet.Doc, a []any) (any, error) { return d.TextContent(argI(a, 0)), nil })},
		{"innerText", doc(func(d *fnet.Doc, a []any) (any, error) { return innerTextOf(d, argI(a, 0)), nil })},
		{"setTextContent", doc(func(d *fnet.Doc, a []any) (any, error) { d.SetTextContent(argI(a, 0), argS(a, 1)); return nil, nil })},
		{"innerHTML", doc(func(d *fnet.Doc, a []any) (any, error) {
			s, err := d.InnerHTML(argI(a, 0))
			return s, err
		})},
		{"setInnerHTML", doc(func(d *fnet.Doc, a []any) (any, error) {
			err := d.SetInnerHTMLCtx(argI(a, 0), argS(a, 1))
			if err == nil {
				j.tab.frameInserted(d, argI(a, 0))
			}
			return nil, err
		})},
		{"outerHTML", doc(func(d *fnet.Doc, a []any) (any, error) { return d.SerializeNode(argI(a, 0)), nil })},
		{"insertAdjacentHTML", doc(func(d *fnet.Doc, a []any) (any, error) {
			err := d.InsertAdjacentHTML(argI(a, 0), argS(a, 1), argS(a, 2))
			if err == nil {
				j.tab.frameInserted(d, argI(a, 0))
				if p := d.ParentID(argI(a, 0)); p != 0 {
					j.tab.frameInserted(d, p)
				}
			}
			return nil, err
		})},
		// attributes
		{"getAttr", doc(func(d *fnet.Doc, a []any) (any, error) {
			v, ok := d.GetAttr(argI(a, 0), argS(a, 1))
			if !ok {
				return nil, nil
			}
			return v, nil
		})},
		{"setAttr", doc(func(d *fnet.Doc, a []any) (any, error) {
			d.SetAttr(argI(a, 0), argS(a, 1), argS(a, 2))
			return nil, nil
		})},
		{"removeAttr", doc(func(d *fnet.Doc, a []any) (any, error) { d.RemoveAttr(argI(a, 0), argS(a, 1)); return nil, nil })},
		{"hasAttr", doc(func(d *fnet.Doc, a []any) (any, error) { _, ok := d.GetAttr(argI(a, 0), argS(a, 1)); return ok, nil })},
		{"nextTabStop", doc(func(d *fnet.Doc, a []any) (any, error) { return d.NextTabStop(argI(a, 0), argB(a, 1)), nil })},
		{"attrNames", doc(func(d *fnet.Doc, a []any) (any, error) {
			names := d.AttrNames(argI(a, 0))
			out := make([]any, len(names))
			for i, n := range names {
				out[i] = n
			}
			return out, nil
		})},
		// queries
		{"querySelector", doc(func(d *fnet.Doc, a []any) (any, error) { return d.QueryFrom(argI(a, 0), argS(a, 1)) })},
		{"querySelectorAll", doc(func(d *fnet.Doc, a []any) (any, error) {
			ids, err := d.QueryAllFrom(argI(a, 0), argS(a, 1))
			return idsToAny(ids), err
		})},
		{"matches", doc(func(d *fnet.Doc, a []any) (any, error) { return d.Matches(argI(a, 0), argS(a, 1)) })},
		{"getElementById", doc(func(d *fnet.Doc, a []any) (any, error) { return d.ElementByID(argS(a, 0)), nil })},
		{"elementsByTag", doc(func(d *fnet.Doc, a []any) (any, error) {
			return idsToAny(d.ElementsBy(argI(a, 0), "tag", argS(a, 1))), nil
		})},
		{"elementsByClass", doc(func(d *fnet.Doc, a []any) (any, error) {
			return idsToAny(d.ElementsBy(argI(a, 0), "class", argS(a, 1))), nil
		})},
		{"elementsByName", doc(func(d *fnet.Doc, a []any) (any, error) { return idsToAny(d.ElementsBy(0, "name", argS(a, 0))), nil })},
		// form state
		{"getValue", doc(func(d *fnet.Doc, a []any) (any, error) { return d.GetValue(argI(a, 0)), nil })},
		{"setValue", doc(func(d *fnet.Doc, a []any) (any, error) { return nil, d.SetValue(argI(a, 0), argS(a, 1)) })},
		{"getChecked", doc(func(d *fnet.Doc, a []any) (any, error) { return d.GetChecked(argI(a, 0)), nil })},
		{"setChecked", doc(func(d *fnet.Doc, a []any) (any, error) { d.SetCheckedRaw(argI(a, 0), argB(a, 1)); return nil, nil })},
		{"selectedIndex", doc(func(d *fnet.Doc, a []any) (any, error) { return d.SelectedIndex(argI(a, 0)), nil })},
		{"setSelectedIndex", doc(func(d *fnet.Doc, a []any) (any, error) {
			d.SetSelectedIndex(argI(a, 0), int(argI(a, 1)))
			return nil, nil
		})},
		{"formOf", doc(func(d *fnet.Doc, a []any) (any, error) { return d.FormOf(argI(a, 0)), nil })},
		{"submitForm", func(a []any) (any, error) {
			j.queueFormSubmit(argI(a, 0))
			return nil, nil
		}},
		{"quirks", doc(func(d *fnet.Doc, a []any) (any, error) { return docQuirks(d), nil })},
		{"mediaMatches", func(a []any) (any, error) { return j.tab.mediaMatches(argS(a, 0)), nil }},
		{"computedStyle", doc(func(d *fnet.Doc, a []any) (any, error) { return computedStyleOf(d, argI(a, 0)), nil })},
		{"box", doc(func(d *fnet.Doc, a []any) (any, error) {
			if g, ok := d.GeometryOf(argI(a, 0)); ok {
				fr := make([]any, 0, len(g.Frags))
				for _, f := range g.Frags {
					fr = append(fr, []any{f[0], f[1], f[2], f[3]})
				}
				return map[string]any{"x": g.Client[0], "y": g.Client[1], "width": g.Client[2], "height": g.Client[3],
					"ox": g.Border[0], "oy": g.Border[1], "ow": g.Border[2], "oh": g.Border[3],
					"cw": g.ClientW, "ch": g.ClientH, "sw": g.ScrollW, "sh": g.ScrollH, "sl": g.ScrollLeft, "st": g.ScrollTop, "frags": fr, "precise": true}, nil
			}
			b, err := d.BoundingBox(argI(a, 0))
			if err != nil {
				return map[string]any{"x": 0, "y": 0, "width": 0, "height": 0}, nil
			}
			return map[string]any{"x": b[0], "y": b[1], "width": b[2], "height": b[3]}, nil
		})},
		// selection
		{"selGet", func(a []any) (any, error) {
			if j.tab == nil {
				return nil, nil
			}
			s := j.tab.SelGet()
			if !s.Has {
				return nil, nil
			}
			return []any{s.AnchorID, int64(s.AnchorOff), s.FocusID, int64(s.FocusOff)}, nil
		}},
		{"selSet", func(a []any) (any, error) {
			if j.tab != nil {
				j.tab.SelSet(argI(a, 0), int(argI(a, 1)), argI(a, 2), int(argI(a, 3)))
			}
			return nil, nil
		}},
		{"selText", func(a []any) (any, error) {
			if j.tab == nil {
				return "", nil
			}
			return j.tab.SelText(), nil
		}},
		{"selAll", func(a []any) (any, error) {
			if j.tab != nil {
				j.tab.SelectAll()
			}
			return nil, nil
		}},
		{"ctlSelGet", func(a []any) (any, error) {
			if j.tab == nil {
				return nil, nil
			}
			s, e, dir, ok := j.tab.CtlSelGet(argI(a, 0))
			if !ok {
				return nil, nil
			}
			return []any{int64(s), int64(e), int64(dir)}, nil
		}},
		{"ctlSelSet", func(a []any) (any, error) {
			if j.tab != nil {
				j.tab.CtlSelSet(argI(a, 0), int(argI(a, 1)), int(argI(a, 2)), int(argI(a, 3)))
			}
			return nil, nil
		}},
		{"setFocus", func(a []any) (any, error) {
			if j.tab != nil {
				j.tab.SetFocusNode(argI(a, 0))
			}
			return nil, nil
		}},
		{"rangeRects", func(a []any) (any, error) {
			if j.tab == nil {
				return []any{}, nil
			}
			rs := j.tab.RangeRects(argI(a, 0), int(argI(a, 1)), argI(a, 2), int(argI(a, 3)))
			out := make([]any, len(rs))
			for i, r := range rs {
				out[i] = []any{r[0], r[1], r[2], r[3]}
			}
			return out, nil
		}},
		{"caretMove", func(a []any) (any, error) {
			if j.tab == nil {
				return nil, nil
			}
			id, off, ok := j.tab.CaretMove(argI(a, 0), int(argI(a, 1)), argS(a, 2))
			if !ok {
				return nil, nil
			}
			return []any{id, int64(off)}, nil
		}},
		{"posAt", func(a []any) (any, error) {
			if j.tab == nil {
				return nil, nil
			}
			id, off, ok := j.tab.PosAtPoint(argF(a, 0), argF(a, 1))
			if !ok {
				return nil, nil
			}
			return []any{id, int64(off)}, nil
		}},
		{"caretRect", func(a []any) (any, error) {
			if j.tab == nil {
				return nil, nil
			}
			r, ok := j.tab.CaretRect(argI(a, 0), int(argI(a, 1)))
			if !ok {
				return nil, nil
			}
			return []any{r[0], r[1], r[2], r[3]}, nil
		}},
		{"scrollPos", func(a []any) (any, error) {
			if j.tab == nil {
				return nil, nil
			}
			return []any{float64(0), float64(j.tab.ScrollY())}, nil
		}},
		{"scrollSet", func(a []any) (any, error) {
			if j.tab == nil {
				return nil, nil
			}
			cx, cy, changed := j.tab.SetElementScroll(argI(a, 0), argF(a, 1), argF(a, 2))
			return []any{cx, cy, changed}, nil
		}},
		{"scrollWindow", func(a []any) (any, error) {
			if j.tab == nil || len(a) < 2 {
				return false, nil
			}
			return j.tab.scrollWindow(argF(a, 0), argF(a, 1), argS(a, 2)), nil
		}},
		{"elementFromPoint", func(a []any) (any, error) {
			id, err := j.tab.hitTest(int(argI(a, 0)), int(argI(a, 1)))
			if err != nil {
				return 0, nil
			}
			return id, nil
		}},
		// environment
		{"log", func(a []any) (any, error) {
			level := argS(a, 0)
			if level == "" {
				level = "log"
			}
			if j.tab != nil {
				loc := argS(a, 2) // "line:col" in the script being run
				if loc != "" && j.curScript != "" {
					loc = j.curScript + ":" + loc
				} else {
					loc = ""
				}
				j.tab.logConsoleAt(level, argS(a, 1), loc)
			}
			return nil, nil
		}},
		{"url", func(a []any) (any, error) { return j.tab.URL(), nil }},
		{"navigate", func(a []any) (any, error) {
			u, err := resolveURL(j.tab.URL(), argS(a, 0))
			if err != nil {
				return nil, err
			}
			j.pendingNav = u // j.mu is held by the running eval
			return nil, nil
		}},
		{"replaceUrl", func(a []any) (any, error) {
			u, err := resolveURL(j.tab.URL(), argS(a, 0))
			if err != nil {
				return nil, err
			}
			j.tab.setURLOnly(u)
			return nil, nil
		}},
		// setUrl: history.pushState/replaceState and same-document traversals moved the document's URL. The
		// prelude has called it from the start but no host function had the name, so Tab.URL (and with it
		// get url, wait --url, expect url and the agent's observation header) kept the URL the document was
		// loaded from on every client-side route change. The session history itself stays in the page.
		{"setUrl", func(a []any) (any, error) {
			u, err := resolveURL(j.tab.URL(), argS(a, 0))
			if err != nil {
				return nil, err
			}
			pu, err := url.Parse(u)
			if err != nil {
				return nil, err
			}
			t := j.tab
			t.mu.Lock()
			t.url = pu
			if t.hIdx >= 0 && t.hIdx < len(t.history) {
				t.history[t.hIdx] = pu
			}
			t.mu.Unlock()
			return nil, nil
		}},
		{"userAgent", func(a []any) (any, error) { return j.tab.UserAgent(), nil }},
		{"viewport", func(a []any) (any, error) {
			w, h, dsf, mobile, _ := j.tab.Viewport()
			return map[string]any{"width": w, "height": h, "dsf": dsf, "mobile": mobile, "colorScheme": j.tab.ColorSchemeName()}, nil
		}},
		{"now", func(a []any) (any, error) { return j.nowMs(), nil }},
		{"http", func(a []any) (any, error) {
			hdr := map[string]string{}
			if m, ok := normalizeJS(a[2]).(map[string]any); ok {
				for k, v := range m {
					hdr[k] = fmt.Sprint(v)
				}
			}
			body := ""
			if len(a) > 3 && a[3] != nil {
				body = argS(a, 3)
			}
			if len(a) > 4 && argB(a, 4) { // binary body, sent as base64
				raw, err := base64.StdEncoding.DecodeString(body)
				if err != nil {
					return nil, fmt.Errorf("TypeError: bad request body")
				}
				body = string(raw)
			}
			return j.tab.httpForScript(argS(a, 0), argS(a, 1), hdr, body)
		}},
		{"storageGet", func(a []any) (any, error) {
			kind, key := argS(a, 0), argS(a, 1)
			lk, err := j.webStorageKey()
			if err != nil {
				return nil, err
			}
			if kind == "session" {
				if v, ok := j.session[key]; ok {
					return v, nil
				}
				return nil, nil
			}
			if v, ok := j.tab.Context().GetStorageValue(lk, key); ok {
				return v, nil
			}
			return nil, nil
		}},
		{"storageSet", func(a []any) (any, error) {
			kind, key, val := argS(a, 0), argS(a, 1), argS(a, 2)
			lk, err := j.webStorageKey()
			if err != nil {
				return nil, err
			}
			if kind == "session" {
				j.session[key] = val
			} else {
				c := j.tab.Context()
				old, had := c.GetStorageValue(lk, key)
				c.SetStorageValue(lk, key, val)
				if !had || old != val {
					var op *string
					if had {
						op = &old
					}
					j.storageChanged(lk, &key, op, &val)
				}
			}
			return nil, nil
		}},
		{"storageRemove", func(a []any) (any, error) {
			kind, key := argS(a, 0), argS(a, 1)
			lk, err := j.webStorageKey()
			if err != nil {
				return nil, err
			}
			if kind == "session" {
				delete(j.session, key)
			} else {
				c := j.tab.Context()
				old, had := c.GetStorageValue(lk, key)
				c.RemoveStorageValue(lk, key)
				if had {
					j.storageChanged(lk, &key, &old, nil)
				}
			}
			return nil, nil
		}},
		{"storageClear", func(a []any) (any, error) {
			lk, err := j.webStorageKey()
			if err != nil {
				return nil, err
			}
			if argS(a, 0) == "session" {
				j.session = map[string]string{}
			} else {
				j.tab.Context().ClearStorage(lk)
				j.storageChanged(lk, nil, nil, nil)
			}
			return nil, nil
		}},
		{"storageKeys", func(a []any) (any, error) {
			lk, err := j.webStorageKey()
			if err != nil {
				return nil, err
			}
			var keys []string
			if argS(a, 0) == "session" {
				for k := range j.session {
					keys = append(keys, k)
				}
			} else {
				keys = j.tab.Context().StorageKeys(lk)
			}
			out := make([]any, len(keys))
			for i, k := range keys {
				out[i] = k
			}
			return out, nil
		}},
		{"cookie", func(a []any) (any, error) { return j.tab.DocumentCookie(), nil }},
		{"setCookie", func(a []any) (any, error) {
			j.tab.SetDocumentCookie(argS(a, 0))
			return nil, nil
		}},
		{"dialog", func(a []any) (any, error) {
			acc, txt := j.tab.HandleDialog(Dialog{Kind: argS(a, 0), Message: argS(a, 1)})
			if argS(a, 0) == "prompt" && txt == "" && acc {
				txt = argS(a, 2)
			}
			return map[string]any{"accepted": acc, "value": txt}, nil
		}},
		{"title", func(a []any) (any, error) { return j.tab.Title(), nil }},
		{"setTitle", func(a []any) (any, error) {
			j.tab.mu.Lock()
			j.tab.title = argS(a, 0)
			j.tab.mu.Unlock()
			return nil, nil
		}},
		{"mutation", func(a []any) (any, error) { return nil, nil }},
		{"docOrigin", func(a []any) (any, error) {
			if j.tab == nil {
				return "", nil
			}
			if o := j.tab.docOrigin(); o != "" {
				return o, nil
			}
			if j.tab.bc != nil { // opaque: a sandboxed frame, a data: document
				return "null", nil
			}
			return "", nil
		}},
		{"wsOpen", func(a []any) (any, error) { return j.wsOpen(argS(a, 0), argS(a, 1)), nil }},
		{"wsSend", func(a []any) (any, error) { return nil, j.wsSend(argI(a, 0), argS(a, 1), argB(a, 2)) }},
		{"wsClose", func(a []any) (any, error) { j.wsClose(argI(a, 0), int(argI(a, 1)), argS(a, 2)); return nil, nil }},
		{"wsBuffered", func(a []any) (any, error) { return j.wsBuffered(argI(a, 0)), nil }},
		{"binding", func(a []any) (any, error) {
			name, args := argS(a, 0), argS(a, 1)
			if !json.Valid([]byte(args)) {
				args = "[]"
			}
			c := BindingCall{Name: name, Args: json.RawMessage(args), URL: j.tab.URL(), At: time.Now()}
			j.tab.bindingRing.Push(Event{Tab: j.tab.ID, Kind: EvBinding, At: c.At, Data: c})
			j.tab.emit(EvBinding, c)
			if r, ok := j.tab.bindingReply(name); ok {
				return r, nil
			}
			return nil, nil
		}},
		{"download", func(a []any) (any, error) {
			u, name := argS(a, 0), argS(a, 1)
			if strings.HasPrefix(u, "data:") {
				mt, data, err := decodeDataURL(u)
				if err != nil {
					return nil, err
				}
				d := j.tab.SaveDownloadBytes(name, mt, data, "data:")
				return d.Path, nil
			}
			go func() { _, _ = j.tab.DownloadURL(u, name) }()
			return nil, nil
		}},
		{"saveDownload", func(a []any) (any, error) {
			data, err := base64.StdEncoding.DecodeString(argS(a, 1))
			if err != nil {
				return nil, err
			}
			d := j.tab.SaveDownloadBytes(argS(a, 0), argS(a, 2), data, argS(a, 3))
			return d.Path, nil
		}},
	}
	extMu.Lock()
	extNames := make([]string, 0, len(hostExtensions))
	for n := range hostExtensions {
		extNames = append(extNames, n)
	}
	sort.Strings(extNames)
	exts := make([]func(j *jsVM) map[string]hostFn, 0, len(extNames))
	for _, n := range extNames {
		exts = append(exts, hostExtensions[n])
	}
	extMu.Unlock()
	for _, ext := range exts {
		fns := ext(j)
		names := make([]string, 0, len(fns))
		for n := range fns {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			hs = append(hs, h{n, fns[n]})
		}
	}
	// One Go callback per VM, not one per host function: modernc quickjs allows 32767 registered callbacks in
	// the whole process, and with several hundred host functions a few dozen live VMs (pool, tabs, frames,
	// workers, UI pages) exhausted it ("too many registered functions"). __sw.name is a JS wrapper that passes
	// the function's index to the dispatcher.
	fns := make([]hostFn, len(hs))
	for i, x := range hs {
		fns[i] = x.fn
		if stageTrace {
			name, inner := x.name, x.fn
			fns[i] = func(a []any) (any, error) {
				t0 := time.Now()
				var n0 int64
				if j.tab != nil {
					n0, _ = j.tab.LayoutStats()
				}
				r, err := inner(a)
				hostStat(name, time.Since(t0))
				if j.tab != nil {
					if n1, _ := j.tab.LayoutStats(); n1 > n0 {
						hostStat("layout<-"+name, time.Since(t0)) // which bridge calls force layouts
					}
				}
				return r, err
			}
		}
	}
	names := make([]string, len(hs))
	for i, x := range hs {
		names[i] = x.name
	}
	return j.jr.installHost(j, names, func(a []any) (any, error) {
		if len(a) == 0 {
			return nil, fmt.Errorf("host call without index")
		}
		i := int(toInt(a[0]))
		if i < 0 || i >= len(fns) {
			return nil, fmt.Errorf("unknown host function %d", i)
		}
		if t := j.tab; t != nil {
			t.scriptGeom.Add(1)
			defer t.scriptGeom.Add(-1)
		}
		if jsStatsOn {
			t0 := time.Now()
			defer func() {
				jsSt.hostCalls.Add(1)
				jsSt.hostNs.Add(int64(time.Since(t0)))
			}()
		}
		return fns[i](a[1:])
	})
}

// noteInserted queues newly connected <script> elements for execution, the
// way browsers run scripts inserted by page code (webpack chunk loading,
// analytics tags, lazy widgets).
func (j *jsVM) noteInserted(d *fnet.Doc, id int64) {
	if !d.IsConnected(id) {
		return
	}
	var scripts []int64
	if d.TagName(id) == "script" {
		scripts = append(scripts, id)
	} else {
		scripts = append(scripts, d.ElementsBy(id, "tag", "script")...)
	}
	for _, s := range scripts {
		if !j.executed[s] {
			j.scriptQueue = append(j.scriptQueue, s)
			j.prefetchInserted(d, s)
		}
	}
}

// prefetchInserted starts downloading a script that page code just inserted, so the downloads of a burst of
// them (webpack chunks, tag managers) overlap each other and the execution of the ones before them; the loader
// joins the request in flight when its turn comes. Under a script budget the loader reads scripts in order.
func (j *jsVM) prefetchInserted(d *fnet.Doc, id int64) {
	t := j.tab
	if t == nil || t.policy().ScriptMaxBytes > 0 {
		return
	}
	el := d.ScriptElement(id)
	if el == nil || el.Src == "" || !isJavaScriptType(el.Type) || el.NoModule {
		return
	}
	abs, err := resolveURL(t.URL(), el.Src)
	if err != nil || !(strings.HasPrefix(abs, "http://") || strings.HasPrefix(abs, "https://")) {
		return
	}
	go func() {
		t.preloadSem <- struct{}{}
		defer func() { <-t.preloadSem }()
		_, _, _ = t.fetchRes(abs, fetchOpts{kind: KindScript, prefetch: true})
	}()
}

func (j *jsVM) queueFormSubmit(formID int64) {
	t := j.tab
	t.mu.RLock()
	d := t.dom
	t.mu.RUnlock()
	if d == nil {
		return
	}
	if d.InfoByBackend(formID) == nil {
		return
	}
	j.pendingNav = "form://" + strconv.FormatInt(formID, 10)
}

// setURLOnly updates the tab URL without a navigation (history.pushState).
func (t *Tab) setURLOnly(raw string) {
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	t.mu.Lock()
	t.url = u
	if t.hIdx+1 < len(t.history) {
		t.history = t.history[:t.hIdx+1]
	}
	t.history = append(t.history, u)
	t.hIdx = len(t.history) - 1
	t.mu.Unlock()
}

// staticImportRE finds the specifiers of static imports and re-exports (not import(...) calls: those may never
// run). staticImportSpecs is the same match without the regexp engine, which took half a second of CPU over
// github.com's 770 kB modules; the regexp stays as its specification (tests).
var staticImportRE = regexp.MustCompile(`(?:\bimport|\bexport)\s*(?:[^'"();=]*?\bfrom\s*)?['"]([^'"\n]+)['"]`)

// moduleLikePath: a specifier the prefetcher may fetch. A browser resolves no file extensions, so a real
// import names its file in full; the import lines that bundles carry as text (code templates of an app
// builder, documentation) mostly do not ("./App", "./styles.css", "./routes.d.ts") and were fetched for
// nothing, 40 requests on pipilot.dev, ahead of the page's real modules.
func moduleLikePath(spec string) bool {
	p := spec
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if strings.HasSuffix(p, ".d.ts") {
		return false
	}
	dot := strings.LastIndexByte(p, '.')
	if dot < 0 || strings.LastIndexByte(p, '/') > dot {
		return !strings.HasPrefix(spec, ".") && !strings.HasPrefix(spec, "/") // a bare name: the import map decides
	}
	switch p[dot+1:] {
	case "js", "mjs", "cjs", "jsx", "ts", "tsx", "vue", "svelte":
		return true
	}
	return false
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isRESpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r' }

// quotedSpec reads ['"]([^'"\n]+)['"] at i: the specifier and the index after the closing quote.
func quotedSpec(src string, i int) (string, int, bool) {
	if i >= len(src) || (src[i] != '"' && src[i] != '\'') {
		return "", 0, false
	}
	j := i + 1
	for j < len(src) && src[j] != '"' && src[j] != '\'' && src[j] != '\n' {
		j++
	}
	if j == i+1 || j >= len(src) || src[j] == '\n' {
		return "", 0, false
	}
	return src[i+1 : j], j + 1, true
}

// staticImportSpecs returns what staticImportRE.FindAllStringSubmatch would capture, in order.
func staticImportSpecs(src string) []string {
	var out []string
	for i := 0; i+6 <= len(src); {
		k := i + strings.IndexAny(src[i:], "ie")
		if k < i || k+6 > len(src) {
			break
		}
		if w := src[k : k+6]; (w != "import" && w != "export") || (k > 0 && isWordByte(src[k-1])) {
			i = k + 1
			continue
		}
		j := k + 6
		for j < len(src) && isRESpace(src[j]) {
			j++
		}
		// (?:[^'"();=]*?\bfrom\s*)? : the shortest run up to a "from" (at a word boundary) followed by a quote
		end, spec, ok := 0, "", false
		for m := j; m < len(src); m++ {
			c := src[m]
			if c == 'f' && strings.HasPrefix(src[m:], "from") && !isWordByte(src[m-1]) {
				q := m + 4
				for q < len(src) && isRESpace(src[q]) {
					q++
				}
				if s, e, good := quotedSpec(src, q); good {
					spec, end, ok = s, e, true
					break
				}
			}
			if c == '\'' || c == '"' || c == '(' || c == ')' || c == ';' || c == '=' {
				break
			}
		}
		if !ok { // the group is optional: a quote right after the keyword and spaces
			spec, end, ok = quotedSpec(src, j)
		}
		if ok {
			out = append(out, spec)
			i = end
			continue
		}
		i = k + 1
	}
	return out
}

// maxPrefetchedModules bounds the speculative reads of one document's module graph.
const maxPrefetchedModules = 400

// prefetchModuleDeps starts downloading the static imports of a module whose source was just read, and, as they
// arrive, theirs: QuickJS asks for the modules of a graph one at a time from inside the evaluation, which would
// make a page with a hundred small modules pay a hundred round trips in a row. The loader then finds each one
// in flight or already read.
func (j *jsVM) prefetchModuleDeps(base, src string) {
	t := j.tab
	if t == nil || t.policy().ScriptMaxBytes > 0 {
		return
	}
	for _, spec := range staticImportSpecs(src) {
		if !moduleLikePath(spec) {
			continue
		}
		var abs string
		switch {
		case strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://"):
			abs = spec
		case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || strings.HasPrefix(spec, "/"):
			u, err := resolveURL(base, spec)
			if err != nil {
				continue
			}
			abs = u
		default:
			u, ok := j.mapImport(spec)
			if !ok {
				continue
			}
			abs = u
		}
		if !(strings.HasPrefix(abs, "http://") || strings.HasPrefix(abs, "https://")) {
			continue
		}
		j.prefetchModule(abs)
	}
}

// prefetchModule starts reading one module (and then its static imports) unless it was already started.
func (j *jsVM) prefetchModule(abs string) {
	t := j.tab
	if t == nil || t.policy().ScriptMaxBytes > 0 || !(strings.HasPrefix(abs, "http://") || strings.HasPrefix(abs, "https://")) {
		return
	}
	j.modMu.Lock() // not j.mu: the loader runs inside an evaluation that holds it
	if j.modSeen == nil {
		j.modSeen = map[string]bool{}
	}
	dup := j.modSeen[abs] || len(j.modSeen) >= maxPrefetchedModules
	j.modSeen[abs] = true
	j.modMu.Unlock()
	if dup {
		return
	}
	go func() {
		t.preloadSem <- struct{}{}
		body, status, err := t.fetchRes(abs, fetchOpts{kind: KindScript, prefetch: true})
		<-t.preloadSem
		if err == nil && status < 400 {
			t.eng.pc.submitModule(abs, body)
			j.prefetchModuleDeps(abs, body)
		}
	}()
}
