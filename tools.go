package main

import (
	_ "embed"
	"encoding/json"
)

//go:embed tools.json
var toolsJSON []byte

// toolGroups holds the tool schemas split by the service that must be
// configured for them to appear. git tools are always present.
type toolGroups struct {
	Git       []json.RawMessage `json:"git"`
	Context   []json.RawMessage `json:"context"`
	Jira      []json.RawMessage `json:"jira"`
	Bitbucket []json.RawMessage `json:"bitbucket"`
	Combined  []json.RawMessage `json:"combined"`
}

// toolList returns the tool schemas applicable to the current configuration,
// mirroring the gating in src/index.ts (git always; get_dev_context when either
// service is up; jira/bitbucket groups per service; complete_work when both).
func toolList() []json.RawMessage {
	var g toolGroups
	if err := json.Unmarshal(toolsJSON, &g); err != nil {
		logf("tool schema parse error: %v", err)
		return []json.RawMessage{}
	}
	out := make([]json.RawMessage, 0, 16)
	out = append(out, g.Git...)
	if jira != nil || bitbucket != nil {
		out = append(out, g.Context...)
	}
	if jira != nil {
		out = append(out, g.Jira...)
	}
	if bitbucket != nil {
		out = append(out, g.Bitbucket...)
	}
	if jira != nil && bitbucket != nil {
		out = append(out, g.Combined...)
	}
	return out
}
