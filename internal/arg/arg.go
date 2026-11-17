package arg

import (
	"encoding/json"
	"fmt"
)

// ── Argument coercion helpers ────────────────────────────────────────────────
// MCP arguments arrive as a map[string]any decoded from JSON, so numbers are
// float64 and absent keys are missing. These mirror the loose access the
// TypeScript implementation got for free.

func Has(m map[string]any, k string) bool { _, ok := m[k]; return ok }

func String(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func Bool(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// BoolDefault returns the bool value if present, else def.
func BoolDefault(m map[string]any, k string, def bool) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return def
}

func Int(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	case int:
		return v
	case string:
		var i int
		_, _ = fmt.Sscanf(v, "%d", &i)
		return i
	}
	return 0
}

func IntDefault(m map[string]any, k string, def int) int {
	if !Has(m, k) {
		return def
	}
	return Int(m, k)
}

func IntPtr(m map[string]any, k string) *int {
	if !Has(m, k) {
		return nil
	}
	i := Int(m, k)
	return &i
}

func Float(m map[string]any, k string) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case json.Number:
		f, _ := v.Float64()
		return f
	case int:
		return float64(v)
	}
	return 0
}

func FloatPtr(m map[string]any, k string) *float64 {
	switch v := m[k].(type) {
	case float64:
		f := v
		return &f
	case json.Number:
		f, _ := v.Float64()
		return &f
	case int:
		f := float64(v)
		return &f
	}
	return nil
}

func StrSlice(m map[string]any, k string) []string {
	// Coerce a lone string to a single-element slice so components:"Frontend"
	// behaves like components:["Frontend"] instead of silently dropping/clearing.
	if s, ok := m[k].(string); ok {
		if s == "" {
			return []string{} // clear, not a one-element [""]
		}
		return []string{s}
	}
	arr, ok := m[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// StrSlicePtr distinguishes an absent array from an empty one (empty array
// is a meaningful "clear" signal for labels/reviewers).
func StrSlicePtr(m map[string]any, k string) *[]string {
	if !Has(m, k) {
		return nil
	}
	s := StrSlice(m, k)
	if s == nil {
		s = []string{}
	}
	return &s
}

func Map(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}
