// Command v8bench measures the Go<->V8 boundary through the purego shim.
// Usage: v8bench host|mix|batch|cpu|parse FILES...|create|mem
package main

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	v8 "github.com/sweefbrowser/sweef/experimental/v8shim/go"
)

const runs = 7

func report(tag string, d []time.Duration) {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	fmt.Printf("%-44s median %9.2f ms  min %9.2f ms  (n=%d)\n", tag, float64(s[len(s)/2].Microseconds())/1000, float64(s[0].Microseconds())/1000, len(s))
}

func rssKB() int {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			n, _ := strconv.Atoi(strings.Fields(l)[1])
			return n
		}
	}
	return 0
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func run(c *v8.Context, tag, src string) {
	var d []time.Duration
	for i := 0; i < runs; i++ {
		t0 := time.Now()
		_, err := c.Eval(src, "b.js")
		must(err)
		d = append(d, time.Since(t0))
	}
	report(tag, d)
}

func newCtx(o v8.Options) (*v8.Isolate, *v8.Context) {
	iso, err := v8.NewIsolate(o)
	must(err)
	c, err := iso.NewContext()
	must(err)
	return iso, c
}

// ---- fake DOM served from Go ------------------------------------------------

const nNodes = 5000

var (
	nodeTypeT = make([]int32, nNodes)
	parentT   = make([]int32, nNodes)
	nextT     = make([]int32, nNodes)
	firstT    = make([]int32, nNodes)
	tags      = []string{"div", "span", "a", "li", "ul", "p", "section", "button"}
	treeGen   int64
)

func init() {
	for i := range nodeTypeT {
		nodeTypeT[i] = int32(1 + (i%5)/4*2)
		parentT[i] = int32(i / 4)
		nextT[i] = int32((i + 1) % nNodes)
		firstT[i] = int32((i*4 + 1) % nNodes)
	}
}

func idx(c *v8.Call, i int) int { return int(c.Int(i)) % nNodes }

type op struct {
	name   string
	weight float64
	fn     func(c *v8.Call, o int) // o = argument offset (1 when behind a dispatcher)
}

var ops = []op{
	{"nodeType", 36, func(c *v8.Call, o int) { c.RetInt(int64(nodeTypeT[idx(c, o)])) }},
	{"parent", 20, func(c *v8.Call, o int) { c.RetInt(int64(parentT[idx(c, o)])) }},
	{"tagName", 10, func(c *v8.Call, o int) { c.RetString(tags[idx(c, o)&7]) }},
	{"nextSibling", 6, func(c *v8.Call, o int) { c.RetInt(int64(nextT[idx(c, o)])) }},
	{"getAttr", 4, func(c *v8.Call, o int) {
		if c.Int(o)&1 == 0 {
			c.RetNull()
		} else {
			c.RetString("btn primary")
		}
	}},
	{"treeGen", 3.5, func(c *v8.Call, o int) { c.RetInt(treeGen) }},
	{"firstChild", 3.3, func(c *v8.Call, o int) { c.RetInt(int64(firstT[idx(c, o)])) }},
	{"attached", 2.2, func(c *v8.Call, o int) { c.RetBool(idx(c, o)&3 != 0) }},
	{"nodeValue", 2.1, func(c *v8.Call, o int) { c.RetString("some text") }},
	{"mutation", 2, func(c *v8.Call, o int) { treeGen++; c.RetUndefined() }},
	{"matches", 1.8, func(c *v8.Call, o int) { c.RetBool(c.Str(o+1) == "div") }},
	{"children", 1.4, func(c *v8.Call, o int) {
		i := int32(idx(c, o))
		c.RetInt32s([]int32{i + 1, i + 2, i + 3})
	}},
	{"hasAttr", 1.2, func(c *v8.Call, o int) { c.RetBool(len(c.Str(o+1)) > 3) }},
	{"setAttr", 0.7, func(c *v8.Call, o int) { _ = c.Str(o + 1); _ = c.Str(o + 2); c.RetUndefined() }},
	{"lastChild", 1.4, func(c *v8.Call, o int) { c.RetInt(int64(firstT[idx(c, o)] + 3)) }},
	{"appendChild", 1.2, func(c *v8.Call, o int) { c.RetInt(c.Int(o + 1)) }},
}

// replaySrc builds the setup (sequence of ops) and the timed loop.
func setupSrc() string {
	var cum []string
	tot := 0.0
	for _, o := range ops {
		tot += o.weight
	}
	acc := 0.0
	for _, o := range ops {
		acc += o.weight / tot
		cum = append(cum, fmt.Sprintf("%.6f", acc))
	}
	return fmt.Sprintf(`var N=100000, CUM=[%s], seq=new Uint8Array(N), ids=new Int32Array(N), r=12345;
for (var i=0;i<N;i++){ r=(r*1103515245+12345)&0x7fffffff; var u=(r>>8)/8388608; var k=0; while(k<CUM.length-1&&u>CUM[k])k++; seq[i]=k; r=(r*1103515245+12345)&0x7fffffff; ids[i]=(r>>8)%%5000 }`, strings.Join(cum, ","))
}

const replayLoop = `(function(){ var s=0; for (var i=0;i<N;i++){ var id=ids[i];
 switch(seq[i]){
 case 0: s+=nodeType(id);break; case 1: s+=parent(id);break; case 2: s+=tagName(id).length;break;
 case 3: s+=nextSibling(id);break; case 4: var v=getAttr(id,'class'); if(v!==null)s+=v.length;break;
 case 5: s+=treeGen();break; case 6: s+=firstChild(id);break; case 7: if(attached(id))s++;break;
 case 8: s+=nodeValue(id).length;break; case 9: mutation(id,'attributes');break; case 10: if(matches(id,'div'))s++;break;
 case 11: s+=children(id).length;break; case 12: if(hasAttr(id,'class'))s++;break; case 13: setAttr(id,'class','x');break;
 case 14: s+=lastChild(id);break; case 15: s+=appendChild(id,id+1);break; }
 } return s })()`

// jsStubs implements the same API in pure JS for the bridge-free baseline.
const jsStubs = `var nt=new Int32Array(5000),pt=new Int32Array(5000),nx=new Int32Array(5000),fc=new Int32Array(5000),tg=['div','span','a','li','ul','p','section','button'],gen=0;
for(var i=0;i<5000;i++){nt[i]=1+((i%5)/4|0)*2;pt[i]=i/4|0;nx[i]=(i+1)%5000;fc[i]=(i*4+1)%5000}
function nodeType(i){return nt[i]} function parent(i){return pt[i]} function tagName(i){return tg[i&7]}
function nextSibling(i){return nx[i]} function getAttr(i,s){return (i&1)==0?null:'btn primary'} function treeGen(){return gen}
function firstChild(i){return fc[i]} function attached(i){return (i&3)!=0} function nodeValue(i){return 'some text'}
function mutation(i,s){gen++} function matches(i,s){return s==='div'} function children(i){return [i+1,i+2,i+3]}
function hasAttr(i,s){return s.length>3} function setAttr(i,a,b){} function lastChild(i){return fc[i]+3} function appendChild(i,j){return j}`

func register(c *v8.Context, dispatch bool) {
	if !dispatch {
		for _, o := range ops {
			o := o
			must(c.Register(o.name, func(call *v8.Call) { o.fn(call, 0) }))
		}
		return
	}
	// Engine-style: one host function __sw_call(idx, ...args) behind JS wrappers.
	must(c.Register("__sw_call", func(call *v8.Call) { ops[call.Int(0)].fn(call, 1) }))
	var sb strings.Builder
	for i, o := range ops {
		fmt.Fprintf(&sb, "function %s(a0,a1,a2){switch(arguments.length){case 0:return __sw_call(%d);case 1:return __sw_call(%d,a0);case 2:return __sw_call(%d,a0,a1);default:return __sw_call(%d,a0,a1,a2)}}\n", o.name, i, i, i, i)
	}
	_, err := c.Eval(sb.String(), "wrap.js")
	must(err)
}

func benchHost() {
	_, c := newCtx(v8.Options{})
	var sink int64
	must(c.Register("f1", func(call *v8.Call) { call.RetInt(call.Int(0) + call.Int(1)) }))
	must(c.Register("hostf", func(call *v8.Call) {
		sink += int64(len(call.StrView(1)))
		call.RetInt(call.Int(0) + call.Int(2))
	}))
	must(c.Register("hosts", func(call *v8.Call) { call.RetString(call.StrView(0) + "x") }))
	must(c.Register("arr3", func(call *v8.Call) { i := int32(call.Int(0)); call.RetInt32s([]int32{i, i + 1, i + 2}) }))
	must(c.Register("objret", func(call *v8.Call) { call.RetJSON(`{"a":1,"b":"x"}`) }))
	must(c.Register("objarg", func(call *v8.Call) { call.RetInt(int64(len(call.StrView(0)))) }))
	must(c.Register("noop0", func(call *v8.Call) {}))
	run(c, "100k noop() (no args, undefined)", `var s=0; for (var i=0;i<100000;i++){ noop0() } s`)
	run(c, "100k f1(int,int)->int", `var s=0; for (var i=0;i<100000;i++){ s += f1(i, 3) } s`)
	run(c, "100k hostf(int,str,int)->int   [A1]", `var s=0; for (var i=0;i<100000;i++){ s += hostf(i, 'attr', 3) } s`)
	run(c, "100k hosts(str,int)->str       [A2]", `var n=0; for (var i=0;i<100000;i++){ n += hosts('tag'+(i&7), i).length } n`)
	run(c, "100k arr3(int)->[3 ints]", `var n=0; for (var i=0;i<100000;i++){ n += arr3(i).length } n`)
	run(c, "100k objret(int)->{a,b} (JSON)", `var n=0; for (var i=0;i<100000;i++){ n += objret(i).a } n`)
	run(c, "100k objarg({a,b}) (JSON arg)", `var n=0; for (var i=0;i<100000;i++){ n += objarg({a:i,b:'x'}) } n`)
	_ = sink
	// Go -> JS direction.
	_, err := c.Eval("function jsf(a,b){return a+b}", "f.js")
	must(err)
	var d []time.Duration
	for r := 0; r < runs; r++ {
		t0 := time.Now()
		for i := 0; i < 100000; i++ {
			if _, err := c.Call("jsf", i, 3); err != nil {
				panic(err)
			}
		}
		d = append(d, time.Since(t0))
	}
	report("100k Go->JS Call(jsf, int, int)", d)
	d = nil
	for r := 0; r < runs; r++ {
		t0 := time.Now()
		for i := 0; i < 100000; i++ {
			if _, err := c.Eval("1", "e.js"); err != nil {
				panic(err)
			}
		}
		d = append(d, time.Since(t0))
	}
	report("100k Go->JS Eval(\"1\") (cached compile)", d)
}

func benchMix() {
	_, js := newCtx(v8.Options{})
	_, err := js.Eval(jsStubs+"\n"+setupSrc(), "setup.js")
	must(err)
	run(js, "mix 100k  pure-JS stubs (no bridge)", replayLoop)
	for _, disp := range []bool{false, true} {
		_, c := newCtx(v8.Options{})
		register(c, disp)
		_, err := c.Eval(setupSrc(), "setup.js")
		must(err)
		name := "mix 100k  direct host fns"
		if disp {
			name = "mix 100k  engine-style __sw_call dispatcher"
		}
		run(c, name, replayLoop)
	}
}

// benchBatch: int-returning ops only, n ops per crossing, results in place.
func benchBatch() {
	_, c := newCtx(v8.Options{})
	table := []func(i int) int32{
		func(i int) int32 { return nodeTypeT[i%nNodes] },
		func(i int) int32 { return parentT[i%nNodes] },
		func(i int) int32 { return nextT[i%nNodes] },
		func(i int) int32 { return firstT[i%nNodes] },
	}
	must(c.Register("one", func(call *v8.Call) { call.RetInt(int64(table[call.Int(0)](int(call.Int(1))))) }))
	must(c.Register("batch", func(call *v8.Call) {
		b := call.View(0)
		n := int(call.Int(1))
		// layout: int32 pairs (op,id) in place; results overwrite pair slot 0.
		p := unsafeInt32s(b)
		for k := 0; k < n; k++ {
			p[2*k] = table[p[2*k]&3](int(p[2*k+1]))
		}
		call.RetUndefined()
	}))
	_, err := c.Eval(`var N=100000, ops=new Int32Array(N), ids=new Int32Array(N), r=7;
for (var i=0;i<N;i++){ r=(r*1103515245+12345)&0x7fffffff; ops[i]=(r>>8)&3; r=(r*1103515245+12345)&0x7fffffff; ids[i]=(r>>8)%5000 }`, "s.js")
	must(err)
	run(c, "100k int ops, 1 crossing each", `var s=0; for (var i=0;i<N;i++){ s+=one(ops[i],ids[i]) } s`)
	for _, n := range []int{4, 16, 64, 256} {
		run(c, fmt.Sprintf("100k int ops, %d per crossing (shared buf)", n), fmt.Sprintf(
			`var s=0, B=new Int32Array(%d); for (var i=0;i<N;i+=%d){ for (var k=0;k<%d;k++){ B[2*k]=ops[i+k]; B[2*k+1]=ids[i+k] } batch(B,%d); for (var k=0;k<%d;k++) s+=B[2*k] } s`, n*2, n, n, n, n))
	}
	// JS-side immutable-getter cache: cost of a hit vs a crossing.
	run(c, "100k nodeType via JS typed-array cache hit", `var C=new Int32Array(5000).fill(-1), s=0; for (var i=0;i<N;i++){ var id=ids[i], v=C[id]; if(v<0){v=one(0,id);C[id]=v} s+=v } s`)
}

func benchCPU() {
	_, c := newCtx(v8.Options{})
	tests := []struct{ name, src string }{
		{"fact10 x1M", `function fact(n){return n<=1?1:n*fact(n-1)} (function(){var s=0; for (var i=0;i<1000000;i++) s+=fact(10); return s})()`},
		{"fib(30)", `function fib(n){return n<2?n:fib(n-1)+fib(n-2)} fib(30)`},
		{"int loop 20M (global var)", `var s=0; for (var i=0;i<20000000;i++){ s=(s+i*3)|0 } s`},
		{"int loop 20M (local)", `(function(){var s=0; for (var i=0;i<20000000;i++){ s=(s+i*3)|0 } return s})()`},
		{"array/obj/string 300k", `var a=[]; for (var i=0;i<300000;i++) a.push({id:i,name:'n'+i,v:i%7});
 var r=a.filter(function(o){return o.v>2}).map(function(o){return o.name.length+o.id}).reduce(function(x,y){return x+y},0);
 var m={}; for (var i=0;i<100000;i++) m['k'+(i%5000)]=(m['k'+(i%5000)]||0)+1; r+Object.keys(m).length`},
		{"regexp+split 200k", `var t=''; for (var i=0;i<20000;i++) t+='abc-'+i+' def_'+(i*7)+';'; var c=0; for (var k=0;k<10;k++){ c+=t.match(/\d+/g).length + t.split(';').length + t.replace(/[a-c]+/g,'x').length } c`},
	}
	for _, t := range tests {
		run(c, t.name, t.src)
	}
}

func benchParse(files []string) {
	for _, f := range files {
		b, err := os.ReadFile(f)
		must(err)
		src := string(b)
		var lazy, eager, mk, use []time.Duration
		var cache []byte
		for i := 0; i < runs; i++ {
			iso, c := newCtx(v8.Options{})
			t0 := time.Now()
			must(c.CompileOnly(src, "x.js", false))
			lazy = append(lazy, time.Since(t0))
			iso.Close()
			iso, c = newCtx(v8.Options{})
			t0 = time.Now()
			must(c.CompileOnly(src, "x.js", true))
			eager = append(eager, time.Since(t0))
			iso.Close()
			iso, c = newCtx(v8.Options{})
			t0 = time.Now()
			cache, err = c.CompileCache(src, "x.js")
			must(err)
			mk = append(mk, time.Since(t0))
			iso.Close()
			iso, c = newCtx(v8.Options{})
			t0 = time.Now()
			_, rej, _ := c.EvalCached(src, "x.js", cache) // includes running top level
			use = append(use, time.Since(t0))
			if rej {
				fmt.Println("cache rejected!")
			}
			iso.Close()
		}
		fmt.Printf("--- %s (%d KB, code cache %d KB)\n", f, len(src)>>10, len(cache)>>10)
		report("parse+compile lazy (fresh isolate)", lazy)
		report("parse+compile eager", eager)
		report("eager compile + serialize cache", mk)
		report("run top level from code cache", use)
	}
}

func benchCreate(prelude string) {
	var base, withCtx, snapIso, snapCtx []time.Duration
	for i := 0; i < runs; i++ {
		t0 := time.Now()
		iso, err := v8.NewIsolate(v8.Options{})
		must(err)
		base = append(base, time.Since(t0))
		t0 = time.Now()
		_, err = iso.NewContext()
		must(err)
		withCtx = append(withCtx, time.Since(t0))
		iso.Close()
	}
	report("isolate create (no snapshot)", base)
	report("context create (no snapshot)", withCtx)
	if prelude == "" {
		return
	}
	b, err := os.ReadFile(prelude)
	must(err)
	// Cost of evaluating the prelude in a fresh context without a snapshot.
	var ev []time.Duration
	for i := 0; i < runs; i++ {
		iso, c := newCtx(v8.Options{})
		t0 := time.Now()
		_, err := c.Eval(string(b), "prelude.js")
		ev = append(ev, time.Since(t0))
		if err != nil {
			fmt.Println("prelude eval error:", err)
			break
		}
		iso.Close()
	}
	report("eval prelude in new context (no snapshot)", ev)
	for _, keep := range []bool{false, true} {
		snap, err := v8.CreateSnapshot(string(b), "prelude.js", keep)
		if err != nil {
			fmt.Println("snapshot error:", err)
			return
		}
		snapIso, snapCtx = nil, nil
		for i := 0; i < runs; i++ {
			t0 := time.Now()
			iso, err := v8.NewIsolate(v8.Options{Snapshot: snap})
			must(err)
			snapIso = append(snapIso, time.Since(t0))
			t0 = time.Now()
			_, err = iso.NewContext()
			must(err)
			snapCtx = append(snapCtx, time.Since(t0))
			iso.Close()
		}
		fmt.Printf("snapshot keep_code=%v blob %d KB\n", keep, len(snap)>>10)
		report("  isolate create (with snapshot)", snapIso)
		report("  context create from snapshot", snapCtx)
	}
}

func benchMem() {
	runtime.GC()
	r0 := rssKB()
	const n = 40
	var isos []*v8.Isolate
	for i := 0; i < n; i++ {
		iso, c := newCtx(v8.Options{})
		_, _ = c.Eval("var x = 1", "m.js")
		isos = append(isos, iso)
	}
	r1 := rssKB()
	fmt.Printf("RSS per isolate+context (n=%d): %.2f MB\n", n, float64(r1-r0)/1024/n)
	st := isos[0].HeapStats()
	fmt.Printf("heap stats of an empty isolate: total %d KB used %d KB physical %d KB\n", st.Total>>10, st.Used>>10, st.Physical>>10)
	iso := isos[0]
	r2 := rssKB()
	for i := 0; i < 40; i++ {
		_, err := iso.NewContext()
		must(err)
	}
	fmt.Printf("RSS per extra context in same isolate: %.2f MB\n", float64(rssKB()-r2)/1024/40)
}

// benchMemPrelude: RSS per isolate that has the DOM prelude loaded, with and
// without a startup snapshot.
func benchMemPrelude(file string) {
	b, err := os.ReadFile(file)
	must(err)
	const n = 20
	measure := func(label string, o v8.Options, eval bool) {
		runtime.GC()
		r0 := rssKB()
		var keep []*v8.Isolate
		for i := 0; i < n; i++ {
			iso, c := newCtx(o)
			if eval {
				_, err := c.Eval(string(b), "prelude.js")
				must(err)
			}
			keep = append(keep, iso)
		}
		fmt.Printf("RSS per isolate, prelude %-22s %.2f MB\n", label, float64(rssKB()-r0)/1024/n)
		for _, k := range keep {
			k.Close()
		}
	}
	measure("evaluated (no snapshot)", v8.Options{}, true)
	snap, err := v8.CreateSnapshot(string(b), "prelude.js", true)
	must(err)
	measure("from snapshot", v8.Options{Snapshot: snap}, false)
}

func main() {
	must(v8.Load())
	fmt.Println("V8", v8.Version(), "lib", v8.LibPath())
	runtime.LockOSThread()
	switch os.Args[1] {
	case "host":
		benchHost()
	case "mix":
		benchMix()
	case "batch":
		benchBatch()
	case "cpu":
		benchCPU()
	case "parse":
		benchParse(os.Args[2:])
	case "create":
		p := ""
		if len(os.Args) > 2 {
			p = os.Args[2]
		}
		benchCreate(p)
	case "mem":
		benchMem()
		if len(os.Args) > 2 {
			benchMemPrelude(os.Args[2])
		}
	}
}
