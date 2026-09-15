package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// TestParseRootList covers the path shapes that only ever arrive from GUI
// desktop clients: a ~ that no shell expanded, and a Windows drive path that
// url.Parse reads as a scheme rather than a path. Both used to resolve to "",
// which silently disables every repo-dependent tool.
func TestParseRootList(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	got := parseRootList("file:///srv/a, ~/code/x", `C:\repo`, "  ", "relative/path")
	want := []string{"/srv/a", filepath.Join(home, "code/x"), `C:\repo`}
	if len(got) != len(want) {
		t.Fatalf("got %d roots, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].path != w {
			t.Errorf("root[%d].path = %q, want %q", i, got[i].path, w)
		}
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
		send: func(_ string, _ any) (json.RawMessage, error) {
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
	out := formatDiff(d, 8000, 0)
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
	if got := len(toolList()); got != 1 {
		t.Errorf("git-only should be 1 tool, got %d", got)
	}
	jira = &JiraClient{}
	if got := len(toolList()); got != 8 {
		t.Errorf("git+context+jira should be 8 tools, got %d", got)
	}
	bitbucket = &BitbucketClient{}
	if got := len(toolList()); got != 15 {
		t.Errorf("all should be 15 tools, got %d", got)
	}
	jira, bitbucket = nil, nil
}

func TestNamedList(t *testing.T) {
	// non-empty → [{"name":n}]
	b, err := json.Marshal(namedList([]string{"Frontend", "Backend"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `[{"name":"Frontend"},{"name":"Backend"}]` {
		t.Errorf("got %s", b)
	}
	// empty → [] not null, so update clears the field
	b, err = json.Marshal(namedList(nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
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
	b, err := json.Marshal(buildTimeTracking(map[string]any{"originalEstimate": "2h"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"originalEstimate":"2h"}` {
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

// fieldCacheClient returns a client with its /field cache pre-seeded so the
// resolution helpers can be tested without HTTP.
func fieldCacheClient() *JiraClient {
	c := NewJiraClient("https://jira.example", "t")
	epicName := jiraField{ID: "customfield_10005", Name: "Epic Name", Custom: true}
	epicName.Schema.Custom = "com.pyxis.greenhopper.jira:gh-epic-label"
	points := jiraField{ID: "customfield_10002", Name: "Story Points", Custom: true}
	c.fields = []jiraField{epicName, points, {ID: "summary", Name: "Summary"}}
	c.fieldsCached = true
	return c
}

func TestResolveFieldID(t *testing.T) {
	c := fieldCacheClient()
	for arg, want := range map[string]string{
		"Epic Name":         "customfield_10005",
		"story points":      "customfield_10002",
		"customfield_10002": "customfield_10002",
		"summary":           "summary",
	} {
		got, err := c.resolveFieldID(arg)
		if err != nil || got != want {
			t.Errorf("resolveFieldID(%q) = %q, %v; want %q", arg, got, err, want)
		}
	}
	if _, err := c.resolveFieldID("Nope"); err == nil {
		t.Error("unknown field should error")
	}
	if id, _ := c.getEpicNameFieldID(); id != "customfield_10005" {
		t.Errorf("getEpicNameFieldID = %q", id)
	}
}

func TestApplyCustomFieldsAndErrorNaming(t *testing.T) {
	c := fieldCacheClient()
	fields := map[string]any{}
	if err := c.applyCustomFields(fields, map[string]any{"Story Points": 5.0}); err != nil {
		t.Fatal(err)
	}
	if fields["customfield_10002"] != 5.0 {
		t.Errorf("value not passed through: %+v", fields)
	}
	got := c.nameCustomFields("Field 'customfield_10005' is required.")
	if got != "Field 'customfield_10005 (Epic Name)' is required." {
		t.Errorf("nameCustomFields = %q", got)
	}
}

func TestCoerceFieldValue(t *testing.T) {
	opt := jiraFieldSchema{Type: "option"}
	multi := jiraFieldSchema{Type: "array", Items: "option"}
	labels := jiraFieldSchema{Type: "array", Items: "string"}

	if got := coerceFieldValue(opt, "A"); !reflect.DeepEqual(got, map[string]any{"value": "A"}) {
		t.Errorf("option scalar = %#v", got)
	}
	if got := coerceFieldValue(jiraFieldSchema{Type: "user"}, "jdoe"); !reflect.DeepEqual(got, map[string]any{"name": "jdoe"}) {
		t.Errorf("user scalar = %#v", got)
	}
	if got := coerceFieldValue(multi, []any{"A", "B"}); !reflect.DeepEqual(got, []any{map[string]any{"value": "A"}, map[string]any{"value": "B"}}) {
		t.Errorf("multi option = %#v", got)
	}
	// A single value for a multi-value field is wrapped into a list.
	if got := coerceFieldValue(multi, "A"); !reflect.DeepEqual(got, []any{map[string]any{"value": "A"}}) {
		t.Errorf("scalar into array field = %#v", got)
	}
	if got := coerceFieldValue(labels, []any{"a"}); !reflect.DeepEqual(got, []any{"a"}) {
		t.Errorf("string array = %#v", got)
	}
	// Raw Jira shapes and clears pass through untouched.
	raw := map[string]any{"id": "10401"}
	if got := coerceFieldValue(opt, raw); !reflect.DeepEqual(got, raw) {
		t.Errorf("raw object rewritten: %#v", got)
	}
	if got := coerceFieldValue(opt, nil); got != nil {
		t.Errorf("nil clear = %#v", got)
	}
	if got := coerceFieldValue(jiraFieldSchema{Type: "string"}, "x"); got != "x" {
		t.Errorf("string = %#v", got)
	}
	if got := coerceFieldValue(jiraFieldSchema{Type: "number"}, 5.0); got != 5.0 {
		t.Errorf("number = %#v", got)
	}
}

func TestSendHintAndAllowedHint(t *testing.T) {
	for _, tc := range []struct {
		schema jiraFieldSchema
		want   string
	}{
		{jiraFieldSchema{Type: "string"}, `"text"`},
		{jiraFieldSchema{Type: "number"}, "5"},
		{jiraFieldSchema{Type: "date"}, `"2026-01-31"`},
		{jiraFieldSchema{Type: "user"}, `"username"`},
		{jiraFieldSchema{Type: "array", Items: "option"}, `["<allowed value>"]`},
	} {
		if got := sendHint(tc.schema); got != tc.want {
			t.Errorf("sendHint(%+v) = %s, want %s", tc.schema, got, tc.want)
		}
	}
	if got := allowedHint(nil); got != "" {
		t.Errorf("no allowed values should render nothing, got %q", got)
	}
	many := make([]string, 25)
	for i := range many {
		many[i] = fmt.Sprintf("v%d", i)
	}
	got := allowedHint(many)
	if !strings.Contains(got, "...and 5 more") || strings.Contains(got, "v20") {
		t.Errorf("allowedHint should cap at 20: %q", got)
	}
}

func TestValidateToolArgs(t *testing.T) {
	// Unknown enum values used to fall through to a handler default.
	if rerr := validateToolArgs("bitbucket_pr_tasks", map[string]any{"prId": 1.0, "action": "close"}); rerr == nil {
		t.Error("action=close should be rejected")
	}
	if rerr := validateToolArgs("jira_search", map[string]any{"resource": "version"}); rerr == nil {
		t.Error("resource=version should be rejected")
	}
	// Case and separator differences are normalised, not rejected.
	args := map[string]any{"prId": 1.0, "action": "NEEDS-WORK"}
	if rerr := validateToolArgs("bitbucket_mutate", args); rerr != nil {
		t.Fatalf("needs-work should normalise: %v", rerr.Message)
	}
	if args["action"] != "needs_work" {
		t.Errorf("action = %v, want needs_work", args["action"])
	}
	// Required fields.
	if rerr := validateToolArgs("get_attachment", map[string]any{"attachmentId": "  "}); rerr == nil {
		t.Error("blank attachmentId should be rejected")
	}
	if rerr := validateToolArgs("get_attachment", map[string]any{"attachmentId": "12"}); rerr != nil {
		t.Errorf("valid args rejected: %v", rerr.Message)
	}
	// Nested objects are checked too.
	nested := map[string]any{"version": map[string]any{"action": "publish"}}
	if rerr := validateToolArgs("jira_mutate", nested); rerr == nil {
		t.Error("version.action=publish should be rejected")
	}
	if rerr := validateToolArgs("no_such_tool", map[string]any{"whatever": 1}); rerr != nil {
		t.Error("unknown tools should pass through to runTool")
	}
}

func TestMarkdownToJiraWiki(t *testing.T) {
	in := "## Heading\n" +
		"Some **bold** and `code` and [link](http://x/y).\n" +
		"- one\n" +
		"  - nested\n" +
		"1. first\n" +
		"```go\nx := **not bold**\n```\n"
	got, converted := markdownToJiraWiki(in)
	if !converted {
		t.Fatal("should report a conversion")
	}
	for _, want := range []string{"h2. Heading", "*bold*", "{{code}}", "[link|http://x/y]", "\n* one", "\n** nested", "\n# first", "{code:go}", "x := **not bold**"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Text that is already wiki markup must survive untouched, including
	// markdown-looking punctuation inside a wiki code block.
	wiki := "h2. Heading\n*bold* and {{code}} and [link|http://x/y]\n* one\n{code:go}\nx := **1** - 2\n{code}"
	if out, converted := markdownToJiraWiki(wiki); converted || out != wiki {
		t.Errorf("wiki markup was rewritten:\n%s", out)
	}
}

func TestMarkdownCodeFenceVariants(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain fence", "```go\nx\n```", "{code:go}\nx\n{code}"},
		{"lang with hash", "```c#\nx\n```", "{code:c#}\nx\n{code}"},
		{"info with extra words", "```shell script\nx\n```", "{code:shell}\nx\n{code}"},
		{"info with attributes", "```js copy startline=3\nx\n```", "{code:js}\nx\n{code}"},
		{"unrecognized lang kept", "```fantom-lang\nx\n```", "{code:fantom-lang}\nx\n{code}"},
		{"tilde fence", "~~~python\nprint(1)\n~~~", "{code:python}\nprint(1)\n{code}"},
		{"bare tilde fence", "~~~\nx\n~~~", "{code}\nx\n{code}"},
		{"four backticks", "````\n```\nx\n```\n````", "{code}\n```\nx\n```\n{code}"},
		{"no stray backticks", "````\nx\n````", "{code}\nx\n{code}"},
		{"closing fence trailing spaces", "```go\nx\n```   ", "{code:go}\nx\n{code}"},
		{"empty body", "```\n```", "{code}\n{code}"},
		{"unterminated runs to end", "```go\nx\nstill code", "{code:go}\nx\nstill code\n{code}"},
		{"crlf fences", "```go\r\nx\r\n```", "{code:go}\nx\n{code}"},
		{"indented fence keeps indent", "- item\n  ```go\n  x\n  ```", "* item\n  {code:go}\n  x\n  {code}"},
		{"inline code inside body untouched", "```go\n`not mono`\n```", "{code:go}\n`not mono`\n{code}"},
		{"trailing blank lines trimmed", "```go\nx\n\n\n```", "{code:go}\nx\n{code}"},
	} {
		got, converted := markdownToJiraWiki(tc.in)
		if !converted || got != tc.want {
			t.Errorf("%s:\nmarkdownToJiraWiki(%q) = %q, %v; want %q", tc.name, tc.in, got, converted, tc.want)
		}
	}
}

func TestMarkdownInlineCodeVariants(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		converted      bool
	}{
		{name: "single backticks", in: "use `make all` here", want: "use {{make all}} here", converted: true},
		{name: "double backticks", in: "use ``a`b`` here", want: "use {{a`b}} here", converted: true},
		{name: "double with space", in: "use `` `escaped` `` here", want: "use {{ `escaped` }} here", converted: true},
		{name: "unpaired backtick stays", in: "a ` b", want: "a ` b"},
		{name: "bold inside inline code stays", in: "`**x**`", want: "{{**x**}}", converted: true},
		{name: "inline across lines stays", in: "a ` b\nc ` d", want: "a ` b\nc ` d"},
	} {
		got, converted := markdownToJiraWiki(tc.in)
		if converted != tc.converted || got != tc.want {
			t.Errorf("%s:\nmarkdownToJiraWiki(%q) = %q, %v; want %q, %v", tc.name, tc.in, got, converted, tc.want, tc.converted)
		}
	}
}

func TestMarkdownTables(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		converted      bool
	}{
		{name: "simple table", in: "| a | b |\n| --- | --- |\n| 1 | 2 |", want: "|| a || b ||\n| 1 | 2 |", converted: true},
		{name: "no outer pipes", in: "a | b\n--- | ---\n1 | 2", want: "|| a || b ||\n| 1 | 2 |", converted: true},
		{name: "alignment dropped", in: "| a | b |\n| :-- | --: |\n| 1 | 2 |", want: "|| a || b ||\n| 1 | 2 |", converted: true},
		{name: "short rows padded", in: "| a | b |\n| --- | --- |\n| 1 |", want: "|| a || b ||\n| 1 |  |", converted: true},
		{name: "long rows clipped", in: "| a |\n| --- |\n| 1 | 2 |", want: "|| a ||\n| 1 |", converted: true},
		{name: "header only", in: "| a |\n| --- |", want: "|| a ||", converted: true},
		{name: "escaped pipe in cell", in: "| a \\| b |\n| --- |\n| c |", want: "|| a | b ||\n| c |", converted: true},
		{name: "cell count mismatch left alone", in: "| a |\n| --- | --- |\n| 1 |", want: "| a |\n| --- | --- |\n| 1 |"},
		{name: "wiki table passthrough", in: "|| a || b ||\n| 1 | 2 |", want: "|| a || b ||\n| 1 | 2 |"},
		{name: "pipes in prose left alone", in: "either | or\nnothing to see", want: "either | or\nnothing to see"},
	} {
		got, converted := markdownToJiraWiki(tc.in)
		if converted != tc.converted || got != tc.want {
			t.Errorf("%s:\nmarkdownToJiraWiki(%q) = %q, %v; want %q, %v", tc.name, tc.in, got, converted, tc.want, tc.converted)
		}
	}
	// Inline formatting inside cells survives the table rewrite.
	got, _ := markdownToJiraWiki("| `x` | **y** | [z](http://u) |\n| --- | --- | --- |")
	for _, want := range []string{"|| {{x}} || *y* || [z|http://u] ||"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// A `|` inside an inline code span does not split the cell.
	if got, _ := markdownToJiraWiki("| `a|b` |\n| --- |"); !strings.Contains(got, "{{a|b}}") {
		t.Errorf("code span with pipe broken by table scan: %q", got)
	}
}

func TestMarkdownQuotesAndEmphasis(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		converted      bool
	}{
		{name: "single line quote", in: "> note", want: "bq. note", converted: true},
		{name: "multi line quote", in: "> a\n> b", want: "{quote}\na\nb\n{quote}", converted: true},
		{name: "nested markers flatten", in: ">> deep", want: "bq. deep", converted: true},
		{name: "blank line inside quote", in: "> a\n>\n> b", want: "{quote}\na\n\nb\n{quote}", converted: true},
		{name: "two quote blocks", in: "> a\n\n> b", want: "bq. a\n\nbq. b", converted: true},
		{name: "heading inside quote block", in: "> ## H\n> body", want: "{quote}\nh2. H\nbody\n{quote}", converted: true},
		{name: "bullet inside quote block", in: "> - item", want: "{quote}\n* item\n{quote}", converted: true},
		{name: "emphasis inside quote", in: "> **x** and [l](http://u)", want: "bq. *x* and [l|http://u]", converted: true},
		{name: "quoted fence", in: "> ```go\n> x\n> ```", want: "{quote}\n{code:go}\nx\n{code}\n{quote}", converted: true},
		{name: "gt inside fence body literal", in: "```\n> literal\n```", want: "{code}\n> literal\n{code}", converted: true},
		{name: "bold italic", in: "***x***", want: "*_x_*", converted: true},
		{name: "bold italic underscores", in: "___x___", want: "*_x_*", converted: true},
		{name: "autolink", in: "see <https://a.b/c> now", want: "see [https://a.b/c|https://a.b/c] now", converted: true},
		{name: "paren ordered list", in: "1) first\n2) second", want: "# first\n# second", converted: true},
		{name: "dot ordered list unchanged path", in: "1. first", want: "# first", converted: true},
	} {
		got, converted := markdownToJiraWiki(tc.in)
		if converted != tc.converted || got != tc.want {
			t.Errorf("%s:\nmarkdownToJiraWiki(%q) = %q, %v; want %q, %v", tc.name, tc.in, got, converted, tc.want, tc.converted)
		}
	}
}

func TestMarkdownKitchenSink(t *testing.T) {
	in := "## Deploy notes\n" +
		"Run `make build` (see [docs](https://x/d)) or <https://x/d>.\n" +
		"- step **one**\n" +
		"  1) check `foo`\n" +
		"> warning: ***read this***\n" +
		"| env | status |\n" +
		"| --- | --- |\n" +
		"| prod | ~~down~~ |\n" +
		"```bash\nmake deploy # **literal**\n```"
	want := "h2. Deploy notes\n" +
		"Run {{make build}} (see [docs|https://x/d]) or [https://x/d|https://x/d].\n" +
		"* step *one*\n" +
		"## check {{foo}}\n" +
		"bq. warning: *_read this_*\n" +
		"|| env || status ||\n" +
		"| prod | -down- |\n" +
		"{code:bash}\nmake deploy # **literal**\n{code}"
	if got, converted := markdownToJiraWiki(in); !converted || got != want {
		t.Errorf("markdownToJiraWiki kitchen sink =\n%s\nwant:\n%s", got, want)
	}
}

func TestLinkifyCommentRefs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only comment 12 exists on this PR.
		if strings.HasSuffix(r.URL.Path, "/pull-requests/7/comments/12") {
			_, _ = w.Write([]byte(`{"id":12,"text":"x"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewBitbucketClient(srv.URL, "t")
	got := c.linkifyCommentRefs("see #12, not #99 or `#12` or code:\n```\n#12\n```", "ENG", "api", 7)
	want := "see [#12](" + srv.URL + "/projects/ENG/repos/api/pull-requests/7/overview?commentId=12), not #99"
	if !strings.HasPrefix(got, want) {
		t.Errorf("linkify = %q, want prefix %q", got, want)
	}
	if strings.Count(got, "[#12]") != 1 {
		t.Errorf("code spans should be left alone: %q", got)
	}
	// A number that names something else is not a comment reference.
	if other := c.linkifyCommentRefs("same as PR #12 and issue #12", "ENG", "api", 7); strings.Contains(other, "[#12]") {
		t.Errorf("PR/issue references should not be linkified: %q", other)
	}
}

func TestResolveReviewers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") == "nobody" {
			_, _ = w.Write([]byte(`{"values":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"values":[{"user":{"name":"abs","displayName":"Alexander Bugge Stage"}}]}`))
	}))
	defer srv.Close()
	c := NewBitbucketClient(srv.URL, "t")
	got, err := c.resolveReviewers("ENG", "api", []string{"ABS"})
	if err != nil || len(got) != 1 || got[0] != "abs" {
		t.Errorf("resolveReviewers = %v, %v; want [abs]", got, err)
	}
	if _, err := c.resolveReviewers("ENG", "api", []string{"Alexander Bugge Stage"}); err == nil {
		t.Error("a display name should be rejected with candidates")
	}
	if _, err := c.resolveReviewers("ENG", "api", []string{"nobody"}); err == nil {
		t.Error("an unknown user should be rejected")
	}
}

func TestPageText(t *testing.T) {
	text := strings.Repeat("x", 100)
	if got := pageText(text, 0, 200, "charOffset", "maxChars"); got != text {
		t.Error("short text should come back whole")
	}
	got := pageText(text, 0, 40, "charOffset", "maxChars")
	if !strings.HasPrefix(got, strings.Repeat("x", 40)) {
		t.Error("wrong window")
	}
	// A truncated response must name the argument that continues it.
	if !strings.Contains(got, "charOffset=40") || !strings.Contains(got, "maxChars") {
		t.Errorf("no continuation hint: %q", got)
	}
	if tail := pageText(text, 40, 60, "charOffset", "maxChars"); tail != strings.Repeat("x", 60) {
		t.Errorf("tail = %q", tail)
	}
}

func TestCheckProjectNames(t *testing.T) {
	if err := checkProjectNames("Component(s)", "PAY", []string{"api"}, []string{"API", "Web"}); err != nil {
		t.Errorf("case-insensitive match should pass: %v", err)
	}
	err := checkProjectNames("Component(s)", "PAY", []string{"Mobile"}, []string{"API", "Web"})
	if err == nil || !strings.Contains(err.Error(), "API, Web") {
		t.Errorf("unknown name should list the real ones, got %v", err)
	}
	// Nothing known about the project means Jira decides, not us.
	if err := checkProjectNames("Version(s)", "PAY", []string{"9.9"}, nil); err != nil {
		t.Errorf("empty available list should not block: %v", err)
	}
}

func TestSessionRemembersReviewedHashes(t *testing.T) {
	s := &sessionState{}
	key := reviewKey("ENG", "api", 7)
	if _, _, ok := s.lastReviewed(key); ok {
		t.Error("nothing should be remembered yet")
	}
	s.rememberReviewed(key, "aaa", "bbb")
	from, to, ok := s.lastReviewed(key)
	if !ok || from != "aaa" || to != "bbb" {
		t.Errorf("lastReviewed = %q %q %v", from, to, ok)
	}
	// Scoped per repo: the same PR number elsewhere is a different review.
	if _, _, ok := s.lastReviewed(reviewKey("ENG", "web", 7)); ok {
		t.Error("review state leaked across repos")
	}
	var nilSession *sessionState
	nilSession.rememberReviewed(key, "a", "b")
	if _, _, ok := nilSession.lastReviewed(key); ok {
		t.Error("nil session should be inert")
	}
}

func TestToolAnnotations(t *testing.T) {
	jira, bitbucket = &JiraClient{}, &BitbucketClient{}
	t.Cleanup(func() { jira, bitbucket = nil, nil })
	readOnly := map[string]bool{}
	for _, raw := range toolList() {
		var tl struct {
			Name        string `json:"name"`
			Annotations *struct {
				Title           string `json:"title"`
				ReadOnlyHint    bool   `json:"readOnlyHint"`
				DestructiveHint bool   `json:"destructiveHint"`
			} `json:"annotations"`
		}
		if err := json.Unmarshal(raw, &tl); err != nil {
			t.Fatal(err)
		}
		if tl.Annotations == nil || tl.Annotations.Title == "" {
			t.Fatalf("%s has no annotations", tl.Name)
		}
		readOnly[tl.Name] = tl.Annotations.ReadOnlyHint
		if tl.Annotations.ReadOnlyHint && tl.Annotations.DestructiveHint {
			t.Errorf("%s is both read-only and destructive", tl.Name)
		}
	}
	for name, want := range map[string]bool{"jira_get": true, "bitbucket_get_pr": true, "jira_mutate": false, "complete_work": false} {
		if readOnly[name] != want {
			t.Errorf("%s readOnlyHint = %v, want %v", name, readOnly[name], want)
		}
	}
}

func TestUserLookupFallsBackWhenScopeDenied(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "/permissions/users") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"values":[{"name":"mm","displayName":"Magnus Melin"}]}`))
	}))
	defer srv.Close()
	c := NewBitbucketClient(srv.URL, "t")
	users, err := c.searchUsersRaw("ENG", "api", "magnus", 10)
	if err != nil || len(users) != 1 || users[0].Name != "mm" {
		t.Fatalf("searchUsersRaw = %v, %v; want [mm] with no error", users, err)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[1], "/users") {
		t.Errorf("expected a fallback to the global directory, got %v", paths)
	}
	// A denial on an unscoped lookup has nothing to fall back to.
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer deny.Close()
	if _, err := NewBitbucketClient(deny.URL, "t").searchUsersRaw("", "", "magnus", 10); err == nil {
		t.Error("an unscoped 401 should still be an error")
	}
}

// TestAttachToText covers the Bitbucket upload path end to end: files reach the
// repo attachments endpoint, a path already written into the text is swapped in
// place, and a file nobody mentioned still ends up referenced.
func TestAttachToText(t *testing.T) {
	var uploads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/attachments") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("X-Atlassian-Token") != "no-check" {
			t.Errorf("missing XSRF header")
		}
		f, hdr, err := r.FormFile("files")
		if err != nil {
			t.Errorf("no files part: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = f.Close()
		uploads = append(uploads, hdr.Filename)
		_, _ = fmt.Fprintf(w, `{"attachments":[{"id":%d,"url":"attachment:%d","name":%q}]}`, len(uploads), len(uploads), hdr.Filename)
	}))
	defer srv.Close()

	dir := t.TempDir()
	shot := filepath.Join(dir, "shot.png")
	log := filepath.Join(dir, "run.log")
	for _, p := range []string{shot, log} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c := NewBitbucketClient(srv.URL, "t")
	text, uploaded, err := c.attachToText("ENG", "api", "Before:\n\n![the widget]("+shot+")", []string{shot, log})
	if err != nil {
		t.Fatalf("attachToText: %v", err)
	}
	if len(uploaded) != 2 || uploaded[0].ID.String() != "1" {
		t.Fatalf("uploaded = %+v", uploaded)
	}
	want := "Before:\n\n![the widget](attachment:1)\n\n[run.log](attachment:2)"
	if text != want {
		t.Errorf("text =\n%q\nwant\n%q", text, want)
	}
	// A bare path (no markdown link around it) becomes the full markup.
	bare, _, err := c.attachToText("ENG", "api", "see "+shot, []string{shot})
	if err != nil {
		t.Fatalf("attachToText: %v", err)
	}
	if bare != "see ![shot.png](attachment:3)" {
		t.Errorf("bare path = %q", bare)
	}
}

func TestWikiMarkupPassesThroughUntouched(t *testing.T) {
	// Everything a wiki-savvy caller might write must survive byte for byte,
	// including "##" nested ordered lists, which look like markdown headings.
	wiki := "* bullet\n** nested\n*** deeper\n# one\n## two\n_italic_\n-strike-\n" +
		"[link|http://x]\n{quote}\nq *b*\n{quote}\nbq. quoted\n----\n|| a || b ||\n| 1 | 2 |\n" +
		"{noformat}\n**raw**\n{noformat}\nh3. Head\n{{mono}}"
	if out, converted := markdownToJiraWiki(wiki); converted || out != wiki {
		t.Errorf("wiki markup was rewritten:\n%s", out)
	}
	// A markdown heading is still converted once the list block has ended.
	md := "# one\n## two\n\n## Real Heading"
	want := "# one\n## two\n\nh2. Real Heading"
	if out, _ := markdownToJiraWiki(md); out != want {
		t.Errorf("heading after list = %q, want %q", out, want)
	}
}

func TestConsecutiveMarkdownHeadings(t *testing.T) {
	in := "## A\n## B\n\ntext\n# item\n## nested"
	want := "h2. A\nh2. B\n\ntext\n# item\n## nested"
	if out, _ := markdownToJiraWiki(in); out != want {
		t.Errorf("consecutive headings = %q, want %q", out, want)
	}
}

func TestAppendAIMarkerWiki(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "Crashes on save", "Crashes on save\n\n" + aiMarkerWiki},
		{"already marked", "x\n\n" + aiMarkerWiki, "x\n\n" + aiMarkerWiki},
		{"trailing whitespace trimmed", "x  \n", "x\n\n" + aiMarkerWiki},
		{"after closed code block", "{code}\nselect 1\n{code}", "{code}\nselect 1\n{code}\n\n" + aiMarkerWiki},
		{"empty stays empty", "  ", "  "},
	}
	for _, c := range cases {
		if got := appendAIMarkerWiki(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// An unclosed {code} runs to the end of the field: a marker appended after
	// it would render inside the block, so the text is left unmarked.
	if got := appendAIMarkerWiki("{code:sql\nselect 1"); got != "{code:sql\nselect 1" {
		t.Errorf("unclosed code block: got %q", got)
	}
	// The real write path converts markdown first, so an unterminated markdown
	// fence is closed into {code} and the marker lands after it, never inside.
	wiki, _ := markdownToJiraWiki("look:\n```go\nfmt.Println(1)")
	if got := appendAIMarkerWiki(wiki); !strings.HasSuffix(got, "{code}\n\n"+aiMarkerWiki) {
		t.Errorf("converted fence: got %q", got)
	}
	markAITextEnabled = false
	defer func() { markAITextEnabled = true }()
	if got := appendAIMarkerWiki("x"); got != "x" {
		t.Errorf("disabled: got %q", got)
	}
}

func TestAppendAIMarkerMarkdown(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "LGTM", "LGTM\n\n" + aiMarkerMarkdown},
		{"already marked", "LGTM\n\n" + aiMarkerMarkdown, "LGTM\n\n" + aiMarkerMarkdown},
		{"after closed fence", "```go\nx\n```", "```go\nx\n```\n\n" + aiMarkerMarkdown},
		{"inline code span intact", "use `foo` here", "use `foo` here\n\n" + aiMarkerMarkdown},
		{"empty stays empty", " ", " "},
	}
	for _, c := range cases {
		if got := appendAIMarkerMarkdown(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// An unterminated fence would swallow the marker, so it is skipped.
	open := "```python\nprint(1)"
	if got := appendAIMarkerMarkdown(open); got != open {
		t.Errorf("open fence: got %q", got)
	}
	// A trailing suggestion block must stay last; the marker goes before it.
	sugg := "drop the loop\n\n```suggestion\nx := 1\n```"
	want := "drop the loop\n\n" + aiMarkerMarkdown + "\n\n```suggestion\nx := 1\n```"
	if got := appendAIMarkerMarkdown(sugg); got != want {
		t.Errorf("suggestion: got %q, want %q", got, want)
	}
	if got := stripAIMarker(want); got != sugg {
		t.Errorf("stripAIMarker: got %q, want %q", got, sugg)
	}
}

// ── attachment sources ───────────────────────────────────────────────────────

func TestResolveAttachmentSourceLocalAndDataURI(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(local, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := resolveAttachmentSource(local)
	if err != nil {
		t.Fatalf("local: %v", err)
	}
	defer releaseAttachments([]resolvedAttachment{r})
	if r.name != "notes.txt" || r.tmp {
		t.Errorf("local resolved = %+v", r)
	}

	// A data: URI carries its own bytes, so it works with no filesystem at all —
	// the transport a sandboxed host is left with.
	d, err := resolveAttachmentSource("data:text/plain;name=run.log;base64," + base64.StdEncoding.EncodeToString([]byte("boom")))
	if err != nil {
		t.Fatalf("data URI: %v", err)
	}
	defer releaseAttachments([]resolvedAttachment{d})
	if d.name != "run.log" || !d.tmp {
		t.Fatalf("data resolved = %+v", d)
	}
	got, err := os.ReadFile(d.path)
	if err != nil || string(got) != "boom" {
		t.Errorf("data bytes = %q, %v", got, err)
	}

	// An unnamed data: URI gets an extension from its media type.
	png, err := resolveAttachmentSource("data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG")))
	if err != nil {
		t.Fatalf("png data URI: %v", err)
	}
	defer releaseAttachments([]resolvedAttachment{png})
	if png.name != "attachment.png" {
		t.Errorf("png name = %q", png.name)
	}

	// An unreadable path must name the alternatives that need no filesystem.
	_, err = resolveAttachmentSource(filepath.Join(dir, "missing.png"))
	if err == nil || !strings.Contains(err.Error(), "data:") ||
		!strings.Contains(err.Error(), "confined") {
		t.Errorf("missing path error = %v", err)
	}
}

func TestResolveAttachmentSourceURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/named":
			w.Header().Set("Content-Disposition", `attachment; filename="diagram.png"`)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("bytes"))
		case "/1234": // Bitbucket-style: a bare id, type only in the header.
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("bytes"))
		case "/secret":
			if r.Header.Get("Authorization") != "Bearer jira-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("ok"))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("nope"))
		}
	}))
	defer srv.Close()

	r, err := resolveAttachmentSource(srv.URL + "/named")
	if err != nil {
		t.Fatalf("named: %v", err)
	}
	defer releaseAttachments([]resolvedAttachment{r})
	if r.name != "diagram.png" || !r.tmp || r.ref != srv.URL+"/named" {
		t.Errorf("named resolved = %+v", r)
	}

	bare, err := resolveAttachmentSource(srv.URL + "/1234")
	if err != nil {
		t.Fatalf("bare: %v", err)
	}
	defer releaseAttachments([]resolvedAttachment{bare})
	if bare.name != "1234.jpg" {
		t.Errorf("bare name = %q, want an extension from Content-Type", bare.name)
	}

	// A URL on the configured Jira host is fetched with the token, so an
	// attachment already in Jira can be re-attached elsewhere.
	prev := jira
	jira = NewJiraClient(srv.URL, "jira-token")
	defer func() { jira = prev }()
	auth, err := resolveAttachmentSource(srv.URL + "/secret")
	if err != nil {
		t.Fatalf("authenticated: %v", err)
	}
	defer releaseAttachments([]resolvedAttachment{auth})

	// A host we are not configured for gets no token, and the failure says so.
	if _, err := resolveAttachmentSource(srv.URL + "/gone"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing URL error = %v", err)
	}
}

func TestAttachToTextSplicesURLSources(t *testing.T) {
	var uploads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shot.png" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("bytes"))
			return
		}
		_, hdr, err := r.FormFile("files")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		uploads = append(uploads, hdr.Filename)
		_, _ = fmt.Fprintf(w, `{"attachments":[{"id":%d,"url":"attachment:%d","name":%q}]}`, len(uploads), len(uploads), hdr.Filename)
	}))
	defer srv.Close()

	c := NewBitbucketClient(srv.URL, "t")
	url := srv.URL + "/shot.png"
	text, uploaded, err := c.attachToText("ENG", "api", "Before:\n\n![the widget]("+url+")", []string{url})
	if err != nil {
		t.Fatalf("attachToText: %v", err)
	}
	if len(uploaded) != 1 || uploaded[0].Name != "shot.png" {
		t.Fatalf("uploaded = %+v", uploaded)
	}
	if want := "Before:\n\n![the widget](attachment:1)"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
}

// ── upload widget (MCP Apps) ─────────────────────────────────────────────────

func TestUploadWidgetIsSelfContained(t *testing.T) {
	html := uploadWidget()
	// The host serves this under `default-src 'none'`: an external script or
	// stylesheet would silently never load.
	for _, bad := range []string{"<script src=", "<link rel=\"stylesheet\"", "https://cdn", "/*@ext-apps@*/"} {
		if strings.Contains(html, bad) {
			t.Errorf("widget must not reference %q — nothing external can load under the host CSP", bad)
		}
	}
	// The bridge has to speak the protocol the host expects.
	for _, want := range []string{"ui/initialize", "ui/notifications/initialized", "ui/notifications/tool-input", "tools/call", "2026-01-26"} {
		if !strings.Contains(html, want) {
			t.Errorf("widget is missing %q", want)
		}
	}
	if !strings.Contains(html, `"attach_files"`) {
		t.Error("widget does not call back into attach_files")
	}
	// Dropping the vendored SDK was the point; keep it from creeping back.
	if len(html) > 64*1024 {
		t.Errorf("widget is %d bytes — it is meant to stay small and self-contained", len(html))
	}
}

func TestAttachFilesTargetValidation(t *testing.T) {
	prevJira, prevBB := jira, bitbucket
	jira, bitbucket = &JiraClient{}, &BitbucketClient{}
	defer func() { jira, bitbucket = prevJira, prevBB }()

	if _, err := attachFiles(nil, map[string]any{}); err == nil ||
		!strings.Contains(err.Error(), "issueKey") {
		t.Errorf("no target should be rejected, got %v", err)
	}
	if _, err := attachFiles(nil, map[string]any{"issueKey": "KON-1", "prId": float64(2)}); err == nil ||
		!strings.Contains(err.Error(), "not both") {
		t.Errorf("two targets should be rejected, got %v", err)
	}

	// A target and no files, on a host that renders widgets, is the model
	// opening the panel: it must not upload, and must warn the model off
	// filling in `files` itself.
	appsHost := &sessionState{caps: clientCaps{apps: true}}
	res, err := attachFiles(appsHost, map[string]any{"issueKey": "KON-1"})
	if err != nil {
		t.Fatalf("opening the panel: %v", err)
	}
	if text := res.Content[0].Text; !strings.Contains(text, "KON-1") ||
		!strings.Contains(text, "Upload panel open") ||
		!strings.Contains(text, "do not fill in") {
		t.Errorf("panel-open result = %q", text)
	}

	// Same call on a host without MCP Apps (Claude Code today) must degrade to
	// an answer the model can act on, not a panel that never appears.
	res, err = attachFiles(&sessionState{}, map[string]any{"issueKey": "KON-1"})
	if err != nil {
		t.Fatalf("degraded path: %v", err)
	}
	if text := res.Content[0].Text; !strings.Contains(text, "does not support MCP Apps") ||
		!strings.Contains(text, "attachments") {
		t.Errorf("degraded result = %q", text)
	}
}

func TestAppsCapabilityNegotiation(t *testing.T) {
	mime := []any{"text/html;profile=mcp-app"}
	cases := []struct {
		name string
		ext  map[string]any
		want bool
	}{
		{"absent", nil, false},
		{"other extensions only", map[string]any{"vendor/other": map[string]any{}}, false},
		{"named but no mime types", map[string]any{uiExtensionKey: map[string]any{}}, false},
		{"unsupported mime type", map[string]any{uiExtensionKey: map[string]any{"mimeTypes": []any{"text/plain"}}}, false},
		{"negotiated", map[string]any{uiExtensionKey: map[string]any{"mimeTypes": mime}}, true},
	}
	for _, c := range cases {
		if got := appsCapability(c.ext); got != c.want {
			t.Errorf("%s: appsCapability = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAttachFilesRequiresConfiguredService(t *testing.T) {
	prevJira, prevBB := jira, bitbucket
	jira, bitbucket = nil, &BitbucketClient{}
	defer func() { jira, bitbucket = prevJira, prevBB }()

	if _, err := attachFiles(nil, map[string]any{"issueKey": "KON-1"}); err == nil ||
		!strings.Contains(err.Error(), "Jira is not configured") {
		t.Errorf("Jira target without Jira should be rejected, got %v", err)
	}
}

// TestWidgetDataURIFormat pins the exact string the upload panel emits. The
// panel builds it in JavaScript and this parses it in Go, so nothing but a test
// holds the two halves together — a change to either side breaks here first.
func TestWidgetDataURIFormat(t *testing.T) {
	// Captured from the widget driven against a stub host: a 4-byte PNG picked
	// as "shot.png".
	const fromWidget = "data:image/png;name=shot.png;base64,AQIDBA=="

	r, err := resolveAttachmentSource(fromWidget)
	if err != nil {
		t.Fatalf("widget data URI: %v", err)
	}
	defer releaseAttachments([]resolvedAttachment{r})
	if r.name != "shot.png" {
		t.Errorf("name = %q, want shot.png", r.name)
	}
	got, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{1, 2, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Errorf("bytes = %v, want %v", got, want)
	}
	// The panel names a pasted screenshot rather than sending an empty one.
	p, err := resolveAttachmentSource("data:image/png;name=pasted-image;base64,CQk=")
	if err != nil {
		t.Fatalf("pasted image: %v", err)
	}
	defer releaseAttachments([]resolvedAttachment{p})
	if p.name != "pasted-image" {
		t.Errorf("pasted name = %q", p.name)
	}
}

// ── config: unfilled .mcpb placeholders ──────────────────────────────────────

// TestUnsubstitutedManifestEnvIsBlank covers the Claude Desktop install path:
// a user_config field left blank arrives as the literal "${user_config.x}", not
// as an empty string. Taken at face value it configured Jira with a bogus URL
// and every call died with net/http's "unsupported protocol scheme".
func TestUnsubstitutedManifestEnvIsBlank(t *testing.T) {
	t.Setenv("JIRA_URL", "${user_config.jira_url}")
	t.Setenv("JIRA_ACCESS_TOKEN", "${user_config.jira_token}")
	t.Setenv("BITBUCKET_URL", "https://bb.example.com")
	t.Setenv("BITBUCKET_ACCESS_TOKEN", "real-token")
	t.Setenv("ATLASSIAN_MCP_REPO_ROOT", "${user_config.repo_root}")

	clearUnsubstitutedEnv()

	if v := os.Getenv("JIRA_URL"); v != "" {
		t.Errorf("JIRA_URL = %q, want cleared", v)
	}
	if v := os.Getenv("ATLASSIAN_MCP_REPO_ROOT"); v != "" {
		t.Errorf("ATLASSIAN_MCP_REPO_ROOT = %q, want cleared", v)
	}
	// A filled-in value must survive untouched.
	if v := os.Getenv("BITBUCKET_URL"); v != "https://bb.example.com" {
		t.Errorf("BITBUCKET_URL = %q, want it left alone", v)
	}
	if v := os.Getenv("BITBUCKET_ACCESS_TOKEN"); v != "real-token" {
		t.Errorf("BITBUCKET_ACCESS_TOKEN = %q, want it left alone", v)
	}
}

func TestPlaceholderMatching(t *testing.T) {
	for _, s := range []string{"${user_config.jira_url}", "${user_config.repo_root}", "${__dirname}"} {
		if !mcpbPlaceholderRe.MatchString(s) {
			t.Errorf("%q should be recognized as an unfilled placeholder", s)
		}
	}
	// Real values that merely resemble one must not be blanked.
	for _, s := range []string{
		"https://jira.example.com", "", "token-${weird", "$notaplaceholder",
		"https://jira.example.com/${x}", "${a b}",
	} {
		if mcpbPlaceholderRe.MatchString(s) {
			t.Errorf("%q must not be treated as a placeholder", s)
		}
	}
}

func TestInvalidServiceURLRejected(t *testing.T) {
	cases := map[string]bool{
		"https://jira.example.com": true,
		"http://localhost:8080":    true,
		"":                         true, // not configured, not an error
		"jira.example.com":         false,
		"${user_config.jira_url}":  false,
		"ftp://jira.example.com":   false,
		"https://":                 false,
	}
	for raw, ok := range cases {
		if got := invalidServiceURL(raw) == ""; got != ok {
			t.Errorf("invalidServiceURL(%q) usable = %v, want %v (%s)", raw, got, ok, invalidServiceURL(raw))
		}
	}
}

// TestBlankExtensionFieldsFallBackToConfigFile is the whole Claude Desktop
// story end to end: install the extension, leave the URL/token fields blank,
// and the server must pick up ~/.atlassian-mcp.json — which is what the install
// dialog promises. Before placeholders were cleared, the literal
// "${user_config.jira_url}" counted as "set" and shadowed the file.
func TestBlankExtensionFieldsFallBackToConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "atlassian-mcp.json")
	cfg := `{
	  "jira":      { "url": "https://jira.example.com", "token": "jira-secret" },
	  "bitbucket": { "url": "https://bb.example.com",   "token": "bb-secret"   }
	}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// Exactly what Claude Desktop passes for an all-blank install dialog.
	t.Setenv("ATLASSIAN_MCP_CONFIG", cfgPath)
	t.Setenv("JIRA_URL", "${user_config.jira_url}")
	t.Setenv("JIRA_ACCESS_TOKEN", "${user_config.jira_token}")
	t.Setenv("BITBUCKET_URL", "${user_config.bitbucket_url}")
	t.Setenv("BITBUCKET_ACCESS_TOKEN", "${user_config.bitbucket_token}")

	got := loadConfig()
	if got.Jira == nil {
		t.Fatal("Jira not configured — the config file was not picked up")
	}
	if got.Jira.URL != "https://jira.example.com" || got.Jira.Token != "jira-secret" {
		t.Errorf("Jira = %+v, want the config file's values", *got.Jira)
	}
	if got.Bitbucket == nil {
		t.Fatal("Bitbucket not configured")
	}
	if got.Bitbucket.URL != "https://bb.example.com" || got.Bitbucket.Token != "bb-secret" {
		t.Errorf("Bitbucket = %+v, want the config file's values", *got.Bitbucket)
	}
}

// A filled-in dialog field still wins over the config file for that field,
// while the others keep falling back.
func TestFilledExtensionFieldOverridesConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "atlassian-mcp.json")
	if err := os.WriteFile(cfgPath, []byte(`{"jira":{"url":"https://from-file.example.com","token":"file-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATLASSIAN_MCP_CONFIG", cfgPath)
	t.Setenv("JIRA_URL", "${user_config.jira_url}")
	t.Setenv("JIRA_ACCESS_TOKEN", "${user_config.jira_token}")
	t.Setenv("BITBUCKET_URL", "https://typed-in.example.com")
	t.Setenv("BITBUCKET_ACCESS_TOKEN", "typed-token")

	got := loadConfig()
	if got.Jira == nil || got.Jira.URL != "https://from-file.example.com" {
		t.Errorf("Jira should come from the file, got %+v", got.Jira)
	}
	if got.Bitbucket == nil || got.Bitbucket.URL != "https://typed-in.example.com" {
		t.Errorf("Bitbucket should come from the dialog, got %+v", got.Bitbucket)
	}
}
