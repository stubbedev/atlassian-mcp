package text

import (
	"strings"
	"testing"
)

func TestPageText(t *testing.T) {
	text := strings.Repeat("x", 100)
	if got := Page(text, 0, 200, "charOffset", "maxChars"); got != text {
		t.Error("short text should come back whole")
	}
	got := Page(text, 0, 40, "charOffset", "maxChars")
	if !strings.HasPrefix(got, strings.Repeat("x", 40)) {
		t.Error("wrong window")
	}
	// A truncated response must name the argument that continues it.
	if !strings.Contains(got, "charOffset=40") || !strings.Contains(got, "maxChars") {
		t.Errorf("no continuation hint: %q", got)
	}
	if tail := Page(text, 40, 60, "charOffset", "maxChars"); tail != strings.Repeat("x", 60) {
		t.Errorf("tail = %q", tail)
	}
}
