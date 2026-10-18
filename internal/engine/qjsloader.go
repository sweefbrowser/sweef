//go:build !(386 || arm)

package engine

// qjsloader.go: the module loader that reads precompiled bytecode.
//
// modernc.org/quickjs's SetModuleLoader takes module source and compiles it inside the loader callback, on the
// page's thread. installBytecodeLoader puts a loader of the same shape in its place at the C level: it gets the
// source the same way (jsVM.loadModuleSource), and when the precompiler (precompile.go) or the code cache has
// that module's bytecode it reads the bytecode instead of compiling. The normalizer is ours too (the C API sets
// both at once); it asks jsVM.resolveModule as before.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"modernc.org/libc"
	lib "modernc.org/libquickjs"
	"modernc.org/quickjs"
)

var bcLoaders sync.Map // C runtime -> *jsVM

// bcLoaderReads counts modules the loader read from bytecode (tests).
var bcLoaderReads atomic.Int64

func qjsRuntime(vm *quickjs.VM) (crt uintptr, tls *libc.TLS, ok bool) {
	if vm == nil || !qjsOffsets() {
		return 0, nil, false
	}
	rt := *(*unsafe.Pointer)(unsafe.Add(unsafe.Pointer(vm), qjsRtOff))
	if rt == nil {
		return 0, nil, false
	}
	tls = *(**libc.TLS)(unsafe.Add(rt, qjsTLSOff))
	crt = *(*uintptr)(rt) // runtime.cRuntime is its first field (qjsOffsets checks the layout of the others)
	return crt, tls, crt != 0 && tls != nil
}

// installBytecodeLoader replaces the VM's module loader and normalizer; false: left as they were.
func installBytecodeLoader(j *jsVM) bool {
	crt, tls, ok := qjsRuntime(j.vm)
	if !ok {
		return false
	}
	bcLoaders.Store(crt, j)
	lib.XJS_SetModuleLoaderFunc2(tls, crt, cfp(swModuleNormalize), cfp(swModuleLoader), 0, crt)
	return true
}

func uninstallBytecodeLoader(j *jsVM) {
	if j.vm == nil {
		return // not a QuickJS VM
	}
	if crt, _, ok := qjsRuntime(j.vm); ok {
		bcLoaders.Delete(crt)
	}
}

// cfp is the C function pointer of a Go function (modernc's fp).
func cfp(f any) uintptr {
	type iface [2]uintptr
	return (*iface)(unsafe.Pointer(&f))[1]
}

var percentSC = [...]byte{'%', 's', 0}

var percentS = uintptr(unsafe.Pointer(&percentSC[0]))

// swThrow throws a TypeError in ctx (modernc's throwTypeError).
func swThrow(tls *libc.TLS, ctx uintptr, msg string) {
	p, err := libc.CString(msg)
	if err != nil {
		lib.XJS_ThrowTypeError(tls, ctx, 0, 0)
		return
	}
	defer libc.Xfree(tls, p)
	bp := tls.Alloc(16)
	defer tls.Free(16)
	lib.XJS_ThrowTypeError(tls, ctx, percentS, libc.VaList(bp+8, p))
}

func swModuleNormalize(tls *libc.TLS, ctx uintptr, baseName, moduleName uintptr, opaque uintptr) uintptr {
	v, ok := bcLoaders.Load(opaque)
	if !ok {
		return 0
	}
	j := v.(*jsVM)
	base, name := "", ""
	if baseName != 0 {
		base = libc.GoString(baseName)
	}
	if moduleName != 0 {
		name = libc.GoString(moduleName)
	}
	norm, err := j.resolveModule(base, name, j.modDocURL)
	if err != nil {
		swThrow(tls, ctx, fmt.Sprintf("module normalization failed: %v", err))
		return 0
	}
	p, err := libc.CString(norm)
	if err != nil {
		return 0
	}
	defer libc.Xfree(tls, p)
	return lib.Xjs_strdup(tls, ctx, p)
}

func swModuleLoader(tls *libc.TLS, ctx uintptr, moduleName uintptr, opaque uintptr, attrs lib.TJSValue) uintptr {
	v, ok := bcLoaders.Load(opaque)
	if !ok {
		return 0
	}
	j := v.(*jsVM)
	name := ""
	if moduleName != 0 {
		name = libc.GoString(moduleName)
	}
	src, err := j.loadModuleSource(name)
	if err != nil {
		swThrow(tls, ctx, fmt.Sprintf("module load failed: %v", err))
		return 0
	}
	if len(src) >= precompileMin {
		key := moduleKey(name, src)
		bc := j.tab.eng.code.get(key)
		if bc == nil {
			bc = j.tab.eng.pc.wait(key)
		}
		if len(bc) > 0 {
			if p, err := libc.CString(string(bc)); err == nil { // as quickjs.VM.EvalBytecodeValue
				m := lib.XJS_ReadObject(tls, ctx, p, libc.Tsize_t(len(bc)), lib.MJS_READ_OBJ_BYTECODE)
				libc.Xfree(tls, p)
				if m.Ftag == lib.EJS_TAG_MODULE {
					ptr := *(*uintptr)(unsafe.Pointer(&m.Fu))
					lib.XFreeValue(tls, ctx, m)
					bcLoaderReads.Add(1)
					return ptr
				}
				if m.Ftag == lib.EJS_TAG_EXCEPTION {
					lib.XFreeValue(tls, ctx, lib.XJS_GetException(tls, ctx)) // unreadable: compile below
				} else {
					lib.XFreeValue(tls, ctx, m)
				}
				j.tab.eng.code.forget(key)
			}
		}
	}
	ps, err := libc.CString(src)
	if err != nil {
		return 0
	}
	defer libc.Xfree(tls, ps)
	m := lib.XJS_Eval(tls, ctx, ps, libc.Tsize_t(len(src)), moduleName, int32(lib.MJS_EVAL_TYPE_MODULE|lib.MJS_EVAL_FLAG_COMPILE_ONLY))
	if m.Ftag == lib.EJS_TAG_EXCEPTION {
		lib.XFreeValue(tls, ctx, m)
		return 0
	}
	ptr := *(*uintptr)(unsafe.Pointer(&m.Fu))
	lib.XFreeValue(tls, ctx, m)
	return ptr
}
