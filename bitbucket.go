package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
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

type bbPullRequest struct {
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

type bbUser struct {
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

type bbTaskCount struct {
	Open     int `json:"open"`
	Resolved int `json:"resolved"`
	Values   []struct {
		State string `json:"state"`
		Count int    `json:"count"`
	} `json:"values"`
}

type bitbucketRemote struct {
	projectKey string
	repoSlug   string
}

// ── Module helpers ───────────────────────────────────────────────────────────

var (
	bbSSHRe  = regexp.MustCompile(`ssh://[^/]+/([^/]+)/([^/]+?)(?:\.git)?$`)
	bbSCPRe  = regexp.MustCompile(`^[^@]+@[^:]+:([^/]+)/([^/]+?)(?:\.git)?$`)
	bbHTTPRe = regexp.MustCompile(`/scm/([^/]+)/([^/]+?)(?:\.git)?$`)
)

// parseBitbucketRemote parses a Bitbucket Server remote into project + slug.
func parseBitbucketRemote(remoteURL string) *bitbucketRemote {
	if m := bbSSHRe.FindStringSubmatch(remoteURL); m != nil {
		return &bitbucketRemote{m[1], m[2]}
	}
	if m := bbSCPRe.FindStringSubmatch(remoteURL); m != nil {
		return &bitbucketRemote{m[1], m[2]}
	}
	if m := bbHTTPRe.FindStringSubmatch(remoteURL); m != nil {
		return &bitbucketRemote{m[1], m[2]}
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

func capTextBB(value string, max int) string {
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

func formatDiff(data *bbDiff, maxChars int) string {
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
	if len(result) > maxChars {
		return result[:maxChars] + fmt.Sprintf("\n\n... (truncated, %d more chars)", len(result)-maxChars)
	}
	return result
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
		return "", fmt.Errorf("Bitbucket comment text must not be empty.")
	}
	if emojiRe.MatchString(trimmed) {
		return "", fmt.Errorf("Bitbucket comments must not include emoji. Use concise plain text only.")
	}
	return trimmed, nil
}

var suggestionBlockRe = regexp.MustCompile("(?s)```suggestion[^\\n]*\\n.*?\\n```")

func validateSuggestionPlacement(textValue string) error {
	if !strings.Contains(textValue, "```suggestion") {
		return nil
	}
	loc := suggestionBlockRe.FindStringIndex(textValue)
	if loc == nil {
		return fmt.Errorf("Invalid suggestion block format. Use the suggestion field to post code suggestions.")
	}
	trailing := strings.TrimSpace(textValue[loc[1]:])
	if len(trailing) > 0 {
		return fmt.Errorf("When using ```suggestion```, do not add text after the closing code fence. Put any explanation before the suggestion block or use the suggestion field.")
	}
	return nil
}

// ── Client ───────────────────────────────────────────────────────────────────

type BitbucketClient struct {
	baseURL         string
	token           string
	mu              sync.Mutex // guards currentUsername (HTTP is concurrent)
	currentUsername string
}

func NewBitbucketClient(baseURL, token string) *BitbucketClient {
	return &BitbucketClient{baseURL: strings.TrimRight(baseURL, "/"), token: token}
}

func (c *BitbucketClient) doRequest(apiBase, method, path string, body any) ([]byte, int, error) {
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
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return raw, res.StatusCode, nil
}

// request mirrors the TS request(): formatted error on non-2xx, nil on 204.
func (c *BitbucketClient) request(method, path string, body any) ([]byte, error) {
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

func bbDecode[T any](c *BitbucketClient, method, path string, body any) (*T, error) {
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

func (c *BitbucketClient) requestText(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/rest/api/1.0"+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("%s", formatBitbucketError(res.StatusCode, "GET", path, parseBitbucketErrorDetails(string(raw))))
	}
	return string(raw), nil
}

func (c *BitbucketClient) requestBuildStatus(path string) (*struct {
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

func (c *BitbucketClient) whoami() (string, error) { return c.getCurrentUsername() }

func (c *BitbucketClient) getCurrentUsername() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentUsername != "" {
		return c.currentUsername, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/rest/api/1.0/application-properties", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	username := res.Header.Get("X-AUSERNAME")
	if username == "" {
		return "", fmt.Errorf("Could not determine current Bitbucket user. Check token permissions.")
	}
	c.currentUsername = username
	return username, nil
}

func (c *BitbucketClient) rp(projectKey, repoSlug string) string {
	return "/projects/" + url.PathEscape(projectKey) + "/repos/" + url.PathEscape(repoSlug)
}

func (c *BitbucketClient) pullRequestURL(projectKey, repoSlug string, prID int, pr *bbPullRequest) string {
	if pr != nil && pr.Links != nil && len(pr.Links.Self) > 0 {
		if h := strings.TrimSpace(pr.Links.Self[0].Href); h != "" {
			return h
		}
	}
	return fmt.Sprintf("%s/projects/%s/repos/%s/pull-requests/%d", c.baseURL, url.PathEscape(projectKey), url.PathEscape(repoSlug), prID)
}

func (c *BitbucketClient) configuredHostname() string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func (c *BitbucketClient) remoteMatchesInstance(remote string) bool {
	host := c.configuredHostname()
	if host == "" {
		return true
	}
	return strings.Contains(strings.ToLower(remote), host)
}

func (c *BitbucketClient) isRemoteForThisInstance(remoteURL string) bool {
	return c.remoteMatchesInstance(remoteURL)
}

// resolveProjectAndRepo returns the project/repo for a call. Explicit
// projectKey+repoSlug bypass git entirely (the primary HTTP path). Otherwise
// they are auto-detected from the origin remote of repoRoot (the client-
// supplied repo); repoRoot is "" when no repoPath/roots/cwd resolved.
func (c *BitbucketClient) resolveProjectAndRepo(projectKey, repoSlug, repoRoot string) (string, string, error) {
	if projectKey != "" && repoSlug != "" {
		return projectKey, repoSlug, nil
	}
	if repoRoot == "" {
		return "", "", fmt.Errorf("Could not determine projectKey/repoSlug — provide them explicitly, pass repoPath, or connect a client that provides workspace roots.")
	}
	remote := safeGit(repoRoot, "", "remote", "get-url", "origin")
	if remote != "" {
		if !c.remoteMatchesInstance(remote) {
			return "", "", fmt.Errorf("This repo's remote does not point to your configured Bitbucket instance (%s). Bitbucket tools only work with repos hosted on that instance.", c.baseURL)
		}
		if parsed := parseBitbucketRemote(remote); parsed != nil {
			pk := projectKey
			if pk == "" {
				pk = parsed.projectKey
			}
			rs := repoSlug
			if rs == "" {
				rs = parsed.repoSlug
			}
			return pk, rs, nil
		}
	}
	return "", "", fmt.Errorf("Could not determine projectKey/repoSlug — provide them explicitly or run from a directory with a Bitbucket remote")
}

func (c *BitbucketClient) findOpenPrForBranch(projectKey, repoSlug, branch string) (*bbPullRequest, error) {
	atRef := url.QueryEscape(toBranchRef(branch))
	path := fmt.Sprintf("/projects/%s/repos/%s/pull-requests?state=OPEN&direction=OUTGOING&at=%s&limit=1",
		url.PathEscape(projectKey), url.PathEscape(repoSlug), atRef)
	data, err := bbDecode[bbPaged[bbPullRequest]](c, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	if data == nil || len(data.Values) == 0 {
		return nil, nil
	}
	pr := data.Values[0]
	return &pr, nil
}

func (c *BitbucketClient) findOpenPrByBranchFilter(projectKey, repoSlug, filterText string) (*bbPullRequest, error) {
	branches, err := bbDecode[bbPaged[bbBranch]](c, "GET", fmt.Sprintf("%s/branches?limit=25&filterText=%s", c.rp(projectKey, repoSlug), url.QueryEscape(filterText)), nil)
	if err != nil {
		return nil, err
	}
	if branches == nil || len(branches.Values) == 0 {
		return nil, nil
	}
	for _, b := range branches.Values {
		pr, err := c.findOpenPrForBranch(projectKey, repoSlug, b.DisplayID)
		if err != nil {
			return nil, err
		}
		if pr != nil {
			return pr, nil
		}
	}
	return nil, nil
}

func (c *BitbucketClient) getDefaultBranchRef(projectKey, repoSlug, repoRoot string) (string, error) {
	data, err := bbDecode[struct {
		DisplayID string `json:"displayId"`
	}](c, "GET", c.rp(projectKey, repoSlug)+"/default-branch", nil)
	if err == nil && data != nil && data.DisplayID != "" {
		return "refs/heads/" + data.DisplayID, nil
	}
	head := ""
	if repoRoot != "" {
		head = safeGit(repoRoot, "", "rev-parse", "--abbrev-ref", "origin/HEAD")
	}
	if strings.HasPrefix(head, "origin/") {
		return "refs/heads/" + strings.TrimPrefix(head, "origin/"), nil
	}
	return "refs/heads/master", nil
}

// ── search dispatcher (bitbucket_search) ─────────────────────────────────────

func (c *BitbucketClient) search(args map[string]any, repoRoot string) (toolResult, error) {
	resource := argString(args, "resource")
	if resource == "" {
		resource = "pull_requests"
	}
	switch resource {
	case "repos":
		return c.listRepos(argString(args, "projectKey"), argIntDefault(args, "limit", 50), argInt(args, "start"))
	case "branches":
		return c.getBranches(argString(args, "projectKey"), argString(args, "repoSlug"), repoRoot, argString(args, "filter"), argIntDefault(args, "limit", 25), argInt(args, "start"))
	case "users":
		return c.searchUsers(argString(args, "projectKey"), argString(args, "repoSlug"), argString(args, "query"), argIntDefault(args, "limit", 25), argInt(args, "start"))
	default:
		if argBool(args, "mine") {
			return c.myPrs(argIntDefault(args, "limit", 25), argInt(args, "start"), argString(args, "role"))
		}
		return c.listPullRequests(argString(args, "projectKey"), argString(args, "repoSlug"), repoRoot, argString(args, "state"), argString(args, "fromBranch"), argString(args, "text"), argIntDefault(args, "limit", 25), argInt(args, "start"))
	}
}

func (c *BitbucketClient) listRepos(projectKey string, limit, start int) (toolResult, error) {
	qs := fmt.Sprintf("?limit=%d&start=%d", limit, start)
	path := "/repos" + qs
	if projectKey != "" {
		path = "/projects/" + url.PathEscape(projectKey) + "/repos" + qs
	}
	data, err := bbDecode[bbPaged[bbRepo]](c, "GET", path, nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return textResult("No repositories found."), nil
	}
	var lines []string
	for i, r := range data.Values {
		lines = append(lines, fmt.Sprintf("%d. %s/%s — %s", start+i+1, r.Project.Key, r.Slug, r.Name))
	}
	return textResult(fmt.Sprintf("%d repo(s)%s:\n%s", len(data.Values), pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n"))), nil
}

func (c *BitbucketClient) usersPath(projectKey, repoSlug, query string, limit, start int) string {
	params := url.Values{}
	if query != "" {
		params.Set("filter", query)
	}
	params.Set("limit", fmt.Sprint(limit))
	if start != 0 {
		params.Set("start", fmt.Sprint(start))
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
	bbUser
	User *bbUser `json:"user"`
}

func (e permEntry) resolved() bbUser {
	if e.User != nil {
		return *e.User
	}
	return e.bbUser
}

func (c *BitbucketClient) searchUsers(projectKey, repoSlug, query string, limit, start int) (toolResult, error) {
	data, err := bbDecode[bbPaged[permEntry]](c, "GET", c.usersPath(projectKey, repoSlug, query, limit, start), nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return textResult("No users found."), nil
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
	return textResult(fmt.Sprintf("%d user(s)%s:\n%s", len(data.Values), pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n"))), nil
}

func (c *BitbucketClient) searchUsersRaw(projectKey, repoSlug, query string, limit int) ([]bbUser, error) {
	data, err := bbDecode[bbPaged[permEntry]](c, "GET", c.usersPath(projectKey, repoSlug, query, limit, 0), nil)
	if err != nil {
		return nil, err
	}
	var out []bbUser
	if data != nil {
		for _, e := range data.Values {
			out = append(out, e.resolved())
		}
	}
	return out, nil
}

func (c *BitbucketClient) listPullRequests(projectKey, repoSlug, repoRoot, state, fromBranch, searchText string, limit, start int) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	if state == "" {
		state = "OPEN"
	}
	qs := url.Values{}
	qs.Set("state", state)
	qs.Set("limit", fmt.Sprint(limit))
	qs.Set("start", fmt.Sprint(start))
	if fromBranch != "" {
		qs.Set("at", toBranchRef(fromBranch))
		qs.Set("direction", "OUTGOING")
	}
	if searchText != "" {
		qs.Set("filterText", searchText)
	}
	data, err := bbDecode[bbPaged[bbPullRequest]](c, "GET", fmt.Sprintf("/projects/%s/repos/%s/pull-requests?%s", url.PathEscape(pk), url.PathEscape(rs), qs.Encode()), nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return textResult(fmt.Sprintf("No %s pull requests found.", state)), nil
	}
	var lines []string
	for _, pr := range data.Values {
		lines = append(lines, fmt.Sprintf("#%d [%s] %s | %s → %s | by %s", pr.ID, pr.State, pr.Title, pr.FromRef.DisplayID, pr.ToRef.DisplayID, pr.Author.User.DisplayName))
	}
	return textResult(fmt.Sprintf("%d PR(s) (%s)%s:\n%s", len(data.Values), state, pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n"))), nil
}

func (c *BitbucketClient) myPrs(limit, start int, role string) (toolResult, error) {
	qs := url.Values{}
	qs.Set("limit", fmt.Sprint(limit))
	qs.Set("start", fmt.Sprint(start))
	qs.Set("state", "OPEN")
	if role != "" {
		qs.Set("role", strings.ToUpper(role))
	}
	data, err := bbDecode[bbPaged[bbPullRequest]](c, "GET", "/dashboard/pull-requests?"+qs.Encode(), nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return textResult("No pull requests found."), nil
	}
	var lines []string
	for _, pr := range data.Values {
		repo := fmt.Sprintf("%s/%s", pr.ToRef.Repository.Project.Key, pr.ToRef.Repository.Slug)
		lines = append(lines, fmt.Sprintf("#%d [%s] %s | %s | %s → %s", pr.ID, pr.State, pr.Title, repo, pr.FromRef.DisplayID, pr.ToRef.DisplayID))
	}
	return textResult(fmt.Sprintf("%d PR(s)%s:\n%s", len(data.Values), pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n"))), nil
}

func (c *BitbucketClient) getPullRequest(projectKey, repoSlug, repoRoot string, prID int) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	data, err := bbDecode[bbPullRequest](c, "GET", fmt.Sprintf("/projects/%s/repos/%s/pull-requests/%d", url.PathEscape(pk), url.PathEscape(rs), prID), nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil {
		return textResult("Pull request not found."), nil
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
		"Reviewers: " + orValue(reviewers, "None"),
		"",
		"Description:",
		desc,
	}
	return textResult(strings.Join(lines, "\n")), nil
}

func joinReviewers(reviewers []bbReviewer) string {
	var parts []string
	for _, r := range reviewers {
		mark := ""
		if r.Approved {
			mark = " ✓"
		}
		parts = append(parts, r.User.DisplayName+mark)
	}
	return strings.Join(parts, ", ")
}

func (c *BitbucketClient) getPrOverview(args map[string]any, repoRoot string) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(argString(args, "projectKey"), argString(args, "repoSlug"), repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	includeCommits := argBoolDefault(args, "includeCommits", true)
	includeComments := argBoolDefault(args, "includeComments", true)
	includeDiff := argBoolDefault(args, "includeDiff", false)
	includeBuildStatus := argBoolDefault(args, "includeBuildStatus", true)
	descriptionCap := 2000
	if argBool(args, "fullDescription") {
		descriptionCap = 0
	} else if has(args, "descriptionMaxChars") {
		descriptionCap = argInt(args, "descriptionMaxChars")
	}

	prID := 0
	if has(args, "prId") {
		prID = argInt(args, "prId")
	} else {
		branch := argString(args, "fromBranch")
		if branch == "" {
			if repoRoot != "" {
				branch = safeGit(repoRoot, "", "rev-parse", "--abbrev-ref", "HEAD")
			}
		}
		if branch == "" || branch == "HEAD" {
			return toolResult{}, fmt.Errorf("Provide prId or fromBranch, or run from a checked-out branch.")
		}
		found, err := c.findOpenPrForBranch(pk, rs, branch)
		if err != nil {
			return toolResult{}, err
		}
		if found == nil {
			found, err = c.findOpenPrByBranchFilter(pk, rs, branchDisplayID(branch))
			if err != nil {
				return toolResult{}, err
			}
		}
		if found == nil {
			return toolResult{}, fmt.Errorf("No open PR found for branch %q.", branchDisplayID(branch))
		}
		prID = found.ID
	}

	pr, err := bbDecode[bbPullRequest](c, "GET", fmt.Sprintf("%s/pull-requests/%d", c.rp(pk, rs), prID), nil)
	if err != nil {
		return toolResult{}, err
	}
	if pr == nil {
		return textResult("Pull request not found."), nil
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
		commitsLine = fmt.Sprintf("Commits:   fromHash=%s toHash=%s (pass to bitbucket_comment to anchor inline comments to this exact state)", fromHash, toHash)
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
	if commitsLine != "" {
		headerLines = append(headerLines, commitsLine)
	}
	headerLines = append(headerLines, "Reviewers: "+orValue(reviewers, "None"))
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
			sections = append(sections, fmt.Sprintf("Build status: none reported for %s", short))
		}
	}

	if includeCommits {
		commitsLimit := argIntDefault(args, "commitsLimit", 25)
		commitsStart := argIntDefault(args, "commitsStart", 0)
		data, err := bbDecode[bbPaged[bbCommit]](c, "GET", fmt.Sprintf("%s/pull-requests/%d/commits?limit=%d&start=%d", c.rp(pk, rs), prID, commitsLimit, commitsStart), nil)
		if err != nil {
			return toolResult{}, err
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
		commentsLimit := argIntDefault(args, "commentsLimit", 50)
		commentsStart := argIntDefault(args, "commentsStart", 0)
		commentsState := argString(args, "commentsState")
		if commentsState == "" {
			commentsState = "ALL"
		}
		commentsSeverity := argString(args, "commentsSeverity")
		if commentsSeverity == "" {
			commentsSeverity = "ALL"
		}
		if commentsSeverity == "BLOCKER" && commentsState == "PENDING" {
			return toolResult{}, fmt.Errorf("commentsState=PENDING is not valid when commentsSeverity=BLOCKER. Use OPEN, RESOLVED, or ALL.")
		}
		if commentsSeverity == "BLOCKER" {
			qs := url.Values{}
			qs.Set("limit", fmt.Sprint(commentsLimit))
			qs.Set("start", fmt.Sprint(commentsStart))
			if commentsState != "ALL" {
				qs.Set("state", commentsState)
			}
			data, err := bbDecode[bbPaged[bbComment]](c, "GET", fmt.Sprintf("%s/pull-requests/%d/blocker-comments?%s", c.rp(pk, rs), prID, qs.Encode()), nil)
			if err != nil {
				return toolResult{}, err
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
				return toolResult{}, err
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
		lines = append(lines, "Use bitbucket_get_attachment with attachmentId to view contents.")
		sections = append(sections, strings.Join(lines, "\n"))
	}

	if includeDiff {
		data, err := bbDecode[bbDiff](c, "GET", fmt.Sprintf("%s/pull-requests/%d/diff", c.rp(pk, rs), prID), nil)
		if err != nil {
			return toolResult{}, err
		}
		diffText := "(no diff found)"
		if data != nil {
			diffText = formatDiff(data, argIntDefault(args, "diffMaxChars", 8000))
		}
		sections = append(sections, "Diff:\n"+diffText)
	}

	return textResult(strings.Join(sections, "\n\n")), nil
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
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (c *BitbucketClient) getBranches(projectKey, repoSlug, repoRoot, filter string, limit, start int) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	qs := url.Values{}
	qs.Set("limit", fmt.Sprint(limit))
	qs.Set("start", fmt.Sprint(start))
	if filter != "" {
		qs.Set("filterText", filter)
	}
	data, err := bbDecode[bbPaged[bbBranch]](c, "GET", c.rp(pk, rs)+"/branches?"+qs.Encode(), nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return textResult("No branches found."), nil
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
	return textResult(fmt.Sprintf("%d branch(es)%s:\n%s", len(data.Values), pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n"))), nil
}

func (c *BitbucketClient) getFile(args map[string]any, repoRoot string) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(argString(args, "projectKey"), argString(args, "repoSlug"), repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	qs := ""
	if ref := argString(args, "ref"); ref != "" {
		qs = "?at=" + url.QueryEscape(ref)
	}
	content, err := c.requestText(c.rp(pk, rs) + "/raw/" + encodePath(argString(args, "path")) + qs)
	if err != nil {
		return toolResult{}, err
	}
	const maxChars = 10000
	if len(content) > maxChars {
		return textResult(content[:maxChars] + fmt.Sprintf("\n\n... (truncated, %d more chars)", len(content)-maxChars)), nil
	}
	return textResult(content), nil
}

func encodePath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

func (c *BitbucketClient) fetchFileText(projectKey, repoSlug, filePath string) string {
	content, err := c.requestText(c.rp(projectKey, repoSlug) + "/raw/" + encodePath(filePath))
	if err != nil {
		return ""
	}
	return content
}

var contentDispositionRe = regexp.MustCompile(`(?i)filename\*?=(?:UTF-8'')?"?([^";]+)"?`)

func (c *BitbucketClient) getAttachment(args map[string]any, repoRoot string) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(argString(args, "projectKey"), argString(args, "repoSlug"), repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	id := strings.TrimSpace(argString(args, "attachmentId"))
	if id == "" {
		return toolResult{}, fmt.Errorf("attachmentId is required.")
	}
	saveTo := argString(args, "saveTo")
	timeout := 60 * time.Second
	if saveTo != "" {
		timeout = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	reqURL := c.baseURL + "/rest/api/1.0" + c.rp(pk, rs) + "/attachments/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return toolResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := httpClient.Do(req)
	if err != nil {
		return toolResult{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(res.Body)
		return toolResult{}, fmt.Errorf("%s", formatBitbucketError(res.StatusCode, "GET", c.rp(pk, rs)+"/attachments/"+id, parseBitbucketErrorDetails(string(raw))))
	}

	filename := "attachment-" + id
	if cd := res.Header.Get("content-disposition"); cd != "" {
		if m := contentDispositionRe.FindStringSubmatch(cd); m != nil {
			if dec, derr := url.QueryUnescape(m[1]); derr == nil {
				filename = dec
			} else {
				filename = m[1]
			}
		}
	}
	mimeType := "application/octet-stream"
	if ct := res.Header.Get("content-type"); ct != "" {
		mimeType = strings.TrimSpace(strings.Split(ct, ";")[0])
	}
	var declaredLength int64
	if cl := res.Header.Get("content-length"); cl != "" {
		fmt.Sscanf(cl, "%d", &declaredLength)
	}

	if saveTo != "" {
		path, _ := filepath.Abs(saveTo)
		f, err := os.Create(path)
		if err != nil {
			return toolResult{}, err
		}
		defer f.Close()
		if _, err := io.Copy(f, res.Body); err != nil {
			return toolResult{}, err
		}
		sizeLabel := "unknown size"
		if declaredLength > 0 {
			sizeLabel = formatBytes(declaredLength)
		}
		return textResult(fmt.Sprintf("Saved attachment #%s (%s — %s, %s) to %s", id, filename, mimeType, sizeLabel, path)), nil
	}

	if declaredLength > maxVideoSourceBytes {
		return toolResult{}, fmt.Errorf("Attachment #%s is %s, exceeds the %s inline cap. Pass saveTo=/absolute/path to stream it to disk.", id, formatBytes(declaredLength), formatBytes(maxVideoSourceBytes))
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
		filename:       filename,
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
