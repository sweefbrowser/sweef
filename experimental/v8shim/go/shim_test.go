package v8shim

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// newCtx returns an isolate + context pinned to this test's OS thread.
func newCtx(t testing.TB, o Options) (*Isolate, *Context) {
	t.Helper()
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	if err := Load(); err != nil {
		if os.Getenv("SWEEF_V8_REQUIRE") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	iso, err := NewIsolate(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(iso.Close)
	ctx, err := iso.NewContext()
	if err != nil {
		t.Fatal(err)
	}
	return iso, ctx
}

func mustEval(t testing.TB, c *Context, src string) Value {
	t.Helper()
	v, err := c.Eval(src, "test.js")
	if err != nil {
		t.Fatalf("eval %q: %v", src, err)
	}
	return v
}

func TestLoadAndVersion(t *testing.T) {
	newCtx(t, Options{})
	if Version() == "" || LibPath() == "" {
		t.Fatalf("version %q path %q", Version(), LibPath())
	}
	t.Logf("V8 %s from %s", Version(), LibPath())
}

func TestEvalTypes(t *testing.T) {
	_, c := newCtx(t, Options{})
	cases := []struct {
		src  string
		kind Kind
		want string
	}{
		{"1+2", Int, "3"},
		{"1.5*2+0.25", Double, "3.25"},
		{"'h' + 'é' + '世'", String, "hé世"},
		{"true", Bool, "true"},
		{"null", Null, "null"},
		{"undefined", Undefined, "undefined"},
		{"({a:1,b:[1,2,{c:'x'}]})", JSON, `{"a":1,"b":[1,2,{"c":"x"}]}`},
		{"[1,'two',null]", JSON, `[1,"two",null]`},
		{"2**40", Double, "1099511627776"},
	}
	for _, tc := range cases {
		v := mustEval(t, c, tc.src)
		if v.Kind != tc.kind || v.String() != tc.want {
			t.Errorf("%s: got kind %d %q, want kind %d %q", tc.src, v.Kind, v.String(), tc.kind, tc.want)
		}
	}
	// State persists across evals in a context; contexts are isolated.
	mustEval(t, c, "var counter = 41")
	if v := mustEval(t, c, "++counter"); v.Int != 42 {
		t.Fatalf("counter %v", v)
	}
	c2, _ := c.iso.NewContext()
	if _, err := c2.Eval("counter", "x.js"); err == nil {
		t.Fatal("contexts must not share globals")
	}
}

func TestErrorsWithStack(t *testing.T) {
	_, c := newCtx(t, Options{})
	_, err := c.Eval("function f(){ throw new TypeError('boom') }\nfunction g(){ f() }\ng()", "app.js")
	var ex *Exception
	if !errors.As(err, &ex) {
		t.Fatalf("want *Exception, got %v", err)
	}
	if ex.Message != "TypeError: boom" {
		t.Errorf("message %q", ex.Message)
	}
	if !strings.Contains(ex.Stack, "at f (app.js:1:") || !strings.Contains(ex.Stack, "at g (app.js:2:") {
		t.Errorf("stack lacks frames:\n%s", ex.Stack)
	}
	if !strings.HasPrefix(ex.Location, "app.js:1:") {
		t.Errorf("location %q", ex.Location)
	}
	// Syntax errors and non-Error throws.
	_, err = c.Eval("var = 1", "bad.js")
	if !errors.As(err, &ex) || !strings.Contains(ex.Message, "SyntaxError") {
		t.Errorf("syntax: %v", err)
	}
	_, err = c.Eval("throw 'plain string'", "t.js")
	if !errors.As(err, &ex) || ex.Message != "plain string" {
		t.Errorf("plain throw: %v", err)
	}
	// Isolate is still usable.
	if v := mustEval(t, c, "7*6"); v.Int != 42 {
		t.Fatal("not usable after error")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	_, c := newCtx(t, Options{})
	var got string
	c.Register("sw.take", func(call *Call) {
		got = call.Str(0)
		call.RetJSON(`{"echo":` + got + `,"n":[1,2,3],"s":"ü\n"}`)
	})
	v := mustEval(t, c, `var r = sw.take({k:'v',nest:{a:[1,2]}}); JSON.stringify([r.echo.nest.a, r.n.length, r.s])`)
	if got != `{"k":"v","nest":{"a":[1,2]}}` {
		t.Errorf("host got %q", got)
	}
	if v.Str != `[[1,2],3,"ü\n"]` {
		t.Errorf("js saw %q", v.Str)
	}
	// Go -> JS typed args and back.
	mustEval(t, c, "function echo(a,b,c,d,e,f){ return JSON.stringify([a,b,c,d,e,f]) }")
	v, err := c.Call("echo", 7, 2.5, "héllo", true, nil, []int32{4, 5})
	if err != nil {
		t.Fatal(err)
	}
	if v.Str != `[7,2.5,"héllo",true,null,[4,5]]` {
		t.Errorf("echo = %s", v.Str)
	}
}

func TestHostFunctionKinds(t *testing.T) {
	_, c := newCtx(t, Options{})
	c.Register("addI", func(call *Call) { call.RetInt(call.Int(0) + call.Int(1)) })
	c.Register("cat", func(call *Call) { call.RetString(call.Str(0) + "|" + call.Str(1)) })
	c.Register("half", func(call *Call) { call.RetFloat(call.Float(0) / 2) })
	c.Register("ids", func(call *Call) { call.RetInt32s([]int32{1, 2, 3, int32(call.Int(0))}) })
	c.Register("boom", func(call *Call) { call.RetThrow("host says no") })
	c.Register("fill", func(call *Call) {
		b := call.View(0)
		for i := range b {
			b[i] = byte(i * 2)
		}
		call.RetInt(int64(len(b)))
	})
	c.Register("kinds", func(call *Call) {
		var parts []string
		for i := 0; i < call.N(); i++ {
			parts = append(parts, fmt.Sprint(call.Kind(i)))
		}
		call.RetString(strings.Join(parts, ","))
	})
	c.Register("panics", func(call *Call) { panic("oops") })
	checks := map[string]string{
		"addI(40,2)":             "42",
		"addI(2**31, 1)":         "2147483649",
		"cat('a','b')":           "a|b",
		"half(5)":                "2.5",
		"JSON.stringify(ids(9))": "[1,2,3,9]",
		"(()=>{try{boom()}catch(e){return e.message}})()":                           "host says no",
		"(()=>{try{panics()}catch(e){return e.message}})()":                         "Go panic in host function: oops",
		"kinds(1,1.5,'s',true,null,undefined,{a:1},new Uint8Array(2))":              "3,4,5,2,1,0,6,9",
		"(()=>{const u=new Uint8Array(5); fill(u); return Array.from(u).join()})()": "0,2,4,6,8",
	}
	for src, want := range checks {
		if got := mustEval(t, c, src).String(); got != want {
			t.Errorf("%s = %q want %q", src, got, want)
		}
	}
}

func TestReentrantCallbacks(t *testing.T) {
	_, c := newCtx(t, Options{})
	// JS -> Go (host) -> JS (eval/call) -> Go (host) -> JS, three deep.
	var depthSeen int
	c.Register("hostA", func(call *Call) {
		n := call.Int(0)
		depthSeen++
		v, err := c.Eval(fmt.Sprintf("jsB(%d)", n), "reenter.js")
		if err != nil {
			call.RetThrow(err.Error())
			return
		}
		call.RetInt(v.Int + 1000)
	})
	mustEval(t, c, `function jsB(n){ return n > 0 ? hostA(n-1) + 1 : 7 }`)
	v := mustEval(t, c, "hostA(2)")
	// hostA(2)->jsB(2)=hostA(1)+1 ; hostA(1)->jsB(1)=hostA(0)+1 ; hostA(0)->jsB(0)=7
	// hostA(0)=1007 ; jsB(1)=1008 ; hostA(1)=2008 ; jsB(2)=2009 ; hostA(2)=3009
	if v.Int != 3009 || depthSeen != 3 {
		t.Fatalf("got %v depth %d", v, depthSeen)
	}
	// Go-initiated Call from inside a host function, args and strings intact.
	c.Register("hostC", func(call *Call) {
		s := call.Str(0)
		r, err := c.Call("jsUp", s+"!", 5)
		if err != nil {
			call.RetThrow(err.Error())
			return
		}
		call.RetString(s + "->" + r.Str)
	})
	mustEval(t, c, "function jsUp(s,n){ return s.toUpperCase()+n }")
	if v := mustEval(t, c, "hostC('abc')"); v.Str != "abc->ABC!5" {
		t.Fatalf("got %q", v.Str)
	}
	// An exception thrown inside the nested JS surfaces through the host fn.
	c.Register("hostD", func(call *Call) {
		_, err := c.Eval("throw new Error('inner')", "inner.js")
		call.RetThrow(err.(*Exception).Message)
	})
	if v := mustEval(t, c, "(()=>{try{hostD()}catch(e){return e.message}})()"); v.Str != "Error: inner" {
		t.Fatalf("got %q", v.Str)
	}
}

// moduleMap is a tiny in-memory module system for tests.
func moduleMap(m map[string]string, log *[]string) ModuleLoader {
	return func(spec, ref string) (string, string, error) {
		name := spec
		if strings.HasPrefix(spec, "./") {
			name = "mem:///" + strings.TrimPrefix(spec, "./")
		}
		if log != nil {
			*log = append(*log, ref+" -> "+name)
		}
		src, ok := m[name]
		if !ok {
			return "", "", fmt.Errorf("module not found: %s", name)
		}
		return name, src, nil
	}
}

func TestModuleGraphWithCycle(t *testing.T) {
	iso, c := newCtx(t, Options{})
	var log []string
	iso.Loader = moduleMap(map[string]string{
		"mem:///b.js":      `import { a } from './a.js'; export const b = () => 'b' + a(); export function early(){ return typeof a }`,
		"mem:///a.js":      `import { b, early } from './b.js'; export const a = () => 'a'; export function probe(){ return typeof b }; export function viaB(){ return b() }`,
		"mem:///util.js":   `export default 40; export const two = 2;`,
		"mem:///throws.js": `throw new Error('module failed')`,
	}, &log)
	main := `
import { b, early } from './b.js';
import { viaB, probe } from './a.js';
import answer, { two } from './util.js';
export const result = [b(), viaB(), probe(), answer + two].join(',');
`
	id, err := c.CompileModule(main, "mem:///main.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RunModule(id); err != nil {
		t.Fatalf("run: %v (log %v)", err, log)
	}
	v, err := c.ModuleExport(id, "result")
	if err != nil {
		t.Fatal(err)
	}
	// In the a<->b cycle, b() returns 'b'+a() = 'ba', a's bAtEval is "function"? b is a
	// const defined later in the cycle order, so reading it while a evaluates first
	// would be a TDZ error; V8 evaluates b first (dependency order), so it is "function".
	if v.Str != "ba,ba,function,42" {
		t.Fatalf("result %q log %v", v.Str, log)
	}
	// Resolver was consulted with real referrer names.
	if !strings.Contains(strings.Join(log, ";"), "mem:///main.js -> mem:///b.js") ||
		!strings.Contains(strings.Join(log, ";"), "mem:///b.js -> mem:///a.js") {
		t.Errorf("log %v", log)
	}
	// Missing module and throwing module are reported, isolate survives.
	bad, _ := c.CompileModule(`import './nope.js'`, "mem:///bad.js")
	err = c.RunModule(bad)
	if err == nil || !strings.Contains(err.Error(), "module not found: mem:///nope.js") {
		t.Errorf("missing: %v", err)
	}
	thr, _ := c.CompileModule(`import './throws.js'`, "mem:///thr.js")
	err = c.RunModule(thr)
	if err == nil || !strings.Contains(err.Error(), "module failed") {
		t.Errorf("throwing: %v", err)
	}
	if _, err := c.CompileModule(`export const = ;`, "mem:///syntax.js"); err == nil {
		t.Error("syntax error expected")
	}
}

func TestDynamicImportAndImportMeta(t *testing.T) {
	iso, c := newCtx(t, Options{})
	iso.Loader = moduleMap(map[string]string{
		"mem:///lazy.js":  `export const hello = 'lazy-' + import.meta.url; export default 99`,
		"mem:///fails.js": `throw new Error('dyn fail')`,
	}, nil)
	iso.ImportMeta = func(name string) string { return "sweef://" + strings.TrimPrefix(name, "mem:///") }
	main := `
export let out = [];
export const meta = import.meta.url;
const m = await import('./lazy.js');
out.push(m.hello, m.default);
try { await import('./missing.js') } catch (e) { out.push('caught:' + e.message) }
try { await import('./fails.js') } catch (e) { out.push('caught:' + e.message) }
`
	id, err := c.CompileModule(main, "mem:///main.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RunModule(id); err != nil {
		t.Fatal(err)
	}
	v, _ := c.ModuleExport(id, "meta")
	if v.Str != "sweef://main.js" {
		t.Errorf("import.meta.url = %q", v.Str)
	}
	v, _ = c.ModuleExport(id, "out")
	want := `["lazy-sweef://lazy.js",99,"caught:module not found: mem:///missing.js","caught:Error: dyn fail"]`
	if !strings.Contains(v.Str, `"lazy-sweef://lazy.js",99`) || !strings.Contains(v.Str, "module not found") {
		t.Errorf("out = %s\nwant like %s", v.Str, want)
	}
	// Dynamic import from a classic script.
	mustEval(t, c, `var dynRes; import('./lazy.js').then(m => { dynRes = m.default })`)
	iso.Microtasks()
	if v := mustEval(t, c, "dynRes"); v.Int != 99 {
		t.Errorf("classic dynamic import: %v", v)
	}
}

func TestMicrotaskOrdering(t *testing.T) {
	iso, c := newCtx(t, Options{})
	mustEval(t, c, `
var log = [];
log.push('sync1');
Promise.resolve().then(() => log.push('then1')).then(() => log.push('then2'));
queueMicrotask(() => log.push('qm1'));
(async () => { log.push('async-start'); await null; log.push('async-after-await') })();
queueMicrotask(() => { log.push('qm2'); Promise.resolve().then(() => log.push('then-from-qm2')) });
log.push('sync2');
`)
	// Explicit microtask policy: nothing runs until we drain.
	if v := mustEval(t, c, "log.join()"); v.Str != "sync1,async-start,sync2" {
		t.Fatalf("before drain: %q", v.Str)
	}
	iso.Microtasks()
	want := "sync1,async-start,sync2,then1,qm1,async-after-await,qm2,then2,then-from-qm2"
	if v := mustEval(t, c, "log.join()"); v.Str != want {
		t.Fatalf("after drain:\n got %q\nwant %q", v.Str, want)
	}
}

func TestCodeCache(t *testing.T) {
	_, c := newCtx(t, Options{})
	src := "var fib = n => n < 2 ? n : fib(n-1) + fib(n-2);\n" + strings.Repeat("function f_x(a){return a*2+1}\n", 200) + "fib(15)"
	cache, err := c.CompileCache(src, "cached.js")
	if err != nil || len(cache) < 100 {
		t.Fatalf("cache: %d bytes err %v", len(cache), err)
	}
	// Hit.
	v, rej, err := c.EvalCached(src, "cached.js", cache)
	if err != nil || rej || v.Int != 610 {
		t.Fatalf("hit: v=%v rejected=%v err=%v", v, rej, err)
	}
	// Miss: source changed (V8 checks a source hash / length).
	v, rej, err = c.EvalCached(src+" ", "cached.js", cache)
	if err != nil || !rej || v.Int != 610 {
		t.Fatalf("miss: v=%v rejected=%v err=%v", v, rej, err)
	}
	// V8 keeps a per-isolate compilation cache keyed by source, so every
	// trial below runs in a fresh isolate to actually exercise the cache bytes.
	fresh := func() *Context {
		iso2, err := NewIsolate(Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(iso2.Close)
		c2, _ := iso2.NewContext()
		return c2
	}
	// Corruption: V8's checksum must reject it and the script must still run.
	bad := append([]byte(nil), cache...)
	for i := len(bad) / 2; i < len(bad)/2+16; i++ {
		bad[i] ^= 0xff
	}
	v, rej, err = fresh().EvalCached(src, "cached.js", bad)
	if err != nil || !rej || v.Int != 610 {
		t.Fatalf("corrupt: v=%v rejected=%v err=%v", v, rej, err)
	}
	// Truncated / garbage / empty caches are rejected, never crash.
	for _, junk := range [][]byte{cache[:20], []byte("not a code cache at all, definitely"), {}} {
		v, rej, err = fresh().EvalCached(src, "cached.js", junk)
		if err != nil || !rej || v.Int != 610 {
			t.Fatalf("junk %d: v=%v rejected=%v err=%v", len(junk), v, rej, err)
		}
	}
	// A cache produced in one isolate loads in another.
	iso2, _ := NewIsolate(Options{})
	defer iso2.Close()
	c2, _ := iso2.NewContext()
	v, rej, err = c2.EvalCached(src, "cached.js", cache)
	if err != nil || rej || v.Int != 610 {
		t.Fatalf("cross-isolate: v=%v rejected=%v err=%v", v, rej, err)
	}
}

func TestTerminateExecution(t *testing.T) {
	iso, c := newCtx(t, Options{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		iso.Terminate()
	}()
	start := time.Now()
	_, err := c.Eval("while(1){}", "spin.js")
	var ex *Exception
	if !errors.As(err, &ex) || !ex.Terminated {
		t.Fatalf("want terminated, got %v", err)
	}
	if d := time.Since(start); d < 90*time.Millisecond || d > 5*time.Second {
		t.Errorf("terminated after %v", d)
	}
	// Isolate remains usable afterwards.
	if v := mustEval(t, c, "6*7"); v.Int != 42 {
		t.Fatal("not usable after terminate")
	}
	// Termination also interrupts a loop that is inside a host callback chain.
	c.Register("tick", func(call *Call) {})
	go func() {
		time.Sleep(50 * time.Millisecond)
		iso.Terminate()
	}()
	if _, err := c.Eval("for(;;){ tick() }", "spin2.js"); !errors.As(err, &ex) || !ex.Terminated {
		t.Fatalf("host loop: %v", err)
	}
}

func TestHeapLimitOOMDoesNotCrash(t *testing.T) {
	iso, c := newCtx(t, Options{HeapLimit: 48 << 20})
	_, err := c.Eval("var a=[]; for(;;){ a.push(new Array(100000).fill(a.length)) }", "oom.js")
	var ex *Exception
	if !errors.As(err, &ex) || !ex.OOM {
		t.Fatalf("want OOM exception, got %v", err)
	}
	t.Logf("oom handled: %s (heap used %d MB)", ex.Message, iso.HeapStats().Used>>20)
	// The process is alive; a fresh isolate works.
	iso2, err := NewIsolate(Options{HeapLimit: 48 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer iso2.Close()
	c2, _ := iso2.NewContext()
	if v, err := c2.Eval("[1,2,3].length", "x.js"); err != nil || v.Int != 3 {
		t.Fatalf("fresh isolate: %v %v", v, err)
	}
}

func TestConcurrentIsolates(t *testing.T) {
	if err := Load(); err != nil {
		if os.Getenv("SWEEF_V8_REQUIRE") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	const N = 8
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for g := 0; g < N; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			iso, err := NewIsolate(Options{})
			if err != nil {
				errs <- err
				return
			}
			defer iso.Close()
			c, _ := iso.NewContext()
			var calls int
			c.Register("bump", func(call *Call) { calls++; call.RetInt(call.Int(0) + int64(g)) })
			v, err := c.Eval(`var s=0; for (let i=0;i<20000;i++){ s = bump(s) } s`, "loop.js")
			if err != nil {
				errs <- err
				return
			}
			if v.Int != int64(20000*g) || calls != 20000 {
				errs <- fmt.Errorf("goroutine %d: got %v calls %d", g, v, calls)
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

const testPrelude = `
var Prelude = { version: 7, make(n){ const o=[]; for(let i=0;i<n;i++) o.push(i*i); return o } };
function preludeSum(n){ return Prelude.make(n).reduce((a,b)=>a+b,0) }
`

func TestSnapshot(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := Load(); err != nil {
		if os.Getenv("SWEEF_V8_REQUIRE") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	for _, keep := range []bool{false, true} {
		snap, err := CreateSnapshot(testPrelude, "prelude.js", keep)
		if err != nil || len(snap) < 1000 {
			t.Fatalf("snapshot keep=%v: %d bytes, %v", keep, len(snap), err)
		}
		iso, err := NewIsolate(Options{Snapshot: snap})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := iso.NewContext()
		v, err := c.Eval("Prelude.version * 1000 + preludeSum(4)", "use.js")
		if err != nil || v.Int != 7014 {
			t.Fatalf("keep=%v: %v %v", keep, v, err)
		}
		// Host functions registered after restore work; queueMicrotask present.
		c.Register("hostAdd", func(call *Call) { call.RetInt(call.Int(0) + call.Int(1)) })
		if v := mustEval(t, c, "hostAdd(preludeSum(3), 1)"); v.Int != 6 {
			t.Fatalf("host after snapshot: %v", v)
		}
		// Second context from the same isolate gets a fresh copy of the prelude state.
		c2, _ := iso.NewContext()
		mustEval(t, c, "Prelude.version = 8")
		if v := mustEval(t, c2, "Prelude.version"); v.Int != 7 {
			t.Fatalf("contexts share prelude state: %v", v)
		}
		iso.Close()
		t.Logf("snapshot keep_code=%v: %d KB", keep, len(snap)>>10)
	}
	if _, err := CreateSnapshot("throw new Error('bad prelude')", "p.js", false); err == nil || !strings.Contains(err.Error(), "bad prelude") {
		t.Fatalf("failing snapshot: %v", err)
	}
}

func TestInterleavedIsolatesFreedOutOfOrder(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := Load(); err != nil {
		if os.Getenv("SWEEF_V8_REQUIRE") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	var isos []*Isolate
	var ctxs []*Context
	for i := 0; i < 6; i++ {
		iso, err := NewIsolate(Options{})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := iso.NewContext()
		isos, ctxs = append(isos, iso), append(ctxs, c)
	}
	// Use them in a scrambled order on one thread, host callbacks included.
	for round := 0; round < 3; round++ {
		for _, k := range []int{0, 5, 2, 3, 1, 4} {
			c := ctxs[k]
			c.Register(fmt.Sprintf("h%d_%d", k, round), func(call *Call) { call.RetInt(call.Int(0) + int64(k)) })
			v, err := c.Eval(fmt.Sprintf("h%d_%d(10)", k, round), "i.js")
			if err != nil || v.Int != int64(10+k) {
				t.Fatalf("iso %d: %v %v", k, v, err)
			}
		}
	}
	for _, k := range []int{2, 0, 5, 1, 4, 3} { // not LIFO
		ctxs[k].Close()
		isos[k].Close()
	}
}

func TestStackOverflowIsAnException(t *testing.T) {
	_, c := newCtx(t, Options{})
	if v := mustEval(t, c, "function d(n){return n?d(n-1)+1:0}; d(2000)"); v.Int != 2000 {
		t.Fatalf("depth 2000: %v", v)
	}
	_, err := c.Eval("function inf(){return inf()+1}; inf()", "inf.js")
	var ex *Exception
	if !errors.As(err, &ex) || !strings.Contains(ex.Message, "Maximum call stack size exceeded") {
		t.Fatalf("want RangeError, got %v", err)
	}
	// Nesting through host callbacks (JS -> Go -> JS ...) 200 levels deep.
	c.Register("viaHost", func(call *Call) {
		n := call.Int(0)
		v, err := c.Call("r", n-1)
		if err != nil {
			call.RetThrow(err.Error())
			return
		}
		call.RetInt(v.Int + 1)
	})
	mustEval(t, c, "function r(n){ return n > 0 ? viaHost(n) : 0 }")
	if v := mustEval(t, c, "r(200)"); v.Int != 200 {
		t.Fatalf("host nesting: %v", v)
	}
}

func TestHeapStatsAndMemory(t *testing.T) {
	iso, c := newCtx(t, Options{})
	mustEval(t, c, "var big = new Array(200000).fill('x'.repeat(10))")
	hs := iso.HeapStats()
	if hs.Used == 0 || hs.Limit == 0 || hs.Total < hs.Used {
		t.Fatalf("%+v", hs)
	}
}
