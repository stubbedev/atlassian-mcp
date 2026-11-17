package gitrepo

import (
	"bytes"
	"container/heap"
	"crypto/sha1" //nolint:gosec // git's index checksum is SHA-1
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/stubbedev/atlassian-mcp/internal/config"
)

// Repository access goes through go-git, so the server needs no git binary:
// everything a tool reads (branch, remote, log, status, diff) and the few
// things it changes (fetch, checkout, branch, push) happen in-process.

// Allowlist for git refs (commits, branches used as refs in diff commands).
var safeRefRe = regexp.MustCompile(`^[a-zA-Z0-9/_.\-@{}~^:]+(\.\.\.[a-zA-Z0-9/_.\-@{}~^:]+)?$`)

// SafeBranchRe allowlists branch names (stricter than refs — no range syntax).
var SafeBranchRe = regexp.MustCompile(`^[a-zA-Z0-9/_.\-]+$`)

// Abbreviated hashes are fixed-width: long enough to stay unambiguous in any
// repository these tools see, and stable across calls.
const shortHashLen = 10

// Open opens the repository containing path, walking up like git does and
// following a linked worktree's .git file to its common directory.
func Open(path string) (*git.Repository, error) {
	if path == "" {
		return nil, errors.New("no repository path")
	}
	r, err := git.PlainOpenWithOptions(path, &git.PlainOpenOptions{DetectDotGit: true, EnableDotGitCommonDir: true})
	if err != nil {
		return nil, err
	}
	fsStorage, ok := r.Storer.(*filesystem.Storage)
	if !ok {
		return r, nil
	}
	wt, err := r.Worktree()
	if err != nil {
		return r, nil // bare repository: no index to read
	}
	return git.Open(skipHashStorage{fsStorage}, wt.Filesystem)
}

// skipHashStorage reads indexes written with index.skipHash (on by default
// under feature.manyFiles), whose trailing checksum git leaves zeroed and
// go-git would otherwise reject as corrupt.
type skipHashStorage struct{ *filesystem.Storage }

func (s skipHashStorage) Index() (*index.Index, error) {
	idx, err := s.Storage.Index()
	if !errors.Is(err, index.ErrInvalidChecksum) {
		return idx, err
	}
	f, err := s.Filesystem().Open("index")
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	const hashLen = sha1.Size
	if len(data) < hashLen || !bytes.Equal(data[len(data)-hashLen:], make([]byte, hashLen)) {
		return nil, index.ErrInvalidChecksum
	}
	sum := sha1.Sum(data[:len(data)-hashLen]) //nolint:gosec // git's index checksum is SHA-1
	copy(data[len(data)-hashLen:], sum[:])
	idx = &index.Index{Version: 2}
	if err := index.NewDecoder(bytes.NewReader(data)).Decode(idx); err != nil {
		return nil, err
	}
	return idx, nil
}

func IsRepo(repoPath string) bool {
	_, err := Open(repoPath)
	return err == nil
}

func NotARepo(repoPath string, err error) error {
	return fmt.Errorf("Not a git repository: %s: %w", repoPath, err)
}

// HeadBranch is the checked-out branch's short name, "HEAD" when detached,
// or "" when HEAD cannot be read.
func HeadBranch(r *git.Repository) string {
	ref, err := r.Reference(plumbing.HEAD, false)
	if err != nil {
		return ""
	}
	if ref.Type() == plumbing.SymbolicReference {
		return ref.Target().Short()
	}
	return "HEAD"
}

// CurrentBranch is headBranch for a path; "" when it is not a repository.
func CurrentBranch(repoPath string) string {
	r, err := Open(repoPath)
	if err != nil {
		return ""
	}
	return HeadBranch(r)
}

// RemoteURL returns the first URL of the named remote, with the user's global
// url.<base>.insteadOf rules applied the way git applies them.
func RemoteURL(r *git.Repository, name string) string {
	rem, err := r.Remote(name)
	if err != nil || len(rem.Config().URLs) == 0 {
		return ""
	}
	u := rem.Config().URLs[0]
	if global, err := gitconfig.LoadConfig(gitconfig.GlobalScope); err == nil {
		u = applyInsteadOf(u, global.URLs)
	}
	return u
}

// applyInsteadOf rewrites u by the longest matching insteadOf prefix.
func applyInsteadOf(u string, rules map[string]*gitconfig.URL) string {
	best, bestBase := "", ""
	for _, rule := range rules {
		if rule.InsteadOf != "" && strings.HasPrefix(u, rule.InsteadOf) && len(rule.InsteadOf) > len(best) {
			best, bestBase = rule.InsteadOf, rule.Name
		}
	}
	if best == "" {
		return u
	}
	return bestBase + u[len(best):]
}

// OriginURL is the origin remote's URL, or "" when there is none.
func OriginURL(repoPath string) string {
	r, err := Open(repoPath)
	if err != nil {
		return ""
	}
	return RemoteURL(r, "origin")
}

func shortHash(h plumbing.Hash) string { return h.String()[:shortHashLen] }

func commitSubject(c *object.Commit) string {
	subject, _, _ := strings.Cut(strings.TrimSpace(c.Message), "\n")
	return strings.TrimSpace(subject)
}

// OnelineLog lists the last n commits reachable from HEAD as
// "<short hash> <subject>" lines, newest first.
func OnelineLog(r *git.Repository, n int) string {
	var lines []string
	_ = walkLog(r, n, func(c *object.Commit) {
		lines = append(lines, shortHash(c.Hash)+" "+commitSubject(c))
	})
	return strings.Join(lines, "\n")
}

// walkLog visits up to n commits from HEAD in the order git log shows them.
func walkLog(r *git.Repository, n int, visit func(*object.Commit)) error {
	head, err := r.Head()
	if err != nil {
		return err
	}
	iter, err := r.Log(&git.LogOptions{From: head.Hash(), Order: git.LogOrderCommitterTime})
	if err != nil {
		return err
	}
	defer iter.Close()
	count := 0
	return iter.ForEach(func(c *object.Commit) error {
		if count >= n {
			return storer.ErrStop
		}
		count++
		visit(c)
		return nil
	})
}

type CommitAuthor struct{ Name, Email string }

// RecentAuthors returns the authors of the last n commits, newest first,
// mapped through the repository's .mailmap like git's %aN/%aE.
func RecentAuthors(r *git.Repository, n int) []CommitAuthor {
	mm := loadMailmap(r)
	var out []CommitAuthor
	_ = walkLog(r, n, func(c *object.Commit) {
		name, email := mm.resolve(c.Author.Name, c.Author.Email)
		out = append(out, CommitAuthor{name, email})
	})
	return out
}

// ── upstream, ahead/behind ───────────────────────────────────────────────────

// upstreamOf returns the tracking ref of a local branch ("origin/main") as git
// shows it for @{u}, or "" when the branch has no upstream.
func upstreamOf(r *git.Repository, branch string) (display string, ref plumbing.ReferenceName) {
	cfg, err := r.Config()
	if err != nil {
		return "", ""
	}
	b, ok := cfg.Branches[branch]
	if !ok || b.Remote == "" || b.Merge == "" {
		return "", ""
	}
	if b.Remote == "." {
		return b.Merge.Short(), b.Merge
	}
	short := strings.TrimPrefix(b.Merge.String(), "refs/heads/")
	return b.Remote + "/" + short, plumbing.NewRemoteReferenceName(b.Remote, short)
}

// UpstreamLine describes how HEAD's branch relates to its upstream, e.g.
// "origin/main (2 ahead, 1 behind)", or "" when there is no upstream.
func UpstreamLine(r *git.Repository) string {
	display, refName := upstreamOf(r, HeadBranch(r))
	if display == "" {
		return ""
	}
	up, err := r.Reference(refName, true)
	if err != nil {
		return ""
	}
	head, err := r.Head()
	if err != nil {
		return ""
	}
	behind, ahead, _, err := compareCommits(r, up.Hash(), head.Hash())
	if err != nil {
		return ""
	}
	var p []string
	if ahead != 0 {
		p = append(p, fmt.Sprintf("%d ahead", ahead))
	}
	if behind != 0 {
		p = append(p, fmt.Sprintf("%d behind", behind))
	}
	if len(p) == 0 {
		return display + " (up to date)"
	}
	return display + " (" + strings.Join(p, ", ") + ")"
}

const (
	sideLeft uint8 = 1 << iota
	sideRight
	sideStale
)

type commitQueue []*object.Commit

func (q *commitQueue) Len() int { return len(*q) }

func (q *commitQueue) Less(i, j int) bool {
	return (*q)[i].Committer.When.After((*q)[j].Committer.When)
}

func (q *commitQueue) Swap(i, j int) { (*q)[i], (*q)[j] = (*q)[j], (*q)[i] }

func (q *commitQueue) Push(x any) { *q = append(*q, x.(*object.Commit)) }

func (q *commitQueue) Pop() any {
	old := *q
	c := old[len(old)-1]
	*q = old[:len(old)-1]
	return c
}

// compareCommits walks both histories newest-first, as git's merge-base search
// does, and stops once everything left to visit is shared. It returns the
// commits only left reaches, only right reaches, and the merge bases (most
// recent first) — `git rev-list --left-right --count left...right` plus
// `git merge-base --all`.
func compareCommits(r *git.Repository, left, right plumbing.Hash) (onlyLeft, onlyRight int, bases []plumbing.Hash, err error) {
	flags := map[plumbing.Hash]uint8{}
	q := &commitQueue{}
	live := 0 // queued commits not yet known to be shared
	add := func(h plumbing.Hash, f uint8) error {
		old, queued := flags[h]
		flags[h] = old | f
		if queued {
			if old&sideStale == 0 && f&sideStale != 0 {
				live--
			}
			return nil
		}
		c, err := r.CommitObject(h)
		if err != nil {
			return err
		}
		heap.Push(q, c)
		if f&sideStale == 0 {
			live++
		}
		return nil
	}
	if err := add(left, sideLeft); err != nil {
		return 0, 0, nil, err
	}
	if err := add(right, sideRight); err != nil {
		return 0, 0, nil, err
	}
	for live > 0 {
		c := heap.Pop(q).(*object.Commit)
		f := flags[c.Hash]
		if f&sideStale == 0 {
			live--
		}
		switch f & (sideLeft | sideRight) {
		case sideLeft | sideRight:
			if f&sideStale == 0 {
				bases = append(bases, c.Hash)
				f |= sideStale
				flags[c.Hash] = f
			}
		case sideLeft:
			onlyLeft++
		case sideRight:
			onlyRight++
		}
		for _, p := range c.ParentHashes {
			// A shallow clone ends history early; git treats the cut as a root.
			if err := add(p, f); err != nil && !errors.Is(err, plumbing.ErrObjectNotFound) {
				return 0, 0, nil, err
			}
		}
	}
	return onlyLeft, onlyRight, bases, nil
}

// ── ref resolution ───────────────────────────────────────────────────────────

// resolveCommit resolves a revision the way git's diff arguments do: names,
// short hashes, ~/^ suffixes, and @{u} / @{upstream} for tracking branches.
func resolveCommit(r *git.Repository, rev string) (*object.Commit, error) {
	if rev == "@" {
		rev = "HEAD"
	}
	for _, suffix := range []string{"@{upstream}", "@{u}"} {
		if before, rest, ok := strings.Cut(rev, suffix); ok {
			branch := before
			if branch == "" || branch == "HEAD" {
				branch = HeadBranch(r)
			}
			display, _ := upstreamOf(r, branch)
			if display == "" {
				return nil, fmt.Errorf("no upstream configured for branch %q", branch)
			}
			rev = display + rest
			break
		}
	}
	h, err := r.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, fmt.Errorf("unknown revision %q", rev)
	}
	return r.CommitObject(*h)
}

func RefExists(r *git.Repository, name plumbing.ReferenceName) bool {
	_, err := r.Reference(name, true)
	return err == nil
}

func ValidateBranch(branch, label string) error {
	if !SafeBranchRe.MatchString(branch) {
		return fmt.Errorf("Invalid %s %q. Use only letters, numbers, /, _, ., -", label, branch)
	}
	return nil
}

func ValidateRef(ref, label string) error {
	if !safeRefRe.MatchString(ref) {
		return fmt.Errorf("Invalid %s %q. Use only safe git ref characters.", label, ref)
	}
	return nil
}

// ── remote branches, checkout, branch creation ───────────────────────────────

type RemoteBranch struct {
	Exists  bool
	Author  string
	Date    string
	Message string
	SHA     string
}

// gitDateLayout is git's default %ad format.
const gitDateLayout = "Mon Jan 2 15:04:05 2006 -0700"

func CheckRemoteBranch(branchName, repoPath string, creds config.Credentials) (RemoteBranch, error) {
	if err := ValidateBranch(branchName, "branchName"); err != nil {
		return RemoteBranch{}, err
	}
	r, err := Open(repoPath)
	if err != nil {
		return RemoteBranch{Exists: false}, nil
	}
	sha, ok := lsRemoteHead(r, "origin", branchName, creds)
	if !ok {
		return RemoteBranch{Exists: false}, nil
	}
	shortSha := sha.String()[:8]
	if err := FetchBranch(r, "origin", branchName, creds); err != nil {
		return RemoteBranch{Exists: true, SHA: shortSha}, nil
	}
	ref, err := r.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
	if err != nil {
		return RemoteBranch{Exists: true, SHA: shortSha}, nil
	}
	c, err := r.CommitObject(ref.Hash())
	if err != nil {
		return RemoteBranch{Exists: true, SHA: shortSha}, nil
	}
	author := c.Author.Name
	if c.Author.Email != "" {
		author += " <" + c.Author.Email + ">"
	}
	return RemoteBranch{
		Exists:  true,
		SHA:     shortSha,
		Author:  author,
		Date:    c.Author.When.Format(gitDateLayout),
		Message: commitSubject(c),
	}, nil
}

// OriginHead is the branch origin/HEAD points at, or "" when unset.
func OriginHead(r *git.Repository) string {
	ref, err := r.Reference(plumbing.NewRemoteHEADReferenceName("origin"), false)
	if err != nil || ref.Type() != plumbing.SymbolicReference {
		return ""
	}
	return strings.TrimPrefix(ref.Target().String(), "refs/remotes/origin/")
}

// CreateTrackingBranch creates refs/heads/<name> at hash with
// <remote>/<upstream> as its upstream, as `git branch --track` does.
func CreateTrackingBranch(r *git.Repository, name string, hash plumbing.Hash, remote, upstream string) error {
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), hash)
	if err := r.Storer.SetReference(ref); err != nil {
		return err
	}
	return setUpstream(r, name, remote, upstream)
}

func setUpstream(r *git.Repository, branch, remote, upstream string) error {
	cfg, err := r.Config()
	if err != nil {
		return err
	}
	cfg.Branches[branch] = &gitconfig.Branch{
		Name:   branch,
		Remote: remote,
		Merge:  plumbing.NewBranchReferenceName(upstream),
	}
	return r.Storer.SetConfig(cfg)
}

// ── network: ls-remote, fetch, push ──────────────────────────────────────────

const gitNetworkTimeout = 2 * time.Minute

// lsRemoteHead asks the remote for refs/heads/<branch> without fetching.
func lsRemoteHead(r *git.Repository, remote, branch string, creds config.Credentials) (plumbing.Hash, bool) {
	rem, err := r.Remote(remote)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	u := RemoteURL(r, remote)
	ctx, cancel := netContext()
	defer cancel()
	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: gitAuth(u, creds)})
	if err != nil {
		return plumbing.ZeroHash, false
	}
	want := plumbing.NewBranchReferenceName(branch)
	for _, ref := range refs {
		if ref.Name() == want {
			return ref.Hash(), true
		}
	}
	return plumbing.ZeroHash, false
}

// FetchBranch updates <remote>/<branch> from the remote.
func FetchBranch(r *git.Repository, remote, branch string, creds config.Credentials) error {
	rem, err := r.Remote(remote)
	if err != nil {
		return err
	}
	u := RemoteURL(r, remote)
	spec := gitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch))
	ctx, cancel := netContext()
	defer cancel()
	err = rem.FetchContext(ctx, &git.FetchOptions{RemoteURL: u, RefSpecs: []gitconfig.RefSpec{spec}, Auth: gitAuth(u, creds)})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}

// PushBranch is `git push -u <remote> <branch>`.
func PushBranch(r *git.Repository, remote, branch string, creds config.Credentials) error {
	rem, err := r.Remote(remote)
	if err != nil {
		return err
	}
	u := RemoteURL(r, remote)
	spec := gitconfig.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch))
	ctx, cancel := netContext()
	defer cancel()
	err = rem.PushContext(ctx, &git.PushOptions{RemoteURL: u, RefSpecs: []gitconfig.RefSpec{spec}, Auth: gitAuth(u, creds)})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}
	local, err := r.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return err
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName(remote, branch), local.Hash())); err != nil {
		return err
	}
	return setUpstream(r, branch, remote, branch)
}
