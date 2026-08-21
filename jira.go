package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ── Wire types (mirror the TypeScript interfaces) ────────────────────────────

type jiraNamed struct {
	Name string `json:"name"`
}

type jiraField struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Custom bool   `json:"custom"`
	Schema struct {
		Type   string `json:"type"`
		Items  string `json:"items"`
		Custom string `json:"custom"`
	} `json:"schema"`
}

type jiraIssueFields struct {
	Summary     string     `json:"summary"`
	Description string     `json:"description"`
	Status      jiraNamed  `json:"status"`
	IssueType   jiraNamed  `json:"issuetype"`
	Priority    *jiraNamed `json:"priority"`
	Assignee    *struct {
		DisplayName string `json:"displayName"`
	} `json:"assignee"`
	Reporter *struct {
		DisplayName string `json:"displayName"`
	} `json:"reporter"`
	DueDate    string      `json:"duedate"`
	Labels     []string    `json:"labels"`
	Components []jiraNamed `json:"components"`
	Parent     *struct {
		Key    string `json:"key"`
		Fields struct {
			Summary   string    `json:"summary"`
			IssueType jiraNamed `json:"issuetype"`
		} `json:"fields"`
	} `json:"parent"`
	FixVersions []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"fixVersions"`
	Versions     []jiraNamed `json:"versions"`
	Environment  string      `json:"environment"`
	TimeTracking *struct {
		OriginalEstimate  string `json:"originalEstimate"`
		RemainingEstimate string `json:"remainingEstimate"`
		TimeSpent         string `json:"timeSpent"`
	} `json:"timetracking"`
	IssueLinks []jiraIssueLink `json:"issuelinks"`
	Subtasks   []struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string    `json:"summary"`
			Status  jiraNamed `json:"status"`
		} `json:"fields"`
	} `json:"subtasks"`
	Attachment []jiraAttachment `json:"attachment"`
}

type jiraIssue struct {
	Key    string          `json:"key"`
	Fields jiraIssueFields `json:"fields"`
}

type jiraAttachment struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
	Created  string `json:"created"`
	Content  string `json:"content"`
}

type jiraSearchResult struct {
	Total   int         `json:"total"`
	StartAt int         `json:"startAt"`
	Issues  []jiraIssue `json:"issues"`
}

type jiraComment struct {
	ID     string `json:"id"`
	Author struct {
		Name        string `json:"name"`
		Key         string `json:"key"`
		DisplayName string `json:"displayName"`
	} `json:"author"`
	Created string `json:"created"`
	Body    string `json:"body"`
}

type jiraCommentResult struct {
	Total    int           `json:"total"`
	StartAt  int           `json:"startAt"`
	Comments []jiraComment `json:"comments"`
}

type jiraTransition struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	To   jiraNamed `json:"to"`
}

type jiraTransitionsResult struct {
	Transitions []jiraTransition `json:"transitions"`
}

type jiraCreatedIssue struct {
	Key string `json:"key"`
	ID  string `json:"id"`
}

type jiraProject struct {
	Key            string `json:"key"`
	Name           string `json:"name"`
	ProjectTypeKey string `json:"projectTypeKey"`
}

type jiraIssueTypeStatuses struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Statuses []jiraNamed `json:"statuses"`
}

type jiraUser struct {
	Name         string `json:"name"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
	Active       bool   `json:"active"`
}

type jiraCurrentUser struct {
	Name        string `json:"name"`
	Key         string `json:"key"`
	DisplayName string `json:"displayName"`
}

type jiraSprint struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
	Goal      string `json:"goal"`
}

type jiraBoard struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Location *struct {
		ProjectKey  string `json:"projectKey"`
		ProjectName string `json:"projectName"`
	} `json:"location"`
}

type jiraIssueLink struct {
	ID   string `json:"id"`
	Type struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Inward  string `json:"inward"`
		Outward string `json:"outward"`
	} `json:"type"`
	InwardIssue  *jiraLinkedIssue `json:"inwardIssue"`
	OutwardIssue *jiraLinkedIssue `json:"outwardIssue"`
}

type jiraLinkedIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string    `json:"summary"`
		Status  jiraNamed `json:"status"`
	} `json:"fields"`
}

type jiraVersion struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Archived    bool   `json:"archived"`
	Released    bool   `json:"released"`
	ReleaseDate string `json:"releaseDate"`
	StartDate   string `json:"startDate"`
}

type jiraPageSprints struct {
	IsLast bool         `json:"isLast"`
	Values []jiraSprint `json:"values"`
}

type jiraPageBoards struct {
	IsLast bool        `json:"isLast"`
	Values []jiraBoard `json:"values"`
}

// ── Module-level helpers ─────────────────────────────────────────────────────

var (
	jiraKeyInBranchRe = regexp.MustCompile(`\b([A-Z][A-Z0-9]+)-\d+\b`)
	emojiRe           = regexp.MustCompile(`[\x{1F000}-\x{1FAFF}\x{2600}-\x{27BF}\x{2190}-\x{21FF}\x{2B00}-\x{2BFF}\x{FE00}-\x{FE0F}\x{1F1E6}-\x{1F1FF}]`)
)

func capText(value string, max int) string {
	if max <= 0 {
		return value
	}
	r := []rune(value)
	if len(r) <= max {
		return value
	}
	more := len(r) - max
	return fmt.Sprintf("%s\n... (truncated, %d more chars — pass fullDescription=true for the rest)", string(r[:max]), more)
}

func pagination(total, startAt, count int) string {
	end := startAt + count
	if total > end {
		return fmt.Sprintf(" (showing %d–%d of %d, use startAt=%d for next page)", startAt+1, end, total, end)
	}
	return ""
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func buildJQL(query, jql, project, status, assignee, issueType string) (string, error) {
	if jql != "" {
		if len(jql) > 2000 {
			return "", fmt.Errorf("JQL query too long (max 2000 characters).")
		}
		return jql, nil
	}
	var clauses []string
	if query != "" {
		clauses = append(clauses, "text ~ "+jsonStr(query))
	}
	if project != "" {
		clauses = append(clauses, `project = "`+project+`"`)
	}
	if status != "" {
		clauses = append(clauses, `status = "`+status+`"`)
	}
	if assignee != "" {
		clauses = append(clauses, `assignee = "`+assignee+`"`)
	}
	if issueType != "" {
		clauses = append(clauses, `issuetype = "`+issueType+`"`)
	}
	if len(clauses) == 0 {
		return "", fmt.Errorf("Provide at least one of: query, jql, project, status, assignee, issueType")
	}
	return strings.Join(clauses, " AND ") + " ORDER BY updated DESC", nil
}

func parseJiraErrorDetails(errText string) string {
	trimmed := strings.TrimSpace(errText)
	if trimmed == "" {
		return ""
	}
	var payload struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
	}
	if json.Unmarshal([]byte(trimmed), &payload) == nil {
		var parts []string
		for _, msg := range payload.ErrorMessages {
			if c := strings.TrimSpace(msg); c != "" {
				parts = append(parts, c)
			}
		}
		// Sort error field keys for deterministic output.
		for _, field := range sortedKeys(payload.Errors) {
			if c := strings.TrimSpace(payload.Errors[field]); c != "" {
				parts = append(parts, field+": "+c)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " | ")
		}
	}
	if len(trimmed) > 500 {
		return trimmed[:500] + "..."
	}
	return trimmed
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// simple insertion sort to avoid importing sort for tiny maps
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

func formatJiraError(status int, method, path, details string) string {
	prefix := fmt.Sprintf("Jira %d %s %s", status, method, path)
	switch status {
	case 400:
		return strings.TrimSpace(fmt.Sprintf("%s. Invalid request or parameters. %s", prefix, details))
	case 401:
		return prefix + ". Authentication failed. Check JIRA_ACCESS_TOKEN."
	case 403:
		return prefix + ". Permission denied. Check Jira permissions for this token."
	case 404:
		return prefix + ". Resource not found. Verify issue/project identifiers and access."
	case 409:
		return strings.TrimSpace(fmt.Sprintf("%s. Conflict. Refresh and retry. %s", prefix, details))
	}
	if details != "" {
		return prefix + ". " + details
	}
	return prefix
}

func validateCommentBody(body string) (string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", fmt.Errorf("Jira comment body must not be empty.")
	}
	if emojiRe.MatchString(trimmed) {
		return "", fmt.Errorf("Jira comments must not include emoji. Use concise Jira wiki markup or plain text only.")
	}
	return trimmed, nil
}

// ── Client ───────────────────────────────────────────────────────────────────

type JiraClient struct {
	baseURL string
	token   string

	mu                 sync.Mutex // guards the lazily-populated caches below (HTTP is concurrent)
	currentUser        *jiraCurrentUser
	projects           []jiraProject
	projectsCached     bool
	issueLinking       bool
	issueLinkingCached bool
	fields             []jiraField
	fieldsCached       bool
	issueTypeCache     map[string]string
}

func NewJiraClient(baseURL, token string) *JiraClient {
	return &JiraClient{
		baseURL:        strings.TrimRight(baseURL, "/"),
		token:          token,
		issueTypeCache: map[string]string{},
	}
}

func (c *JiraClient) issueURL(key string) string {
	return c.baseURL + "/browse/" + url.PathEscape(key)
}
func (c *JiraClient) projectURL(key string) string {
	return c.baseURL + "/projects/" + url.PathEscape(key)
}
func (c *JiraClient) boardURL(boardID int) string {
	return fmt.Sprintf("%s/secure/RapidBoard.jspa?rapidView=%d", c.baseURL, boardID)
}
func (c *JiraClient) sprintURL(boardID, sprintID int) string {
	return fmt.Sprintf("%s&sprint=%d", c.boardURL(boardID), sprintID)
}

// doRequest performs an HTTP request against apiBase+path, returning the raw
// response body (nil on 204/empty) or a formatted error on non-2xx.
func (c *JiraClient) doRequest(apiBase, method, path string, body any) ([]byte, error) {
	reqURL := c.baseURL + apiBase + path
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, reqURL, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", formatJiraError(res.StatusCode, method, path, parseJiraErrorDetails(string(raw))))
	}
	if res.StatusCode == 204 || len(raw) == 0 {
		return nil, nil
	}
	return raw, nil
}

// jiraGet decodes into *T, returning nil (not error) when the body is empty or
// unparseable — matching the TS request() that resolves to null in those cases.
func jiraGet[T any](c *JiraClient, apiBase, method, path string, body any) (*T, error) {
	raw, err := c.doRequest(apiBase, method, path, body)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var v T
	if json.Unmarshal(raw, &v) != nil {
		return nil, nil
	}
	return &v, nil
}

func (c *JiraClient) api(method, path string, body any) ([]byte, error) {
	return c.doRequest("/rest/api/2", method, path, body)
}
func (c *JiraClient) agile(method, path string, body any) ([]byte, error) {
	return c.doRequest("/rest/agile/1.0", method, path, body)
}

func normalizeIdentity(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func (c *JiraClient) getCurrentUser() (*jiraCurrentUser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentUser != nil {
		return c.currentUser, nil
	}
	me, err := jiraGet[jiraCurrentUser](c, "/rest/api/2", "GET", "/myself", nil)
	if err != nil {
		return nil, err
	}
	if me == nil {
		return nil, fmt.Errorf("Could not determine current Jira user identity.")
	}
	c.currentUser = me
	return me, nil
}

func (c *JiraClient) whoami() (*jiraCurrentUser, error) { return c.getCurrentUser() }

func (c *JiraClient) getIssueLinkingEnabled() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.issueLinkingCached {
		return c.issueLinking, nil
	}
	cfg, err := jiraGet[struct {
		IssueLinkingEnabled bool `json:"issueLinkingEnabled"`
	}](c, "/rest/api/2", "GET", "/configuration", nil)
	if err != nil {
		return false, err
	}
	c.issueLinking = cfg != nil && cfg.IssueLinkingEnabled
	c.issueLinkingCached = true
	return c.issueLinking, nil
}

// fieldList returns /field once per process, cached.
func (c *JiraClient) fieldList() ([]jiraField, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fieldsCached {
		return c.fields, nil
	}
	data, err := jiraGet[[]jiraField](c, "/rest/api/2", "GET", "/field", nil)
	c.fieldsCached = true
	if err != nil {
		return nil, err
	}
	if data != nil {
		c.fields = *data
	}
	return c.fields, nil
}

// fieldIDBySchema finds a custom field by its Jira schema key, e.g.
// "com.pyxis.greenhopper.jira:gh-epic-link". Empty string when absent.
func (c *JiraClient) fieldIDBySchema(schemaKey string) (string, error) {
	fields, err := c.fieldList()
	if err != nil {
		return "", err
	}
	for _, f := range fields {
		if f.Schema.Custom == schemaKey {
			return f.ID, nil
		}
	}
	return "", nil
}

func (c *JiraClient) getEpicLinkFieldID() (string, error) {
	return c.fieldIDBySchema("com.pyxis.greenhopper.jira:gh-epic-link")
}

func (c *JiraClient) getEpicNameFieldID() (string, error) {
	return c.fieldIDBySchema("com.pyxis.greenhopper.jira:gh-epic-label")
}

// resolveFieldID maps a field name ("Epic Name", "Story Points") or a raw id
// ("customfield_10005", "duedate") to the id Jira's field payload wants.
func (c *JiraClient) resolveFieldID(nameOrID string) (string, error) {
	key := strings.TrimSpace(nameOrID)
	if key == "" {
		return "", fmt.Errorf("customFields keys must be a field name or id.")
	}
	fields, err := c.fieldList()
	if err != nil {
		return "", err
	}
	var names []string
	for _, f := range fields {
		if f.ID == key {
			return f.ID, nil
		}
		if strings.EqualFold(f.Name, key) {
			return f.ID, nil
		}
		if f.Custom {
			names = append(names, f.Name)
		}
	}
	return "", fmt.Errorf("Unknown Jira field %q. Custom fields on this instance: %s", key, strings.Join(names, ", "))
}

// applyCustomFields resolves each key to a field id and copies the value in
// untouched, so callers pass Jira's own shape: "text", 5, {"value":"A"},
// [{"value":"A"}], {"name":"user"}.
func (c *JiraClient) applyCustomFields(target, custom map[string]any) error {
	for k, v := range custom {
		id, err := c.resolveFieldID(k)
		if err != nil {
			return err
		}
		target[id] = v
	}
	return nil
}

// listFields renders the field catalog for jira_search resource=fields.
func (c *JiraClient) listFields(query string) (toolResult, error) {
	fields, err := c.fieldList()
	if err != nil {
		return toolResult{}, err
	}
	ql := strings.ToLower(strings.TrimSpace(query))
	var lines []string
	for _, f := range fields {
		if !f.Custom && ql == "" {
			continue // built-ins have dedicated args; only show them on an explicit query
		}
		if ql != "" && !strings.Contains(strings.ToLower(f.Name), ql) && !strings.Contains(strings.ToLower(f.ID), ql) {
			continue
		}
		typ := f.Schema.Type
		if f.Schema.Items != "" {
			typ += " of " + f.Schema.Items
		}
		lines = append(lines, fmt.Sprintf("- %s [%s] %s", f.Name, f.ID, typ))
	}
	if len(lines) == 0 {
		return textResult("No matching fields."), nil
	}
	return textResult(fmt.Sprintf("%d field(s) — pass by name or id in jira_mutate create/update customFields:\n%s", len(lines), strings.Join(lines, "\n"))), nil
}

func (c *JiraClient) getIssueType(issueKey string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.issueTypeCache[issueKey]; ok && cached != "" {
		return cached, nil
	}
	data, err := jiraGet[jiraIssue](c, "/rest/api/2", "GET", "/issue/"+url.PathEscape(issueKey)+"?fields=issuetype", nil)
	if err != nil {
		return "", err
	}
	if data == nil {
		return "", nil
	}
	name := data.Fields.IssueType.Name
	if name != "" {
		c.issueTypeCache[issueKey] = name
	}
	return name, nil
}

func (c *JiraClient) assertOwnComment(comment *jiraComment) error {
	me, err := c.getCurrentUser()
	if err != nil {
		return err
	}
	caName := normalizeIdentity(comment.Author.Name)
	caKey := normalizeIdentity(comment.Author.Key)
	caDisplay := normalizeIdentity(comment.Author.DisplayName)
	meName := normalizeIdentity(me.Name)
	meKey := normalizeIdentity(me.Key)
	meDisplay := normalizeIdentity(me.DisplayName)

	strongComment := caName != "" || caKey != ""
	strongUser := meName != "" || meKey != ""
	matchesNameKey := (caName != "" && (caName == meName || caName == meKey)) ||
		(caKey != "" && (caKey == meName || caKey == meKey))
	matchesDisplay := !strongComment && !strongUser && caDisplay != "" && caDisplay == meDisplay
	if matchesNameKey || matchesDisplay {
		return nil
	}
	return fmt.Errorf("You can only edit your own Jira comments. Comment %s is authored by %s.", comment.ID, comment.Author.DisplayName)
}

func (c *JiraClient) addIssuesToSprintInternal(sprintID int, issueKeys []string) error {
	_, err := c.agile("POST", fmt.Sprintf("/sprint/%d/issue", sprintID), map[string]any{"issues": issueKeys})
	return err
}

func (c *JiraClient) resolveProjectKey(projectKey, repoRoot string) (string, error) {
	if projectKey != "" {
		return projectKey, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.projectsCached {
		data, err := jiraGet[[]jiraProject](c, "/rest/api/2", "GET", "/project?maxResults=100", nil)
		if err != nil {
			return "", err
		}
		if data != nil {
			c.projects = *data
		}
		c.projectsCached = true
	}
	projects := c.projects
	if len(projects) == 0 {
		return "", fmt.Errorf("No Jira projects found for your account.")
	}
	keys := map[string]bool{}
	for _, p := range projects {
		keys[p.Key] = true
	}
	if repoRoot != "" {
		branch := safeGit(repoRoot, "", "rev-parse", "--abbrev-ref", "HEAD")
		if m := jiraKeyInBranchRe.FindStringSubmatch(branch); m != nil {
			if keys[m[1]] {
				return m[1], nil
			}
		}
	}
	if len(projects) == 1 {
		return projects[0].Key, nil
	}
	shown := projects
	extra := ""
	if len(projects) > 20 {
		shown = projects[:20]
		extra = fmt.Sprintf("\n...and %d more.", len(projects)-20)
	}
	var lines []string
	for i, p := range shown {
		lines = append(lines, fmt.Sprintf("%d. %s — %s", i+1, p.Key, p.Name))
	}
	return "", fmt.Errorf("Please provide projectKey (or project) for this Jira action. Choose one of these project codes:\n%s%s", strings.Join(lines, "\n"), extra)
}

func (c *JiraClient) resolveTransitionID(issueKey, transitionID, transitionName string) (string, error) {
	if transitionID != "" {
		return transitionID, nil
	}
	requested := strings.TrimSpace(transitionName)
	if requested == "" {
		return "", fmt.Errorf("Provide transitionId or transitionName")
	}
	data, err := jiraGet[jiraTransitionsResult](c, "/rest/api/2", "GET", "/issue/"+url.PathEscape(issueKey)+"/transitions", nil)
	if err != nil {
		return "", err
	}
	var transitions []jiraTransition
	if data != nil {
		transitions = data.Transitions
	}
	lowered := strings.ToLower(requested)
	for _, t := range transitions {
		if strings.ToLower(t.Name) == lowered {
			return t.ID, nil
		}
	}
	var names []string
	for _, t := range transitions {
		names = append(names, t.Name)
	}
	avail := strings.Join(names, ", ")
	if avail == "" {
		avail = "(none)"
	}
	return "", fmt.Errorf("Transition %q not found for %s. Available: %s", requested, issueKey, avail)
}

// namedList maps names to Jira's [{"name": n}] shape. Returns non-nil empty
// slice so an empty input clears the field (marshals to [], not null).
func namedList(names []string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n})
	}
	return out
}

// buildTimeTracking assembles Jira's timetracking object from estimate args.
// Returns nil when neither estimate is provided.
func buildTimeTracking(m map[string]any) map[string]any {
	oe := argString(m, "originalEstimate")
	re := argString(m, "remainingEstimate")
	if oe == "" && re == "" {
		return nil
	}
	tt := map[string]any{}
	if oe != "" {
		tt["originalEstimate"] = oe
	}
	if re != "" {
		tt["remainingEstimate"] = re
	}
	return tt
}

// projectKeyOf extracts the project key from an issue key (KON-123 → KON).
func projectKeyOf(issueKey string) string {
	if i := strings.LastIndex(issueKey, "-"); i > 0 {
		return issueKey[:i]
	}
	return ""
}

// componentNames returns the project's component names (nil on error/empty).
func (c *JiraClient) componentNames(pk string) []string {
	data, err := jiraGet[[]struct {
		Name string `json:"name"`
	}](c, "/rest/api/2", "GET", "/project/"+url.PathEscape(pk)+"/components", nil)
	if err != nil || data == nil {
		return nil
	}
	var out []string
	for _, x := range *data {
		out = append(out, x.Name)
	}
	return out
}

// enrichFieldError appends valid component names when Jira rejects a component
// value, turning "Component name 'X' is not valid" into an actionable message.
// Versions are intentionally not enriched — projects can have hundreds.
func (c *JiraClient) enrichFieldError(err error, pk string) error {
	if err == nil || pk == "" {
		return err
	}
	msg := c.nameCustomFields(err.Error())
	if strings.Contains(msg, "components:") {
		if names := c.componentNames(pk); len(names) > 0 {
			return fmt.Errorf("%s | Valid components in %s: %s", msg, pk, strings.Join(names, ", "))
		}
	}
	if msg != err.Error() {
		return fmt.Errorf("%s", msg)
	}
	return err
}

var customFieldIDRe = regexp.MustCompile(`customfield_\d+`)

// nameCustomFields rewrites bare custom field ids in a Jira error into
// "customfield_10005 (Epic Name)" so the caller knows what to pass.
func (c *JiraClient) nameCustomFields(msg string) string {
	if !customFieldIDRe.MatchString(msg) {
		return msg
	}
	fields, err := c.fieldList()
	if err != nil {
		return msg
	}
	byID := map[string]string{}
	for _, f := range fields {
		byID[f.ID] = f.Name
	}
	return customFieldIDRe.ReplaceAllStringFunc(msg, func(id string) string {
		if name := byID[id]; name != "" {
			return id + " (" + name + ")"
		}
		return id
	})
}

func (c *JiraClient) createIssueInternal(create map[string]any, repoRoot string) (*jiraCreatedIssue, error) {
	projectKey, err := c.resolveProjectKey(argString(create, "projectKey"), repoRoot)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{
		"project":   map[string]any{"key": projectKey},
		"issuetype": map[string]any{"name": argString(create, "issueType")},
		"summary":   argString(create, "summary"),
	}
	if d := argString(create, "description"); d != "" {
		fields["description"] = d
	}
	if a := argString(create, "assignee"); a != "" {
		fields["assignee"] = map[string]any{"name": a}
	}
	if r := argString(create, "reporter"); r != "" {
		fields["reporter"] = map[string]any{"name": r}
	}
	if p := argString(create, "priority"); p != "" {
		fields["priority"] = map[string]any{"name": p}
	}
	if dd := argString(create, "dueDate"); dd != "" {
		fields["duedate"] = dd
	}
	if labels := argStrSlice(create, "labels"); len(labels) > 0 {
		fields["labels"] = labels
	}
	if comps := argStrSlice(create, "components"); len(comps) > 0 {
		fields["components"] = namedList(comps)
	}
	if fvs := argStrSlice(create, "fixVersions"); len(fvs) > 0 {
		fields["fixVersions"] = namedList(fvs)
	} else if fv := argString(create, "fixVersion"); fv != "" {
		fields["fixVersions"] = namedList([]string{fv})
	}
	if vers := argStrSlice(create, "versions"); len(vers) > 0 {
		fields["versions"] = namedList(vers)
	}
	if env := argString(create, "environment"); env != "" {
		fields["environment"] = env
	}
	if tt := buildTimeTracking(create); tt != nil {
		fields["timetracking"] = tt
	}
	epicTarget := strings.TrimSpace(argString(create, "epicLink"))
	if parent := argString(create, "parent"); parent != "" {
		parentType, err := c.getIssueType(parent)
		if err != nil {
			return nil, err
		}
		if parentType == "Epic" {
			if epicTarget == "" {
				epicTarget = parent
			}
		} else {
			fields["parent"] = map[string]any{"key": parent}
		}
	}
	if epicTarget != "" {
		epicFieldID, err := c.getEpicLinkFieldID()
		if err != nil {
			return nil, err
		}
		if epicFieldID == "" {
			return nil, fmt.Errorf("Epic Link custom field not found on this Jira instance. Set it manually in the Jira UI.")
		}
		fields[epicFieldID] = epicTarget
	}
	if cf := argMap(create, "customFields"); cf != nil {
		if err := c.applyCustomFields(fields, cf); err != nil {
			return nil, err
		}
	}
	// Epic create screens require Epic Name; default it to the summary so an
	// epic can be created without a UI round trip.
	if strings.EqualFold(strings.TrimSpace(argString(create, "issueType")), "epic") {
		if id, err := c.getEpicNameFieldID(); err == nil && id != "" {
			if _, set := fields[id]; !set {
				fields[id] = argString(create, "summary")
			}
		}
	}
	created, err := jiraGet[jiraCreatedIssue](c, "/rest/api/2", "POST", "/issue", map[string]any{"fields": fields})
	return created, c.enrichFieldError(err, projectKey)
}

// updateIssueFieldsInternal applies the update.* fields. has* flags signal an
// explicitly-provided key so empty strings/arrays act as clears like the TS.
func (c *JiraClient) updateIssueFieldsInternal(issueKey string, update map[string]any) (bool, error) {
	fields := map[string]any{}
	if has(update, "issueType") {
		fields["issuetype"] = map[string]any{"name": argString(update, "issueType")}
	}
	if has(update, "summary") {
		fields["summary"] = argString(update, "summary")
	}
	if has(update, "description") {
		fields["description"] = argString(update, "description")
	}
	if has(update, "assignee") {
		if a := argString(update, "assignee"); a != "" {
			fields["assignee"] = map[string]any{"name": a}
		} else {
			fields["assignee"] = nil
		}
	}
	if has(update, "reporter") {
		fields["reporter"] = map[string]any{"name": argString(update, "reporter")}
	}
	if has(update, "priority") {
		fields["priority"] = map[string]any{"name": argString(update, "priority")}
	}
	if has(update, "dueDate") {
		if dd := argString(update, "dueDate"); dd != "" {
			fields["duedate"] = dd
		} else {
			fields["duedate"] = nil
		}
	}
	if has(update, "labels") {
		fields["labels"] = argStrSlice(update, "labels")
	}
	if has(update, "components") {
		fields["components"] = namedList(argStrSlice(update, "components"))
	}
	if has(update, "fixVersions") {
		fields["fixVersions"] = namedList(argStrSlice(update, "fixVersions"))
	} else if has(update, "fixVersion") {
		if fv := argString(update, "fixVersion"); fv != "" {
			fields["fixVersions"] = namedList([]string{fv})
		} else {
			fields["fixVersions"] = []any{}
		}
	}
	if has(update, "versions") {
		fields["versions"] = namedList(argStrSlice(update, "versions"))
	}
	if has(update, "environment") {
		if env := argString(update, "environment"); env != "" {
			fields["environment"] = env
		} else {
			fields["environment"] = nil
		}
	}
	if tt := buildTimeTracking(update); tt != nil {
		fields["timetracking"] = tt
	}
	if has(update, "epicLink") {
		epicFieldID, err := c.getEpicLinkFieldID()
		if err != nil {
			return false, err
		}
		if epicFieldID == "" {
			return false, fmt.Errorf("Epic Link custom field not found on this Jira instance. Set it manually in the Jira UI.")
		}
		if e := argString(update, "epicLink"); e != "" {
			fields[epicFieldID] = e
		} else {
			fields[epicFieldID] = nil
		}
	}
	if cf := argMap(update, "customFields"); cf != nil {
		if err := c.applyCustomFields(fields, cf); err != nil {
			return false, err
		}
	}
	if len(fields) == 0 {
		return false, nil
	}
	if _, err := c.api("PUT", "/issue/"+url.PathEscape(issueKey), map[string]any{"fields": fields}); err != nil {
		return false, c.enrichFieldError(err, projectKeyOf(issueKey))
	}
	return true, nil
}

// ── Public tool methods ──────────────────────────────────────────────────────

// search dispatches the jira_search resource routing from src/index.ts.
func (c *JiraClient) search(args map[string]any, repoRoot string) (toolResult, error) {
	resource := argString(args, "resource")
	if resource == "" {
		resource = "issues"
	}
	switch resource {
	case "projects":
		return c.getProjects(args)
	case "issue_types":
		return c.getIssueTypes(argString(args, "projectKey"), repoRoot)
	case "boards":
		return c.getBoards(argString(args, "projectKey"), argIntDefault(args, "maxResults", 25), argInt(args, "startAt"))
	case "sprints":
		return c.getSprints(argInt(args, "boardId"), argString(args, "sprintState"), argIntDefault(args, "maxResults", 20), argInt(args, "startAt"))
	case "board_overview":
		return c.boardOverview(args)
	case "versions":
		return c.listVersions(argString(args, "projectKey"), argString(args, "query"), argIntPtr(args, "maxResults"), repoRoot)
	case "fields":
		return c.listFields(argString(args, "query"))
	case "components":
		return c.listComponents(argString(args, "projectKey"), argString(args, "query"), repoRoot)
	case "users":
		return c.searchUsers(argString(args, "query"), argIntDefault(args, "maxResults", 10))
	default:
		if argBool(args, "mine") {
			return c.searchIssues("", "assignee = currentUser() ORDER BY updated DESC", "", "", "", "", argIntDefault(args, "maxResults", 20), argInt(args, "startAt"))
		}
		return c.searchIssues(
			argString(args, "query"), argString(args, "jql"), argString(args, "project"),
			argString(args, "status"), argString(args, "assignee"), argString(args, "issueType"),
			argIntDefault(args, "maxResults", 20), argInt(args, "startAt"))
	}
}

func (c *JiraClient) searchIssues(query, jql, project, status, assignee, issueType string, maxResults, startAt int) (toolResult, error) {
	builtJQL, err := buildJQL(query, jql, project, status, assignee, issueType)
	if err != nil {
		return toolResult{}, err
	}
	q := url.Values{}
	q.Set("jql", builtJQL)
	q.Set("maxResults", fmt.Sprint(maxResults))
	q.Set("startAt", fmt.Sprint(startAt))
	q.Set("fields", "summary,status,assignee,priority,issuetype")
	data, err := jiraGet[jiraSearchResult](c, "/rest/api/2", "GET", "/search?"+q.Encode(), nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil {
		return textResult("No results."), nil
	}
	var lines []string
	for idx, i := range data.Issues {
		assignee := "Unassigned"
		if i.Fields.Assignee != nil {
			assignee = i.Fields.Assignee.DisplayName
		}
		lines = append(lines, fmt.Sprintf("%d. [%s] %s | %s | %s | %s", startAt+idx+1, i.Key, i.Fields.Summary, i.Fields.Status.Name, assignee, c.issueURL(i.Key)))
	}
	page := pagination(data.Total, startAt, len(data.Issues))
	return textResult(fmt.Sprintf("Found %d issues%s:\n%s", data.Total, page, strings.Join(lines, "\n"))), nil
}

type jiraFoundIssue struct {
	Key, Summary, Status, Type string
}

func (c *JiraClient) findIssues(query string, maxResults int) ([]jiraFoundIssue, error) {
	builtJQL, err := buildJQL(query, "", "", "", "", "")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("jql", builtJQL)
	q.Set("maxResults", fmt.Sprint(maxResults))
	q.Set("startAt", "0")
	q.Set("fields", "summary,status,issuetype")
	data, err := jiraGet[jiraSearchResult](c, "/rest/api/2", "GET", "/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var out []jiraFoundIssue
	if data != nil {
		for _, i := range data.Issues {
			out = append(out, jiraFoundIssue{i.Key, i.Fields.Summary, i.Fields.Status.Name, i.Fields.IssueType.Name})
		}
	}
	return out, nil
}

func (c *JiraClient) getProjects(args map[string]any) (toolResult, error) {
	limit := argIntDefault(args, "maxResults", 50)
	data, err := jiraGet[[]jiraProject](c, "/rest/api/2", "GET", fmt.Sprintf("/project?maxResults=%d", limit), nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil || len(*data) == 0 {
		return textResult("No projects found."), nil
	}
	var lines []string
	for i, p := range *data {
		lines = append(lines, fmt.Sprintf("%d. [%s] %s (%s) | %s", i+1, p.Key, p.Name, p.ProjectTypeKey, c.projectURL(p.Key)))
	}
	return textResult(fmt.Sprintf("%d project(s):\n%s", len(*data), strings.Join(lines, "\n"))), nil
}

func (c *JiraClient) getIssueTypes(projectKey, repoRoot string) (toolResult, error) {
	pk, err := c.resolveProjectKey(projectKey, repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	data, err := jiraGet[[]jiraIssueTypeStatuses](c, "/rest/api/2", "GET", "/project/"+url.PathEscape(pk)+"/statuses", nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil || len(*data) == 0 {
		return textResult("No issue types found."), nil
	}
	var lines []string
	for _, t := range *data {
		var statuses []string
		for _, s := range t.Statuses {
			statuses = append(statuses, s.Name)
		}
		lines = append(lines, fmt.Sprintf("%s: %s", t.Name, strings.Join(statuses, ", ")))
	}
	return textResult(fmt.Sprintf("Issue types and statuses for %s:\n%s", pk, strings.Join(lines, "\n"))), nil
}

func (c *JiraClient) getSprints(boardID int, state string, maxResults, startAt int) (toolResult, error) {
	q := url.Values{}
	q.Set("maxResults", fmt.Sprint(maxResults))
	q.Set("startAt", fmt.Sprint(startAt))
	if state != "" {
		q.Set("state", state)
	}
	data, err := jiraGet[jiraPageSprints](c, "/rest/agile/1.0", "GET", fmt.Sprintf("/board/%d/sprint?%s", boardID, q.Encode()), nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return textResult(fmt.Sprintf("No sprints found for board %d.", boardID)), nil
	}
	var lines []string
	for i, s := range data.Values {
		window := joinNonEmpty([]string{slice10(s.StartDate), slice10(s.EndDate)}, " -> ")
		goal := ""
		if strings.TrimSpace(s.Goal) != "" {
			goal = " | Goal: " + s.Goal
		}
		w := ""
		if window != "" {
			w = " | " + window
		}
		lines = append(lines, fmt.Sprintf("%d. [%d] %s | %s%s%s | %s", startAt+i+1, s.ID, s.Name, s.State, w, goal, c.sprintURL(boardID, s.ID)))
	}
	rangeEnd := startAt + len(data.Values)
	page := ""
	if !data.IsLast {
		page = fmt.Sprintf(" (showing %d-%d, use startAt=%d for next page)", startAt+1, rangeEnd, rangeEnd)
	}
	return textResult(fmt.Sprintf("Sprints for board %d%s:\nBoard URL: %s\n%s", boardID, page, c.boardURL(boardID), strings.Join(lines, "\n"))), nil
}

func (c *JiraClient) searchUsers(query string, maxResults int) (toolResult, error) {
	q := url.Values{}
	q.Set("username", query)
	q.Set("maxResults", fmt.Sprint(maxResults))
	data, err := jiraGet[[]jiraUser](c, "/rest/api/2", "GET", "/user/search?"+q.Encode(), nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil || len(*data) == 0 {
		return textResult("No users found."), nil
	}
	var lines []string
	n := 0
	for _, u := range *data {
		if !u.Active {
			continue
		}
		lines = append(lines, fmt.Sprintf("%d. %s (%s) — %s", n+1, u.DisplayName, u.Name, u.EmailAddress))
		n++
	}
	return textResult(fmt.Sprintf("%d user(s) found:\n%s", len(lines), strings.Join(lines, "\n"))), nil
}

func (c *JiraClient) getIssueFields(issueKey string) (summary, status, issueType string, err error) {
	data, e := jiraGet[jiraIssue](c, "/rest/api/2", "GET", "/issue/"+url.PathEscape(issueKey)+"?fields=summary,status,issuetype", nil)
	if e != nil {
		return "", "", "", e
	}
	if data == nil {
		return "", "", "", fmt.Errorf("Issue %s not found", issueKey)
	}
	return data.Fields.Summary, data.Fields.Status.Name, data.Fields.IssueType.Name, nil
}

type issueOverviewOpts struct {
	issueKey           string
	includeComments    bool
	commentsMaxResults int
	commentsStartAt    int
	includeTransitions bool
	includeSprint      bool
	descriptionCap     int // 0 = no cap
}

func issueOverviewOptsFromArgs(args map[string]any) issueOverviewOpts {
	full := argBool(args, "fullDescription")
	cap := 2000
	if full {
		cap = 0
	} else if has(args, "descriptionMaxChars") {
		cap = argInt(args, "descriptionMaxChars")
	}
	return issueOverviewOpts{
		issueKey:           argString(args, "issueKey"),
		includeComments:    argBoolDefault(args, "includeComments", true),
		commentsMaxResults: argIntDefault(args, "commentsMaxResults", 10),
		commentsStartAt:    argIntDefault(args, "commentsStartAt", 0),
		includeTransitions: argBoolDefault(args, "includeTransitions", true),
		includeSprint:      argBoolDefault(args, "includeSprint", true),
		descriptionCap:     cap,
	}
}

func (c *JiraClient) issueOverview(o issueOverviewOpts) (toolResult, error) {
	baseFields := "summary,description,status,assignee,reporter,priority,duedate,issuetype,labels,components,parent,fixVersions,versions,environment,timetracking,issuelinks,subtasks,attachment"
	epicFieldID, err := c.getEpicLinkFieldID()
	if err != nil {
		return toolResult{}, err
	}
	fields := baseFields
	if epicFieldID != "" {
		fields += "," + epicFieldID
	}
	raw, err := c.api("GET", "/issue/"+url.PathEscape(o.issueKey)+"?fields="+fields, nil)
	if err != nil {
		return toolResult{}, err
	}
	if len(raw) == 0 {
		return textResult("Issue not found."), nil
	}
	var issue jiraIssue
	if json.Unmarshal(raw, &issue) != nil {
		return textResult("Issue not found."), nil
	}
	f := issue.Fields

	// Epic link is a dynamic custom-field key; pull it out of the raw fields.
	epicLinkKey := ""
	if epicFieldID != "" {
		var probe struct {
			Fields map[string]json.RawMessage `json:"fields"`
		}
		if json.Unmarshal(raw, &probe) == nil {
			if rawVal, ok := probe.Fields[epicFieldID]; ok {
				var s string
				if json.Unmarshal(rawVal, &s) == nil {
					epicLinkKey = s
				}
			}
		}
	}

	priority := "None"
	if f.Priority != nil {
		priority = f.Priority.Name
	}
	assignee := "Unassigned"
	if f.Assignee != nil {
		assignee = f.Assignee.DisplayName
	}
	reporter := "None"
	if f.Reporter != nil {
		reporter = f.Reporter.DisplayName
	}
	lines := []string{
		fmt.Sprintf("Issue: %s — %s", issue.Key, f.Summary),
		"URL:        " + c.issueURL(issue.Key),
		"Status:     " + f.Status.Name,
		"Type:       " + f.IssueType.Name,
		"Priority:   " + priority,
		"Assignee:   " + assignee,
		"Reporter:   " + reporter,
		"Due Date:   " + orNone(f.DueDate),
		"Labels:     " + orNone(strings.Join(f.Labels, ", ")),
		"Components: " + orNone(joinNamed(f.Components, ", ")),
	}
	if f.Parent != nil {
		lines = append(lines, fmt.Sprintf("Parent:     [%s] %s (%s)", f.Parent.Key, f.Parent.Fields.Summary, f.Parent.Fields.IssueType.Name))
	}
	if epicLinkKey != "" {
		lines = append(lines, "Epic Link:  "+epicLinkKey)
	}
	if len(f.FixVersions) > 0 {
		var names []string
		for _, v := range f.FixVersions {
			names = append(names, v.Name)
		}
		lines = append(lines, "Fix Vers:   "+strings.Join(names, ", "))
	}
	if len(f.Versions) > 0 {
		lines = append(lines, "Affects:    "+joinNamed(f.Versions, ", "))
	}
	if f.TimeTracking != nil {
		var parts []string
		if f.TimeTracking.OriginalEstimate != "" {
			parts = append(parts, "estimate "+f.TimeTracking.OriginalEstimate)
		}
		if f.TimeTracking.RemainingEstimate != "" {
			parts = append(parts, "remaining "+f.TimeTracking.RemainingEstimate)
		}
		if f.TimeTracking.TimeSpent != "" {
			parts = append(parts, "spent "+f.TimeTracking.TimeSpent)
		}
		if len(parts) > 0 {
			lines = append(lines, "Time:       "+strings.Join(parts, ", "))
		}
	}
	if f.Environment != "" {
		lines = append(lines, "Environ:    "+f.Environment)
	}
	if len(f.Subtasks) > 0 {
		var parts []string
		for _, s := range f.Subtasks {
			parts = append(parts, fmt.Sprintf("[%s] %s (%s)", s.Key, s.Fields.Summary, s.Fields.Status.Name))
		}
		lines = append(lines, "Subtasks:   "+strings.Join(parts, ", "))
	}
	if len(f.IssueLinks) > 0 {
		var parts []string
		for _, l := range f.IssueLinks {
			switch {
			case l.OutwardIssue != nil:
				parts = append(parts, fmt.Sprintf("%s → [%s] %s", l.Type.Outward, l.OutwardIssue.Key, l.OutwardIssue.Fields.Summary))
			case l.InwardIssue != nil:
				parts = append(parts, fmt.Sprintf("%s ← [%s] %s", l.Type.Inward, l.InwardIssue.Key, l.InwardIssue.Fields.Summary))
			default:
				parts = append(parts, l.Type.Name)
			}
		}
		lines = append(lines, "Links:      "+strings.Join(parts, "; "))
	}

	if o.includeSprint {
		agileIssue, aerr := jiraGet[struct {
			Fields struct {
				Sprint        *jiraSprint  `json:"sprint"`
				ClosedSprints []jiraSprint `json:"closedSprints"`
			} `json:"fields"`
		}](c, "/rest/agile/1.0", "GET", "/issue/"+url.PathEscape(o.issueKey)+"?fields=sprint,closedSprints", nil)
		if aerr != nil {
			lines = append(lines, "Sprint:     unavailable ("+aerr.Error()+")")
		} else {
			var active *jiraSprint
			var closed []jiraSprint
			if agileIssue != nil {
				active = agileIssue.Fields.Sprint
				closed = agileIssue.Fields.ClosedSprints
			}
			if active != nil {
				lines = append(lines, fmt.Sprintf("Sprint:     [%d] %s (%s)", active.ID, active.Name, active.State))
			} else {
				lines = append(lines, "Sprint:     None")
			}
			if len(closed) > 0 {
				shown := closed
				more := ""
				if len(closed) > 5 {
					shown = closed[:5]
					more = fmt.Sprintf(" (+%d more)", len(closed)-5)
				}
				var parts []string
				for _, s := range shown {
					parts = append(parts, fmt.Sprintf("[%d] %s", s.ID, s.Name))
				}
				lines = append(lines, "History:    "+strings.Join(parts, ", ")+more)
			}
		}
	}

	if o.includeTransitions {
		transitions, _ := jiraGet[jiraTransitionsResult](c, "/rest/api/2", "GET", "/issue/"+url.PathEscape(o.issueKey)+"/transitions", nil)
		var names []string
		if transitions != nil {
			for _, t := range transitions.Transitions {
				names = append(names, fmt.Sprintf("%s -> %s", t.Name, t.To.Name))
			}
		}
		if len(names) > 0 {
			lines = append(lines, "Transitions: "+strings.Join(names, ", "))
		} else {
			lines = append(lines, "Transitions: (none)")
		}
	}

	desc := "(no description)"
	if f.Description != "" {
		desc = capText(f.Description, o.descriptionCap)
	}
	lines = append(lines, "", "Description:", desc)

	if len(f.Attachment) > 0 {
		lines = append(lines, "", fmt.Sprintf("Attachments: %d", len(f.Attachment)))
		for _, att := range f.Attachment {
			mt := att.MimeType
			if mt == "" {
				mt = "application/octet-stream"
			}
			lines = append(lines, fmt.Sprintf("  #%s %s (%s, %s)", att.ID, att.Filename, mt, formatBytes(att.Size)))
		}
		lines = append(lines, "Use jira_get_attachment with attachmentId to view contents.")
	}

	if o.includeComments {
		comments, _ := jiraGet[jiraCommentResult](c, "/rest/api/2", "GET",
			fmt.Sprintf("/issue/%s/comment?startAt=%d&maxResults=%d", url.PathEscape(o.issueKey), o.commentsStartAt, o.commentsMaxResults), nil)
		var items []jiraComment
		total := 0
		page := ""
		if comments != nil {
			items = comments.Comments
			total = comments.Total
			page = pagination(total, o.commentsStartAt, len(items))
		}
		lines = append(lines, "", fmt.Sprintf("Comments: %d%s", total, page))
		if len(items) == 0 {
			lines = append(lines, "(none)")
		} else {
			for _, cm := range items {
				lines = append(lines, fmt.Sprintf("--- #%s %s (%s) ---", cm.ID, cm.Author.DisplayName, slice10(cm.Created)), cm.Body, "")
			}
		}
	}

	return textResult(strings.TrimRight(strings.Join(lines, "\n"), "\n \t")), nil
}

func (c *JiraClient) boardOverview(args map[string]any) (toolResult, error) {
	boardID := argInt(args, "boardId")
	includeIssues := argBoolDefault(args, "includeIssues", true)
	sprintMaxResults := argIntDefault(args, "maxResults", 10)
	sprintStartAt := argInt(args, "startAt")
	sprintState := argString(args, "sprintState")
	assignee := argString(args, "assignee")
	status := argString(args, "status")
	const issueMaxResults, issueStartAt = 25, 0

	board, _ := jiraGet[jiraBoard](c, "/rest/agile/1.0", "GET", fmt.Sprintf("/board/%d", boardID), nil)
	sq := url.Values{}
	sq.Set("maxResults", fmt.Sprint(sprintMaxResults))
	sq.Set("startAt", fmt.Sprint(sprintStartAt))
	if sprintState != "" {
		sq.Set("state", sprintState)
	} else {
		sq.Set("state", "active,future")
	}
	sprints, err := jiraGet[jiraPageSprints](c, "/rest/agile/1.0", "GET", fmt.Sprintf("/board/%d/sprint?%s", boardID, sq.Encode()), nil)
	if err != nil {
		return toolResult{}, err
	}
	boardName := ""
	if board != nil {
		boardName = board.Name
	}
	if sprints == nil || len(sprints.Values) == 0 {
		suffix := ""
		if boardName != "" {
			suffix = " (" + boardName + ")"
		}
		return textResult(fmt.Sprintf("Board %d%s: no matching sprints.", boardID, suffix)), nil
	}

	var filterClauses []string
	if assignee != "" {
		filterClauses = append(filterClauses, "assignee = "+jsonStr(assignee))
	}
	if status != "" {
		filterClauses = append(filterClauses, "status = "+jsonStr(status))
	}
	issueJql := strings.Join(filterClauses, " AND ")

	type sprintIssues struct {
		issues *jiraSearchResult
	}
	issueBySprint := map[int]*jiraSearchResult{}
	if includeIssues {
		for _, sprint := range sprints.Values {
			q := url.Values{}
			q.Set("maxResults", fmt.Sprint(issueMaxResults))
			q.Set("startAt", fmt.Sprint(issueStartAt))
			q.Set("fields", "summary,status,assignee,priority,issuetype")
			if issueJql != "" {
				q.Set("jql", issueJql)
			}
			issues, _ := jiraGet[jiraSearchResult](c, "/rest/agile/1.0", "GET", fmt.Sprintf("/sprint/%d/issue?%s", sprint.ID, q.Encode()), nil)
			issueBySprint[sprint.ID] = issues
		}
	}

	boardTypeStr := "(unknown type)"
	boardNameStr := "(unknown)"
	if board != nil {
		boardNameStr = board.Name
		boardTypeStr = board.Type
	}
	lines := []string{
		fmt.Sprintf("Board: [%d] %s | %s", boardID, boardNameStr, boardTypeStr),
		"URL: " + c.boardURL(boardID),
		fmt.Sprintf("Sprints: %d", len(sprints.Values)),
		"",
	}
	for idx, sprint := range sprints.Values {
		window := joinNonEmpty([]string{slice10(sprint.StartDate), slice10(sprint.EndDate)}, " -> ")
		w := ""
		if window != "" {
			w = " | " + window
		}
		lines = append(lines, fmt.Sprintf("%d. [%d] %s | %s%s | %s", sprintStartAt+idx+1, sprint.ID, sprint.Name, sprint.State, w, c.sprintURL(boardID, sprint.ID)))
		if strings.TrimSpace(sprint.Goal) != "" {
			lines = append(lines, "   Goal: "+sprint.Goal)
		}
		if includeIssues {
			issueData := issueBySprint[sprint.ID]
			var issues []jiraIssue
			total := 0
			if issueData != nil {
				issues = issueData.Issues
				total = issueData.Total
			}
			lines = append(lines, fmt.Sprintf("   Issues: %d", total))
			for _, issue := range issues {
				a := "Unassigned"
				if issue.Fields.Assignee != nil {
					a = issue.Fields.Assignee.DisplayName
				}
				lines = append(lines, fmt.Sprintf("   - [%s] %s | %s | %s | %s", issue.Key, issue.Fields.Summary, issue.Fields.Status.Name, a, c.issueURL(issue.Key)))
			}
			if total > len(issues) {
				lines = append(lines, fmt.Sprintf("   ...and %d more (adjust issueStartAt/issueMaxResults).", total-len(issues)))
			}
		}
		lines = append(lines, "")
	}
	if !sprints.IsLast {
		lines = append(lines, fmt.Sprintf("More sprints available: use sprintStartAt=%d.", sprintStartAt+len(sprints.Values)))
	}
	return textResult(strings.TrimRight(strings.Join(lines, "\n"), "\n \t")), nil
}

// mutateIssue handles jira_mutate: create/update/sprint/transition/comment/link/worklog.
func (c *JiraClient) mutateIssue(args map[string]any, repoRoot string) (toolResult, error) {
	issueKey := strings.TrimSpace(argString(args, "issueKey"))
	var actions []string

	if create := argMap(args, "create"); create != nil {
		created, err := c.createIssueInternal(create, repoRoot)
		if err != nil {
			return toolResult{}, err
		}
		if created == nil {
			return toolResult{}, fmt.Errorf("Issue creation did not return an issue key.")
		}
		issueKey = created.Key
		actions = append(actions, "created issue")
	}

	if issueKey == "" {
		return toolResult{}, fmt.Errorf("Provide issueKey, or provide create with issueType and summary.")
	}

	if update := argMap(args, "update"); update != nil {
		updated, err := c.updateIssueFieldsInternal(issueKey, update)
		if err != nil {
			return toolResult{}, err
		}
		if updated {
			actions = append(actions, "updated fields")
		}
	}

	if has(args, "sprintId") {
		sprintID := argInt(args, "sprintId")
		if err := c.addIssuesToSprintInternal(sprintID, []string{issueKey}); err != nil {
			return toolResult{}, err
		}
		actions = append(actions, fmt.Sprintf("added to sprint %d", sprintID))
	}

	if argBool(args, "removeFromSprint") {
		if _, err := c.agile("POST", "/backlog/issue", map[string]any{"issues": []string{issueKey}}); err != nil {
			return toolResult{}, err
		}
		actions = append(actions, "moved to backlog")
	}

	if argString(args, "transitionId") != "" || argString(args, "transitionName") != "" {
		transitionID, err := c.resolveTransitionID(issueKey, argString(args, "transitionId"), argString(args, "transitionName"))
		if err != nil {
			return toolResult{}, err
		}
		if _, err := c.api("POST", "/issue/"+url.PathEscape(issueKey)+"/transitions", map[string]any{"transition": map[string]any{"id": transitionID}}); err != nil {
			return toolResult{}, err
		}
		actions = append(actions, "transitioned via "+transitionID)
	}

	if has(args, "comment") {
		body, err := validateCommentBody(argString(args, "comment"))
		if err != nil {
			return toolResult{}, err
		}
		if _, err := c.api("POST", "/issue/"+url.PathEscape(issueKey)+"/comment", map[string]any{"body": body}); err != nil {
			return toolResult{}, err
		}
		actions = append(actions, "added comment")
	}

	if paths := argStrSlice(args, "attachments"); len(paths) > 0 {
		names, err := c.uploadAttachments(issueKey, paths)
		if err != nil {
			return toolResult{}, err
		}
		actions = append(actions, "attached "+strings.Join(names, ", "))
	}

	var warnings []string
	if link := argMap(args, "link"); link != nil {
		enabled, err := c.getIssueLinkingEnabled()
		if err != nil {
			return toolResult{}, err
		}
		if !enabled {
			warnings = append(warnings, "issue linking is disabled in this Jira instance — add the link manually")
		} else {
			dir := argString(link, "direction")
			if dir == "" {
				dir = "outward"
			}
			target := argString(link, "targetIssueKey")
			outward, inward := issueKey, target
			if dir != "outward" {
				outward, inward = target, issueKey
			}
			if _, err := c.api("POST", "/issueLink", map[string]any{
				"type":         map[string]any{"name": argString(link, "linkType")},
				"outwardIssue": map[string]any{"key": outward},
				"inwardIssue":  map[string]any{"key": inward},
			}); err != nil {
				return toolResult{}, err
			}
			actions = append(actions, fmt.Sprintf("linked %s → %s", argString(link, "linkType"), target))
		}
	}

	if worklog := argMap(args, "worklog"); worklog != nil {
		wBody := map[string]any{"timeSpent": argString(worklog, "timeSpent")}
		if cmt := argString(worklog, "comment"); cmt != "" {
			wBody["comment"] = cmt
		}
		if started := argString(worklog, "started"); started != "" {
			wBody["started"] = started
		}
		if _, err := c.api("POST", "/issue/"+url.PathEscape(issueKey)+"/worklog", wBody); err != nil {
			return toolResult{}, err
		}
		actions = append(actions, "logged "+argString(worklog, "timeSpent"))
	}

	if len(actions) == 0 {
		return textResult("Nothing to mutate."), nil
	}
	parts := []string{fmt.Sprintf("Mutated %s: %s.", issueKey, strings.Join(actions, ", ")), c.issueURL(issueKey)}
	if len(warnings) > 0 {
		parts = append(parts, "Warnings: "+strings.Join(warnings, "; "))
	}
	return textResult(strings.Join(parts, "\n")), nil
}

// comment dispatches jira_comment add/update/delete.
func (c *JiraClient) comment(args map[string]any) (toolResult, error) {
	action := argString(args, "action")
	if action == "" {
		action = "add"
	}
	issueKey := argString(args, "issueKey")
	switch action {
	case "update":
		return c.editComment(issueKey, argString(args, "commentId"), argString(args, "body"))
	case "delete":
		return c.deleteComment(issueKey, argString(args, "commentId"))
	default:
		return c.addComment(issueKey, argString(args, "body"), argStrSlice(args, "attachments"))
	}
}

func (c *JiraClient) addComment(issueKey, body string, attachments []string) (toolResult, error) {
	var actions []string
	// Body is optional when only attaching files; required otherwise.
	if strings.TrimSpace(body) != "" || len(attachments) == 0 {
		v, err := validateCommentBody(body)
		if err != nil {
			return toolResult{}, err
		}
		if _, err := c.api("POST", "/issue/"+url.PathEscape(issueKey)+"/comment", map[string]any{"body": v}); err != nil {
			return toolResult{}, err
		}
		actions = append(actions, "comment added")
	}
	if len(attachments) > 0 {
		names, err := c.uploadAttachments(issueKey, attachments)
		if err != nil {
			return toolResult{}, err
		}
		actions = append(actions, "attached "+strings.Join(names, ", "))
	}
	return textResult(fmt.Sprintf("%s on %s.", strings.Join(actions, ", "), issueKey)), nil
}

func (c *JiraClient) editComment(issueKey, commentID, body string) (toolResult, error) {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" || commentID == "undefined" || commentID == "null" {
		return toolResult{}, fmt.Errorf("commentId is required.")
	}
	path := "/issue/" + url.PathEscape(issueKey) + "/comment/" + url.PathEscape(commentID)
	current, err := jiraGet[jiraComment](c, "/rest/api/2", "GET", path, nil)
	if err != nil {
		return toolResult{}, err
	}
	if current == nil {
		return toolResult{}, fmt.Errorf("Comment %s not found on %s.", commentID, issueKey)
	}
	if err := c.assertOwnComment(current); err != nil {
		return toolResult{}, err
	}
	v, err := validateCommentBody(body)
	if err != nil {
		return toolResult{}, err
	}
	if _, err := c.api("PUT", path, map[string]any{"body": v}); err != nil {
		return toolResult{}, err
	}
	return textResult(fmt.Sprintf("Comment %s updated on %s.", commentID, issueKey)), nil
}

func (c *JiraClient) deleteComment(issueKey, commentID string) (toolResult, error) {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" || commentID == "undefined" || commentID == "null" {
		return toolResult{}, fmt.Errorf("commentId is required.")
	}
	path := "/issue/" + url.PathEscape(issueKey) + "/comment/" + url.PathEscape(commentID)
	current, err := jiraGet[jiraComment](c, "/rest/api/2", "GET", path, nil)
	if err != nil {
		return toolResult{}, err
	}
	if current == nil {
		return toolResult{}, fmt.Errorf("Comment %s not found on %s.", commentID, issueKey)
	}
	if err := c.assertOwnComment(current); err != nil {
		return toolResult{}, err
	}
	if _, err := c.api("DELETE", path, nil); err != nil {
		return toolResult{}, err
	}
	return textResult(fmt.Sprintf("Comment %s deleted from %s.", commentID, issueKey)), nil
}

func (c *JiraClient) getBoards(projectKey string, maxResults, startAt int) (toolResult, error) {
	q := url.Values{}
	q.Set("maxResults", fmt.Sprint(maxResults))
	q.Set("startAt", fmt.Sprint(startAt))
	if projectKey != "" {
		q.Set("projectKeyOrId", projectKey)
	}
	data, err := jiraGet[jiraPageBoards](c, "/rest/agile/1.0", "GET", "/board?"+q.Encode(), nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return textResult("No boards found."), nil
	}
	var lines []string
	for i, b := range data.Values {
		hint := ""
		if b.Location != nil && b.Location.ProjectKey != "" {
			hint = " [" + b.Location.ProjectKey + "]"
		}
		lines = append(lines, fmt.Sprintf("%d. [%d] %s (%s)%s | %s", startAt+i+1, b.ID, b.Name, b.Type, hint, c.boardURL(b.ID)))
	}
	page := ""
	if !data.IsLast {
		page = fmt.Sprintf(" (use startAt=%d for next page)", startAt+len(data.Values))
	}
	return textResult(fmt.Sprintf("%d board(s)%s:\n%s", len(data.Values), page, strings.Join(lines, "\n"))), nil
}

func (c *JiraClient) listComponents(projectKey, query string, repoRoot string) (toolResult, error) {
	pk, err := c.resolveProjectKey(projectKey, repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	data, err := jiraGet[[]struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}](c, "/rest/api/2", "GET", "/project/"+url.PathEscape(pk)+"/components", nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil || len(*data) == 0 {
		return textResult(fmt.Sprintf("No components in %s.", pk)), nil
	}
	ql := strings.ToLower(strings.TrimSpace(query))
	var lines []string
	for _, comp := range *data {
		if ql != "" && !strings.Contains(strings.ToLower(comp.Name), ql) {
			continue
		}
		line := comp.Name
		if comp.Description != "" {
			line += " — " + comp.Description
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return textResult(fmt.Sprintf("No component matching %q in %s.", query, pk)), nil
	}
	return textResult(fmt.Sprintf("Components in %s:\n%s", pk, strings.Join(lines, "\n"))), nil
}

func (c *JiraClient) listVersions(projectKey, query string, maxResults *int, repoRoot string) (toolResult, error) {
	pk, err := c.resolveProjectKey(projectKey, repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	data, err := jiraGet[[]jiraVersion](c, "/rest/api/2", "GET", "/project/"+url.PathEscape(pk)+"/versions", nil)
	if err != nil {
		return toolResult{}, err
	}
	query = strings.TrimSpace(query)
	createHint := func(name string) string {
		return fmt.Sprintf(`Create it with: jira_version action=create projectKey=%s name="%s"`, pk, name)
	}
	if data == nil || len(*data) == 0 {
		if query != "" {
			return textResult(fmt.Sprintf("No versions in %s. %s", pk, createHint(query))), nil
		}
		return textResult(fmt.Sprintf("No versions in %s.", pk)), nil
	}
	versions := *data
	var filtered []jiraVersion
	if query != "" {
		ql := strings.ToLower(query)
		for _, v := range versions {
			if strings.Contains(strings.ToLower(v.Name), ql) {
				filtered = append(filtered, v)
			}
		}
	} else {
		filtered = versions
	}
	if query != "" && len(filtered) == 0 {
		return textResult(fmt.Sprintf("No version matching %q in %s (%d other version(s) exist). %s", query, pk, len(versions), createHint(query))), nil
	}
	sorted := append([]jiraVersion(nil), filtered...)
	stableSortVersions(sorted)
	limit := len(sorted)
	if maxResults != nil {
		limit = *maxResults
	}
	shown := sorted
	if limit < len(sorted) {
		shown = sorted[:limit]
	}
	var lines []string
	for i, v := range shown {
		var tags []string
		if v.Released {
			tags = append(tags, "released")
		}
		if v.Archived {
			tags = append(tags, "archived")
		}
		tagStr := ""
		if len(tags) > 0 {
			tagStr = " [" + strings.Join(tags, ", ") + "]"
		}
		var dateParts []string
		if v.StartDate != "" {
			dateParts = append(dateParts, "start "+v.StartDate)
		}
		if v.ReleaseDate != "" {
			dateParts = append(dateParts, "release "+v.ReleaseDate)
		}
		dateStr := ""
		if len(dateParts) > 0 {
			dateStr = " (" + strings.Join(dateParts, ", ") + ")"
		}
		lines = append(lines, fmt.Sprintf("%d. [%s] %s%s%s", i+1, v.ID, v.Name, tagStr, dateStr))
	}
	header := fmt.Sprintf("%d version(s) in %s:", len(filtered), pk)
	if query != "" {
		header = fmt.Sprintf("%d version(s) matching %q in %s:", len(filtered), query, pk)
	}
	more := ""
	if len(filtered) > len(shown) {
		more = fmt.Sprintf("\n...and %d more (raise maxResults).", len(filtered)-len(shown))
	}
	return textResult(fmt.Sprintf("%s\n%s%s", header, strings.Join(lines, "\n"), more)), nil
}

// stableSortVersions: released last, archived last, releaseDate DESC.
func stableSortVersions(v []jiraVersion) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && versionLess(v[j], v[j-1]); j-- {
			v[j-1], v[j] = v[j], v[j-1]
		}
	}
}

func versionLess(a, b jiraVersion) bool {
	if a.Released != b.Released {
		return !a.Released // unreleased first
	}
	if a.Archived != b.Archived {
		return !a.Archived // unarchived first
	}
	return b.ReleaseDate < a.ReleaseDate // releaseDate DESC
}

func (c *JiraClient) mutateVersion(args map[string]any, repoRoot string) (toolResult, error) {
	action := argString(args, "action")
	if action == "" {
		action = "create"
	}
	if action == "create" {
		pk, err := c.resolveProjectKey(argString(args, "projectKey"), repoRoot)
		if err != nil {
			return toolResult{}, err
		}
		name := strings.TrimSpace(argString(args, "name"))
		if name == "" {
			return toolResult{}, fmt.Errorf("name is required to create a version.")
		}
		body := map[string]any{"project": pk, "name": name}
		if has(args, "description") {
			body["description"] = argString(args, "description")
		}
		if d := argString(args, "releaseDate"); d != "" {
			body["releaseDate"] = d
		}
		if d := argString(args, "startDate"); d != "" {
			body["startDate"] = d
		}
		if has(args, "released") {
			body["released"] = argBool(args, "released")
		}
		if has(args, "archived") {
			body["archived"] = argBool(args, "archived")
		}
		created, err := jiraGet[jiraVersion](c, "/rest/api/2", "POST", "/version", body)
		if err != nil {
			return toolResult{}, err
		}
		if created == nil {
			return toolResult{}, fmt.Errorf("Jira returned no body when creating version.")
		}
		return textResult(fmt.Sprintf("Created version [%s] %s in %s.", created.ID, created.Name, pk)), nil
	}

	id := strings.TrimSpace(argString(args, "id"))
	if id == "" {
		return toolResult{}, fmt.Errorf("version id is required for action=%s.", action)
	}
	if action == "delete" {
		if _, err := c.api("DELETE", "/version/"+url.PathEscape(id), nil); err != nil {
			return toolResult{}, err
		}
		return textResult("Deleted version " + id + "."), nil
	}

	body := map[string]any{}
	if has(args, "name") {
		body["name"] = argString(args, "name")
	}
	if has(args, "description") {
		body["description"] = argString(args, "description")
	}
	if has(args, "startDate") {
		body["startDate"] = argString(args, "startDate")
	}
	if has(args, "releaseDate") {
		body["releaseDate"] = argString(args, "releaseDate")
	}
	if has(args, "released") {
		body["released"] = argBool(args, "released")
	}
	if has(args, "archived") {
		body["archived"] = argBool(args, "archived")
	}
	if action == "release" {
		body["released"] = true
		if _, ok := body["releaseDate"]; !ok {
			body["releaseDate"] = time.Now().UTC().Format("2006-01-02")
		}
	} else if action == "archive" {
		body["archived"] = true
	}
	if len(body) == 0 {
		return toolResult{}, fmt.Errorf("Nothing to update.")
	}
	updated, err := jiraGet[jiraVersion](c, "/rest/api/2", "PUT", "/version/"+url.PathEscape(id), body)
	if err != nil {
		return toolResult{}, err
	}
	label := id
	if updated != nil {
		label = fmt.Sprintf("[%s] %s", updated.ID, updated.Name)
	}
	switch action {
	case "release":
		return textResult(fmt.Sprintf("Released version %s on %s.", label, body["releaseDate"])), nil
	case "archive":
		return textResult("Archived version " + label + "."), nil
	}
	return textResult("Updated version " + label + "."), nil
}

// uploadAttachments POSTs one or more local files to an issue's attachments
// endpoint as multipart/form-data. Jira requires the X-Atlassian-Token header
// and the "file" form field; multiple files ride one request. Returns the
// uploaded base filenames.
func (c *JiraClient) uploadAttachments(issueKey string, paths []string) ([]string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	var names []string
	for _, p := range paths {
		abs, _ := filepath.Abs(p)
		f, err := os.Open(abs)
		if err != nil {
			return nil, fmt.Errorf("cannot open attachment %s: %w", p, err)
		}
		part, err := w.CreateFormFile("file", filepath.Base(abs))
		if err != nil {
			f.Close()
			return nil, err
		}
		if _, err := io.Copy(part, f); err != nil {
			f.Close()
			return nil, err
		}
		f.Close()
		names = append(names, filepath.Base(abs))
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	reqURL := c.baseURL + "/rest/api/2/issue/" + url.PathEscape(issueKey) + "/attachments"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Atlassian-Token", "no-check") // Jira XSRF guard for multipart uploads
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", formatJiraError(res.StatusCode, "POST", "/issue/"+issueKey+"/attachments", parseJiraErrorDetails(string(raw))))
	}
	return names, nil
}

// getAttachment fetches metadata then streams or buffers the content.
func (c *JiraClient) getAttachment(args map[string]any) (toolResult, error) {
	id := strings.TrimSpace(argString(args, "attachmentId"))
	if id == "" {
		return toolResult{}, fmt.Errorf("attachmentId is required.")
	}
	meta, err := jiraGet[jiraAttachment](c, "/rest/api/2", "GET", "/attachment/"+url.PathEscape(id), nil)
	if err != nil {
		return toolResult{}, err
	}
	if meta == nil {
		return toolResult{}, fmt.Errorf("Attachment %s not found.", id)
	}
	mimeType := meta.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	// saveTo: stream straight to disk.
	if saveTo := argString(args, "saveTo"); saveTo != "" {
		path, _ := filepath.Abs(saveTo)
		res, err := c.fetchContent(meta.Content, 300*time.Second)
		if err != nil {
			return toolResult{}, err
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			raw, _ := io.ReadAll(res.Body)
			return toolResult{}, fmt.Errorf("%s", formatJiraError(res.StatusCode, "GET", meta.Content, parseJiraErrorDetails(string(raw))))
		}
		f, err := os.Create(path)
		if err != nil {
			return toolResult{}, err
		}
		defer f.Close()
		if _, err := io.Copy(f, res.Body); err != nil {
			return toolResult{}, err
		}
		sizeLabel := "unknown size"
		if meta.Size > 0 {
			sizeLabel = formatBytes(meta.Size)
		}
		return textResult(fmt.Sprintf("Saved attachment #%s (%s — %s, %s) to %s", id, meta.Filename, mimeType, sizeLabel, path)), nil
	}

	if meta.Size > 0 && meta.Size > maxVideoSourceBytes {
		return toolResult{}, fmt.Errorf("Attachment #%s is %s, exceeds the %s inline cap. Pass saveTo=/absolute/path to stream it to disk.", id, formatBytes(meta.Size), formatBytes(maxVideoSourceBytes))
	}
	res, err := c.fetchContent(meta.Content, 60*time.Second)
	if err != nil {
		return toolResult{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(res.Body)
		return toolResult{}, fmt.Errorf("%s", formatJiraError(res.StatusCode, "GET", meta.Content, parseJiraErrorDetails(string(raw))))
	}
	if cl := res.Header.Get("content-length"); cl != "" {
		var declared int64
		fmt.Sscanf(cl, "%d", &declared)
		if declared > maxVideoSourceBytes {
			return toolResult{}, fmt.Errorf("Attachment #%s is %s, exceeds the %s inline cap. Pass saveTo=/absolute/path to stream it to disk.", id, formatBytes(declared), formatBytes(maxVideoSourceBytes))
		}
	}
	buffer, err := io.ReadAll(io.LimitReader(res.Body, maxVideoSourceBytes+1))
	if err != nil {
		return toolResult{}, err
	}
	if int64(len(buffer)) > maxVideoSourceBytes {
		return toolResult{}, fmt.Errorf("Attachment #%s downloaded %s, exceeds the %s inline cap. Pass saveTo=/absolute/path to stream it to disk.", id, formatBytes(int64(len(buffer))), formatBytes(maxVideoSourceBytes))
	}

	return buildAttachmentResult(attachmentArgs{
		id:             id,
		filename:       meta.Filename,
		mimeType:       mimeType,
		buffer:         buffer,
		maxDimension:   argIntPtr(args, "maxDimension"),
		quality:        argIntPtr(args, "quality"),
		frames:         argIntPtr(args, "frames"),
		start:          argFloatPtr(args, "start"),
		end:            argFloatPtr(args, "end"),
		mode:           argString(args, "mode"),
		sceneThreshold: argFloatPtr(args, "sceneThreshold"),
	})
}

// fetchContent GETs an absolute attachment-content URL with just the auth header.
func (c *JiraClient) fetchContent(contentURL string, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req, err := http.NewRequestWithContext(ctx, "GET", contentURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	// cancel is intentionally tied to body close via a wrapper.
	res.Body = &cancelReadCloser{ReadCloser: res.Body, cancel: cancel}
	return res, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// ── small formatting helpers ─────────────────────────────────────────────────

func slice10(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

func joinNamed(items []jiraNamed, sep string) string {
	var names []string
	for _, i := range items {
		names = append(names, i.Name)
	}
	return strings.Join(names, sep)
}

func joinNonEmpty(items []string, sep string) string {
	var nonEmpty []string
	for _, i := range items {
		if i != "" {
			nonEmpty = append(nonEmpty, i)
		}
	}
	return strings.Join(nonEmpty, sep)
}
