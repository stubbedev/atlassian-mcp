package main

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSDKServerSmoke exercises the full MCP path on the official SDK in-process:
// initialize handshake, tools/list (gated by configured services), and a
// tools/call round-trip. It replaces the old stdio/JSON-RPC smoke.
func TestSDKServerSmoke(t *testing.T) {
	jira = NewJiraClient("https://example.com", "tok")
	bitbucket = NewBitbucketClient("https://example.com", "tok")
	t.Cleanup(func() { jira = nil; bitbucket = nil })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := buildServer("smoke instructions")
	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	// git (always), get_dev_context (either service), jira/bitbucket groups,
	// complete_work (both services) — all expected with both configured.
	for _, want := range []string{"git_get_context", "get_dev_context", "get_attachment", "jira_search", "bitbucket_get_pr", "complete_work"} {
		if !names[want] {
			t.Fatalf("tools/list missing %q; got %v", want, names)
		}
	}
	// Consolidated away — a client that still calls them must get an error, not
	// a silent no-op.
	for _, gone := range []string{"git_get_diff", "jira_comment", "jira_version", "jira_get_attachment", "bitbucket_get_attachment"} {
		if names[gone] {
			t.Fatalf("tools/list still advertises %q", gone)
		}
	}

	// Annotations survive to the client: they are how a host decides whether a
	// call needs confirmation.
	for _, tl := range tools.Tools {
		if tl.Annotations == nil || tl.Annotations.Title == "" {
			t.Fatalf("tool %q reached the client without annotations", tl.Name)
		}
	}

	// Argument validation reaches the wire: an unknown enum value is refused
	// before the handler runs.
	bad, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "bitbucket_pr_tasks",
		Arguments: map[string]any{"prId": 1, "action": "close"},
	})
	if err != nil {
		t.Fatalf("call bitbucket_pr_tasks: %v", err)
	}
	if !bad.IsError {
		t.Fatal("action=close should be an error result")
	}

	// A tool call round-trips through the SDK (no transport error). The result
	// may be IsError (the temp dir is not a git repo) — that's expected.
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "git_get_context",
		Arguments: map[string]any{"repoPath": t.TempDir()},
	}); err != nil {
		t.Fatalf("call git_get_context: %v", err)
	}

	// The dev-context resource is advertised and reads back. Repo resolves from
	// the stdio cwd fallback (this repo), so the report is non-empty.
	resList, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("list resources: %v", err)
	}
	var devCtxURI string
	for _, r := range resList.Resources {
		if r.Name == "dev-context" {
			devCtxURI = r.URI
		}
	}
	if devCtxURI == "" {
		t.Fatalf("resources/list missing dev-context; got %v", resList.Resources)
	}
	rr, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: devCtxURI})
	if err != nil {
		t.Fatalf("read dev-context: %v", err)
	}
	if len(rr.Contents) == 0 || rr.Contents[0].Text == "" {
		t.Fatalf("dev-context resource empty: %+v", rr.Contents)
	}
}
