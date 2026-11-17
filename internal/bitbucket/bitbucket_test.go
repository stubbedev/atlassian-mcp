package bitbucket

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stubbedev/atlassian-mcp/internal/attachment"
	"github.com/stubbedev/atlassian-mcp/internal/markup"
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
		got := ParseRemote(c.in)
		if c.nil {
			if got != nil {
				t.Errorf("%s: expected nil, got %+v", c.in, got)
			}
			continue
		}
		if got == nil || got.ProjectKey != c.pk || got.RepoSlug != c.rs {
			t.Errorf("%s: got %+v, want %s/%s", c.in, got, c.pk, c.rs)
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

func TestRemoteMatchesBitbucketInstance(t *testing.T) {
	if !remoteMatchesBitbucketInstance("git@bb.example.com:P/r.git", "https://bb.example.com") {
		t.Error("should match")
	}
	if remoteMatchesBitbucketInstance("git@github.com:o/r.git", "https://bb.example.com") {
		t.Error("should not match")
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
	c := NewBitbucketClient(srv.URL, "t", nil)
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
	c := NewBitbucketClient(srv.URL, "t", nil)
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

func TestSessionRemembersReviewedHashes(t *testing.T) {
	s := &PRReviews{}
	key := reviewKey("ENG", "api", 7)
	if _, _, ok := s.last(key); ok {
		t.Error("nothing should be remembered yet")
	}
	s.remember(key, "aaa", "bbb")
	from, to, ok := s.last(key)
	if !ok || from != "aaa" || to != "bbb" {
		t.Errorf("last = %q %q %v", from, to, ok)
	}
	// Scoped per repo: the same PR number elsewhere is a different review.
	if _, _, ok := s.last(reviewKey("ENG", "web", 7)); ok {
		t.Error("review state leaked across repos")
	}
	var none *PRReviews
	none.remember(key, "a", "b")
	if _, _, ok := none.last(key); ok {
		t.Error("a session without review state should be inert")
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
	c := NewBitbucketClient(srv.URL, "t", nil)
	users, err := c.SearchUsersRaw("ENG", "api", "magnus", 10)
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
	if _, err := NewBitbucketClient(deny.URL, "t", nil).SearchUsersRaw("", "", "magnus", 10); err == nil {
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

	c := NewBitbucketClient(srv.URL, "t", nil)
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

func TestBitbucketCommentUpdateAuthorship(t *testing.T) {
	const current = "Looks off\n\n**[AI]**"
	var put []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"id":7,"version":3,"text":%q}`, current)
		case http.MethodPost:
			_, _ = w.Write([]byte(`{"attachments":[{"id":1,"url":"attachment:1","name":"log.txt"}]}`))
		case http.MethodPut:
			var body struct{ Text string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			put = append(put, body.Text)
			_, _ = w.Write([]byte(`{"id":7}`))
		}
	}))
	defer srv.Close()
	c := NewBitbucketClient(srv.URL, "t", nil)
	base := map[string]any{"projectKey": "ENG", "repoSlug": "api", "prId": 1, "commentId": 7}
	with := func(extra map[string]any) map[string]any {
		args := maps.Clone(base)
		maps.Copy(args, extra)
		return args
	}

	attach := []any{"data:text/plain;base64,aGk="}
	calls := []map[string]any{
		with(map[string]any{"attachments": attach}),
		with(map[string]any{"attachments": attach, "userAuthored": true}),
		with(map[string]any{"text": "Ship it", "userAuthored": true}),
	}
	for _, args := range calls {
		if _, err := c.UpdatePrComment(args, ""); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	want := []string{
		"Looks off\n\n[log.txt](attachment:1)\n\n" + markup.AIMarkerMarkdown,
		"Looks off\n\n[log.txt](attachment:1)",
		"Ship it",
	}
	if !slices.Equal(put, want) {
		t.Errorf("put = %q, want %q", put, want)
	}

	if _, err := c.UpdatePrComment(with(map[string]any{"threadResolved": true, "userAuthored": true}), ""); err == nil || !strings.Contains(err.Error(), "userAuthored") {
		t.Errorf("text-less update: err = %v, want a userAuthored rejection", err)
	}
	if _, err := c.CommentDispatch(nil, with(map[string]any{"action": "delete", "userAuthored": true}), ""); err == nil || !strings.Contains(err.Error(), "userAuthored") {
		t.Errorf("delete: err = %v, want a userAuthored rejection", err)
	}
}

func TestComposeCommentTextSuggestion(t *testing.T) {
	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"suggestion": "\t\tgetMergedOptions() {\n\t\t\treturn {};\n\t\t}"}, "```suggestion\n\t\tgetMergedOptions() {\n\t\t\treturn {};\n\t\t}\n```"},
		{map[string]any{"suggestion": "\n  \r\n    x := 1\n\n  \n", "text": "  Use x  "}, "Use x\n\n```suggestion\n    x := 1\n```"},
		{map[string]any{"text": "plain"}, "plain"},
	}
	for _, tc := range cases {
		got, err := composeCommentText(tc.args)
		if err != nil || got != tc.want {
			t.Errorf("%q: got %q, %v; want %q", tc.args, got, err, tc.want)
		}
	}
	for _, args := range []map[string]any{
		{"suggestion": " \n\t\n"},
		{"text": "```suggestion\nx\n```\nafter"},
	} {
		if _, err := composeCommentText(args); err == nil {
			t.Errorf("%q: expected an error", args)
		}
	}
}

func suggestionCommentServer(t *testing.T, sent *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ausername", "me")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/activities"):
			_, _ = w.Write([]byte(`{"values":[],"isLastPage":true}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":7,"version":3,"text":"old","author":{"user":{"name":"someone"}}}`))
		default:
			var body struct{ Text string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			*sent = append(*sent, body.Text)
			_, _ = w.Write([]byte(`{"id":7}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBitbucketCommentSuggestionKeepsIndentation(t *testing.T) {
	var sent []string
	c := NewBitbucketClient(suggestionCommentServer(t, &sent).URL, "t", nil)
	args := map[string]any{"projectKey": "ENG", "repoSlug": "api", "prId": 1, "suggestion": "\t\tgetMergedOptions() {\n\t\t\treturn {};\n\t\t}"}
	if _, err := c.addPrComment(nil, args, ""); err != nil {
		t.Fatal(err)
	}
	want := []string{markup.AIMarkerMarkdown + "\n\n```suggestion\n\t\tgetMergedOptions() {\n\t\t\treturn {};\n\t\t}\n```"}
	if !slices.Equal(sent, want) {
		t.Errorf("posted = %q, want %q", sent, want)
	}
}

func TestBitbucketCommentUpdateSuggestion(t *testing.T) {
	var sent []string
	c := NewBitbucketClient(suggestionCommentServer(t, &sent).URL, "t", nil)
	base := map[string]any{"projectKey": "ENG", "repoSlug": "api", "prId": 1, "commentId": 7, "userAuthored": true}
	with := func(extra map[string]any) map[string]any {
		args := maps.Clone(base)
		maps.Copy(args, extra)
		return args
	}
	for _, args := range []map[string]any{
		with(map[string]any{"text": "Simpler", "suggestion": "\tif x {\n\t\treturn\n\t}"}),
		with(map[string]any{"suggestion": "\ty := 2"}),
	} {
		if _, err := c.UpdatePrComment(args, ""); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	want := []string{
		"Simpler\n\n```suggestion\n\tif x {\n\t\treturn\n\t}\n```",
		"```suggestion\n\ty := 2\n```",
	}
	if !slices.Equal(sent, want) {
		t.Errorf("put = %q, want %q", sent, want)
	}

	for _, args := range []map[string]any{
		with(map[string]any{"text": "x", "suggestion": "  \n"}),
		with(map[string]any{"text": "```suggestion\nx\n```\nafter"}),
	} {
		if _, err := c.UpdatePrComment(args, ""); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
	if len(sent) != len(want) {
		t.Errorf("rejected updates wrote to Bitbucket: %q", sent[len(want):])
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

	c := NewBitbucketClient(srv.URL, "t", nil)
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

// Bitbucket 8.x answers the upload with links.attachment.href, the only spelling
// its markup renderer resolves — attachment:<id> built from the bare id renders
// to an empty src, so the href has to win over both fallbacks.
func TestParseUploadedAttachmentPrefersAttachmentHref(t *testing.T) {
	raw := []byte(`{"attachments":[{"id":"826","url":"https://git.example.com/projects/P/repos/r/attachments/826","links":{"attachment":{"href":"attachment:1324/826"}}}]}`)
	a, err := parseUploadedAttachment(raw, attachment.Resolved{Name: "clip.mp4"})
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if a.link() != "attachment:1324/826" {
		t.Errorf("link = %q, want attachment:1324/826", a.link())
	}
	// The upload response carries no name, and a video is a link, not an image.
	if got, want := a.markup(), "[clip.mp4](attachment:1324/826)"; got != want {
		t.Errorf("markup = %q, want %q", got, want)
	}
}
