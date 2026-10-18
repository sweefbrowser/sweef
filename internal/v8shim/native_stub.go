//go:build linux && !sweef_v8

package v8shim

import "errors"

// This build has no V8 support: purego would make the Linux binary dynamically linked. Build with -tags sweef_v8
// for a binary that can load the V8 library (see the package comment).

func openLib(path string) (uintptr, error) {
	return 0, errors.New("this build has no V8 support (Linux builds are static; build with -tags sweef_v8 for a binary that can load the V8 library)")
}

func libFile() string { return "libsweef_v8shim.so" }

func closeLib(h uintptr) {}

func registerLibFunc(fptr any, h uintptr, name string) { panic("no V8 support in this build") }

func newCallback(f any) uintptr { panic("no V8 support in this build") }
