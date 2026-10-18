package engine

// wasm.go — the WebAssembly JavaScript API, Go side.
//
// The API surface (WebAssembly.Module, Instance, Memory, Table, Global, ...) is written in JavaScript
// (wasm_prelude.js, loaded lazily the first time a page touches `WebAssembly`) and reaches this file through a
// small set of host functions. wazero (pure Go; its optimizing compiler where the platform supports it, its
// interpreter elsewhere) validates, compiles and runs the modules. See docs/webassembly.md for the design, the
// limits and measured costs.
//
// Object model
//
//   - One wazero Runtime per JS VM (page or worker). Everything instantiated for the VM lives in it and is
//     closed with it, which is what frees native code and linear memory on navigation or tab close.
//   - Memory, Table and Global objects created from JS are tiny generated modules ("providers") exporting the
//     object. Importing one, or an export of another instance, is wired with a generated "aggregator" module
//     per import module name that re-exports the right providers under the names the guest imports. That keeps
//     the guest's bytes unmodified, so the compiled code is compiled once per WebAssembly.Module.
//   - A memory is identified by a key "module/export". memory.buffer is an ArrayBuffer aliasing wazero's
//     linear memory (wasm_qjs.go); it is detached and replaced when the memory moves or grows.
//   - JS functions imported by a guest become functions of a generated host module that call back into one JS
//     dispatcher; a JS exception crosses the wasm frames as a Go panic and is rethrown unchanged by the caller.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/sweefbrowser/sweef/internal/zpack"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/experimental/table"
	"modernc.org/quickjs"
)

//go:embed wasm_prelude.js.z
var wasmPreludeJSZ string

var wasmPreludeLazy zpack.Lazy

const (
	// wasmMemLimitPages caps every linear memory (also the ones a module declares larger): 8192 pages = 512 MiB.
	wasmMemLimitPages = 8192
	// wasmMaxModuleBytes is the largest module accepted by WebAssembly.compile and friends.
	wasmMaxModuleBytes = 128 << 20
	// wasmCacheRotateBytes: after this many compiled source bytes a new compilation cache generation starts, so
	// the process-wide cache cannot grow without bound.
	wasmCacheRotateBytes = 192 << 20

	wasmErrSentinel = "\x00wasm-error"
)

var wasmLive atomic.Int64 // hosts that are open; tests use it to prove teardown

// wasmForceInterpreter selects wazero's interpreter on platforms that also have the compiler (tests).
var wasmForceInterpreter bool

// ---- process-wide compilation cache ----------------------------------------------------------------------

type wasmCacheGen struct {
	c       wazero.CompilationCache
	refs    int
	bytes   int64
	retired bool
}

var wasmCaches struct {
	mu  sync.Mutex
	cur *wasmCacheGen
}

func acquireWasmCache() *wasmCacheGen {
	wasmCaches.mu.Lock()
	defer wasmCaches.mu.Unlock()
	g := wasmCaches.cur
	if g == nil || g.bytes > wasmCacheRotateBytes {
		if g != nil {
			g.retired = true
			if g.refs == 0 {
				_ = g.c.Close(context.Background())
			}
		}
		g = &wasmCacheGen{c: wazero.NewCompilationCache()}
		wasmCaches.cur = g
	}
	g.refs++
	return g
}

func (g *wasmCacheGen) noteBytes(n int) {
	wasmCaches.mu.Lock()
	g.bytes += int64(n)
	wasmCaches.mu.Unlock()
}

func (g *wasmCacheGen) release() {
	wasmCaches.mu.Lock()
	defer wasmCaches.mu.Unlock()
	g.refs--
	if g.retired && g.refs == 0 {
		_ = g.c.Close(context.Background())
	}
}

// wasmEngineName says which wazero engine NewRuntimeConfig picks on this platform (it mirrors wazero's own
// platform test; it is informational).
func wasmEngineName() string {
	switch runtime.GOOS {
	case "darwin", "linux", "freebsd", "netbsd", "dragonfly", "solaris", "windows":
		if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
			return "compiler"
		}
	}
	return "interpreter"
}

// ---- host ------------------------------------------------------------------------------------------------

type wmodule struct {
	id       int
	bytes    []byte
	info     *wInfo
	compiled wazero.CompiledModule
	shared   bool                  // compiled in the shared (threads) runtime
	other    wazero.CompiledModule // the same bytes compiled for the other runtime, when an instantiation needed it
}

type winst struct {
	tok  int
	name string
	mod  api.Module
	info *wInfo
	host api.Module
	aggs []api.Module
	fns  map[string]api.Function
}

// wref is a Memory, Table or Global addressed by key.
type wref struct {
	key  string
	kind byte
	mod  api.Module
	name string
	mem  api.Memory
	// shared: a shared memory (threads); shRT: the object lives in the shared runtime (wasm_threads.go)
	shared, shRT bool
	// ArrayBuffer aliasing the memory
	hasJS bool
	js    quickjs.Value
	view  []byte
	live  bool
}

// wfn is a callable wasm function JS holds a handle to (an export, or an entry read from a table).
type impRef struct {
	tok, idx int
	sig      string
}

type wfn struct {
	inst *winst
	name string // export name, resolved on first call
	fn   api.Function
	pt   []byte
	rt   []byte
	sig  string
	ref  uintptr // the funcref value, for Table.set

	// An api.Function owns one native stack and must not be re-entered while it runs, so a re-entrant
	// activation (wasm -> JS -> the same function) takes a fresh one from mk, and parks it in spare afterwards.
	mk    func() api.Function
	busy  bool
	spare []api.Function
}

func (f *wfn) acquire() (api.Function, func()) {
	if !f.busy {
		f.busy = true
		return f.fn, func() { f.busy = false }
	}
	var fn api.Function
	if n := len(f.spare); n > 0 {
		fn, f.spare = f.spare[n-1], f.spare[:n-1]
	} else if fn = f.mk(); fn == nil {
		return nil, func() {}
	}
	return fn, func() { f.spare = append(f.spare, fn) }
}

type wasmHost struct {
	vm      *quickjs.VM
	raw     qjsRaw
	timeout func() time.Duration
	rt      wazero.Runtime
	gen     *wasmCacheGen
	ctx     context.Context
	closed  bool
	nextID  int

	mods  map[int]*wmodule
	insts map[int]*winst
	refs  map[string]*wref
	fnTab map[int]*wfn
	fnRef map[uintptr]int
	// funcref values are runtime pointers: JavaScript only ever sees small handles into this table
	refTab []uintptr
	refIdx map[uintptr]uint32
	// funcref values of imported functions (as ref.func / element segments store them) -> the JS import
	impRefs map[uintptr]impRef
	types   map[string]wFuncType

	live []*wref // memories with an ArrayBuffer out

	disp     quickjs.Value
	haveDisp bool

	depth     int        // >0 while wasm is running (nested calls run inline)
	cur       *wcall     // the outermost call in flight
	run       *wrunner   // goroutine the outermost calls run on
	pageNest  int        // >0 while wasm runs nested on the page thread (wasm -> JS -> wasm)
	life      sync.Mutex // closing versus a runaway call that is still executing
	zomb      bool       // a call overran its budget and is still running; the runtime is closed when it ends
	dead      bool       // poisoned: a call overran its budget, nothing may use this host any more
	wantClose bool

	// threads (wasm_threads.go)
	jsv      *jsVM
	aborted  atomic.Bool    // the VM is closing: imports end the call
	inflight atomic.Int32   // outermost calls running on the runner
	shut     bool           // reallyClose ran
	srt      wazero.Runtime // the process-wide runtime for threaded instances, once this host uses it
	threads  bool           // this host runs threaded instances: its calls have no time budget
	smems    map[*wasmSharedMem]bool
	sOwned   []api.Module
	sCompile []wazero.CompiledModule

	errKind byte // set with wasmErrSentinel results: 'R' RuntimeError, 'S' stack overflow, 'T' JS exception, 'C' closed/timeout
	errMsg  string
}

type wasmJSThrow struct {
	msg      string
	internal bool // the dispatcher itself failed (interrupt, closed host): not an exception to rethrow
}

func (e *wasmJSThrow) Error() string {
	return "javascript exception crossed webassembly frames: " + e.msg
}

func newWasmHost(vm *quickjs.VM, timeout func() time.Duration) (*wasmHost, error) {
	if err := wasmQJS(); err != nil {
		return nil, fmt.Errorf("webassembly bridge unavailable: %w", err)
	}
	gen := acquireWasmCache()
	base := wazero.NewRuntimeConfig() // the compiler where the platform has one, the interpreter elsewhere
	if wasmForceInterpreter {
		base = wazero.NewRuntimeConfigInterpreter()
	}
	cfg := base.
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesTailCall | experimental.CoreFeaturesExtendedConst | experimental.CoreFeaturesExceptionHandling).
		WithMemoryLimitPages(wasmMemLimitPages).
		WithCompilationCache(gen.c)
	ctx := context.Background()
	h := &wasmHost{
		vm: vm, raw: rawOf(vm), timeout: timeout, gen: gen, ctx: ctx,
		rt:   wazero.NewRuntimeWithConfig(ctx, cfg),
		mods: map[int]*wmodule{}, insts: map[int]*winst{}, refs: map[string]*wref{},
		fnTab: map[int]*wfn{}, fnRef: map[uintptr]int{}, refIdx: map[uintptr]uint32{}, impRefs: map[uintptr]impRef{}, types: map[string]wFuncType{},
	}
	wasmLive.Add(1)
	return h, nil
}

// close frees every instance, memory and compiled module. The JS VM is already closed (or about to be), so
// nothing here may call into it.
func (h *wasmHost) close() {
	if h == nil {
		return
	}
	h.life.Lock()
	defer h.life.Unlock()
	if h.wantClose {
		return
	}
	h.wantClose = true
	h.closed = true
	h.live = nil
	h.haveDisp = false
	h.aborted.Store(true)
	h.releaseSharedMems() // retires memories nobody else uses, which stops their running threads
	if h.zomb || h.inflight.Load() > 0 {
		return // finishCall completes the close when the runaway call (or a thread) ends
	}
	h.reallyClose()
}

// abort is called, without any lock of the VM, as soon as the VM starts closing (a terminated worker, a page that
// is navigating away). A call that is still inside wasm may be blocked for good, and the VM's own close waits for
// it, so the host drops its shared memories and refuses to call JS right now.
func (h *wasmHost) abort() {
	if h == nil {
		return
	}
	h.aborted.Store(true)
	h.life.Lock()
	h.releaseSharedMems()
	h.life.Unlock()
}

func (h *wasmHost) reallyClose() {
	if h.shut {
		return
	}
	h.shut = true
	if h.run != nil {
		h.run.shutdown()
	}
	_ = h.rt.Close(context.Background())
	h.gen.release()
	h.closeSharedUse()
	h.mods, h.insts, h.refs, h.fnTab, h.fnRef = nil, nil, nil, nil, nil
	wasmLive.Add(-1)
}

func (h *wasmHost) id() int { h.nextID++; return h.nextID }

// tmo is how long one outermost call into wasm may run: six times the page's script budget (30 s by default).
// Compiled wasm cannot be interrupted, so this is a watchdog, not a kill switch; see guard.
func (h *wasmHost) tmo() time.Duration {
	if h.threads {
		return 1000 * time.Hour // a thread's start routine is one call that ends when the thread does
	}
	d := 5 * time.Second
	if h.timeout != nil {
		if t := h.timeout(); t > 0 {
			d = t
		}
	}
	return 6 * d
}

// ---- value conversion ------------------------------------------------------------------------------------

func wasmToI64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case *big.Int:
		if x.IsInt64() {
			return x.Int64()
		}
		return int64(new(big.Int).And(x, new(big.Int).SetUint64(math.MaxUint64)).Uint64())
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

func wasmToF64(v any) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return math.NaN()
}

func wasmEncode(t byte, v any) uint64 {
	switch t {
	case wvI32:
		return api.EncodeI32(int32(uint32(uint64(wasmToI64(v)) & math.MaxUint32))) // wasm i32 wraps modulo 2^32
	case wvI64:
		return uint64(wasmToI64(v))
	case wvF32:
		return api.EncodeF32(float32(wasmToF64(v)))
	case wvF64:
		return api.EncodeF64(wasmToF64(v))
	}
	return 0
}

func wasmDecode(t byte, v uint64) any {
	switch t {
	case wvI32:
		return int(api.DecodeI32(v))
	case wvI64:
		return big.NewInt(int64(v))
	case wvF32:
		return float64(api.DecodeF32(v))
	case wvF64:
		return api.DecodeF64(v)
	}
	return nil
}

func wasmSupported(ts []byte) bool {
	for _, t := range ts {
		if t != wvI32 && t != wvI64 && t != wvF32 && t != wvF64 {
			return false
		}
	}
	return true
}

func sigString(params, results []byte) string {
	var b strings.Builder
	for _, t := range params {
		b.WriteByte(sigChar(t))
	}
	b.WriteByte('>')
	for _, t := range results {
		b.WriteByte(sigChar(t))
	}
	return b.String()
}

func sigChar(t byte) byte {
	switch t {
	case wvI32:
		return 'i'
	case wvI64:
		return 'l'
	case wvF32:
		return 'f'
	case wvF64:
		return 'd'
	}
	return 'x'
}

// ---- errors ----------------------------------------------------------------------------------------------

// trapMessage turns a wazero error into the text V8 would use.
func trapMessage(err error) (kind byte, msg string) {
	var jt *wasmJSThrow
	if errors.As(err, &jt) {
		if jt.internal {
			return 'R', jt.msg
		}
		return 'T', jt.msg
	}
	if errors.Is(err, errWasmTimedOut) {
		return 'C', "WebAssembly call exceeded its time budget and was abandoned"
	}
	s := err.Error()
	if strings.Contains(s, "exit_code(") {
		return 'C', "WebAssembly instance was closed or its execution timed out"
	}
	s = strings.TrimPrefix(s, "wasm error: ")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	switch {
	case strings.Contains(s, "stack overflow"):
		return 'S', "Maximum call stack size exceeded"
	case strings.HasPrefix(s, "unreachable"):
		return 'R', "unreachable"
	case strings.HasPrefix(s, "integer divide by zero"):
		return 'R', "divide by zero"
	case strings.HasPrefix(s, "integer overflow"):
		return 'R', "divide result unrepresentable"
	case strings.HasPrefix(s, "invalid conversion to integer"):
		return 'R', "float unrepresentable in integer range"
	case strings.HasPrefix(s, "out of bounds memory access"):
		return 'R', "memory access out of bounds"
	case strings.HasPrefix(s, "invalid table access"):
		return 'R', "table index is out of bounds or null function"
	case strings.HasPrefix(s, "indirect call type mismatch"):
		return 'R', "null function or function signature mismatch"
	case strings.HasPrefix(s, "null reference"):
		return 'R', "dereferencing a null pointer"
	case strings.HasPrefix(s, "uncaught exception"):
		return 'R', "uncaught exception"
	}
	return 'R', s
}

func (h *wasmHost) fail(err error) any {
	h.errKind, h.errMsg = trapMessage(err)
	return wasmErrSentinel
}

// ---- compile ---------------------------------------------------------------------------------------------

func kindsOf(info *wInfo) (imps, exps []any) {
	for _, im := range info.imports {
		m := map[string]any{"m": im.module, "n": im.name, "k": kindName(im.kind)}
		switch im.kind {
		case wkFunc:
			t := info.types[im.typeIdx]
			m["s"] = sigString(t.params, t.results)
		case wkGlobal:
			m["t"], m["mut"] = string(sigChar(im.desc[0])), im.desc[1] == 1
		}
		imps = append(imps, m)
	}
	for _, ex := range info.exports {
		exps = append(exps, map[string]any{"n": ex.name, "k": kindName(ex.kind)})
	}
	return
}

// nCompile(buf, off, len): compiles a module. Returns {id, imports, exports} or {e: message}.
func (h *wasmHost) nCompile(buf quickjs.Value, off, n int) any {
	b, ok := h.raw.bufferBytes(&buf)
	if !ok || off < 0 || n < 0 || off+n > len(b) {
		return map[string]any{"e": "BufferSource argument is empty"}
	}
	return h.compileSource(append([]byte(nil), b[off:off+n]...))
}

// compileSource compiles a module for this host: {id, imports, exports} or {e: message}.
func (h *wasmHost) compileSource(src []byte) any {
	if len(src) > wasmMaxModuleBytes {
		return map[string]any{"e": fmt.Sprintf("module is larger than the %d MiB limit", wasmMaxModuleBytes>>20)}
	}
	info, err := parseWasm(src)
	if err != nil {
		return map[string]any{"e": err.Error()}
	}
	if info.hasMemory64 {
		return map[string]any{"e": "memory64 is not supported"}
	}
	cm, shared, err := h.compileModule(src, info)
	if err != nil {
		return map[string]any{"e": err.Error()}
	}
	if shared {
		h.sCompile = append(h.sCompile, cm)
	}
	h.gen.noteBytes(len(src))
	id := h.id()
	h.mods[id] = &wmodule{id: id, bytes: src, info: info, compiled: cm, shared: shared}
	for _, t := range info.types {
		h.types[t.sig()] = t
	}
	imps, exps := kindsOf(info)
	return map[string]any{"id": id, "imports": imps, "exports": exps}
}

// nValidate(buf, off, len): WebAssembly.validate. The compiled code goes to the shared cache, so a following
// compile of the same bytes is cheap.
func (h *wasmHost) nValidate(buf quickjs.Value, off, n int) any {
	b, ok := h.raw.bufferBytes(&buf)
	if !ok || off < 0 || n < 0 || off+n > len(b) || n > wasmMaxModuleBytes {
		return false
	}
	src := b[off : off+n]
	info, err := parseWasm(src)
	if err != nil || info.hasMemory64 {
		return false
	}
	cm, _, err := h.compileModule(src, info)
	if err != nil {
		return false
	}
	h.gen.noteBytes(len(src))
	_ = cm.Close(h.ctx)
	return true
}

// customSections(id, name) -> count
func (h *wasmHost) customCount(a []any) (any, error) {
	m := h.mods[int(argI(a, 0))]
	if m == nil {
		return 0, nil
	}
	n := 0
	for _, c := range m.info.customs {
		if c.name == argS(a, 1) {
			n++
		}
	}
	return n, nil
}

func (h *wasmHost) nCustom(id int, name string, idx int) quickjs.Value {
	m := h.mods[id]
	if m != nil {
		i := 0
		for _, c := range m.info.customs {
			if c.name == name {
				if i == idx {
					return h.raw.newCopyBuffer(h.vm, c.data)
				}
				i++
			}
		}
	}
	return h.raw.newCopyBuffer(h.vm, nil)
}

// ---- instantiate -----------------------------------------------------------------------------------------

type impDesc struct {
	K   string `json:"k"`
	Key string `json:"key"`
}

func (h *wasmHost) errObj(kind, msg string) any { return map[string]any{"e": kind, "m": msg} }

// instantiate(modID, tok, descJSON) -> {exports:[...]} | {e:'L'|'R'|'T', m}
func (h *wasmHost) instantiate(a []any) (any, error) {
	wm := h.mods[int(argI(a, 0))]
	tok := int(argI(a, 1))
	if wm == nil {
		return h.errObj("L", "unknown module"), nil
	}
	var descs []impDesc
	if err := json.Unmarshal([]byte(argS(a, 2)), &descs); err != nil || len(descs) != len(wm.info.imports) {
		return h.errObj("L", "import descriptor mismatch"), nil
	}
	info := wm.info
	// Instances that use threads live in the shared runtime (wasm_threads.go): a module that needs it, defines a
	// shared memory, or imports something that lives there.
	useShared := wm.shared || info.definesShared
	for i, im := range info.imports {
		if im.kind != wkFunc && im.kind != wkTag {
			if r := h.refs[descs[i].Key]; r != nil && r.shRT {
				useShared = true
			}
		}
	}
	rt := h.rt
	seq := h.id()
	if useShared {
		rt = h.sharedRuntime()
		seq = int(wasmGlobalSeq.Add(1)) // names are unique across every VM that uses the shared runtime
	}
	compiled, cerr := h.compiledFor(wm, useShared)
	if cerr != nil {
		return h.errObj("L", cerr.Error()), nil
	}
	inst := &winst{tok: tok, name: fmt.Sprintf("swi%d", seq), info: info, fns: map[string]api.Function{}}
	cleanup := func() {
		for _, m := range inst.aggs {
			_ = m.Close(h.ctx)
		}
		if inst.host != nil {
			_ = inst.host.Close(h.ctx)
		}
		if inst.mod != nil {
			_ = inst.mod.Close(h.ctx)
		}
	}

	// host module for the JS functions the guest imports
	hostName := fmt.Sprintf("swh%d", seq)
	hb := rt.NewHostModuleBuilder(hostName)
	nHost := 0
	for i, im := range info.imports {
		if im.kind != wkFunc {
			continue
		}
		ft := info.types[im.typeIdx]
		fn := h.hostFunc(tok, i, ft)
		hb.NewFunctionBuilder().WithGoModuleFunction(fn, valTypes(ft.params), valTypes(ft.results)).Export("f" + strconv.Itoa(i))
		nHost++
	}
	if nHost > 0 {
		hm, err := hb.Instantiate(h.ctx)
		if err != nil {
			return h.errObj("L", err.Error()), nil
		}
		inst.host = hm
	}

	// aggregators, one per import module name
	type group struct {
		name  string
		items []aggImport
	}
	var groups []*group
	byName := map[string]*group{}
	impKeys := map[byte][]string{}
	for i, im := range info.imports {
		g := byName[im.module]
		if g == nil {
			g = &group{name: im.module}
			byName[im.module] = g
			groups = append(groups, g)
		}
		it := aggImport{as: im.name, kind: im.kind, desc: im.desc}
		switch im.kind {
		case wkFunc:
			it.from, it.field, it.ftype = hostName, "f"+strconv.Itoa(i), info.types[im.typeIdx].raw
		case wkTag:
			cleanup()
			return h.errObj("L", "tag imports are not supported"), nil
		default:
			r := h.refs[descs[i].Key]
			if r == nil || r.kind != im.kind {
				cleanup()
				return h.errObj("L", fmt.Sprintf("import %d (%s %q): not a compatible %s object", i, im.module, im.name, kindName(im.kind))), nil
			}
			if r.shRT != useShared {
				cleanup()
				return h.errObj("L", fmt.Sprintf("import %d (%s %q): a %s created outside the threads runtime cannot be shared with a threaded instance; create it after the shared WebAssembly.Memory", i, im.module, im.name, kindName(im.kind))), nil
			}
			it.from, it.field = r.mod.Name(), r.name
			impKeys[im.kind] = append(impKeys[im.kind], r.key)
		}
		g.items = append(g.items, it)
	}
	aggMods := map[string]api.Module{}
	for _, g := range groups {
		bin := asmAggregator(g.items)
		cm, err := rt.CompileModule(h.ctx, bin)
		if err != nil {
			cleanup()
			return h.errObj("L", "internal aggregator: "+err.Error()), nil
		}
		am, err := rt.InstantiateModule(h.ctx, cm, wazero.NewModuleConfig().WithName(fmt.Sprintf("swa%d_%d", seq, len(inst.aggs))).WithStartFunctions())
		_ = cm.Close(h.ctx)
		if err != nil {
			cleanup()
			return h.errObj("L", linkMessage(g.name, err)), nil
		}
		inst.aggs = append(inst.aggs, am)
		aggMods[g.name] = am
	}

	ictx := experimental.WithImportResolver(h.ctx, func(name string) api.Module { return aggMods[name] })
	var mod api.Module
	err := h.guard(func() (e error) {
		mod, e = rt.InstantiateModule(ictx, compiled, wazero.NewModuleConfig().WithName(inst.name).WithStartFunctions())
		return
	})
	h.syncMems()
	if err != nil {
		cleanup()
		kind, msg := trapMessage(err)
		if kind == 'R' && !strings.Contains(err.Error(), "wasm error:") {
			return h.errObj("L", linkMessage("", err)), nil
		}
		return h.errObj(string(kind), msg), nil
	}
	inst.mod = mod
	if useShared {
		for _, m := range inst.aggs {
			h.ownShared(m)
		}
		h.ownShared(inst.host)
		h.ownShared(mod)
	}
	h.insts[tok] = inst
	for i, im := range info.imports {
		if im.kind != wkFunc {
			continue
		}
		t := info.types[im.typeIdx]
		if ref := engineRef(mod, uint32(info.funcIndexOfImport(i))); ref != 0 {
			h.impRefs[ref] = impRef{tok: tok, idx: i, sig: sigString(t.params, t.results)}
		}
	}

	// exports
	canon := map[[2]int]string{}
	var exps []any
	for _, ex := range info.exports {
		e := map[string]any{"n": ex.name}
		switch ex.kind {
		case wkFunc:
			t := info.types[info.funcType[ex.idx]]
			fid := h.id()
			exName := ex.name
			h.fnTab[fid] = &wfn{inst: inst, name: exName, pt: t.params, rt: t.results, sig: sigString(t.params, t.results),
				mk: func() api.Function { return mod.ExportedFunction(exName) }}
			e["k"], e["i"], e["s"], e["f"] = "f", ex.idx, sigString(t.params, t.results), fid
		case wkMemory, wkTable, wkGlobal:
			key := ""
			nImp := map[byte]int{wkMemory: info.nImpMemory, wkTable: info.nImpTable, wkGlobal: info.nImpGlobal}[ex.kind]
			if ex.idx < nImp {
				key = impKeys[ex.kind][ex.idx]
			} else if k, ok := canon[[2]int{int(ex.kind), ex.idx}]; ok {
				key = k
			} else {
				key = inst.name + "/" + ex.name
				canon[[2]int{int(ex.kind), ex.idx}] = key
				wr := &wref{key: key, kind: ex.kind, mod: mod, name: ex.name, shRT: useShared}
				if ex.kind == wkMemory && info.definesShared {
					wr.shared = true
					if sm := sharedMemOf(wr.memory()); sm != nil {
						h.holdShared(sm)
					}
				}
				h.refs[key] = wr
			}
			e["key"] = key
			switch ex.kind {
			case wkMemory:
				e["k"] = "m"
			case wkTable:
				e["k"] = "t"
			case wkGlobal:
				e["k"] = "g"
				if g := mod.ExportedGlobal(ex.name); g != nil {
					_, mut := g.(api.MutableGlobal)
					e["t"], e["m"] = string(sigChar(g.Type())), mut
				}
			}
		default:
			continue // tags
		}
		exps = append(exps, e)
	}
	return map[string]any{"exports": exps}, nil
}

func linkMessage(mod string, err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func valTypes(ts []byte) []api.ValueType {
	out := make([]api.ValueType, len(ts))
	for i, t := range ts {
		out[i] = api.ValueType(t)
	}
	return out
}

func parseResult(t byte, s string) uint64 {
	switch t {
	case wvI32, wvI64:
		v, _ := strconv.ParseInt(strings.TrimSuffix(s, "n"), 10, 64)
		return wasmEncode(t, v)
	}
	f, _ := strconv.ParseFloat(s, 64)
	return wasmEncode(t, f)
}

// ---- calling ---------------------------------------------------------------------------------------------

func (h *wasmHost) invoke(f *wfn, args []any) any {
	var buf [8]uint64
	stack := buf[:]
	if n := max(len(f.pt), len(f.rt)); n > len(buf) {
		stack = make([]uint64, n)
	}
	for i, t := range f.pt {
		var v any
		if i < len(args) {
			v = args[i]
		}
		stack[i] = wasmEncode(t, v)
	}
	fn, done := f.acquire()
	if fn == nil {
		done()
		return h.fail(errors.New("function can no longer be re-entered"))
	}
	err := h.guard(func() error { return fn.CallWithStack(h.ctx, stack) })
	done()
	if err != errWasmTimedOut {
		h.syncMems()
	}
	if err != nil {
		return h.fail(err)
	}
	switch len(f.rt) {
	case 0:
		return nil
	case 1:
		return wasmDecode(f.rt[0], stack[0])
	}
	out := make([]any, len(f.rt))
	for i, t := range f.rt {
		out[i] = wasmDecode(t, stack[i])
	}
	return out
}

// call(fid, ...args)
func (h *wasmHost) call(a []any) (any, error) {
	if h.closed || len(a) < 1 {
		return nil, errors.New("webassembly host is closed")
	}
	f := h.fnTab[int(argI(a, 0))]
	if f == nil {
		return nil, errors.New("unknown function reference")
	}
	if f.fn == nil {
		f.fn = f.mk()
		if f.fn == nil {
			return nil, fmt.Errorf("no exported function %q", f.name)
		}
	}
	return h.invoke(f, a[1:]), nil
}

// errInfo() -> kind + ":" + message of the failure just reported with the sentinel.
func (h *wasmHost) errInfo(a []any) (any, error) {
	return string(h.errKind) + ":" + h.errMsg, nil
}

func (h *wasmHost) nSetDispatch(fn quickjs.Value) any {
	h.disp = fn.Dup()
	h.haveDisp = true
	return nil
}

// ---- memory ----------------------------------------------------------------------------------------------

func (r *wref) memory() api.Memory {
	if r.mem == nil {
		r.mem = r.mod.ExportedMemory(r.name)
	}
	return r.mem
}

func (h *wasmHost) dropBuf(r *wref) {
	if !r.hasJS {
		return
	}
	if !h.closed {
		if !r.shared { // a SharedArrayBuffer is never detached: the old one keeps its length when the memory grows
			h.raw.detach(&r.js)
		}
		r.js.Free()
	}
	r.hasJS, r.view = false, nil
	for i, l := range h.live {
		if l == r {
			h.live = append(h.live[:i], h.live[i+1:]...)
			break
		}
	}
}

// syncMems detaches the ArrayBuffer of every memory that grew or moved since JS last saw it. It runs on every
// edge back into JavaScript (return from a wasm call, call from wasm into a JS import).
func (h *wasmHost) syncMems() {
	for i := 0; i < len(h.live); i++ {
		r := h.live[i]
		size := r.memory().Size()
		view, _ := r.mem.Read(0, size)
		if uint32(len(r.view)) != size || unsafe.SliceData(view) != unsafe.SliceData(r.view) {
			h.dropBuf(r)
			i--
		}
	}
}

// nMemBuf(key): the ArrayBuffer for a memory (undefined for an empty one).
func (h *wasmHost) nMemBuf(key string) quickjs.Value {
	r := h.refs[key]
	if r == nil || r.kind != wkMemory {
		return quickjs.UndefinedValue
	}
	mem := r.memory()
	size := mem.Size()
	if size == 0 {
		h.dropBuf(r)
		return quickjs.UndefinedValue
	}
	view, _ := mem.Read(0, size)
	if r.hasJS && unsafe.SliceData(view) == unsafe.SliceData(r.view) && len(view) == len(r.view) {
		return r.js.Dup()
	}
	h.dropBuf(r)
	r.view = view
	if r.shared {
		sm := sharedMemOf(mem)
		if sm == nil || h.cluster() == nil {
			return quickjs.UndefinedValue
		}
		if _, ok := h.cluster().adopt(sm.full, sm); !ok {
			return quickjs.UndefinedValue
		}
		r.js = h.raw.newSharedBuffer(h.vm, view, unsafe.Pointer(unsafe.SliceData(sm.full)))
	} else {
		r.js = h.raw.newExternalBuffer(h.vm, view)
	}
	r.hasJS = true
	h.live = append(h.live, r)
	return r.js.Dup()
}

// provider instantiates a generated module holding one Memory/Table/Global.
// shared selects the shared (threads) runtime; Tables and Globals follow once this host runs threads, so that a
// threaded instance can import them.
func (h *wasmHost) provider(bin []byte, export string, kind byte, shared bool) (string, error) {
	rt := h.rt
	name := fmt.Sprintf("swp%d", h.id())
	if shared {
		rt = h.sharedRuntime()
		name = fmt.Sprintf("swp%d", wasmGlobalSeq.Add(1))
	}
	cm, err := rt.CompileModule(h.ctx, bin)
	if err != nil {
		return "", err
	}
	m, err := rt.InstantiateModule(h.ctx, cm, wazero.NewModuleConfig().WithName(name).WithStartFunctions())
	_ = cm.Close(h.ctx)
	if err != nil {
		return "", err
	}
	if shared {
		h.ownShared(m)
	}
	key := name + "/" + export
	h.refs[key] = &wref{key: key, kind: kind, mod: m, name: export, shRT: shared}
	return key, nil
}

func optMax(a []any, i int) int64 {
	if i >= len(a) {
		return -1
	}
	switch v := a[i].(type) {
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return -1
}

// memNew(initial, max|-1, shared) -> key | {e}
func (h *wasmHost) memNew(a []any) (any, error) {
	min, max := argI(a, 0), optMax(a, 1)
	if argB(a, 2) {
		if max < 0 {
			return h.errObj("O", "shared memory requires a maximum"), nil
		}
		if min > wasmSharedMemLimitPages {
			return h.errObj("O", fmt.Sprintf("WebAssembly.Memory(): Property 'initial': value %d is above the upper bound %d", min, wasmSharedMemLimitPages)), nil
		}
		if max > wasmSharedMemLimitPages {
			max = wasmSharedMemLimitPages
		}
		key, err := h.provider(asmSharedMemory(uint32(min), uint32(max)), "m", wkMemory, true)
		if err != nil {
			return h.errObj("O", err.Error()), nil
		}
		r := h.refs[key]
		r.shared = true
		if sm := sharedMemOf(r.memory()); sm != nil {
			h.holdShared(sm)
		} else {
			return h.errObj("O", "shared memory is not available (unexpected wazero internals)"), nil
		}
		return key, nil
	}
	if min > wasmMemLimitPages {
		return h.errObj("O", fmt.Sprintf("WebAssembly.Memory(): Property 'initial': value %d is above the upper bound %d", min, wasmMemLimitPages)), nil
	}
	if max > wasmMemLimitPages {
		max = wasmMemLimitPages
	}
	key, err := h.provider(asmMemory(uint32(min), max), "m", wkMemory, false)
	if err != nil {
		return h.errObj("O", err.Error()), nil
	}
	return key, nil
}

// memGrow(key, delta) -> previous size in pages | -1
func (h *wasmHost) memGrow(a []any) (any, error) {
	r := h.refs[argS(a, 0)]
	if r == nil {
		return -1, nil
	}
	prev, ok := r.memory().Grow(uint32(argI(a, 1)))
	h.dropBuf(r) // memory.grow detaches the old ArrayBuffer even for a delta of 0
	h.syncMems()
	if !ok {
		return -1, nil
	}
	return int(prev), nil
}

func (h *wasmHost) memPages(a []any) (any, error) {
	r := h.refs[argS(a, 0)]
	if r == nil {
		return 0, nil
	}
	return int(r.memory().Size() / 65536), nil
}

// ---- table -----------------------------------------------------------------------------------------------

// tableOf digs the wazero table instance out of a module for the exported table `name` (wazero's public API
// has no table access). The fields and methods used are exported by wazero's internal wasm package.
func tableOf(mod api.Module, name string) (tv reflect.Value, idx int, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	mv := reflect.ValueOf(mod)
	if mv.Kind() != reflect.Pointer {
		return
	}
	mv = mv.Elem()
	e := mv.FieldByName("Exports").MapIndex(reflect.ValueOf(name))
	if !e.IsValid() {
		return
	}
	idx = int(e.Elem().FieldByName("Index").Uint())
	tv = mv.FieldByName("Tables").Index(idx).Elem()
	return tv, idx, tv.IsValid()
}

func (h *wasmHost) tabRef(key string) (tv reflect.Value, idx int, r *wref, ok bool) {
	r = h.refs[key]
	if r == nil || r.kind != wkTable {
		return
	}
	tv, idx, ok = tableOf(r.mod, r.name)
	return
}

// tabNew(initial, max|-1) -> key | {e}
func (h *wasmHost) tabNew(a []any) (any, error) {
	key, err := h.provider(asmTable(uint32(argI(a, 0)), optMax(a, 1)), "t", wkTable, h.threads)
	if err != nil {
		return h.errObj("O", err.Error()), nil
	}
	return key, nil
}

func (h *wasmHost) tabLen(a []any) (any, error) {
	tv, _, _, ok := h.tabRef(argS(a, 0))
	if !ok {
		return 0, nil
	}
	return tv.FieldByName("References").Len(), nil
}

// refHandle returns the small handle JavaScript uses for a funcref value (1-based, stable per host).
func (h *wasmHost) refHandle(ref uintptr) uint32 {
	if n, ok := h.refIdx[ref]; ok {
		return n
	}
	h.refTab = append(h.refTab, ref)
	n := uint32(len(h.refTab))
	h.refIdx[ref] = n
	return n
}

func (h *wasmHost) funcRefValue(a []any, i int) (uintptr, error) {
	// a[i] = "tok:funcIdx" instance function or "ref:<n>" value obtained from Table.get; "" = null
	s := argS(a, i)
	switch {
	case s == "":
		return 0, nil
	case strings.HasPrefix(s, "ref:"):
		n, err := strconv.ParseUint(s[4:], 10, 32)
		if err != nil || n == 0 || n > uint64(len(h.refTab)) {
			return 0, errors.New("bad function reference")
		}
		return h.refTab[n-1], nil
	}
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, errors.New("bad function reference")
	}
	tok, _ := strconv.Atoi(parts[0])
	fi64, _ := strconv.ParseUint(parts[1], 10, 32)
	fi := uint32(fi64)
	inst := h.insts[tok]
	if inst == nil {
		return 0, errors.New("function belongs to a released instance")
	}
	ref := engineRef(inst.mod, fi)
	if ref == 0 {
		return 0, errors.New("cannot take a reference to this function")
	}
	return ref, nil
}

// engineRef is the funcref value wazero stores in tables for function fi of mod (0 if it cannot be taken).
func engineRef(mod api.Module, fi uint32) (ref uintptr) {
	defer func() {
		if recover() != nil {
			ref = 0
		}
	}()
	mv := reflect.ValueOf(mod).Elem()
	out := mv.FieldByName("Engine").MethodByName("FunctionInstanceReference").Call([]reflect.Value{reflect.ValueOf(fi)})
	return uintptr(out[0].Uint())
}

// tabGrow(key, delta, initRef) -> previous length | -1
func (h *wasmHost) tabGrow(a []any) (any, error) {
	tv, _, _, ok := h.tabRef(argS(a, 0))
	if !ok {
		return -1, nil
	}
	init, err := h.funcRefValue(a, 2)
	if err != nil {
		return nil, err
	}
	out := tv.Addr().MethodByName("Grow").Call([]reflect.Value{reflect.ValueOf(uint32(argI(a, 1))), reflect.ValueOf(init)})
	prev := uint32(out[0].Uint())
	if prev == 0xffffffff {
		return -1, nil
	}
	return int(prev), nil
}

// tabSet(key, i, ref)
func (h *wasmHost) tabSet(a []any) (any, error) {
	tv, _, _, ok := h.tabRef(argS(a, 0))
	if !ok {
		return nil, errors.New("not a table")
	}
	refs := tv.FieldByName("References")
	i := int(argI(a, 1))
	if i < 0 || i >= refs.Len() {
		return nil, errors.New("RangeError:index out of bounds")
	}
	v, err := h.funcRefValue(a, 2)
	if err != nil {
		return nil, err
	}
	refs.Index(i).SetUint(uint64(v))
	return nil, nil
}

// tabGet(key, i) -> null | {id, s, r}
func (h *wasmHost) tabGet(a []any) (any, error) {
	tv, tidx, r, ok := h.tabRef(argS(a, 0))
	if !ok {
		return nil, errors.New("not a table")
	}
	refs := tv.FieldByName("References")
	i := int(argI(a, 1))
	if i < 0 || i >= refs.Len() {
		return nil, errors.New("RangeError:index out of bounds")
	}
	ref := uintptr(refs.Index(i).Uint())
	if ref == 0 {
		return nil, nil
	}
	if ir, ok := h.impRefs[ref]; ok { // a JS function placed in the table by an element segment
		return map[string]any{"imp": ir.tok, "ii": ir.idx, "s": ir.sig, "r": strconv.FormatUint(uint64(h.refHandle(ref)), 10)}, nil
	}
	if id, ok := h.fnRef[ref]; ok {
		f := h.fnTab[id]
		return map[string]any{"id": id, "s": f.sig, "r": strconv.FormatUint(uint64(h.refHandle(ref)), 10)}, nil
	}
	for _, t := range h.types {
		fn := lookupTableFunc(r.mod, uint32(tidx), uint32(i), valTypes(t.params), valTypes(t.results))
		if fn == nil {
			continue
		}
		id := h.id()
		f := &wfn{fn: fn, pt: t.params, rt: t.results, sig: sigString(t.params, t.results), ref: ref}
		tm, tp, tr, toff := r.mod, valTypes(t.params), valTypes(t.results), uint32(i)
		f.mk = func() api.Function {
			if uintptr(refs.Index(int(toff)).Uint()) != ref {
				return nil // the table entry was replaced since this function was read from it
			}
			return lookupTableFunc(tm, uint32(tidx), toff, tp, tr)
		}
		h.fnTab[id] = f
		h.fnRef[ref] = id
		return map[string]any{"id": id, "s": f.sig, "r": strconv.FormatUint(uint64(h.refHandle(ref)), 10)}, nil
	}
	return nil, errors.New("TypeError:table entry has a function type this runtime cannot name")
}

func lookupTableFunc(mod api.Module, tidx, off uint32, params, results []api.ValueType) (fn api.Function) {
	defer func() {
		if recover() != nil {
			fn = nil
		}
	}()
	return table.LookupFunction(mod, tidx, off, params, results)
}

// ---- global ----------------------------------------------------------------------------------------------

// globNew(type, mutable, value) -> key | {e}
func (h *wasmHost) globNew(a []any) (any, error) {
	t := byte(0)
	switch argS(a, 0) {
	case "i":
		t = wvI32
	case "l":
		t = wvI64
	case "f":
		t = wvF32
	case "d":
		t = wvF64
	default:
		return h.errObj("O", "unsupported global type"), nil
	}
	key, err := h.provider(asmGlobal(t, argB(a, 1), wasmEncode(t, a[2])), "g", wkGlobal, h.threads)
	if err != nil {
		return h.errObj("O", err.Error()), nil
	}
	return key, nil
}

func (h *wasmHost) globGet(a []any) (any, error) {
	r := h.refs[argS(a, 0)]
	if r == nil {
		return nil, errors.New("unknown global")
	}
	g := r.mod.ExportedGlobal(r.name)
	if g == nil {
		return nil, errors.New("unknown global")
	}
	return wasmDecode(byte(g.Type()), g.Get()), nil
}

func (h *wasmHost) globSet(a []any) (any, error) {
	r := h.refs[argS(a, 0)]
	if r == nil {
		return nil, errors.New("unknown global")
	}
	g, ok := r.mod.ExportedGlobal(r.name).(api.MutableGlobal)
	if !ok {
		return nil, errors.New("TypeError:Can't set the value of an immutable global.")
	}
	g.Set(wasmEncode(byte(g.Type()), a[1]))
	return nil, nil
}

// ---- wiring ----------------------------------------------------------------------------------------------

// install registers the wasm host functions as globals named __swWasm_<name> in vm and returns the JS API
// source that collects them. Called from the lazy `WebAssembly` getter, so VMs that never touch WebAssembly pay
// one registered function and no prelude parse.
func (h *wasmHost) install() (string, error) {
	vm := h.vm
	reg := func(name string, fn func(a []any) (any, error)) error {
		return vm.RegisterHostFunc("__swWasm_"+name, func(a []any) (any, error) {
			if h.closed {
				return nil, errors.New("webassembly host is closed")
			}
			if h.dead {
				if name == "errInfo" {
					return fn(a)
				}
				if name == "call" {
					return h.fail(errWasmTimedOut), nil
				}
				return nil, errors.New("webassembly instance was stopped: a call exceeded its time budget")
			}
			return fn(a)
		})
	}
	for name, fn := range map[string]func(a []any) (any, error){
		"instantiate": h.instantiate, "call": h.call, "errInfo": h.errInfo,
		"customCount": h.customCount,
		"memNew":      h.memNew, "memGrow": h.memGrow, "memPages": h.memPages,
		"tabNew": h.tabNew, "tabLen": h.tabLen, "tabGrow": h.tabGrow, "tabSet": h.tabSet, "tabGet": h.tabGet,
		"globNew": h.globNew, "globGet": h.globGet, "globSet": h.globSet,
		"memShare": h.memShare, "memOpen": h.memOpen, "modShare": h.modShare, "modOpen": h.modOpen,
	} {
		if err := reg(name, fn); err != nil {
			return "", err
		}
	}
	natives := map[string]any{
		"compile":     h.nCompile,
		"validate":    h.nValidate,
		"memBuf":      h.nMemBuf,
		"custom":      h.nCustom,
		"setDispatch": h.nSetDispatch,
	}
	for name, fn := range natives {
		if err := vm.RegisterFunc("__swWasm_"+name, fn, false); err != nil {
			return "", err
		}
	}
	return wasmPreludeLazy.Get(wasmPreludeJSZ), nil
}

// wasmPageStub is evaluated with every page prelude: a `WebAssembly` accessor that loads the real API on first
// use. `typeof WebAssembly` is therefore truthful and costs nothing for pages that never look.
const wasmPageStub = `(function (g) {
  var sw = g.__sw;
  if (!sw || !sw.wasmInit) return;
  var busy = false;
  function load() {
    if (busy) return undefined;
    busy = true;
    try {
      delete g.WebAssembly;
      var src = sw.wasmInit();
      if (src) (0, eval)(src);
    } finally { busy = false; }
    return g.WebAssembly;
  }
  Object.defineProperty(g, 'WebAssembly', {
    configurable: true, enumerable: false, get: load,
    set: function (v) { Object.defineProperty(g, 'WebAssembly', { value: v, writable: true, configurable: true, enumerable: false }); }
  });
})(globalThis);`

func init() {
	registerPrelude("wasm", wasmPageStub)
	registerHostExtension("wasm", func(j *jsVM) map[string]hostFn {
		installed := false
		return map[string]hostFn{"wasmInit": func(a []any) (any, error) {
			if installed { // the lazy getter calls this once; anything else is page script poking at __sw
				return "", nil
			}
			installed = true
			h, err := wasmHostFor(j, func() time.Duration {
				if j.tab != nil && j.tab.eng != nil && j.tab.eng.opts.JSTimeoutMs > 0 {
					return time.Duration(j.tab.eng.opts.JSTimeoutMs) * time.Millisecond
				}
				return 0
			}, &j.closeHooks)
			if err != nil {
				return "", nil // the stub then leaves WebAssembly undefined
			}
			return h.install()
		}}
	})
}

// wasmHostFor creates the host for one VM and arranges for it to be closed with the VM.
func wasmHostFor(j *jsVM, timeout func() time.Duration, hooks *[]func()) (*wasmHost, error) {
	if j.vm == nil {
		return nil, errV8Unsupported("the WebAssembly host") // V8 has its own WebAssembly
	}
	h, err := newWasmHost(j.vm, timeout)
	if err != nil {
		return nil, err
	}
	h.jsv = j
	j.clMu.Lock()
	j.wasmh = h
	j.clMu.Unlock()
	*hooks = append(*hooks, h.close)
	return h, nil
}

// wasmAbort tells the VM's WebAssembly host (if it has one) that the VM is closing (see wasmHost.abort).
func (j *jsVM) wasmAbort() {
	j.clMu.Lock()
	h := j.wasmh
	j.clMu.Unlock()
	h.abort()
}
