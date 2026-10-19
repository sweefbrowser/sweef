//go:build !linux || sweef_v8

package v8shim

import "github.com/ebitengine/purego"

func registerLibFunc(fptr any, h uintptr, name string) { purego.RegisterLibFunc(fptr, h, name) }

func newCallback(f any) uintptr { return purego.NewCallback(f) }
