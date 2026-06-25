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
	for _, want := range []string{"git_get_context", "get_dev_context", "jira_search", "bitbucket_get_pr", "complete_work"} {
		if !names[want] {
			t.Fatalf("tools/list missing %q; got %v", want, names)
		}
	}

	// A tool call round-trips through the SDK (no transport error). The result
	// may be IsError (the temp dir is not a git repo) — that's expected.
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "git_get_context",
		Arguments: map[string]any{"repoPath": t.TempDir()},
	}); err != nil {
		t.Fatalf("call git_get_context: %v", err)
	}
}
