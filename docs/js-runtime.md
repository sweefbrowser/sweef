# JavaScript runtimes

Sweef runs a page's JavaScript on one of two engines behind one interface (`internal/engine/jsruntime.go`):

| Runtime | What it is | Selected by |
|---|---|---|
| `quickjs` (default) | QuickJS transpiled to pure Go (`modernc.org/quickjs`). In the binary, no extra files. | default |
| `v8` | V8 15.2 through a Rust shim (`experimental/v8shim/rust`, a cdylib over rusty_v8) loaded at run time with purego (`internal/v8shim`). The binary stays `CGO_ENABLED=0`; the library is a separate file. | `--js-engine v8`, `Options.JSEngine`, `SWEEF_JS_ENGINE=v8` |
| `auto` | V8 when its library loads, QuickJS otherwise (one line on stderr says why) | `--js-engine auto` |

**Build tag.** purego without cgo makes a Linux binary dynamically linked (libc, libdl, libpthread), and Sweef's Linux
builds must be static. The V8 loader is therefore compiled on Linux only with `-tags sweef_v8`; the default build has a
stub whose `Load` says "this build has no V8 support", so `--js-engine v8` fails loudly and `auto` falls back to
QuickJS. macOS and Windows builds always carry the loader. A Linux binary that can use V8 is a separate, glibc-only
build variant.

`SWEEF_JS_ENGINE` overrides the option and the flag. `v8` fails (the daemon exits with status 2, a library user gets a
console error per tab) when the library cannot be loaded. The library is looked up in `$SWEEF_V8_LIB`, then next to the
executable, then in the working directory.

## The interface

The engine owns what is not the language runtime: the table of ~100 host functions (`registerHost`, backend-neutral
closures behind the single dispatcher `__sw_call`), the prelude that builds the browser API on it, the event loop
(`pumpUntil`: quiet windows, budgets, timers, scripts inserted by the page) and the load bookkeeping. A `jsRuntime`
is the part underneath: install the host object, run a classic script (plain, or through the code cache), run an ES
module, install the module resolver, call a prelude function, drain microtasks, evaluate an agent expression, interrupt,
close, report heap numbers. `qjsRT` is the original `jsvm.go` code moved behind it unchanged; `v8RT` is the second
implementation. The host closures are not ported: V8 hands them the same Go values QuickJS does (ints and doubles
and strings as tagged scalars, objects and arrays as JSON) and takes their results back the same way (maps and slices
as a native tree, so a 10 MB response body is one string conversion).

Where the runtimes differ:

* **Threads.** An isolate belongs to the thread that created it, so each V8 VM owns one goroutine locked to one OS
  thread; a call from the engine is a hand-off to it (about 30 us, a few hundred per page load).
* **Startup.** QuickJS loads the prelude from bytecode (~3 ms). V8 boots from a startup snapshot built at first use
  and cached in the per-user cache directory (`<cache>/v8/v8snap-<key>.bin`, keyed by the library, the prelude, the
  host table and the binary; `SWEEF_V8_SNAP_DIR` moves it). The snapshot replays the few host calls the prelude makes
  while it loads. Without a usable snapshot a VM evaluates the prelude source instead.
* **All isolates in a process share one startup snapshot** (or none): V8 shares the read-only heap and aborts the
  process when a snapshot isolate and a plain one are alive together (`v8shim.Options`).
* **Code cache.** QuickJS: bytecode, produced by background workers as scripts arrive. V8: off by default; opt in
  with `SWEEF_V8_CODECACHE=on` (V8's code cache, produced by compiling the script eagerly in a background isolate
  after it ran, consumed on the next visit). Warm it saved 50-160 ms on three of the six replay pages and cost
  390 ms on nextjs.org/docs, so the default waits for a cache made from what the page really executed.
* **Module evaluation** leaves the module's promise jobs queued for the event loop (as QuickJS does) rather than
  draining them, so the page's requests overlap instead of being awaited in place.
* **Unhandled promise rejections** raise `unhandledrejection` on V8 (QuickJS never did).

## Feature scope on V8 (this is the gate prototype)

| Feature | V8 | Notes |
|---|---|---|
| Classic scripts, modules, dynamic `import()`, `import.meta.url` | supported | resolver and loader are the engine's |
| Timers, rAF, microtasks, the event loop and its quiet-window / budget semantics | supported | the pump is shared code |
| `fetch`, XHR, DOM, storage, events through the host table | supported | |
| Prelude from a startup snapshot, snapshot-less fallback | supported | |
| Execution timeout, heap cap | supported | `JSTimeoutMs` terminates the script; the heap cap ends the isolate (the tab keeps its DOM) |
| Unhandled-rejection events | supported | |
| Agent `evaluate` (promises, DOM nodes, syntax-error retry) | supported | |
| Native `Intl`, `WebAssembly` | V8's own | the QuickJS-only WebAssembly host is not loaded; `compileStreaming` / `instantiateStreaming` are added in JS |
| `SharedArrayBuffer` | V8's own, no agent cluster | cross-origin-isolation gating and workers not wired |
| Canvas getImageData/putImageData buffer fast path, WebGL and WebCodecs value handles | not yet | fall back to the slower path, or report "not supported on the V8 runtime yet" |
| Workers, service workers | QuickJS | they keep running on QuickJS in a V8 process |
| Code cache for ES modules; prefetch of queued `import()` calls | not yet | |
| Named-property interceptors | not needed so far | |
