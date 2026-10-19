package v8shim

import (
	"math"
	"sort"
	"unsafe"
)

// Typed trees. A host function that returns a map or a slice, and an engine call that passes one, would
// otherwise round-trip through JSON text: marshal (escaping every string), copy, JSON.parse. The tree form
// hands V8 the structure directly (tags and pointers into Go memory), so a 10 MB response body inside a result
// costs one UTF-8 to V8 string conversion. Values it cannot express (structs, byte slices, ...) make buildVal
// report false and the caller falls back to JSON.

const (
	tObject = 12
	tArray  = 13
)

// rawEntry mirrors the Rust Entry: one property of an object.
type rawEntry struct {
	key  uintptr
	klen uintptr
	val  rawVal
}

// buildVal converts v into the wire form. Everything it points at is appended to pin, which the caller keeps
// alive until V8 has consumed the value.
func buildVal(v any, pin *[]any) (rawVal, bool) {
	switch x := v.(type) {
	case nil:
		return rawVal{tag: tNull}, true
	case bool:
		r := rawVal{tag: tBool}
		if x {
			r.u = 1
		}
		return r, true
	case int:
		return rawVal{tag: tInt, u: uint64(int64(x))}, true
	case int32:
		return rawVal{tag: tInt, u: uint64(int64(x))}, true
	case int64:
		return rawVal{tag: tInt, u: uint64(x)}, true
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<31 && !(x == 0 && math.Signbit(x)) {
			return rawVal{tag: tInt, u: uint64(int64(x))}, true
		}
		return rawVal{tag: tDouble, u: math.Float64bits(x)}, true
	case string:
		r := rawVal{tag: tString, n: uint32(len(x))}
		if len(x) > 0 {
			if uint64(len(x)) > math.MaxUint32 {
				return rawVal{}, false
			}
			r.u = uint64(uintptr(sptr(x)))
			*pin = append(*pin, x)
		}
		return r, true
	case []any:
		items := make([]rawVal, len(x))
		for i, e := range x {
			rv, ok := buildVal(e, pin)
			if !ok {
				return rawVal{}, false
			}
			items[i] = rv
		}
		return arrayOf(items, pin), true
	case []string:
		items := make([]rawVal, len(x))
		for i, e := range x {
			rv, ok := buildVal(e, pin)
			if !ok {
				return rawVal{}, false
			}
			items[i] = rv
		}
		return arrayOf(items, pin), true
	case []int:
		items := make([]rawVal, len(x))
		for i, e := range x {
			items[i] = rawVal{tag: tInt, u: uint64(int64(e))}
		}
		return arrayOf(items, pin), true
	case []int64:
		items := make([]rawVal, len(x))
		for i, e := range x {
			items[i] = rawVal{tag: tInt, u: uint64(e)}
		}
		return arrayOf(items, pin), true
	case []float64:
		items := make([]rawVal, len(x))
		for i, e := range x {
			rv, _ := buildVal(e, pin)
			items[i] = rv
		}
		return arrayOf(items, pin), true
	case []map[string]any:
		items := make([]rawVal, len(x))
		for i, e := range x {
			rv, ok := buildVal(e, pin)
			if !ok {
				return rawVal{}, false
			}
			items[i] = rv
		}
		return arrayOf(items, pin), true
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // the order json.Marshal would have produced
		ents := make([]rawEntry, len(keys))
		for i, k := range keys {
			rv, ok := buildVal(x[k], pin)
			if !ok {
				return rawVal{}, false
			}
			ents[i] = rawEntry{klen: uintptr(len(k)), val: rv}
			if len(k) > 0 {
				ents[i].key = uintptr(sptr(k))
				*pin = append(*pin, k)
			}
		}
		r := rawVal{tag: tObject, n: uint32(len(ents))}
		if len(ents) > 0 {
			r.u = uint64(uintptr(unsafe.Pointer(&ents[0])))
			*pin = append(*pin, ents)
		}
		return r, true
	}
	return rawVal{}, false
}

func arrayOf(items []rawVal, pin *[]any) rawVal {
	r := rawVal{tag: tArray, n: uint32(len(items))}
	if len(items) > 0 {
		r.u = uint64(uintptr(unsafe.Pointer(&items[0])))
		*pin = append(*pin, items)
	}
	return r
}

// RetTree sets the result of a host call to the object or array v. It reports false, leaving the result
// untouched, when v holds something a tree cannot express (the caller then uses RetJSON).
func (c *Call) RetTree(v any) bool {
	c.pins = c.pins[:0]
	rv, ok := buildVal(v, &c.pins)
	if !ok {
		c.pins = c.pins[:0]
		return false
	}
	*c.ret = rv
	return true
}
