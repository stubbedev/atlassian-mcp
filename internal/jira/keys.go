package jira

import (
	"regexp"
)

// ── Shared regex ─────────────────────────────────────────────────────────────

var KeyRe = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`)
