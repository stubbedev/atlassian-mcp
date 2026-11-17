package server

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stubbedev/atlassian-mcp/internal/arg"
	"github.com/stubbedev/atlassian-mcp/internal/bitbucket"
	"github.com/stubbedev/atlassian-mcp/internal/gitrepo"
	"github.com/stubbedev/atlassian-mcp/internal/jira"
	"github.com/stubbedev/atlassian-mcp/internal/text"
	"github.com/stubbedev/atlassian-mcp/internal/tool"
)

// resolveRepoRoot determines which repo a tool call targets: an explicit
// `repoPath` arg wins, else the client's workspace root (MCP roots), else the
// process cwd — but only in stdio mode (a shared HTTP server has no meaningful
// cwd). Returns "" when nothing resolves (HTTP without roots/repoPath), which
// downstream auto-detection turns into a clear error.
func resolveRepoRoot(s Session, args map[string]any) string {
	repoPathArg := arg.String(args, "repoPath")
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

// gitGetContext implements git_get_context.
func gitGetContext(args map[string]any, repoPath string) tool.Result {
	// fromRef/toRef switch the tool into diff mode:
	// same repo resolution, different question.
	if arg.String(args, "fromRef") != "" || arg.String(args, "toRef") != "" {
		return gitGetDiffPaged(args, repoPath)
	}
	if repoPath == "" {
		return tool.TextResult("Error reading git context: no repo path — pass repoPath, or connect a client that provides workspace roots.")
	}
	limit := arg.IntDefault(args, "commitLimit", 10)
	if limit < 1 {
		limit = 1
	} else if limit > 100 {
		limit = 100
	}
	r, err := gitrepo.Open(repoPath)
	if err != nil {
		return tool.TextResult("Error reading git context: " + gitrepo.NotARepo(repoPath, err).Error())
	}
	branch := text.Or(gitrepo.HeadBranch(r), "(unknown)")
	remote := text.Or(gitrepo.RemoteURL(r, "origin"), "(no remote)")
	commits := text.Or(gitrepo.OnelineLog(r, limit), "(no commits)")
	status, statusErr := gitrepo.StatusShort(r)
	upstreamLine := gitrepo.UpstreamLine(r)

	jiraKeys := text.Unique(jira.KeyRe.FindAllString(branch, -1))

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
	lines = append(lines, "", fmt.Sprintf("Recent commits (last %d):", limit), text.Or(commits, "(none)"), "", "Working tree:")

	switch {
	case statusErr != nil:
		lines = append(lines, "(status unavailable: "+statusErr.Error()+")")
	case status != "":
		lines = append(lines, status)
		if stat := gitrepo.DiffStatSummary(r); stat != "" {
			lines = append(lines, "", "Diff stat:  "+stat)
		}
	default:
		lines = append(lines, "(clean)")
	}

	if arg.Bool(args, "includeDiff") && status != "" {
		diff, _ := gitrepo.Diff(r, "HEAD", "")
		if diff != "" {
			const maxDiffChars = 6000
			body := diff
			if len(diff) > maxDiffChars {
				body = diff[:maxDiffChars] + fmt.Sprintf("\n\n... (truncated, %d more chars — re-call with fromRef=HEAD and maxChars/charOffset to page through it)", len(diff)-maxDiffChars)
			}
			lines = append(lines, "", "── Uncommitted diff ──", body)
		}
	}

	return tool.TextResult(strings.Join(lines, "\n"))
}

// getDiff implements diff mode (before paging).
func getDiff(args map[string]any, repoPath string) tool.Result {
	if repoPath == "" {
		return tool.TextResult("Error reading diff: no repo path — pass repoPath, or connect a client that provides workspace roots.")
	}
	r, err := gitrepo.Open(repoPath)
	if err != nil {
		return tool.TextResult("Error reading diff: " + gitrepo.NotARepo(repoPath, err).Error())
	}
	fromRef := arg.String(args, "fromRef")
	toRef := arg.String(args, "toRef")
	if fromRef == "" && toRef != "" {
		return tool.TextResult("Error reading diff: toRef needs fromRef — pass both to diff a range, or fromRef alone to diff it against the working tree.")
	}
	if fromRef == "" {
		fromRef = "HEAD"
	}
	if err := gitrepo.ValidateRef(fromRef, "fromRef"); err != nil {
		return tool.TextResult("Error reading diff: " + err.Error())
	}
	if toRef != "" {
		if err := gitrepo.ValidateRef(toRef, "toRef"); err != nil {
			return tool.TextResult("Error reading diff: " + err.Error())
		}
	}
	diff, err := gitrepo.Diff(r, fromRef, toRef)
	if err != nil {
		return tool.TextResult("Error reading diff: " + err.Error())
	}
	if diff == "" {
		return tool.TextResult("No differences found.")
	}
	return tool.TextResult(diff)
}

// gitGetDiffPaged applies maxChars/charOffset paging to diff mode.
func gitGetDiffPaged(args map[string]any, repoPath string) tool.Result {
	result := getDiff(args, repoPath)
	return tool.TextResult(text.Page(result.Content[0].Text, arg.Int(args, "charOffset"), arg.IntDefault(args, "maxChars", 8000), "charOffset", "maxChars"))
}

// getDefaultBranch resolves the branch a new branch should fork from, without
// assuming any naming convention: git's own origin/HEAD first, then the server's
// answer for this repository, and only then a guess.
func getDefaultBranch(r *git.Repository) string {
	if head := gitrepo.OriginHead(r); head != "" {
		return head
	}
	if bitbucketClient != nil {
		if parsed := bitbucket.ParseRemote(gitrepo.RemoteURL(r, "origin")); parsed != nil {
			if name := bitbucketClient.DefaultBranchName(parsed.ProjectKey, parsed.RepoSlug); name != "" {
				return name
			}
		}
	}
	for _, guess := range []string{"main", "master"} {
		if gitrepo.RefExists(r, plumbing.NewRemoteReferenceName("origin", guess)) {
			return guess
		}
	}
	return "master"
}

func checkoutRemoteBranch(branchName, repoPath string) tool.Result {
	if err := gitrepo.ValidateBranch(branchName, "branchName"); err != nil {
		return tool.TextResult("Error checking out branch: " + err.Error())
	}
	r, err := gitrepo.Open(repoPath)
	if err != nil {
		return tool.TextResult("Error checking out branch: " + err.Error())
	}
	local := plumbing.NewBranchReferenceName(branchName)
	if gitrepo.RefExists(r, local) {
		if err := gitrepo.SafeCheckout(r, local); err != nil {
			return tool.TextResult("Error checking out branch: " + err.Error())
		}
		return tool.TextResult(fmt.Sprintf("Switched to existing local branch %q.", branchName))
	}
	remoteRef, err := r.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
	if err != nil {
		return tool.TextResult(fmt.Sprintf("Error checking out branch: origin/%s not found locally — fetch it first.", branchName))
	}
	if err := gitrepo.CreateTrackingBranch(r, branchName, remoteRef.Hash(), "origin", branchName); err != nil {
		return tool.TextResult("Error checking out branch: " + err.Error())
	}
	if err := gitrepo.SafeCheckout(r, local); err != nil {
		_ = r.Storer.RemoveReference(local)
		return tool.TextResult("Error checking out branch: " + err.Error())
	}
	return tool.TextResult(fmt.Sprintf("Checked out %q tracking origin/%s.", branchName, branchName))
}

func createBranch(branchName, baseBranch, repoPath string, push bool) tool.Result {
	if repoPath == "" {
		repoPath = mustGetwd()
	}
	r, err := gitrepo.Open(repoPath)
	if err != nil {
		return tool.TextResult("Error creating branch: " + gitrepo.NotARepo(repoPath, err).Error())
	}
	if !gitrepo.SafeBranchRe.MatchString(branchName) {
		return tool.TextResult(fmt.Sprintf("Invalid branch name %q. Use only letters, numbers, /, _, ., -", branchName))
	}
	if baseBranch == "" {
		baseBranch = getDefaultBranch(r)
	}
	if !gitrepo.SafeBranchRe.MatchString(baseBranch) {
		return tool.TextResult(fmt.Sprintf("Invalid base branch name %q. Use only letters, numbers, /, _, ., -", baseBranch))
	}
	local := plumbing.NewBranchReferenceName(branchName)
	if gitrepo.RefExists(r, local) {
		return tool.TextResult(fmt.Sprintf("Branch %q already exists locally. Switch with: git checkout %s", branchName, branchName))
	}
	_ = gitrepo.FetchBranch(r, "origin", baseBranch, credentials)
	base, err := r.Reference(plumbing.NewRemoteReferenceName("origin", baseBranch), true)
	if err != nil {
		return tool.TextResult(fmt.Sprintf("Error creating branch: origin/%s not found.", baseBranch))
	}
	// Forking from a remote-tracking branch tracks it, as git's default
	// branch.autoSetupMerge does; a push below re-points it at the new branch.
	if err := gitrepo.CreateTrackingBranch(r, branchName, base.Hash(), "origin", baseBranch); err != nil {
		return tool.TextResult("Error creating branch: " + err.Error())
	}
	if err := gitrepo.SafeCheckout(r, local); err != nil {
		_ = r.Storer.RemoveReference(local)
		return tool.TextResult("Error creating branch: " + err.Error())
	}
	lines := []string{fmt.Sprintf("Created and switched to branch %q from origin/%s.", branchName, baseBranch)}
	if push {
		if err := gitrepo.PushBranch(r, "origin", branchName, credentials); err != nil {
			return tool.TextResult("Error creating branch: " + err.Error())
		}
		lines = append(lines, fmt.Sprintf("Pushed to origin/%s and set upstream.", branchName))
	}
	return tool.TextResult(strings.Join(lines, "\n"))
}
