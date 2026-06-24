package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// Version is the server version, overridable at build time via
// -ldflags "-X main.Version=x.y.z". Kept in sync with package.json.
var Version = "0.4.5"

const defaultProtocolVersion = "2025-06-18"

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[atlassian-mcp] "+format+"\n", args...)
}

// ── JSON-RPC types ───────────────────────────────────────────────────────────

type rpcRequest struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

type rpcResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// ── Tool result types ────────────────────────────────────────────────────────

type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type toolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

func textResult(s string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: s}}}
}

var errUnknownTool = fmt.Errorf("unknown tool")

// ── Globals ──────────────────────────────────────────────────────────────────

var (
	jira      *JiraClient
	bitbucket *BitbucketClient

	stdinReader  *bufio.Reader
	stdoutWriter *bufio.Writer

	clientSupportsElicitation bool
	nextReqID                 = 1
)

func main() {
	config := loadConfig()
	if config.Jira != nil {
		jira = NewJiraClient(config.Jira.URL, config.Jira.Token)
	}
	// Bitbucket is gated on the current repo's origin remote matching the
	// configured instance host (mirrors src/index.ts).
	if config.Bitbucket != nil {
		remote := currentGitRemote()
		if remoteMatchesBitbucketInstance(remote, config.Bitbucket.URL) {
			bitbucket = NewBitbucketClient(config.Bitbucket.URL, config.Bitbucket.Token)
		} else {
			logf("Bitbucket configured but remote %q does not match %s — Bitbucket tools disabled for this repo.", remote, config.Bitbucket.URL)
		}
	}

	if jira == nil && bitbucket == nil {
		logf("No Jira or Bitbucket configuration found. Set jira.{url,token} / bitbucket.{url,token} in ~/.atlassian-mcp.json or JIRA_URL/JIRA_ACCESS_TOKEN/BITBUCKET_URL/BITBUCKET_ACCESS_TOKEN env vars. Only git tools will be available.")
	}

	instructions := buildInstructions(config)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		os.Exit(0)
	}()

	stdinReader = bufio.NewReader(os.Stdin)
	stdoutWriter = bufio.NewWriter(os.Stdout)

	for {
		line, err := stdinReader.ReadBytes('\n')
		if len(line) > 0 {
			handleLine(line, instructions)
			stdoutWriter.Flush()
		}
		if err != nil {
			if err == io.EOF {
				return
			}
			logf("read error: %v", err)
			return
		}
	}
}

func handleLine(line []byte, instructions string) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		logf("parse error: %v", err)
		return
	}
	isNotification := len(req.ID) == 0

	result, rerr := dispatch(&req, instructions)

	if isNotification {
		return
	}
	resp := rpcResponse{Jsonrpc: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	out, _ := json.Marshal(resp)
	stdoutWriter.Write(out)
	stdoutWriter.WriteByte('\n')
}

func dispatch(req *rpcRequest, instructions string) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Elicitation *json.RawMessage `json:"elicitation"`
			} `json:"capabilities"`
		}
		json.Unmarshal(req.Params, &p)
		clientSupportsElicitation = p.Capabilities.Elicitation != nil
		protocol := p.ProtocolVersion
		if protocol == "" {
			protocol = defaultProtocolVersion
		}
		return map[string]any{
			"protocolVersion": protocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "atlassian-mcp", "version": Version},
			"instructions":    instructions,
		}, nil

	case "notifications/initialized", "notifications/cancelled":
		return nil, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		return map[string]any{"tools": toolList()}, nil

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
		}
		return callTool(p.Name, p.Arguments)

	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "Method not found: " + req.Method}
	}
}

// callTool dispatches a tools/call. A returned *rpcError is a protocol-level
// error; a tool-execution error is returned as an isError tool result.
func callTool(name string, rawArgs map[string]any) (any, *rpcError) {
	if rawArgs == nil {
		rawArgs = map[string]any{}
	}
	result, err := runTool(name, rawArgs)
	if err != nil {
		if err == errUnknownTool {
			return nil, &rpcError{Code: codeMethodNotFound, Message: "Unknown tool: " + name}
		}
		var re *rpcError
		if errAs(err, &re) {
			return nil, re
		}
		return toolResult{Content: []contentBlock{{Type: "text", Text: "Error: " + err.Error()}}, IsError: true}, nil
	}
	return result, nil
}

func errAs(err error, target **rpcError) bool {
	if re, ok := err.(*rpcError); ok {
		*target = re
		return true
	}
	return false
}

// ── Elicitation (server → client request) ────────────────────────────────────

type elicitResult struct {
	Action  string         `json:"action"` // accept | decline | cancel
	Content map[string]any `json:"content"`
}

var errNoElicitation = fmt.Errorf("client does not support elicitation")

// elicit sends an elicitation/create request to the client and blocks reading
// stdin until the matching response arrives. Returns errNoElicitation if the
// client did not declare elicitation support at initialize.
func elicit(message string, schema map[string]any) (*elicitResult, error) {
	if !clientSupportsElicitation {
		return nil, errNoElicitation
	}
	id := nextReqID
	nextReqID++
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "elicitation/create",
		"params":  map[string]any{"message": message, "requestedSchema": schema},
	}
	out, _ := json.Marshal(req)
	stdoutWriter.Write(out)
	stdoutWriter.WriteByte('\n')
	stdoutWriter.Flush()

	want := strconv.Itoa(id)
	for {
		line, err := stdinReader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var resp struct {
					ID     json.RawMessage `json:"id"`
					Result *elicitResult   `json:"result"`
					Error  *rpcError       `json:"error"`
				}
				if json.Unmarshal(trimmed, &resp) == nil && len(resp.ID) > 0 && string(resp.ID) == want {
					if resp.Error != nil {
						return nil, resp.Error
					}
					if resp.Result == nil {
						return &elicitResult{Action: "cancel"}, nil
					}
					return resp.Result, nil
				}
				// Any other line during elicitation (notification or unrelated
				// request) is ignored — well-behaved clients block until they
				// answer the elicitation.
			}
		}
		if err != nil {
			return nil, fmt.Errorf("elicitation aborted: %w", err)
		}
	}
}

// ── Instructions ─────────────────────────────────────────────────────────────

func buildInstructions(config Config) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	branch := currentGitBranch()
	isGitRepo := branch != ""
	var jiraKeys []string
	if isGitRepo {
		jiraKeys = uniqueStrings(jiraKeyRe.FindAllString(branch, -1))
	}
	remote := currentGitRemote()
	var parsed *bitbucketRemote
	if bitbucket != nil && remote != "" {
		parsed = parseBitbucketRemote(remote)
	}
	var committers []committer
	if isGitRepo {
		committers = getTopCommitters(mustGetwd(), 50, 5)
	}

	var jiraMe *jiraCurrentUser
	if jira != nil {
		jiraMe, _ = jira.whoami()
	}
	var bbMe string
	if bitbucket != nil {
		bbMe, _ = bitbucket.whoami()
	}

	w("# atlassian-mcp")
	w("")
	w("Self-hosted Jira + Bitbucket Server tooling. Prefer these tools over shelling out to `git log`, `gh`, or any `bitbucket`/`bb` CLI for anything that touches tickets, PRs, reviewers, comments, or user lookups.")
	w("")
	w("## Configured services")
	jiraLine := "- Jira:      "
	if jira != nil {
		jiraLine += config.Jira.URL
		if jiraMe != nil {
			id := jiraMe.Name
			if id == "" {
				id = jiraMe.Key
			}
			if id == "" {
				id = "?"
			}
			jiraLine += " — you are " + id
			if jiraMe.DisplayName != "" {
				jiraLine += " \"" + jiraMe.DisplayName + "\""
			}
		}
	} else {
		jiraLine += "(not configured)"
	}
	w(jiraLine)

	bbLine := "- Bitbucket: "
	if bitbucket != nil {
		bbLine += config.Bitbucket.URL
		if bbMe != "" {
			bbLine += " — you are " + bbMe
		}
	} else if config.Bitbucket != nil {
		bbLine += config.Bitbucket.URL + " — DISABLED for this cwd (remote does not match)"
	} else {
		bbLine += "(not configured)"
	}
	w(bbLine)
	w("")

	w("## Current repo")
	if isGitRepo {
		w(fmt.Sprintf("- Branch: %s (may have changed since startup — re-run `get_dev_context` to refresh)", branch))
		r := remote
		if r == "" {
			r = "(none)"
		}
		w("- Remote: " + r)
		if parsed != nil {
			w(fmt.Sprintf("- Bitbucket repo: %s/%s", parsed.projectKey, parsed.repoSlug))
		}
		if len(jiraKeys) > 0 {
			w("- Jira keys in branch: " + strings.Join(jiraKeys, ", "))
		}
	} else {
		w("- Not a git repository.")
	}

	if len(committers) > 0 {
		w("")
		w("## Recent committers in this repo (last 50 commits)")
		for _, c := range committers {
			ident := c.name
			if c.email != "" {
				ident = c.name + " <" + c.email + ">"
			}
			w(fmt.Sprintf("- %d× %s", c.commits, ident))
		}
	}

	w("")
	w("## Use these tools — do NOT shell out")
	w("- \"What am I working on / what's the status / show me the context\" → call `get_dev_context` first. It returns branch state, linked Jira tickets, the open PR, and reviewer status in one shot.")
	w("- Looking up a person's username (for reviewers, assignees, mentions) → ALWAYS use `bitbucket_search resource=users` or `jira_search resource=users`. NEVER use `git log`/`git shortlog`/`gh api`/`bb`/any bitbucket CLI to discover who someone is — those return commit-author strings, not Bitbucket/Jira usernames, and the wrong identifier breaks reviewer assignment.")
	w("- Reading a Jira ticket → `jira_get` (single) or `jira_search` (many). Mutating → `jira_mutate`.")
	w("- Reading a PR → `bitbucket_get_pr`. Creating/updating/merging → `bitbucket_mutate`. Commenting → `bitbucket_comment`.")
	b.WriteString("- Starting work on a ticket (branch + status transition + README) → `start_work`. Closing it (merge + transition) → `complete_work`.")

	return b.String()
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
