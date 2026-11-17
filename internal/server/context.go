package server

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/stubbedev/atlassian-mcp/internal/bitbucket"
	"github.com/stubbedev/atlassian-mcp/internal/gitrepo"
	"github.com/stubbedev/atlassian-mcp/internal/jira"
	"github.com/stubbedev/atlassian-mcp/internal/text"
	"github.com/stubbedev/atlassian-mcp/internal/tool"
)

type committer struct {
	name    string
	email   string
	commits int
}

// getTopCommitters returns the top recent committers from the last `lookback`
// commits, ranked by count (descending), capped to `top`.
func getTopCommitters(r *git.Repository, lookback, top int) []committer {
	authors := gitrepo.RecentAuthors(r, lookback)
	if len(authors) == 0 {
		return nil
	}
	type entry struct {
		c     committer
		order int
	}
	counts := map[string]*entry{}
	order := 0
	for _, a := range authors {
		name, email := a.Name, a.Email
		if name == "" {
			continue
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
			if b.c.commits < a.c.commits || (b.c.commits == a.c.commits && b.order >= a.order) {
				break
			}
			list[j-1], list[j] = list[j], list[j-1]
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
func getDevContext(repoPath string) (tool.Result, error) {
	if repoPath == "" {
		return tool.TextResult("No repo resolved. Pass repoPath, or connect a client that provides workspace roots (MCP roots)."), nil
	}
	r, err := gitrepo.Open(repoPath)
	if err != nil {
		return tool.TextResult("Not a git repository: " + repoPath), nil
	}
	var sections []string

	branch := text.Or(gitrepo.HeadBranch(r), "(unknown)")
	remote := text.Or(gitrepo.RemoteURL(r, "origin"), "(no remote)")
	recentCommits := text.Or(gitrepo.OnelineLog(r, 5), "(none)")
	status, err := gitrepo.StatusShort(r)
	if err != nil {
		status = "(status unavailable: " + err.Error() + ")"
	}
	status = text.Or(status, "(clean)")
	committers := getTopCommitters(r, 50, 5)

	var parsed *bitbucket.Remote
	if bitbucketClient != nil && bitbucketClient.IsRemoteForThisInstance(remote) {
		parsed = bitbucket.ParseRemote(remote)
	}

	upstreamLine := gitrepo.UpstreamLine(r)

	// Identity (best-effort).
	var youParts []string
	if jiraClient != nil {
		if me, err := jiraClient.Whoami(); err == nil && me != nil {
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
	if bitbucketClient != nil {
		if me, err := bitbucketClient.Whoami(); err == nil && me != "" {
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
		headerLines = append(headerLines, fmt.Sprintf("Bitbucket:  %s/%s", parsed.ProjectKey, parsed.RepoSlug))
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
	if jiraClient != nil {
		jiraKeys := text.Unique(jira.KeyRe.FindAllString(branch, -1))
		for _, key := range jiraKeys {
			res, err := jiraClient.IssueOverview(jira.IssueOverviewOpts{
				IssueKey:           key,
				IncludeComments:    true,
				CommentsMaxResults: 5,
				CommentsStartAt:    0,
				IncludeTransitions: true,
				IncludeSprint:      true,
				DescriptionCap:     800,
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
		pr, err := bitbucketClient.FindOpenPrForBranch(parsed.ProjectKey, parsed.RepoSlug, branch)
		switch {
		case err != nil:
			sections = append(sections, fmt.Sprintf("── Bitbucket (%s/%s) ── (could not fetch PRs)", parsed.ProjectKey, parsed.RepoSlug))
		case pr != nil:
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
			switch {
			case total > 0 && approved == total:
				prLines = append(prLines, "", fmt.Sprintf(`✓ All reviewers approved — ready to merge: bitbucket_mutate {prId: %d, action: "merge"}`, pr.ID))
			case total > 0:
				prLines = append(prLines, "", fmt.Sprintf("→ %d reviewer(s) pending — use bitbucket_get_pr {prId: %d} to see open comments", total-approved, pr.ID))
			default:
				prLines = append(prLines, "", fmt.Sprintf("→ No reviewers assigned — use bitbucket_mutate {prId: %d, update: {reviewers: [...]}} to add them", pr.ID))
			}
			sections = append(sections, strings.Join(prLines, "\n"))
		default:
			noPrHint := []string{
				fmt.Sprintf(`── Bitbucket (%s/%s) ── No open PR for branch "%s"`, parsed.ProjectKey, parsed.RepoSlug, branch),
				fmt.Sprintf(`→ Create one: bitbucket_mutate {create: {title: "...", fromBranch: "%s"}}`, branch),
			}
			sections = append(sections, strings.Join(noPrHint, "\n"))
		}
	}

	return tool.TextResult(strings.Join(sections, "\n\n")), nil
}
