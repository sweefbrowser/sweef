//go:build !linux || sweef_v8

package v8shim

import (
	"runtime"
	"testing"
)

// Isolates with and without a startup snapshot come and go in one process (the engine boots from a snapshot, the
// code-cache producer and the snapshot-less fallback do not). V8 shares the read-only heap between isolates, so
// the order they are created and disposed in must not matter.
func TestSnapshotAndPlainIsolatesInterleave(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := Load(); err != nil {
		t.Skip(err)
	}
	blob, err := CreateSnapshot(`globalThis.fromSnapshot = 41 + 1;`, "snap.js", true)
	if err != nil {
		t.Fatal(err)
	}
	run := func(withSnap bool) {
		o := Options{}
		if withSnap {
			o.Snapshot = blob
		}
		iso, err := NewIsolate(o)
		if err != nil {
			t.Fatal(err)
		}
		defer iso.Close()
		c, err := iso.NewContext()
		if err != nil {
			t.Fatal(err)
		}
		v, err := c.Eval(`typeof fromSnapshot === 'number' ? fromSnapshot : -1`, "t.js")
		if err != nil {
			t.Fatal(err)
		}
		want := int64(-1)
		if withSnap {
			want = 42
		}
		if v.Int != want {
			t.Fatalf("snapshot=%v: got %d want %d", withSnap, v.Int, want)
		}
	}
	for _, order := range [][]bool{{true, true, false, true}, {false, true, false, true, true}, {true, false, false, true, false, true}} {
		for _, w := range order {
			run(w)
		}
	}
}

// Isolates on one snapshot, and plain ones, may each be alive in numbers at the same time as long as the two
// kinds are not mixed (see Options).
func TestSameKindIsolatesCoexist(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := Load(); err != nil {
		t.Skip(err)
	}
	blob, err := CreateSnapshot(`globalThis.fromSnapshot = 7;`, "snap.js", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, withSnap := range []bool{true, false} {
		var live []*Isolate
		for k := 0; k < 5; k++ {
			o := Options{}
			if withSnap {
				o.Snapshot = blob
			}
			iso, err := NewIsolate(o)
			if err != nil {
				t.Fatal(err)
			}
			live = append(live, iso)
			c, err := iso.NewContext()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Eval(`1 + 1`, "t.js"); err != nil {
				t.Fatalf("isolate %d: %v", k, err)
			}
			if k == 2 {
				live[0].Close()
				live = live[1:]
			}
		}
		for _, i := range live {
			i.Close()
		}
	}
}
