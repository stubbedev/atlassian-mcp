package main

import (
	"fmt"
	"strconv"
	"strings"
)

type committer struct {
	name    string
	email   string
	commits int
}

// getTopCommitters returns the top recent committers from the last `lookback`
// commits, ranked by count (descending), capped to `top`.
func getTopCommitters(repoPath string, lookback, top int) []committer {
	raw := safeGit(repoPath, "", "log", "-n", strconv.Itoa(lookback), "--format=%aN%x09%aE")
	if raw == "" {
		return nil
	}
	type entry struct {
		c     committer
		order int
	}
	counts := map[string]*entry{}
	order := 0
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		name := parts[0]
		if name == "" {
			continue
		}
		email := ""
		if len(parts) > 1 {
			email = parts[1]
		}
		key := strings.ToLower(name)
		if email != "" {
			key = strings.ToLower(email)
		}
		if e, ok := counts[key]; ok {
			e.c.commits++
		} else {
			counts[key] = &entry{c: committer{name: name, email: email, commits: 1}, order: order}
			order++
		}
	}
	list := make([]*entry, 0, len(counts))
	for _, e := range counts {
		list = append(list, e)
	}
	// Stable sort by commits desc, preserving first-seen order on ties.
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			a, b := list[j-1], list[j]
			if b.c.commits > a.c.commits || (b.c.commits == a.c.commits && b.order < a.order) {
				list[j-1], list[j] = list[j], list[j-1]
			} else {
				break
			}
		}
	}
	var out []committer
	for i, e := range list {
		if i >= top {
			break
		}
		out = append(out, e.c)
	}
	return out
}

// getDevContext assembles git state + linked Jira issues + the open PR for the
// current branch into one report (get_dev_context tool).
func getDevContext(repoPath string) (toolResult, error) {
	if repoPath == "" {
		return textResult("No repo resolved. Pass repoPath, or connect a client that provides workspace roots (MCP roots)."), nil
	}
	if !isGitRepo(repoPath) {
		return textResult(fmt.Sprintf("Not a git repository: %s", repoPath)), nil
	}
	var sections []string

	branch := orValue(safeGit(repoPath, "", "rev-parse", "--abbrev-ref", "HEAD"), "(unknown)")
	remote := orValue(safeGit(repoPath, "", "remote", "get-url", "origin"), "(no remote)")
	recentCommits := orValue(safeGit(repoPath, "", "log", "--oneline", "-5"), "(none)")
	status := orValue(safeGit(repoPath, "", "status", "--short"), "(clean)")
	committers := getTopCommitters(repoPath, 50, 5)

	var parsed *bitbucketRemote
	if bitbucket != nil && bitbucket.isRemoteForThisInstance(remote) {
		parsed = parseBitbucketRemote(remote)
	}

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

	// Identity (best-effort).
	var youParts []string
	if jira != nil {
		if me, err := jira.whoami(); err == nil && me != nil {
			id := me.Name
			if id == "" {
				id = me.Key
			}
			if id == "" {
				id = "(unknown)"
			}
			disp := ""
			if me.DisplayName != "" {
				disp = ` "` + me.DisplayName + `"`
			}
			youParts = append(youParts, "Jira "+id+disp)
		}
	}
	if bitbucket != nil {
		if me, err := bitbucket.whoami(); err == nil && me != "" {
			youParts = append(youParts, "Bitbucket "+me)
		}
	}

	headerLines := []string{
		"Repository: " + repoPath,
		"Branch:     " + branch,
	}
	if upstreamLine != "" {
		headerLines = append(headerLines, "Upstream:   "+upstreamLine)
	}
	headerLines = append(headerLines, "Remote:     "+remote)
	if parsed != nil {
		headerLines = append(headerLines, fmt.Sprintf("Bitbucket:  %s/%s", parsed.projectKey, parsed.repoSlug))
	}
	if len(youParts) > 0 {
		headerLines = append(headerLines, "You:        "+strings.Join(youParts, " · "))
	}
	if len(committers) > 0 {
		headerLines = append(headerLines, "", "Top recent committers (last 50):")
		for _, c := range committers {
			ident := c.name
			if c.email != "" {
				ident = c.name + " <" + c.email + ">"
			}
			headerLines = append(headerLines, fmt.Sprintf("  %3d — %s", c.commits, ident))
		}
		headerLines = append(headerLines, "  (look up usernames via jira_search resource=users / bitbucket_search resource=users — do NOT shell out to git/gh/bb)")
	}
	headerLines = append(headerLines, "", "Recent commits:", recentCommits, "", "Working tree:", status)
	sections = append(sections, strings.Join(headerLines, "\n"))

	// Jira — overview for any keys in the branch name.
	if jira != nil {
		jiraKeys := uniqueStrings(jiraKeyRe.FindAllString(branch, -1))
		for _, key := range jiraKeys {
			res, err := jira.issueOverview(issueOverviewOpts{
				issueKey:           key,
				includeComments:    true,
				commentsMaxResults: 5,
				commentsStartAt:    0,
				includeTransitions: true,
				includeSprint:      true,
				descriptionCap:     800,
			})
			if err != nil || len(res.Content) == 0 {
				sections = append(sections, fmt.Sprintf("── Jira %s ── (could not fetch)", key))
			} else {
				sections = append(sections, fmt.Sprintf("── Jira %s ──\n%s", key, res.Content[0].Text))
			}
		}
	}

	// Bitbucket — open PR for this branch.
	if parsed != nil {
		pr, err := bitbucket.findOpenPrForBranch(parsed.projectKey, parsed.repoSlug, branch)
		if err != nil {
			sections = append(sections, fmt.Sprintf("── Bitbucket (%s/%s) ── (could not fetch PRs)", parsed.projectKey, parsed.repoSlug))
		} else if pr != nil {
			approved := 0
			for _, r := range pr.Reviewers {
				if r.Approved {
					approved++
				}
			}
			total := len(pr.Reviewers)
			var reviewerParts []string
			for _, r := range pr.Reviewers {
				mark := ""
				if r.Approved {
					mark = " ✓"
				}
				reviewerParts = append(reviewerParts, r.User.DisplayName+mark)
			}
			reviewers := strings.Join(reviewerParts, ", ")
			if reviewers == "" {
				reviewers = "none"
			}
			prURL := ""
			if pr.Links != nil && len(pr.Links.Self) > 0 {
				prURL = pr.Links.Self[0].Href
			}
			descSnippet := ""
			if pr.Description != "" {
				if len(pr.Description) > 200 {
					descSnippet = pr.Description[:200] + "…"
				} else {
					descSnippet = pr.Description
				}
			}
			prLines := []string{
				fmt.Sprintf("── PR #%d: %s ──", pr.ID, pr.Title),
				"State:     " + pr.State,
				"Author:    " + pr.Author.User.DisplayName,
				fmt.Sprintf("Branch:    %s → %s", pr.FromRef.DisplayID, pr.ToRef.DisplayID),
				fmt.Sprintf("Reviewers: %s (%d/%d approved)", reviewers, approved, total),
			}
			if prURL != "" {
				prLines = append(prLines, "URL:       "+prURL)
			}
			if descSnippet != "" {
				prLines = append(prLines, "", descSnippet)
			}
			if total > 0 && approved == total {
				prLines = append(prLines, "", fmt.Sprintf(`✓ All reviewers approved — ready to merge: bitbucket_mutate {prId: %d, action: "merge"}`, pr.ID))
			} else if total > 0 {
				prLines = append(prLines, "", fmt.Sprintf("→ %d reviewer(s) pending — use bitbucket_get_pr {prId: %d} to see open comments", total-approved, pr.ID))
			} else {
				prLines = append(prLines, "", fmt.Sprintf("→ No reviewers assigned — use bitbucket_mutate {prId: %d, update: {reviewers: [...]}} to add them", pr.ID))
			}
			sections = append(sections, strings.Join(prLines, "\n"))
		} else {
			noPrHint := []string{
				fmt.Sprintf(`── Bitbucket (%s/%s) ── No open PR for branch "%s"`, parsed.projectKey, parsed.repoSlug, branch),
				fmt.Sprintf(`→ Create one: bitbucket_mutate {create: {title: "...", fromBranch: "%s"}}`, branch),
			}
			sections = append(sections, strings.Join(noPrHint, "\n"))
		}
	}

	return textResult(strings.Join(sections, "\n\n")), nil
}
