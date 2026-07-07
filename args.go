package main

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// ── Argument coercion helpers ────────────────────────────────────────────────
// MCP arguments arrive as a map[string]any decoded from JSON, so numbers are
// float64 and absent keys are missing. These mirror the loose access the
// TypeScript implementation got for free.

func has(m map[string]any, k string) bool { _, ok := m[k]; return ok }

func argString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func argBool(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// argBoolDefault returns the bool value if present, else def.
func argBoolDefault(m map[string]any, k string, def bool) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return def
}

func argInt(m map[string]any, k string) int {
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
		fmt.Sscanf(v, "%d", &i)
		return i
	}
	return 0
}

func argIntDefault(m map[string]any, k string, def int) int {
	if !has(m, k) {
		return def
	}
	return argInt(m, k)
}

func argIntPtr(m map[string]any, k string) *int {
	if !has(m, k) {
		return nil
	}
	i := argInt(m, k)
	return &i
}

func argFloat(m map[string]any, k string) float64 {
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

func argFloatPtr(m map[string]any, k string) *float64 {
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

func argStrSlice(m map[string]any, k string) []string {
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

// argStrSlicePtr distinguishes an absent array from an empty one (empty array
// is a meaningful "clear" signal for labels/reviewers).
func argStrSlicePtr(m map[string]any, k string) *[]string {
	if !has(m, k) {
		return nil
	}
	s := argStrSlice(m, k)
	if s == nil {
		s = []string{}
	}
	return &s
}

func argMap(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

// ── Alias normalisation (project→projectKey, repo→repoSlug) ───────────────────

func normalizeBitbucketArgs(args map[string]any) map[string]any {
	if _, ok := args["projectKey"].(string); !ok {
		if p, ok := args["project"].(string); ok {
			args["projectKey"] = p
		}
	}
	if _, ok := args["repoSlug"].(string); !ok {
		if r, ok := args["repo"].(string); ok {
			args["repoSlug"] = r
		}
	}
	return args
}

func normalizeJiraProjectArgs(args map[string]any) map[string]any {
	if _, ok := args["projectKey"].(string); !ok {
		if p, ok := args["project"].(string); ok {
			args["projectKey"] = p
		}
	}
	return args
}

func normalizeJiraMutateArgs(args map[string]any) map[string]any {
	normalizeJiraProjectArgs(args)
	if create := argMap(args, "create"); create != nil {
		if _, ok := create["projectKey"].(string); !ok {
			if p, ok := create["project"].(string); ok {
				create["projectKey"] = p
			}
		}
	}
	return args
}

// ── Shared regex ─────────────────────────────────────────────────────────────

var jiraKeyRe = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`)

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// validateAttachmentArgs enforces the same numeric ranges as the TS server,
// returning a protocol-level InvalidParams error on violation.
func validateAttachmentArgs(a map[string]any) *rpcError {
	bad := func(msg string) *rpcError { return &rpcError{Code: codeInvalidParams, Message: msg} }
	if has(a, "frames") {
		f := argFloat(a, "frames")
		if f < 1 || f > 60 {
			return bad("frames must be a number between 1 and 60.")
		}
	}
	if has(a, "start") {
		if argFloat(a, "start") < 0 {
			return bad("start must be a non-negative number of seconds.")
		}
	}
	if has(a, "end") {
		if argFloat(a, "end") <= 0 {
			return bad("end must be a positive number of seconds.")
		}
	}
	if has(a, "start") && has(a, "end") {
		if argFloat(a, "end") <= argFloat(a, "start") {
			return bad("end must be greater than start.")
		}
	}
	if has(a, "mode") {
		m := argString(a, "mode")
		if m != "uniform" && m != "scenes" {
			return bad(`mode must be "uniform" or "scenes".`)
		}
	}
	if has(a, "maxDimension") {
		d := argFloat(a, "maxDimension")
		if d < 64 || d > 4096 {
			return bad("maxDimension must be a number between 64 and 4096.")
		}
	}
	if has(a, "quality") {
		q := argFloat(a, "quality")
		if q < 1 || q > 100 {
			return bad("quality must be a number between 1 and 100.")
		}
	}
	if has(a, "sceneThreshold") {
		s := argFloat(a, "sceneThreshold")
		if s <= 0 || s > 1 {
			return bad("sceneThreshold must be a number in (0, 1].")
		}
	}
	return nil
}
