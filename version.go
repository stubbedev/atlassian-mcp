package main

import (
	_ "embed"
	"encoding/json"
)

// package.json is the single source of truth for the version. It is embedded at
// build time so `go build`, `go install`, the Nix flake, and the release
// workflow all report the same version with no hardcoding and no -ldflags.
//
//go:embed package.json
var packageJSON []byte

// Version is the server version, parsed from the embedded package.json.
var Version = func() string {
	var p struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(packageJSON, &p) == nil && p.Version != "" {
		return p.Version
	}
	return "dev"
}()
