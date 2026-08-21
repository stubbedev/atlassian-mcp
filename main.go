package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Version is defined in version.go (embedded from package.json).

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[atlassian-mcp] "+format+"\n", args...)
}

// ── Errors ───────────────────────────────────────────────────────────────────

// rpcError is a structured error a tool may return for invalid params; the MCP
// layer surfaces it as an isError tool result. Kept as a small typed error so
// validation helpers can carry a code + message.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

const codeInvalidParams = -32602

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

	runServer(instructions)
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

// ── Instructions ─────────────────────────────────────────────────────────────

// buildInstructions runs before any client handshake, so it cannot know the
// client's repo — repo state is surfaced dynamically by get_dev_context.
func buildInstructions(config Config) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	// Identity enrichment ("you are X") is best-effort and must NOT delay the
	// listener: each whoami can block up to 30s when Jira/Bitbucket is slow or
	// unreachable (e.g. VPN not up yet at boot), which would otherwise stall the
	// HTTP server ~60s and make clients fail to connect. Fetch both concurrently
	// and wait only briefly; if they're not back in time, serve instructions
	// without the identity line (the background fetch is harmless and discarded).
	type identity struct {
		jira *jiraCurrentUser
		bb   string
	}
	idCh := make(chan identity, 1)
	go func() {
		var id identity
		var wg sync.WaitGroup
		if jira != nil {
			wg.Add(1)
			go func() { defer wg.Done(); id.jira, _ = jira.whoami() }()
		}
		if bitbucket != nil {
			wg.Add(1)
			go func() { defer wg.Done(); id.bb, _ = bitbucket.whoami() }()
		}
		wg.Wait()
		idCh <- id
	}()
	var who identity
	select {
	case who = <-idCh:
	case <-time.After(3 * time.Second):
	}
	jiraMe := who.jira
	bbMe := who.bb

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
	w("- \"What am I working on / what's the status / show me the context\" → call `get_dev_context` first. It returns branch state, linked Jira tickets, the open PR, and reviewer status in one shot. The same report is also the `dev-context://current` resource — re-read it for fresh state instead of re-calling the tool.")
	w("- Looking up a person's username (for reviewers, assignees, mentions) → ALWAYS use `bitbucket_search resource=users` or `jira_search resource=users`. NEVER use `git log`/`git shortlog`/`gh api`/`bb`/any bitbucket CLI to discover who someone is — those return commit-author strings, not Bitbucket/Jira usernames, and the wrong identifier breaks reviewer assignment.")
	w("- Reading a Jira ticket → `jira_get` (single) or `jira_search` (many). Mutating → `jira_mutate`.")
	w("- A Jira field with no dedicated argument (Epic Name, Story Points, any custom field) → `jira_mutate` `create.customFields` / `update.customFields`, keyed by field name. To learn a screen's fields, required ones, and allowed values → `jira_search resource=fields` with project+issueType or issueKey. Never tell the user to set a field in the Jira UI before trying this.")
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
