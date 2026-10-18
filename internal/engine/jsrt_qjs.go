package engine

// jsrt_qjs.go: the QuickJS implementation of jsRuntime. The bodies are the original jsvm.go code (qjsRun,
// qjsRunCached, ... in jsvm.go), reached through the interface without any change of behaviour.

import (
	"encoding/json"
	"strconv"
	"strings"

	"modernc.org/quickjs"
)

type qjsRT struct {
	vm *quickjs.VM
}

func (q *qjsRT) kind() string { return JSEngineQuickJS }

func (q *qjsRT) run(j *jsVM, src, name string) (any, error)       { return j.qjsRun(src, name) }
func (q *qjsRT) runCached(j *jsVM, src, name string) (any, error) { return j.qjsRunCached(src, name) }
func (q *qjsRT) runModule(j *jsVM, src, name string) error        { return j.qjsRunModule(src, name) }
func (q *qjsRT) installModuleLoader(j *jsVM, docURL string)       { j.qjsInstallModuleLoader(docURL) }
func (q *qjsRT) call(j *jsVM, fn string, args ...any) (any, error) {
	return j.qjsCall(fn, args...)
}
func (q *qjsRT) callLocked(fn string, args ...any) (any, error) { return q.vm.Call(fn, args...) }
func (q *qjsRT) jobs(j *jsVM)                                   { j.qjsJobs() }
func (q *qjsRT) evaluate(j *jsVM, expr string) (json.RawMessage, error) {
	return j.qjsEvaluate(expr)
}
func (q *qjsRT) interrupt() { q.vm.Interrupt() }
func (q *qjsRT) closeVM() {
	q.vm.SetModuleLoader(nil, nil)
	q.vm.Close()
}
func (q *qjsRT) stats() jsHeapStats { return jsHeapStats{Engine: JSEngineQuickJS} }

// installHost registers the one Go callback of the VM and builds __sw on top of it. One callback per VM, not
// one per host function: modernc quickjs allows 32767 registered callbacks in the whole process, and with
// several hundred host functions a few dozen live VMs (pool, tabs, frames, workers, UI pages) exhausted it
// ("too many registered functions"). __sw.name is a JS wrapper that passes the function's index to the dispatcher.
func (q *qjsRT) installHost(j *jsVM, names []string, dispatch hostFn) error {
	if err := q.vm.RegisterHostFunc("__sw_call", dispatch); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString("globalThis.__sw = (function (c) { var o = {}, n = [")
	for i, x := range names {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(strconv.Quote(x))
	}
	// Fixed-arity fast paths: a rest parameter plus spread allocates an array and goes through apply on every host
	// call in QuickJS. arguments.length keeps the exact argument count the host sees, so behaviour is unchanged.
	sb.WriteString("]; n.forEach(function (k, i) { o[k] = function (a0, a1, a2, a3) { switch (arguments.length) { case 0: return c(i); case 1: return c(i, a0); case 2: return c(i, a0, a1); case 3: return c(i, a0, a1, a2); case 4: return c(i, a0, a1, a2, a3); } var r = [i]; for (var j = 0; j < arguments.length; j++) r.push(arguments[j]); return c.apply(null, r); }; }); return o; })(__sw_call); delete globalThis.__sw_call;")
	if j.cv != nil {
		if err := j.cv.registerBufferFns(); err != nil {
			return err
		}
		sb.WriteString("__sw.cvGetBuf=__swCvGetBuf;__sw.cvPutBuf=__swCvPutBuf;delete globalThis.__swCvGetBuf;delete globalThis.__swCvPutBuf;")
	}
	_, err := q.vm.Eval(sb.String(), quickjs.EvalGlobal)
	return err
}
