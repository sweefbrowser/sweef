//go:build windows

package v8shim

import "syscall"

func openLib(path string) (uintptr, error) {
	h, err := syscall.LoadLibrary(path)
	return uintptr(h), err
}

func libFile() string { return "sweef_v8shim.dll" }

func closeLib(h uintptr) {}
