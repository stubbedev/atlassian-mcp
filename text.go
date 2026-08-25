package main

import "fmt"

// pageText returns the window [offset, offset+limit) of text, and — when there
// is more — a hint naming the exact argument that fetches the rest. A truncated
// response that does not say how to continue is a dead end: the caller either
// guesses an argument or silently works from half the content.
func pageText(text string, offset, limit int, offsetArg, limitArg string) string {
	if limit <= 0 {
		limit = 8000
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(text) {
		offset = len(text)
	}
	end := offset + limit
	if end > len(text) {
		end = len(text)
	}
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
