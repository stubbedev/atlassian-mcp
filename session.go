package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// reviewed records the commit pair last shown to this client for a PR, so an
	// inline comment anchors to the state that was actually reviewed instead of
	// whatever the PR head has become. Keyed by project/repo/PR.
	reviewed map[string]reviewedState
}

// reviewedState is the diff a client was last shown for one PR.
type reviewedState struct {
	fromHash string
	toHash   string
}

// rememberReviewed stores the commit pair a client has just been shown.
func (s *sessionState) rememberReviewed(key, fromHash, toHash string) {
	if s == nil || key == "" || fromHash == "" || toHash == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reviewed == nil {
		s.reviewed = map[string]reviewedState{}
	}
	s.reviewed[key] = reviewedState{fromHash: fromHash, toHash: toHash}
}

// lastReviewed returns the commit pair this client was last shown for a PR.
func (s *sessionState) lastReviewed(key string) (string, string, bool) {
	if s == nil {
		return "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.reviewed[key]
	return st.fromHash, st.toHash, ok
}

// reviewKey scopes remembered state to one PR in one repository.
func reviewKey(projectKey, repoSlug string, prID int) string {
	return fmt.Sprintf("%s/%s#%d", projectKey, repoSlug, prID)
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

// envRoots holds workspace roots pinned via ATLASSIAN_MCP_REPO_ROOT. GUI
// desktop clients (Claude Desktop, LM Studio, …) expose no MCP roots and start
// the server with cwd "/", so this env var is the only workspace signal they
// have. Comma-separate for several worktrees.
var envRoots = sync.OnceValue(func() []rootEntry {
	return parseRootList(os.Getenv("ATLASSIAN_MCP_REPO_ROOT"))
})

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
	// Env-pinned roots are as authoritative as header roots: a client that gave
	// us neither roots nor a header still gets repo tools.
	if list := envRoots(); len(list) > 0 {
		s.setHeaderRoots(list)
		return list
	}
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

// parseRootList turns comma-separated file:// URIs or filesystem paths
// (absolute, ~-prefixed, or Windows drive paths) into root entries. Shared by
// the root request headers and ATLASSIAN_MCP_REPO_ROOT.
func parseRootList(values ...string) []rootEntry {
	var list []rootEntry
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = expandHome(strings.TrimSpace(part))
			if part == "" {
				continue
			}
			if p := fileURIToPath(part); p != "" {
				list = append(list, rootEntry{uri: part, path: p})
			}
		}
	}
	return list
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
		// Windows drive path (C:\repo, C:/repo) — url.Parse reads "C:" as a
		// scheme, so it never reaches the file:// branch above.
		if len(uri) >= 3 && uri[1] == ':' && (uri[2] == '\\' || uri[2] == '/') {
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
