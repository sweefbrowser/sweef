//! sweef_v8shim: a small C ABI over rusty_v8 (the `v8` crate), for loading from
//! Go with purego (no cgo). EXPERIMENTAL prototype. See include/sweef_v8shim.h
//! for the documented ABI; this file implements it.
//!
//! Threading model: an isolate may be used from one thread at a time (callers
//! must pin the goroutine with runtime.LockOSThread). `sv8_terminate` is the
//! only call that is safe from any thread.
//!
//! Re-entrancy: host callbacks into Go may call back into any `sv8_*` function
//! on the same isolate (eval inside a host function, etc.). Raw pointers are
//! used for the per-isolate state so no Rust reference is held across the
//! callback.

#![allow(clippy::missing_safety_doc)]
#![allow(clippy::not_unsafe_ptr_arg_deref)]
// The per-isolate state is reached through raw pointers on purpose (host
// callbacks re-enter); autoref on `(*p).field.method()` is intentional.
#![allow(dangerous_implicit_autorefs)]

use std::collections::HashMap;
use std::ffi::c_void;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::Once;

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

pub const T_UNDEFINED: u32 = 0;
pub const T_NULL: u32 = 1;
pub const T_BOOL: u32 = 2;
pub const T_INT: u32 = 3;
pub const T_DOUBLE: u32 = 4;
pub const T_STRING: u32 = 5;
pub const T_JSON: u32 = 6;
pub const T_INT32S: u32 = 7;
pub const T_BYTES: u32 = 9;
pub const T_VIEW: u32 = 10;
/// Return only: throw `Error(message)` in JS (string payload).
pub const T_THROW: u32 = 11;

#[repr(C)]
#[derive(Clone, Copy)]
pub union ValU {
    pub i: i64,
    pub d: f64,
    pub p: *const u8,
}

/// 16-byte tagged value used for every argument and result.
#[repr(C)]
#[derive(Clone, Copy)]
pub struct Val {
    pub tag: u32,
    pub len: u32,
    pub u: ValU,
}

impl Val {
    const fn undef() -> Val {
        Val {
            tag: T_UNDEFINED,
            len: 0,
            u: ValU { i: 0 },
        }
    }
}

type HostCb = unsafe extern "C" fn(u64, u32, u32, *const Val, *mut Val) -> i32;
type EventCb =
    unsafe extern "C" fn(u32, u64, u64, *const u8, usize, *const u8, usize, *mut Val) -> i64;

static HOST_CB: AtomicUsize = AtomicUsize::new(0);
static EVENT_CB: AtomicUsize = AtomicUsize::new(0);
static NEXT_ISO_ID: AtomicU64 = AtomicU64::new(1);

fn host_cb() -> Option<HostCb> {
    let p = HOST_CB.load(Ordering::Relaxed);
    if p == 0 {
        None
    } else {
        Some(unsafe { std::mem::transmute::<usize, HostCb>(p) })
    }
}
fn event_cb() -> Option<EventCb> {
    let p = EVENT_CB.load(Ordering::Relaxed);
    if p == 0 {
        None
    } else {
        Some(unsafe { std::mem::transmute::<usize, EventCb>(p) })
    }
}

// ---------------------------------------------------------------------------
// Isolate state
// ---------------------------------------------------------------------------

struct FnData {
    iso: *mut Iso,
    fn_id: u32,
}

/// Stored in the isolate slot so V8 callbacks can find our state.
struct IsoPtr(*mut Iso);

pub struct Iso {
    id: u64,
    ctxs: Vec<Option<v8::Global<v8::Context>>>,
    modules: Vec<Option<v8::Global<v8::Module>>>,
    module_by_hash: HashMap<i32, u32>,
    // Boxed on purpose: V8 holds raw pointers to these, so addresses must be stable.
    #[allow(clippy::vec_box)]
    fn_data: Vec<Box<FnData>>,
    out: Vec<u8>,
    arena: Vec<u8>,
    handle: v8::IsolateHandle,
    oom: bool,
    // Must stay last: Globals above must be dropped before the isolate.
    isolate: v8::OwnedIsolate,
}

struct PlatformBox(v8::SharedRef<v8::Platform>);
unsafe impl Send for PlatformBox {}
unsafe impl Sync for PlatformBox {}
static PLATFORM: std::sync::OnceLock<PlatformBox> = std::sync::OnceLock::new();
static INIT: Once = Once::new();

fn ensure_init() {
    INIT.call_once(|| {
        let p = v8::new_default_platform(0, false).make_shared();
        v8::V8::initialize_platform(p.clone());
        v8::V8::initialize();
        let _ = PLATFORM.set(PlatformBox(p));
    });
}

unsafe fn slice<'a>(p: *const u8, n: usize) -> &'a [u8] {
    if p.is_null() || n == 0 {
        &[]
    } else {
        std::slice::from_raw_parts(p, n)
    }
}

fn iso_of(isolate: &v8::Isolate) -> *mut Iso {
    isolate
        .get_slot::<IsoPtr>()
        .map(|p| p.0)
        .unwrap_or(std::ptr::null_mut())
}

// ---------------------------------------------------------------------------
// Value conversion
// ---------------------------------------------------------------------------

fn new_str<'s>(scope: &v8::PinScope<'s, '_>, b: &[u8]) -> v8::Local<'s, v8::String> {
    v8::String::new_from_utf8(scope, b, v8::NewStringType::Normal)
        .unwrap_or_else(|| v8::String::empty(scope))
}

fn arena_str(
    scope: &v8::PinScope,
    arena: &mut Vec<u8>,
    s: v8::Local<v8::String>,
) -> (usize, usize) {
    let n = s.utf8_length(scope);
    let start = arena.len();
    arena.resize(start + n, 0);
    s.write_utf8_v2(
        scope,
        &mut arena[start..],
        v8::WriteFlags::kReplaceInvalidUtf8,
        None,
    );
    (start, n)
}

/// Offsets are stored in `u.i` while building; `fixup` turns them into pointers.
fn js_to_val(scope: &mut v8::PinScope, v: v8::Local<v8::Value>, arena: &mut Vec<u8>) -> Val {
    if v.is_int32() {
        let i = unsafe { v8::Local::<v8::Integer>::cast_unchecked(v) }.value();
        return Val {
            tag: T_INT,
            len: 0,
            u: ValU { i },
        };
    }
    if v.is_number() {
        return Val {
            tag: T_DOUBLE,
            len: 0,
            u: ValU {
                d: v.number_value(scope).unwrap_or(f64::NAN),
            },
        };
    }
    if v.is_string() {
        let s = unsafe { v8::Local::<v8::String>::cast_unchecked(v) };
        let (off, n) = arena_str(scope, arena, s);
        return Val {
            tag: T_STRING,
            len: n as u32,
            u: ValU { i: off as i64 },
        };
    }
    if v.is_undefined() {
        return Val::undef();
    }
    if v.is_null() {
        return Val {
            tag: T_NULL,
            len: 0,
            u: ValU { i: 0 },
        };
    }
    if v.is_boolean() {
        return Val {
            tag: T_BOOL,
            len: 0,
            u: ValU {
                i: v.is_true() as i64,
            },
        };
    }
    if let Ok(view) = v8::Local::<v8::ArrayBufferView>::try_from(v) {
        // Zero-copy view of the backing store (valid during the call).
        let len = view.byte_length();
        let ptr = view.data() as *const u8;
        return Val {
            tag: T_VIEW,
            len: len as u32,
            u: ValU { p: ptr },
        };
    }
    if let Ok(ab) = v8::Local::<v8::ArrayBuffer>::try_from(v) {
        let len = ab.byte_length();
        let ptr = ab
            .data()
            .map(|p| p.as_ptr() as *const u8)
            .unwrap_or(std::ptr::null());
        return Val {
            tag: T_VIEW,
            len: len as u32,
            u: ValU { p: ptr },
        };
    }
    // Objects, arrays, bigint, symbols, functions: JSON (undefined -> undefined).
    match v8::json::stringify(scope, v) {
        Some(s) => {
            let (off, n) = arena_str(scope, arena, s);
            Val {
                tag: T_JSON,
                len: n as u32,
                u: ValU { i: off as i64 },
            }
        }
        None => Val::undef(),
    }
}

fn fixup(vals: &mut [Val], arena: &[u8]) {
    for v in vals {
        if v.tag == T_STRING || v.tag == T_JSON {
            let off = unsafe { v.u.i } as usize;
            v.u.p = unsafe { arena.as_ptr().add(off) };
        }
    }
}

fn val_to_js<'s>(scope: &mut v8::PinScope<'s, '_>, v: &Val) -> Option<v8::Local<'s, v8::Value>> {
    unsafe {
        Some(match v.tag {
            T_UNDEFINED => v8::undefined(scope).into(),
            T_NULL => v8::null(scope).into(),
            T_BOOL => v8::Boolean::new(scope, v.u.i != 0).into(),
            T_INT => {
                if v.u.i >= i32::MIN as i64 && v.u.i <= i32::MAX as i64 {
                    v8::Integer::new(scope, v.u.i as i32).into()
                } else {
                    v8::Number::new(scope, v.u.i as f64).into()
                }
            }
            T_DOUBLE => v8::Number::new(scope, v.u.d).into(),
            T_STRING => new_str(scope, slice(v.u.p, v.len as usize)).into(),
            T_JSON => {
                let s = new_str(scope, slice(v.u.p, v.len as usize));
                v8::json::parse(scope, s)?
            }
            T_INT32S => {
                let n = v.len as usize;
                let ints = if n == 0 {
                    &[][..]
                } else {
                    std::slice::from_raw_parts(v.u.p as *const i32, n)
                };
                let elems: Vec<v8::Local<v8::Value>> = ints
                    .iter()
                    .map(|&i| v8::Integer::new(scope, i).into())
                    .collect();
                v8::Array::new_with_elements(scope, &elems).into()
            }
            T_BYTES => {
                let b = slice(v.u.p, v.len as usize).to_vec();
                let store = v8::ArrayBuffer::new_backing_store_from_vec(b).make_shared();
                v8::ArrayBuffer::with_backing_store(scope, &store).into()
            }
            _ => v8::undefined(scope).into(),
        })
    }
}

/// Write `v` into the caller's `out`, using iso.out as backing storage.
fn store_result(scope: &mut v8::PinScope, iso: *mut Iso, v: v8::Local<v8::Value>, out: *mut Val) {
    unsafe {
        let mut arena = std::mem::take(&mut (*iso).out);
        arena.clear();
        let mut val = js_to_val(scope, v, &mut arena);
        if val.tag == T_VIEW {
            // Copy view bytes for results (not valid after return).
            let src = slice(val.u.p, val.len as usize);
            let off = arena.len();
            arena.extend_from_slice(src);
            val = Val {
                tag: T_BYTES,
                len: val.len,
                u: ValU { i: off as i64 },
            };
        }
        if matches!(val.tag, T_STRING | T_JSON | T_BYTES) {
            let off = val.u.i as usize;
            val.u.p = arena.as_ptr().add(off);
        }
        (*iso).out = arena;
        *out = val;
    }
}

fn set_out_str(iso: *mut Iso, tag: u32, s: &[u8], out: *mut Val) {
    unsafe {
        (*iso).out.clear();
        (*iso).out.extend_from_slice(s);
        *out = Val {
            tag,
            len: s.len() as u32,
            u: ValU {
                p: (*iso).out.as_ptr(),
            },
        };
    }
}

// ---------------------------------------------------------------------------
// Error reporting
// ---------------------------------------------------------------------------

/// Error payload: "message\0stack\0resource:line:col".
fn report_exception(
    tc: &mut v8::PinnedRef<'_, v8::TryCatch<v8::HandleScope>>,
    iso: *mut Iso,
    out: *mut Val,
) -> i32 {
    let (terminated, oom) = (tc.has_terminated(), unsafe { (*iso).oom });
    let mut msg = String::new();
    let mut stack = String::new();
    let mut loc = String::new();
    if oom {
        msg.push_str("RangeError: JavaScript heap out of memory (heap limit reached)");
    } else if terminated {
        msg.push_str("Error: execution terminated");
    } else if let Some(ex) = tc.exception() {
        msg = ex
            .to_string(tc)
            .map(|s| s.to_rust_string_lossy(tc))
            .unwrap_or_else(|| "exception".into());
        if let Some(st) = tc.stack_trace() {
            if let Some(s) = st.to_string(tc) {
                stack = s.to_rust_string_lossy(tc);
            }
        }
        if let Some(m) = tc.message() {
            let res = m
                .get_script_resource_name(tc)
                .and_then(|r| r.to_string(tc))
                .map(|s| s.to_rust_string_lossy(tc))
                .unwrap_or_default();
            loc = format!(
                "{}:{}:{}",
                res,
                m.get_line_number(tc).unwrap_or(0),
                m.get_start_column()
            );
        }
    } else {
        msg.push_str("unknown error");
    }
    let payload = format!("{msg}\0{stack}\0{loc}");
    set_out_str(iso, T_STRING, payload.as_bytes(), out);
    unsafe {
        if terminated || oom {
            (*iso).isolate.cancel_terminate_execution();
            (*iso).oom = false;
        }
    }
    if oom {
        2
    } else if terminated {
        3
    } else {
        1
    }
}

fn err_str(iso: *mut Iso, out: *mut Val, msg: &str) -> i32 {
    set_out_str(iso, T_STRING, format!("{msg}\0\0").as_bytes(), out);
    1
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

unsafe extern "C" fn near_heap_limit(data: *mut c_void, current: usize, _initial: usize) -> usize {
    let iso = data as *mut Iso;
    (*iso).oom = true;
    (*iso).handle.terminate_execution();
    // Give V8 headroom to unwind; the caller should discard the isolate.
    current + 32 * 1024 * 1024
}

unsafe extern "C" fn import_meta_cb(
    ctx: v8::Local<v8::Context>,
    module: v8::Local<v8::Module>,
    meta: v8::Local<v8::Object>,
) {
    v8::callback_scope!(unsafe scope, ctx);
    let iso = iso_of(scope);
    if iso.is_null() {
        return;
    }
    let Some(cb) = event_cb() else { return };
    let hash = module.get_identity_hash().get();
    let mid = (*iso).module_by_hash.get(&hash).copied().unwrap_or(0);
    let mut out = Val::undef();
    let r = cb(
        3,
        (*iso).id,
        mid as u64,
        std::ptr::null(),
        0,
        std::ptr::null(),
        0,
        &mut out,
    );
    if r == 0 && out.tag == T_STRING {
        let url = new_str(scope, slice(out.u.p, out.len as usize));
        let key = v8::String::new(scope, "url").unwrap();
        meta.create_data_property(scope, key.into(), url.into());
    }
}

fn resolve_cb<'s>(
    ctx: v8::Local<'s, v8::Context>,
    spec: v8::Local<'s, v8::String>,
    _attrs: v8::Local<'s, v8::FixedArray>,
    referrer: v8::Local<'s, v8::Module>,
) -> Option<v8::Local<'s, v8::Module>> {
    v8::callback_scope!(unsafe scope, ctx);
    let iso = iso_of(scope);
    let cb = event_cb()?;
    unsafe {
        let hash = referrer.get_identity_hash().get();
        let rid = (*iso).module_by_hash.get(&hash).copied().unwrap_or(0);
        let mut arena = Vec::new();
        let (off, n) = arena_str(scope, &mut arena, spec);
        let mut out = Val::undef();
        let id = cb(
            1,
            (*iso).id,
            rid as u64,
            arena.as_ptr().add(off),
            n,
            std::ptr::null(),
            0,
            &mut out,
        );
        if id <= 0 {
            let m = if out.tag == T_STRING {
                String::from_utf8_lossy(slice(out.u.p, out.len as usize)).into_owned()
            } else {
                "cannot resolve module".to_string()
            };
            let s = new_str(scope, m.as_bytes());
            let e = v8::Exception::error(scope, s);
            scope.throw_exception(e);
            return None;
        }
        let g = (*iso).modules.get(id as usize)?.as_ref()?;
        Some(v8::Local::new(scope, g))
    }
}

fn instantiate_and_eval<'s>(
    scope: &mut v8::PinScope<'s, '_>,
    module: v8::Local<'s, v8::Module>,
) -> Option<v8::Local<'s, v8::Value>> {
    if module.get_status() == v8::ModuleStatus::Uninstantiated {
        module.instantiate_module(scope, resolve_cb)?;
    }
    module.evaluate(scope)
}

fn dyn_import_cb<'s>(
    scope: &mut v8::PinScope<'s, '_>,
    _opts: v8::Local<'s, v8::Data>,
    resource: v8::Local<'s, v8::Value>,
    spec: v8::Local<'s, v8::String>,
    _attrs: v8::Local<'s, v8::FixedArray>,
) -> Option<v8::Local<'s, v8::Promise>> {
    let resolver = v8::PromiseResolver::new(scope)?;
    let promise = resolver.get_promise(scope);
    let iso = iso_of(scope);
    let cb = event_cb()?;
    unsafe {
        let mut arena = Vec::new();
        let rs = resource.to_string(scope)?;
        let (roff, rn) = arena_str(scope, &mut arena, rs);
        let (soff, sn) = arena_str(scope, &mut arena, spec);
        let mut out = Val::undef();
        let id = cb(
            2,
            (*iso).id,
            0,
            arena.as_ptr().add(roff),
            rn,
            arena.as_ptr().add(soff),
            sn,
            &mut out,
        );
        if id <= 0 {
            let m = if out.tag == T_STRING {
                String::from_utf8_lossy(slice(out.u.p, out.len as usize)).into_owned()
            } else {
                "cannot load module".to_string()
            };
            let s = new_str(scope, m.as_bytes());
            let e = v8::Exception::error(scope, s);
            resolver.reject(scope, e);
            return Some(promise);
        }
        let Some(g) = (*iso).modules.get(id as usize).and_then(|m| m.as_ref()) else {
            return Some(promise);
        };
        let module = v8::Local::new(scope, g);
        v8::tc_scope!(let tc, scope);
        match instantiate_and_eval(tc, module) {
            Some(p) => {
                let ns = module.get_module_namespace();
                // p.then(() => ns)
                let helper = v8::Script::compile(
                    tc,
                    v8::String::new(
                        tc,
                        "(function(p,ns){return p.then(function(){return ns;});})",
                    )
                    .unwrap(),
                    None,
                )
                .and_then(|s| s.run(tc))
                .and_then(|f| v8::Local::<v8::Function>::try_from(f).ok());
                let undef = v8::undefined(tc);
                match helper.and_then(|f| f.call(tc, undef.into(), &[p, ns])) {
                    Some(chained) => {
                        resolver.resolve(tc, chained);
                    }
                    None => {
                        let e = tc.exception().unwrap_or_else(|| v8::undefined(tc).into());
                        resolver.reject(tc, e);
                    }
                }
            }
            None => {
                let e = tc.exception().unwrap_or_else(|| v8::undefined(tc).into());
                tc.reset();
                resolver.reject(tc, e);
            }
        }
    }
    Some(promise)
}

fn queue_microtask(
    scope: &mut v8::PinScope,
    args: v8::FunctionCallbackArguments,
    _rv: v8::ReturnValue,
) {
    if let Ok(f) = v8::Local::<v8::Function>::try_from(args.get(0)) {
        scope.enqueue_microtask(f);
    }
}

fn new_params(heap_max: usize, snapshot: Option<Vec<u8>>) -> v8::CreateParams {
    let mut p = v8::CreateParams::default();
    if heap_max > 0 {
        p = p.heap_limits(0, heap_max);
    }
    if let Some(s) = snapshot {
        p = p.snapshot_blob(v8::StartupData::from(s));
    }
    p
}

/// Initialise V8 (once per process) and return its version string (static).
#[no_mangle]
pub unsafe extern "C" fn sv8_init(
    flags: *const u8,
    flags_len: usize,
) -> *const std::os::raw::c_char {
    if flags_len > 0 {
        // Must happen before V8 initialises; ignored if already initialised.
        let f = String::from_utf8_lossy(slice(flags, flags_len)).into_owned();
        if !INIT.is_completed() {
            v8::V8::set_flags_from_string(&f);
        }
    }
    ensure_init();
    static VER: std::sync::OnceLock<std::ffi::CString> = std::sync::OnceLock::new();
    VER.get_or_init(|| std::ffi::CString::new(v8::V8::get_version()).unwrap())
        .as_ptr()
}

#[no_mangle]
pub extern "C" fn sv8_set_callbacks(host: usize, event: usize) {
    HOST_CB.store(host, Ordering::Relaxed);
    EVENT_CB.store(event, Ordering::Relaxed);
}

/// Create an isolate. `heap_max` bytes (0 = V8 default). `snap`/`snap_len`:
/// optional startup snapshot produced by `sv8_snapshot_create`.
#[no_mangle]
pub unsafe extern "C" fn sv8_isolate_new(
    heap_max: usize,
    snap: *const u8,
    snap_len: usize,
) -> *mut Iso {
    ensure_init();
    let snapshot = if snap.is_null() || snap_len == 0 {
        None
    } else {
        Some(slice(snap, snap_len).to_vec())
    };
    let mut isolate = v8::Isolate::new(new_params(heap_max, snapshot));
    isolate.set_microtasks_policy(v8::MicrotasksPolicy::Explicit);
    isolate.set_host_import_module_dynamically_callback(dyn_import_cb);
    isolate.set_host_initialize_import_meta_object_callback(import_meta_cb);
    let handle = isolate.thread_safe_handle();
    let iso = Box::new(Iso {
        id: NEXT_ISO_ID.fetch_add(1, Ordering::Relaxed),
        ctxs: Vec::new(),
        modules: vec![None], // id 0 reserved
        module_by_hash: HashMap::new(),
        fn_data: Vec::new(),
        out: Vec::with_capacity(4096),
        arena: Vec::with_capacity(4096),
        handle,
        oom: false,
        isolate,
    });
    let p = Box::into_raw(iso);
    (*p).isolate.set_slot(IsoPtr(p));
    if heap_max > 0 {
        (*p).isolate
            .add_near_heap_limit_callback(near_heap_limit, p as *mut c_void);
    }
    p
}

#[no_mangle]
pub unsafe extern "C" fn sv8_isolate_id(iso: *mut Iso) -> u64 {
    (*iso).id
}

#[no_mangle]
pub unsafe extern "C" fn sv8_isolate_free(iso: *mut Iso) {
    if iso.is_null() {
        return;
    }
    let mut b = Box::from_raw(iso);
    b.ctxs.clear();
    b.modules.clear();
    drop(b);
}

/// Returns a context id (>= 1), or 0 on failure.
#[no_mangle]
pub unsafe extern "C" fn sv8_context_new(iso: *mut Iso) -> u32 {
    let isolate = &mut (*iso).isolate;
    v8::scope!(let scope, isolate);
    let ctx = v8::Context::new(scope, Default::default());
    let g = v8::Global::new(scope, ctx);
    {
        let scope = &mut v8::ContextScope::new(scope, ctx);
        let global = ctx.global(scope);
        let name = v8::String::new(scope, "queueMicrotask").unwrap();
        let f = v8::Function::new(scope, queue_microtask).unwrap();
        global.set(scope, name.into(), f.into());
    }
    (*iso).ctxs.push(Some(g));
    (*iso).ctxs.len() as u32
}

#[no_mangle]
pub unsafe extern "C" fn sv8_context_free(iso: *mut Iso, ctx: u32) {
    if let Some(slot) = (*iso).ctxs.get_mut(ctx as usize - 1) {
        *slot = None;
    }
}

macro_rules! with_ctx {
    ($iso:expr, $ctx:expr, $scope:ident, $body:block) => {{
        let isolate = &mut (*$iso).isolate;
        let Some(Some(g)) = (*$iso).ctxs.get($ctx as usize - 1) else {
            return -1;
        };
        let g = g.clone();
        v8::scope_with_context!(let $scope, isolate, &g);
        $body
    }};
}

// ---------------------------------------------------------------------------
// Host functions
// ---------------------------------------------------------------------------

fn host_tramp(
    scope: &mut v8::PinScope,
    args: v8::FunctionCallbackArguments,
    mut rv: v8::ReturnValue,
) {
    let Some(cb) = host_cb() else { return };
    let ext = unsafe { v8::Local::<v8::External>::cast_unchecked(args.data()) };
    let fd = unsafe { &*(ext.value() as *const FnData) };
    let iso = fd.iso;
    let n = args.length() as usize;
    unsafe {
        let mut arena = std::mem::take(&mut (*iso).arena);
        let base = arena.len();
        let mut small = [Val::undef(); 8];
        let mut big: Vec<Val>;
        let vals: &mut [Val] = if n <= 8 {
            &mut small[..n]
        } else {
            big = vec![Val::undef(); n];
            &mut big[..]
        };
        for (i, slot) in vals.iter_mut().enumerate() {
            *slot = js_to_val(scope, args.get(i as i32), &mut arena);
        }
        fixup(vals, &arena);
        // Park the arena back while Go runs: nested host calls append to it.
        (*iso).arena = arena;
        let mut ret = Val::undef();
        let st = cb((*iso).id, fd.fn_id, n as u32, vals.as_ptr(), &mut ret);
        // Convert the result before truncating (ret may alias Go memory only).
        if st != 0 || ret.tag == T_THROW {
            let m = String::from_utf8_lossy(slice(ret.u.p, ret.len as usize)).into_owned();
            let s = new_str(scope, m.as_bytes());
            let e = v8::Exception::error(scope, s);
            scope.throw_exception(e);
        } else {
            match ret.tag {
                T_UNDEFINED => {}
                T_INT if ret.u.i >= i32::MIN as i64 && ret.u.i <= i32::MAX as i64 => {
                    rv.set_int32(ret.u.i as i32)
                }
                T_DOUBLE => rv.set_double(ret.u.d),
                T_BOOL => rv.set_bool(ret.u.i != 0),
                T_NULL => rv.set_null(),
                _ => {
                    if let Some(v) = val_to_js(scope, &ret) {
                        rv.set(v);
                    }
                }
            }
        }
        (*iso).arena.truncate(base);
    }
}

/// Install a global function `name` (dotted names create namespaces, e.g.
/// "sw.getNodeType") that dispatches to the host callback with `fn_id`.
#[no_mangle]
pub unsafe extern "C" fn sv8_register_fn(
    iso: *mut Iso,
    ctx: u32,
    name: *const u8,
    name_len: usize,
    fn_id: u32,
) -> i32 {
    let name = String::from_utf8_lossy(slice(name, name_len)).into_owned();
    let fd = Box::new(FnData { iso, fn_id });
    let fdp = &*fd as *const FnData as *mut c_void;
    (*iso).fn_data.push(fd);
    with_ctx!(iso, ctx, scope, {
        let ext = v8::External::new(scope, fdp);
        let func = match v8::Function::builder(host_tramp)
            .data(ext.into())
            .build(scope)
        {
            Some(f) => f,
            None => return -2,
        };
        let mut obj = scope.get_current_context().global(scope);
        let parts: Vec<&str> = name.split('.').collect();
        for part in &parts[..parts.len() - 1] {
            let k = v8::String::new(scope, part).unwrap();
            let existing = obj.get(scope, k.into());
            obj = match existing.and_then(|v| v8::Local::<v8::Object>::try_from(v).ok()) {
                Some(o) => o,
                None => {
                    let o = v8::Object::new(scope);
                    obj.set(scope, k.into(), o.into());
                    o
                }
            };
        }
        let k = v8::String::new(scope, parts[parts.len() - 1]).unwrap();
        obj.set(scope, k.into(), func.into());
        0
    })
}

// ---------------------------------------------------------------------------
// Scripts
// ---------------------------------------------------------------------------

fn make_origin<'s>(
    scope: &v8::PinScope<'s, '_>,
    name: &[u8],
    is_module: bool,
) -> v8::ScriptOrigin<'s> {
    let n = new_str(scope, name);
    v8::ScriptOrigin::new(
        scope,
        n.into(),
        0,
        0,
        false,
        0,
        None,
        false,
        false,
        is_module,
        None,
    )
}

/// mode 0: compile+run. mode 1: compile eagerly and return code cache bytes
/// (tag T_BYTES). mode 2: compile with `cache`, run; `*rejected` set to 1 if
/// V8 rejected the cache (the script is still compiled and run).
#[no_mangle]
pub unsafe extern "C" fn sv8_script(
    iso: *mut Iso,
    ctx: u32,
    src: *const u8,
    src_len: usize,
    name: *const u8,
    name_len: usize,
    mode: i32,
    cache: *const u8,
    cache_len: usize,
    rejected: *mut i32,
    out: *mut Val,
) -> i32 {
    with_ctx!(iso, ctx, scope, {
        v8::tc_scope!(let tc, scope);
        let code = new_str(tc, slice(src, src_len));
        let origin = make_origin(tc, slice(name, name_len), false);
        let script: v8::Local<v8::Script>;
        match mode {
            0 => match v8::Script::compile(tc, code, Some(&origin)) {
                Some(s) => script = s,
                None => return report_exception(tc, iso, out),
            },
            1 => {
                let mut source = v8::script_compiler::Source::new(code, Some(&origin));
                let Some(us) = v8::script_compiler::compile_unbound_script(
                    tc,
                    &mut source,
                    v8::script_compiler::CompileOptions::EagerCompile,
                    v8::script_compiler::NoCacheReason::NoReason,
                ) else {
                    return report_exception(tc, iso, out);
                };
                return match us.create_code_cache() {
                    Some(c) => {
                        set_out_str(iso, T_BYTES, &c, out);
                        0
                    }
                    None => err_str(iso, out, "create_code_cache failed"),
                };
            }
            _ => {
                let cd = v8::script_compiler::CachedData::new(slice(cache, cache_len));
                let mut source =
                    v8::script_compiler::Source::new_with_cached_data(code, Some(&origin), cd);
                let Some(us) = v8::script_compiler::compile_unbound_script(
                    tc,
                    &mut source,
                    v8::script_compiler::CompileOptions::ConsumeCodeCache,
                    v8::script_compiler::NoCacheReason::NoReason,
                ) else {
                    return report_exception(tc, iso, out);
                };
                if !rejected.is_null() {
                    *rejected = source
                        .get_cached_data()
                        .map(|c| c.rejected())
                        .unwrap_or(true) as i32;
                }
                script = us.bind_to_current_context(tc);
            }
        }
        match script.run(tc) {
            Some(v) => {
                store_result(tc, iso, v, out);
                0
            }
            None => report_exception(tc, iso, out),
        }
    })
}

/// Call global function `name` (dotted allowed) with typed args.
#[no_mangle]
pub unsafe extern "C" fn sv8_call(
    iso: *mut Iso,
    ctx: u32,
    name: *const u8,
    name_len: usize,
    args: *const Val,
    nargs: u32,
    out: *mut Val,
) -> i32 {
    let name = String::from_utf8_lossy(slice(name, name_len)).into_owned();
    let argv = if nargs == 0 {
        &[][..]
    } else {
        std::slice::from_raw_parts(args, nargs as usize)
    };
    with_ctx!(iso, ctx, scope, {
        v8::tc_scope!(let tc, scope);
        let mut holder: v8::Local<v8::Object> = tc.get_current_context().global(tc);
        let mut target: v8::Local<v8::Value> = holder.into();
        for part in name.split('.') {
            let k = v8::String::new(tc, part).unwrap();
            holder = match v8::Local::<v8::Object>::try_from(target) {
                Ok(o) => o,
                Err(_) => return err_str(iso, out, "not an object in call path"),
            };
            target = match holder.get(tc, k.into()) {
                Some(v) => v,
                None => return report_exception(tc, iso, out),
            };
        }
        let Ok(f) = v8::Local::<v8::Function>::try_from(target) else {
            return err_str(iso, out, &format!("{name} is not a function"));
        };
        let mut jsargs: Vec<v8::Local<v8::Value>> = Vec::with_capacity(argv.len());
        for a in argv {
            match val_to_js(tc, a) {
                Some(v) => jsargs.push(v),
                None => return err_str(iso, out, "bad argument"),
            }
        }
        match f.call(tc, holder.into(), &jsargs) {
            Some(v) => {
                store_result(tc, iso, v, out);
                0
            }
            None => report_exception(tc, iso, out),
        }
    })
}

// ---------------------------------------------------------------------------
// Modules
// ---------------------------------------------------------------------------

/// Compile an ES module. On success `*out` is T_INT holding the module id.
#[no_mangle]
pub unsafe extern "C" fn sv8_module_compile(
    iso: *mut Iso,
    ctx: u32,
    src: *const u8,
    src_len: usize,
    name: *const u8,
    name_len: usize,
    out: *mut Val,
) -> i32 {
    with_ctx!(iso, ctx, scope, {
        v8::tc_scope!(let tc, scope);
        let code = new_str(tc, slice(src, src_len));
        let origin = make_origin(tc, slice(name, name_len), true);
        let mut source = v8::script_compiler::Source::new(code, Some(&origin));
        let Some(m) = v8::script_compiler::compile_module(tc, &mut source) else {
            return report_exception(tc, iso, out);
        };
        let g = v8::Global::new(tc, m);
        let id = (*iso).modules.len() as u32;
        (*iso).modules.push(Some(g));
        (*iso)
            .module_by_hash
            .insert(m.get_identity_hash().get(), id);
        *out = Val {
            tag: T_INT,
            len: 0,
            u: ValU { i: id as i64 },
        };
        0
    })
}

/// Instantiate (linking via the Go resolver event) and evaluate a module,
/// draining microtasks until its evaluation promise settles. Result: 0 ok.
#[no_mangle]
pub unsafe extern "C" fn sv8_module_run(iso: *mut Iso, ctx: u32, mid: u32, out: *mut Val) -> i32 {
    with_ctx!(iso, ctx, scope, {
        v8::tc_scope!(let tc, scope);
        let Some(Some(g)) = (*iso).modules.get(mid as usize) else {
            return err_str(iso, out, "bad module id");
        };
        let module = v8::Local::new(tc, g);
        let Some(p) = instantiate_and_eval(tc, module) else {
            return report_exception(tc, iso, out);
        };
        tc.perform_microtask_checkpoint();
        if let Ok(p) = v8::Local::<v8::Promise>::try_from(p) {
            match p.state() {
                v8::PromiseState::Rejected => {
                    p.mark_as_handled();
                    let ex = p.result(tc);
                    let msg = ex
                        .to_string(tc)
                        .map(|s| s.to_rust_string_lossy(tc))
                        .unwrap_or_default();
                    let stack = v8::Local::<v8::Object>::try_from(ex)
                        .ok()
                        .and_then(|o| {
                            let k = v8::String::new(tc, "stack").unwrap();
                            o.get(tc, k.into())
                        })
                        .and_then(|s| s.to_string(tc))
                        .map(|s| s.to_rust_string_lossy(tc))
                        .unwrap_or_default();
                    set_out_str(iso, T_STRING, format!("{msg}\0{stack}\0").as_bytes(), out);
                    return 1;
                }
                v8::PromiseState::Pending => {
                    return err_str(
                        iso,
                        out,
                        "module evaluation did not settle (pending top-level await)",
                    );
                }
                _ => {}
            }
        }
        0
    })
}

/// Read an export of an evaluated module (typed value in `out`).
#[no_mangle]
pub unsafe extern "C" fn sv8_module_export(
    iso: *mut Iso,
    ctx: u32,
    mid: u32,
    name: *const u8,
    name_len: usize,
    out: *mut Val,
) -> i32 {
    with_ctx!(iso, ctx, scope, {
        v8::tc_scope!(let tc, scope);
        let Some(Some(g)) = (*iso).modules.get(mid as usize) else {
            return err_str(iso, out, "bad module id");
        };
        let module = v8::Local::new(tc, g);
        let ns = module.get_module_namespace();
        let Ok(o) = v8::Local::<v8::Object>::try_from(ns) else {
            return err_str(iso, out, "module not instantiated");
        };
        let k = new_str(tc, slice(name, name_len));
        match o.get(tc, k.into()) {
            Some(v) => {
                store_result(tc, iso, v, out);
                0
            }
            None => report_exception(tc, iso, out),
        }
    })
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

/// Run the microtask queue to exhaustion.
#[no_mangle]
pub unsafe extern "C" fn sv8_microtasks(iso: *mut Iso) {
    (*iso).isolate.perform_microtask_checkpoint();
}

/// Pump one batch of platform tasks (wasm compile, GC helpers). Returns 1 if
/// any task ran.
#[no_mangle]
pub unsafe extern "C" fn sv8_pump(iso: *mut Iso) -> i32 {
    match PLATFORM.get() {
        Some(p) => v8::Platform::pump_message_loop(&p.0, &(*iso).isolate, false) as i32,
        None => 0,
    }
}

/// Thread-safe: abort a running script in `iso` (also stops a `while(1){}`).
#[no_mangle]
pub unsafe extern "C" fn sv8_terminate(iso: *mut Iso) {
    (*iso).handle.terminate_execution();
}

#[no_mangle]
pub unsafe extern "C" fn sv8_cancel_terminate(iso: *mut Iso) {
    (*iso).handle.cancel_terminate_execution();
}

/// out[0..6] = total_heap, used_heap, heap_limit, total_physical, external, malloced
#[no_mangle]
pub unsafe extern "C" fn sv8_heap_stats(iso: *mut Iso, out: *mut u64) {
    let s = (*iso).isolate.get_heap_statistics();
    let o = std::slice::from_raw_parts_mut(out, 6);
    o[0] = s.total_heap_size() as u64;
    o[1] = s.used_heap_size() as u64;
    o[2] = s.heap_size_limit() as u64;
    o[3] = s.total_physical_size() as u64;
    o[4] = s.external_memory() as u64;
    o[5] = s.malloced_memory() as u64;
}

#[no_mangle]
pub unsafe extern "C" fn sv8_low_memory(iso: *mut Iso) {
    (*iso).isolate.low_memory_notification();
}

/// Build a startup snapshot by running `src` in a fresh context. The blob is
/// leaked into `*out_ptr/*out_len`; release it with `sv8_free`.
#[no_mangle]
pub unsafe extern "C" fn sv8_snapshot_create(
    src: *const u8,
    src_len: usize,
    name: *const u8,
    name_len: usize,
    keep_code: i32,
    out_ptr: *mut *mut u8,
    out_len: *mut usize,
    err: *mut Val,
) -> i32 {
    ensure_init();
    let mut sc = v8::Isolate::snapshot_creator(None, None);
    let mut errmsg = None;
    {
        v8::scope!(let scope, &mut sc);
        let ctx = v8::Context::new(scope, Default::default());
        let scope = &mut v8::ContextScope::new(scope, ctx);
        v8::tc_scope!(let tc, scope);
        let code = new_str(tc, slice(src, src_len));
        let origin = make_origin(tc, slice(name, name_len), false);
        match v8::Script::compile(tc, code, Some(&origin)).and_then(|s| s.run(tc)) {
            Some(_) => {}
            None => {
                errmsg = Some(
                    tc.exception()
                        .and_then(|e| e.to_string(tc))
                        .map(|s| s.to_rust_string_lossy(tc))
                        .unwrap_or_else(|| "snapshot script failed".into()),
                );
            }
        }
        tc.set_default_context(ctx);
    }
    if let Some(m) = errmsg {
        // A snapshot creator must always produce a blob before it is dropped.
        let _ = sc.create_blob(v8::FunctionCodeHandling::Clear);
        let b = m.into_bytes().into_boxed_slice();
        let len = b.len();
        let p = Box::into_raw(b) as *mut u8;
        *err = Val {
            tag: T_STRING,
            len: len as u32,
            u: ValU { p },
        };
        return 1;
    }
    let mode = if keep_code != 0 {
        v8::FunctionCodeHandling::Keep
    } else {
        v8::FunctionCodeHandling::Clear
    };
    match sc.create_blob(mode) {
        Some(blob) => {
            let b: Box<[u8]> = blob.to_vec().into_boxed_slice();
            *out_len = b.len();
            *out_ptr = Box::into_raw(b) as *mut u8;
            0
        }
        None => 2,
    }
}

#[no_mangle]
pub unsafe extern "C" fn sv8_free(p: *mut u8, len: usize) {
    if !p.is_null() {
        drop(Box::from_raw(std::ptr::slice_from_raw_parts_mut(p, len)));
    }
}
