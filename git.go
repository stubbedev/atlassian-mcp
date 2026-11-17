package main

import (
	"bytes"
	"container/heap"
	"crypto/sha1" //nolint:gosec // git's index checksum is SHA-1
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// Repository access goes through go-git, so the server needs no git binary:
// everything a tool reads (branch, remote, log, status, diff) and the few
// things it changes (fetch, checkout, branch, push) happen in-process.

// Allowlist for git refs (commits, branches used as refs in diff commands).
var safeRefRe = regexp.MustCompile(`^[a-zA-Z0-9/_.\-@{}~^:]+(\.\.\.[a-zA-Z0-9/_.\-@{}~^:]+)?$`)

// Allowlist for branch names (stricter — no range syntax).
var safeBranchRe = regexp.MustCompile(`^[a-zA-Z0-9/_.\-]+$`)

// Abbreviated hashes are fixed-width: long enough to stay unambiguous in any
// repository these tools see, and stable across calls.
const shortHashLen = 10

// openRepo opens the repository containing path, walking up like git does and
// following a linked worktree's .git file to its common directory.
func openRepo(path string) (*git.Repository, error) {
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

func isGitRepo(repoPath string) bool {
	_, err := openRepo(repoPath)
	return err == nil
}

func notARepo(repoPath string, err error) error {
	return fmt.Errorf("Not a git repository: %s: %w", repoPath, err)
}

// headBranch is the checked-out branch's short name, "HEAD" when detached,
// or "" when HEAD cannot be read.
func headBranch(r *git.Repository) string {
	ref, err := r.Reference(plumbing.HEAD, false)
	if err != nil {
		return ""
	}
	if ref.Type() == plumbing.SymbolicReference {
		return ref.Target().Short()
	}
	return "HEAD"
}

// gitCurrentBranch is headBranch for a path; "" when it is not a repository.
func gitCurrentBranch(repoPath string) string {
	r, err := openRepo(repoPath)
	if err != nil {
		return ""
	}
	return headBranch(r)
}

// remoteURL returns the first URL of the named remote, with the user's global
// url.<base>.insteadOf rules applied the way git applies them.
func remoteURL(r *git.Repository, name string) string {
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

// gitOriginURL is the origin remote's URL, or "" when there is none.
func gitOriginURL(repoPath string) string {
	r, err := openRepo(repoPath)
	if err != nil {
		return ""
	}
	return remoteURL(r, "origin")
}

func shortHash(h plumbing.Hash) string { return h.String()[:shortHashLen] }

func commitSubject(c *object.Commit) string {
	subject, _, _ := strings.Cut(strings.TrimSpace(c.Message), "\n")
	return strings.TrimSpace(subject)
}

// gitOnelineLog lists the last n commits reachable from HEAD as
// "<short hash> <subject>" lines, newest first.
func gitOnelineLog(r *git.Repository, n int) string {
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

type commitAuthor struct{ name, email string }

// gitRecentAuthors returns the authors of the last n commits, newest first,
// mapped through the repository's .mailmap like git's %aN/%aE.
func gitRecentAuthors(r *git.Repository, n int) []commitAuthor {
	mm := loadMailmap(r)
	var out []commitAuthor
	_ = walkLog(r, n, func(c *object.Commit) {
		name, email := mm.resolve(c.Author.Name, c.Author.Email)
		out = append(out, commitAuthor{name, email})
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

// gitUpstreamLine describes how HEAD's branch relates to its upstream, e.g.
// "origin/main (2 ahead, 1 behind)", or "" when there is no upstream.
func gitUpstreamLine(r *git.Repository) string {
	display, refName := upstreamOf(r, headBranch(r))
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
func (q *commitQueue) Push(x any)    { *q = append(*q, x.(*object.Commit)) }
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
				branch = headBranch(r)
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

func refExists(r *git.Repository, name plumbing.ReferenceName) bool {
	_, err := r.Reference(name, true)
	return err == nil
}

// ── git_get_context ──────────────────────────────────────────────────────────

func remoteMatchesBitbucketInstance(remote, bitbucketURL string) bool {
	if remote == "" {
		return false
	}
	u, err := url.Parse(bitbucketURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	return strings.Contains(strings.ToLower(remote), strings.ToLower(u.Hostname()))
}

// resolveRepoRoot determines which repo a tool call targets: an explicit
// `repoPath` arg wins, else the client's workspace root (MCP roots), else the
// process cwd — but only in stdio mode (a shared HTTP server has no meaningful
// cwd). Returns "" when nothing resolves (HTTP without roots/repoPath), which
// downstream auto-detection turns into a clear error.
func resolveRepoRoot(s Session, args map[string]any) string {
	repoPathArg := argString(args, "repoPath")
	if s != nil {
		if r := s.resolveRepo(repoPathArg); r != "" {
			return r
		}
		if s.isStdio() {
			return mustGetwd()
		}
		return ""
	}
	return repoPathArg
}

func validateBranch(branch, label string) error {
	if !safeBranchRe.MatchString(branch) {
		return fmt.Errorf("Invalid %s %q. Use only letters, numbers, /, _, ., -", label, branch)
	}
	return nil
}

func validateRef(ref, label string) error {
	if !safeRefRe.MatchString(ref) {
		return fmt.Errorf("Invalid %s %q. Use only safe git ref characters.", label, ref)
	}
	return nil
}

// gitGetContext implements git_get_context.
func gitGetContext(args map[string]any, repoPath string) toolResult {
	// fromRef/toRef switch the tool into diff mode:
	// same repo resolution, different question.
	if argString(args, "fromRef") != "" || argString(args, "toRef") != "" {
		return gitGetDiffPaged(args, repoPath)
	}
	if repoPath == "" {
		return textResult("Error reading git context: no repo path — pass repoPath, or connect a client that provides workspace roots.")
	}
	limit := argIntDefault(args, "commitLimit", 10)
	if limit < 1 {
		limit = 1
	} else if limit > 100 {
		limit = 100
	}
	r, err := openRepo(repoPath)
	if err != nil {
		return textResult("Error reading git context: " + notARepo(repoPath, err).Error())
	}
	branch := orValue(headBranch(r), "(unknown)")
	remote := orValue(remoteURL(r, "origin"), "(no remote)")
	commits := orValue(gitOnelineLog(r, limit), "(no commits)")
	status, statusErr := gitStatusShort(r)
	upstreamLine := gitUpstreamLine(r)

	jiraKeys := uniqueStrings(jiraKeyRe.FindAllString(branch, -1))

	lines := []string{
		"Repository: " + repoPath,
		"Branch:     " + branch,
	}
	if upstreamLine != "" {
		lines = append(lines, "Upstream:   "+upstreamLine)
	}
	lines = append(lines, "Remote:     "+remote)
	if len(jiraKeys) > 0 {
		lines = append(lines, "Jira:       "+strings.Join(jiraKeys, ", "))
	}
	lines = append(lines, "", fmt.Sprintf("Recent commits (last %d):", limit), orValue(commits, "(none)"), "", "Working tree:")

	switch {
	case statusErr != nil:
		lines = append(lines, "(status unavailable: "+statusErr.Error()+")")
	case status != "":
		lines = append(lines, status)
		if stat := gitDiffStatSummary(r); stat != "" {
			lines = append(lines, "", "Diff stat:  "+stat)
		}
	default:
		lines = append(lines, "(clean)")
	}

	if argBool(args, "includeDiff") && status != "" {
		diff, _ := gitDiff(r, "HEAD", "")
		if diff != "" {
			const maxDiffChars = 6000
			body := diff
			if len(diff) > maxDiffChars {
				body = diff[:maxDiffChars] + fmt.Sprintf("\n\n... (truncated, %d more chars — re-call with fromRef=HEAD and maxChars/charOffset to page through it)", len(diff)-maxDiffChars)
			}
			lines = append(lines, "", "── Uncommitted diff ──", body)
		}
	}

	return textResult(strings.Join(lines, "\n"))
}

// getDiff implements diff mode (before paging).
func getDiff(args map[string]any, repoPath string) toolResult {
	if repoPath == "" {
		return textResult("Error reading diff: no repo path — pass repoPath, or connect a client that provides workspace roots.")
	}
	r, err := openRepo(repoPath)
	if err != nil {
		return textResult("Error reading diff: " + notARepo(repoPath, err).Error())
	}
	fromRef := argString(args, "fromRef")
	toRef := argString(args, "toRef")
	if fromRef == "" && toRef != "" {
		return textResult("Error reading diff: toRef needs fromRef — pass both to diff a range, or fromRef alone to diff it against the working tree.")
	}
	if fromRef == "" {
		fromRef = "HEAD"
	}
	if err := validateRef(fromRef, "fromRef"); err != nil {
		return textResult("Error reading diff: " + err.Error())
	}
	if toRef != "" {
		if err := validateRef(toRef, "toRef"); err != nil {
			return textResult("Error reading diff: " + err.Error())
		}
	}
	diff, err := gitDiff(r, fromRef, toRef)
	if err != nil {
		return textResult("Error reading diff: " + err.Error())
	}
	if diff == "" {
		return textResult("No differences found.")
	}
	return textResult(diff)
}

// gitGetDiffPaged applies maxChars/charOffset paging to diff mode.
func gitGetDiffPaged(args map[string]any, repoPath string) toolResult {
	result := getDiff(args, repoPath)
	return textResult(pageText(result.Content[0].Text, argInt(args, "charOffset"), argIntDefault(args, "maxChars", 8000), "charOffset", "maxChars"))
}

// ── remote branches, checkout, branch creation ───────────────────────────────

type remoteBranchInfo struct {
	exists  bool
	author  string
	date    string
	message string
	sha     string
}

// gitDateLayout is git's default %ad format.
const gitDateLayout = "Mon Jan 2 15:04:05 2006 -0700"

func checkRemoteBranch(branchName, repoPath string, creds Credentials) (remoteBranchInfo, error) {
	if err := validateBranch(branchName, "branchName"); err != nil {
		return remoteBranchInfo{}, err
	}
	r, err := openRepo(repoPath)
	if err != nil {
		return remoteBranchInfo{exists: false}, nil
	}
	sha, ok := lsRemoteHead(r, "origin", branchName, creds)
	if !ok {
		return remoteBranchInfo{exists: false}, nil
	}
	shortSha := sha.String()[:8]
	if err := fetchBranch(r, "origin", branchName, creds); err != nil {
		return remoteBranchInfo{exists: true, sha: shortSha}, nil
	}
	ref, err := r.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
	if err != nil {
		return remoteBranchInfo{exists: true, sha: shortSha}, nil
	}
	c, err := r.CommitObject(ref.Hash())
	if err != nil {
		return remoteBranchInfo{exists: true, sha: shortSha}, nil
	}
	author := c.Author.Name
	if c.Author.Email != "" {
		author += " <" + c.Author.Email + ">"
	}
	return remoteBranchInfo{
		exists:  true,
		sha:     shortSha,
		author:  author,
		date:    c.Author.When.Format(gitDateLayout),
		message: commitSubject(c),
	}, nil
}

// gitOriginHead is the branch origin/HEAD points at, or "" when unset.
func gitOriginHead(r *git.Repository) string {
	ref, err := r.Reference(plumbing.NewRemoteHEADReferenceName("origin"), false)
	if err != nil || ref.Type() != plumbing.SymbolicReference {
		return ""
	}
	return strings.TrimPrefix(ref.Target().String(), "refs/remotes/origin/")
}

// getDefaultBranch resolves the branch a new branch should fork from, without
// assuming any naming convention: git's own origin/HEAD first, then the server's
// answer for this repository, and only then a guess.
func getDefaultBranch(r *git.Repository) string {
	if head := gitOriginHead(r); head != "" {
		return head
	}
	if bitbucketClient != nil {
		if parsed := parseBitbucketRemote(remoteURL(r, "origin")); parsed != nil {
			if name := bitbucketClient.defaultBranchName(parsed.projectKey, parsed.repoSlug); name != "" {
				return name
			}
		}
	}
	for _, guess := range []string{"main", "master"} {
		if refExists(r, plumbing.NewRemoteReferenceName("origin", guess)) {
			return guess
		}
	}
	return "master"
}

func checkoutRemoteBranch(branchName, repoPath string) toolResult {
	if err := validateBranch(branchName, "branchName"); err != nil {
		return textResult("Error checking out branch: " + err.Error())
	}
	r, err := openRepo(repoPath)
	if err != nil {
		return textResult("Error checking out branch: " + err.Error())
	}
	local := plumbing.NewBranchReferenceName(branchName)
	if refExists(r, local) {
		if err := safeCheckout(r, local); err != nil {
			return textResult("Error checking out branch: " + err.Error())
		}
		return textResult(fmt.Sprintf("Switched to existing local branch %q.", branchName))
	}
	remoteRef, err := r.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
	if err != nil {
		return textResult(fmt.Sprintf("Error checking out branch: origin/%s not found locally — fetch it first.", branchName))
	}
	if err := createTrackingBranch(r, branchName, remoteRef.Hash(), "origin", branchName); err != nil {
		return textResult("Error checking out branch: " + err.Error())
	}
	if err := safeCheckout(r, local); err != nil {
		_ = r.Storer.RemoveReference(local)
		return textResult("Error checking out branch: " + err.Error())
	}
	return textResult(fmt.Sprintf("Checked out %q tracking origin/%s.", branchName, branchName))
}

// createTrackingBranch creates refs/heads/<name> at hash with
// <remote>/<upstream> as its upstream, as `git branch --track` does.
func createTrackingBranch(r *git.Repository, name string, hash plumbing.Hash, remote, upstream string) error {
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

func createBranch(branchName, baseBranch, repoPath string, push bool) toolResult {
	if repoPath == "" {
		repoPath = mustGetwd()
	}
	r, err := openRepo(repoPath)
	if err != nil {
		return textResult("Error creating branch: " + notARepo(repoPath, err).Error())
	}
	if !safeBranchRe.MatchString(branchName) {
		return textResult(fmt.Sprintf("Invalid branch name %q. Use only letters, numbers, /, _, ., -", branchName))
	}
	if baseBranch == "" {
		baseBranch = getDefaultBranch(r)
	}
	if !safeBranchRe.MatchString(baseBranch) {
		return textResult(fmt.Sprintf("Invalid base branch name %q. Use only letters, numbers, /, _, ., -", baseBranch))
	}
	local := plumbing.NewBranchReferenceName(branchName)
	if refExists(r, local) {
		return textResult(fmt.Sprintf("Branch %q already exists locally. Switch with: git checkout %s", branchName, branchName))
	}
	_ = fetchBranch(r, "origin", baseBranch, credentials)
	base, err := r.Reference(plumbing.NewRemoteReferenceName("origin", baseBranch), true)
	if err != nil {
		return textResult(fmt.Sprintf("Error creating branch: origin/%s not found.", baseBranch))
	}
	// Forking from a remote-tracking branch tracks it, as git's default
	// branch.autoSetupMerge does; a push below re-points it at the new branch.
	if err := createTrackingBranch(r, branchName, base.Hash(), "origin", baseBranch); err != nil {
		return textResult("Error creating branch: " + err.Error())
	}
	if err := safeCheckout(r, local); err != nil {
		_ = r.Storer.RemoveReference(local)
		return textResult("Error creating branch: " + err.Error())
	}
	lines := []string{fmt.Sprintf("Created and switched to branch %q from origin/%s.", branchName, baseBranch)}
	if push {
		if err := pushBranch(r, "origin", branchName, credentials); err != nil {
			return textResult("Error creating branch: " + err.Error())
		}
		lines = append(lines, fmt.Sprintf("Pushed to origin/%s and set upstream.", branchName))
	}
	return textResult(strings.Join(lines, "\n"))
}

// ── network: ls-remote, fetch, push ──────────────────────────────────────────

const gitNetworkTimeout = 2 * time.Minute

// lsRemoteHead asks the remote for refs/heads/<branch> without fetching.
func lsRemoteHead(r *git.Repository, remote, branch string, creds Credentials) (plumbing.Hash, bool) {
	rem, err := r.Remote(remote)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	u := remoteURL(r, remote)
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

// fetchBranch updates <remote>/<branch> from the remote.
func fetchBranch(r *git.Repository, remote, branch string, creds Credentials) error {
	rem, err := r.Remote(remote)
	if err != nil {
		return err
	}
	u := remoteURL(r, remote)
	spec := gitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch))
	ctx, cancel := netContext()
	defer cancel()
	err = rem.FetchContext(ctx, &git.FetchOptions{RemoteURL: u, RefSpecs: []gitconfig.RefSpec{spec}, Auth: gitAuth(u, creds)})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}

// pushBranch is `git push -u <remote> <branch>`.
func pushBranch(r *git.Repository, remote, branch string, creds Credentials) error {
	rem, err := r.Remote(remote)
	if err != nil {
		return err
	}
	u := remoteURL(r, remote)
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

// ── helpers ──────────────────────────────────────────────────────────────────

func orValue(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// pluralize renders "1 file" / "2 files".
func pluralize(n int, one, many string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + one
	}
	return strconv.Itoa(n) + " " + many
}
