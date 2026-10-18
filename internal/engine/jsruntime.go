package engine

// jsruntime.go: the seam between the engine and the JavaScript runtime that executes a page.
//
// The engine (jsvm.go and the files around it) owns everything about a page's JavaScript that is not the
// language runtime itself: the host function table (registerHost: ~100 backend-neutral closures behind the
// single dispatcher __sw_call), the prelude that builds the browser API on top of it, the event loop
// (pumpUntil: quiet windows, budgets, timers, scripts inserted by the page) and the load-time bookkeeping.
// A jsRuntime is the engine room underneath: it creates the context, evaluates scripts and modules, runs
// microtasks, and calls prelude functions. Two implementations exist:
//
//	qjsRT  QuickJS, transpiled to pure Go (jsrt_qjs.go). The default; its code is the original jsvm.go code
//	       moved behind this interface without behaviour changes.
//	v8RT   V8 through the purego shim (jsrt_v8.go, internal/v8shim). Selected by Options.JSEngine / SWEEF_JS_ENGINE.
//
// Every method that takes j expects the caller NOT to hold j.mu unless said otherwise: the runtime methods
// take it themselves, exactly as the jsVM methods did before the seam existed.
//
// Features that reach into the QuickJS C API directly (canvas pixel buffers, the WebAssembly host, the
// SharedArrayBuffer agent cluster, WebCodecs buffers) are QuickJS-only; they check j.vm != nil and report
// errV8Unsupported on V8 (see the feature table in docs/js-runtime.md).

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sweefbrowser/sweef/internal/v8shim"
)

// jsRuntime is the language runtime of one jsVM.
type jsRuntime interface {
	// kind is "quickjs" or "v8".
	kind() string

	// installHost makes the dispatcher reachable from JavaScript as the per-VM table `__sw`: names[i] is the
	// host function whose index is i, and a call from JS arrives at dispatch as (i, args...).
	installHost(j *jsVM, names []string, dispatch hostFn) error

	// run evaluates classic script source in the global scope. Errors are logged to the tab's console.
	run(j *jsVM, src, name string) (any, error)
	// runCached is run through the process-wide code cache (QuickJS bytecode, V8 code cache).
	runCached(j *jsVM, src, name string) (any, error)
	// runModule evaluates an ES module (name is its URL, or "" for an inline module).
	runModule(j *jsVM, src, name string) error
	// installModuleLoader sets the resolver and loader that static imports and import() use.
	installModuleLoader(j *jsVM, docURL string)

	// call invokes a global (prelude) function and returns its result as plain Go data.
	call(j *jsVM, fn string, args ...any) (any, error)
	// callLocked is call for a caller that already holds j.mu.
	callLocked(fn string, args ...any) (any, error)
	// jobs drains the microtask queue.
	jobs(j *jsVM)
	// evaluate runs an agent expression, awaits a returned promise, and returns its JSON.
	evaluate(j *jsVM, expr string) (json.RawMessage, error)

	// interrupt aborts the running script, from any goroutine.
	interrupt()
	// closeVM destroys the runtime (caller holds j.mu).
	closeVM()
	// stats reports heap numbers for diagnostics.
	stats() jsHeapStats
}

type jsHeapStats struct {
	Engine   string
	HeapUsed uint64
	HeapSize uint64
	HeapMax  uint64
}

func (j *jsVM) run(src, name string) (any, error) {
	if jsStatsOn {
		defer jsStatEnter()()
	}
	return j.jr.run(j, src, name)
}
func (j *jsVM) runCached(src, name string) (any, error) {
	if jsStatsOn {
		defer jsStatEnter()()
	}
	return j.jr.runCached(j, src, name)
}
func (j *jsVM) runModule(src, name string) error {
	if jsStatsOn {
		defer jsStatEnter()()
	}
	return j.jr.runModule(j, src, name)
}
func (j *jsVM) installModuleLoader(docURL string) { j.jr.installModuleLoader(j, docURL) }
func (j *jsVM) call(fn string, args ...any) (any, error) {
	if jsStatsOn {
		defer jsStatEnter()()
	}
	return j.jr.call(j, fn, args...)
}
func (j *jsVM) jobs() {
	if jsStatsOn {
		defer jsStatEnter()()
	}
	j.jr.jobs(j)
}
func (j *jsVM) evaluate(expr string) (json.RawMessage, error) { return j.jr.evaluate(j, expr) }

// jsStatsOn (SWEEF_JSSTATS=1) counts the time spent inside the JavaScript runtime and inside host functions, for
// "how much of this page load is the language runtime" (bench/loadtl -jsstats). Off by default: no cost.
var jsStatsOn = os.Getenv("SWEEF_JSSTATS") != ""

// EnableJSStats turns the accounting on (what SWEEF_JSSTATS does at start-up).
func EnableJSStats() { jsStatsOn = true }

var jsSt struct {
	entries, entryNs, hostCalls, hostNs, loadCalls, loadNs, coreNs, jsonCalls, jsonBytes, jsonNs atomic.Int64
	depth                                                                                        atomic.Int32
}

// JSStats is the process-wide runtime accounting since the last JSStatsReset.
type JSStats struct {
	Entries   int64   // calls from the engine into the runtime (outermost)
	RuntimeMs float64 // wall time inside the runtime, host functions included
	HostCalls int64   // JS -> Go host function calls
	HostMs    float64 // wall time inside host functions
	LoadCalls int64   // module loads the runtime asked the engine for (fetch + prefetch)
	LoadMs    float64 // wall time inside them
	CoreMs    float64 // V8 only: wall time on the runtime's thread outside host functions and module loads (compile + execution)
}

// ReadJSStats returns the counters (all zero unless SWEEF_JSSTATS is set).
func ReadJSStats() JSStats {
	return JSStats{jsSt.entries.Load(), float64(jsSt.entryNs.Load()) / 1e6, jsSt.hostCalls.Load(), float64(jsSt.hostNs.Load()) / 1e6, jsSt.loadCalls.Load(), float64(jsSt.loadNs.Load()) / 1e6, float64(jsSt.coreNs.Load()) / 1e6}
}

// ResetJSStats zeroes the counters.
func ResetJSStats() {
	jsSt.entries.Store(0)
	jsSt.entryNs.Store(0)
	jsSt.hostCalls.Store(0)
	jsSt.hostNs.Store(0)
	jsSt.loadCalls.Store(0)
	jsSt.loadNs.Store(0)
	jsSt.coreNs.Store(0)
}

// jsStatLoad times a module load (the fetch of a module's source from inside the runtime's resolver).
func jsStatLoad() func() {
	if !jsStatsOn {
		return func() {}
	}
	t0 := time.Now()
	return func() {
		jsSt.loadCalls.Add(1)
		jsSt.loadNs.Add(int64(time.Since(t0)))
	}
}

func jsStatEnter() func() {
	if jsSt.depth.Add(1) > 1 {
		return func() { jsSt.depth.Add(-1) }
	}
	t0 := time.Now()
	return func() {
		jsSt.entries.Add(1)
		jsSt.entryNs.Add(int64(time.Since(t0)))
		jsSt.depth.Add(-1)
	}
}

// JSEngine names.
const (
	JSEngineQuickJS = "quickjs"
	JSEngineV8      = "v8"
	JSEngineAuto    = "auto"
)

// EnvJSEngine overrides Options.JSEngine.
const EnvJSEngine = "SWEEF_JS_ENGINE"

// errV8Unsupported is what a QuickJS-only feature reports on the V8 runtime.
func errV8Unsupported(feature string) error {
	return fmt.Errorf("%s is not supported on the V8 runtime yet", feature)
}

var jsEngineNoteOnce sync.Once

// ResolveJSEngine decides which runtime a new Engine uses: the SWEEF_JS_ENGINE environment variable wins
// over o.JSEngine ("" means quickjs). "v8" needs the V8 library (SWEEF_V8_LIB, or next to the binary) and
// fails when it cannot be loaded; "auto" uses V8 when the library loads and otherwise QuickJS, with one line
// on stderr saying why. note is that line ("" when there is nothing to say).
func ResolveJSEngine(o Options) (kind, note string, err error) {
	mode := strings.ToLower(strings.TrimSpace(o.JSEngine))
	if e := strings.ToLower(strings.TrimSpace(os.Getenv(EnvJSEngine))); e != "" {
		mode = e
	}
	switch mode {
	case "", JSEngineQuickJS:
		return JSEngineQuickJS, "", nil
	case JSEngineV8:
		if err := v8shim.Load(); err != nil {
			return JSEngineV8, "", fmt.Errorf("js engine v8 requested but the V8 library cannot be loaded: %w", err)
		}
		return JSEngineV8, "", nil
	case JSEngineAuto:
		if err := v8shim.Load(); err != nil {
			return JSEngineQuickJS, "sweef: js-engine auto: V8 library unavailable (" + firstLine(err.Error()) + "), using QuickJS", nil
		}
		return JSEngineV8, "", nil
	}
	return "", "", fmt.Errorf("unknown js engine %q (want quickjs, v8 or auto)", mode)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// buildVM creates an unbound VM on the engine o.JSEngine names (resolved by New; anything but "v8" is QuickJS).
func buildVM(o Options) (*jsVM, error) {
	if o.JSEngine == JSEngineV8 {
		return buildV8VM(o)
	}
	return buildQJSVM(o)
}

// ReadJSONRetStats reports the host results the V8 runtime passed as JSON text (calls, bytes, marshalling ms).
func ReadJSONRetStats() (calls, bytes int64, ms float64) {
	return jsSt.jsonCalls.Load(), jsSt.jsonBytes.Load(), float64(jsSt.jsonNs.Load()) / 1e6
}

var jsonTop struct {
	mu sync.Mutex
	m  map[string][2]int64
}

// jsonTopNote accounts the size of host results that go the JSON way, per host function (jsstats only).
func jsonTopNote(names []string, idx int, res any) {
	switch res.(type) {
	case map[string]any, []any:
	default:
		return
	}
	size := approxSize(res)
	name := "?"
	if idx >= 0 && idx < len(names) {
		name = names[idx]
	}
	jsonTop.mu.Lock()
	if jsonTop.m == nil {
		jsonTop.m = map[string][2]int64{}
	}
	e := jsonTop.m[name]
	e[0]++
	e[1] += size
	jsonTop.m[name] = e
	jsonTop.mu.Unlock()
}

// ReadJSONTop returns "name=calls/bytes" for the host functions that returned the most JSON.
func ReadJSONTop() string {
	jsonTop.mu.Lock()
	defer jsonTop.mu.Unlock()
	type kv struct {
		n string
		v [2]int64
	}
	var all []kv
	for k, v := range jsonTop.m {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, k int) bool { return all[i].v[1] > all[k].v[1] })
	var sb strings.Builder
	for i, e := range all {
		if i >= 8 {
			break
		}
		fmt.Fprintf(&sb, " %s=%d/%dKB", e.n, e.v[0], e.v[1]/1024)
	}
	return sb.String()
}

// approxSize is the size of the strings (and a few bytes per value) in a host result, without marshalling it.
func approxSize(v any) int64 {
	switch x := v.(type) {
	case string:
		return int64(len(x)) + 2
	case []any:
		n := int64(2)
		for _, e := range x {
			n += approxSize(e) + 1
		}
		return n
	case map[string]any:
		n := int64(2)
		for k, e := range x {
			n += int64(len(k)) + 3 + approxSize(e)
		}
		return n
	}
	return 4
}
