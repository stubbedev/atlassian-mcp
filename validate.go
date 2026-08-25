package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// The MCP Go SDK's untyped Server.AddTool does not validate tools/call
// arguments against the tool's inputSchema — only the generic AddTool[In, Out]
// does. So the enums and required lists in tools.json are advisory on the wire:
// without the checks below, `resource="version"` or `action="close"` falls
// through to a handler's default branch and silently does something else.
//
// This validates what can be checked without breaking the loose coercion the
// handlers rely on (argInt accepts "5", argStrSlice accepts a bare string):
// required keys must be present and non-empty, and string values constrained by
// an enum must match one — case- and separator-insensitively, rewriting the
// argument to the canonical spelling so handlers only ever see exact values.

type schemaNode struct {
	Type       string                `json:"type"`
	Enum       []any                 `json:"enum"`
	Required   []string              `json:"required"`
	Properties map[string]schemaNode `json:"properties"`
}

var (
	argRulesOnce sync.Once
	argRules     map[string]schemaNode
)

func loadArgRules() {
	argRules = map[string]schemaNode{}
	var groups map[string][]json.RawMessage
	if err := json.Unmarshal(toolsJSON, &groups); err != nil {
		logf("arg rule parse error: %v", err)
		return
	}
	for _, tools := range groups {
		for _, raw := range tools {
			var t struct {
				Name        string     `json:"name"`
				InputSchema schemaNode `json:"inputSchema"`
			}
			if err := json.Unmarshal(raw, &t); err != nil {
				logf("arg rule parse error: %v", err)
				continue
			}
			argRules[t.Name] = t.InputSchema
		}
	}
}

// validateToolArgs checks args against the tool's schema and normalises enum
// spellings in place. Unknown tools pass through (runTool rejects them).
func validateToolArgs(name string, args map[string]any) *rpcError {
	argRulesOnce.Do(loadArgRules)
	schema, ok := argRules[name]
	if !ok {
		return nil
	}
	return checkNode(schema, name, "", args)
}

func checkNode(n schemaNode, tool, prefix string, args map[string]any) *rpcError {
	for _, key := range n.Required {
		if isBlankArg(args[key]) {
			return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf("%s: %s%s is required.", tool, prefix, key)}
		}
	}
	for key, prop := range n.Properties {
		v, present := args[key]
		if !present {
			continue
		}
		if len(prop.Enum) > 0 {
			s, isStr := v.(string)
			if isStr && s != "" {
				canonical, ok := matchEnum(prop.Enum, s)
				if !ok {
					return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf("%s: %s%s=%q is not valid. Use one of: %s.", tool, prefix, key, s, strings.Join(enumStrings(prop.Enum), ", "))}
				}
				args[key] = canonical
			}
		}
		if len(prop.Properties) > 0 || len(prop.Required) > 0 {
			if nested, ok := v.(map[string]any); ok {
				if rerr := checkNode(prop, tool, prefix+key+".", nested); rerr != nil {
					return rerr
				}
			}
		}
	}
	return nil
}

// isBlankArg reports whether a required argument is effectively absent.
func isBlankArg(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	}
	return false
}

// matchEnum resolves a value to its canonical enum spelling, tolerating case
// and -/_ differences ("needs-work" → "needs_work", "open" → "OPEN").
func matchEnum(enum []any, value string) (string, bool) {
	want := foldEnum(value)
	for _, e := range enum {
		s, ok := e.(string)
		if !ok {
			continue
		}
		if foldEnum(s) == want {
			return s, true
		}
	}
	return "", false
}

func foldEnum(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}

func enumStrings(enum []any) []string {
	out := make([]string, 0, len(enum))
	for _, e := range enum {
		out = append(out, fmt.Sprint(e))
	}
	return out
}
