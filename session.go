package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// clientCaps holds the client capabilities declared at initialize.
type clientCaps struct {
	roots       bool
	elicitation bool
}

// Session represents one client connection. It provides the server→client
// request channel (used for roots/list and elicitation) and resolves the
// client's workspace repo root on demand.
type Session interface {
	// sendRequest issues a server→client JSON-RPC request and blocks for the
	// matching response, returning the raw `result` (or an error).
	sendRequest(method string, params any) (json.RawMessage, error)
	elicitationSupported() bool
	rootsSupported() bool
	// repoRoot returns the session's primary workspace path (first git-repo root
	// from roots/list), cached for the session; "" if unavailable.
	repoRoot() string
	// resolveRepo resolves a tool's target repo: an explicit repoPath arg
	// (absolute as-is; relative/basename matched against the session roots), or
	// — when empty — the primary root. "" if nothing resolves.
	resolveRepo(repoPathArg string) string
	invalidateRoots()
	isStdio() bool
}

// sessionState is the single concrete Session; stdio and HTTP differ only in
// the send closure they supply.
type sessionState struct {
	send  func(method string, params any) (json.RawMessage, error)
	caps  clientCaps
	stdio bool

	mu          sync.Mutex
	rootsDone   bool
	headerRoots bool // roots pinned via request header — authoritative
	roots       []rootEntry
}

// rootEntry is one client workspace root (a worktree).
type rootEntry struct {
	uri  string
	path string
	name string
}

func (s *sessionState) sendRequest(method string, params any) (json.RawMessage, error) {
	return s.send(method, params)
}
func (s *sessionState) elicitationSupported() bool { return s.caps.elicitation }
func (s *sessionState) rootsSupported() bool       { return s.caps.roots }
func (s *sessionState) isStdio() bool              { return s.stdio }

func (s *sessionState) invalidateRoots() {
	s.mu.Lock()
	if !s.headerRoots { // header-pinned roots are authoritative
		s.rootsDone = false
		s.roots = nil
	}
	s.mu.Unlock()
}

// setHeaderRoots pins workspace roots supplied via a request header (proxy
// injection). They are authoritative: they satisfy loadRoots without a
// roots/list round-trip, work even when the client did not advertise the roots
// capability, and survive list_changed invalidation.
func (s *sessionState) setHeaderRoots(list []rootEntry) {
	s.mu.Lock()
	s.roots = list
	s.rootsDone = true
	s.headerRoots = true
	s.mu.Unlock()
}

// loadRoots returns the session's workspace roots, querying roots/list once and
// caching the result. The round-trip is issued WITHOUT holding the lock — it
// blocks on the client (HTTP back-channel, up to 120s) and must not stall other
// callers; concurrent first-callers may duplicate the harmless query.
func (s *sessionState) loadRoots() []rootEntry {
	// Cached (incl. header-pinned) roots return first — header roots work even
	// when the client never advertised the roots capability.
	s.mu.Lock()
	if s.rootsDone {
		r := s.roots
		s.mu.Unlock()
		return r
	}
	s.mu.Unlock()
	if !s.caps.roots {
		return nil
	}

	raw, err := s.send("roots/list", nil)
	if err != nil {
		return nil // transient — leave rootsDone unset so a later call retries
	}
	var res struct {
		Roots []struct {
			URI  string `json:"uri"`
			Name string `json:"name"`
		} `json:"roots"`
	}
	_ = json.Unmarshal(raw, &res)
	var list []rootEntry
	for _, r := range res.Roots {
		if p := fileURIToPath(r.URI); p != "" {
			list = append(list, rootEntry{uri: r.URI, path: p, name: r.Name})
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rootsDone { // another concurrent caller already resolved
		return s.roots
	}
	s.roots = list
	s.rootsDone = true
	return list
}

// primaryRoot picks the first git-repo root, else the first root dir.
func primaryRoot(list []rootEntry) string {
	var first string
	for _, e := range list {
		if first == "" {
			first = e.path
		}
		if isGitRepo(e.path) {
			return e.path
		}
	}
	return first
}

func (s *sessionState) repoRoot() string { return primaryRoot(s.loadRoots()) }

func (s *sessionState) resolveRepo(repoPathArg string) string {
	if repoPathArg != "" {
		if filepath.IsAbs(repoPathArg) {
			return repoPathArg
		}
		// Relative or basename — match against the session's worktree roots.
		base := filepath.Base(repoPathArg)
		for _, e := range s.loadRoots() {
			if e.path == repoPathArg || strings.HasSuffix(e.path, "/"+repoPathArg) ||
				filepath.Base(e.path) == base || (e.name != "" && e.name == repoPathArg) {
				return e.path
			}
		}
		return repoPathArg // best effort — let git report if it's not a repo
	}
	return primaryRoot(s.loadRoots())
}

// fileURIToPath converts a file:// URI to a local filesystem path. Returns ""
// for non-file URIs.
func fileURIToPath(uri string) string {
	if uri == "" {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		// Some clients send bare paths; accept absolute ones.
		if len(uri) > 0 && uri[0] == '/' {
			return uri
		}
		return ""
	}
	p := u.Path
	// Windows: file:///C:/x → /C:/x → C:/x
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return p
}

// ── server→client request id ─────────────────────────────────────────────────

var reqIDCounter int64

func nextRequestID() string {
	return strconv.FormatInt(atomic.AddInt64(&reqIDCounter, 1), 10)
}

// ── stdio session ────────────────────────────────────────────────────────────

func newStdioSession(caps clientCaps) *sessionState {
	return &sessionState{stdio: true, caps: caps, send: stdioSendRequest}
}

// stdioSendRequest writes the request to stdout then reads stdin until the
// matching response arrives. Safe because the stdio loop is single-threaded:
// it is only ever called from within a tool handler, mid-read.
func stdioSendRequest(method string, params any) (json.RawMessage, error) {
	id := nextRequestID()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	out, _ := json.Marshal(req)
	stdoutWriter.Write(out)
	stdoutWriter.WriteByte('\n')
	stdoutWriter.Flush()

	for {
		line, err := stdinReader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var resp struct {
					ID     json.RawMessage `json:"id"`
					Result json.RawMessage `json:"result"`
					Error  *rpcError       `json:"error"`
				}
				if json.Unmarshal(trimmed, &resp) == nil && len(resp.ID) > 0 && trimQuotes(string(resp.ID)) == id {
					if resp.Error != nil {
						return nil, resp.Error
					}
					return resp.Result, nil
				}
				// Any other line during a server→client request (notification or
				// unrelated request) is ignored — well-behaved clients answer first.
			}
		}
		if err != nil {
			return nil, fmt.Errorf("server request aborted: %w", err)
		}
	}
}

// trimQuotes strips surrounding quotes from a JSON id token so numeric ("5")
// and string ("\"5\"") ids compare equal to our string id.
func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// ── elicitation (built on the back-channel) ──────────────────────────────────

type elicitResult struct {
	Action  string         `json:"action"` // accept | decline | cancel
	Content map[string]any `json:"content"`
}

var errNoElicitation = fmt.Errorf("client does not support elicitation")

func elicit(s Session, message string, schema map[string]any) (*elicitResult, error) {
	if s == nil || !s.elicitationSupported() {
		return nil, errNoElicitation
	}
	raw, err := s.sendRequest("elicitation/create", map[string]any{"message": message, "requestedSchema": schema})
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return &elicitResult{Action: "cancel"}, nil
	}
	var r elicitResult
	if json.Unmarshal(raw, &r) != nil {
		return &elicitResult{Action: "cancel"}, nil
	}
	return &r, nil
}
