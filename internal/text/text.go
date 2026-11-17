package text

import (
	"fmt"
	"strconv"
)

// Page returns the window [offset, offset+limit) of text, and — when there
// is more — a hint naming the exact argument that fetches the rest. A truncated
// response that does not say how to continue is a dead end: the caller either
// guesses an argument or silently works from half the content.
func Page(text string, offset, limit int, offsetArg, limitArg string) string {
	if limit <= 0 {
		limit = 8000
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(text) {
		offset = len(text)
	}
	end := min(offset+limit, len(text))
	chunk := text[offset:end]
	remaining := len(text) - end
	if remaining == 0 {
		if offset == 0 {
			return text
		}
		return chunk
	}
	return chunk + fmt.Sprintf("\n\n... (truncated, %d more chars — pass %s=%d for the next chunk, or raise %s)", remaining, offsetArg, end, limitArg)
}

func Unique(in []string) []string {
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

// ── helpers ──────────────────────────────────────────────────────────────────

func Or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// Pluralize renders "1 file" / "2 files".
func Pluralize(n int, one, many string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + one
	}
	return strconv.Itoa(n) + " " + many
}
