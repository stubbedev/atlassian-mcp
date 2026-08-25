package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	if got := len(toolList()); got != 7 {
		t.Errorf("git+context+jira should be 7 tools, got %d", got)
	}
	bitbucket = &BitbucketClient{}
	if got := len(toolList()); got != 14 {
		t.Errorf("all should be 14 tools, got %d", got)
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

func TestLinkifyCommentRefs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only comment 12 exists on this PR.
		if strings.HasSuffix(r.URL.Path, "/pull-requests/7/comments/12") {
			w.Write([]byte(`{"id":12,"text":"x"}`))
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
			w.Write([]byte(`{"values":[]}`))
			return
		}
		w.Write([]byte(`{"values":[{"user":{"name":"abs","displayName":"Alexander Bugge Stage"}}]}`))
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
