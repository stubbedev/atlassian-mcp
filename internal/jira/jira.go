package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stubbedev/atlassian-mcp/internal/arg"
	"github.com/stubbedev/atlassian-mcp/internal/attachment"
	"github.com/stubbedev/atlassian-mcp/internal/config"
	"github.com/stubbedev/atlassian-mcp/internal/gitrepo"
	"github.com/stubbedev/atlassian-mcp/internal/httpx"
	"github.com/stubbedev/atlassian-mcp/internal/logx"
	"github.com/stubbedev/atlassian-mcp/internal/markup"
	"github.com/stubbedev/atlassian-mcp/internal/media"
	"github.com/stubbedev/atlassian-mcp/internal/tool"
)

// ── Wire types (mirror the TypeScript interfaces) ────────────────────────────

type jiraNamed struct {
	Name string `json:"name"`
}

type jiraFieldSchema struct {
	Type   string `json:"type"`
	Items  string `json:"items"`
	Custom string `json:"custom"`
}

type jiraField struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Custom bool            `json:"custom"`
	Schema jiraFieldSchema `json:"schema"`
}

// jiraFieldMeta is one entry of createmeta/editmeta: what this screen accepts
// for the field, including whether it is mandatory and its allowed values.
type jiraFieldMeta struct {
	FieldID       string          `json:"fieldId"`
	Name          string          `json:"name"`
	Required      bool            `json:"required"`
	Schema        jiraFieldSchema `json:"schema"`
	AllowedValues []struct {
		ID    string `json:"id"`
		Value string `json:"value"`
		Name  string `json:"name"`
	} `json:"allowedValues"`
}

// label is what jira_mutate customFields wants for this allowed value.
func (m jiraFieldMeta) labels() []string {
	var out []string
	for _, av := range m.AllowedValues {
		switch {
		case av.Value != "":
			out = append(out, av.Value)
		case av.Name != "":
			out = append(out, av.Name)
		}
	}
	return out
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
	raw        map[string]json.RawMessage
}

func (f *jiraIssueFields) UnmarshalJSON(b []byte) error {
	type typed jiraIssueFields
	if err := json.Unmarshal(b, (*typed)(f)); err != nil {
		return err
	}
	return json.Unmarshal(b, &f.raw)
}

func (f jiraIssueFields) value(field jiraField) string {
	return formatFieldValue(f.raw[field.ID])
}

func (f jiraIssueFields) assigneeName() string {
	if f.Assignee == nil {
		return "Unassigned"
	}
	return f.Assignee.DisplayName
}

const issueListFields = "summary,status,assignee,priority,issuetype"

func (c *Client) issueListLine(i jiraIssue, extra []jiraField) string {
	parts := []string{"[" + i.Key + "] " + i.Fields.Summary, i.Fields.Status.Name, i.Fields.assigneeName()}
	for _, f := range extra {
		parts = append(parts, f.Name+": "+i.Fields.value(f))
	}
	return strings.Join(append(parts, c.issueURL(i.Key)), " | ")
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

type CurrentUser struct {
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

type jiraBoardConfig struct {
	Estimation struct {
		Type  string `json:"type"`
		Field struct {
			FieldID string `json:"fieldId"`
		} `json:"field"`
	} `json:"estimation"`
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
	EmojiRe           = regexp.MustCompile(`[\x{1F000}-\x{1FAFF}\x{2600}-\x{27BF}\x{2190}-\x{21FF}\x{2B00}-\x{2BFF}\x{FE00}-\x{FE0F}\x{1F1E6}-\x{1F1FF}]`)
)

func capText(value string, limit int) string {
	if limit <= 0 {
		return value
	}
	r := []rune(value)
	if len(r) <= limit {
		return value
	}
	more := len(r) - limit
	return fmt.Sprintf("%s\n... (truncated, %d more chars — pass fullDescription=true for the rest)", string(r[:limit]), more)
}

func pagination(total, startAt, count int) string {
	end := startAt + count
	if total > end {
		return fmt.Sprintf(" (showing %d–%d of %d, use startAt=%d for next page)", startAt+1, end, total, end)
	}
	return ""
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s) //nolint:errchkjson // a string cannot fail to marshal
	return string(b)
}

func buildJQL(query, jql, project, status, assignee, issueType string) (string, error) {
	if jql != "" {
		if len(jql) > 2000 {
			return "", errors.New("JQL query too long (max 2000 characters).")
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
		return "", errors.New("Provide at least one of: query, jql, project, status, assignee, issueType")
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

// validateCommentBody is the single gate every Jira comment write passes
// through: emoji are rejected, and markdown is converted to wiki markup so a
// comment renders as intended regardless of what the caller sent.
func validateCommentBody(body string) (string, bool, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", false, errors.New("Jira comment body must not be empty.")
	}
	if EmojiRe.MatchString(trimmed) {
		return "", false, errors.New("Jira comments must not include emoji. Use concise Jira wiki markup or plain text only.")
	}
	wiki, converted := markup.MarkdownToJiraWiki(trimmed)
	return wiki, converted, nil
}

// wikiNote labels a conversion in the tool result so the caller can see its
// markdown was rewritten rather than posted literally.
func wikiNote(converted bool) string {
	if converted {
		return " (markdown converted to wiki markup)"
	}
	return ""
}

// ── Client ───────────────────────────────────────────────────────────────────

type Client struct {
	baseURL string
	token   string
	creds   config.Credentials

	mu                 sync.Mutex // guards the lazily-populated caches below (HTTP is concurrent)
	currentUser        *CurrentUser
	projects           []jiraProject
	projectsCached     bool
	issueLinking       bool
	issueLinkingCached bool
	linkTypes          []jiraIssueLinkType
	linkTypesCached    bool
	fields             []jiraField
	fieldsCached       bool
	issueTypeCache     map[string]string
}

func NewJiraClient(baseURL, token string, creds config.Credentials) *Client {
	return &Client{
		baseURL:        strings.TrimRight(baseURL, "/"),
		token:          token,
		creds:          creds,
		issueTypeCache: map[string]string{},
	}
}

func (c *Client) issueURL(key string) string {
	return c.baseURL + "/browse/" + url.PathEscape(key)
}

func (c *Client) projectURL(key string) string {
	return c.baseURL + "/projects/" + url.PathEscape(key)
}

func (c *Client) boardURL(boardID int) string {
	return fmt.Sprintf("%s/secure/RapidBoard.jspa?rapidView=%d", c.baseURL, boardID)
}

func (c *Client) sprintURL(boardID, sprintID int) string {
	return fmt.Sprintf("%s&sprint=%d", c.boardURL(boardID), sprintID)
}

// doRequest performs an HTTP request against apiBase+path, returning the raw
// response body (nil on 204/empty) or a formatted error on non-2xx.
func (c *Client) doRequest(apiBase, method, path string, body any) ([]byte, error) {
	reqURL := c.baseURL + apiBase + path
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
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
	res, err := httpx.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", formatJiraError(res.StatusCode, method, path, parseJiraErrorDetails(string(raw))))
	}
	if res.StatusCode == http.StatusNoContent || len(raw) == 0 {
		return nil, nil
	}
	return raw, nil
}

// jiraGet decodes into *T, returning nil (not error) when the body is empty or
// unparseable — matching the TS request() that resolves to null in those cases.
func jiraGet[T any](c *Client, apiBase, method, path string, body any) (*T, error) {
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

func (c *Client) api(method, path string, body any) ([]byte, error) {
	return c.doRequest("/rest/api/2", method, path, body)
}

func (c *Client) agile(method, path string, body any) ([]byte, error) {
	return c.doRequest("/rest/agile/1.0", method, path, body)
}

func normalizeIdentity(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func (c *Client) getCurrentUser() (*CurrentUser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentUser != nil {
		return c.currentUser, nil
	}
	me, err := jiraGet[CurrentUser](c, "/rest/api/2", "GET", "/myself", nil)
	if err != nil {
		return nil, err
	}
	if me == nil {
		return nil, errors.New("Could not determine current Jira user identity.")
	}
	c.currentUser = me
	return me, nil
}

func (c *Client) Whoami() (*CurrentUser, error) { return c.getCurrentUser() }

func (c *Client) getIssueLinkingEnabled() (bool, error) {
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

// jiraIssueLinkType is one entry of /issueLinkType: the type name plus the two
// phrases Jira shows on the issues at either end of the link.
type jiraIssueLinkType struct {
	Name    string `json:"name"`
	Inward  string `json:"inward"`
	Outward string `json:"outward"`
}

// issueLinkTypes returns /issueLinkType once per process, cached. A lookup that
// cannot run yields nil, which resolveLinkType treats as "cannot tell".
func (c *Client) issueLinkTypes() []jiraIssueLinkType {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.linkTypesCached {
		return c.linkTypes
	}
	data, err := jiraGet[struct {
		IssueLinkTypes []jiraIssueLinkType `json:"issueLinkTypes"`
	}](c, "/rest/api/2", "GET", "/issueLinkType", nil)
	c.linkTypesCached = true
	if err != nil {
		logx.Logf("issue link type lookup failed, sending the name as given: %v", err)
		return nil
	}
	if data != nil {
		c.linkTypes = data.IssueLinkTypes
	}
	return c.linkTypes
}

// resolveLinkType maps whatever the caller called the relationship onto a type
// this instance actually has. It matters because Jira answers POST /issueLink
// with a bare 404 "Resource not found" when the type name is wrong, which reads
// as a missing issue: "Relates to" is the phrase, the type is called "Relates".
// Matching the inward phrase also reverses the link — "is blocked by" is the
// Blocks type pointing the other way.
func (c *Client) resolveLinkType(name string) (canonical string, reversed bool, err error) {
	name = strings.TrimSpace(name)
	types := c.issueLinkTypes()
	if name == "" || len(types) == 0 {
		return name, false, nil
	}
	for _, t := range types {
		if strings.EqualFold(t.Name, name) {
			return t.Name, false, nil
		}
	}
	for _, t := range types {
		if strings.EqualFold(t.Outward, name) {
			return t.Name, false, nil
		}
	}
	for _, t := range types {
		if strings.EqualFold(t.Inward, name) {
			return t.Name, true, nil
		}
	}
	available := make([]string, 0, len(types))
	for _, t := range types {
		available = append(available, fmt.Sprintf("%s (%s / %s)", t.Name, t.Outward, t.Inward))
	}
	return "", false, fmt.Errorf("Link type %q does not exist in this Jira. Available: %s.", name, strings.Join(available, ", "))
}

// fieldList returns /field once per process, cached.
func (c *Client) fieldList() ([]jiraField, error) {
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
func (c *Client) fieldIDBySchema(schemaKey string) (string, error) {
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

func (c *Client) getEpicLinkFieldID() (string, error) {
	return c.fieldIDBySchema("com.pyxis.greenhopper.jira:gh-epic-link")
}

func (c *Client) getEpicNameFieldID() (string, error) {
	return c.fieldIDBySchema("com.pyxis.greenhopper.jira:gh-epic-label")
}

func (c *Client) resolveField(nameOrID string) (jiraField, error) {
	key := strings.TrimSpace(nameOrID)
	if key == "" {
		return jiraField{}, errors.New("field entries must be a field name or id.")
	}
	fields, err := c.fieldList()
	if err != nil {
		return jiraField{}, err
	}
	var names []string
	for _, f := range fields {
		if f.ID == key || strings.EqualFold(f.Name, key) {
			return f, nil
		}
		if f.Custom {
			names = append(names, f.Name)
		}
	}
	return jiraField{}, fmt.Errorf("Unknown Jira field %q. Custom fields on this instance: %s", key, strings.Join(names, ", "))
}

func (c *Client) resolveFields(namesOrIDs []string) ([]jiraField, error) {
	out := make([]jiraField, 0, len(namesOrIDs))
	seen := make(map[string]bool, len(namesOrIDs))
	for _, n := range namesOrIDs {
		f, err := c.resolveField(n)
		if err != nil {
			return nil, err
		}
		if seen[f.ID] {
			continue
		}
		seen[f.ID] = true
		out = append(out, f)
	}
	return out, nil
}

func fieldIDs(fields []jiraField) string {
	var b strings.Builder
	for _, f := range fields {
		b.WriteByte(',')
		b.WriteString(f.ID)
	}
	return b.String()
}

var greenhopperSprintNameRe = regexp.MustCompile(`\bname=([^,\]]*)`)

func formatFieldValue(raw json.RawMessage) string {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return "None"
	}
	return formatFieldAny(v)
}

func formatFieldAny(v any) string {
	switch tv := v.(type) {
	case nil:
		return "None"
	case string:
		if strings.HasPrefix(tv, "com.atlassian.greenhopper.service.sprint.Sprint@") {
			if m := greenhopperSprintNameRe.FindStringSubmatch(tv); m != nil {
				return m[1]
			}
		}
		return orNone(tv)
	case float64:
		return strconv.FormatFloat(tv, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(tv)
	case []any:
		if len(tv) == 0 {
			return "None"
		}
		parts := make([]string, 0, len(tv))
		for _, item := range tv {
			parts = append(parts, formatFieldAny(item))
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		return formatFieldObject(tv)
	}
	return fmt.Sprint(v)
}

func formatFieldObject(m map[string]any) string {
	for _, k := range []string{"displayName", "value", "name", "key"} {
		s, ok := m[k].(string)
		if !ok || s == "" {
			continue
		}
		if child, ok := m["child"].(map[string]any); ok && k == "value" {
			return s + " > " + formatFieldObject(child)
		}
		return s
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprint(m)
	}
	return string(b)
}

// coerceFieldValue turns the plain value a caller passes ("Bug fix", 5,
// ["a","b"], "jdoe") into the shape Jira's field payload wants, using the
// field's own schema. Objects and object arrays pass through untouched, so a
// caller that already knows Jira's wire format keeps full control.
func coerceFieldValue(schema jiraFieldSchema, v any) any {
	switch tv := v.(type) {
	case nil:
		return nil
	case map[string]any:
		return tv
	case []any:
		if schema.Type != "array" {
			return tv
		}
		out := make([]any, 0, len(tv))
		for _, item := range tv {
			out = append(out, wrapScalar(schema.Items, item))
		}
		return out
	}
	if schema.Type == "array" {
		return []any{wrapScalar(schema.Items, v)} // single value for a multi-value field
	}
	return wrapScalar(schema.Type, v)
}

// wrapScalar wraps one scalar according to a field (or item) type. Jira names
// select options by "value", and users/versions/components/groups by "name".
func wrapScalar(typ string, v any) any {
	s, isStr := v.(string)
	if !isStr {
		return v // numbers, bools: Jira takes them raw
	}
	switch typ {
	case "option", "option-with-child":
		return map[string]any{"value": s}
	case "user", "version", "component", "group", "project", "priority", "resolution", "issuetype", "status":
		return map[string]any{"name": s}
	default:
		return s // string, number, date, datetime, any, sd-*
	}
}

// allowedHint renders a field's allowed values, capped — some projects have
// hundreds of versions and the full list would bury the answer.
func allowedHint(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	const maxLabels = 20
	if len(labels) <= maxLabels {
		return " — allowed: " + strings.Join(labels, " | ")
	}
	return fmt.Sprintf(" — allowed: %s | ...and %d more", strings.Join(labels[:maxLabels], " | "), len(labels)-maxLabels)
}

// sendHint is the example value to put in customFields for a schema.
func sendHint(schema jiraFieldSchema) string {
	one := func(typ string) string {
		switch typ {
		case "number":
			return "5"
		case "date":
			return `"2026-01-31"`
		case "datetime":
			return `"2026-01-31T09:00:00.000+0000"`
		case "option", "option-with-child":
			return `"<allowed value>"`
		case "user":
			return `"username"`
		case "version", "component", "group", "priority", "resolution":
			return `"<name>"`
		default:
			return `"text"`
		}
	}
	if schema.Type == "array" {
		return "[" + one(schema.Items) + "]"
	}
	return one(schema.Type)
}

func (c *Client) applyCustomFields(target, custom map[string]any) error {
	for k, v := range custom {
		f, err := c.resolveField(k)
		if err != nil {
			return err
		}
		target[f.ID] = coerceFieldValue(f.Schema, v)
	}
	return nil
}

// createMeta returns the fields the create screen for one project+issue type
// accepts, keyed by field id.
// ponytail: Jira 9's paginated createmeta only — the pre-9 flat
// /issue/createmeta?projectKeys= shape is gone here (404 on 9.12); add it back
// if this ever has to talk to an older Server.
func (c *Client) createMeta(projectKey, issueType string) (map[string]jiraFieldMeta, error) {
	if projectKey == "" || issueType == "" {
		return nil, nil
	}
	types, err := jiraGet[struct {
		Values []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"values"`
	}](c, "/rest/api/2", "GET", "/issue/createmeta/"+url.PathEscape(projectKey)+"/issuetypes?maxResults=200", nil)
	if err != nil || types == nil {
		return nil, err
	}
	typeID := ""
	var names []string
	for _, t := range types.Values {
		names = append(names, t.Name)
		if strings.EqualFold(t.Name, issueType) {
			typeID = t.ID
		}
	}
	if typeID == "" {
		return nil, fmt.Errorf("Issue type %q not available in %s. Types: %s", issueType, projectKey, strings.Join(names, ", "))
	}
	page, err := jiraGet[struct {
		Values []jiraFieldMeta `json:"values"`
	}](c, "/rest/api/2", "GET", "/issue/createmeta/"+url.PathEscape(projectKey)+"/issuetypes/"+url.PathEscape(typeID)+"?maxResults=200", nil)
	if err != nil || page == nil {
		return nil, err
	}
	meta := map[string]jiraFieldMeta{}
	for _, f := range page.Values {
		if f.FieldID != "" {
			meta[f.FieldID] = f
		}
	}
	return meta, nil
}

// editMeta returns the fields an existing issue's edit screen accepts.
func (c *Client) editMeta(issueKey string) (map[string]jiraFieldMeta, error) {
	data, err := jiraGet[struct {
		Fields map[string]jiraFieldMeta `json:"fields"`
	}](c, "/rest/api/2", "GET", "/issue/"+url.PathEscape(issueKey)+"/editmeta", nil)
	if err != nil || data == nil {
		return nil, err
	}
	return data.Fields, nil
}

// listFields renders the field catalog for jira_search resource=fields. With a
// project (and optionally issueType) or an issueKey it reports the real screen:
// which fields exist there, which are required, and the allowed values. Without
// either it can only list the instance-wide custom fields.
func (c *Client) listFields(args map[string]any, repoRoot string) (tool.Result, error) {
	query := strings.ToLower(strings.TrimSpace(arg.String(args, "query")))
	issueKey := strings.TrimSpace(arg.String(args, "issueKey"))
	issueType := strings.TrimSpace(arg.String(args, "issueType"))
	projectKey := strings.TrimSpace(arg.String(args, "projectKey"))
	if projectKey == "" {
		projectKey = strings.TrimSpace(arg.String(args, "project"))
	}

	var (
		meta  map[string]jiraFieldMeta
		scope string
		err   error
	)
	switch {
	case issueKey != "":
		meta, err = c.editMeta(issueKey)
		scope = "settable on " + issueKey + " (jira_mutate update.customFields)"
	case projectKey != "" || issueType != "":
		pk, perr := c.resolveProjectKey(projectKey, repoRoot)
		if perr != nil {
			return tool.Result{}, perr
		}
		if issueType == "" {
			return tool.Result{}, errors.New("Pass issueType together with project (create screens are per issue type), or pass issueKey for an existing issue.")
		}
		meta, err = c.createMeta(pk, issueType)
		scope = fmt.Sprintf("on the %s create screen in %s (jira_mutate create.customFields)", issueType, pk)
	}
	if err != nil {
		return tool.Result{}, err
	}

	matches := func(name, id string) bool {
		return query == "" || strings.Contains(strings.ToLower(name), query) || strings.Contains(strings.ToLower(id), query)
	}

	if meta != nil {
		var req, opt []string
		for id, m := range meta {
			if !matches(m.Name, id) {
				continue
			}
			line := fmt.Sprintf("- %s [%s] send %s%s", m.Name, id, sendHint(m.Schema), allowedHint(m.labels()))
			if m.Required {
				req = append(req, line)
			} else {
				opt = append(opt, line)
			}
		}
		sort.Strings(req)
		sort.Strings(opt)
		if len(req)+len(opt) == 0 {
			return tool.TextResult("No matching fields " + scope + "."), nil
		}
		out := []string{fmt.Sprintf("Fields %s.", scope), "Keys accept the field name or its id; values are the plain form shown (an object or array of objects passes to Jira unchanged)."}
		if len(req) > 0 {
			out = append(out, "", "Required:", strings.Join(req, "\n"))
		}
		if len(opt) > 0 {
			out = append(out, "", "Optional:", strings.Join(opt, "\n"))
		}
		return tool.TextResult(strings.Join(out, "\n")), nil
	}

	fields, err := c.fieldList()
	if err != nil {
		return tool.Result{}, err
	}
	var lines []string
	for _, f := range fields {
		if !f.Custom && query == "" {
			continue // built-ins have dedicated args; show them only on an explicit query
		}
		if !matches(f.Name, f.ID) {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s [%s] send %s", f.Name, f.ID, sendHint(f.Schema)))
	}
	if len(lines) == 0 {
		return tool.TextResult("No matching fields."), nil
	}
	return tool.TextResult(fmt.Sprintf("%d custom field(s) on this instance — pass the name or id as a jira_mutate customFields key:\n%s\n\nThis list is instance-wide: it does not say which fields a given screen has, which are required, or what a select field allows. For that, pass project+issueType (create) or issueKey (existing issue).", len(lines), strings.Join(lines, "\n"))), nil
}

func (c *Client) getIssueType(issueKey string) (string, error) {
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

func (c *Client) assertOwnComment(comment *jiraComment) error {
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

func (c *Client) addIssuesToSprintInternal(sprintID int, issueKeys []string) error {
	_, err := c.agile("POST", fmt.Sprintf("/sprint/%d/issue", sprintID), map[string]any{"issues": issueKeys})
	return err
}

func (c *Client) resolveProjectKey(projectKey, repoRoot string) (string, error) {
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
		return "", errors.New("No Jira projects found for your account.")
	}
	keys := map[string]bool{}
	for _, p := range projects {
		keys[p.Key] = true
	}
	if repoRoot != "" {
		branch := gitrepo.CurrentBranch(repoRoot)
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

func (c *Client) resolveTransitionID(issueKey, transitionID, transitionName string) (string, error) {
	if transitionID != "" {
		return transitionID, nil
	}
	requested := strings.TrimSpace(transitionName)
	if requested == "" {
		return "", errors.New("Provide transitionId or transitionName")
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
	oe := arg.String(m, "originalEstimate")
	re := arg.String(m, "remainingEstimate")
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
func (c *Client) componentNames(pk string) []string {
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
func (c *Client) enrichFieldError(err error, pk string) error {
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
// "customfield_10005 (Epic Name)" so the caller knows what it is being told about.
func (c *Client) nameCustomFields(msg string) string {
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

// fieldHelp turns a rejected create/update into instructions: for every field
// the error names, what that screen wants (value shape, allowed values), plus
// the screen's required fields — so the caller can fix the call without a trip
// to the Jira UI. metaFn is only invoked when there is something to explain.
func (c *Client) fieldHelp(err error, metaFn func() (map[string]jiraFieldMeta, error)) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !strings.Contains(msg, "cannot be set") && !strings.Contains(msg, "is required") && !strings.Contains(msg, "not valid") && !customFieldIDRe.MatchString(msg) {
		return err
	}
	meta, mErr := metaFn()
	if mErr != nil || len(meta) == 0 {
		return err
	}
	lower := strings.ToLower(msg)
	var named, required []string
	for id, m := range meta {
		line := fmt.Sprintf("%s [%s]: send %s%s", m.Name, id, sendHint(m.Schema), allowedHint(m.labels()))
		if strings.Contains(msg, id) || (m.Name != "" && strings.Contains(lower, strings.ToLower(m.Name))) {
			named = append(named, line)
		}
		if m.Required {
			required = append(required, m.Name+" ["+id+"]")
		}
	}
	sort.Strings(named)
	sort.Strings(required)
	parts := []string{msg}
	if len(named) > 0 {
		parts = append(parts, "This screen wants: "+strings.Join(named, "; "))
	}
	if len(required) > 0 {
		parts = append(parts, "Required on this screen: "+strings.Join(required, ", "))
	}
	if len(parts) == 1 {
		return err
	}
	parts = append(parts, "Set them via customFields, keyed by name or id. Full screen: jira_search resource=fields.")
	return fmt.Errorf("%s", strings.Join(parts, " | "))
}

// ── Value resolution ────────────────────────────────────────────────────────
// Jira accepts a username, component or version by name and answers a bad one
// with a generic 400 that names neither the field nor the valid values. These
// resolve the caller's strings first, so a typo comes back as "did you mean",
// and nothing is half-written before the error.

func (c *Client) resolveUsername(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	q := url.Values{}
	q.Set("username", value)
	q.Set("maxResults", "10")
	data, err := jiraGet[[]jiraUser](c, "/rest/api/2", "GET", "/user/search?"+q.Encode(), nil)
	if err != nil || data == nil {
		// A lookup that cannot run must not block the write; Jira still validates.
		if err != nil {
			logx.Logf("%s lookup for %q failed, using it as given: %v", field, value, err)
		}
		return value, nil
	}
	var candidates []string
	for _, u := range *data {
		if strings.EqualFold(u.Name, value) {
			return u.Name, nil
		}
		if u.Active {
			candidates = append(candidates, fmt.Sprintf("%s (%s)", u.Name, u.DisplayName))
		}
	}
	if len(candidates) == 0 {
		// No candidates at all can mean the instance restricts user search
		// rather than that the name is wrong — let Jira be the judge.
		return value, nil
	}
	return "", fmt.Errorf("%s %q is not a Jira username. Did you mean: %s? Usernames come from jira_search resource=users.", field, value, strings.Join(candidates, ", "))
}

// checkProjectNames verifies component/version names against what the project
// has. An unknown name is an error listing the real ones; a lookup that returns
// nothing is treated as "cannot tell" and left to Jira.
func checkProjectNames(field, projectKey string, wanted []string, available []string) error {
	if len(wanted) == 0 || len(available) == 0 {
		return nil
	}
	var unknown []string
	for _, w := range wanted {
		found := false
		for _, a := range available {
			if strings.EqualFold(strings.TrimSpace(w), a) {
				found = true
				break
			}
		}
		if !found {
			unknown = append(unknown, w)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("%s %s do not exist in %s. Available: %s.", field, strings.Join(unknown, ", "), projectKey, strings.Join(available, ", "))
}

func (c *Client) versionNames(pk string) []string {
	data, err := jiraGet[[]jiraVersion](c, "/rest/api/2", "GET", "/project/"+url.PathEscape(pk)+"/versions", nil)
	if err != nil || data == nil {
		return nil
	}
	var out []string
	for _, v := range *data {
		out = append(out, v.Name)
	}
	return out
}

// checkIssueValues resolves the user fields and validates component/version
// names for one create or update payload, rewriting the map in place.
func (c *Client) checkIssueValues(pk string, payload map[string]any) error {
	for _, field := range []string{"assignee", "reporter"} {
		if v := arg.String(payload, field); v != "" {
			resolved, err := c.resolveUsername(field, v)
			if err != nil {
				return err
			}
			payload[field] = resolved
		}
	}
	if pk == "" {
		return nil
	}
	if comps := arg.StrSlice(payload, "components"); len(comps) > 0 {
		if err := checkProjectNames("Component(s)", pk, comps, c.componentNames(pk)); err != nil {
			return err
		}
	}
	var versions []string
	for _, key := range []string{"fixVersions", "versions"} {
		versions = append(versions, arg.StrSlice(payload, key)...)
	}
	for _, key := range []string{"fixVersion"} {
		if v := arg.String(payload, key); v != "" {
			versions = append(versions, v)
		}
	}
	if len(versions) > 0 {
		names := c.versionNames(pk)
		if err := checkProjectNames("Version(s)", pk, versions, names); err != nil {
			return fmt.Errorf("%w Create one with jira_mutate version.action=create.", err)
		}
	}
	return nil
}

func (c *Client) createIssueInternal(create map[string]any, repoRoot string) (*jiraCreatedIssue, error) {
	projectKey, err := c.resolveProjectKey(arg.String(create, "projectKey"), repoRoot)
	if err != nil {
		return nil, err
	}
	if err := c.checkIssueValues(projectKey, create); err != nil {
		return nil, err
	}
	fields := map[string]any{
		"project":   map[string]any{"key": projectKey},
		"issuetype": map[string]any{"name": arg.String(create, "issueType")},
		"summary":   arg.String(create, "summary"),
	}
	if d := arg.String(create, "description"); d != "" {
		wiki, _ := markup.MarkdownToJiraWiki(d)
		fields["description"] = markup.AppendAIMarkerWiki(wiki, markup.AuthoredByAI)
	}
	if a := arg.String(create, "assignee"); a != "" {
		fields["assignee"] = map[string]any{"name": a}
	}
	if r := arg.String(create, "reporter"); r != "" {
		fields["reporter"] = map[string]any{"name": r}
	}
	if p := arg.String(create, "priority"); p != "" {
		fields["priority"] = map[string]any{"name": p}
	}
	if dd := arg.String(create, "dueDate"); dd != "" {
		fields["duedate"] = dd
	}
	if labels := arg.StrSlice(create, "labels"); len(labels) > 0 {
		fields["labels"] = labels
	}
	if comps := arg.StrSlice(create, "components"); len(comps) > 0 {
		fields["components"] = namedList(comps)
	}
	if fvs := arg.StrSlice(create, "fixVersions"); len(fvs) > 0 {
		fields["fixVersions"] = namedList(fvs)
	} else if fv := arg.String(create, "fixVersion"); fv != "" {
		fields["fixVersions"] = namedList([]string{fv})
	}
	if vers := arg.StrSlice(create, "versions"); len(vers) > 0 {
		fields["versions"] = namedList(vers)
	}
	if env := arg.String(create, "environment"); env != "" {
		wiki, _ := markup.MarkdownToJiraWiki(env)
		fields["environment"] = wiki
	}
	if tt := buildTimeTracking(create); tt != nil {
		fields["timetracking"] = tt
	}
	epicTarget := strings.TrimSpace(arg.String(create, "epicLink"))
	if parent := arg.String(create, "parent"); parent != "" {
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
			return nil, errors.New("Epic Link custom field not found on this Jira instance. Set it manually in the Jira UI.")
		}
		fields[epicFieldID] = epicTarget
	}
	if cf := arg.Map(create, "customFields"); cf != nil {
		if err := c.applyCustomFields(fields, cf); err != nil {
			return nil, err
		}
	}
	// Epic create screens require Epic Name; default it to the summary so an
	// epic can be created without a UI round trip.
	if strings.EqualFold(strings.TrimSpace(arg.String(create, "issueType")), "epic") {
		if id, err := c.getEpicNameFieldID(); err == nil && id != "" {
			if _, set := fields[id]; !set {
				fields[id] = arg.String(create, "summary")
			}
		}
	}
	created, err := jiraGet[jiraCreatedIssue](c, "/rest/api/2", "POST", "/issue", map[string]any{"fields": fields})
	if err != nil {
		err = c.enrichFieldError(err, projectKey)
		issueType := arg.String(create, "issueType")
		return nil, c.fieldHelp(err, func() (map[string]jiraFieldMeta, error) { return c.createMeta(projectKey, issueType) })
	}
	return created, nil
}

// updateIssueFieldsInternal applies the update.* fields. has* flags signal an
// explicitly-provided key so empty strings/arrays act as clears like the TS.
func (c *Client) updateIssueFieldsInternal(issueKey string, update map[string]any) (bool, error) {
	if err := c.checkIssueValues(projectKeyOf(issueKey), update); err != nil {
		return false, err
	}
	fields := map[string]any{}
	if arg.Has(update, "issueType") {
		fields["issuetype"] = map[string]any{"name": arg.String(update, "issueType")}
	}
	if arg.Has(update, "summary") {
		fields["summary"] = arg.String(update, "summary")
	}
	if arg.Has(update, "description") {
		wiki, _ := markup.MarkdownToJiraWiki(arg.String(update, "description"))
		fields["description"] = markup.AppendAIMarkerWiki(wiki, markup.AuthoredByAI)
	}
	if arg.Has(update, "assignee") {
		if a := arg.String(update, "assignee"); a != "" {
			fields["assignee"] = map[string]any{"name": a}
		} else {
			fields["assignee"] = nil
		}
	}
	if arg.Has(update, "reporter") {
		fields["reporter"] = map[string]any{"name": arg.String(update, "reporter")}
	}
	if arg.Has(update, "priority") {
		fields["priority"] = map[string]any{"name": arg.String(update, "priority")}
	}
	if arg.Has(update, "dueDate") {
		if dd := arg.String(update, "dueDate"); dd != "" {
			fields["duedate"] = dd
		} else {
			fields["duedate"] = nil
		}
	}
	if arg.Has(update, "labels") {
		fields["labels"] = arg.StrSlice(update, "labels")
	}
	if arg.Has(update, "components") {
		fields["components"] = namedList(arg.StrSlice(update, "components"))
	}
	if arg.Has(update, "fixVersions") {
		fields["fixVersions"] = namedList(arg.StrSlice(update, "fixVersions"))
	} else if arg.Has(update, "fixVersion") {
		if fv := arg.String(update, "fixVersion"); fv != "" {
			fields["fixVersions"] = namedList([]string{fv})
		} else {
			fields["fixVersions"] = []any{}
		}
	}
	if arg.Has(update, "versions") {
		fields["versions"] = namedList(arg.StrSlice(update, "versions"))
	}
	if arg.Has(update, "environment") {
		if env := arg.String(update, "environment"); env != "" {
			wiki, _ := markup.MarkdownToJiraWiki(env)
			fields["environment"] = wiki
		} else {
			fields["environment"] = nil
		}
	}
	if tt := buildTimeTracking(update); tt != nil {
		fields["timetracking"] = tt
	}
	if arg.Has(update, "epicLink") {
		epicFieldID, err := c.getEpicLinkFieldID()
		if err != nil {
			return false, err
		}
		if epicFieldID == "" {
			return false, errors.New("Epic Link custom field not found on this Jira instance. Set it manually in the Jira UI.")
		}
		if e := arg.String(update, "epicLink"); e != "" {
			fields[epicFieldID] = e
		} else {
			fields[epicFieldID] = nil
		}
	}
	if cf := arg.Map(update, "customFields"); cf != nil {
		if err := c.applyCustomFields(fields, cf); err != nil {
			return false, err
		}
	}
	if len(fields) == 0 {
		return false, nil
	}
	put := func(payload map[string]any) error {
		if _, err := c.api("PUT", "/issue/"+url.PathEscape(issueKey), map[string]any{"fields": payload}); err != nil {
			err = c.enrichFieldError(err, projectKeyOf(issueKey))
			return c.fieldHelp(err, func() (map[string]jiraFieldMeta, error) { return c.editMeta(issueKey) })
		}
		return nil
	}
	// Jira validates a PUT against the issue's *current* edit screen, so a type
	// change plus a field only the new type has (Epic Name) is rejected as one
	// call. Change the type first, then set the rest against the new screen.
	if it, ok := fields["issuetype"]; ok && len(fields) > 1 {
		delete(fields, "issuetype")
		if err := put(map[string]any{"issuetype": it}); err != nil {
			return false, err
		}
	}
	if err := put(fields); err != nil {
		return false, err
	}
	return true, nil
}

// ── Public tool methods ──────────────────────────────────────────────────────

// Search dispatches the jira_search resource routing from src/index.ts.
func (c *Client) Search(args map[string]any, repoRoot string) (tool.Result, error) {
	resource := arg.String(args, "resource")
	if resource == "" {
		resource = "issues"
	}
	// Board-scoped resources take a boardId; when only a project is known, look
	// the board up rather than making the caller run the two-step by hand.
	if resource == "sprints" || resource == "board_overview" {
		if arg.Int(args, "boardId") <= 0 {
			boardID, err := c.resolveBoardID(arg.String(args, "projectKey"), repoRoot)
			if err != nil {
				return tool.Result{}, err
			}
			args["boardId"] = float64(boardID)
		}
	}
	switch resource {
	case "projects":
		return c.getProjects(args)
	case "issue_types":
		return c.getIssueTypes(arg.String(args, "projectKey"), repoRoot)
	case "boards":
		return c.getBoards(arg.String(args, "projectKey"), arg.IntDefault(args, "maxResults", 25), arg.Int(args, "startAt"))
	case "sprints":
		return c.getSprints(arg.Int(args, "boardId"), arg.String(args, "sprintState"), arg.IntDefault(args, "maxResults", 20), arg.Int(args, "startAt"))
	case "board_overview":
		return c.boardOverview(args)
	case "versions":
		return c.listVersions(arg.String(args, "projectKey"), arg.String(args, "query"), arg.IntPtr(args, "maxResults"), repoRoot)
	case "fields":
		return c.listFields(args, repoRoot)
	case "components":
		return c.listComponents(arg.String(args, "projectKey"), arg.String(args, "query"), repoRoot)
	case "field_history":
		return c.fieldHistory(args, repoRoot)
	case "users":
		return c.searchUsers(arg.String(args, "query"), arg.IntDefault(args, "maxResults", 10))
	case "issues":
		extra, err := c.resolveFields(arg.StrSlice(args, "customFields"))
		if err != nil {
			return tool.Result{}, err
		}
		if arg.Bool(args, "mine") {
			return c.searchIssues("", "assignee = currentUser() ORDER BY updated DESC", "", "", "", "", extra, arg.IntDefault(args, "maxResults", 20), arg.Int(args, "startAt"))
		}
		return c.searchIssues(
			arg.String(args, "query"), arg.String(args, "jql"), arg.String(args, "project"),
			arg.String(args, "status"), arg.String(args, "assignee"), arg.String(args, "issueType"),
			extra, arg.IntDefault(args, "maxResults", 20), arg.Int(args, "startAt"))
	default:
		return tool.Result{}, fmt.Errorf("Unknown resource %q. Use one of: issues, projects, issue_types, boards, sprints, board_overview, field_history, versions, components, fields, users.", resource)
	}
}

func (c *Client) searchIssues(query, jql, project, status, assignee, issueType string, extra []jiraField, maxResults, startAt int) (tool.Result, error) {
	builtJQL, err := buildJQL(query, jql, project, status, assignee, issueType)
	if err != nil {
		return tool.Result{}, err
	}
	q := url.Values{}
	q.Set("jql", builtJQL)
	q.Set("maxResults", strconv.Itoa(maxResults))
	q.Set("startAt", strconv.Itoa(startAt))
	q.Set("fields", issueListFields+fieldIDs(extra))
	data, err := jiraGet[jiraSearchResult](c, "/rest/api/2", "GET", "/search?"+q.Encode(), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil {
		return tool.TextResult("No results."), nil
	}
	var lines []string
	for idx, i := range data.Issues {
		lines = append(lines, fmt.Sprintf("%d. %s", startAt+idx+1, c.issueListLine(i, extra)))
	}
	page := pagination(data.Total, startAt, len(data.Issues))
	return tool.TextResult(fmt.Sprintf("Found %d issues%s:\n%s", data.Total, page, strings.Join(lines, "\n"))), nil
}

type FoundIssue struct {
	Key, Summary, Status, Type string
}

func (c *Client) FindIssues(query string, maxResults int) ([]FoundIssue, error) {
	builtJQL, err := buildJQL(query, "", "", "", "", "")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("jql", builtJQL)
	q.Set("maxResults", strconv.Itoa(maxResults))
	q.Set("startAt", "0")
	q.Set("fields", "summary,status,issuetype")
	data, err := jiraGet[jiraSearchResult](c, "/rest/api/2", "GET", "/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var out []FoundIssue
	if data != nil {
		for _, i := range data.Issues {
			out = append(out, FoundIssue{i.Key, i.Fields.Summary, i.Fields.Status.Name, i.Fields.IssueType.Name})
		}
	}
	return out, nil
}

func (c *Client) getProjects(args map[string]any) (tool.Result, error) {
	limit := arg.IntDefault(args, "maxResults", 50)
	data, err := jiraGet[[]jiraProject](c, "/rest/api/2", "GET", fmt.Sprintf("/project?maxResults=%d", limit), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil || len(*data) == 0 {
		return tool.TextResult("No projects found."), nil
	}
	var lines []string
	for i, p := range *data {
		lines = append(lines, fmt.Sprintf("%d. [%s] %s (%s) | %s", i+1, p.Key, p.Name, p.ProjectTypeKey, c.projectURL(p.Key)))
	}
	return tool.TextResult(fmt.Sprintf("%d project(s):\n%s", len(*data), strings.Join(lines, "\n"))), nil
}

func (c *Client) getIssueTypes(projectKey, repoRoot string) (tool.Result, error) {
	pk, err := c.resolveProjectKey(projectKey, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	data, err := jiraGet[[]jiraIssueTypeStatuses](c, "/rest/api/2", "GET", "/project/"+url.PathEscape(pk)+"/statuses", nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil || len(*data) == 0 {
		return tool.TextResult("No issue types found."), nil
	}
	var lines []string
	for _, t := range *data {
		var statuses []string
		for _, s := range t.Statuses {
			statuses = append(statuses, s.Name)
		}
		lines = append(lines, fmt.Sprintf("%s: %s", t.Name, strings.Join(statuses, ", ")))
	}
	return tool.TextResult(fmt.Sprintf("Issue types and statuses for %s:\n%s", pk, strings.Join(lines, "\n"))), nil
}

func (c *Client) getSprints(boardID int, state string, maxResults, startAt int) (tool.Result, error) {
	q := url.Values{}
	q.Set("maxResults", strconv.Itoa(maxResults))
	q.Set("startAt", strconv.Itoa(startAt))
	if state != "" {
		q.Set("state", state)
	}
	data, err := jiraGet[jiraPageSprints](c, "/rest/agile/1.0", "GET", fmt.Sprintf("/board/%d/sprint?%s", boardID, q.Encode()), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return tool.TextResult(fmt.Sprintf("No sprints found for board %d.", boardID)), nil
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
	return tool.TextResult(fmt.Sprintf("Sprints for board %d%s:\nBoard URL: %s\n%s", boardID, page, c.boardURL(boardID), strings.Join(lines, "\n"))), nil
}

func (c *Client) searchUsers(query string, maxResults int) (tool.Result, error) {
	q := url.Values{}
	q.Set("username", query)
	q.Set("maxResults", strconv.Itoa(maxResults))
	data, err := jiraGet[[]jiraUser](c, "/rest/api/2", "GET", "/user/search?"+q.Encode(), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil || len(*data) == 0 {
		return tool.TextResult("No users found."), nil
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
	return tool.TextResult(fmt.Sprintf("%d user(s) found:\n%s", len(lines), strings.Join(lines, "\n"))), nil
}

func (c *Client) GetIssueFields(issueKey string) (summary, status, issueType string, err error) {
	data, e := jiraGet[jiraIssue](c, "/rest/api/2", "GET", "/issue/"+url.PathEscape(issueKey)+"?fields=summary,status,issuetype", nil)
	if e != nil {
		return "", "", "", e
	}
	if data == nil {
		return "", "", "", fmt.Errorf("Issue %s not found", issueKey)
	}
	return data.Fields.Summary, data.Fields.Status.Name, data.Fields.IssueType.Name, nil
}

type IssueOverviewOpts struct {
	IssueKey           string
	IncludeComments    bool
	CommentsMaxResults int
	CommentsStartAt    int
	IncludeTransitions bool
	IncludeSprint      bool
	DescriptionCap     int // 0 = no cap
	customFields       []string
	changelog          *changeFilter
}

func (c *Client) IssueOverviewOptsFromArgs(args map[string]any) (IssueOverviewOpts, error) {
	full := arg.Bool(args, "fullDescription")
	defaultCap := 2000
	if full {
		defaultCap = 0
	} else if arg.Has(args, "descriptionMaxChars") {
		defaultCap = arg.Int(args, "descriptionMaxChars")
	}
	o := IssueOverviewOpts{
		IssueKey:           arg.String(args, "issueKey"),
		IncludeComments:    arg.BoolDefault(args, "includeComments", true),
		CommentsMaxResults: arg.IntDefault(args, "commentsMaxResults", 10),
		CommentsStartAt:    arg.IntDefault(args, "commentsStartAt", 0),
		IncludeTransitions: arg.BoolDefault(args, "includeTransitions", true),
		IncludeSprint:      arg.BoolDefault(args, "includeSprint", true),
		DescriptionCap:     defaultCap,
		customFields:       arg.StrSlice(args, "customFields"),
	}
	if !arg.Bool(args, "includeChangelog") {
		return o, nil
	}
	f, err := c.changeFilterFromArgs(args)
	if err != nil {
		return IssueOverviewOpts{}, err
	}
	o.changelog = &f
	return o, nil
}

func (c *Client) IssueOverview(o IssueOverviewOpts) (tool.Result, error) {
	baseFields := "summary,description,status,assignee,reporter,priority,duedate,issuetype,labels,components,parent,fixVersions,versions,environment,timetracking,issuelinks,subtasks,attachment"
	epicFieldID, err := c.getEpicLinkFieldID()
	if err != nil {
		return tool.Result{}, err
	}
	extra, err := c.resolveFields(o.customFields)
	if err != nil {
		return tool.Result{}, err
	}
	fields := baseFields + fieldIDs(extra)
	if epicFieldID != "" {
		fields += "," + epicFieldID
	}
	issue, err := jiraGet[jiraIssue](c, "/rest/api/2", "GET", "/issue/"+url.PathEscape(o.IssueKey)+"?fields="+fields, nil)
	if err != nil {
		return tool.Result{}, err
	}
	if issue == nil {
		return tool.TextResult("Issue not found."), nil
	}
	f := issue.Fields

	epicLinkKey := ""
	if epicFieldID != "" {
		_ = json.Unmarshal(f.raw[epicFieldID], &epicLinkKey)
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
	for _, x := range extra {
		lines = append(lines, fmt.Sprintf("%-11s %s", x.Name+":", f.value(x)))
	}

	if o.IncludeSprint {
		agileIssue, aerr := jiraGet[struct {
			Fields struct {
				Sprint        *jiraSprint  `json:"sprint"`
				ClosedSprints []jiraSprint `json:"closedSprints"`
			} `json:"fields"`
		}](c, "/rest/agile/1.0", "GET", "/issue/"+url.PathEscape(o.IssueKey)+"?fields=sprint,closedSprints", nil)
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

	if o.IncludeTransitions {
		transitions, _ := jiraGet[jiraTransitionsResult](c, "/rest/api/2", "GET", "/issue/"+url.PathEscape(o.IssueKey)+"/transitions", nil)
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
		desc = capText(f.Description, o.DescriptionCap)
	}
	lines = append(lines, "", "Description:", desc)

	if len(f.Attachment) > 0 {
		lines = append(lines, "", fmt.Sprintf("Attachments: %d", len(f.Attachment)))
		for _, att := range f.Attachment {
			mt := att.MimeType
			if mt == "" {
				mt = "application/octet-stream"
			}
			lines = append(lines, fmt.Sprintf("  #%s %s (%s, %s)", att.ID, att.Filename, mt, attachment.FormatBytes(att.Size)))
		}
		lines = append(lines, "Use get_attachment source=jira with attachmentId to view contents.")
	}

	if o.changelog != nil {
		history, err := c.issueChangelogLines(o.IssueKey, *o.changelog)
		if err != nil {
			return tool.Result{}, fmt.Errorf("reading the changelog of %s: %w", o.IssueKey, err)
		}
		lines = append(lines, history...)
	}

	if o.IncludeComments {
		comments, _ := jiraGet[jiraCommentResult](c, "/rest/api/2", "GET",
			fmt.Sprintf("/issue/%s/comment?startAt=%d&maxResults=%d", url.PathEscape(o.IssueKey), o.CommentsStartAt, o.CommentsMaxResults), nil)
		var items []jiraComment
		total := 0
		page := ""
		if comments != nil {
			items = comments.Comments
			total = comments.Total
			page = pagination(total, o.CommentsStartAt, len(items))
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

	return tool.TextResult(strings.TrimRight(strings.Join(lines, "\n"), "\n \t")), nil
}

func (c *Client) boardIssueFields(boardID int, requested []string) ([]jiraField, error) {
	cfg, err := jiraGet[jiraBoardConfig](c, "/rest/agile/1.0", "GET", fmt.Sprintf("/board/%d/configuration", boardID), nil)
	if err != nil {
		return nil, err
	}
	var names []string
	if cfg != nil && cfg.Estimation.Type == "field" && strings.HasPrefix(cfg.Estimation.Field.FieldID, "customfield_") {
		names = append(names, cfg.Estimation.Field.FieldID)
	}
	return c.resolveFields(append(names, requested...))
}

func (c *Client) boardOverview(args map[string]any) (tool.Result, error) {
	boardID := arg.Int(args, "boardId")
	includeIssues := arg.BoolDefault(args, "includeIssues", true)
	sprintMaxResults := arg.IntDefault(args, "maxResults", 10)
	sprintStartAt := arg.Int(args, "startAt")
	sprintState := arg.String(args, "sprintState")
	assignee := arg.String(args, "assignee")
	status := arg.String(args, "status")
	const issueMaxResults, issueStartAt = 25, 0

	board, _ := jiraGet[jiraBoard](c, "/rest/agile/1.0", "GET", fmt.Sprintf("/board/%d", boardID), nil)
	sq := url.Values{}
	sq.Set("maxResults", strconv.Itoa(sprintMaxResults))
	sq.Set("startAt", strconv.Itoa(sprintStartAt))
	if sprintState != "" {
		sq.Set("state", sprintState)
	} else {
		sq.Set("state", "active,future")
	}
	sprints, err := jiraGet[jiraPageSprints](c, "/rest/agile/1.0", "GET", fmt.Sprintf("/board/%d/sprint?%s", boardID, sq.Encode()), nil)
	if err != nil {
		return tool.Result{}, err
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
		return tool.TextResult(fmt.Sprintf("Board %d%s: no matching sprints.", boardID, suffix)), nil
	}

	var filterClauses []string
	if assignee != "" {
		filterClauses = append(filterClauses, "assignee = "+jsonStr(assignee))
	}
	if status != "" {
		filterClauses = append(filterClauses, "status = "+jsonStr(status))
	}
	issueJql := strings.Join(filterClauses, " AND ")

	issueBySprint := map[int]*jiraSearchResult{}
	var extra []jiraField
	if includeIssues {
		extra, err = c.boardIssueFields(boardID, arg.StrSlice(args, "customFields"))
		if err != nil {
			return tool.Result{}, err
		}
		for _, sprint := range sprints.Values {
			q := url.Values{}
			q.Set("maxResults", strconv.Itoa(issueMaxResults))
			q.Set("startAt", strconv.Itoa(issueStartAt))
			q.Set("fields", issueListFields+fieldIDs(extra))
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
				lines = append(lines, "   - "+c.issueListLine(issue, extra))
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
	return tool.TextResult(strings.TrimRight(strings.Join(lines, "\n"), "\n \t")), nil
}

// deleteIssue removes an issue for good. Jira takes its comments, attachments,
// worklogs and links with it and there is no undo, so the issue is read first:
// the result then says what was destroyed rather than just echoing a key.
func (c *Client) deleteIssue(issueKey string, deleteSubtasks bool) (tool.Result, error) {
	summary, _, issueType, err := c.GetIssueFields(issueKey)
	if err != nil {
		return tool.Result{}, err
	}
	path := "/issue/" + url.PathEscape(issueKey)
	if deleteSubtasks {
		path += "?deleteSubtasks=true"
	}
	if _, err := c.api("DELETE", path, nil); err != nil {
		// Jira refuses to delete a parent unless the sub-tasks go too, and says
		// so in prose the caller should not have to parse.
		if strings.Contains(strings.ToLower(err.Error()), "subtask") {
			return tool.Result{}, fmt.Errorf("%w Pass deleteSubtasks: true to delete %s together with its sub-tasks.", err, issueKey)
		}
		return tool.Result{}, err
	}
	label := strings.TrimSpace(issueType + " " + summary)
	if label != "" {
		label = " (" + label + ")"
	}
	return tool.TextResult(fmt.Sprintf("Deleted %s%s. This cannot be undone.", issueKey, label)), nil
}

var issueMutationKeys = []string{"create", "update", "sprintId", "removeFromSprint", "transitionId", "transitionName", "comment", "commentAction", "commentId", "commentUserAuthored", "attachments", "link", "worklog"}

// MutateIssue handles jira_mutate: create/update/delete/sprint/transition/comment/link/worklog.
func (c *Client) MutateIssue(args map[string]any, repoRoot string) (tool.Result, error) {
	issueKey := strings.TrimSpace(arg.String(args, "issueKey"))
	var actions []string
	var extras []string

	// Fix versions are project-scoped, not issue-scoped: handled before the
	// issueKey requirement and on their own.
	if version := arg.Map(args, "version"); version != nil {
		// Version work and issue work in one call would mean half the arguments
		// silently doing nothing, so they are kept apart.
		for _, k := range append([]string{"delete"}, issueMutationKeys...) {
			if arg.Has(args, k) {
				return tool.Result{}, fmt.Errorf("version cannot be combined with %s — call jira_mutate twice: once for the version, once for the issue.", k)
			}
		}
		if _, ok := version["projectKey"].(string); !ok {
			if pk := arg.String(args, "projectKey"); pk != "" {
				version["projectKey"] = pk
			} else if issueKey != "" {
				version["projectKey"] = projectKeyOf(issueKey)
			}
		}
		return c.mutateVersion(version, repoRoot)
	}

	// Deletion is decided before anything is written: combined with create it
	// would otherwise make the issue and then refuse the call.
	if arg.Bool(args, "delete") {
		for _, k := range issueMutationKeys {
			if arg.Has(args, k) {
				return tool.Result{}, fmt.Errorf("delete cannot be combined with %s — the issue is gone, so the rest of the call would have nothing to act on.", k)
			}
		}
		if issueKey == "" {
			return tool.Result{}, errors.New("delete needs the issueKey of the issue to remove.")
		}
		return c.deleteIssue(issueKey, arg.Bool(args, "deleteSubtasks"))
	}

	if arg.Bool(args, "commentUserAuthored") && (arg.String(args, "commentAction") == "delete" || strings.TrimSpace(arg.String(args, "comment")) == "") {
		return tool.Result{}, errors.New("commentUserAuthored only applies to a comment add or update with comment text.")
	}

	if create := arg.Map(args, "create"); create != nil {
		created, err := c.createIssueInternal(create, repoRoot)
		if err != nil {
			return tool.Result{}, err
		}
		if created == nil {
			return tool.Result{}, errors.New("Issue creation did not return an issue key.")
		}
		issueKey = created.Key
		actions = append(actions, "created issue")
	}

	if issueKey == "" {
		return tool.Result{}, errors.New("Provide issueKey, or provide create with issueType and summary.")
	}

	// Every step below runs against an issue that may have just been created.
	// Returning a bare error there loses the key, and the caller — seeing only a
	// failure — creates the issue a second time. fail keeps the key, the URL and
	// what already went through in the error, so only the failed step is retried.
	fail := func(step string, err error) error {
		if len(actions) == 0 {
			return err
		}
		return fmt.Errorf("%s: %s. Then %s failed: %w\n%s\nThe issue exists — retry only the failed step against %s instead of creating it again.",
			issueKey, strings.Join(actions, ", "), step, err, c.issueURL(issueKey), issueKey)
	}

	if update := arg.Map(args, "update"); update != nil {
		updated, err := c.updateIssueFieldsInternal(issueKey, update)
		if err != nil {
			return tool.Result{}, fail("the field update", err)
		}
		if updated {
			actions = append(actions, "updated fields")
		}
	}

	if arg.Has(args, "sprintId") {
		sprintID := arg.Int(args, "sprintId")
		if err := c.addIssuesToSprintInternal(sprintID, []string{issueKey}); err != nil {
			return tool.Result{}, fail(fmt.Sprintf("adding it to sprint %d", sprintID), err)
		}
		actions = append(actions, fmt.Sprintf("added to sprint %d", sprintID))
	}

	if arg.Bool(args, "removeFromSprint") {
		if _, err := c.agile("POST", "/backlog/issue", map[string]any{"issues": []string{issueKey}}); err != nil {
			return tool.Result{}, fail("the move to the backlog", err)
		}
		actions = append(actions, "moved to backlog")
	}

	if arg.String(args, "transitionId") != "" || arg.String(args, "transitionName") != "" {
		transitionID, err := c.resolveTransitionID(issueKey, arg.String(args, "transitionId"), arg.String(args, "transitionName"))
		if err != nil {
			return tool.Result{}, fail("the transition", err)
		}
		if _, err := c.api("POST", "/issue/"+url.PathEscape(issueKey)+"/transitions", map[string]any{"transition": map[string]any{"id": transitionID}}); err != nil {
			return tool.Result{}, fail("the transition", err)
		}
		actions = append(actions, "transitioned via "+transitionID)
	}

	if arg.Has(args, "comment") || arg.Has(args, "commentId") || arg.Bool(args, "commentUserAuthored") || arg.String(args, "commentAction") != "" {
		action := arg.String(args, "commentAction")
		if action == "" {
			action = "add"
		}
		res, err := c.comment(map[string]any{
			"issueKey":     issueKey,
			"action":       action,
			"commentId":    arg.String(args, "commentId"),
			"body":         arg.String(args, "comment"),
			"userAuthored": arg.Bool(args, "commentUserAuthored"),
		})
		if err != nil {
			return tool.Result{}, fail("the comment", err)
		}
		if len(res.Content) > 0 && res.Content[0].Text != "" {
			extras = append(extras, res.Content[0].Text)
		}
		actions = append(actions, map[string]string{"add": "added comment", "update": "updated comment", "delete": "deleted comment"}[action])
	}

	if paths := arg.StrSlice(args, "attachments"); len(paths) > 0 {
		names, err := c.UploadAttachments(issueKey, paths)
		if err != nil {
			return tool.Result{}, fail("the attachment upload", err)
		}
		actions = append(actions, "attached "+strings.Join(names, ", "))
	}

	var warnings []string
	if link := arg.Map(args, "link"); link != nil {
		enabled, err := c.getIssueLinkingEnabled()
		if err != nil {
			return tool.Result{}, fail("the issue link", err)
		}
		if !enabled {
			warnings = append(warnings, "issue linking is disabled in this Jira instance — add the link manually")
		} else {
			linkType, reversed, err := c.resolveLinkType(arg.String(link, "linkType"))
			if err != nil {
				return tool.Result{}, fail("the issue link", err)
			}
			dir := arg.String(link, "direction")
			if dir == "" {
				// An explicit direction wins; otherwise the inward phrase sets it.
				dir = "outward"
				if reversed {
					dir = "inward"
				}
			}
			target := arg.String(link, "targetIssueKey")
			outward, inward := issueKey, target
			if dir != "outward" {
				outward, inward = target, issueKey
			}
			if _, err := c.api("POST", "/issueLink", map[string]any{
				"type":         map[string]any{"name": linkType},
				"outwardIssue": map[string]any{"key": outward},
				"inwardIssue":  map[string]any{"key": inward},
			}); err != nil {
				return tool.Result{}, fail("the issue link", err)
			}
			actions = append(actions, fmt.Sprintf("linked %s → %s", linkType, target))
		}
	}

	if worklog := arg.Map(args, "worklog"); worklog != nil {
		wBody := map[string]any{"timeSpent": arg.String(worklog, "timeSpent")}
		if cmt := arg.String(worklog, "comment"); cmt != "" {
			wiki, _ := markup.MarkdownToJiraWiki(cmt)
			wBody["comment"] = markup.AppendAIMarkerWiki(wiki, markup.AuthoredByAI)
		}
		if started := arg.String(worklog, "started"); started != "" {
			wBody["started"] = started
		}
		if _, err := c.api("POST", "/issue/"+url.PathEscape(issueKey)+"/worklog", wBody); err != nil {
			return tool.Result{}, fail("the worklog", err)
		}
		actions = append(actions, "logged "+arg.String(worklog, "timeSpent"))
	}

	if len(actions) == 0 {
		return tool.TextResult("Nothing to mutate."), nil
	}
	parts := []string{fmt.Sprintf("Mutated %s: %s.", issueKey, strings.Join(actions, ", ")), c.issueURL(issueKey)}
	parts = append(parts, extras...)
	if len(warnings) > 0 {
		parts = append(parts, "Warnings: "+strings.Join(warnings, "; "))
	}
	return tool.TextResult(strings.Join(parts, "\n")), nil
}

// comment dispatches the jira_mutate comment actions (add/update/delete).
func (c *Client) comment(args map[string]any) (tool.Result, error) {
	action := arg.String(args, "action")
	if action == "" {
		action = "add"
	}
	issueKey := arg.String(args, "issueKey")
	body := arg.String(args, "body")
	by := markup.ParseAuthorship(args, "userAuthored")
	switch action {
	case "update":
		return c.editComment(issueKey, arg.String(args, "commentId"), body, by)
	case "delete":
		return c.deleteComment(issueKey, arg.String(args, "commentId"))
	case "add":
		return c.addComment(issueKey, body, arg.StrSlice(args, "attachments"), by)
	default:
		return tool.Result{}, fmt.Errorf("Unknown comment action %q. Use add, update, or delete.", action)
	}
}

func (c *Client) addComment(issueKey, body string, attachments []string, by markup.Authorship) (tool.Result, error) {
	var actions []string
	// Body is optional when only attaching files; required otherwise.
	if strings.TrimSpace(body) != "" || len(attachments) == 0 {
		v, converted, err := validateCommentBody(body)
		if err != nil {
			return tool.Result{}, err
		}
		v = markup.AppendAIMarkerWiki(v, by)
		if _, err := c.api("POST", "/issue/"+url.PathEscape(issueKey)+"/comment", map[string]any{"body": v}); err != nil {
			return tool.Result{}, err
		}
		actions = append(actions, "comment added"+wikiNote(converted))
	}
	if len(attachments) > 0 {
		names, err := c.UploadAttachments(issueKey, attachments)
		if err != nil {
			return tool.Result{}, err
		}
		actions = append(actions, "attached "+strings.Join(names, ", "))
	}
	return tool.TextResult(fmt.Sprintf("%s on %s.", strings.Join(actions, ", "), issueKey)), nil
}

func (c *Client) editComment(issueKey, commentID, body string, by markup.Authorship) (tool.Result, error) {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" || commentID == "undefined" || commentID == "null" {
		return tool.Result{}, errors.New("commentId is required.")
	}
	path := "/issue/" + url.PathEscape(issueKey) + "/comment/" + url.PathEscape(commentID)
	current, err := jiraGet[jiraComment](c, "/rest/api/2", "GET", path, nil)
	if err != nil {
		return tool.Result{}, err
	}
	if current == nil {
		return tool.Result{}, fmt.Errorf("Comment %s not found on %s.", commentID, issueKey)
	}
	if err := c.assertOwnComment(current); err != nil {
		return tool.Result{}, err
	}
	v, converted, err := validateCommentBody(body)
	if err != nil {
		return tool.Result{}, err
	}
	v = markup.AppendAIMarkerWiki(v, by)
	if _, err := c.api("PUT", path, map[string]any{"body": v}); err != nil {
		return tool.Result{}, err
	}
	return tool.TextResult(fmt.Sprintf("Comment %s updated on %s%s.", commentID, issueKey, wikiNote(converted))), nil
}

func (c *Client) deleteComment(issueKey, commentID string) (tool.Result, error) {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" || commentID == "undefined" || commentID == "null" {
		return tool.Result{}, errors.New("commentId is required.")
	}
	path := "/issue/" + url.PathEscape(issueKey) + "/comment/" + url.PathEscape(commentID)
	current, err := jiraGet[jiraComment](c, "/rest/api/2", "GET", path, nil)
	if err != nil {
		return tool.Result{}, err
	}
	if current == nil {
		return tool.Result{}, fmt.Errorf("Comment %s not found on %s.", commentID, issueKey)
	}
	if err := c.assertOwnComment(current); err != nil {
		return tool.Result{}, err
	}
	if _, err := c.api("DELETE", path, nil); err != nil {
		return tool.Result{}, err
	}
	return tool.TextResult(fmt.Sprintf("Comment %s deleted from %s.", commentID, issueKey)), nil
}

// resolveBoardID finds the board to use when the caller gave a project instead
// of a boardId. One board for the project is unambiguous; several is a question
// only the caller can answer, so the error lists them with their ids.
func (c *Client) resolveBoardID(projectKey, repoRoot string) (int, error) {
	pk, err := c.resolveProjectKey(projectKey, repoRoot)
	if err != nil {
		return 0, errors.New("boardId is required, or pass project so the board can be resolved. Find boards with jira_search resource=boards.")
	}
	q := url.Values{}
	q.Set("maxResults", "50")
	q.Set("projectKeyOrId", pk)
	data, err := jiraGet[jiraPageBoards](c, "/rest/agile/1.0", "GET", "/board?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	if data == nil || len(data.Values) == 0 {
		return 0, fmt.Errorf("No boards found for project %s. Pass boardId explicitly.", pk)
	}
	if len(data.Values) == 1 {
		return data.Values[0].ID, nil
	}
	var options []string
	for _, b := range data.Values {
		options = append(options, fmt.Sprintf("%d (%s)", b.ID, b.Name))
	}
	return 0, fmt.Errorf("Project %s has %d boards — pass boardId: %s.", pk, len(data.Values), strings.Join(options, ", "))
}

func (c *Client) getBoards(projectKey string, maxResults, startAt int) (tool.Result, error) {
	q := url.Values{}
	q.Set("maxResults", strconv.Itoa(maxResults))
	q.Set("startAt", strconv.Itoa(startAt))
	if projectKey != "" {
		q.Set("projectKeyOrId", projectKey)
	}
	data, err := jiraGet[jiraPageBoards](c, "/rest/agile/1.0", "GET", "/board?"+q.Encode(), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return tool.TextResult("No boards found."), nil
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
	return tool.TextResult(fmt.Sprintf("%d board(s)%s:\n%s", len(data.Values), page, strings.Join(lines, "\n"))), nil
}

func (c *Client) listComponents(projectKey, query string, repoRoot string) (tool.Result, error) {
	pk, err := c.resolveProjectKey(projectKey, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	data, err := jiraGet[[]struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}](c, "/rest/api/2", "GET", "/project/"+url.PathEscape(pk)+"/components", nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil || len(*data) == 0 {
		return tool.TextResult(fmt.Sprintf("No components in %s.", pk)), nil
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
		return tool.TextResult(fmt.Sprintf("No component matching %q in %s.", query, pk)), nil
	}
	return tool.TextResult(fmt.Sprintf("Components in %s:\n%s", pk, strings.Join(lines, "\n"))), nil
}

func (c *Client) listVersions(projectKey, query string, maxResults *int, repoRoot string) (tool.Result, error) {
	pk, err := c.resolveProjectKey(projectKey, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	data, err := jiraGet[[]jiraVersion](c, "/rest/api/2", "GET", "/project/"+url.PathEscape(pk)+"/versions", nil)
	if err != nil {
		return tool.Result{}, err
	}
	query = strings.TrimSpace(query)
	createHint := func(name string) string {
		return fmt.Sprintf(`Create it with: jira_mutate version.action=create version.projectKey=%s version.name=%q`, pk, name)
	}
	if data == nil || len(*data) == 0 {
		if query != "" {
			return tool.TextResult(fmt.Sprintf("No versions in %s. %s", pk, createHint(query))), nil
		}
		return tool.TextResult(fmt.Sprintf("No versions in %s.", pk)), nil
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
		return tool.TextResult(fmt.Sprintf("No version matching %q in %s (%d other version(s) exist). %s", query, pk, len(versions), createHint(query))), nil
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
	return tool.TextResult(fmt.Sprintf("%s\n%s%s", header, strings.Join(lines, "\n"), more)), nil
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

func (c *Client) mutateVersion(args map[string]any, repoRoot string) (tool.Result, error) {
	action := arg.String(args, "action")
	if action == "" {
		action = "create"
	}
	switch action {
	case "create", "update", "release", "archive", "delete":
	default:
		return tool.Result{}, fmt.Errorf("Unknown version action %q. Use create, update, release, archive, or delete.", action)
	}
	if action == "create" {
		pk, err := c.resolveProjectKey(arg.String(args, "projectKey"), repoRoot)
		if err != nil {
			return tool.Result{}, err
		}
		name := strings.TrimSpace(arg.String(args, "name"))
		if name == "" {
			return tool.Result{}, errors.New("name is required to create a version.")
		}
		body := map[string]any{"project": pk, "name": name}
		if arg.Has(args, "description") {
			body["description"] = arg.String(args, "description")
		}
		if d := arg.String(args, "releaseDate"); d != "" {
			body["releaseDate"] = d
		}
		if d := arg.String(args, "startDate"); d != "" {
			body["startDate"] = d
		}
		if arg.Has(args, "released") {
			body["released"] = arg.Bool(args, "released")
		}
		if arg.Has(args, "archived") {
			body["archived"] = arg.Bool(args, "archived")
		}
		created, err := jiraGet[jiraVersion](c, "/rest/api/2", "POST", "/version", body)
		if err != nil {
			return tool.Result{}, err
		}
		if created == nil {
			return tool.Result{}, errors.New("Jira returned no body when creating version.")
		}
		return tool.TextResult(fmt.Sprintf("Created version [%s] %s in %s.", created.ID, created.Name, pk)), nil
	}

	id := strings.TrimSpace(arg.String(args, "id"))
	if id == "" {
		return tool.Result{}, fmt.Errorf("version id is required for action=%s.", action)
	}
	if action == "delete" {
		if _, err := c.api("DELETE", "/version/"+url.PathEscape(id), nil); err != nil {
			return tool.Result{}, err
		}
		return tool.TextResult("Deleted version " + id + "."), nil
	}

	body := map[string]any{}
	if arg.Has(args, "name") {
		body["name"] = arg.String(args, "name")
	}
	if arg.Has(args, "description") {
		body["description"] = arg.String(args, "description")
	}
	if arg.Has(args, "startDate") {
		body["startDate"] = arg.String(args, "startDate")
	}
	if arg.Has(args, "releaseDate") {
		body["releaseDate"] = arg.String(args, "releaseDate")
	}
	if arg.Has(args, "released") {
		body["released"] = arg.Bool(args, "released")
	}
	if arg.Has(args, "archived") {
		body["archived"] = arg.Bool(args, "archived")
	}
	switch action {
	case "release":
		body["released"] = true
		if _, ok := body["releaseDate"]; !ok {
			body["releaseDate"] = time.Now().UTC().Format("2006-01-02")
		}
	case "archive":
		body["archived"] = true
	}
	if len(body) == 0 {
		return tool.Result{}, errors.New("Nothing to update.")
	}
	updated, err := jiraGet[jiraVersion](c, "/rest/api/2", "PUT", "/version/"+url.PathEscape(id), body)
	if err != nil {
		return tool.Result{}, err
	}
	label := id
	if updated != nil {
		label = fmt.Sprintf("[%s] %s", updated.ID, updated.Name)
	}
	switch action {
	case "release":
		return tool.TextResult(fmt.Sprintf("Released version %s on %s.", label, body["releaseDate"])), nil
	case "archive":
		return tool.TextResult("Archived version " + label + "."), nil
	}
	return tool.TextResult("Updated version " + label + "."), nil
}

// UploadAttachments POSTs one or more attachment sources to an issue's
// attachments endpoint as multipart/form-data. Jira requires the
// X-Atlassian-Token header and the "file" form field; multiple files ride one
// request. Sources are resolved (local path, URL, data: URI) first. Returns the
// uploaded filenames.
func (c *Client) UploadAttachments(issueKey string, sources []string) ([]string, error) {
	resolved, err := attachment.ResolveSources(sources, c.creds)
	if err != nil {
		return nil, err
	}
	defer attachment.Release(resolved)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	names := make([]string, 0, len(resolved))
	for _, r := range resolved {
		if err := func() error {
			f, err := os.Open(r.Path)
			if err != nil {
				return fmt.Errorf("cannot open attachment %s: %w", r.Label(), err)
			}
			defer func() { _ = f.Close() }()
			part, err := w.CreateFormFile("file", r.Name)
			if err != nil {
				return err
			}
			_, err = io.Copy(part, f)
			return err
		}(); err != nil {
			return nil, err
		}
		names = append(names, r.Name)
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	reqURL := c.baseURL + "/rest/api/2/issue/" + url.PathEscape(issueKey) + "/attachments"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Atlassian-Token", "no-check") // Jira XSRF guard for multipart uploads
	res, err := httpx.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", formatJiraError(res.StatusCode, "POST", "/issue/"+issueKey+"/attachments", parseJiraErrorDetails(string(raw))))
	}
	return names, nil
}

// GetAttachment fetches metadata then streams or buffers the content.
func (c *Client) GetAttachment(args map[string]any) (tool.Result, error) {
	id := strings.TrimSpace(arg.String(args, "attachmentId"))
	if id == "" {
		return tool.Result{}, errors.New("attachmentId is required.")
	}
	meta, err := jiraGet[jiraAttachment](c, "/rest/api/2", "GET", "/attachment/"+url.PathEscape(id), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if meta == nil {
		return tool.Result{}, fmt.Errorf("Attachment %s not found.", id)
	}
	mimeType := meta.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	// saveTo: stream straight to disk.
	if saveTo := arg.String(args, "saveTo"); saveTo != "" {
		path, _ := filepath.Abs(saveTo)
		res, err := c.fetchContent(meta.Content, 300*time.Second)
		if err != nil {
			return tool.Result{}, err
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			raw, _ := io.ReadAll(res.Body)
			return tool.Result{}, fmt.Errorf("%s", formatJiraError(res.StatusCode, "GET", meta.Content, parseJiraErrorDetails(string(raw))))
		}
		f, err := os.Create(path)
		if err != nil {
			return tool.Result{}, err
		}
		_, copyErr := io.Copy(f, res.Body)
		closeErr := f.Close()
		if copyErr != nil {
			return tool.Result{}, copyErr
		}
		if closeErr != nil {
			return tool.Result{}, closeErr
		}
		sizeLabel := "unknown size"
		if meta.Size > 0 {
			sizeLabel = attachment.FormatBytes(meta.Size)
		}
		return tool.TextResult(fmt.Sprintf("Saved attachment #%s (%s — %s, %s) to %s", id, meta.Filename, mimeType, sizeLabel, path)), nil
	}

	if meta.Size > 0 && meta.Size > media.MaxVideoSourceBytes {
		return tool.Result{}, fmt.Errorf("Attachment #%s is %s, exceeds the %s inline cap. Pass saveTo=/absolute/path to stream it to disk.", id, attachment.FormatBytes(meta.Size), attachment.FormatBytes(media.MaxVideoSourceBytes))
	}
	res, err := c.fetchContent(meta.Content, 60*time.Second)
	if err != nil {
		return tool.Result{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(res.Body)
		return tool.Result{}, fmt.Errorf("%s", formatJiraError(res.StatusCode, "GET", meta.Content, parseJiraErrorDetails(string(raw))))
	}
	if cl := res.Header.Get("Content-Length"); cl != "" {
		var declared int64
		_, _ = fmt.Sscanf(cl, "%d", &declared)
		if declared > media.MaxVideoSourceBytes {
			return tool.Result{}, fmt.Errorf("Attachment #%s is %s, exceeds the %s inline cap. Pass saveTo=/absolute/path to stream it to disk.", id, attachment.FormatBytes(declared), attachment.FormatBytes(media.MaxVideoSourceBytes))
		}
	}
	buffer, err := io.ReadAll(io.LimitReader(res.Body, media.MaxVideoSourceBytes+1))
	if err != nil {
		return tool.Result{}, err
	}
	if int64(len(buffer)) > media.MaxVideoSourceBytes {
		return tool.Result{}, fmt.Errorf("Attachment #%s downloaded %s, exceeds the %s inline cap. Pass saveTo=/absolute/path to stream it to disk.", id, attachment.FormatBytes(int64(len(buffer))), attachment.FormatBytes(media.MaxVideoSourceBytes))
	}

	return attachment.BuildResult(attachment.Args{
		ID:             id,
		Filename:       meta.Filename,
		MimeType:       mimeType,
		Buffer:         buffer,
		MaxDimension:   arg.IntPtr(args, "maxDimension"),
		Quality:        arg.IntPtr(args, "quality"),
		Frames:         arg.IntPtr(args, "frames"),
		Start:          arg.FloatPtr(args, "start"),
		End:            arg.FloatPtr(args, "end"),
		Mode:           arg.String(args, "mode"),
		SceneThreshold: arg.FloatPtr(args, "sceneThreshold"),
	})
}

// fetchContent GETs an absolute attachment-content URL with just the auth header.
func (c *Client) fetchContent(contentURL string, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, contentURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := httpx.Client.Do(req)
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
	names := make([]string, 0, len(items))
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
