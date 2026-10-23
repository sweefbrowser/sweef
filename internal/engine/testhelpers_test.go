package engine

import (
	"encoding/json"
	"testing"
)

// evalJSON evaluates js in tab and decodes the JSON result into out (nil to ignore it).
func evalJSON(t *testing.T, tab *Tab, js string, out any) {
	t.Helper()
	raw, err := tab.Evaluate(js)
	if err != nil {
		t.Fatalf("evaluate: %v\n%s", err, js)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
}

// jsString quotes s as a JavaScript string literal.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
