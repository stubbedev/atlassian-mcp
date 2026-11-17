package server

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/stubbedev/atlassian-mcp/internal/arg"
	"github.com/stubbedev/atlassian-mcp/internal/bitbucket"
	"github.com/stubbedev/atlassian-mcp/internal/gitrepo"
	"github.com/stubbedev/atlassian-mcp/internal/tool"
)

func issueTypePrefix(issueType string) string {
	t := strings.ToLower(issueType)
	switch t {
	case "bug", "bugfix", "defect":
		return "bugfix"
	case "hotfix":
		return "hotfix"
	case "task", "sub-task", "subtask":
		return "task"
	}
	return "feature"
}

var slugNonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugifyBranchName(issueKey, summary, issueType string) string {
	prefix := issueTypePrefix(issueType)
	slug := strings.ToLower(summary)
	slug = slugNonAlnumRe.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	slug = strings.TrimRight(slug, "-")
	return fmt.Sprintf("%s/%s-%s", prefix, issueKey, slug)
}

var jiraKeyCaptureRe = regexp.MustCompile(`\b([A-Z][A-Z0-9]+-\d+)\b`)

// startWork implements the start_work tool.
func startWork(session *sessionState, args map[string]any, repoRoot string) (tool.Result, error) {
	issueKey := arg.String(args, "issueKey")
	query := arg.String(args, "query")
	if issueKey == "" && query == "" {
		return tool.Result{}, errors.New("Provide issueKey (e.g. FOO-123) or query (free-text search).")
	}

	if issueKey == "" && query != "" {
		candidates, err := jiraClient.FindIssues(query, 10)
		if err != nil {
			return tool.Result{}, err
		}
		if len(candidates) == 0 {
			return tool.TextResult(fmt.Sprintf("No Jira tickets found for: %q", query)), nil
		}
		if len(candidates) == 1 {
			issueKey = candidates[0].Key
		} else {
			pickerMessage := []string{fmt.Sprintf("Found %d tickets matching %q. Which one do you want to work on?", len(candidates), query)}
			var oneOf []any
			for i, c := range candidates {
				pickerMessage = append(pickerMessage, fmt.Sprintf("%d. [%s] %s (%s)", i+1, c.Key, c.Summary, c.Status))
				oneOf = append(oneOf, map[string]any{"const": c.Key, "title": fmt.Sprintf("[%s] %s", c.Key, c.Summary)})
			}
			oneOf = append(oneOf, map[string]any{"const": "__cancel__", "title": "Cancel"})
			schema := map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ticket": map[string]any{"type": "string", "title": "Select a ticket", "oneOf": oneOf},
				},
				"required": []any{"ticket"},
			}
			res, err := elicit(session, strings.Join(pickerMessage, "\n"), schema)
			if err != nil {
				// Elicitation unsupported — list options and ask to retry.
				var list []string
				for _, c := range candidates {
					list = append(list, fmt.Sprintf("  • %s — %s (%s)", c.Key, c.Summary, c.Status))
				}
				return tool.TextResult(fmt.Sprintf("Found %d tickets matching %q:\n%s\n\nRe-run start_work with the desired issueKey.", len(candidates), query, strings.Join(list, "\n"))), nil
			}
			if res.Action == "cancel" || res.Action == "decline" || (res.Content != nil && res.Content["ticket"] == "__cancel__") {
				return tool.TextResult("Canceled."), nil
			}
			if res.Content != nil {
				if t, ok := res.Content["ticket"].(string); ok {
					issueKey = t
				}
			}
			if issueKey == "" || issueKey == "__cancel__" {
				return tool.TextResult("Canceled."), nil
			}
		}
	}

	if issueKey == "" {
		return tool.Result{}, errors.New("Could not resolve issue key.")
	}

	summary, status, issueType, err := jiraClient.GetIssueFields(issueKey)
	if err != nil {
		return tool.Result{}, err
	}
	branchName := arg.String(args, "branchName")
	if branchName == "" {
		branchName = slugifyBranchName(issueKey, summary, issueType)
	}
	repoPath := repoRoot
	if repoPath == "" {
		return tool.Result{}, errors.New("No repo resolved for branch creation. Pass repoPath, or connect a client that provides workspace roots.")
	}

	remote, err := gitrepo.CheckRemoteBranch(branchName, repoPath, credentials)
	if err != nil {
		return tool.Result{}, err
	}
	if remote.Exists {
		var contextLines []string
		if remote.Author != "" {
			contextLines = append(contextLines, "Last author: "+remote.Author)
		}
		if remote.Date != "" {
			commit := fmt.Sprintf("Last commit: %s — %s", remote.Date, remote.Message)
			if remote.SHA != "" {
				commit += fmt.Sprintf(" (%s)", remote.SHA)
			}
			contextLines = append(contextLines, commit)
		}
		messageLines := make([]string, 0, 2+len(contextLines))
		messageLines = append(messageLines,
			fmt.Sprintf("Branch %q already exists on remote.", branchName),
			fmt.Sprintf("Ticket: %s — %s", issueKey, summary),
		)
		messageLines = append(messageLines, contextLines...)
		message := strings.Join(messageLines, "\n")

		schema := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":  "string",
					"title": "What would you like to do?",
					"oneOf": []any{
						map[string]any{"const": "checkout", "title": fmt.Sprintf("Check out existing branch %q", branchName)},
						map[string]any{"const": "new_name", "title": "Use a different branch name (re-run start_work with branchName)"},
						map[string]any{"const": "cancel", "title": "Cancel"},
					},
				},
			},
			"required": []any{"action"},
		}
		res, eerr := elicit(session, message, schema)
		if eerr != nil {
			return tool.TextResult(strings.Join([]string{
				message, "",
				"Options:",
				"  • Check out existing: git checkout --track origin/" + branchName,
				"  • Use a different name: re-run start_work with branchName set",
			}, "\n")), nil
		}
		if res.Action == "cancel" || res.Action == "decline" {
			return tool.TextResult("Canceled."), nil
		}
		if res.Action == "accept" {
			chosen, _ := res.Content["action"].(string)
			switch chosen {
			case "checkout":
				checkout := checkoutRemoteBranch(branchName, repoPath)
				return tool.TextResult(message + "\n\n" + checkout.Content[0].Text), nil
			case "cancel":
				return tool.TextResult("Canceled."), nil
			}
			return tool.TextResult(message + "\n\nRe-run start_work with a custom branchName to proceed."), nil
		}
		return tool.TextResult("Canceled."), nil
	}

	branchResult := createBranch(branchName, arg.String(args, "baseBranch"), repoPath, arg.Bool(args, "push"))
	lines := []string{
		fmt.Sprintf("Ticket:  %s — %s", issueKey, summary),
		"Status:  " + status,
		branchResult.Content[0].Text,
	}
	if tn := arg.String(args, "transitionName"); tn != "" {
		if _, err := jiraClient.MutateIssue(map[string]any{"issueKey": issueKey, "transitionName": tn}, repoRoot); err != nil {
			lines = append(lines, "Jira:    could not transition — "+err.Error())
		} else {
			lines = append(lines, "Jira:    transitioned → "+tn)
		}
	}

	if bitbucketClient != nil {
		remoteURL := gitrepo.OriginURL(repoPath)
		if parsed := bitbucket.ParseRemote(remoteURL); parsed != nil {
			if readme := bitbucketClient.FetchFileText(parsed.ProjectKey, parsed.RepoSlug, "README.md"); readme != "" {
				const maxLen = 4000
				truncated := readme
				if len(readme) > maxLen {
					truncated = readme[:maxLen] + "\n... (truncated)"
				}
				lines = append(lines,
					"",
					"Project conventions (from README.md):",
					"────────────────────────────────────",
					truncated,
					"────────────────────────────────────",
					"Follow the conventions above when writing commit messages and the PR description.")
			}
		}
		lines = append(lines,
			"",
			"Next steps:",
			"  1. Make your changes and commit following the project conventions.",
			"  2. Use bitbucket_mutate (create) to open a PR — the Jira summary and ticket key make a good title/description starting point.",
			"  3. Add reviewers: bitbucket_search resource=users to find colleagues, or pass pickReviewers=true in create to get an interactive picker.")
	}
	return tool.TextResult(strings.Join(lines, "\n")), nil
}

// completeWork implements the complete_work tool.
func completeWork(args map[string]any, repoRoot string) (tool.Result, error) {
	repoPath := repoRoot
	var lines []string

	issueKey := arg.String(args, "issueKey")
	var resolvedPrID int
	hasPrID := arg.Has(args, "prId")
	if hasPrID {
		resolvedPrID = arg.Int(args, "prId")
	} else {
		if repoPath == "" {
			return tool.Result{}, errors.New("No repo resolved. Provide prId, or pass repoPath / connect a client that provides workspace roots.")
		}
		branch := gitrepo.CurrentBranch(repoPath)
		if branch == "" || branch == "HEAD" {
			return tool.Result{}, errors.New("Could not determine current branch. Provide prId or run from a checked-out branch.")
		}
		remote := gitrepo.OriginURL(repoPath)
		parsed := bitbucket.ParseRemote(remote)
		if parsed == nil {
			return tool.Result{}, errors.New("Could not parse Bitbucket remote URL. Provide projectKey/repoSlug explicitly.")
		}
		pk := arg.String(args, "projectKey")
		if pk == "" {
			pk = parsed.ProjectKey
		}
		rs := arg.String(args, "repoSlug")
		if rs == "" {
			rs = parsed.RepoSlug
		}
		pr, err := bitbucketClient.FindOpenPrForBranch(pk, rs, branch)
		if err != nil {
			return tool.Result{}, err
		}
		if pr == nil {
			return tool.Result{}, fmt.Errorf("No open PR found for branch %q. Provide prId explicitly.", branch)
		}
		resolvedPrID = pr.ID
		lines = append(lines, fmt.Sprintf("Branch:  %s → PR #%d", branch, resolvedPrID))
		if issueKey == "" {
			if m := jiraKeyCaptureRe.FindStringSubmatch(branch); m != nil {
				issueKey = m[1]
				lines = append(lines, fmt.Sprintf("Jira:    auto-detected %s from branch name", issueKey))
			}
		}
	}

	if !arg.Bool(args, "force") {
		pk, rs, rerr := bitbucketClient.ResolveProjectAndRepo(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot)
		if rerr == nil {
			if blockers := bitbucketClient.MergeBlockers(pk, rs, resolvedPrID); len(blockers) > 0 {
				return tool.Result{}, fmt.Errorf("PR #%d is not ready to merge: %s. Resolve these first, or pass force=true if the user wants it merged anyway.", resolvedPrID, strings.Join(blockers, "; "))
			}
		}
	}

	mergeResult, err := bitbucketClient.MergePr(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot, resolvedPrID, arg.String(args, "mergeStrategy"), arg.String(args, "mergeMessage"))
	if err != nil {
		return tool.Result{}, err
	}
	lines = append(lines, mergeResult.Content[0].Text)

	skip := arg.Bool(args, "skipJiraTransition")
	if !skip && issueKey != "" {
		transitionName := arg.String(args, "transitionName")
		if transitionName == "" {
			transitionName = "Done"
		}
		if _, err := jiraClient.MutateIssue(map[string]any{"issueKey": issueKey, "transitionName": transitionName}, repoRoot); err != nil {
			lines = append(lines, fmt.Sprintf("Jira:    could not transition %s — %s", issueKey, err.Error()))
		} else {
			lines = append(lines, fmt.Sprintf("Jira:    %s transitioned → %s", issueKey, transitionName))
		}
	} else if !skip {
		lines = append(lines, "Jira:    no issue key — skipped transition (provide issueKey to transition)")
	}

	return tool.TextResult(strings.Join(lines, "\n")), nil
}
