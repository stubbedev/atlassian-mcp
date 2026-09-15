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
			Instructions: instructions,
			Capabilities: &mcp.ServerCapabilities{
				Tools: &mcp.ToolCapabilities{ListChanged: true},
			},
			RootsListChangedHandler: handleRootsListChanged, //nolint:staticcheck // SEP-2577 deprecation window; roots stay this server's repo-discovery path until a replacement lands
		},
	)
	// attach_files is a UI tool: on a host that renders no widget it would be a
	// dead end, so it is filtered out of tools/list per session rather than
	// merely failing when called. One server serves many sessions over HTTP, so
	// this cannot be decided at registration time.
	srv.AddReceivingMiddleware(hideAppOnlyTools)

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

	// The MCP Apps upload panel. Hosts that implement MCP Apps (Claude Desktop
	// does, for MCPB-packaged servers too) fetch this when attach_files runs and
	// render it as an iframe in the chat; hosts that do not simply ignore the
	// tool's _meta and the tool still works with paths and URLs.
	if jira != nil || bitbucket != nil {
		srv.AddResource(&mcp.Resource{
			URI:         uploadWidgetURI,
			Name:        "upload-panel",
			Title:       "Attach files",
			Description: "File picker, drop target and paste handler for attaching files to a Jira issue or Bitbucket PR. Rendered by attach_files.",
			MIMEType:    appResourceMIMEType,
		}, uploadWidgetHandler)
	}
	return srv
}

// appResourceMIMEType marks an HTML resource as an MCP Apps UI surface
// (ext-apps 2026-01-26). The profile parameter is what tells a host to render
// the resource in an iframe instead of showing it as text.
const appResourceMIMEType = "text/html;profile=mcp-app"

func uploadWidgetHandler(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
		URI:      req.Params.URI,
		MIMEType: appResourceMIMEType,
		Text:     uploadWidget(),
	}}}, nil
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
	firstCallMeta.Do(func() {
		if len(req.Params.Meta) > 0 {
			logf("First tools/call _meta: %s", compactJSON(map[string]any(req.Params.Meta)))
		} else {
			logf("First tools/call carried no _meta — the host sends no per-call conversation identifier.")
		}
	})
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
			caps.apps = appsCapability(ip.Capabilities.Extensions)
		}
		st = &sessionState{stdio: !httpMode, caps: caps, send: sdkSend(ss)}
		sessions[id] = st
		logClientIdentity(ss, id)
	}
	sessMu.Unlock()

	if extra != nil {
		if roots := rootsFromHeaders(extra.Header); len(roots) > 0 {
			st.setHeaderRoots(roots)
		}
	}
	return st
}

// appOnlyTools are tools that do nothing useful without an MCP Apps host.
var appOnlyTools = map[string]bool{"attach_files": true}

// hideAppOnlyTools keeps the MCP Apps surface out of sight on hosts that cannot
// render it: the picker tool is dropped from tools/list, and the widget is
// dropped from resources/list and refused by resources/read — a client that
// fetched it anyway would pour a few hundred KB of inlined SDK into its context
// for a page it can never draw.
func hideAppOnlyTools(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch method {
		case "tools/list", "resources/list", "resources/read":
		default:
			return next(ctx, method, req)
		}
		ss, _ := req.GetSession().(*mcp.ServerSession)
		if ss != nil && sessionFor(ss, nil).appsSupported() {
			return next(ctx, method, req)
		}
		// Refuse the read before it runs, rather than filtering the payload out.
		if method == "resources/read" {
			if p, ok := req.GetParams().(*mcp.ReadResourceParams); ok && p != nil && p.URI == uploadWidgetURI {
				return nil, fmt.Errorf("resource %s is only available to clients that support MCP Apps", uploadWidgetURI)
			}
			return next(ctx, method, req)
		}
		res, err := next(ctx, method, req)
		if err != nil {
			return res, err
		}
		// Copy rather than mutate: results may be shared across sessions.
		switch listed := res.(type) {
		case *mcp.ListToolsResult:
			kept := make([]*mcp.Tool, 0, len(listed.Tools))
			for _, t := range listed.Tools {
				if t != nil && appOnlyTools[t.Name] {
					continue
				}
				kept = append(kept, t)
			}
			out := *listed
			out.Tools = kept
			return &out, nil
		case *mcp.ListResourcesResult:
			kept := make([]*mcp.Resource, 0, len(listed.Resources))
			for _, r := range listed.Resources {
				if r != nil && r.URI == uploadWidgetURI {
					continue
				}
				kept = append(kept, r)
			}
			out := *listed
			out.Resources = kept
			return &out, nil
		}
		return res, err
	}
}

// logClientIdentity records, once per session, everything the client told us
// about itself. Whether a host can say *which conversation* a call came from
// decides whether a server can reach that conversation's attached files on
// disk, and no host documents what it sends — so log it and look.
// See anthropics/claude-code#41836: clientInfo is generic and no conversation
// id is sent today, which is why the upload panel exists.
func logClientIdentity(ss *mcp.ServerSession, id string) {
	name, version, meta := "unknown", "", map[string]any(nil)
	if ip := ss.InitializeParams(); ip != nil {
		if ip.ClientInfo != nil {
			name, version = ip.ClientInfo.Name, ip.ClientInfo.Version
		}
		meta = ip.Meta
	}
	cwd, _ := os.Getwd()
	apps := "no (attach_files and the upload panel are hidden)"
	if ip := ss.InitializeParams(); ip != nil && ip.Capabilities != nil && appsCapability(ip.Capabilities.Extensions) {
		apps = "yes"
	}
	logf("Client: %s %s (mcp session id %q, transport %s, cwd %s)", name, version, id, transportLabel(), cwd)
	logf("Client MCP Apps support: %s", apps)
	if len(meta) > 0 {
		logf("Client initialize _meta: %s", compactJSON(meta))
	} else {
		logf("Client initialize _meta: none — no conversation identifier offered.")
	}
}

func transportLabel() string {
	if httpMode {
		return "http"
	}
	return "stdio"
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// firstCallMeta reports the per-call _meta once per process. If a host ever
// does pass a conversation id, this is where it would show up.
var firstCallMeta sync.Once

// uiExtensionKey is how a host advertises MCP Apps support at initialize
// (apps spec 2026-01-26, "Client<>Server Capability Negotiation").
const uiExtensionKey = "io.modelcontextprotocol/ui"

// appsCapability reports whether the client can render this server's UI
// resources: it must name the extension AND accept the app MIME type.
func appsCapability(extensions map[string]any) bool {
	settings, ok := extensions[uiExtensionKey].(map[string]any)
	if !ok {
		return false
	}
	types, ok := settings["mimeTypes"].([]any)
	if !ok {
		return false
	}
	for _, t := range types {
		if s, ok := t.(string); ok && strings.EqualFold(strings.TrimSpace(s), appResourceMIMEType) {
			return true
		}
	}
	return false
}

// handleRootsListChanged invalidates the cached roots for the signaling client.
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
			if !rootsUsable(ss) {
				return nil, errRootsUnavailable
			}
			res, err := ss.ListRoots(ctx, &mcp.ListRootsParams{}) //nolint:staticcheck // SEP-2577 deprecation window; roots stay this server's repo-discovery path until a replacement lands
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
// X-Repo-Root leads: it is the name the rest of this fleet reads and the one
// the Claude Code entries send, and headers are the only workspace signal that
// survives MCP 2026-07-28 (see resolveRoots).
var rootHeaders = []string{"X-Repo-Root", "X-Mcp-Roots", "X-Mcp-Root", "Mcp-Roots", "Mcp-Root"}

func rootsFromHeaders(h http.Header) []rootEntry {
	list := make([]rootEntry, 0, len(rootHeaders))
	for _, name := range rootHeaders {
		list = append(list, parseRootList(h.Values(name)...)...)
	}
	if len(list) == 0 {
		return nil
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

// rootsRemovedFrom is the first protocol revision that forbids server-initiated
// JSON-RPC requests (SEP-2322 / SEP-2575): from there on a server cannot ask a
// client for its roots. Such clients pin the workspace with one of the
// rootHeaders instead. ISO dates compare correctly as strings.
const rootsRemovedFrom = "2026-07-28"

// errRootsUnavailable is what the back-channel reports instead of attempting a
// call the protocol forbids; callers already fall back to header roots and the
// process cwd.
var errRootsUnavailable = errors.New(
	"client cannot be asked for roots on this protocol version: send an X-Repo-Root header or pass the repo explicitly",
)

// rootsUsable reports whether this session may still be asked for its roots:
// the client advertised the capability, on a protocol version that still allows
// the question.
func rootsUsable(ss *mcp.ServerSession) bool {
	if ss == nil {
		return false
	}
	return rootsAllowed(ss.InitializeParams())
}

// rootsAllowed is rootsUsable's decision, split out so it can be tested without
// a live session.
func rootsAllowed(ip *mcp.InitializeParams) bool {
	if ip == nil || ip.Capabilities == nil || ip.ProtocolVersion >= rootsRemovedFrom {
		return false
	}
	return ip.Capabilities.RootsV2 != nil //nolint:staticcheck // SEP-2577 deprecation window; roots stay this server's repo-discovery path until a replacement lands
}
