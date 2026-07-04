package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file is the MCP protocol layer, built on the official
// modelcontextprotocol/go-sdk. The tool logic (runTool + jira/bitbucket/git)
// and the session's roots/elicitation resolution (sessionState) are unchanged;
// only the transport and the server→client back-channel are the SDK's.

// httpMode is set when serving over HTTP (vs stdio); it controls the stdio cwd
// fallback in repo resolution.
var httpMode bool

// mcpServer is the single server instance; the HTTP handler hands it to every
// request via getServer.
var mcpServer *mcp.Server

// Per-client session registry: maps the SDK session id to our sessionState so
// roots are cached and roots/list_changed invalidation works across a client's
// calls. Built lazily on first tool call; swept when the SDK session ends.
var (
	sessMu   sync.Mutex
	sessions = map[string]*sessionState{}
)

// runServer builds the server and serves it over the configured transport.
func runServer(instructions string) {
	mcpServer = buildServer(instructions)
	if addr := httpAddr(); addr != "" {
		serveHTTP(addr)
		return
	}
	if err := mcpServer.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		logf("stdio server error: %v", err)
		os.Exit(1)
	}
}

// buildServer creates the MCP server and registers the tools applicable to the
// current configuration (toolList gates by configured service).
func buildServer(instructions string) *mcp.Server {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "atlassian-mcp", Version: Version},
		&mcp.ServerOptions{
			Instructions:            instructions,
			HasTools:                true,
			RootsListChangedHandler: handleRootsListChanged,
		},
	)
	for _, raw := range toolList() {
		var t mcp.Tool
		if err := json.Unmarshal(raw, &t); err != nil {
			logf("tool schema parse error: %v", err)
			continue
		}
		if t.InputSchema == nil {
			t.InputSchema = map[string]any{"type": "object"}
		}
		srv.AddTool(&t, toolHandler)
	}
	// dev-context as a resource: same live report as the get_dev_context tool,
	// but a resource the client can pull on demand and re-read for freshness
	// without accreting a tool call in the conversation each time. Repo is
	// resolved per read from the caller's session (roots/headers/cwd), so the
	// static URI serves whatever repo the client is in.
	srv.AddResource(&mcp.Resource{
		URI:         "dev-context://current",
		Name:        "dev-context",
		Title:       "Current dev context",
		Description: "Live branch state, linked Jira tickets, and the open PR for the current repo/branch. Re-read any time for fresh state — same content as the get_dev_context tool.",
		MIMEType:    "text/markdown",
	}, devContextResourceHandler)
	return srv
}

// devContextResourceHandler serves the dev-context resource: it resolves the
// caller's repo the same way the tools do and returns getDevContext's report.
func devContextResourceHandler(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	repo := resolveRepoRoot(sessionFor(req.Session, req.Extra), nil)
	res, err := getDevContext(repo)
	if err != nil {
		return nil, err
	}
	text := ""
	if len(res.Content) > 0 {
		text = res.Content[0].Text
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
		URI:      req.Params.URI,
		MIMEType: "text/markdown",
		Text:     text,
	}}}, nil
}

// toolHandler bridges an SDK tools/call to runTool, building the per-client
// session (roots/elicitation) and converting the result.
func toolHandler(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := map[string]any{}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), nil
		}
	}
	res, err := runTool(sessionFor(req.Session, req.Extra), req.Params.Name, args)
	if err != nil {
		var re *rpcError
		switch {
		case errors.As(err, &re):
			return errResult(re.Message), nil
		case errors.Is(err, errUnknownTool):
			return errResult("Unknown tool: " + req.Params.Name), nil
		default:
			return errResult("Error: " + err.Error()), nil
		}
	}
	return toCallToolResult(res), nil
}

// sessionFor returns the sessionState for the calling client, creating it on
// first use. Header-provided roots (proxy/harness injection) are refreshed each
// call — they are authoritative and may change. Shared by the tool and resource
// handlers, so it takes the session + request extra rather than a specific
// request type.
func sessionFor(ss *mcp.ServerSession, extra *mcp.RequestExtra) *sessionState {
	id := ss.ID()

	sessMu.Lock()
	st := sessions[id]
	if st == nil {
		caps := clientCaps{roots: true}
		if ip := ss.InitializeParams(); ip != nil && ip.Capabilities != nil {
			caps.elicitation = ip.Capabilities.Elicitation != nil
		}
		st = &sessionState{stdio: !httpMode, caps: caps, send: sdkSend(ss)}
		sessions[id] = st
	}
	sessMu.Unlock()

	if extra != nil {
		if roots := rootsFromHeaders(extra.Header); len(roots) > 0 {
			st.setHeaderRoots(roots)
		}
	}
	return st
}

// handleRootsListChanged invalidates the cached roots for the signalling client.
func handleRootsListChanged(_ context.Context, req *mcp.RootsListChangedRequest) {
	if req.Session == nil {
		return
	}
	sessMu.Lock()
	st := sessions[req.Session.ID()]
	sessMu.Unlock()
	if st != nil {
		st.invalidateRoots()
	}
}

// sdkSend wires sessionState's server→client back-channel to the SDK session:
// roots/list → ListRoots, elicitation/create → Elicit. The JSON shapes match
// what loadRoots and elicit already parse, so that logic is reused unchanged.
func sdkSend(ss *mcp.ServerSession) func(method string, params any) (json.RawMessage, error) {
	return func(method string, params any) (json.RawMessage, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		switch method {
		case "roots/list":
			res, err := ss.ListRoots(ctx, &mcp.ListRootsParams{})
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"roots": res.Roots})
		case "elicitation/create":
			p, _ := params.(map[string]any)
			msg, _ := p["message"].(string)
			res, err := ss.Elicit(ctx, &mcp.ElicitParams{Message: msg, RequestedSchema: p["requestedSchema"]})
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"action": res.Action, "content": res.Content})
		}
		return nil, fmt.Errorf("unsupported server→client method: %s", method)
	}
}

// toCallToolResult converts the internal toolResult to the SDK type.
func toCallToolResult(r toolResult) *mcp.CallToolResult {
	content := make([]mcp.Content, 0, len(r.Content))
	for _, c := range r.Content {
		if c.Type == "image" {
			data, err := base64.StdEncoding.DecodeString(c.Data)
			if err != nil {
				content = append(content, &mcp.TextContent{Text: "image decode error: " + err.Error()})
				continue
			}
			content = append(content, &mcp.ImageContent{Data: data, MIMEType: c.MimeType})
		} else {
			content = append(content, &mcp.TextContent{Text: c.Text})
		}
	}
	return &mcp.CallToolResult{Content: content, IsError: r.IsError}
}

func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// ── HTTP transport ───────────────────────────────────────────────────────────

const mcpPath = "/mcp"

// serveHTTP serves the streamable-HTTP transport: the SDK handler at /mcp,
// behind an auth gate, plus an unauthenticated /healthz liveness probe.
func serveHTTP(addr string) {
	httpMode = true
	loopback := isLoopbackAddr(addr)
	token := os.Getenv("ATLASSIAN_MCP_HTTP_TOKEN")
	if !loopback && token == "" {
		logf("Refusing to bind a non-loopback address (%s) without ATLASSIAN_MCP_HTTP_TOKEN set.", addr)
		os.Exit(1)
	}
	authOK := func(r *http.Request) bool {
		if token == "" {
			return true // loopback, no token configured
		}
		return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) == token
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})

	go sweepSessions()

	logf("Listening on http://%s%s (loopback=%v, auth=%v)", addr, mcpPath, loopback, token != "")
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		logf("http server error: %v", err)
		os.Exit(1)
	}
}

// sweepSessions drops registry entries whose SDK session has ended, so a
// long-lived server doesn't leak sessionStates.
func sweepSessions() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		live := map[string]struct{}{}
		for s := range mcpServer.Sessions() {
			live[s.ID()] = struct{}{}
		}
		sessMu.Lock()
		for id := range sessions {
			if _, ok := live[id]; !ok {
				delete(sessions, id)
			}
		}
		sessMu.Unlock()
	}
}

// rootHeaders are the request headers a proxy/harness may set to hand the
// server the workspace root(s) without the MCP roots/list round-trip. Values
// are file:// URIs or absolute paths; multiple roots may be comma-separated.
var rootHeaders = []string{"X-Mcp-Roots", "X-Mcp-Root", "Mcp-Roots", "Mcp-Root"}

func rootsFromHeaders(h http.Header) []rootEntry {
	var list []rootEntry
	for _, name := range rootHeaders {
		for _, v := range h.Values(name) {
			for _, part := range strings.Split(v, ",") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				if p := fileURIToPath(part); p != "" {
					list = append(list, rootEntry{uri: part, path: p})
				}
			}
		}
	}
	return list
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
