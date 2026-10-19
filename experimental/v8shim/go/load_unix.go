//go:build !windows

package v8shim

import "github.com/ebitengine/purego"

func openLib(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

func libFile() string {
	if isDarwin {
		return "libsweef_v8shim.dylib"
	}
	return "libsweef_v8shim.so"
}

func closeLib(h uintptr) { _ = purego.Dlclose(h) }
