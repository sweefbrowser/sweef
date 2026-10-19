package v8shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"unsafe"
)

// Kind is the type of a Value.
type Kind uint8

// Value kinds. JSON holds the JSON text in Str (objects/arrays).
const (
	Undefined Kind = tUndefined
	Null      Kind = tNull
	Bool      Kind = tBool
	Int       Kind = tInt
	Double    Kind = tDouble
	String    Kind = tString
	JSON      Kind = tJSON
	Bytes     Kind = tBytes
)

// Value is a typed result copied out of V8.
type Value struct {
	Kind  Kind
	Int   int64
	Float float64
	Str   string
	Bytes []byte
}

// String renders the value like JS String(v) for primitives.
func (v Value) String() string {
	switch v.Kind {
	case Undefined:
		return "undefined"
	case Null:
		return "null"
	case Bool:
		if v.Int != 0 {
			return "true"
		}
		return "false"
	case Int:
		return fmt.Sprint(v.Int)
	case Double:
		if v.Float == math.Trunc(v.Float) && math.Abs(v.Float) < 1e21 {
			return strconv.FormatFloat(v.Float, 'f', -1, 64)
		}
		return strconv.FormatFloat(v.Float, 'g', -1, 64)
	case Bytes:
		return fmt.Sprintf("bytes(%d)", len(v.Bytes))
	}
	return v.Str
}

// Exception is a JS error (or termination / OOM) reported by V8.
type Exception struct {
	Message    string
	Stack      string
	Location   string
	Terminated bool
	OOM        bool
}

func (e *Exception) Error() string {
	if e.Stack != "" {
		return e.Stack
	}
	return e.Message
}

// JSUndefined is the argument type that passes JS undefined to Call (nil passes null).
type JSUndefined struct{}

// HostFunc is a Go function callable from JS. It reads arguments from c and
// sets a result with one of the Ret* methods (default undefined).
type HostFunc func(c *Call)

// ModuleLoader resolves a module specifier relative to the importing module's
// name. It returns the canonical module name (cache key; also import.meta.url)
// and its source. Returning an error makes the import fail with that message.
type ModuleLoader func(specifier, referrer string) (name, source string, err error)

type modInfo struct {
	name string
	id   uint32
}

// Isolate is one V8 isolate (heap + microtask queue). Not goroutine-safe.
type Isolate struct {
	p    uintptr
	id   uint64
	out  rawVal
	fns  []HostFunc
	call []*Call
	dep  int

	// Loader resolves static imports and dynamic import().
	Loader ModuleLoader
	// ImportMeta, if set, returns import.meta.url for a module name.
	ImportMeta func(moduleName string) string

	mods   map[uint32]*modInfo
	byName map[string]uint32
	ctx0   uint32
	keepB  []byte
}

var registry atomic.Pointer[[]*Isolate]

func regPut(i *Isolate) {
	for {
		old := registry.Load()
		var n []*Isolate
		if old != nil {
			n = append(n, *old...)
		}
		for uint64(len(n)) <= i.id {
			n = append(n, nil)
		}
		n[i.id] = i
		if registry.CompareAndSwap(old, &n) {
			return
		}
	}
}

func regDel(id uint64) {
	for {
		old := registry.Load()
		if old == nil || uint64(len(*old)) <= id {
			return
		}
		n := append([]*Isolate(nil), *old...)
		n[id] = nil
		if registry.CompareAndSwap(old, &n) {
			return
		}
	}
}

func regGet(id uint64) *Isolate {
	r := registry.Load()
	if r == nil || uint64(len(*r)) <= id {
		return nil
	}
	return (*r)[id]
}

// Options configures NewIsolate.
//
// All isolates alive in a process at the same time must be created from the same startup snapshot, or all
// without one: V8 shares the read-only heap between isolates, and an isolate on a custom snapshot next to a plain
// one (or one on another snapshot) aborts the process while deserializing ("vector[] index out of bounds" in
// ReadReadOnlyHeapRef). Isolates that are created and closed one after the other may use either.
type Options struct {
	HeapLimit uint64 // max old-space bytes, 0 = V8 default
	Snapshot  []byte // startup snapshot from CreateSnapshot
}

// NewIsolate creates an isolate. Call Close when done.
func NewIsolate(o Options) (*Isolate, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	var sp unsafe.Pointer
	if len(o.Snapshot) > 0 {
		sp = unsafe.Pointer(&o.Snapshot[0])
	}
	p := fIsolateNew(uintptr(o.HeapLimit), sp, uintptr(len(o.Snapshot)))
	runtime.KeepAlive(o.Snapshot)
	if p == 0 {
		return nil, errors.New("v8shim: isolate creation failed")
	}
	i := &Isolate{p: p, id: fIsolateID(p), mods: map[uint32]*modInfo{}, byName: map[string]uint32{}}
	regPut(i)
	return i, nil
}

// Close destroys the isolate and all its contexts.
func (i *Isolate) Close() {
	if i.p == 0 {
		return
	}
	regDel(i.id)
	fIsolateFree(i.p)
	i.p = 0
}

// Terminate aborts running JS (thread-safe). The running call returns an
// *Exception with Terminated set.
func (i *Isolate) Terminate() { fTerminate(i.p) }

// CancelTerminate clears a pending termination request.
func (i *Isolate) CancelTerminate() { fCancelTerm(i.p) }

// Microtasks drains the microtask queue.
func (i *Isolate) Microtasks() { fMicrotasks(i.p) }

// Pump runs one batch of platform tasks; reports whether any ran.
func (i *Isolate) Pump() bool { return fPump(i.p) != 0 }

// PendingRejections is the number of promises rejected with no handler since the page last took them (the
// global __swTakeRejections() returns and clears them).
func (i *Isolate) PendingRejections() int { return int(fRejCount(i.p)) }

// ModuleByName returns the module compiled under name in this isolate.
func (i *Isolate) ModuleByName(name string) (ModuleID, bool) {
	id, ok := i.byName[name]
	return ModuleID(id), ok
}

// LowMemory asks V8 to collect garbage.
func (i *Isolate) LowMemory() { fLowMemory(i.p) }

// HeapStats mirrors the V8 heap statistics fields we expose.
type HeapStats struct{ Total, Used, Limit, Physical, External, Malloced uint64 }

// HeapStats returns current heap numbers.
func (i *Isolate) HeapStats() HeapStats {
	var a [6]uint64
	fHeapStats(i.p, &a)
	return HeapStats{a[0], a[1], a[2], a[3], a[4], a[5]}
}

// Context is one JS global environment inside an isolate.
type Context struct {
	iso *Isolate
	id  uint32
}

// NewContext creates a context (from the snapshot's default context if the
// isolate was created with one). queueMicrotask is installed natively.
func (i *Isolate) NewContext() (*Context, error) {
	id := fContextNew(i.p)
	if id == 0 {
		return nil, errors.New("v8shim: context creation failed")
	}
	if i.ctx0 == 0 {
		i.ctx0 = id
	}
	return &Context{iso: i, id: id}, nil
}

// Close releases the context.
func (c *Context) Close() { fContextFree(c.iso.p, c.id) }

// Isolate returns the owning isolate.
func (c *Context) Isolate() *Isolate { return c.iso }

// Register installs a global host function (dotted names make namespaces).
func (c *Context) Register(name string, fn HostFunc) error {
	i := c.iso
	id := uint32(len(i.fns))
	i.fns = append(i.fns, fn)
	if r := fRegisterFn(i.p, c.id, sptr(name), uintptr(len(name)), id); r != 0 {
		return fmt.Errorf("v8shim: register %q failed (%d)", name, r)
	}
	return nil
}

func (i *Isolate) result(st int32) (Value, error) {
	o := &i.out
	if st == 0 {
		return i.decode(o), nil
	}
	if o.tag != tString {
		return Value{}, fmt.Errorf("v8shim: status %d", st)
	}
	parts := strings.SplitN(string(unsafe.Slice((*byte)(ptr(uintptr(o.u))), o.n)), "\x00", 3)
	e := &Exception{Terminated: st == 3, OOM: st == 2}
	e.Message = parts[0]
	if len(parts) > 1 {
		e.Stack = parts[1]
	}
	if len(parts) > 2 {
		e.Location = parts[2]
	}
	return Value{}, e
}

func (i *Isolate) decode(o *rawVal) Value {
	switch o.tag {
	case tUndefined:
		return Value{Kind: Undefined}
	case tNull:
		return Value{Kind: Null}
	case tBool:
		return Value{Kind: Bool, Int: int64(o.u)}
	case tInt:
		return Value{Kind: Int, Int: int64(o.u)}
	case tDouble:
		return Value{Kind: Double, Float: math.Float64frombits(o.u)}
	case tString, tJSON:
		return Value{Kind: Kind(o.tag), Str: string(unsafe.Slice((*byte)(ptr(uintptr(o.u))), o.n))}
	case tBytes:
		return Value{Kind: Bytes, Bytes: append([]byte(nil), unsafe.Slice((*byte)(ptr(uintptr(o.u))), o.n)...)}
	}
	return Value{}
}

// Eval compiles and runs a classic script.
func (c *Context) Eval(src, name string) (Value, error) {
	i := c.iso
	st := fScript(i.p, c.id, sptr(src), uintptr(len(src)), sptr(name), uintptr(len(name)), 0, nil, 0, nil, &i.out)
	return i.result(st)
}

// EvalStash runs a classic script and keeps its completion value as the global __swLast instead of copying
// it out, so the caller can post-process the live value in JS (awaiting a promise, serialising a DOM node).
func (c *Context) EvalStash(src, name string) error {
	i := c.iso
	st := fScript(i.p, c.id, sptr(src), uintptr(len(src)), sptr(name), uintptr(len(name)), 5, nil, 0, nil, &i.out)
	_, err := i.result(st)
	return err
}

// CompileOnly parses and compiles src without running it (lazy, or eager when
// eager is set). Useful for measuring parse cost.
func (c *Context) CompileOnly(src, name string, eager bool) error {
	i := c.iso
	mode := int32(3)
	if eager {
		mode = 4
	}
	st := fScript(i.p, c.id, sptr(src), uintptr(len(src)), sptr(name), uintptr(len(name)), mode, nil, 0, nil, &i.out)
	_, err := i.result(st)
	return err
}

// CompileCache compiles src eagerly and returns V8's code cache bytes.
func (c *Context) CompileCache(src, name string) ([]byte, error) {
	i := c.iso
	st := fScript(i.p, c.id, sptr(src), uintptr(len(src)), sptr(name), uintptr(len(name)), 1, nil, 0, nil, &i.out)
	v, err := i.result(st)
	if err != nil {
		return nil, err
	}
	return v.Bytes, nil
}

// EvalCached runs src using a code cache. rejected reports whether V8 refused
// the cache (version/flags/source mismatch, corruption); the script still ran.
func (c *Context) EvalCached(src, name string, cache []byte) (v Value, rejected bool, err error) {
	i := c.iso
	var rej int32
	var cp unsafe.Pointer
	if len(cache) > 0 {
		cp = unsafe.Pointer(&cache[0])
	}
	st := fScript(i.p, c.id, sptr(src), uintptr(len(src)), sptr(name), uintptr(len(name)), 2, cp, uintptr(len(cache)), &rej, &i.out)
	runtime.KeepAlive(cache)
	v, err = i.result(st)
	return v, rej != 0, err
}

// Call invokes the global function (dotted path allowed) with Go values:
// nil, bool, ints, floats, string, []byte, []int32.
func (c *Context) Call(name string, args ...any) (Value, error) {
	i := c.iso
	raw := make([]rawVal, len(args))
	keep := make([]any, 0, len(args))
	for k, a := range args {
		switch x := a.(type) {
		case nil:
			raw[k] = rawVal{tag: tNull}
		case JSUndefined:
			raw[k] = rawVal{tag: tUndefined}
		case bool:
			raw[k] = rawVal{tag: tBool}
			if x {
				raw[k].u = 1
			}
		case int:
			raw[k] = rawVal{tag: tInt, u: uint64(int64(x))}
		case int32:
			raw[k] = rawVal{tag: tInt, u: uint64(int64(x))}
		case int64:
			raw[k] = rawVal{tag: tInt, u: uint64(x)}
		case float64:
			raw[k] = rawVal{tag: tDouble, u: math.Float64bits(x)}
		case string:
			raw[k] = rawVal{tag: tString, n: uint32(len(x)), u: uint64(uintptr(sptr(x)))}
			keep = append(keep, x)
		case []byte:
			var p uintptr
			if len(x) > 0 {
				p = uintptr(unsafe.Pointer(&x[0]))
			}
			raw[k] = rawVal{tag: tBytes, n: uint32(len(x)), u: uint64(p)}
			keep = append(keep, x)
		case []int32:
			var p uintptr
			if len(x) > 0 {
				p = uintptr(unsafe.Pointer(&x[0]))
			}
			raw[k] = rawVal{tag: tInt32s, n: uint32(len(x)), u: uint64(p)}
			keep = append(keep, x)
		case int8, int16, uint8, uint16, uint32:
			raw[k] = rawVal{tag: tInt, u: uint64(reflect.ValueOf(x).Convert(reflect.TypeOf(int64(0))).Int())}
		case uint, uint64:
			raw[k] = rawVal{tag: tDouble, u: math.Float64bits(float64(reflect.ValueOf(x).Uint()))}
		case float32:
			raw[k] = rawVal{tag: tDouble, u: math.Float64bits(float64(x))}
		default:
			if rv, ok := buildVal(a, &keep); ok { // maps and slices: a typed tree, no JSON text
				raw[k] = rv
				continue
			}
			// structs and the rest: JSON in, JSON.parse on the JS side
			b, err := json.Marshal(a)
			if err != nil {
				return Value{}, fmt.Errorf("v8shim: unsupported arg type %T: %w", a, err)
			}
			var p uintptr
			if len(b) > 0 {
				p = uintptr(unsafe.Pointer(&b[0]))
			}
			raw[k] = rawVal{tag: tJSON, n: uint32(len(b)), u: uint64(p)}
			keep = append(keep, b)
		}
	}
	var ap unsafe.Pointer
	if len(raw) > 0 {
		ap = unsafe.Pointer(&raw[0])
	}
	st := fCall(i.p, c.id, sptr(name), uintptr(len(name)), ap, uint32(len(raw)), &i.out)
	runtime.KeepAlive(raw)
	runtime.KeepAlive(keep)
	return i.result(st)
}

// CreateSnapshot runs src in a fresh context and serialises the heap. With
// keepCode the compiled bytecode is retained in the blob.
func CreateSnapshot(src, name string, keepCode bool) ([]byte, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	var p, n uintptr
	var ev rawVal
	k := int32(0)
	if keepCode {
		k = 1
	}
	st := fSnapCreate(sptr(src), uintptr(len(src)), sptr(name), uintptr(len(name)), k, &p, &n, &ev)
	if st != 0 {
		if ev.tag == tString {
			msg := string(unsafe.Slice((*byte)(ptr(uintptr(ev.u))), ev.n))
			fFree(uintptr(ev.u), uintptr(ev.n))
			return nil, errors.New("v8shim: snapshot: " + msg)
		}
		return nil, fmt.Errorf("v8shim: snapshot failed (%d)", st)
	}
	b := append([]byte(nil), unsafe.Slice((*byte)(ptr(p)), n)...)
	fFree(p, n)
	return b, nil
}

// ---------------------------------------------------------------------------
// Modules
// ---------------------------------------------------------------------------

// ModuleID identifies a compiled module.
type ModuleID uint32

// CompileModule compiles an ES module source under the given name.
func (c *Context) CompileModule(src, name string) (ModuleID, error) {
	i := c.iso
	st := fModCompile(i.p, c.id, sptr(src), uintptr(len(src)), sptr(name), uintptr(len(name)), &i.out)
	v, err := i.result(st)
	if err != nil {
		return 0, err
	}
	id := uint32(v.Int)
	i.mods[id] = &modInfo{name: name, id: id}
	i.byName[name] = id
	return ModuleID(id), nil
}

// RunModule links (calling Loader for imports), evaluates and drains
// microtasks. pending reports a top-level await that has not settled yet (it
// waits for a timer or the network); the caller keeps pumping and the module
// finishes later. A module whose evaluation rejected returns an *Exception.
func (c *Context) RunModule(m ModuleID) (pending bool, err error) {
	return c.RunModuleDrain(m, true)
}

// RunModuleDrain is RunModule with a choice about the microtask queue: with drain false the evaluation's
// promise jobs stay queued for the caller's own checkpoint (Isolate.Microtasks), as in a browser's event loop.
func (c *Context) RunModuleDrain(m ModuleID, drain bool) (pending bool, err error) {
	i := c.iso
	i.ctx0 = c.id
	d := int32(0)
	if drain {
		d = 1
	}
	st := fModRun(i.p, c.id, uint32(m), d, &i.out)
	v, err := i.result(st)
	return err == nil && v.Kind == Int && v.Int == 1, err
}

// ModuleExport reads an export of an evaluated module.
func (c *Context) ModuleExport(m ModuleID, name string) (Value, error) {
	i := c.iso
	st := fModExport(i.p, c.id, uint32(m), sptr(name), uintptr(len(name)), &i.out)
	return i.result(st)
}

// loadModule compiles (or returns a cached) module for a loader answer.
func (i *Isolate) loadModule(specifier, referrer string) (uint32, string) {
	if i.Loader == nil {
		return 0, "no module loader installed"
	}
	name, src, err := i.Loader(specifier, referrer)
	if err != nil {
		return 0, err.Error()
	}
	if id, ok := i.byName[name]; ok {
		return id, ""
	}
	c := &Context{iso: i, id: i.ctx0}
	id, err := c.CompileModule(src, name)
	if err != nil {
		return 0, err.Error()
	}
	return uint32(id), ""
}

// ---------------------------------------------------------------------------
// Callbacks from Rust
// ---------------------------------------------------------------------------

func setRetString(i *Isolate, out *rawVal, s string) {
	b := []byte(s)
	i.keepB = b
	out.tag = tString
	out.n = uint32(len(b))
	if len(b) > 0 {
		out.u = uint64(uintptr(unsafe.Pointer(&b[0])))
	}
}

// eventTramp serves resolve (1), dynamic import (2) and import.meta (3).
func eventTramp(kind uintptr, isoID uintptr, a uintptr, p1 uintptr, l1 uintptr, p2 uintptr, l2 uintptr, out uintptr) uintptr {
	i := regGet(uint64(isoID))
	if i == nil {
		return 0
	}
	ov := (*rawVal)(ptr(out))
	str := func(p, l uintptr) string {
		if l == 0 {
			return ""
		}
		return string(unsafe.Slice((*byte)(ptr(p)), l))
	}
	switch kind {
	case 1: // static import: a = referrer module id
		ref := ""
		if m := i.mods[uint32(a)]; m != nil {
			ref = m.name
		}
		id, msg := i.loadModule(str(p1, l1), ref)
		if id == 0 {
			setRetString(i, ov, msg)
			return 0
		}
		return uintptr(id)
	case 2: // dynamic import: p1 = referrer name, p2 = specifier
		id, msg := i.loadModule(str(p2, l2), str(p1, l1))
		if id == 0 {
			setRetString(i, ov, msg)
			return 0
		}
		return uintptr(id)
	case 3: // import.meta: a = module id
		name := ""
		if m := i.mods[uint32(a)]; m != nil {
			name = m.name
		}
		if i.ImportMeta != nil {
			name = i.ImportMeta(name)
		}
		setRetString(i, ov, name)
		return 0
	}
	return 0
}

// hostTramp dispatches JS -> Go host function calls.
func hostTramp(isoID, fnID, nargs, args, ret uintptr) (r uintptr) {
	i := regGet(uint64(isoID))
	if i == nil || int(fnID) >= len(i.fns) {
		return 1
	}
	if i.dep == len(i.call) {
		i.call = append(i.call, &Call{})
	}
	c := i.call[i.dep]
	i.dep++
	c.iso = i
	c.ret = (*rawVal)(ptr(ret))
	if nargs > 0 {
		c.args = unsafe.Slice((*rawVal)(ptr(args)), nargs)
	} else {
		c.args = nil
	}
	defer func() {
		i.dep--
		if e := recover(); e != nil {
			setRetString(i, c.ret, fmt.Sprint("Go panic in host function: ", e))
			r = 1
		}
	}()
	i.fns[fnID](c)
	return 0
}

// Call is the view of one JS->Go host call.
type Call struct {
	iso  *Isolate
	args []rawVal
	ret  *rawVal
	keep []byte
	keeS string
	pins []any // memory a tree result points at, alive until the next host call at this depth
}

// N is the argument count.
func (c *Call) N() int { return len(c.args) }

// Kind of argument i (Undefined when out of range).
func (c *Call) Kind(i int) Kind {
	if i >= len(c.args) {
		return Undefined
	}
	t := c.args[i].tag
	if t == tView {
		return Bytes
	}
	return Kind(t)
}

// Int returns argument i as an integer (floats truncate).
func (c *Call) Int(i int) int64 {
	if i >= len(c.args) {
		return 0
	}
	a := &c.args[i]
	if a.tag == tDouble {
		return int64(math.Float64frombits(a.u))
	}
	return int64(a.u)
}

// Float returns argument i as a float64.
func (c *Call) Float(i int) float64 {
	if i >= len(c.args) {
		return 0
	}
	a := &c.args[i]
	if a.tag == tDouble {
		return math.Float64frombits(a.u)
	}
	return float64(int64(a.u))
}

// Bool returns argument i as a boolean.
func (c *Call) Bool(i int) bool { return i < len(c.args) && c.args[i].u != 0 }

// Str copies argument i (string or JSON text) into a Go string.
func (c *Call) Str(i int) string {
	if i >= len(c.args) || c.args[i].n == 0 {
		return ""
	}
	a := &c.args[i]
	return string(unsafe.Slice((*byte)(ptr(uintptr(a.u))), a.n))
}

// StrView is Str without a copy: valid only until the host function returns
// and until it makes a re-entrant call into the isolate.
func (c *Call) StrView(i int) string {
	if i >= len(c.args) || c.args[i].n == 0 {
		return ""
	}
	a := &c.args[i]
	return unsafe.String((*byte)(ptr(uintptr(a.u))), a.n)
}

// View returns the shared backing store of an ArrayBuffer/TypedArray argument
// (zero copy, writable). Valid only during the call.
func (c *Call) View(i int) []byte {
	if i >= len(c.args) || c.args[i].tag != tView || c.args[i].n == 0 {
		return nil
	}
	a := &c.args[i]
	return unsafe.Slice((*byte)(ptr(uintptr(a.u))), a.n)
}

// RetUndefined, RetNull, RetBool, RetInt, RetFloat, RetString, RetJSON,
// RetInt32s and RetThrow set the call's result.
func (c *Call) RetUndefined() { c.ret.tag = tUndefined }
func (c *Call) RetNull()      { c.ret.tag = tNull }
func (c *Call) RetBool(b bool) {
	c.ret.tag = tBool
	c.ret.u = 0
	if b {
		c.ret.u = 1
	}
}
func (c *Call) RetInt(v int64) { c.ret.tag = tInt; c.ret.u = uint64(v) }
func (c *Call) RetFloat(v float64) {
	c.ret.tag = tDouble
	c.ret.u = math.Float64bits(v)
}
func (c *Call) retStr(tag uint32, s string) {
	c.keeS = s
	c.ret.tag = tag
	c.ret.n = uint32(len(s))
	c.ret.u = uint64(uintptr(sptr(s)))
}
func (c *Call) RetString(s string) { c.retStr(tString, s) }
func (c *Call) RetJSON(s string)   { c.retStr(tJSON, s) }
func (c *Call) RetThrow(s string)  { c.retStr(tThrow, s) }

// RetInt32s returns a JS array of ints (e.g. child id lists).
func (c *Call) RetInt32s(v []int32) {
	c.ret.tag = tInt32s
	c.ret.n = uint32(len(v))
	if len(v) > 0 {
		c.ret.u = uint64(uintptr(unsafe.Pointer(&v[0])))
		c.keep = unsafe.Slice((*byte)(unsafe.Pointer(&v[0])), len(v)*4)
	}
}
