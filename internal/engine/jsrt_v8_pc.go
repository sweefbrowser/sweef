package engine

// jsrt_v8_pc.go: V8 code cache for fetched scripts, produced off the page's thread.
//
// A script runs the first time without a cache (V8 compiles lazily as it goes, which is fast). A background worker
// then compiles it eagerly in an isolate of its own and stores V8's code cache next to the source's hash; the
// next visit hands the blob to the compiler and skips parsing and compiling. The worker is a single goroutine
// locked to its thread (an isolate is thread-bound, see jsrt_v8.go), idle when there is nothing to do.

import (
	"runtime"
	"sync"

	"github.com/sweefbrowser/sweef/internal/v8shim"
)

type v8PcJob struct {
	src, name string
	key       [32]byte
}

type v8Precompiler struct {
	code *codeCache
	jobs chan v8PcJob
	once sync.Once
	mu   sync.Mutex
	seen map[[32]byte]bool
	quit chan struct{}
}

func newV8Precompiler(code *codeCache) *v8Precompiler {
	return &v8Precompiler{code: code, jobs: make(chan v8PcJob, 128), seen: map[[32]byte]bool{}, quit: make(chan struct{})}
}

// submit queues a compile of src (dropped when the queue is full or the script was queued before).
func (p *v8Precompiler) submit(src, name string, key [32]byte) {
	if p == nil || len(src) > 8<<20 {
		return
	}
	p.mu.Lock()
	if p.seen[key] {
		p.mu.Unlock()
		return
	}
	p.seen[key] = true
	p.mu.Unlock()
	p.once.Do(func() { go p.worker() })
	select {
	case p.jobs <- v8PcJob{src, name, key}:
	default:
	}
}

func (p *v8Precompiler) close() {
	select {
	case <-p.quit:
	default:
		close(p.quit)
	}
}

func (p *v8Precompiler) worker() {
	runtime.LockOSThread()
	var iso *v8shim.Isolate
	var ctx *v8shim.Context
	n := 0
	drop := func() {
		if ctx != nil {
			ctx.Close()
		}
		if iso != nil {
			iso.Close()
		}
		iso, ctx, n = nil, nil, 0
	}
	defer drop()
	for {
		var job v8PcJob
		select {
		case <-p.quit:
			return
		case job = <-p.jobs:
		}
		if iso == nil || n >= 24 { // a fresh isolate now and then: V8 keeps compiled scripts alive per isolate
			drop()
			var err error
			o := v8shim.Options{}
			if b := v8Active.Load(); b != nil { // the same snapshot as the pages' isolates (see v8shim.Options)
				o.Snapshot = *b
			}
			if iso, err = v8shim.NewIsolate(o); err != nil {
				return
			}
			if ctx, err = iso.NewContext(); err != nil {
				drop()
				return
			}
		}
		n++
		if b, err := ctx.CompileCache(job.src, job.name); err == nil && len(b) > 0 {
			p.code.put(job.key, v8CacheWrap(b))
		}
	}
}
