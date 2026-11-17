package bitbucket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stubbedev/atlassian-mcp/internal/arg"
	"github.com/stubbedev/atlassian-mcp/internal/attachment"
	"github.com/stubbedev/atlassian-mcp/internal/config"
	"github.com/stubbedev/atlassian-mcp/internal/gitrepo"
	"github.com/stubbedev/atlassian-mcp/internal/httpx"
	"github.com/stubbedev/atlassian-mcp/internal/jira"
	"github.com/stubbedev/atlassian-mcp/internal/logx"
	"github.com/stubbedev/atlassian-mcp/internal/markup"
	"github.com/stubbedev/atlassian-mcp/internal/media"
	"github.com/stubbedev/atlassian-mcp/internal/text"
	"github.com/stubbedev/atlassian-mcp/internal/tool"
)

var attachmentRefRe = regexp.MustCompile(`!?\[([^\]]*)\]\(attachment:(\d+)\)`)

// ── Wire types ───────────────────────────────────────────────────────────────

type bbUserRef struct {
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
}

type bbRef struct {
	DisplayID    string `json:"displayId"`
	LatestCommit string `json:"latestCommit"`
	Repository   struct {
		Slug    string `json:"slug"`
		Project struct {
			Key string `json:"key"`
		} `json:"project"`
	} `json:"repository"`
}

type bbReviewer struct {
	User     bbUserRef `json:"user"`
	Approved bool      `json:"approved"`
}

type PullRequest struct {
	ID          int    `json:"id"`
	Version     int    `json:"version"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	Author      struct {
		User bbUserRef `json:"user"`
	} `json:"author"`
	FromRef   bbRef        `json:"fromRef"`
	ToRef     bbRef        `json:"toRef"`
	Reviewers []bbReviewer `json:"reviewers"`
	Links     *struct {
		Self []struct {
			Href string `json:"href"`
		} `json:"self"`
	} `json:"links"`
}

type bbComment struct {
	ID             int         `json:"id"`
	Version        int         `json:"version"`
	Text           string      `json:"text"`
	Deleted        bool        `json:"deleted"`
	ThreadResolved *bool       `json:"threadResolved"`
	State          string      `json:"state"`
	Severity       string      `json:"severity"`
	Author         *bbUserRef  `json:"author"`
	CreatedDate    int64       `json:"createdDate"`
	UpdatedDate    int64       `json:"updatedDate"`
	Comments       []bbComment `json:"comments"`
}

type bbActivity struct {
	Action  string     `json:"action"`
	Comment *bbComment `json:"comment"`
}

type bbBranch struct {
	ID           string `json:"id"`
	DisplayID    string `json:"displayId"`
	LatestCommit string `json:"latestCommit"`
	IsDefault    bool   `json:"isDefault"`
}

type bbDiffSegment struct {
	Type  string `json:"type"`
	Lines []struct {
		Line        string `json:"line"`
		Source      int    `json:"source"`
		Destination int    `json:"destination"`
	} `json:"lines"`
}

type bbDiffHunk struct {
	SourceLine      int             `json:"sourceLine"`
	SourceSpan      int             `json:"sourceSpan"`
	DestinationLine int             `json:"destinationLine"`
	DestinationSpan int             `json:"destinationSpan"`
	Segments        []bbDiffSegment `json:"segments"`
}

type bbDiff struct {
	FromHash string `json:"fromHash"`
	ToHash   string `json:"toHash"`
	Diffs    []struct {
		Source *struct {
			ToString string `json:"toString"`
		} `json:"source"`
		Destination *struct {
			ToString string `json:"toString"`
		} `json:"destination"`
		Hunks []bbDiffHunk `json:"hunks"`
	} `json:"diffs"`
}

type bbParticipant struct {
	User struct {
		DisplayName string `json:"displayName"`
	} `json:"user"`
	Approved bool   `json:"approved"`
	Status   string `json:"status"`
}

type bbBuildStatus struct {
	State       string `json:"state"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description"`
	DateAdded   int64  `json:"dateAdded"`
}

type bbTask struct {
	ID          int        `json:"id"`
	Version     int        `json:"version"`
	Text        string     `json:"text"`
	State       string     `json:"state"`
	Author      *bbUserRef `json:"author"`
	CreatedDate int64      `json:"createdDate"`
	Anchor      *struct {
		ID   int    `json:"id"`
		Type string `json:"type"`
	} `json:"anchor"`
}

type bbCommit struct {
	ID        string `json:"id"`
	DisplayID string `json:"displayId"`
	Author    struct {
		Name string `json:"name"`
	} `json:"author"`
	AuthorTimestamp int64  `json:"authorTimestamp"`
	Message         string `json:"message"`
}

type bbRepo struct {
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Project struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"project"`
}

type Account struct {
	Name         string `json:"name"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
	Active       *bool  `json:"active"`
	Slug         string `json:"slug"`
}

// bbPaged is parameterised at decode time; values use the concrete element type.
type bbPaged[T any] struct {
	Values        []T  `json:"values"`
	Size          int  `json:"size"`
	IsLastPage    bool `json:"isLastPage"`
	NextPageStart int  `json:"nextPageStart"`
	Start         int  `json:"start"`
}

type Remote struct {
	ProjectKey string
	RepoSlug   string
}

// ── Module helpers ───────────────────────────────────────────────────────────

var (
	bbSSHRe  = regexp.MustCompile(`ssh://[^/]+/([^/]+)/([^/]+?)(?:\.git)?$`)
	bbSCPRe  = regexp.MustCompile(`^[^@]+@[^:]+:([^/]+)/([^/]+?)(?:\.git)?$`)
	bbHTTPRe = regexp.MustCompile(`/scm/([^/]+)/([^/]+?)(?:\.git)?$`)
)

// ParseRemote parses a Bitbucket Server remote into project + slug.
func ParseRemote(remoteURL string) *Remote {
	if m := bbSSHRe.FindStringSubmatch(remoteURL); m != nil {
		return &Remote{m[1], m[2]}
	}
	if m := bbSCPRe.FindStringSubmatch(remoteURL); m != nil {
		return &Remote{m[1], m[2]}
	}
	if m := bbHTTPRe.FindStringSubmatch(remoteURL); m != nil {
		return &Remote{m[1], m[2]}
	}
	return nil
}

func toBranchRef(branch string) string {
	if strings.HasPrefix(branch, "refs/") {
		return branch
	}
	return "refs/heads/" + branch
}

func branchDisplayID(branch string) string {
	return strings.TrimPrefix(branch, "refs/heads/")
}

func formatBBDate(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02")
}

func capTextBB(value string, limit int) string {
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

type attachmentRef struct {
	id, filename, source string
}

func collectAttachmentRefs(input, source string, out map[string]*attachmentRef, order *[]string) {
	if input == "" {
		return
	}
	for _, m := range attachmentRefRe.FindAllStringSubmatch(input, -1) {
		id := m[2]
		if _, ok := out[id]; !ok {
			name := m[1]
			if name == "" {
				name = "(unnamed)"
			}
			out[id] = &attachmentRef{id: id, filename: name, source: source}
			*order = append(*order, id)
		}
	}
}

func collectFromCommentTree(c *bbComment, out map[string]*attachmentRef, order *[]string) {
	if c.Deleted {
		return
	}
	collectAttachmentRefs(c.Text, fmt.Sprintf("comment #%d", c.ID), out, order)
	for i := range c.Comments {
		collectFromCommentTree(&c.Comments[i], out, order)
	}
}

func formatCommentThread(c *bbComment, indent string, depth int) []string {
	if depth > 20 {
		return []string{indent + "... (deeply nested replies omitted)"}
	}
	author := "Unknown"
	if c.Author != nil {
		if c.Author.DisplayName != "" {
			author = c.Author.DisplayName
		} else if c.Author.Name != "" {
			author = c.Author.Name
		}
	}
	date := ""
	if c.CreatedDate != 0 {
		date = " (" + formatBBDate(c.CreatedDate) + ")"
	}
	var flags []string
	if st := c.State; st != "" && st != "OPEN" {
		flags = append(flags, st)
	}
	if sev := c.Severity; sev != "" && sev != "NORMAL" {
		flags = append(flags, sev)
	}
	if c.ThreadResolved != nil && *c.ThreadResolved {
		flags = append(flags, "thread=RESOLVED")
	}
	flagStr := ""
	if len(flags) > 0 {
		flagStr = " [" + strings.Join(flags, "/") + "]"
	}
	lines := []string{
		fmt.Sprintf("%s#%d%s %s%s (v%d)", indent, c.ID, flagStr, author, date, c.Version),
		indent + c.Text,
	}
	for i := range c.Comments {
		lines = append(lines, formatCommentThread(&c.Comments[i], indent+"  ", depth+1)...)
	}
	return lines
}

func commentMatchesState(c *bbComment, state string) bool {
	severity := c.Severity
	if severity == "" {
		severity = "NORMAL"
	}
	if state != "PENDING" && severity != "BLOCKER" && c.ThreadResolved != nil {
		threadState := "OPEN"
		if *c.ThreadResolved {
			threadState = "RESOLVED"
		}
		if threadState == state {
			return true
		}
		for i := range c.Comments {
			if commentMatchesState(&c.Comments[i], state) {
				return true
			}
		}
		return false
	}
	cur := c.State
	if cur == "" {
		cur = "OPEN"
	}
	if cur == state {
		return true
	}
	for i := range c.Comments {
		if commentMatchesState(&c.Comments[i], state) {
			return true
		}
	}
	return false
}

func commentMatchesSeverity(c *bbComment, severity string) bool {
	if severity == "ALL" {
		return true
	}
	cur := c.Severity
	if cur == "" {
		cur = "NORMAL"
	}
	if cur == severity {
		return true
	}
	for i := range c.Comments {
		if commentMatchesSeverity(&c.Comments[i], severity) {
			return true
		}
	}
	return false
}

func uniqueCommentsFromActivities(activities []bbActivity) []bbComment {
	type slot struct {
		c   bbComment
		idx int
	}
	byID := map[int]*slot{}
	order := 0
	for _, a := range activities {
		if a.Comment == nil {
			continue
		}
		comment := *a.Comment
		existing, ok := byID[comment.ID]
		if !ok {
			byID[comment.ID] = &slot{c: comment, idx: order}
			order++
			continue
		}
		if comment.Version > existing.c.Version {
			existing.c = comment
			continue
		}
		if comment.Version == existing.c.Version {
			cu := comment.UpdatedDate
			if cu == 0 {
				cu = comment.CreatedDate
			}
			eu := existing.c.UpdatedDate
			if eu == 0 {
				eu = existing.c.CreatedDate
			}
			if cu > eu {
				existing.c = comment
			}
		}
	}
	var out []bbComment
	for _, s := range byID {
		if !s.c.Deleted {
			out = append(out, s.c)
		}
	}
	// sort by createdDate desc (stable insertion sort)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedDate > out[j-1].CreatedDate; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func pageHintPaged(isLast bool, nextStart int) string {
	if isLast {
		return ""
	}
	return fmt.Sprintf(" (use start=%d for next page)", nextStart)
}

func formatDiff(data *bbDiff, maxChars, charOffset int) string {
	var parts []string
	if data.FromHash != "" && data.ToHash != "" {
		parts = append(parts, fmt.Sprintf("# fromHash=%s toHash=%s", data.FromHash, data.ToHash))
		parts = append(parts, "# Pass these to bitbucket_comment as fromHash/toHash to anchor inline comments to this exact diff.")
	}
	for _, diff := range data.Diffs {
		from := "/dev/null"
		if diff.Source != nil {
			from = diff.Source.ToString
		}
		to := "/dev/null"
		if diff.Destination != nil {
			to = diff.Destination.ToString
		}
		parts = append(parts, fmt.Sprintf("--- a/%s\n+++ b/%s", from, to))
		for _, hunk := range diff.Hunks {
			parts = append(parts, fmt.Sprintf("@@ -%d,%d +%d,%d @@", hunk.SourceLine, hunk.SourceSpan, hunk.DestinationLine, hunk.DestinationSpan))
			for _, segment := range hunk.Segments {
				prefix := " "
				switch segment.Type {
				case "ADDED":
					prefix = "+"
				case "REMOVED":
					prefix = "-"
				}
				for _, line := range segment.Lines {
					parts = append(parts, prefix+line.Line)
				}
			}
		}
	}
	result := strings.Join(parts, "\n")
	if result == "" {
		return "(no diff)"
	}
	return text.Page(result, charOffset, maxChars, "diffCharOffset", "diffMaxChars")
}

func parseBitbucketErrorDetails(errText string) string {
	trimmed := strings.TrimSpace(errText)
	if trimmed == "" {
		return ""
	}
	var payload struct {
		Errors []struct {
			Message string `json:"message"`
			Context string `json:"context"`
		} `json:"errors"`
	}
	if json.Unmarshal([]byte(trimmed), &payload) == nil && len(payload.Errors) > 0 {
		var messages []string
		for _, e := range payload.Errors {
			msg := strings.TrimSpace(e.Message)
			if msg == "" {
				continue
			}
			if e.Context != "" {
				messages = append(messages, e.Context+": "+msg)
			} else {
				messages = append(messages, msg)
			}
		}
		if len(messages) > 0 {
			return strings.Join(messages, " | ")
		}
	}
	if len(trimmed) > 500 {
		return trimmed[:500] + "..."
	}
	return trimmed
}

func formatBitbucketError(status int, method, path, details string) string {
	prefix := fmt.Sprintf("Bitbucket %d %s %s", status, method, path)
	switch status {
	case 400:
		return strings.TrimSpace(fmt.Sprintf("%s. Invalid request or parameters. %s", prefix, details))
	case 401:
		return prefix + ". Authentication failed. Check BITBUCKET_ACCESS_TOKEN."
	case 403:
		return prefix + ". Permission denied. Check repository/project permissions for this token."
	case 404:
		return prefix + ". Resource not found. Verify project/repo/PR identifiers and access."
	case 409:
		return strings.TrimSpace(fmt.Sprintf("%s. Conflict (often stale version/state). Refresh and retry. %s", prefix, details))
	}
	if details != "" {
		return prefix + ". " + details
	}
	return prefix
}

func validateCommentText(textValue string) (string, error) {
	trimmed := strings.TrimSpace(textValue)
	if trimmed == "" {
		return "", errors.New("Bitbucket comment text must not be empty.")
	}
	if jira.EmojiRe.MatchString(trimmed) {
		return "", errors.New("Bitbucket comments must not include emoji. Use concise plain text only.")
	}
	return trimmed, nil
}

func validateSuggestionPlacement(textValue string) error {
	if !strings.Contains(textValue, "```suggestion") {
		return nil
	}
	loc := markup.SuggestionBlockRe.FindStringIndex(textValue)
	if loc == nil {
		return errors.New("Invalid suggestion block format. Use the suggestion field to post code suggestions.")
	}
	trailing := strings.TrimSpace(textValue[loc[1]:])
	if len(trailing) > 0 {
		return errors.New("When using ```suggestion```, do not add text after the closing code fence. Put any explanation before the suggestion block or use the suggestion field.")
	}
	return nil
}

// ── Client ───────────────────────────────────────────────────────────────────

type Client struct {
	baseURL         string
	token           string
	creds           config.Credentials
	mu              sync.Mutex // guards currentUsername (HTTP is concurrent)
	currentUsername string
}

// PRReviews records the commit pair a client was last shown for each PR, so an
// inline comment anchors to the state that was actually reviewed instead of
// whatever the PR head has become. Keyed by project/repo/PR.
type PRReviews struct {
	mu   sync.Mutex
	seen map[string]reviewedState
}

type reviewedState struct {
	fromHash string
	toHash   string
}

func (p *PRReviews) remember(key, fromHash, toHash string) {
	if p == nil || key == "" || fromHash == "" || toHash == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = map[string]reviewedState{}
	}
	p.seen[key] = reviewedState{fromHash: fromHash, toHash: toHash}
}

func (p *PRReviews) last(key string) (string, string, bool) {
	if p == nil {
		return "", "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.seen[key]
	return st.fromHash, st.toHash, ok
}

// reviewKey scopes remembered state to one PR in one repository.
func reviewKey(projectKey, repoSlug string, prID int) string {
	return fmt.Sprintf("%s/%s#%d", projectKey, repoSlug, prID)
}

func NewBitbucketClient(baseURL, token string, creds config.Credentials) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, creds: creds}
}

func (c *Client) doRequest(apiBase, method, path string, body any) ([]byte, int, error) {
	reqURL := c.baseURL + apiBase + path
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, reqURL, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := httpx.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return raw, res.StatusCode, nil
}

// request mirrors the TS request(): formatted error on non-2xx, nil on 204.
func (c *Client) request(method, path string, body any) ([]byte, error) {
	raw, status, err := c.doRequest("/rest/api/1.0", method, path, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%s", formatBitbucketError(status, method, path, parseBitbucketErrorDetails(string(raw))))
	}
	if status == 204 {
		return nil, nil
	}
	return raw, nil
}

func bbDecode[T any](c *Client, method, path string, body any) (*T, error) {
	raw, err := c.request(method, path, body)
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

func (c *Client) requestText(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/rest/api/1.0"+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := httpx.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("%s", formatBitbucketError(res.StatusCode, "GET", path, parseBitbucketErrorDetails(string(raw))))
	}
	return string(raw), nil
}

func (c *Client) requestBuildStatus(path string) (*struct {
	Values []bbBuildStatus `json:"values"`
}, error) {
	raw, status, err := c.doRequest("/rest/build-status/1.0", "GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, nil
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%s", formatBitbucketError(status, "GET", path, parseBitbucketErrorDetails(string(raw))))
	}
	if status == 204 || len(raw) == 0 {
		return nil, nil
	}
	var v struct {
		Values []bbBuildStatus `json:"values"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil, nil
	}
	return &v, nil
}

func (c *Client) Whoami() (string, error) { return c.getCurrentUsername() }

func (c *Client) getCurrentUsername() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentUsername != "" {
		return c.currentUsername, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/rest/api/1.0/application-properties", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := httpx.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	username := res.Header.Get("X-Ausername")
	if username == "" {
		return "", errors.New("Could not determine current Bitbucket user. Check token permissions.")
	}
	c.currentUsername = username
	return username, nil
}

func (c *Client) rp(projectKey, repoSlug string) string {
	return "/projects/" + url.PathEscape(projectKey) + "/repos/" + url.PathEscape(repoSlug)
}

// viewerRole says how the caller relates to this PR. bitbucket_comment enforces
// the author case; surfacing it here means a review flow knows which hat it is
// wearing before it starts writing comments.
func (c *Client) viewerRole(pr *PullRequest) string {
	me, err := c.getCurrentUsername()
	if err != nil || me == "" || pr == nil {
		return ""
	}
	if pr.Author.User.Name == me {
		return "you are the author"
	}
	for _, r := range pr.Reviewers {
		if r.User.Name == me {
			return "you are a reviewer"
		}
	}
	return "you are neither author nor reviewer"
}

func (c *Client) pullRequestURL(projectKey, repoSlug string, prID int, pr *PullRequest) string {
	if pr != nil && pr.Links != nil && len(pr.Links.Self) > 0 {
		if h := strings.TrimSpace(pr.Links.Self[0].Href); h != "" {
			return h
		}
	}
	return fmt.Sprintf("%s/projects/%s/repos/%s/pull-requests/%d", c.baseURL, url.PathEscape(projectKey), url.PathEscape(repoSlug), prID)
}

func (c *Client) configuredHostname() string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func (c *Client) remoteMatchesInstance(remote string) bool {
	host := c.configuredHostname()
	if host == "" {
		return true
	}
	return strings.Contains(strings.ToLower(remote), host)
}

func (c *Client) IsRemoteForThisInstance(remoteURL string) bool {
	return c.remoteMatchesInstance(remoteURL)
}

// ResolveProjectAndRepo returns the project/repo for a call. Explicit
// projectKey+repoSlug bypass git entirely (the primary HTTP path). Otherwise
// they are auto-detected from the origin remote of repoRoot (the client-
// supplied repo); repoRoot is "" when no repoPath/roots/cwd resolved.
func (c *Client) ResolveProjectAndRepo(projectKey, repoSlug, repoRoot string) (string, string, error) {
	if projectKey != "" && repoSlug != "" {
		return projectKey, repoSlug, nil
	}
	if repoRoot == "" {
		return "", "", errors.New("Could not determine projectKey/repoSlug — provide them explicitly, pass repoPath, or connect a client that provides workspace roots.")
	}
	remote := gitrepo.OriginURL(repoRoot)
	if remote != "" {
		if !c.remoteMatchesInstance(remote) {
			return "", "", fmt.Errorf("This repo's remote does not point to your configured Bitbucket instance (%s). Bitbucket tools only work with repos hosted on that instance.", c.baseURL)
		}
		if parsed := ParseRemote(remote); parsed != nil {
			pk := projectKey
			if pk == "" {
				pk = parsed.ProjectKey
			}
			rs := repoSlug
			if rs == "" {
				rs = parsed.RepoSlug
			}
			return pk, rs, nil
		}
	}
	return "", "", errors.New("Could not determine projectKey/repoSlug — provide them explicitly or run from a directory with a Bitbucket remote")
}

func (c *Client) FindOpenPrForBranch(projectKey, repoSlug, branch string) (*PullRequest, error) {
	atRef := url.QueryEscape(toBranchRef(branch))
	path := fmt.Sprintf("/projects/%s/repos/%s/pull-requests?state=OPEN&direction=OUTGOING&at=%s&limit=1",
		url.PathEscape(projectKey), url.PathEscape(repoSlug), atRef)
	data, err := bbDecode[bbPaged[PullRequest]](c, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	if data == nil || len(data.Values) == 0 {
		return nil, nil
	}
	pr := data.Values[0]
	return &pr, nil
}

func (c *Client) findOpenPrByBranchFilter(projectKey, repoSlug, filterText string) (*PullRequest, error) {
	branches, err := bbDecode[bbPaged[bbBranch]](c, "GET", fmt.Sprintf("%s/branches?limit=25&filterText=%s", c.rp(projectKey, repoSlug), url.QueryEscape(filterText)), nil)
	if err != nil {
		return nil, err
	}
	if branches == nil || len(branches.Values) == 0 {
		return nil, nil
	}
	for _, b := range branches.Values {
		pr, err := c.FindOpenPrForBranch(projectKey, repoSlug, b.DisplayID)
		if err != nil {
			return nil, err
		}
		if pr != nil {
			return pr, nil
		}
	}
	return nil, nil
}

// DefaultBranchName is the server's answer for this repository, or "" when it
// does not answer. Callers that need a usable value fall back themselves —
// guessing inside here hides "unknown" behind a plausible branch name.
func (c *Client) DefaultBranchName(projectKey, repoSlug string) string {
	data, err := bbDecode[struct {
		DisplayID string `json:"displayId"`
	}](c, "GET", c.rp(projectKey, repoSlug)+"/default-branch", nil)
	if err != nil || data == nil {
		return ""
	}
	return data.DisplayID
}

func (c *Client) getDefaultBranchRef(projectKey, repoSlug, repoRoot string) string {
	data, err := bbDecode[struct {
		DisplayID string `json:"displayId"`
	}](c, "GET", c.rp(projectKey, repoSlug)+"/default-branch", nil)
	if err == nil && data != nil && data.DisplayID != "" {
		return "refs/heads/" + data.DisplayID
	}
	if r, err := gitrepo.Open(repoRoot); err == nil {
		if head := gitrepo.OriginHead(r); head != "" {
			return "refs/heads/" + head
		}
	}
	return "refs/heads/master"
}

// ── search dispatcher (bitbucket_search) ─────────────────────────────────────

func (c *Client) Search(args map[string]any, repoRoot string) (tool.Result, error) {
	resource := arg.String(args, "resource")
	if resource == "" {
		resource = "pull_requests"
	}
	switch resource {
	case "repos":
		return c.listRepos(arg.String(args, "projectKey"), arg.IntDefault(args, "limit", 50), arg.Int(args, "start"))
	case "branches":
		return c.getBranches(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot, arg.String(args, "filter"), arg.IntDefault(args, "limit", 25), arg.Int(args, "start"))
	case "users":
		return c.searchUsers(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), arg.String(args, "query"), arg.IntDefault(args, "limit", 25), arg.Int(args, "start"))
	case "pull_requests":
		if arg.Bool(args, "mine") {
			return c.myPrs(arg.IntDefault(args, "limit", 25), arg.Int(args, "start"), arg.String(args, "role"))
		}
		return c.listPullRequests(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot, arg.String(args, "state"), arg.String(args, "fromBranch"), arg.String(args, "text"), arg.IntDefault(args, "limit", 25), arg.Int(args, "start"))
	default:
		return tool.Result{}, fmt.Errorf("Unknown resource %q. Use one of: pull_requests, repos, branches, users.", resource)
	}
}

func (c *Client) listRepos(projectKey string, limit, start int) (tool.Result, error) {
	qs := fmt.Sprintf("?limit=%d&start=%d", limit, start)
	path := "/repos" + qs
	if projectKey != "" {
		path = "/projects/" + url.PathEscape(projectKey) + "/repos" + qs
	}
	data, err := bbDecode[bbPaged[bbRepo]](c, "GET", path, nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return tool.TextResult("No repositories found."), nil
	}
	var lines []string
	for i, r := range data.Values {
		lines = append(lines, fmt.Sprintf("%d. %s/%s — %s", start+i+1, r.Project.Key, r.Slug, r.Name))
	}
	return tool.TextResult(fmt.Sprintf("%d repo(s)%s:\n%s", len(data.Values), pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n"))), nil
}

func (c *Client) usersPath(projectKey, repoSlug, query string, limit, start int) string {
	params := url.Values{}
	if query != "" {
		params.Set("filter", query)
	}
	params.Set("limit", strconv.Itoa(limit))
	if start != 0 {
		params.Set("start", strconv.Itoa(start))
	}
	switch {
	case projectKey != "" && repoSlug != "":
		return c.rp(projectKey, repoSlug) + "/permissions/users?" + params.Encode()
	case projectKey != "":
		return "/projects/" + url.PathEscape(projectKey) + "/permissions/users?" + params.Encode()
	default:
		return "/users?" + params.Encode()
	}
}

// permEntry decodes either a bare user or a {user, permission} wrapper.
type permEntry struct {
	Account

	User *Account `json:"user"`
}

func (e permEntry) resolved() Account {
	if e.User != nil {
		return *e.User
	}
	return e.Account
}

// usersPage lists users scoped to the repo/project, falling back to the global
// user directory when the token may not read repository permissions. The scope
// is only an access filter on the results, so a 401/403 there is not worth
// surfacing — silently widening beats handing the caller a dead end.
func (c *Client) usersPage(projectKey, repoSlug, query string, limit, start int) (*bbPaged[permEntry], error) {
	data, err := bbDecode[bbPaged[permEntry]](c, "GET", c.usersPath(projectKey, repoSlug, query, limit, start), nil)
	if err != nil && (projectKey != "" || repoSlug != "") && isBbAccessDenied(err) {
		logx.Logf("scoped user lookup denied (%v), falling back to the global user directory", err)
		return bbDecode[bbPaged[permEntry]](c, "GET", c.usersPath("", "", query, limit, start), nil)
	}
	return data, err
}

func isBbAccessDenied(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "Bitbucket 401 ") || strings.Contains(msg, "Bitbucket 403 ")
}

func (c *Client) searchUsers(projectKey, repoSlug, query string, limit, start int) (tool.Result, error) {
	data, err := c.usersPage(projectKey, repoSlug, query, limit, start)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return tool.TextResult("No users found."), nil
	}
	var lines []string
	for i, entry := range data.Values {
		u := entry.resolved()
		parts := []string{fmt.Sprintf("%d. %s (%s)", i+1, u.DisplayName, u.Name)}
		if u.EmailAddress != "" {
			parts = append(parts, "— "+u.EmailAddress)
		}
		if u.Active != nil && !*u.Active {
			parts = append(parts, "[inactive]")
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	return tool.TextResult(fmt.Sprintf("%d user(s)%s:\n%s", len(data.Values), pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n"))), nil
}

func (c *Client) SearchUsersRaw(projectKey, repoSlug, query string, limit int) ([]Account, error) {
	data, err := c.usersPage(projectKey, repoSlug, query, limit, 0)
	if err != nil {
		return nil, err
	}
	var out []Account
	if data != nil {
		for _, e := range data.Values {
			out = append(out, e.resolved())
		}
	}
	return out, nil
}

func (c *Client) listPullRequests(projectKey, repoSlug, repoRoot, state, fromBranch, searchText string, limit, start int) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	if state == "" {
		state = "OPEN"
	}
	qs := url.Values{}
	qs.Set("state", state)
	qs.Set("limit", strconv.Itoa(limit))
	qs.Set("start", strconv.Itoa(start))
	if fromBranch != "" {
		qs.Set("at", toBranchRef(fromBranch))
		qs.Set("direction", "OUTGOING")
	}
	if searchText != "" {
		qs.Set("filterText", searchText)
	}
	data, err := bbDecode[bbPaged[PullRequest]](c, "GET", fmt.Sprintf("/projects/%s/repos/%s/pull-requests?%s", url.PathEscape(pk), url.PathEscape(rs), qs.Encode()), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return tool.TextResult(fmt.Sprintf("No %s pull requests found.", state)), nil
	}
	var lines []string
	for _, pr := range data.Values {
		lines = append(lines, fmt.Sprintf("#%d [%s] %s | %s → %s | by %s", pr.ID, pr.State, pr.Title, pr.FromRef.DisplayID, pr.ToRef.DisplayID, pr.Author.User.DisplayName))
	}
	return tool.TextResult(fmt.Sprintf("%d PR(s) (%s)%s:\n%s", len(data.Values), state, pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n"))), nil
}

func (c *Client) myPrs(limit, start int, role string) (tool.Result, error) {
	qs := url.Values{}
	qs.Set("limit", strconv.Itoa(limit))
	qs.Set("start", strconv.Itoa(start))
	qs.Set("state", "OPEN")
	if role != "" {
		qs.Set("role", strings.ToUpper(role))
	}
	data, err := bbDecode[bbPaged[PullRequest]](c, "GET", "/dashboard/pull-requests?"+qs.Encode(), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return tool.TextResult("No pull requests found."), nil
	}
	var lines []string
	for _, pr := range data.Values {
		repo := fmt.Sprintf("%s/%s", pr.ToRef.Repository.Project.Key, pr.ToRef.Repository.Slug)
		lines = append(lines, fmt.Sprintf("#%d [%s] %s | %s | %s → %s", pr.ID, pr.State, pr.Title, repo, pr.FromRef.DisplayID, pr.ToRef.DisplayID))
	}
	return tool.TextResult(fmt.Sprintf("%d PR(s)%s:\n%s", len(data.Values), pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n"))), nil
}

func (c *Client) getPullRequest(projectKey, repoSlug, repoRoot string, prID int) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	data, err := bbDecode[PullRequest](c, "GET", fmt.Sprintf("/projects/%s/repos/%s/pull-requests/%d", url.PathEscape(pk), url.PathEscape(rs), prID), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil {
		return tool.TextResult("Pull request not found."), nil
	}
	reviewers := joinReviewers(data.Reviewers)
	desc := data.Description
	if desc == "" {
		desc = "(no description)"
	}
	lines := []string{
		fmt.Sprintf("PR #%d: %s", data.ID, data.Title),
		"State:     " + data.State,
		"Author:    " + data.Author.User.DisplayName,
		fmt.Sprintf("Branch:    %s → %s", data.FromRef.DisplayID, data.ToRef.DisplayID),
		"Reviewers: " + text.Or(reviewers, "None"),
		"",
		"Description:",
		desc,
	}
	return tool.TextResult(strings.Join(lines, "\n")), nil
}

func joinReviewers(reviewers []bbReviewer) string {
	parts := make([]string, 0, len(reviewers))
	for _, r := range reviewers {
		mark := ""
		if r.Approved {
			mark = " ✓"
		}
		parts = append(parts, r.User.DisplayName+mark)
	}
	return strings.Join(parts, ", ")
}

func (c *Client) GetPrOverview(reviews *PRReviews, args map[string]any, repoRoot string) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	includeCommits := arg.BoolDefault(args, "includeCommits", true)
	includeComments := arg.BoolDefault(args, "includeComments", true)
	includeDiff := arg.BoolDefault(args, "includeDiff", false)
	includeBuildStatus := arg.BoolDefault(args, "includeBuildStatus", true)
	descriptionCap := 2000
	if arg.Bool(args, "fullDescription") {
		descriptionCap = 0
	} else if arg.Has(args, "descriptionMaxChars") {
		descriptionCap = arg.Int(args, "descriptionMaxChars")
	}

	var prID int
	if arg.Has(args, "prId") {
		prID = arg.Int(args, "prId")
	} else {
		branch := arg.String(args, "fromBranch")
		if branch == "" {
			if repoRoot != "" {
				branch = gitrepo.CurrentBranch(repoRoot)
			}
		}
		if branch == "" || branch == "HEAD" {
			return tool.Result{}, errors.New("Provide prId or fromBranch, or run from a checked-out branch.")
		}
		found, err := c.FindOpenPrForBranch(pk, rs, branch)
		if err != nil {
			return tool.Result{}, err
		}
		if found == nil {
			found, err = c.findOpenPrByBranchFilter(pk, rs, branchDisplayID(branch))
			if err != nil {
				return tool.Result{}, err
			}
		}
		if found == nil {
			return tool.Result{}, fmt.Errorf("No open PR found for branch %q.", branchDisplayID(branch))
		}
		prID = found.ID
	}

	pr, err := bbDecode[PullRequest](c, "GET", fmt.Sprintf("%s/pull-requests/%d", c.rp(pk, rs), prID), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if pr == nil {
		return tool.TextResult("Pull request not found."), nil
	}

	var sections []string
	attachmentRefs := map[string]*attachmentRef{}
	var attachmentOrder []string
	collectAttachmentRefs(pr.Description, "description", attachmentRefs, &attachmentOrder)

	reviewers := joinReviewers(pr.Reviewers)
	prURL := ""
	if pr.Links != nil && len(pr.Links.Self) > 0 {
		prURL = pr.Links.Self[0].Href
	}
	fromHash := pr.ToRef.LatestCommit
	toHash := pr.FromRef.LatestCommit
	commitsLine := ""
	if fromHash != "" && toHash != "" {
		// Remembered per session: bitbucket_comment then anchors inline comments
		// to this pair by default, so nobody has to carry the hashes by hand.
		reviews.remember(reviewKey(pk, rs, prID), fromHash, toHash)
		commitsLine = fmt.Sprintf("Commits:   fromHash=%s toHash=%s (inline comments anchor here automatically)", fromHash, toHash)
	}
	desc := pr.Description
	if desc == "" {
		desc = "(no description)"
	} else {
		desc = capTextBB(desc, descriptionCap)
	}
	headerLines := []string{
		fmt.Sprintf("PR #%d: %s", pr.ID, pr.Title),
		"State:     " + pr.State,
		"Author:    " + pr.Author.User.DisplayName,
		fmt.Sprintf("Branch:    %s → %s", pr.FromRef.DisplayID, pr.ToRef.DisplayID),
	}
	if role := c.viewerRole(pr); role != "" {
		headerLines = append(headerLines, "Viewing as: "+role)
	}
	if commitsLine != "" {
		headerLines = append(headerLines, commitsLine)
	}
	headerLines = append(headerLines, "Reviewers: "+text.Or(reviewers, "None"))
	if prURL != "" {
		headerLines = append(headerLines, "URL:       "+prURL)
	}
	headerLines = append(headerLines, "", "Description:", desc)
	sections = append(sections, strings.Join(headerLines, "\n"))

	if includeBuildStatus && pr.FromRef.LatestCommit != "" {
		statuses, _ := c.requestBuildStatus("/commits/" + pr.FromRef.LatestCommit)
		short := pr.FromRef.LatestCommit
		if len(short) > 8 {
			short = short[:8]
		}
		if statuses != nil && len(statuses.Values) > 0 {
			var statusLines []string
			for _, s := range statuses.Values {
				icon := buildIcon(s.State)
				name := s.Name
				if name == "" {
					name = s.Key
				}
				descPart := ""
				if s.Description != "" {
					descPart = " — " + s.Description
				}
				urlPart := ""
				if s.URL != "" {
					urlPart = "\n   URL: " + s.URL
				}
				statusLines = append(statusLines, fmt.Sprintf("%s [%s] %s%s%s", icon, s.State, name, descPart, urlPart))
			}
			sections = append(sections, fmt.Sprintf("Build status (%s):\n%s", short, strings.Join(statusLines, "\n")))
		} else {
			sections = append(sections, "Build status: none reported for "+short)
		}
	}

	if includeCommits {
		commitsLimit := arg.IntDefault(args, "commitsLimit", 25)
		commitsStart := arg.IntDefault(args, "commitsStart", 0)
		data, err := bbDecode[bbPaged[bbCommit]](c, "GET", fmt.Sprintf("%s/pull-requests/%d/commits?limit=%d&start=%d", c.rp(pk, rs), prID, commitsLimit, commitsStart), nil)
		if err != nil {
			return tool.Result{}, err
		}
		if data == nil || len(data.Values) == 0 {
			sections = append(sections, "Commits:\n(no commits found)")
		} else {
			var lines []string
			for _, cm := range data.Values {
				lines = append(lines, fmt.Sprintf("%s %s %s: %s", cm.DisplayID, formatBBDate(cm.AuthorTimestamp), cm.Author.Name, firstLine(cm.Message)))
			}
			sections = append(sections, fmt.Sprintf("Commits (%d)%s:\n%s", len(data.Values), pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n")))
		}
	}

	if includeComments {
		commentsLimit := arg.IntDefault(args, "commentsLimit", 50)
		commentsStart := arg.IntDefault(args, "commentsStart", 0)
		commentsState := arg.String(args, "commentsState")
		if commentsState == "" {
			commentsState = "ALL"
		}
		commentsSeverity := arg.String(args, "commentsSeverity")
		if commentsSeverity == "" {
			commentsSeverity = "ALL"
		}
		if commentsSeverity == "BLOCKER" && commentsState == "PENDING" {
			return tool.Result{}, errors.New("commentsState=PENDING is not valid when commentsSeverity=BLOCKER. Use OPEN, RESOLVED, or ALL.")
		}
		if commentsSeverity == "BLOCKER" {
			qs := url.Values{}
			qs.Set("limit", strconv.Itoa(commentsLimit))
			qs.Set("start", strconv.Itoa(commentsStart))
			if commentsState != "ALL" {
				qs.Set("state", commentsState)
			}
			data, err := bbDecode[bbPaged[bbComment]](c, "GET", fmt.Sprintf("%s/pull-requests/%d/blocker-comments?%s", c.rp(pk, rs), prID, qs.Encode()), nil)
			if err != nil {
				return tool.Result{}, err
			}
			if data == nil || len(data.Values) == 0 {
				sections = append(sections, fmt.Sprintf("Comments:\n(no %s BLOCKER comments)", commentsState))
			} else {
				var blocks []string
				for i := range data.Values {
					collectFromCommentTree(&data.Values[i], attachmentRefs, &attachmentOrder)
					blocks = append(blocks, strings.Join(formatCommentThread(&data.Values[i], "", 0), "\n"))
				}
				sections = append(sections, fmt.Sprintf("Comments (%d BLOCKER thread(s))%s:\n\n%s", len(data.Values), pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(blocks, "\n\n")))
			}
		} else {
			activityData, err := bbDecode[bbPaged[bbActivity]](c, "GET", fmt.Sprintf("%s/pull-requests/%d/activities?limit=%d&start=%d", c.rp(pk, rs), prID, commentsLimit, commentsStart), nil)
			if err != nil {
				return tool.Result{}, err
			}
			var activities []bbActivity
			if activityData != nil {
				activities = activityData.Values
			}
			comments := uniqueCommentsFromActivities(activities)
			var filtered []bbComment
			for _, cm := range comments {
				ms := commentsState == "ALL" || commentMatchesState(&cm, commentsState)
				if ms && commentMatchesSeverity(&cm, commentsSeverity) {
					filtered = append(filtered, cm)
				}
			}
			for i := range filtered {
				collectFromCommentTree(&filtered[i], attachmentRefs, &attachmentOrder)
			}
			if len(filtered) == 0 {
				sections = append(sections, "Comments:\n(no matching comments)")
			} else {
				var blocks []string
				for i := range filtered {
					blocks = append(blocks, strings.Join(formatCommentThread(&filtered[i], "", 0), "\n"))
				}
				paging := ""
				if activityData != nil {
					paging = pageHintPaged(activityData.IsLastPage, activityData.NextPageStart)
				}
				sections = append(sections, fmt.Sprintf("Comments (%d thread(s), newest first)%s:\n\n%s", len(filtered), paging, strings.Join(blocks, "\n\n")))
			}
		}
	}

	if len(attachmentOrder) > 0 {
		lines := []string{fmt.Sprintf("Attachments referenced: %d", len(attachmentOrder))}
		for _, id := range attachmentOrder {
			ref := attachmentRefs[id]
			lines = append(lines, fmt.Sprintf("  #%s %s — in %s", ref.id, ref.filename, ref.source))
		}
		lines = append(lines, "Use get_attachment source=bitbucket with attachmentId to view contents.")
		sections = append(sections, strings.Join(lines, "\n"))
	}

	if includeDiff {
		data, err := bbDecode[bbDiff](c, "GET", fmt.Sprintf("%s/pull-requests/%d/diff", c.rp(pk, rs), prID), nil)
		if err != nil {
			return tool.Result{}, err
		}
		diffText := "(no diff found)"
		if data != nil {
			diffText = formatDiff(data, arg.IntDefault(args, "diffMaxChars", 8000), arg.Int(args, "diffCharOffset"))
		}
		sections = append(sections, "Diff:\n"+diffText)
	}

	return tool.TextResult(strings.Join(sections, "\n\n")), nil
}

func buildIcon(state string) string {
	switch state {
	case "SUCCESSFUL":
		return "✓"
	case "FAILED":
		return "✗"
	default:
		return "…"
	}
}

func firstLine(s string) string {
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return before
	}
	return s
}

func (c *Client) getBranches(projectKey, repoSlug, repoRoot, filter string, limit, start int) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	qs := url.Values{}
	qs.Set("limit", strconv.Itoa(limit))
	qs.Set("start", strconv.Itoa(start))
	if filter != "" {
		qs.Set("filterText", filter)
	}
	data, err := bbDecode[bbPaged[bbBranch]](c, "GET", c.rp(pk, rs)+"/branches?"+qs.Encode(), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return tool.TextResult("No branches found."), nil
	}
	var lines []string
	for _, b := range data.Values {
		def := ""
		if b.IsDefault {
			def = " (default)"
		}
		short := b.LatestCommit
		if len(short) > 8 {
			short = short[:8]
		}
		lines = append(lines, fmt.Sprintf("%s%s — %s", b.DisplayID, def, short))
	}
	return tool.TextResult(fmt.Sprintf("%d branch(es)%s:\n%s", len(data.Values), pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n"))), nil
}

func (c *Client) GetFile(args map[string]any, repoRoot string) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	path := arg.String(args, "path")
	ref := arg.String(args, "ref")
	// Reading the wrong branch is the failure mode here, and it looks exactly
	// like success. prId resolves the ref from the PR itself so a review never
	// has to remember the source branch, and the ref is echoed either way.
	origin := ""
	if ref == "" && arg.Has(args, "prId") {
		prID := arg.Int(args, "prId")
		pr, perr := bbDecode[PullRequest](c, "GET", fmt.Sprintf("%s/pull-requests/%d", c.rp(pk, rs), prID), nil)
		if perr != nil {
			return tool.Result{}, perr
		}
		if pr == nil {
			return tool.Result{}, fmt.Errorf("PR #%d not found.", prID)
		}
		ref = pr.FromRef.DisplayID
		origin = fmt.Sprintf(" (source branch of PR #%d)", prID)
	}
	qs := ""
	if ref != "" {
		qs = "?at=" + url.QueryEscape(ref)
	} else {
		if defaultRef := c.getDefaultBranchRef(pk, rs, repoRoot); defaultRef != "" {
			ref = strings.TrimPrefix(defaultRef, "refs/heads/")
		}
		origin = " (repository default branch — pass ref or prId to read the branch under review)"
	}
	content, err := c.requestText(c.rp(pk, rs) + "/raw/" + encodePath(path) + qs)
	if err != nil {
		return tool.Result{}, err
	}
	header := fmt.Sprintf("%s @ %s%s\n\n", path, text.Or(ref, "(unknown ref)"), origin)
	return tool.TextResult(header + text.Page(content, arg.Int(args, "charOffset"), arg.IntDefault(args, "maxChars", 10000), "charOffset", "maxChars")), nil
}

func encodePath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

func (c *Client) FetchFileText(projectKey, repoSlug, filePath string) string {
	content, err := c.requestText(c.rp(projectKey, repoSlug) + "/raw/" + encodePath(filePath))
	if err != nil {
		return ""
	}
	return content
}

var contentDispositionRe = regexp.MustCompile(`(?i)filename\*?=(?:UTF-8'')?"?([^";]+)"?`)

func (c *Client) GetAttachment(args map[string]any, repoRoot string) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	id := strings.TrimSpace(arg.String(args, "attachmentId"))
	if id == "" {
		return tool.Result{}, errors.New("attachmentId is required.")
	}
	saveTo := arg.String(args, "saveTo")
	timeout := 60 * time.Second
	if saveTo != "" {
		timeout = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	reqURL := c.baseURL + "/rest/api/1.0" + c.rp(pk, rs) + "/attachments/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return tool.Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := httpx.Client.Do(req)
	if err != nil {
		return tool.Result{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(res.Body)
		return tool.Result{}, fmt.Errorf("%s", formatBitbucketError(res.StatusCode, "GET", c.rp(pk, rs)+"/attachments/"+id, parseBitbucketErrorDetails(string(raw))))
	}

	filename := "attachment-" + id
	if cd := res.Header.Get("Content-Disposition"); cd != "" {
		if m := contentDispositionRe.FindStringSubmatch(cd); m != nil {
			if dec, derr := url.QueryUnescape(m[1]); derr == nil {
				filename = dec
			} else {
				filename = m[1]
			}
		}
	}
	mimeType := "application/octet-stream"
	if ct := res.Header.Get("Content-Type"); ct != "" {
		mimeType = strings.TrimSpace(strings.Split(ct, ";")[0])
	}
	var declaredLength int64
	if cl := res.Header.Get("Content-Length"); cl != "" {
		_, _ = fmt.Sscanf(cl, "%d", &declaredLength)
	}

	if saveTo != "" {
		path, _ := filepath.Abs(saveTo)
		f, err := os.Create(path)
		if err != nil {
			return tool.Result{}, err
		}
		defer func() { _ = f.Close() }()
		if _, err := io.Copy(f, res.Body); err != nil {
			return tool.Result{}, err
		}
		sizeLabel := "unknown size"
		if declaredLength > 0 {
			sizeLabel = attachment.FormatBytes(declaredLength)
		}
		return tool.TextResult(fmt.Sprintf("Saved attachment #%s (%s — %s, %s) to %s", id, filename, mimeType, sizeLabel, path)), nil
	}

	if declaredLength > media.MaxVideoSourceBytes {
		return tool.Result{}, fmt.Errorf("Attachment #%s is %s, exceeds the %s inline cap. Pass saveTo=/absolute/path to stream it to disk.", id, attachment.FormatBytes(declaredLength), attachment.FormatBytes(media.MaxVideoSourceBytes))
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
		Filename:       filename,
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
