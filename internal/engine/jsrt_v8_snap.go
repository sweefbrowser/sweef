package engine

// jsrt_v8_snap.go: the V8 startup snapshot of the prelude.
//
// QuickJS loads the prelude from bytecode into every new VM (~3 ms, vmPool keeps a few built ahead). V8 does
// better: the whole initialised heap of a context that has run the prelude is serialised once into a startup
// snapshot, and a new isolate maps it back in a few milliseconds. The snapshot is built the first time a V8 VM is
// needed and cached on disk in the per-user cache directory, keyed by the library (size, mtime, V8 version), the
// prelude and the host function table, so a stale blob can never be picked up. When it cannot be built or
// loaded, VMs boot by evaluating the prelude source instead (slower, same behaviour).
//
// A snapshot cannot hold Go callbacks. The snapshotted script therefore creates __sw (the per-VM host object)
// around a variable `c` that stands for the dispatcher; every VM binds `c` to its own native function right after
// its context is created (hostBindJS / __swBind).

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sweefbrowser/sweef/internal/v8shim"
	"github.com/sweefbrowser/sweef/internal/zpack"
	"modernc.org/quickjs"
)

// v8SkipPrelude are the registered preludes that the V8 runtime does not load: they implement, on top of
// QuickJS-only natives, what V8 has built in (WebAssembly, SharedArrayBuffer and Atomics).
var v8SkipPrelude = map[string]bool{"wasm": true, "sab": true}

// hostBindJS builds __sw. names[i] is the host function with index i; calls go through the variable c, which
// __swBind(fn) points at the VM's native dispatcher. Fixed-arity fast paths avoid the rest-parameter array
// (arguments.length keeps the exact argument count the host sees).
func hostBindJS(names []string, rec []v8Rec) string {
	var sb strings.Builder
	sb.WriteString("globalThis.__sw = (function () { var c = ")
	if rec == nil {
		sb.WriteString("function () { throw new Error('host call before the VM was bound'); }")
	} else {
		// The prelude calls a few host functions while it loads (the WebGL constant table, the document id of a VM
		// that has no document yet). A snapshot has no host to ask: it replays what a VM booting from source saw.
		sb.WriteString("(function () { var r = [")
		for i, x := range rec {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "[%d,%d,%s]", x.idx, x.fail, x.js)
		}
		sb.WriteString("], p = 0; return function (i) { var e = r[p]; if (!e || e[0] !== i) throw new Error('no document'); p++; if (e[1]) throw new Error(e[2]); return e[2]; }; })()")
	}
	sb.WriteString("; var o = {}, n = [")
	for i, x := range names {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(strconv.Quote(x))
	}
	sb.WriteString("]; globalThis.__swBind = function (f) { c = f; }; n.forEach(function (k, i) { o[k] = function (a0, a1, a2, a3) { switch (arguments.length) { case 0: return c(i); case 1: return c(i, a0); case 2: return c(i, a0, a1); case 3: return c(i, a0, a1, a2); case 4: return c(i, a0, a1, a2, a3); } var r = [i]; for (var j = 0; j < arguments.length; j++) r.push(arguments[j]); return c.apply(null, r); }; }); return o; })();\n")
	return sb.String()
}

// v8HelperJS runs after the prelude. It is the part of the runtime that is JavaScript on V8 where QuickJS had Go
// code: awaiting an agent's expression, reporting unhandled promise rejections, and the streaming WebAssembly
// entry points V8 leaves to its embedder.
const v8HelperJS = `;(function (g) {
  function errStr(e) {
    try { if (e && typeof e === 'object' && 'message' in e && 'name' in e) return e.name + ': ' + e.message; return String(e); } catch (x) { return 'rejected'; }
  }
  // Agent evaluate(): the completion value of the expression is in __swLast (EvalStash). A thenable is awaited
  // by the Go pump; __swAgentPoll reports when it settled.
  g.__swAgentSettle = function () {
    var v = g.__swLast; delete g.__swLast;
    var thenable = false; try { thenable = g.__swIsThenable(v); } catch (e) {}
    if (thenable) {
      var st = { done: false, ok: true, res: null, err: '' };
      g.__swAgentSt = st;
      v.then(function (x) { try { st.res = g.__swSerialize(x); } catch (e) { st.res = null; } st.done = true; },
             function (e) { st.ok = false; st.err = errStr(e); st.done = true; });
      return { async: true };
    }
    return { async: false, res: g.__swSerialize(v) };
  };
  g.__swAgentPoll = function () {
    var st = g.__swAgentSt;
    if (!st || !st.done) return { done: false };
    delete g.__swAgentSt;
    return { done: true, ok: st.ok, res: st.res, err: st.err };
  };
  // Promises rejected with no handler (the shim's reject callback keeps them until the microtasks ran).
  g.__swFlushRejections = function () {
    var list = g.__swTakeRejections();
    for (var i = 0; i < list.length; i++) {
      var p = list[i][0], reason = list[i][1], ev, ok = true;
      try { ev = new g.PromiseRejectionEvent('unhandledrejection', { promise: p, reason: reason, cancelable: true }); }
      catch (e) { ev = new g.Event('unhandledrejection', { cancelable: true }); ev.promise = p; ev.reason = reason; }
      try { ok = g.dispatchEvent(ev); } catch (e) {}
      if (ok && g.console) g.console.error('Uncaught (in promise)', reason);
    }
  };
  var W = g.WebAssembly;
  if (W && !W.compileStreaming) {
    var bytes = function (r) { return Promise.resolve(r).then(function (res) { return res.arrayBuffer(); }); };
    W.compileStreaming = function (r) { return bytes(r).then(function (b) { return W.compile(b); }); };
    W.instantiateStreaming = function (r, imp) { return bytes(r).then(function (b) { return W.instantiate(b, imp); }); };
  }
})(globalThis);
`

// v8PreludeSource is the script a snapshot (or a snapshot-less VM) runs: the host object, the prelude and every
// registered extension but the ones V8 provides itself, the native-code toString pass (V8 keeps the source of
// functions, so the prelude's functions would otherwise print their text), and the helpers.
func v8PreludeSource(names []string, rec []v8Rec) string {
	extMu.Lock()
	defer extMu.Unlock()
	var b strings.Builder
	b.WriteString(hostBindJS(names, rec))
	b.WriteString(zpack.String(domPreludeZ))
	b.WriteString("\n")
	b.WriteString(zpack.String(domPreludeExtZ))
	for _, n := range preludeNames() {
		if v8SkipPrelude[n] {
			continue
		}
		b.WriteString("\n;/* extension: " + n + " */\n")
		if src, ok := preludeExtras[n]; ok {
			b.WriteString(src)
		} else {
			b.WriteString(zpack.String(preludeZExtras[n]))
		}
	}
	b.WriteString("\n")
	b.WriteString(nativeToStringJS)
	b.WriteString("\n")
	b.WriteString(v8HelperJS)
	return b.String()
}

// v8SnapKey names a snapshot: library identity, prelude, host table, helpers.
func v8SnapKey(names []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "v8snap2\x00%s\x00", v8LibIdentity())
	k := preludeKey()
	h.Write(k[:])
	for _, n := range preludeNames() {
		if v8SkipPrelude[n] {
			fmt.Fprintf(h, "skip\x00%s\x00", n)
		}
	}
	fmt.Fprintf(h, "%s\x00%s\x00%v\x00%s", hostBindJS(names, nil), v8HelperJS, v8SnapKeepCode, exeIdentity())
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// v8CacheTag names the V8 code cache directory: a cache from another V8 build or flag set is never read.
func v8CacheTag() string {
	sum := sha256.Sum256([]byte(v8LibIdentity() + "|" + v8shim.Flags))
	return v8shim.Version() + "-" + hex.EncodeToString(sum[:4])
}

// exeIdentity changes with every build of the binary: the host calls a snapshot replays come from Go code, so a
// snapshot is only valid for the binary that recorded them.
func exeIdentity() string {
	p, err := os.Executable()
	if err != nil {
		return "?"
	}
	fi, err := os.Stat(p)
	if err != nil {
		return p
	}
	return fmt.Sprintf("%s|%d|%d", filepath.Base(p), fi.Size(), fi.ModTime().UnixNano())
}

// v8Rec is one host call made while the prelude loads: the function index, whether it failed, and the result as
// a JavaScript expression (or the error message as a JSON string).
type v8Rec struct {
	idx  int
	fail int
	js   string
}

// v8RecordLoad boots the prelude from source in a throwaway isolate bound to the real host table (the dispatcher
// of a VM without a tab, exactly the state a pooled VM is built in) and records the host calls it makes.
func v8RecordLoad(names []string, dispatch hostFn, act *v8Actor) (rec []v8Rec, err error) {
	act.do(func() {
		iso, e := v8shim.NewIsolate(v8shim.Options{})
		if e != nil {
			err = e
			return
		}
		defer iso.Close()
		ctx, e := iso.NewContext()
		if e != nil {
			err = e
			return
		}
		defer ctx.Close()
		rec = []v8Rec{}
		e = ctx.Register("__rec_native", func(c *v8shim.Call) {
			n := c.N()
			args := make([]any, n)
			for i := 0; i < n; i++ {
				args[i] = hostArg(c, i)
			}
			idx := -1
			if n > 0 {
				idx = int(toInt(args[0]))
			}
			res, derr := dispatch(args)
			if derr != nil {
				m, _ := json.Marshal(derr.Error())
				rec = append(rec, v8Rec{idx, 1, string(m)})
				c.RetThrow(derr.Error())
				return
			}
			js := "void 0"
			if _, isU := res.(quickjs.Undefined); !isU {
				m, merr := json.Marshal(res)
				if merr != nil {
					m = []byte("null")
				}
				js = string(m)
			}
			rec = append(rec, v8Rec{idx, 0, js})
			if herr := hostRet(c, res); herr != nil {
				c.RetThrow(herr.Error())
			}
		})
		if e != nil {
			err = e
			return
		}
		src := v8PreludeSource(names, nil)
		i := strings.Index(src, "\n") // the first line is hostBindJS: bind the recorder right after it
		if _, e = ctx.Eval(src[:i+1]+"__swBind(__rec_native);\n"+src[i+1:], "sweef:prelude-rec"); e != nil {
			err = e
		}
	})
	return
}

func v8LibIdentity() string {
	p := v8shim.LibPath()
	fi, err := os.Stat(p)
	if err != nil {
		return p
	}
	return fmt.Sprintf("%s|%d|%d|%s", filepath.Base(p), fi.Size(), fi.ModTime().UnixNano(), v8shim.Version())
}

// v8SnapKeepCode keeps the prelude's compiled bytecode in the snapshot: bigger blob, no re-parse when a prelude
// function first runs.
var v8SnapKeepCode = os.Getenv("SWEEF_V8_SNAPSHOT_KEEPCODE") != "0"

var (
	v8SnapDirMu sync.Mutex
	v8SnapDir   string // the cache directory snapshots live in ("" = memory only)
	v8Snaps     sync.Map
)

// setV8SnapDir lets the first engine point the snapshot cache at disk.
func setV8SnapDir(dir string) {
	v8SnapDirMu.Lock()
	if v8SnapDir == "" && dir != "" {
		v8SnapDir = dir
	}
	v8SnapDirMu.Unlock()
}

// v8Active is the snapshot the process's isolates were created on (nil: none). Isolates that are alive together
// must share it (v8shim.Options), so the code-cache producer follows it too.
var v8Active atomic.Pointer[[]byte]

type v8SnapEntry struct {
	once sync.Once
	blob []byte
	from string // "disk", "built" or "" (unavailable), for the load timeline
	took time.Duration
}

// v8SnapshotFor returns the startup snapshot for a host table, building it once per process (and per disk
// cache). nil means "boot from source".
func v8SnapshotFor(names []string, dispatch hostFn, act *v8Actor) (blob []byte, from string) {
	if os.Getenv("SWEEF_V8_SNAPSHOT") == "off" {
		return nil, ""
	}
	key := v8SnapKey(names)
	e, _ := v8Snaps.LoadOrStore(key, &v8SnapEntry{})
	ent := e.(*v8SnapEntry)
	ent.once.Do(func() {
		t0 := time.Now()
		v8SnapDirMu.Lock()
		dir := v8SnapDir
		v8SnapDirMu.Unlock()
		var file string
		if dir != "" {
			file = filepath.Join(dir, "v8snap-"+key+".bin")
			if b, err := readSnapFile(file); err == nil {
				ent.blob, ent.from = b, "disk"
				ent.took = time.Since(t0)
				return
			}
		}
		rec, err := v8RecordLoad(names, dispatch, act)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sweef: v8 snapshot unavailable, booting the prelude from source:", firstLine(err.Error()))
			return
		}
		b, err := v8shim.CreateSnapshot(v8PreludeSource(names, rec), "sweef:prelude", v8SnapKeepCode)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sweef: v8 snapshot unavailable, booting the prelude from source:", firstLine(err.Error()))
			return
		}
		ent.blob, ent.from = b, "built"
		ent.took = time.Since(t0)
		if file != "" {
			writeSnapFile(file, b)
		}
	})
	return ent.blob, ent.from
}

// Snapshot files end with a CRC-32C of the blob: V8 treats a corrupt snapshot as fatal, so it never sees one.
func readSnapFile(file string) ([]byte, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if len(b) < 8 {
		return nil, fmt.Errorf("short snapshot")
	}
	n := len(b) - 4
	if crc32.Checksum(b[:n], crc32.MakeTable(crc32.Castagnoli)) != binary.LittleEndian.Uint32(b[n:]) {
		_ = os.Remove(file)
		return nil, fmt.Errorf("corrupt snapshot")
	}
	return b[:n], nil
}

func writeSnapFile(file string, blob []byte) {
	if os.MkdirAll(filepath.Dir(file), 0o700) != nil {
		return
	}
	sum := make([]byte, 4)
	binary.LittleEndian.PutUint32(sum, crc32.Checksum(blob, crc32.MakeTable(crc32.Castagnoli)))
	tmp := fmt.Sprintf("%s.%d.tmp", file, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	_, e1 := f.Write(blob)
	_, e2 := f.Write(sum)
	e3 := f.Close()
	if e1 != nil || e2 != nil || e3 != nil || os.Rename(tmp, file) != nil {
		_ = os.Remove(tmp)
	}
}

// V8 trusts the code cache bytes it is handed (the engine does not turn on V8's own checksum, see v8shim.Flags), so
// every blob the engine stores for it carries a CRC-32C trailer that is checked before V8 sees the bytes.
func v8CacheWrap(b []byte) []byte {
	out := make([]byte, len(b)+4)
	copy(out, b)
	binary.LittleEndian.PutUint32(out[len(b):], crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)))
	return out
}

// v8CacheUnwrap returns the blob inside w, or nil when w is not a wrapped blob or does not match its CRC.
func v8CacheUnwrap(w []byte) []byte {
	if len(w) < 5 {
		return nil
	}
	n := len(w) - 4
	if crc32.Checksum(w[:n], crc32.MakeTable(crc32.Castagnoli)) != binary.LittleEndian.Uint32(w[n:]) {
		return nil
	}
	return w[:n]
}
