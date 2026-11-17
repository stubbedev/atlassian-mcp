package main

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type jiraChangelog struct {
	Total     int           `json:"total"`
	Histories []jiraHistory `json:"histories"`
}

type jiraIssueWithChangelog struct {
	jiraIssue

	Changelog *jiraChangelog `json:"changelog"`
}

type jiraHistory struct {
	Author  *jiraUser         `json:"author"`
	Created string            `json:"created"`
	Items   []jiraHistoryItem `json:"items"`
}

type jiraHistoryItem struct {
	Field      string  `json:"field"`
	FieldID    string  `json:"fieldId"`
	From       *string `json:"from"`
	FromString *string `json:"fromString"`
	To         *string `json:"to"`
	ToString   *string `json:"toString"`
}

type fieldChange struct {
	at     time.Time
	author string
	field  string
	from   string
	to     string
}

type changeFilter struct {
	fields []jiraField
	since  time.Time
}

func (f changeFilter) matchesField(item jiraHistoryItem) bool {
	if len(f.fields) == 0 {
		return true
	}
	for _, field := range f.fields {
		if item.FieldID != "" && item.FieldID == field.ID {
			return true
		}
		if strings.EqualFold(item.Field, field.Name) || strings.EqualFold(item.Field, field.ID) {
			return true
		}
	}
	return false
}

func (f changeFilter) describe() string {
	var parts []string
	if len(f.fields) > 0 {
		names := make([]string, len(f.fields))
		for i, field := range f.fields {
			names[i] = field.Name
		}
		parts = append(parts, strings.Join(names, ", "))
	} else {
		parts = append(parts, "all fields")
	}
	if !f.since.IsZero() {
		parts = append(parts, "changed since "+formatChangeTime(f.since))
	}
	return strings.Join(parts, ", ")
}

func (cl *jiraChangelog) changes(f changeFilter) ([]fieldChange, error) {
	if cl == nil {
		return nil, nil
	}
	var out []fieldChange
	for _, h := range cl.Histories {
		at, err := parseJiraTime(h.Created)
		if err != nil {
			return nil, fmt.Errorf("reading changelog entry time: %w", err)
		}
		if !f.since.IsZero() && at.Before(f.since) {
			continue
		}
		author := "(unknown)"
		if h.Author != nil {
			author = h.Author.DisplayName
		}
		for _, item := range h.Items {
			if !f.matchesField(item) {
				continue
			}
			out = append(out, fieldChange{
				at:     at,
				author: author,
				field:  item.Field,
				from:   historyValue(item.FromString, item.From),
				to:     historyValue(item.ToString, item.To),
			})
		}
	}
	return out, nil
}

func (cl *jiraChangelog) truncation() string {
	if cl == nil || cl.Total <= len(cl.Histories) {
		return ""
	}
	return fmt.Sprintf("Jira returned only %d of %d changelog entries; older changes are missing.", len(cl.Histories), cl.Total)
}

func historyValue(display, raw *string) string {
	for _, v := range []*string{display, raw} {
		if v != nil && strings.TrimSpace(*v) != "" {
			return *v
		}
	}
	return "None"
}

func (c fieldChange) String() string {
	return fmt.Sprintf("%s %s: %s %s → %s", formatChangeTime(c.at), c.author, c.field, c.from, c.to)
}

var jiraTimeLayouts = []string{
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05.000Z07:00",
	time.RFC3339Nano,
	"2006-01-02T15:04",
	time.DateOnly,
}

func parseJiraTime(s string) (time.Time, error) {
	for _, layout := range jiraTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q; use YYYY-MM-DD or an ISO 8601 timestamp", s)
}

func formatChangeTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func (c *JiraClient) changeFilterFromArgs(args map[string]any) (changeFilter, error) {
	fields, err := c.resolveFields(argStrSlice(args, "changelogFields"))
	if err != nil {
		return changeFilter{}, err
	}
	f := changeFilter{fields: fields}
	if s := argString(args, "changedSince"); s != "" {
		if f.since, err = parseJiraTime(s); err != nil {
			return changeFilter{}, fmt.Errorf("changedSince: %w", err)
		}
	}
	return f, nil
}

const issueChangelogCap = 50

func (c *JiraClient) issueChangelogLines(issueKey string, f changeFilter) ([]string, error) {
	issue, err := jiraGet[struct {
		Changelog *jiraChangelog `json:"changelog"`
	}](c, "/rest/api/2", "GET", "/issue/"+url.PathEscape(issueKey)+"?fields=summary&expand=changelog", nil)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, fmt.Errorf("issue %s returned no changelog", issueKey)
	}
	changes, err := issue.Changelog.changes(f)
	if err != nil {
		return nil, err
	}
	lines := []string{"", fmt.Sprintf("Changelog (%s): %d change(s)", f.describe(), len(changes))}
	if note := issue.Changelog.truncation(); note != "" {
		lines = append(lines, note)
	}
	if len(changes) == 0 {
		return append(lines, "(none)"), nil
	}
	shown := changes
	if len(changes) > issueChangelogCap {
		shown = changes[len(changes)-issueChangelogCap:]
		lines = append(lines, fmt.Sprintf("Showing the newest %d; %d older change(s) hidden — narrow with changelogFields or changedSince.", issueChangelogCap, len(changes)-issueChangelogCap))
	}
	for _, ch := range shown {
		lines = append(lines, "  "+ch.String())
	}
	return lines, nil
}

type historyScope struct {
	label string
	jql   string
	since time.Time
}

const maxHistorySprints = 3

func (c *JiraClient) historyScopes(args map[string]any, repoRoot string) ([]historyScope, error) {
	if jql := argString(args, "jql"); jql != "" {
		return []historyScope{{label: "JQL " + jql, jql: jql}}, nil
	}
	if id := argInt(args, "sprintId"); id > 0 {
		sprint, err := jiraGet[jiraSprint](c, "/rest/agile/1.0", "GET", fmt.Sprintf("/sprint/%d", id), nil)
		if err != nil {
			return nil, err
		}
		if sprint == nil {
			return nil, fmt.Errorf("sprint %d not found", id)
		}
		scope, err := sprintScope(*sprint)
		if err != nil {
			return nil, err
		}
		return []historyScope{scope}, nil
	}
	boardID := argInt(args, "boardId")
	if boardID <= 0 {
		id, err := c.resolveBoardID(argString(args, "projectKey"), repoRoot)
		if err != nil {
			return nil, fmt.Errorf("field_history needs jql, sprintId, or a board: %w", err)
		}
		boardID = id
	}
	state := argString(args, "sprintState")
	if state == "" {
		state = "active"
	}
	q := url.Values{}
	q.Set("state", state)
	q.Set("maxResults", strconv.Itoa(maxHistorySprints+1))
	sprints, err := jiraGet[jiraPageSprints](c, "/rest/agile/1.0", "GET", fmt.Sprintf("/board/%d/sprint?%s", boardID, q.Encode()), nil)
	if err != nil {
		return nil, err
	}
	if sprints == nil || len(sprints.Values) == 0 {
		return nil, fmt.Errorf("board %d has no %s sprint; pass sprintId or jql instead", boardID, state)
	}
	if len(sprints.Values) > maxHistorySprints || !sprints.IsLast {
		names := make([]string, len(sprints.Values))
		for i, s := range sprints.Values {
			names[i] = fmt.Sprintf("[%d] %s", s.ID, s.Name)
		}
		return nil, fmt.Errorf("board %d has more than %d %s sprints (%s, …); pass sprintId to pick one", boardID, maxHistorySprints, state, strings.Join(names, ", "))
	}
	scopes := make([]historyScope, 0, len(sprints.Values))
	for _, s := range sprints.Values {
		scope, err := sprintScope(s)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, nil
}

func sprintScope(s jiraSprint) (historyScope, error) {
	scope := historyScope{label: fmt.Sprintf("sprint [%d] %s (%s)", s.ID, s.Name, s.State), jql: fmt.Sprintf("sprint = %d ORDER BY key ASC", s.ID)}
	if s.StartDate == "" {
		return scope, nil
	}
	start, err := parseJiraTime(s.StartDate)
	if err != nil {
		return historyScope{}, fmt.Errorf("reading start of sprint %d: %w", s.ID, err)
	}
	scope.since = start
	return scope, nil
}

func (c *JiraClient) fieldHistory(args map[string]any, repoRoot string) (toolResult, error) {
	filter, err := c.changeFilterFromArgs(args)
	if err != nil {
		return toolResult{}, err
	}
	scopes, err := c.historyScopes(args, repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	maxResults := argIntDefault(args, "maxResults", 50)
	startAt := argInt(args, "startAt")
	var sections []string
	for _, scope := range scopes {
		f := filter
		if f.since.IsZero() {
			f.since = scope.since
		}
		section, err := c.scopeHistory(scope, f, maxResults, startAt)
		if err != nil {
			return toolResult{}, fmt.Errorf("reading field history for %s: %w", scope.label, err)
		}
		sections = append(sections, section)
	}
	return textResult(strings.Join(sections, "\n\n")), nil
}

func (c *JiraClient) scopeHistory(scope historyScope, f changeFilter, maxResults, startAt int) (string, error) {
	q := url.Values{}
	q.Set("jql", scope.jql)
	q.Set("fields", issueListFields)
	q.Set("expand", "changelog")
	q.Set("maxResults", strconv.Itoa(maxResults))
	q.Set("startAt", strconv.Itoa(startAt))
	data, err := jiraGet[struct {
		Total  int                      `json:"total"`
		Issues []jiraIssueWithChangelog `json:"issues"`
	}](c, "/rest/api/2", "GET", "/search?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	if data == nil {
		return "", errors.New("Jira returned no search result")
	}
	lines := []string{fmt.Sprintf("Field history for %s — %s:", scope.label, f.describe())}
	changed := 0
	for _, issue := range data.Issues {
		changes, err := issue.Changelog.changes(f)
		if err != nil {
			return "", fmt.Errorf("%s: %w", issue.Key, err)
		}
		note := issue.Changelog.truncation()
		if len(changes) == 0 && note == "" {
			continue
		}
		changed++
		lines = append(lines, c.issueListLine(issue.jiraIssue, nil))
		if note != "" {
			lines = append(lines, "  "+note)
		}
		for _, ch := range changes {
			lines = append(lines, "  "+ch.String())
		}
	}
	summary := fmt.Sprintf("%d of %d issue(s) checked had matching changes", changed, len(data.Issues))
	if changed == 0 {
		summary = fmt.Sprintf("No matching changes in %d issue(s) checked", len(data.Issues))
	}
	lines = append(lines, summary+pagination(data.Total, startAt, len(data.Issues))+".")
	return strings.Join(lines, "\n"), nil
}
