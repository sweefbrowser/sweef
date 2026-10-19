// Package v8shim is an EXPERIMENTAL prototype: a pure-Go (no cgo) wrapper
// around a Rust cdylib (rust/, built on rusty_v8) that embeds V8. The library
// is loaded at runtime with purego, so the Go binary stays CGO_ENABLED=0.
//
// Threading: an Isolate must only be used from one goroutine at a time, and
// that goroutine should call runtime.LockOSThread (V8 keeps per-thread state).
// Isolate.Terminate is the only method that is safe from another goroutine.
package v8shim

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Flags are V8 command-line flags applied once at library load (set before
// the first Load call). The checksum flag makes V8 verify code caches and
// snapshots byte-for-byte, so corrupted data is rejected rather than run.
var Flags = "--verify-snapshot-checksum"

var isDarwin = runtime.GOOS == "darwin"

// EnvLib is the environment variable that overrides the library path.
const EnvLib = "SWEEF_V8_LIB"

// Wire tags, mirrored from rust/src/lib.rs and include/sweef_v8shim.h.
const (
	tUndefined = 0
	tNull      = 1
	tBool      = 2
	tInt       = 3
	tDouble    = 4
	tString    = 5
	tJSON      = 6
	tInt32s    = 7
	tBytes     = 9
	tView      = 10
	tThrow     = 11
)

// rawVal mirrors the 16-byte sv8_val.
type rawVal struct {
	tag uint32
	n   uint32
	u   uint64
}

var (
	fInit         func(flags unsafe.Pointer, n uintptr) uintptr
	fSetCallbacks func(host, event uintptr)
	fIsolateNew   func(heapMax uintptr, snap unsafe.Pointer, snapLen uintptr) uintptr
	fIsolateID    func(iso uintptr) uint64
	fIsolateFree  func(iso uintptr)
	fContextNew   func(iso uintptr) uint32
	fContextFree  func(iso uintptr, ctx uint32)
	fRegisterFn   func(iso uintptr, ctx uint32, name unsafe.Pointer, nlen uintptr, id uint32) int32
	fScript       func(iso uintptr, ctx uint32, src unsafe.Pointer, slen uintptr, name unsafe.Pointer, nlen uintptr, mode int32, cache unsafe.Pointer, clen uintptr, rejected *int32, out *rawVal) int32
	fCall         func(iso uintptr, ctx uint32, name unsafe.Pointer, nlen uintptr, args unsafe.Pointer, nargs uint32, out *rawVal) int32
	fModCompile   func(iso uintptr, ctx uint32, src unsafe.Pointer, slen uintptr, name unsafe.Pointer, nlen uintptr, out *rawVal) int32
	fModRun       func(iso uintptr, ctx uint32, mid uint32, out *rawVal) int32
	fModExport    func(iso uintptr, ctx uint32, mid uint32, name unsafe.Pointer, nlen uintptr, out *rawVal) int32
	fMicrotasks   func(iso uintptr)
	fPump         func(iso uintptr) int32
	fTerminate    func(iso uintptr)
	fCancelTerm   func(iso uintptr)
	fHeapStats    func(iso uintptr, out *[6]uint64)
	fLowMemory    func(iso uintptr)
	fSnapCreate   func(src unsafe.Pointer, slen uintptr, name unsafe.Pointer, nlen uintptr, keep int32, outPtr *uintptr, outLen *uintptr, errv *rawVal) int32
	fFree         func(p uintptr, n uintptr)
)

var (
	loadOnce sync.Once
	loadErr  error
	version  string
	libPath  string
)

// LibPath returns the path of the loaded library ("" before Load).
func LibPath() string { return libPath }

// Version returns the V8 version string of the loaded library.
func Version() string { return version }

func candidates() []string {
	var c []string
	if p := os.Getenv(EnvLib); p != "" {
		c = append(c, p)
	}
	if exe, err := os.Executable(); err == nil {
		c = append(c, filepath.Join(filepath.Dir(exe), libFile()))
	}
	if wd, err := os.Getwd(); err == nil {
		c = append(c, filepath.Join(wd, libFile()))
	}
	return c
}

// Load locates and loads the shared library (once). The search order is
// $SWEEF_V8_LIB, then next to the executable, then the working directory.
func Load() error {
	loadOnce.Do(func() { loadErr = load() })
	return loadErr
}

func load() error {
	var h uintptr
	var lastErr error
	for _, p := range candidates() {
		if _, err := os.Stat(p); err != nil {
			lastErr = err
			continue
		}
		hh, err := openLib(p)
		if err != nil {
			lastErr = err
			continue
		}
		h, libPath = hh, p
		break
	}
	if h == 0 {
		if lastErr == nil {
			lastErr = errors.New("no candidate paths")
		}
		return fmt.Errorf("v8shim: cannot load %s (set %s): %w", libFile(), EnvLib, lastErr)
	}
	reg := func(fptr any, name string) { purego.RegisterLibFunc(fptr, h, name) }
	reg(&fInit, "sv8_init")
	reg(&fSetCallbacks, "sv8_set_callbacks")
	reg(&fIsolateNew, "sv8_isolate_new")
	reg(&fIsolateID, "sv8_isolate_id")
	reg(&fIsolateFree, "sv8_isolate_free")
	reg(&fContextNew, "sv8_context_new")
	reg(&fContextFree, "sv8_context_free")
	reg(&fRegisterFn, "sv8_register_fn")
	reg(&fScript, "sv8_script")
	reg(&fCall, "sv8_call")
	reg(&fModCompile, "sv8_module_compile")
	reg(&fModRun, "sv8_module_run")
	reg(&fModExport, "sv8_module_export")
	reg(&fMicrotasks, "sv8_microtasks")
	reg(&fPump, "sv8_pump")
	reg(&fTerminate, "sv8_terminate")
	reg(&fCancelTerm, "sv8_cancel_terminate")
	reg(&fHeapStats, "sv8_heap_stats")
	reg(&fLowMemory, "sv8_low_memory")
	reg(&fSnapCreate, "sv8_snapshot_create")
	reg(&fFree, "sv8_free")

	fl := Flags
	if e := os.Getenv("SWEEF_V8_FLAGS"); e != "" {
		fl += " " + e
	}
	version = cstring(fInit(sptr(fl), uintptr(len(fl))))
	fSetCallbacks(purego.NewCallback(hostTramp), purego.NewCallback(eventTramp))
	return nil
}

// ptr converts a C address to unsafe.Pointer without tripping go vet.
func ptr(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }

func cstring(p uintptr) string {
	if p == 0 {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(ptr(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(ptr(p)), n))
}

func sptr(s string) unsafe.Pointer { return unsafe.Pointer(unsafe.StringData(s)) }
