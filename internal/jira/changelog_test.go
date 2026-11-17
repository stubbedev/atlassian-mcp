package jira

import (
	"net/http"
	"strings"
	"testing"
)

const changelogJSON = `{"total":3,"histories":[
	{"author":{"displayName":"Jane Doe"},"created":"2026-10-30T09:00:00.000+0100","items":[{"field":"Story Points","fieldtype":"custom","from":null,"fromString":null,"to":"5","toString":"5"}]},
	{"author":{"displayName":"Jane Doe"},"created":"2026-11-03T10:12:00.000+0100","items":[{"field":"Story Points","fieldtype":"custom","from":"5","fromString":"5","to":"13","toString":"13"},{"field":"status","fieldtype":"jira","fromString":"Open","toString":"In Progress"}]},
	{"created":"2026-11-04T08:00:00.000+0000","items":[{"field":"assignee","fieldtype":"jira","fromString":"Jane Doe","toString":""}]}]}`

func TestJiraGetChangelogFilteredByField(t *testing.T) {
	c, _ := jiraStub(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/field"):
			_, _ = w.Write([]byte(storyPointsFieldList))
		case strings.HasSuffix(r.URL.Path, "/issue/KON-1") && r.URL.Query().Get("expand") == "changelog":
			_, _ = w.Write([]byte(`{"key":"KON-1","changelog":` + changelogJSON + `}`))
		case strings.HasSuffix(r.URL.Path, "/issue/KON-1"):
			_, _ = w.Write([]byte(`{"key":"KON-1","fields":{"summary":"S","status":{"name":"Open"},"issuetype":{"name":"Story"}}}`))
		default:
			return false
		}
		return true
	})
	opts, err := c.IssueOverviewOptsFromArgs(map[string]any{
		"issueKey": "KON-1", "includeComments": false, "includeTransitions": false, "includeSprint": false,
		"includeChangelog": true, "changelogFields": []any{"story points"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.IssueOverview(opts)
	if err != nil {
		t.Fatal(err)
	}
	want := "Changelog (Story Points): 2 change(s)\n" +
		"  2026-10-30 08:00 UTC Jane Doe: Story Points None → 5\n" +
		"  2026-11-03 09:12 UTC Jane Doe: Story Points 5 → 13"
	if text := res.Content[0].Text; !strings.Contains(text, want) || strings.Contains(text, "In Progress") {
		t.Errorf("jira_get changelog:\n%s\nwant to contain:\n%s", text, want)
	}

	opts, err = c.IssueOverviewOptsFromArgs(map[string]any{"issueKey": "KON-1", "includeChangelog": true, "changedSince": "2026-11-04"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.changelog == nil || len(opts.changelog.fields) != 0 || formatChangeTime(opts.changelog.since) != "2026-11-04 00:00 UTC" {
		t.Errorf("changedSince alone should keep every field from midnight UTC: %+v", opts.changelog)
	}
	if _, err := c.IssueOverviewOptsFromArgs(map[string]any{"issueKey": "KON-1", "includeChangelog": true, "changedSince": "last week"}); err == nil || !strings.Contains(err.Error(), "changedSince") {
		t.Errorf("an unparseable changedSince must be rejected, got %v", err)
	}
}

func TestFieldHistoryForSprintCountsFromSprintStart(t *testing.T) {
	c, calls := jiraStub(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/field"):
			_, _ = w.Write([]byte(storyPointsFieldList))
		case strings.HasSuffix(r.URL.Path, "/sprint/7"):
			_, _ = w.Write([]byte(`{"id":7,"name":"Sprint 12","state":"active","startDate":"2026-11-01T09:00:00.000+01:00"}`))
		case strings.HasSuffix(r.URL.Path, "/search"):
			if q := r.URL.Query(); q.Get("jql") != "sprint = 7 ORDER BY key ASC" || q.Get("expand") != "changelog" {
				t.Errorf("search query = %v", q)
			}
			_, _ = w.Write([]byte(`{"total":4,"issues":[
				{"key":"KON-1","fields":{"summary":"A","status":{"name":"Open"}},"changelog":` + changelogJSON + `},
				{"key":"KON-2","fields":{"summary":"B","status":{"name":"Done"}},"changelog":{"total":0,"histories":[]}},
				{"key":"KON-3","fields":{"summary":"C","status":{"name":"Open"}},"changelog":{"total":120,"histories":[]}}]}`))
		default:
			return false
		}
		return true
	})
	res, err := c.Search(map[string]any{"resource": "field_history", "sprintId": 7.0, "changelogFields": []any{"Story Points"}, "maxResults": 3.0}, "")
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].Text
	for _, want := range []string{
		"Field history for sprint [7] Sprint 12 (active) — Story Points, changed since 2026-11-01 08:00 UTC:",
		"[KON-1] A | Open | Unassigned | ",
		"  2026-11-03 09:12 UTC Jane Doe: Story Points 5 → 13",
		"[KON-3] C | Open | Unassigned | ",
		"  Jira returned only 0 of 120 changelog entries; older changes are missing.",
		"2 of 3 issue(s) checked had matching changes (showing 1–3 of 4, use startAt=3 for next page).",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("field_history missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"None → 5", "KON-2"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("field_history should not show %q:\n%s", unwanted, text)
		}
	}
	searches := 0
	for _, call := range *calls {
		if strings.Contains(call, "/search?") {
			searches++
		}
	}
	if searches != 1 {
		t.Errorf("one search should cover the sprint, got %d", searches)
	}
}

func TestFieldHistoryBoardScope(t *testing.T) {
	sprints := `{"isLast":true,"values":[{"id":7,"name":"Sprint 12","state":"active"}]}`
	c, _ := jiraStub(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/board/3/sprint"):
			if got := r.URL.Query().Get("state"); got != "active" {
				t.Errorf("sprint state = %q, want the active default", got)
			}
			_, _ = w.Write([]byte(sprints))
		case strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"total":1,"issues":[{"key":"KON-1","fields":{"summary":"A","status":{"name":"Open"}},"changelog":{"total":0,"histories":[]}}]}`))
		default:
			return false
		}
		return true
	})
	res, err := c.Search(map[string]any{"resource": "field_history", "boardId": 3.0}, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Field history for sprint [7] Sprint 12 (active) — all fields:\nNo matching changes in 1 issue(s) checked."; res.Content[0].Text != want {
		t.Errorf("field_history = %q, want %q", res.Content[0].Text, want)
	}

	sprints = `{"isLast":false,"values":[{"id":1,"name":"S1"},{"id":2,"name":"S2"},{"id":3,"name":"S3"},{"id":4,"name":"S4"}]}`
	if _, err := c.Search(map[string]any{"resource": "field_history", "boardId": 3.0}, ""); err == nil || !strings.Contains(err.Error(), "pass sprintId") {
		t.Errorf("too many sprints should ask for sprintId, got %v", err)
	}
}
