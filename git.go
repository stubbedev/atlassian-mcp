package main

import (
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Allowlist for git refs (commits, branches used as refs in diff commands).
var safeRefRe = regexp.MustCompile(`^[a-zA-Z0-9/_.\-@{}~^:]+(\.\.\.[a-zA-Z0-9/_.\-@{}~^:]+)?$`)

// Allowlist for branch names (stricter — no range syntax).
var safeBranchRe = regexp.MustCompile(`^[a-zA-Z0-9/_.\-]+$`)

// gitIn runs git in cwd and returns trimmed stdout, or an error.
func gitIn(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// safeGit runs git in cwd, returning fallback on any error.
func safeGit(cwd string, fallback string, args ...string) string {
	out, err := gitIn(cwd, args...)
	if err != nil {
		return fallback
	}
	return out
}

// safeExecGit runs git in the process cwd, returning "" on error.
func safeExecGit(args ...string) string {
	return safeGit("", "", args...)
}

func currentGitRemote() string {
	return safeExecGit("remote", "get-url", "origin")
}

func currentGitBranch() string {
	return safeExecGit("rev-parse", "--abbrev-ref", "HEAD")
}

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

func validateRepoPath(repoPath string) error {
	if _, err := gitIn(repoPath, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("Not a git repository: %s", repoPath)
	}
	return nil
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

func repoPathOrCwd(args map[string]any) string {
	if p := argString(args, "repoPath"); p != "" {
		return p
	}
	return mustGetwd()
}

// gitGetContext implements git_get_context.
func gitGetContext(args map[string]any) toolResult {
	repoPath := repoPathOrCwd(args)
	limit := argIntDefault(args, "commitLimit", 10)
	if limit < 1 {
		limit = 1
	} else if limit > 100 {
		limit = 100
	}
	if err := validateRepoPath(repoPath); err != nil {
		return textResult("Error reading git context: " + err.Error())
	}
	branch := safeGit(repoPath, "(unknown)", "rev-parse", "--abbrev-ref", "HEAD")
	remote := safeGit(repoPath, "(no remote)", "remote", "get-url", "origin")
	commits := safeGit(repoPath, "(no commits)", "log", "--oneline", fmt.Sprintf("-%d", limit))
	status := safeGit(repoPath, "", "status", "--short")

	upstream := safeGit(repoPath, "", "rev-parse", "--abbrev-ref", "@{u}")
	upstreamLine := ""
	if upstream != "" {
		ab := safeGit(repoPath, "", "rev-list", "--left-right", "--count", upstream+"...HEAD")
		if strings.Contains(ab, "\t") {
			parts := strings.SplitN(ab, "\t", 2)
			behind, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
			ahead, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
			var p []string
			if ahead != 0 {
				p = append(p, fmt.Sprintf("%d ahead", ahead))
			}
			if behind != 0 {
				p = append(p, fmt.Sprintf("%d behind", behind))
			}
			if len(p) > 0 {
				upstreamLine = upstream + " (" + strings.Join(p, ", ") + ")"
			} else {
				upstreamLine = upstream + " (up to date)"
			}
		}
	}

	diffStat := ""
	diffStatLines := splitNonEmpty(safeGit(repoPath, "", "diff", "HEAD", "--stat"), "\n")
	if len(diffStatLines) > 0 {
		diffStat = strings.TrimSpace(diffStatLines[len(diffStatLines)-1])
	}

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

	if status != "" {
		lines = append(lines, status)
		if diffStat != "" {
			lines = append(lines, "", "Diff stat:  "+diffStat)
		}
	} else {
		lines = append(lines, "(clean)")
	}

	if argBool(args, "includeDiff") && status != "" {
		diff := safeGit(repoPath, "", "diff", "HEAD")
		if diff != "" {
			const max = 6000
			body := diff
			if len(diff) > max {
				body = diff[:max] + fmt.Sprintf("\n\n... (truncated, %d more chars)", len(diff)-max)
			}
			lines = append(lines, "", "── Uncommitted diff ──", body)
		}
	}

	return textResult(strings.Join(lines, "\n"))
}

// getDiff implements the core of git_get_diff (before paging).
func getDiff(args map[string]any) toolResult {
	repoPath := repoPathOrCwd(args)
	if err := validateRepoPath(repoPath); err != nil {
		return textResult("Error reading diff: " + err.Error())
	}
	fromRef := argString(args, "fromRef")
	toRef := argString(args, "toRef")
	var gitArgs []string
	switch {
	case fromRef != "" && toRef != "":
		if err := validateRef(fromRef, "fromRef"); err != nil {
			return textResult("Error reading diff: " + err.Error())
		}
		if err := validateRef(toRef, "toRef"); err != nil {
			return textResult("Error reading diff: " + err.Error())
		}
		gitArgs = []string{"diff", fromRef, toRef}
	case fromRef != "":
		if err := validateRef(fromRef, "fromRef"); err != nil {
			return textResult("Error reading diff: " + err.Error())
		}
		gitArgs = []string{"diff", fromRef}
	default:
		gitArgs = []string{"diff", "HEAD"}
	}
	diff := safeGit(repoPath, "", gitArgs...)
	if diff == "" {
		return textResult("No differences found.")
	}
	return textResult(diff)
}

// gitGetDiffPaged applies the maxChars/charOffset paging from src/index.ts.
func gitGetDiffPaged(args map[string]any) toolResult {
	result := getDiff(args)
	raw := result.Content[0].Text
	offset := argInt(args, "charOffset")
	limit := argIntDefault(args, "maxChars", 8000)
	if offset == 0 && len(raw) <= limit {
		return result
	}
	if offset > len(raw) {
		offset = len(raw)
	}
	end := offset + limit
	if end > len(raw) {
		end = len(raw)
	}
	chunk := raw[offset:end]
	remaining := len(raw) - offset - len(chunk)
	suffix := ""
	if remaining > 0 {
		suffix = fmt.Sprintf("\n\n... (%d more chars, use charOffset=%d)", remaining, offset+len(chunk))
	}
	return textResult(chunk + suffix)
}

type remoteBranchInfo struct {
	exists  bool
	author  string
	date    string
	message string
	sha     string
}

func checkRemoteBranch(branchName, repoPath string) (remoteBranchInfo, error) {
	if err := validateBranch(branchName, "branchName"); err != nil {
		return remoteBranchInfo{}, err
	}
	lsRemote := safeGit(repoPath, "", "ls-remote", "--heads", "origin", "refs/heads/"+branchName)
	if lsRemote == "" {
		return remoteBranchInfo{exists: false}, nil
	}
	sha := strings.TrimSpace(strings.Fields(lsRemote)[0])
	shortSha := sha
	if len(shortSha) > 8 {
		shortSha = shortSha[:8]
	}
	if _, err := gitIn(repoPath, "fetch", "origin", branchName); err != nil {
		return remoteBranchInfo{exists: true, sha: shortSha}, nil
	}
	logLine := safeGit(repoPath, "", "log", "origin/"+branchName, "-1", "--format=%an%x09%ae%x09%ad%x09%s")
	if logLine == "" {
		return remoteBranchInfo{exists: true, sha: shortSha}, nil
	}
	parts := strings.SplitN(logLine, "\t", 4)
	info := remoteBranchInfo{exists: true, sha: shortSha}
	if len(parts) >= 1 {
		info.author = parts[0]
	}
	if len(parts) >= 2 && parts[1] != "" {
		info.author = parts[0] + " <" + parts[1] + ">"
	}
	if len(parts) >= 3 {
		info.date = parts[2]
	}
	if len(parts) >= 4 {
		info.message = parts[3]
	}
	return info, nil
}

func getDefaultBranch(repoPath string) string {
	head := safeGit(repoPath, "", "rev-parse", "--abbrev-ref", "origin/HEAD")
	if strings.HasPrefix(head, "origin/") {
		return strings.TrimPrefix(head, "origin/")
	}
	if safeGit(repoPath, "", "rev-parse", "--verify", "origin/main") != "" {
		return "main"
	}
	return "master"
}

func checkoutRemoteBranch(branchName, repoPath string) toolResult {
	if err := validateBranch(branchName, "branchName"); err != nil {
		return textResult("Error checking out branch: " + err.Error())
	}
	existing := safeGit(repoPath, "", "branch", "--list", branchName)
	if strings.TrimSpace(existing) != "" {
		if _, err := gitIn(repoPath, "checkout", branchName); err != nil {
			return textResult("Error checking out branch: " + err.Error())
		}
		return textResult(fmt.Sprintf("Switched to existing local branch %q.", branchName))
	}
	if _, err := gitIn(repoPath, "checkout", "--track", "origin/"+branchName); err != nil {
		return textResult("Error checking out branch: " + err.Error())
	}
	return textResult(fmt.Sprintf("Checked out %q tracking origin/%s.", branchName, branchName))
}

func createBranch(branchName, baseBranch, repoPath string, push bool) toolResult {
	if repoPath == "" {
		repoPath = mustGetwd()
	}
	if err := validateRepoPath(repoPath); err != nil {
		return textResult("Error creating branch: " + err.Error())
	}
	if !safeBranchRe.MatchString(branchName) {
		return textResult(fmt.Sprintf("Invalid branch name %q. Use only letters, numbers, /, _, ., -", branchName))
	}
	if baseBranch == "" {
		baseBranch = getDefaultBranch(repoPath)
	}
	if !safeBranchRe.MatchString(baseBranch) {
		return textResult(fmt.Sprintf("Invalid base branch name %q. Use only letters, numbers, /, _, ., -", baseBranch))
	}
	existing := safeGit(repoPath, "", "branch", "--list", branchName)
	if strings.TrimSpace(existing) != "" {
		return textResult(fmt.Sprintf("Branch %q already exists locally. Switch with: git checkout %s", branchName, branchName))
	}
	safeGit(repoPath, "", "fetch", "origin", baseBranch)
	if _, err := gitIn(repoPath, "checkout", "-b", branchName, "origin/"+baseBranch); err != nil {
		return textResult("Error creating branch: " + err.Error())
	}
	lines := []string{fmt.Sprintf("Created and switched to branch %q from origin/%s.", branchName, baseBranch)}
	if push {
		if _, err := gitIn(repoPath, "push", "-u", "origin", branchName); err != nil {
			return textResult("Error creating branch: " + err.Error())
		}
		lines = append(lines, fmt.Sprintf("Pushed to origin/%s and set upstream.", branchName))
	}
	return textResult(strings.Join(lines, "\n"))
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orValue(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
