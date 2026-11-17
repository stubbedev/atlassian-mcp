package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/stubbedev/atlassian-mcp/internal/bitbucket"
	"github.com/stubbedev/atlassian-mcp/internal/config"
	"github.com/stubbedev/atlassian-mcp/internal/gitrepo"
	"github.com/stubbedev/atlassian-mcp/internal/logx"
)

// clientCaps holds the client capabilities declared at initialize.
type clientCaps struct {
	roots       bool
	elicitation bool
	// apps reports MCP Apps support, negotiated via the
	// "io.modelcontextprotocol/ui" extension. The apps spec asks servers to
	// check it before offering a UI-backed tool, so attach_files degrades to a
	// text answer on hosts (Claude Code today) that render no widget.
	apps bool
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
	appsSupported() bool
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
	reviews     bitbucket.PRReviews
}

func (s *sessionState) prReviews() *bitbucket.PRReviews {
	if s == nil {
		return nil
	}
	return &s.reviews
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

func (s *sessionState) appsSupported() bool { return s != nil && s.caps.apps }

func (s *sessionState) rootsSupported() bool { return s.caps.roots }

func (s *sessionState) isStdio() bool { return s.stdio }

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
		if p := config.FileURIToPath(r.URI); p != "" {
			list = append(list, rootEntry{uri: r.URI, path: p, name: r.Name})
		}
	}

	// Log what the client actually advertised, once. GUI hosts differ widely in
	// whether they expose a workspace at all, and this is the only way to find
	// out what a given one hands over without attaching a debugger to it.
	if len(list) == 0 {
		logx.Logf("Client advertised no workspace roots; repo tools need ATLASSIAN_MCP_REPO_ROOT or an explicit repoPath.")
	} else {
		paths := make([]string, 0, len(list))
		for _, e := range list {
			paths = append(paths, e.path)
		}
		logx.Logf("Client workspace roots: %s", strings.Join(paths, ", "))
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
		if gitrepo.IsRepo(e.path) {
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
		for part := range strings.SplitSeq(v, ",") {
			part = config.ExpandHome(strings.TrimSpace(part))
			if part == "" {
				continue
			}
			if p := config.FileURIToPath(part); p != "" {
				list = append(list, rootEntry{uri: part, path: p})
			}
		}
	}
	return list
}

// ── elicitation (built on the back-channel) ──────────────────────────────────

type elicitResult struct {
	Action  string         `json:"action"` // accept | decline | cancel
	Content map[string]any `json:"content"`
}

var errNoElicitation = errors.New("client does not support elicitation")

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
