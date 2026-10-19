package engine

// sab.go: SharedArrayBuffer and Atomics across script VMs.
//
// Every page, frame and Worker has its own QuickJS runtime, so a buffer that two of them can see has to live
// outside both. The memory of a SharedArrayBuffer made here is Go-owned (an 8-byte aligned []uint64 the garbage
// collector never moves), registered in an agent cluster, and wrapped by each VM that receives it in an
// ArrayBuffer-class object over the same bytes (JS_NewArrayBuffer with is_shared and no free function; the same
// reflect/unsafe bridge as wasm_qjs.go). Typed arrays on it are plain QuickJS typed arrays, so loads, stores and
// Atomics.add/and/compareExchange/... are QuickJS's own (they compile to real atomic instructions) and are
// visible across goroutines at once. What QuickJS cannot do across runtimes with its own state is wait and wake,
// so Atomics.wait, notify and waitAsync are implemented here, over one process-wide wait queue keyed by address.
//
// Agent cluster. A SharedArrayBuffer can only be posted to the document that owns it, the workers it started and
// their workers. The cluster is created by the page VM and handed to workers by bgWorker.boot; it frees its
// memory when the page VM closes. A buffer id coming from anywhere else is refused.
//
// Gating (sab_prelude.js): the SharedArrayBuffer constructor is a global only in cross-origin isolated
// documents and workers (coi.go), as in Chromium. Shared WebAssembly memories (wasm threads) may hand out an
// instance anyway; posting one needs isolation too.

import (
	_ "embed"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"modernc.org/quickjs"
)

const (
	sabMaxBytes   = 1 << 30 // one SharedArrayBuffer
	sabClusterCap = 2 << 30 // all buffers of one agent cluster
)

// sabWasm is implemented by shared WebAssembly memories: waiting and waking on their addresses goes through the
// memory's own wait queue, so JavaScript Atomics and wasm memory.atomic.wait/notify see each other.
type sabWasm interface {
	wait(off uint32, size int, exp int64, timeoutNs int64) int // 0 woken, 1 value differed, 2 timed out
	notify(off uint32, count uint32) uint32
}

type sabRegion struct {
	id    uint64
	cl    *agentCluster
	words []uint64 // owner of plain buffers (nil for adopted wasm memory)
	data  []byte   // the whole region
	base  unsafe.Pointer
	wasm  sabWasm
}

type agentCluster struct {
	mu      sync.Mutex
	next    uint64
	regions map[uint64]*sabRegion
	objs    map[uint64]any // shared WebAssembly memories and module bytes handed to workers (wasm_threads.go)
	bytes   int64
	dead    bool
}

func (c *agentCluster) putObj(v any) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	if c.objs == nil {
		c.objs = map[uint64]any{}
	}
	c.objs[c.next] = v
	return c.next
}

func (c *agentCluster) getObj(id uint64) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.objs[id]
}

func newAgentCluster() *agentCluster { return &agentCluster{regions: map[uint64]*sabRegion{}} }

var sabByBase sync.Map // unsafe.Pointer (region base) -> *sabRegion

func (c *agentCluster) add(r *sabRegion) {
	c.next++
	r.id = c.next
	r.cl = c
	c.regions[r.id] = r
	sabByBase.Store(r.base, r)
}

// newRegion allocates n zeroed bytes. ok is false when the cluster is gone or over its cap.
func (c *agentCluster) newRegion(n int) (*sabRegion, bool) {
	if n < 0 || n > sabMaxBytes {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead || c.bytes+int64(n) > sabClusterCap {
		return nil, false
	}
	w := make([]uint64, (n+7)/8+1)
	r := &sabRegion{words: w, data: unsafe.Slice((*byte)(unsafe.Pointer(&w[0])), n), base: unsafe.Pointer(&w[0])}
	c.bytes += int64(n)
	c.add(r)
	return r, true
}

// adopt registers memory owned elsewhere (a shared WebAssembly memory). data is the whole reserved region.
func (c *agentCluster) adopt(data []byte, w sabWasm) (*sabRegion, bool) {
	if len(data) == 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead {
		return nil, false
	}
	base := unsafe.Pointer(unsafe.SliceData(data))
	if v, ok := sabByBase.Load(base); ok {
		if r := v.(*sabRegion); r.cl == c {
			return r, true
		}
	}
	r := &sabRegion{data: data, base: base, wasm: w}
	c.add(r)
	return r, true
}

func (c *agentCluster) get(id uint64) *sabRegion {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.regions[id]
}

func (c *agentCluster) release() {
	c.mu.Lock()
	rs := c.regions
	c.regions, c.objs, c.dead = map[uint64]*sabRegion{}, nil, true
	c.mu.Unlock()
	for _, r := range rs {
		sabByBase.Delete(r.base)
	}
}

// ---- address-keyed wait queue --------------------------------------------------------------------------

type sabWaiter struct {
	ch   chan struct{}
	addr uintptr
}

type sabQueue struct {
	mu sync.Mutex
	w  map[uintptr][]*sabWaiter
}

var sabHub = &sabQueue{w: map[uintptr][]*sabWaiter{}}

func sabLoad(p unsafe.Pointer, size int) int64 {
	if size == 8 {
		return atomic.LoadInt64((*int64)(p))
	}
	return int64(atomic.LoadInt32((*int32)(p)))
}

// enqueue checks the value and, when it equals exp, appends a waiter, atomically with respect to notify.
func (q *sabQueue) enqueue(p unsafe.Pointer, size int, exp int64) *sabWaiter {
	addr := uintptr(p)
	q.mu.Lock()
	defer q.mu.Unlock()
	if sabLoad(p, size) != exp {
		return nil
	}
	w := &sabWaiter{ch: make(chan struct{}), addr: addr}
	q.w[addr] = append(q.w[addr], w)
	return w
}

func (q *sabQueue) remove(w *sabWaiter) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	l := q.w[w.addr]
	for i, x := range l {
		if x == w {
			l = append(l[:i:i], l[i+1:]...)
			if len(l) == 0 {
				delete(q.w, w.addr)
			} else {
				q.w[w.addr] = l
			}
			return true
		}
	}
	return false
}

// notify wakes up to count waiters at addr in the order they started waiting.
func (q *sabQueue) notify(addr uintptr, count int64) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	l := q.w[addr]
	n := int64(len(l))
	if count < n {
		n = count
	}
	for _, w := range l[:n] {
		close(w.ch)
	}
	if int(n) == len(l) {
		delete(q.w, addr)
	} else {
		q.w[addr] = append([]*sabWaiter(nil), l[n:]...)
	}
	return n
}

// ---- per-VM state ---------------------------------------------------------------------------------------

type vmSAB struct {
	mu      sync.Mutex
	cancel  chan struct{}
	closed  bool
	pending map[int64]*sabWaiter
	next    int64
}

func (j *jsVM) sabState() *vmSAB {
	j.clMu.Lock()
	defer j.clMu.Unlock()
	if j.sabv == nil {
		s := &vmSAB{cancel: make(chan struct{}), pending: map[int64]*sabWaiter{}}
		j.sabv = s
		j.onClose(s.shutdown)
	}
	return j.sabv
}

func (s *vmSAB) shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.cancel)
	ws := s.pending
	s.pending = map[int64]*sabWaiter{}
	s.mu.Unlock()
	for _, w := range ws {
		sabHub.remove(w)
	}
}

// sabInterrupt wakes a VM that is blocked in Atomics.wait and makes its script stop (worker termination).
func (j *jsVM) sabInterrupt() {
	j.clMu.Lock()
	s := j.sabv
	j.clMu.Unlock()
	if s == nil {
		return
	}
	s.shutdown()
	j.jr.interrupt()
}

// agentCluster returns the VM's agent cluster, creating it (owned by this VM) on first use.
func (j *jsVM) agentCluster() *agentCluster {
	j.clMu.Lock()
	defer j.clMu.Unlock()
	if j.cluster == nil {
		c := newAgentCluster()
		j.cluster = c
		j.onClose(c.release)
	}
	return j.cluster
}

func (j *jsVM) joinCluster(c *agentCluster) {
	j.clMu.Lock()
	j.cluster = c
	j.clMu.Unlock()
}

// ---- natives -------------------------------------------------------------------------------------------

func toInt(v any) int64 { return wasmToI64(v) }

func toFloat(v any) float64 { return wasmToF64(v) }

// sabTarget resolves (buffer, byte offset) of a SharedArrayBuffer value to its region and absolute pointer.
func (j *jsVM) sabTarget(raw qjsRaw, v quickjs.Value, off int64, size int) (*sabRegion, unsafe.Pointer, bool) {
	b, ok := raw.bufferBytes(&v)
	if !ok || len(b) == 0 || off < 0 || off+int64(size) > int64(len(b)) {
		return nil, nil, false
	}
	base := unsafe.Pointer(unsafe.SliceData(b))
	var rg *sabRegion
	if r, ok := sabByBase.Load(base); ok {
		rg = r.(*sabRegion)
	}
	return rg, unsafe.Add(base, off), true
}

func sabTimeout(ms float64) time.Duration {
	if ms != ms || ms > 1e12 {
		return -1
	}
	if ms < 0 {
		return 0
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// wait blocks the calling VM. cancel ends the wait when the VM is torn down.
func (j *jsVM) sabWait(rg *sabRegion, p unsafe.Pointer, size int, exp int64, to time.Duration) string {
	if rg != nil && rg.wasm != nil {
		off := uint32(uintptr(p) - uintptr(rg.base))
		switch rg.wasm.wait(off, size, exp, int64(to)) {
		case 1:
			return "not-equal"
		case 2:
			return "timed-out"
		}
		return "ok"
	}
	w := sabHub.enqueue(p, size, exp)
	if w == nil {
		return "not-equal"
	}
	cancel := j.sabState().cancel
	if to < 0 {
		select {
		case <-w.ch:
			return "ok"
		case <-cancel:
			sabHub.remove(w)
			return "ok"
		}
	}
	t := time.NewTimer(to)
	defer t.Stop()
	select {
	case <-w.ch:
		return "ok"
	case <-t.C:
		if !sabHub.remove(w) {
			return "ok" // woken at the same moment
		}
		return "timed-out"
	case <-cancel:
		sabHub.remove(w)
		return "ok"
	}
}

func (j *jsVM) sabNotify(rg *sabRegion, p unsafe.Pointer, count int64) int64 {
	n := sabHub.notify(uintptr(p), count)
	if rg != nil && rg.wasm != nil && n < count {
		off := uint32(uintptr(p) - uintptr(rg.base))
		c := count - n
		if c > 1<<31 {
			c = 1 << 31
		}
		n += int64(rg.wasm.notify(off, uint32(c)))
	}
	return n
}

func (j *jsVM) sabRegisterNatives() {
	if j.vm == nil {
		return // V8 runtime: no agent cluster (SharedArrayBuffer is V8's own, without workers)
	}
	if wasmQJS() != nil {
		return
	}
	vm := j.vm
	raw := rawOf(vm)
	reg := func(name string, f any) { _ = vm.RegisterFunc("__swSab_"+name, f, false) }

	reg("new", func(n any) quickjs.Value {
		cl := j.agentCluster()
		r, ok := cl.newRegion(int(toInt(n)))
		if !ok {
			return quickjs.UndefinedValue
		}
		return raw.newSharedBuffer(vm, r.data, r.base)
	})
	reg("id", func(v quickjs.Value) any {
		b, ok := raw.bufferBytes(&v)
		if !ok || len(b) == 0 {
			return -1
		}
		r, ok := sabByBase.Load(unsafe.Pointer(unsafe.SliceData(b)))
		if !ok || r.(*sabRegion).cl != j.agentCluster() {
			return -1
		}
		return int(r.(*sabRegion).id)
	})
	reg("open", func(id, n any) quickjs.Value {
		r := j.agentCluster().get(uint64(toInt(id)))
		if r == nil {
			return quickjs.UndefinedValue
		}
		l := int(toInt(n))
		if l < 0 || l > len(r.data) {
			l = len(r.data)
		}
		return j.sabBuffer(raw, r, l)
	})
	reg("wait", func(v quickjs.Value, off, size, exp, timeoutMs any) any {
		sz := int(toInt(size))
		rg, p, ok := j.sabTarget(raw, v, toInt(off), sz)
		if !ok {
			return "invalid"
		}
		e, _ := strconv.ParseInt(anyString(exp), 10, 64)
		if sz == 4 && (e < math.MinInt32 || e > math.MaxInt32) {
			e = toInt32Range(e)
		}
		return j.sabWait(rg, p, sz, e, sabTimeout(toFloat(timeoutMs)))
	})
	reg("notify", func(v quickjs.Value, off, size, count any) any {
		rg, p, ok := j.sabTarget(raw, v, toInt(off), int(toInt(size)))
		if !ok {
			return 0
		}
		c := toFloat(count)
		n := int64(1) << 62
		if c == c && c < 1e18 {
			n = int64(c)
		}
		return int(j.sabNotify(rg, p, n))
	})
	reg("waitAsync", func(v quickjs.Value, off, size, exp, timeoutMs any) any {
		sz := int(toInt(size))
		rg, p, ok := j.sabTarget(raw, v, toInt(off), sz)
		if !ok {
			return []any{"invalid", 0}
		}
		e, _ := strconv.ParseInt(anyString(exp), 10, 64)
		if sz == 4 && (e < math.MinInt32 || e > math.MaxInt32) {
			e = toInt32Range(e)
		}
		return j.sabWaitAsync(rg, p, sz, e, sabTimeout(toFloat(timeoutMs)))
	})
}

func anyString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatInt(int64(x), 10)
	}
	return "0"
}

// sabBuffer makes a SharedArrayBuffer object of length n over the region.
func (j *jsVM) sabBuffer(raw qjsRaw, r *sabRegion, n int) quickjs.Value {
	return raw.newSharedBuffer(j.vm, r.data[:n:n], r.base)
}

// sabWaitAsync registers the waiter synchronously (so a later notify cannot miss it) and resolves it from the
// event loop. Returns [status, token].
func (j *jsVM) sabWaitAsync(rg *sabRegion, p unsafe.Pointer, size int, exp int64, to time.Duration) any {
	s := j.sabState()
	if rg != nil && rg.wasm != nil {
		// The wasm memory keeps its own queue; the value check and the wait are one operation there, so a
		// wake that arrives before the goroutine is waiting shows up as a changed value.
		if sabLoad(p, size) != exp {
			return []any{"not-equal", 0}
		}
		if to == 0 {
			return []any{"timed-out", 0}
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return []any{"timed-out", 0}
		}
		s.next++
		tok := s.next
		s.mu.Unlock()
		off := uint32(uintptr(p) - uintptr(rg.base))
		go func() {
			res := "ok"
			switch rg.wasm.wait(off, size, exp, int64(to)) {
			case 2:
				res = "timed-out"
			}
			j.sabDeliver(tok, res)
		}()
		return []any{"waiting", int(tok)}
	}
	w := sabHub.enqueue(p, size, exp)
	if w == nil {
		return []any{"not-equal", 0}
	}
	if to == 0 {
		sabHub.remove(w)
		return []any{"timed-out", 0}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		sabHub.remove(w)
		return []any{"timed-out", 0}
	}
	s.next++
	tok := s.next
	s.pending[tok] = w
	s.mu.Unlock()
	go func() {
		var tc <-chan time.Time
		if to > 0 {
			t := time.NewTimer(to)
			defer t.Stop()
			tc = t.C
		}
		select {
		case <-w.ch:
			j.sabDeliver(tok, "ok")
		case <-tc:
			if !sabHub.remove(w) {
				j.sabDeliver(tok, "ok")
				return
			}
			j.sabDeliver(tok, "timed-out")
		case <-s.cancel:
		}
	}()
	return []any{"waiting", int(tok)}
}

func (j *jsVM) sabDeliver(tok int64, res string) {
	s := j.sabState()
	s.mu.Lock()
	delete(s.pending, tok)
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	j.rt().push(asyncEvent{kind: "sab", id: tok, data: res})
	if j.tab != nil {
		j.tab.wake()
	}
}

//go:embed sab_prelude.js.z
var sabPreludeJSZ string

func init() {
	registerPreludeZ("sab", sabPreludeJSZ)
	registerHostExtension("sab", func(j *jsVM) map[string]hostFn {
		j.sabRegisterNatives()
		return nil
	})
	registerAsyncHandler("sab", func(j *jsVM, e asyncEvent) {
		if _, err := j.call("__swSabEvent", e.id, e.data); err != nil && j.tab != nil {
			j.tab.LogConsole("error", jsErrorText(err, "Atomics.waitAsync"))
		}
		j.jobs()
	})
}

// toInt32Range applies ECMAScript ToInt32 wrapping to an int64 arithmetically (no narrowing conversion), so a
// 32-bit wait compares against the same value JS would store in an Int32Array.
func toInt32Range(e int64) int64 {
	e %= 1 << 32
	if e < 0 {
		e += 1 << 32
	}
	if e >= 1<<31 {
		e -= 1 << 32
	}
	return e
}
