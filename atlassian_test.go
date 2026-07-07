package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRootsFromHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Mcp-Root", "file:///srv/a")
	h.Add("X-Mcp-Roots", "/srv/b, /srv/c")

	roots := rootsFromHeaders(h)
	want := []string{"/srv/b", "/srv/c", "/srv/a"} // order follows rootHeaders precedence (X-Mcp-Roots before X-Mcp-Root)
	if len(roots) != len(want) {
		t.Fatalf("got %d roots, want %d: %+v", len(roots), len(want), roots)
	}
	for i, w := range want {
		if roots[i].path != w {
			t.Errorf("root[%d].path = %q, want %q", i, roots[i].path, w)
		}
	}
	if got := rootsFromHeaders(http.Header{}); got != nil {
		t.Errorf("empty headers should yield nil, got %+v", got)
	}
}

func TestHeaderRootsAreAuthoritative(t *testing.T) {
	// Client did NOT advertise roots capability, and send would fail — header
	// roots must still resolve without a roots/list round-trip.
	s := &sessionState{
		caps: clientCaps{roots: false},
		send: func(string, any) (json.RawMessage, error) {
			t.Fatal("roots/list must not be called when header roots are set")
			return nil, nil
		},
	}
	s.setHeaderRoots([]rootEntry{{uri: "file:///srv/x", path: "/srv/x"}})

	if got := s.repoRoot(); got != "/srv/x" {
		t.Fatalf("repoRoot = %q, want /srv/x", got)
	}
	// list_changed must not clear header-pinned roots.
	s.invalidateRoots()
	if got := s.resolveRepo(""); got != "/srv/x" {
		t.Errorf("header roots cleared by invalidate: %q", got)
	}
	// A basename arg still resolves against header roots.
	if got := s.resolveRepo("x"); got != "/srv/x" {
		t.Errorf("basename match against header roots: %q", got)
	}
}

func fakeRootsSession(rootsJSON string) *sessionState {
	return &sessionState{
		caps: clientCaps{roots: true},
		send: func(method string, params any) (json.RawMessage, error) {
			return json.RawMessage(rootsJSON), nil
		},
	}
}

func TestResolveRepo(t *testing.T) {
	twoRoots := `{"roots":[{"uri":"file:///home/u/wt-main","name":"main"},{"uri":"file:///home/u/feature-x","name":"feature"}]}`

	// Absolute repoPath passes through untouched (no roots query needed).
	noRoots := &sessionState{caps: clientCaps{roots: false}}
	if got := noRoots.resolveRepo("/abs/repo"); got != "/abs/repo" {
		t.Errorf("abs passthrough: got %q", got)
	}
	// No repoPath, no roots capability → "".
	if got := noRoots.resolveRepo(""); got != "" {
		t.Errorf("no roots: got %q", got)
	}
	// Relative/basename matches a root.
	if got := fakeRootsSession(twoRoots).resolveRepo("feature-x"); got != "/home/u/feature-x" {
		t.Errorf("basename match: got %q", got)
	}
	if got := fakeRootsSession(twoRoots).resolveRepo("feature"); got != "/home/u/feature-x" {
		t.Errorf("name match: got %q", got)
	}
	// Unmatched relative arg → returned verbatim (best effort).
	if got := fakeRootsSession(twoRoots).resolveRepo("nope"); got != "nope" {
		t.Errorf("unmatched: got %q", got)
	}
	// No repoPath, multiple roots, none are git repos → first root path.
	if got := fakeRootsSession(twoRoots).resolveRepo(""); got != "/home/u/wt-main" {
		t.Errorf("primary root: got %q", got)
	}
	// Single root.
	one := `{"roots":[{"uri":"file:///home/u/only","name":"only"}]}`
	if got := fakeRootsSession(one).resolveRepo(""); got != "/home/u/only" {
		t.Errorf("single root: got %q", got)
	}
}

func TestParseBitbucketRemote(t *testing.T) {
	cases := []struct {
		in, pk, rs string
		nil        bool
	}{
		{in: "ssh://git@bb.example.com/PROJ/repo.git", pk: "PROJ", rs: "repo"},
		{in: "ssh://git@bb.example.com:7999/PROJ/repo", pk: "PROJ", rs: "repo"},
		{in: "git@bb.example.com:PROJ/repo.git", pk: "PROJ", rs: "repo"},
		{in: "https://bb.example.com/scm/PROJ/repo.git", pk: "PROJ", rs: "repo"},
		{in: "https://bb.example.com/scm/proj/repo", pk: "proj", rs: "repo"},
		{in: "https://github.com/owner/repo.git", nil: true},
	}
	for _, c := range cases {
		got := parseBitbucketRemote(c.in)
		if c.nil {
			if got != nil {
				t.Errorf("%s: expected nil, got %+v", c.in, got)
			}
			continue
		}
		if got == nil || got.projectKey != c.pk || got.repoSlug != c.rs {
			t.Errorf("%s: got %+v, want %s/%s", c.in, got, c.pk, c.rs)
		}
	}
}

func TestBuildJQL(t *testing.T) {
	if j, _ := buildJQL("", "project = FOO", "", "", "", ""); j != "project = FOO" {
		t.Errorf("jql passthrough failed: %q", j)
	}
	j, err := buildJQL("login bug", "", "FOO", "Open", "", "Bug")
	if err != nil {
		t.Fatal(err)
	}
	want := `text ~ "login bug" AND project = "FOO" AND status = "Open" AND issuetype = "Bug" ORDER BY updated DESC`
	if j != want {
		t.Errorf("got %q want %q", j, want)
	}
	if _, err := buildJQL("", "", "", "", "", ""); err == nil {
		t.Error("expected error for no clauses")
	}
	if _, err := buildJQL("", strings.Repeat("x", 2001), "", "", "", ""); err == nil {
		t.Error("expected error for too-long jql")
	}
}

func TestSlugifyBranchName(t *testing.T) {
	cases := []struct{ key, summary, typ, want string }{
		{"FOO-1", "Fix the Login Bug!", "Bug", "bugfix/FOO-1-fix-the-login-bug"},
		{"FOO-2", "Add feature", "Story", "feature/FOO-2-add-feature"},
		{"FOO-3", "Quick task", "Sub-task", "task/FOO-3-quick-task"},
		{"FOO-4", "Hot one", "Hotfix", "hotfix/FOO-4-hot-one"},
	}
	for _, c := range cases {
		if got := slugifyBranchName(c.key, c.summary, c.typ); got != c.want {
			t.Errorf("%s/%s: got %q want %q", c.key, c.typ, got, c.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{500, "500 B"},
		{2048, "2.0 KB"},
		{5 * 1024 * 1024, "5.0 MB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("%d: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestFormatDiff(t *testing.T) {
	d := &bbDiff{FromHash: "aaa", ToHash: "bbb"}
	d.Diffs = append(d.Diffs, struct {
		Source *struct {
			ToString string `json:"toString"`
		} `json:"source"`
		Destination *struct {
			ToString string `json:"toString"`
		} `json:"destination"`
		Hunks []bbDiffHunk `json:"hunks"`
	}{
		Source: &struct {
			ToString string `json:"toString"`
		}{ToString: "a.go"},
		Destination: &struct {
			ToString string `json:"toString"`
		}{ToString: "a.go"},
		Hunks: []bbDiffHunk{{
			SourceLine: 1, SourceSpan: 2, DestinationLine: 1, DestinationSpan: 3,
			Segments: []bbDiffSegment{
				{Type: "CONTEXT", Lines: []struct {
					Line        string `json:"line"`
					Source      int    `json:"source"`
					Destination int    `json:"destination"`
				}{{Line: "ctx"}}},
				{Type: "ADDED", Lines: []struct {
					Line        string `json:"line"`
					Source      int    `json:"source"`
					Destination int    `json:"destination"`
				}{{Line: "new"}}},
				{Type: "REMOVED", Lines: []struct {
					Line        string `json:"line"`
					Source      int    `json:"source"`
					Destination int    `json:"destination"`
				}{{Line: "old"}}},
			},
		}},
	})
	out := formatDiff(d, 8000)
	for _, want := range []string{"# fromHash=aaa toHash=bbb", "--- a/a.go", "+++ b/a.go", "@@ -1,2 +1,3 @@", " ctx", "+new", "-old"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatDiff missing %q in:\n%s", want, out)
		}
	}
}

func TestArgCoercion(t *testing.T) {
	m := map[string]any{"f": float64(3.7), "s": "hello", "b": true, "arr": []any{"a", "b"}}
	if argInt(m, "f") != 3 {
		t.Error("argInt float64 failed")
	}
	if argFloat(m, "f") != 3.7 {
		t.Error("argFloat failed")
	}
	if argString(m, "s") != "hello" {
		t.Error("argString failed")
	}
	if !argBool(m, "b") {
		t.Error("argBool failed")
	}
	if got := argStrSlice(m, "arr"); len(got) != 2 || got[0] != "a" {
		t.Errorf("argStrSlice failed: %v", got)
	}
	if argIntDefault(m, "missing", 9) != 9 {
		t.Error("argIntDefault failed")
	}
	if argStrSlicePtr(m, "missing") != nil {
		t.Error("argStrSlicePtr should be nil when absent")
	}
	if p := argStrSlicePtr(map[string]any{"x": []any{}}, "x"); p == nil || len(*p) != 0 {
		t.Error("argStrSlicePtr should be empty-non-nil for empty array")
	}
}

func TestNormalizeAliases(t *testing.T) {
	bb := normalizeBitbucketArgs(map[string]any{"project": "ENG", "repo": "api"})
	if bb["projectKey"] != "ENG" || bb["repoSlug"] != "api" {
		t.Errorf("bitbucket alias failed: %v", bb)
	}
	jm := normalizeJiraMutateArgs(map[string]any{"create": map[string]any{"project": "FOO"}})
	if c := jm["create"].(map[string]any); c["projectKey"] != "FOO" {
		t.Errorf("jira create alias failed: %v", c)
	}
}

func TestValidateCommentText(t *testing.T) {
	if _, err := validateCommentText("  "); err == nil {
		t.Error("expected error for empty")
	}
	if _, err := validateCommentText("looks good 🚀"); err == nil {
		t.Error("expected error for emoji")
	}
	if v, err := validateCommentText("  ok  "); err != nil || v != "ok" {
		t.Errorf("got %q %v", v, err)
	}
}

func TestValidateSuggestionPlacement(t *testing.T) {
	if err := validateSuggestionPlacement("```suggestion\nx\n```"); err != nil {
		t.Errorf("valid suggestion rejected: %v", err)
	}
	if err := validateSuggestionPlacement("```suggestion\nx\n```\ntrailing"); err == nil {
		t.Error("expected error for trailing text after suggestion")
	}
	if err := validateSuggestionPlacement("no suggestion here"); err != nil {
		t.Errorf("plain text rejected: %v", err)
	}
}

func TestParseBitbucketErrorDetails(t *testing.T) {
	in := `{"errors":[{"message":"bad","context":"field"},{"message":"oops"}]}`
	if got := parseBitbucketErrorDetails(in); got != "field: bad | oops" {
		t.Errorf("got %q", got)
	}
}

func TestParseJiraErrorDetails(t *testing.T) {
	in := `{"errorMessages":["top"],"errors":{"summary":"required"}}`
	if got := parseJiraErrorDetails(in); got != "top | summary: required" {
		t.Errorf("got %q", got)
	}
}

func TestRemoteMatchesBitbucketInstance(t *testing.T) {
	if !remoteMatchesBitbucketInstance("git@bb.example.com:P/r.git", "https://bb.example.com") {
		t.Error("should match")
	}
	if remoteMatchesBitbucketInstance("git@github.com:o/r.git", "https://bb.example.com") {
		t.Error("should not match")
	}
}

func TestToolListGating(t *testing.T) {
	jira, bitbucket = nil, nil
	if got := len(toolList()); got != 2 {
		t.Errorf("git-only should be 2 tools, got %d", got)
	}
	jira = &JiraClient{}
	if got := len(toolList()); got != 10 {
		t.Errorf("git+context+jira should be 10 tools, got %d", got)
	}
	bitbucket = &BitbucketClient{}
	if got := len(toolList()); got != 18 {
		t.Errorf("all should be 18 tools, got %d", got)
	}
	jira, bitbucket = nil, nil
}

func TestNamedList(t *testing.T) {
	// non-empty → [{"name":n}]
	if b, _ := json.Marshal(namedList([]string{"Frontend", "Backend"})); string(b) != `[{"name":"Frontend"},{"name":"Backend"}]` {
		t.Errorf("got %s", b)
	}
	// empty → [] not null, so update clears the field
	if b, _ := json.Marshal(namedList(nil)); string(b) != "[]" {
		t.Errorf("empty should marshal to [], got %s", b)
	}
}

func TestArgStrSliceStringCoercion(t *testing.T) {
	// lone string → single-element slice (not silently dropped)
	if got := argStrSlice(map[string]any{"c": "Frontend"}, "c"); len(got) != 1 || got[0] != "Frontend" {
		t.Errorf("string coercion: got %v", got)
	}
	// empty string → empty slice (clear), never [""]
	if got := argStrSlice(map[string]any{"c": ""}, "c"); len(got) != 0 {
		t.Errorf("empty string should clear, got %v", got)
	}
	// real array still works
	if got := argStrSlice(map[string]any{"c": []any{"a", "b"}}, "c"); len(got) != 2 {
		t.Errorf("array: got %v", got)
	}
	// absent → nil (skip)
	if got := argStrSlice(map[string]any{}, "c"); got != nil {
		t.Errorf("absent should be nil, got %v", got)
	}
}

func TestBuildTimeTracking(t *testing.T) {
	if buildTimeTracking(map[string]any{}) != nil {
		t.Error("no estimates should yield nil")
	}
	tt := buildTimeTracking(map[string]any{"originalEstimate": "3d", "remainingEstimate": "1d"})
	if tt["originalEstimate"] != "3d" || tt["remainingEstimate"] != "1d" {
		t.Errorf("got %v", tt)
	}
	if b, _ := json.Marshal(buildTimeTracking(map[string]any{"originalEstimate": "2h"})); string(b) != `{"originalEstimate":"2h"}` {
		t.Errorf("partial: got %s", b)
	}
}

func TestProjectKeyOf(t *testing.T) {
	for in, want := range map[string]string{"KON-12887": "KON", "ABC-1": "ABC", "nodash": "", "": ""} {
		if got := projectKeyOf(in); got != want {
			t.Errorf("projectKeyOf(%q)=%q want %q", in, got, want)
		}
	}
}
