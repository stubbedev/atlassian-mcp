package jira

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stubbedev/atlassian-mcp/internal/markup"
)

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

func TestParseJiraErrorDetails(t *testing.T) {
	in := `{"errorMessages":["top"],"errors":{"summary":"required"}}`
	if got := parseJiraErrorDetails(in); got != "top | summary: required" {
		t.Errorf("got %q", got)
	}
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
func fieldCacheClient() *Client {
	c := NewJiraClient("https://jira.example", "t", nil)
	epicName := jiraField{ID: "customfield_10005", Name: "Epic Name", Custom: true}
	epicName.Schema.Custom = "com.pyxis.greenhopper.jira:gh-epic-label"
	points := jiraField{ID: "customfield_10002", Name: "Story Points", Custom: true}
	c.fields = []jiraField{epicName, points, {ID: "summary", Name: "Summary"}}
	c.fieldsCached = true
	return c
}

func TestResolveField(t *testing.T) {
	c := fieldCacheClient()
	for arg, want := range map[string]string{
		"Epic Name":         "customfield_10005",
		"story points":      "customfield_10002",
		"customfield_10002": "customfield_10002",
		"summary":           "summary",
	} {
		got, err := c.resolveField(arg)
		if err != nil || got.ID != want {
			t.Errorf("resolveField(%q) = %q, %v; want %q", arg, got.ID, err, want)
		}
	}
	if _, err := c.resolveField("Nope"); err == nil {
		t.Error("unknown field should error")
	}
	if id, _ := c.getEpicNameFieldID(); id != "customfield_10005" {
		t.Errorf("getEpicNameFieldID = %q", id)
	}
}

func TestResolveFieldsDropsRepeats(t *testing.T) {
	got, err := fieldCacheClient().resolveFields([]string{"Story Points", "customfield_10002", "Epic Name"})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, f := range got {
		ids = append(ids, f.ID)
	}
	if want := []string{"customfield_10002", "customfield_10005"}; !slices.Equal(ids, want) {
		t.Errorf("resolveFields ids = %v, want %v", ids, want)
	}
	if fieldIDs(got) != ",customfield_10002,customfield_10005" {
		t.Errorf("fieldIDs = %q", fieldIDs(got))
	}
}

func TestFormatFieldValue(t *testing.T) {
	for raw, want := range map[string]string{
		``:                                     "None",
		`null`:                                 "None",
		`5.0`:                                  "5",
		`2.5`:                                  "2.5",
		`"KON-7"`:                              "KON-7",
		`""`:                                   "None",
		`true`:                                 "true",
		`[]`:                                   "None",
		`{"self":"x","value":"High","id":"1"}`: "High",
		`{"value":"Hardware","child":{"value":"Disk"}}`: "Hardware > Disk",
		`{"name":"jdoe","displayName":"Jane Doe"}`:      "Jane Doe",
		`[{"name":"1.0"},{"name":"1.1"}]`:               "1.0, 1.1",
		`["a","b"]`:                                     "a, b",
		`{"key":"KON-3","id":"10003"}`:                  "KON-3",
		`{"id":"42"}`:                                   `{"id":"42"}`,
		`["com.atlassian.greenhopper.service.sprint.Sprint@1f2[id=7,rapidViewId=3,state=ACTIVE,name=Sprint 12,startDate=2026-11-01T09:00:00.000Z]"]`: "Sprint 12",
	} {
		if got := formatFieldValue(json.RawMessage(raw)); got != want {
			t.Errorf("formatFieldValue(%s) = %q, want %q", raw, got, want)
		}
	}
}

const storyPointsFieldList = `[{"id":"customfield_10002","name":"Story Points","custom":true,"schema":{"type":"number","custom":"com.atlassian.jira.plugin.system.customfieldtypes:float"}},{"id":"customfield_10008","name":"Epic Link","custom":true,"schema":{"type":"any","custom":"com.pyxis.greenhopper.jira:gh-epic-link"}},{"id":"summary","name":"Summary"}]`

func requireFields(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if got := r.URL.Query().Get("fields"); !strings.Contains(","+got+",", ","+want+",") {
		t.Errorf("%s fetched fields=%q, missing %s", r.URL.Path, got, want)
	}
}

func TestJiraGetShowsRequestedCustomFields(t *testing.T) {
	c, _ := jiraStub(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/field"):
			_, _ = w.Write([]byte(storyPointsFieldList))
			return true
		case strings.HasSuffix(r.URL.Path, "/issue/KON-1"):
			requireFields(t, r, "customfield_10002")
			_, _ = w.Write([]byte(`{"key":"KON-1","fields":{"summary":"S","status":{"name":"Open"},"issuetype":{"name":"Story"},"customfield_10002":3.0,"customfield_10008":"KON-9"}}`))
			return true
		}
		return false
	})
	res, err := c.IssueOverview(IssueOverviewOpts{IssueKey: "KON-1", customFields: []string{"story points"}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].Text
	for _, want := range []string{"Story Points: 3\n", "Epic Link:  KON-9"} {
		if !strings.Contains(text, want) {
			t.Errorf("jira_get output missing %q:\n%s", want, text)
		}
	}
	if _, err := c.IssueOverview(IssueOverviewOpts{IssueKey: "KON-1", customFields: []string{"Velocity"}}); err == nil || !strings.Contains(err.Error(), "Story Points") {
		t.Errorf("an unknown field should fail naming the fields that exist, got %v", err)
	}
}

func TestSearchIssuesShowsCustomFields(t *testing.T) {
	c, _ := jiraStub(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/field"):
			_, _ = w.Write([]byte(storyPointsFieldList))
			return true
		case strings.HasSuffix(r.URL.Path, "/search"):
			requireFields(t, r, "customfield_10002")
			_, _ = w.Write([]byte(`{"total":2,"startAt":0,"issues":[
				{"key":"KON-1","fields":{"summary":"A","status":{"name":"Done"},"customfield_10002":5.0}},
				{"key":"KON-2","fields":{"summary":"B","status":{"name":"Open"},"customfield_10002":null}}]}`))
			return true
		}
		return false
	})
	res, err := c.Search(map[string]any{"jql": "sprint = 7", "customFields": []any{"Story Points"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].Text
	for _, want := range []string{"[KON-1] A | Done | Unassigned | Story Points: 5 | ", "[KON-2] B | Open | Unassigned | Story Points: None | "} {
		if !strings.Contains(text, want) {
			t.Errorf("search output missing %q:\n%s", want, text)
		}
	}
}

func boardStub(t *testing.T, estimation string, sprintIssues func(r *http.Request)) *Client {
	t.Helper()
	c, _ := jiraStub(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/field"):
			_, _ = w.Write([]byte(storyPointsFieldList))
		case strings.HasSuffix(r.URL.Path, "/board/3/configuration"):
			_, _ = w.Write([]byte(`{"id":3,"estimation":` + estimation + `}`))
		case strings.HasSuffix(r.URL.Path, "/board/3"):
			_, _ = w.Write([]byte(`{"id":3,"name":"KON board","type":"scrum"}`))
		case strings.HasSuffix(r.URL.Path, "/board/3/sprint"):
			_, _ = w.Write([]byte(`{"isLast":true,"values":[{"id":7,"name":"Sprint 12","state":"active"}]}`))
		case strings.HasSuffix(r.URL.Path, "/sprint/7/issue"):
			sprintIssues(r)
			_, _ = w.Write([]byte(`{"total":1,"issues":[{"key":"KON-1","fields":{"summary":"A","status":{"name":"Open"},"customfield_10002":8.0,"customfield_10008":"KON-9"}}]}`))
		default:
			return false
		}
		return true
	})
	return c
}

func TestBoardOverviewShowsBoardEstimationField(t *testing.T) {
	c := boardStub(t, `{"type":"field","field":{"fieldId":"customfield_10002","displayName":"Story Points"}}`, func(r *http.Request) {
		requireFields(t, r, "customfield_10002")
		requireFields(t, r, "customfield_10008")
	})
	res, err := c.boardOverview(map[string]any{"boardId": 3.0, "customFields": []any{"Epic Link", "Story Points"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "[KON-1] A | Open | Unassigned | Story Points: 8 | Epic Link: KON-9 | "; !strings.Contains(res.Content[0].Text, want) {
		t.Errorf("board overview missing %q:\n%s", want, res.Content[0].Text)
	}
}

func TestBoardOverviewSkipsTimeTrackingEstimation(t *testing.T) {
	c := boardStub(t, `{"type":"field","field":{"fieldId":"timeoriginalestimate","displayName":"Original Time Estimate"}}`, func(r *http.Request) {
		if got := r.URL.Query().Get("fields"); got != issueListFields {
			t.Errorf("sprint issues fetched fields=%q, want only %q", got, issueListFields)
		}
	})
	res, err := c.boardOverview(map[string]any{"boardId": 3.0})
	if err != nil {
		t.Fatal(err)
	}
	if want := "[KON-1] A | Open | Unassigned | "; !strings.Contains(res.Content[0].Text, want) || strings.Contains(res.Content[0].Text, "Estimate") {
		t.Errorf("time-tracking estimation should add nothing to the listing:\n%s", res.Content[0].Text)
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

func TestJiraCommentAuthorship(t *testing.T) {
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Body string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		posted = append(posted, body.Body)
		_, _ = w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()
	c := NewJiraClient(srv.URL, "t", nil)

	for _, userAuthored := range []bool{false, true} {
		if _, err := c.comment(map[string]any{"issueKey": "ENG-1", "body": "Ship it", "userAuthored": userAuthored}); err != nil {
			t.Fatalf("userAuthored=%v: %v", userAuthored, err)
		}
	}
	if want := []string{"Ship it\n\n" + markup.AIMarkerWiki, "Ship it"}; !slices.Equal(posted, want) {
		t.Errorf("posted = %q, want %q", posted, want)
	}

	for _, args := range []map[string]any{
		{"issueKey": "ENG-1", "transitionName": "Done", "commentAction": "delete", "commentId": "1", "commentUserAuthored": true},
		{"issueKey": "ENG-1", "transitionName": "Done", "commentUserAuthored": true},
	} {
		if _, err := c.MutateIssue(args, ""); err == nil || !strings.Contains(err.Error(), "commentUserAuthored") {
			t.Errorf("%v: err = %v, want a commentUserAuthored rejection", args, err)
		}
	}
	if len(posted) != 2 {
		t.Errorf("rejected calls wrote to Jira: %q", posted)
	}
}

// jiraStub serves a Jira whose link types are the real ones from a Server
// instance ("Relates", not "Relates to") and records every request path.
func jiraStub(t *testing.T, handler func(w http.ResponseWriter, r *http.Request) bool) (*Client, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if handler != nil && handler(w, r) {
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/issueLinkType"):
			_, _ = w.Write([]byte(`{"issueLinkTypes":[
				{"name":"Blocks","inward":"is blocked by","outward":"blocks"},
				{"name":"Relates","inward":"relates to","outward":"relates to"}]}`))
		case strings.HasSuffix(r.URL.Path, "/configuration"):
			_, _ = w.Write([]byte(`{"issueLinkingEnabled":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorMessages":["Issue Does Not Exist"],"errors":{}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return NewJiraClient(srv.URL, "t", nil), &calls
}

func TestResolveLinkType(t *testing.T) {
	c, _ := jiraStub(t, nil)
	for _, tc := range []struct {
		in       string
		want     string
		reversed bool
	}{
		{"Relates", "Relates", false},
		{"relates to", "Relates", false}, // the phrase Jira shows, not the type name
		{"BLOCKS", "Blocks", false},
		{"blocks", "Blocks", false},
		{"is blocked by", "Blocks", true}, // inward phrase reverses the link
	} {
		got, reversed, err := c.resolveLinkType(tc.in)
		if err != nil || got != tc.want || reversed != tc.reversed {
			t.Errorf("resolveLinkType(%q) = %q, %v, %v; want %q, %v", tc.in, got, reversed, err, tc.want, tc.reversed)
		}
	}
	// An unknown name must be rejected here: Jira answers POST /issueLink with a
	// bare 404 that reads as a missing issue.
	_, _, err := c.resolveLinkType("Depends on")
	if err == nil || !strings.Contains(err.Error(), "Blocks") || !strings.Contains(err.Error(), "Relates") {
		t.Errorf("unknown link type should list the real ones, got %v", err)
	}
	// A lookup that cannot run leaves the decision to Jira.
	blind := NewJiraClient("https://jira.example", "t", nil)
	blind.linkTypesCached = true
	if got, _, err := blind.resolveLinkType("Whatever"); err != nil || got != "Whatever" {
		t.Errorf("unknown link types should pass through, got %q, %v", got, err)
	}
}

// TestCreatedIssueSurvivesLaterFailure pins the bug that produced a duplicate
// ticket: create succeeded, the link step failed, and the error mentioned
// neither the key nor the URL, so the issue looked like it had never been made.
func TestCreatedIssueSurvivesLaterFailure(t *testing.T) {
	c, calls := jiraStub(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issue"):
			_, _ = w.Write([]byte(`{"key":"KON-13542"}`))
			return true
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issueLink"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorMessages":["Issue Does Not Exist"],"errors":{}}`))
			return true
		}
		return false
	})
	_, err := c.MutateIssue(map[string]any{
		"create": map[string]any{"projectKey": "KON", "issueType": "Bug", "summary": "x"},
		"link":   map[string]any{"linkType": "Relates", "targetIssueKey": "KON-13486"},
	}, "")
	if err == nil {
		t.Fatal("a failed link step must still be an error")
	}
	for _, want := range []string{"KON-13542", "created issue", "/browse/KON-13542", "instead of creating it again"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("error should carry %q so the caller does not re-create the issue: %s", want, err)
		}
	}
	_ = calls
}

// A failure before anything was written stays a plain error — there is no key
// to carry and no risk of a duplicate.
func TestFailureBeforeAnyWriteStaysPlain(t *testing.T) {
	c, _ := jiraStub(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issueLink") {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		return false
	})
	_, err := c.MutateIssue(map[string]any{
		"issueKey": "KON-1",
		"link":     map[string]any{"linkType": "Relates", "targetIssueKey": "KON-2"},
	}, "")
	if err == nil || strings.Contains(err.Error(), "retry only the failed step") {
		t.Errorf("nothing was written, so the error should be the bare Jira one: %v", err)
	}
}

func TestLinkDirectionFollowsThePhrase(t *testing.T) {
	var posted map[string]any
	c, _ := jiraStub(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issueLink") {
			_ = json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(http.StatusCreated)
			return true
		}
		return false
	})
	if _, err := c.MutateIssue(map[string]any{
		"issueKey": "KON-1",
		"link":     map[string]any{"linkType": "is blocked by", "targetIssueKey": "KON-2"},
	}, ""); err != nil {
		t.Fatal(err)
	}
	name, _ := posted["type"].(map[string]any)["name"].(string)
	outward, _ := posted["outwardIssue"].(map[string]any)["key"].(string)
	inward, _ := posted["inwardIssue"].(map[string]any)["key"].(string)
	if name != "Blocks" || outward != "KON-2" || inward != "KON-1" {
		t.Errorf(`"KON-1 is blocked by KON-2" should post Blocks KON-2 → KON-1, got %s %s → %s`, name, outward, inward)
	}
}

func TestDeleteIssue(t *testing.T) {
	c, calls := jiraStub(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issue/KON-9"):
			_, _ = w.Write([]byte(`{"key":"KON-9","fields":{"summary":"Created in error","issuetype":{"name":"Soft Bug"},"status":{"name":"Open"}}}`))
			return true
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		return false
	})
	res, err := c.MutateIssue(map[string]any{"issueKey": "KON-9", "delete": true}, "")
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "Deleted KON-9") || !strings.Contains(text, "Created in error") {
		t.Errorf("the result should name what was destroyed, got %q", text)
	}
	if !strings.Contains(strings.Join(*calls, " "), "DELETE /rest/api/2/issue/KON-9") {
		t.Errorf("expected a DELETE for the issue, got %v", *calls)
	}
	// deleteSubtasks reaches Jira as the query parameter it needs.
	if _, err := c.MutateIssue(map[string]any{"issueKey": "KON-9", "delete": true, "deleteSubtasks": true}, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*calls, " "), "/issue/KON-9?deleteSubtasks=true") {
		t.Errorf("deleteSubtasks should be sent to Jira, got %v", *calls)
	}
}

func TestDeleteRefusesToShareACall(t *testing.T) {
	c, _ := jiraStub(t, nil)
	for _, extra := range []map[string]any{
		{"create": map[string]any{"issueType": "Bug", "summary": "x"}},
		{"comment": "bye"},
		{"transitionName": "Close Ticket"},
	} {
		args := map[string]any{"issueKey": "KON-9", "delete": true}
		maps.Copy(args, extra)
		if _, err := c.MutateIssue(args, ""); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Errorf("delete alongside %v should be refused, got %v", extra, err)
		}
	}
}

// Jira refuses to delete a parent unless the sub-tasks go too; the caller should
// be told the argument that fixes it rather than the raw Jira prose.
func TestDeleteParentSuggestsDeleteSubtasks(t *testing.T) {
	c, _ := jiraStub(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issue/KON-9"):
			_, _ = w.Write([]byte(`{"key":"KON-9","fields":{"summary":"Parent","issuetype":{"name":"Story"},"status":{"name":"Open"}}}`))
			return true
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errorMessages":["The issue 'KON-9' has subtasks and subtasks must be deleted before the issue can be deleted."],"errors":{}}`))
			return true
		}
		return false
	})
	_, err := c.MutateIssue(map[string]any{"issueKey": "KON-9", "delete": true}, "")
	if err == nil || !strings.Contains(err.Error(), "deleteSubtasks: true") {
		t.Errorf("expected the sub-task hint, got %v", err)
	}
}
