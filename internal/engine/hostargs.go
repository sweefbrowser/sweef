package engine

import "encoding/json"

// argF reads a numeric host-function argument as float64 (0 when absent or not a number).
func argF(a []any, i int) float64 {
	if i >= len(a) {
		return 0
	}
	switch v := a[i].(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case float64:
		return v
	}
	return 0
}

// itoa64 formats an int64 as a JSON number.
func itoa64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
