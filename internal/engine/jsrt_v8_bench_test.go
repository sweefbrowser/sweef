package engine

import (
	"testing"

	"github.com/sweefbrowser/sweef/internal/v8shim"
)

// BenchmarkV8BuildVM is the cost of a VM that is ready for a page: isolate from the startup snapshot, context,
// host binding (the QuickJS counterpart is BenchmarkQJSBuildVM).
func BenchmarkV8BuildVM(b *testing.B) {
	if err := v8shim.Load(); err != nil {
		b.Skip(err)
	}
	opts := DefaultOptions()
	opts.JSEngine = JSEngineV8
	e := New(opts)
	defer e.Close()
	if j, err := buildV8VM(opts); err != nil { // builds the snapshot once, outside the timing
		b.Fatal(err)
	} else {
		j.close()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j, err := buildV8VM(opts)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		j.close()
		b.StartTimer()
	}
}

func BenchmarkQJSBuildVM(b *testing.B) {
	opts := DefaultOptions()
	e := New(opts)
	defer e.Close()
	if j, err := buildQJSVM(opts); err != nil {
		b.Fatal(err)
	} else {
		j.close()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j, err := buildQJSVM(opts)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		j.close()
		b.StartTimer()
	}
}
