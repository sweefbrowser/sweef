package engine

// jsrt_v8_conv.go: value conversion between the engine's plain Go data and the V8 shim.
//
// The host table (registerHost) was written against QuickJS, where a JS number arrives as int (int32 range) or
// float64, a string as string, null as nil, undefined as quickjs.Undefined, and an object or array as a
// *quickjs.Object that normalizeJS turns into map[string]any / []any with float64 numbers. The V8 runtime hands
// the very same Go types to the very same closures, so none of them is ported: ints and doubles and short
// strings cross the bridge as tagged scalars, objects and arrays as JSON text.

import (
	"encoding/json"
	"math"
	"time"

	"github.com/sweefbrowser/sweef/internal/v8shim"
	"modernc.org/quickjs"
)

// fromV8 converts a result the shim copied out of V8.
func fromV8(v v8shim.Value) any {
	switch v.Kind {
	case v8shim.Undefined:
		return quickjs.Undefined{}
	case v8shim.Null:
		return nil
	case v8shim.Bool:
		return v.Int != 0
	case v8shim.Int:
		return int(v.Int)
	case v8shim.Double:
		return v.Float
	case v8shim.String:
		return v.Str
	case v8shim.JSON:
		var x any
		if json.Unmarshal([]byte(v.Str), &x) == nil {
			return x
		}
		return v.Str
	case v8shim.Bytes:
		return v.Bytes
	}
	return nil
}

// toV8Args converts engine arguments for a call into JS.
func toV8Args(args []any) []any {
	for i, a := range args {
		switch a.(type) {
		case quickjs.Undefined:
			out := make([]any, len(args))
			copy(out, args)
			for k := i; k < len(out); k++ {
				if _, ok := out[k].(quickjs.Undefined); ok {
					out[k] = v8shim.JSUndefined{}
				}
			}
			return out
		}
	}
	return args
}

// hostArg converts argument i of a JS -> Go host call into the Go type the QuickJS runtime delivers.
func hostArg(c *v8shim.Call, i int) any {
	switch c.Kind(i) {
	case v8shim.Undefined:
		return quickjs.Undefined{}
	case v8shim.Null:
		return nil
	case v8shim.Bool:
		return c.Bool(i)
	case v8shim.Int:
		return int(c.Int(i))
	case v8shim.Double:
		return c.Float(i)
	case v8shim.String:
		return c.Str(i)
	case v8shim.JSON:
		var x any
		if json.Unmarshal([]byte(c.StrView(i)), &x) == nil {
			return x
		}
		return c.Str(i)
	case v8shim.Bytes:
		return append([]byte(nil), c.View(i)...)
	}
	return nil
}

// hostRet sets the result of a host call from what the closure returned, mirroring how modernc quickjs turns a
// Go value into a JS value (jsValue): nil is null, Undefined is undefined, numbers and strings are scalars,
// maps, slices and structs are JSON.
func hostRet(c *v8shim.Call, v any) error {
	switch x := v.(type) {
	case nil:
		c.RetNull()
	case quickjs.Undefined:
		c.RetUndefined()
	case bool:
		c.RetBool(x)
	case int:
		c.RetInt(int64(x))
	case int64:
		c.RetInt(x)
	case int32:
		c.RetInt(int64(x))
	case uint8:
		c.RetInt(int64(x))
	case uint16:
		c.RetInt(int64(x))
	case uint32:
		c.RetInt(int64(x))
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 && !(x == 0 && math.Signbit(x)) {
			c.RetInt(int64(x))
		} else {
			c.RetFloat(x)
		}
	case float32:
		c.RetFloat(float64(x))
	case string:
		c.RetString(x)
	case error:
		c.RetString(x.Error())
	case []int64:
		if a, ok := int32s(x); ok {
			c.RetInt32s(a)
			return nil
		}
		return retJSON(c, v)
	case []int:
		a := make([]int32, len(x))
		for i, n := range x {
			if n < math.MinInt32 || n > math.MaxInt32 {
				return retJSON(c, v)
			}
			a[i] = int32(n)
		}
		c.RetInt32s(a)
	case []int32:
		c.RetInt32s(x)
	case []byte:
		a := make([]int32, len(x))
		for i, b := range x {
			a[i] = int32(b)
		}
		c.RetInt32s(a)
	default:
		if c.RetTree(v) { // maps and slices as a native tree: no JSON text, no escaping of a big body
			return nil
		}
		return retJSON(c, v)
	}
	return nil
}

func int32s(x []int64) ([]int32, bool) {
	a := make([]int32, len(x))
	for i, n := range x {
		if n < math.MinInt32 || n > math.MaxInt32 {
			return nil, false
		}
		a[i] = int32(n)
	}
	return a, true
}

func retJSON(c *v8shim.Call, v any) error {
	var t0 time.Time
	if jsStatsOn {
		t0 = time.Now()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if jsStatsOn {
		jsSt.jsonCalls.Add(1)
		jsSt.jsonBytes.Add(int64(len(b)))
		jsSt.jsonNs.Add(int64(time.Since(t0)))
	}
	c.RetJSON(string(b))
	return nil
}
