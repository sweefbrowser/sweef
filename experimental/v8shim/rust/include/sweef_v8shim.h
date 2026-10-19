/*
 * sweef_v8shim.h - C ABI of the Sweef V8 shim (EXPERIMENTAL, prototype).
 *
 * The shim is a Rust cdylib over rusty_v8 (the `v8` crate, V8 15.2). It is
 * loaded at runtime from Go with purego, so the host binary needs no cgo.
 *
 * CONVENTIONS
 *  - All functions are plain C, no C++ types. Strings are (ptr, len) UTF-8,
 *    never NUL-terminated. Status results: 0 = ok, 1 = JS exception,
 *    2 = heap limit hit (isolate should be discarded), 3 = terminated,
 *    -1 = bad context id, -2 = internal failure.
 *  - An isolate is single-threaded: call it from one thread at a time (pin
 *    the goroutine with runtime.LockOSThread). sv8_terminate is the one call
 *    that is safe from any thread.
 *  - Re-entrancy: callbacks into the host (host functions, module resolve,
 *    dynamic import, import.meta) may call any sv8_* function on the same
 *    isolate; JS may call back into the host again, to any depth.
 *  - Results are written to a caller-provided sv8_val. Pointers inside it
 *    (strings, bytes) point into a buffer owned by the isolate and stay valid
 *    until the next sv8_* call on that isolate; copy them out.
 *  - On status 1/2/3, `out` is a T_STRING holding "message\0stack\0res:line:col".
 *
 * VALUES (sv8_val, 16 bytes, little endian)
 *    typedef struct { uint32_t tag; uint32_t len; union { int64_t i; double d;
 *                     const uint8_t *p; } u; } sv8_val;
 *  tag  0 undefined   1 null   2 bool (u.i)   3 int (u.i, int64)
 *       4 double (u.d)   5 string (u.p,len)   6 json (u.p,len; objects/arrays,
 *       JS->host via JSON.stringify, host->JS via JSON.parse)
 *       7 int32 array (u.p = int32_t*, len = count; host->JS only)
 *       9 bytes (u.p,len; copied into an ArrayBuffer host->JS)
 *      10 view (u.p,len; JS->host only: zero-copy pointer into the backing
 *         store of an ArrayBuffer / TypedArray argument; valid during the
 *         call and writable, so a host function can fill JS memory in place)
 *      11 throw (host->JS return only: string payload thrown as Error)
 *
 * HOST CALLBACKS (registered once per process with sv8_set_callbacks)
 *   int32_t host(uint64_t iso_id, uint32_t fn_id, uint32_t nargs,
 *                const sv8_val *args, sv8_val *ret);
 *     Called when JS invokes a function installed with sv8_register_fn.
 *     Nonzero return, or ret->tag == 11, throws Error(string) in JS.
 *     String/JSON arg pointers are valid until the host makes a re-entrant
 *     call into the isolate.
 *   int64_t event(uint32_t kind, uint64_t iso_id, uint64_t a,
 *                 const uint8_t *p1, size_t l1,
 *                 const uint8_t *p2, size_t l2, sv8_val *out);
 *     kind 1 static import: a = referrer module id, p1/l1 = specifier.
 *            Return a module id (from sv8_module_compile, may be called
 *            re-entrantly) or <= 0 with an error message string in out.
 *     kind 2 dynamic import(): p1/l1 = referrer resource name, p2/l2 =
 *            specifier. Return as for kind 1.
 *     kind 3 import.meta: a = module id; return 0 and set out to the
 *            import.meta.url string.
 *
 * FUNCTIONS (see lib.rs for the implementation)
 *   const char *sv8_init(const uint8_t *flags, size_t n)   V8 flags + version
 *   void   sv8_set_callbacks(host_fn, event_fn)
 *   Iso   *sv8_isolate_new(size_t heap_max, const uint8_t *snap, size_t n)
 *   uint64 sv8_isolate_id(Iso*)        void sv8_isolate_free(Iso*)
 *   uint32 sv8_context_new(Iso*)       void sv8_context_free(Iso*, uint32)
 *   int    sv8_register_fn(Iso*, ctx, name, nlen, fn_id)  (dotted names ok)
 *   int    sv8_script(Iso*, ctx, src, n, name, n, mode, cache, n, int *rej, out)
 *            mode 0 run, 1 return code cache bytes (tag 9), 2 run with cache
 *            (*rej = 1 if V8 rejected the cache; the script still runs)
 *   int    sv8_call(Iso*, ctx, name, nlen, const sv8_val *args, nargs, out)
 *   int    sv8_module_compile(Iso*, ctx, src, n, name, n, out)  out = id (int)
 *   int    sv8_module_run(Iso*, ctx, module_id, out)  link+evaluate+drain
 *   int    sv8_module_export(Iso*, ctx, module_id, name, n, out)
 *   void   sv8_microtasks(Iso*)        int sv8_pump(Iso*)
 *   void   sv8_terminate(Iso*)         void sv8_cancel_terminate(Iso*)
 *   void   sv8_heap_stats(Iso*, uint64_t out[6])
 *   void   sv8_low_memory(Iso*)
 *   int    sv8_snapshot_create(src, n, name, n, keep_code, uint8_t **blob,
 *                              size_t *len, sv8_val *err)  (free: sv8_free)
 *   void   sv8_free(uint8_t *p, size_t len)
 */
#ifndef SWEEF_V8SHIM_H
#define SWEEF_V8SHIM_H
#include <stddef.h>
#include <stdint.h>

typedef struct {
  uint32_t tag;
  uint32_t len;
  union {
    int64_t i;
    double d;
    const uint8_t *p;
  } u;
} sv8_val;

#endif
