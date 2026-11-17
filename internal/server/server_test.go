package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/atlassian-mcp/internal/bitbucket"
	"github.com/stubbedev/atlassian-mcp/internal/jira"
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

func TestToolListGating(t *testing.T) {
	jiraClient, bitbucketClient = nil, nil
	if got := len(toolList()); got != 1 {
		t.Errorf("git-only should be 1 tool, got %d", got)
	}
	jiraClient = &jira.Client{}
	if got := len(toolList()); got != 8 {
		t.Errorf("git+context+jira should be 8 tools, got %d", got)
	}
	bitbucketClient = &bitbucket.Client{}
	if got := len(toolList()); got != 15 {
		t.Errorf("all should be 15 tools, got %d", got)
	}
	jiraClient, bitbucketClient = nil, nil
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

func TestToolAnnotations(t *testing.T) {
	jiraClient, bitbucketClient = &jira.Client{}, &bitbucket.Client{}
	t.Cleanup(func() { jiraClient, bitbucketClient = nil, nil })
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
	prevJira, prevBB := jiraClient, bitbucketClient
	jiraClient, bitbucketClient = &jira.Client{}, &bitbucket.Client{}
	defer func() { jiraClient, bitbucketClient = prevJira, prevBB }()

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
	prevJira, prevBB := jiraClient, bitbucketClient
	jiraClient, bitbucketClient = nil, &bitbucket.Client{}
	defer func() { jiraClient, bitbucketClient = prevJira, prevBB }()

	if _, err := attachFiles(nil, map[string]any{"issueKey": "KON-1"}); err == nil ||
		!strings.Contains(err.Error(), "Jira is not configured") {
		t.Errorf("Jira target without Jira should be rejected, got %v", err)
	}
}
