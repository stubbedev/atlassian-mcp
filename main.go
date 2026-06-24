package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// Version is the server version, overridable at build time via
// -ldflags "-X main.Version=x.y.z". Kept in sync with package.json.
var Version = "0.5.1"

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
)

func main() {
	config := loadConfig()
	if config.Jira != nil {
		jira = NewJiraClient(config.Jira.URL, config.Jira.Token)
	}
	// Bitbucket tools register whenever Bitbucket is configured — they are no
	// longer gated on the process cwd's git remote. Per-call resolution still
	// validates the remote host when auto-detecting project/repo from a repo.
	if config.Bitbucket != nil {
		bitbucket = NewBitbucketClient(config.Bitbucket.URL, config.Bitbucket.Token)
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

	if addr := httpAddr(); addr != "" {
		runHTTP(addr, instructions)
		return
	}
	runStdio(instructions)
}

// httpAddr returns the HTTP bind address if HTTP mode is requested via
// --http [addr] or ATLASSIAN_MCP_HTTP, else "" (stdio mode). A bare --http or
// empty env value defaults to 127.0.0.1:7337.
func httpAddr() string {
	const def = "127.0.0.1:7337"
	args := os.Args[1:]
	for i, a := range args {
		if a == "--http" {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
			return def
		}
		if strings.HasPrefix(a, "--http=") {
			if v := strings.TrimPrefix(a, "--http="); v != "" {
				return v
			}
			return def
		}
	}
	if v, ok := os.LookupEnv("ATLASSIAN_MCP_HTTP"); ok {
		if v == "" || v == "1" || strings.EqualFold(v, "true") {
			return def
		}
		return v
	}
	return ""
}

// ── stdio transport ──────────────────────────────────────────────────────────

func runStdio(instructions string) {
	stdinReader = bufio.NewReader(os.Stdin)
	stdoutWriter = bufio.NewWriter(os.Stdout)
	session := newStdioSession(clientCaps{})

	for {
		line, err := stdinReader.ReadBytes('\n')
		if len(line) > 0 {
			handleLine(session, line, instructions)
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

func handleLine(session *sessionState, line []byte, instructions string) {
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

	result, rerr := dispatch(session, &req, instructions)

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

// dispatch handles one JSON-RPC request for the given session. Transport-
// agnostic: used by both stdio and HTTP.
func dispatch(session *sessionState, req *rpcRequest, instructions string) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Roots       *json.RawMessage `json:"roots"`
				Elicitation *json.RawMessage `json:"elicitation"`
			} `json:"capabilities"`
		}
		json.Unmarshal(req.Params, &p)
		if session != nil {
			session.caps = clientCaps{
				roots:       p.Capabilities.Roots != nil,
				elicitation: p.Capabilities.Elicitation != nil,
			}
		}
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

	case "notifications/roots/list_changed":
		if session != nil {
			session.invalidateRoots()
		}
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
		return callTool(session, p.Name, p.Arguments)

	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "Method not found: " + req.Method}
	}
}

// callTool dispatches a tools/call. A returned *rpcError is a protocol-level
// error; a tool-execution error is returned as an isError tool result.
func callTool(session *sessionState, name string, rawArgs map[string]any) (any, *rpcError) {
	if rawArgs == nil {
		rawArgs = map[string]any{}
	}
	result, err := runTool(session, name, rawArgs)
	if err != nil {
		if err == errUnknownTool {
			return nil, &rpcError{Code: codeMethodNotFound, Message: "Unknown tool: " + name}
		}
		if re, ok := err.(*rpcError); ok {
			return nil, re
		}
		return toolResult{Content: []contentBlock{{Type: "text", Text: "Error: " + err.Error()}}, IsError: true}, nil
	}
	return result, nil
}

// ── Instructions ─────────────────────────────────────────────────────────────

// buildInstructions runs before any client handshake, so it cannot know the
// client's repo — repo state is surfaced dynamically by get_dev_context.
func buildInstructions(config Config) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

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
	} else {
		bbLine += "(not configured)"
	}
	w(bbLine)
	w("")
	w("## Repo context")
	w("- Tools that need a repo (git_*, get_dev_context, start_work, complete_work, and Bitbucket project/repo auto-detection) resolve it from your MCP workspace roots, or from an explicit `repoPath` argument. Pass `repoPath` (or `projectKey`+`repoSlug` for Bitbucket) when working outside a single known workspace.")
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
