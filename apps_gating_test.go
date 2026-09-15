package main

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectProbe runs a real client against a real server over the in-memory
// transport, so the capability gate is exercised through the protocol rather
// than by calling the middleware directly.
func connectProbe(t *testing.T, caps *mcp.ClientCapabilities) *mcp.ClientSession {
	t.Helper()
	// The session registry is keyed by session id, and in-memory (like stdio)
	// sessions have none — so without this, a second probe in the same process
	// reuses the first one's cached capabilities.
	sessMu.Lock()
	sessions = map[string]*sessionState{}
	sessMu.Unlock()

	srv := buildServer("test instructions")
	var opts *mcp.ClientOptions
	if caps != nil {
		opts = &mcp.ClientOptions{Capabilities: caps}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "1"}, opts)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	go func() { _ = srv.Run(ctx, st) }()
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) map[string]bool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	out := map[string]bool{}
	for _, tool := range res.Tools {
		out[tool.Name] = true
	}
	return out
}

func resourceURIs(t *testing.T, cs *mcp.ClientSession) map[string]bool {
	t.Helper()
	res, err := cs.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	out := map[string]bool{}
	for _, r := range res.Resources {
		out[r.URI] = true
	}
	return out
}

// A host without MCP Apps must never be shown the picker: not the tool, not the
// widget resource, and a direct read of it is refused rather than pouring a few
// hundred KB of inlined SDK into a client that can never render it.
func TestAppsSurfaceHiddenWithoutCapability(t *testing.T) {
	prevJira, prevBB := jira, bitbucket
	jira, bitbucket = &JiraClient{}, &BitbucketClient{}
	defer func() { jira, bitbucket = prevJira, prevBB }()

	cs := connectProbe(t, nil)

	if toolNames(t, cs)["attach_files"] {
		t.Error("attach_files must not be listed without MCP Apps")
	}
	if resourceURIs(t, cs)[uploadWidgetURI] {
		t.Error("the upload widget must not be listed without MCP Apps")
	}
	_, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uploadWidgetURI})
	if err == nil {
		t.Fatal("reading the widget must be refused without MCP Apps")
	}
	if !strings.Contains(err.Error(), "MCP Apps") {
		t.Errorf("refusal should say why, got %v", err)
	}
	// Everything else must be exactly as it was.
	if names := toolNames(t, cs); !names["jira_mutate"] || !names["get_attachment"] {
		t.Error("non-UI tools must be unaffected")
	}
	if !resourceURIs(t, cs)["dev-context://current"] {
		t.Error("the dev-context resource must be unaffected")
	}
}

func TestAppsSurfaceVisibleWithCapability(t *testing.T) {
	prevJira, prevBB := jira, bitbucket
	jira, bitbucket = &JiraClient{}, &BitbucketClient{}
	defer func() { jira, bitbucket = prevJira, prevBB }()

	caps := &mcp.ClientCapabilities{}
	caps.AddExtension(uiExtensionKey, map[string]any{"mimeTypes": []any{appResourceMIMEType}})
	cs := connectProbe(t, caps)

	if !toolNames(t, cs)["attach_files"] {
		t.Error("attach_files must be listed for an MCP Apps host")
	}
	if !resourceURIs(t, cs)[uploadWidgetURI] {
		t.Error("the upload widget must be listed for an MCP Apps host")
	}
	res, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uploadWidgetURI})
	if err != nil {
		t.Fatalf("reading the widget: %v", err)
	}
	if got := res.Contents[0].MIMEType; got != appResourceMIMEType {
		t.Errorf("widget MIME = %q, want %q", got, appResourceMIMEType)
	}
	if !strings.Contains(res.Contents[0].Text, "ui/initialize") {
		t.Error("served widget is missing its MCP Apps bridge")
	}
}
