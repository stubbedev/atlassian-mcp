package arg

import (
	"testing"
)

func TestArgCoercion(t *testing.T) {
	m := map[string]any{"f": float64(3.7), "s": "hello", "b": true, "arr": []any{"a", "b"}}
	if Int(m, "f") != 3 {
		t.Error("argInt float64 failed")
	}
	if Float(m, "f") != 3.7 {
		t.Error("argFloat failed")
	}
	if String(m, "s") != "hello" {
		t.Error("argString failed")
	}
	if !Bool(m, "b") {
		t.Error("argBool failed")
	}
	if got := StrSlice(m, "arr"); len(got) != 2 || got[0] != "a" {
		t.Errorf("argStrSlice failed: %v", got)
	}
	if IntDefault(m, "missing", 9) != 9 {
		t.Error("argIntDefault failed")
	}
	if StrSlicePtr(m, "missing") != nil {
		t.Error("argStrSlicePtr should be nil when absent")
	}
	if p := StrSlicePtr(map[string]any{"x": []any{}}, "x"); p == nil || len(*p) != 0 {
		t.Error("argStrSlicePtr should be empty-non-nil for empty array")
	}
}

func TestArgStrSliceStringCoercion(t *testing.T) {
	// lone string → single-element slice (not silently dropped)
	if got := StrSlice(map[string]any{"c": "Frontend"}, "c"); len(got) != 1 || got[0] != "Frontend" {
		t.Errorf("string coercion: got %v", got)
	}
	// empty string → empty slice (clear), never [""]
	if got := StrSlice(map[string]any{"c": ""}, "c"); len(got) != 0 {
		t.Errorf("empty string should clear, got %v", got)
	}
	// real array still works
	if got := StrSlice(map[string]any{"c": []any{"a", "b"}}, "c"); len(got) != 2 {
		t.Errorf("array: got %v", got)
	}
	// absent → nil (skip)
	if got := StrSlice(map[string]any{}, "c"); got != nil {
		t.Errorf("absent should be nil, got %v", got)
	}
}
