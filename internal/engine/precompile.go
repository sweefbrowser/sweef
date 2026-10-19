package engine

// precompile.go: scripts compiled to bytecode on other cores while the page's thread is busy.
//
// QuickJS compiles a script completely before running it, on the page's one thread. On a cold load that is
// most of the cost of a module graph: github.com's issue page pulls 156 modules, and compiling them was 1.9 s
// of the 2.6 s its entry module took. A browser compiles off the main thread as the bytes arrive; so does
// this. When a script or module body arrives from the network (the preload scanner, the module prefetcher),
// a worker with its own QuickJS runtime compiles it and leaves the bytecode in the engine's code cache under
// the key the runner looks up (runCached, runModule, the module loader). The runner then reads bytecode
// instead of parsing; a script it reaches while its compile is still running waits for that compile rather
// than starting a second one. Reading bytecode is a fraction of compiling it.

import (
	"runtime"
	"sync"

	"modernc.org/quickjs"
)

// precompileMin is the smallest source worth a background compile.
const precompileMin = 4 << 10

type pcJob struct {
	key    [32]byte
	src    string
	name   string // module name (its URL); "" for a classic script
	module bool
	done   chan struct{}
	bc     []byte
	// state under precompiler.mu: 0 queued, 1 compiling, 2 taken back by the page (it compiles it itself:
	// waiting behind the rest of the queue would be slower)
	state int
}

type precompiler struct {
	off      bool // the V8 runtime does not use QuickJS bytecode: nothing is compiled ahead of time
	code     *codeCache
	once     sync.Once
	jobs     chan *pcJob
	mu       sync.Mutex
	inflight map[[32]byte]*pcJob
	closed   chan struct{}
}

func newPrecompiler(code *codeCache) *precompiler {
	return &precompiler{code: code, inflight: map[[32]byte]*pcJob{}, closed: make(chan struct{})}
}

func (p *precompiler) start() {
	n := runtime.NumCPU() - 1
	if n > 3 {
		n = 3
	}
	if n < 1 {
		n = 1
	}
	p.jobs = make(chan *pcJob, 256)
	for i := 0; i < n; i++ {
		go p.worker()
	}
}

// classicKey and moduleKey are the code cache keys runCached and runModule use.
func classicKey(src string) [32]byte { return codeKey(src) }
func moduleKey(name, src string) [32]byte {
	return codeKey("\x00module\x00" + name + "\x00" + src)
}

// submitClassic queues a classic script's compile.
func (p *precompiler) submitClassic(src string) {
	if p == nil || p.off || len(src) < precompileMin {
		return
	}
	p.submit(&pcJob{key: classicKey(src), src: src})
}

// submitModule queues the compile of the module name whose body is raw (as fetched).
func (p *precompiler) submitModule(name, raw string) {
	if p == nil || p.off || len(raw) < precompileMin {
		return
	}
	src := withImportMetaURL(raw, name)
	p.submit(&pcJob{key: moduleKey(name, src), src: src, name: name, module: true})
}

func (p *precompiler) submit(j *pcJob) {
	if p.code.get(j.key) != nil {
		return
	}
	p.once.Do(p.start)
	p.mu.Lock()
	if _, ok := p.inflight[j.key]; ok {
		p.mu.Unlock()
		return
	}
	j.done = make(chan struct{})
	p.inflight[j.key] = j
	p.mu.Unlock()
	select {
	case p.jobs <- j:
	default: // the queue is full: the page compiles this one itself
		p.finish(j, nil)
	}
}

func (p *precompiler) finish(j *pcJob, bc []byte) {
	if len(bc) > 0 {
		p.code.put(j.key, bc)
	}
	j.bc = bc
	p.mu.Lock()
	delete(p.inflight, j.key)
	p.mu.Unlock()
	close(j.done)
}

// wait returns the bytecode of a compile that is queued or running for key, after it finishes; nil when there
// is none or it failed (the caller compiles itself; a syntax error is reported from there).
func (p *precompiler) wait(key [32]byte) []byte {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	j := p.inflight[key]
	if j != nil && j.state == 0 {
		j.state = 2
		delete(p.inflight, key)
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	if j == nil {
		return nil
	}
	<-j.done
	return j.bc
}

func (p *precompiler) close() {
	if p == nil {
		return
	}
	select {
	case <-p.closed:
	default:
		close(p.closed)
	}
}

// pcRecycle is how many compiles a worker VM serves before it is replaced: the stand-in modules a module
// compile registers stay in its runtime.
const pcRecycle = 300

func (p *precompiler) worker() {
	var vm *quickjs.VM
	used := 0
	defer func() {
		if vm != nil {
			vm.Close()
		}
	}()
	for {
		var j *pcJob
		select {
		case <-p.closed:
			return
		case j = <-p.jobs:
		}
		p.mu.Lock()
		taken := j.state == 2
		j.state = 1
		p.mu.Unlock()
		if taken {
			continue
		}
		if vm == nil || used >= pcRecycle {
			if vm != nil {
				vm.Close()
			}
			var err error
			if vm, err = quickjs.NewVM(); err != nil {
				vm = nil
				p.finish(j, nil)
				continue
			}
			// static imports resolve while a module compiles: empty stand-ins do (the bytecode keeps only the
			// specifiers; the page's own loader supplies the real modules when it reads it)
			vm.SetModuleLoader(func(*quickjs.VM, string) (string, error) { return "", nil }, nil)
			used = 0
		}
		used++
		var bc []byte
		if j.module {
			bc, _ = compileModuleNamed(vm, j.src, j.name)
		} else {
			bc, _ = vm.Compile(j.src, quickjs.EvalGlobal)
		}
		p.finish(j, bc)
	}
}
