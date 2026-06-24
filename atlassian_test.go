package main

import (
	"strings"
	"testing"
)

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
