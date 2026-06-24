package main

import (
	"fmt"
	"regexp"
	"strings"
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
func startWork(session *sessionState, args map[string]any, repoRoot string) (toolResult, error) {
	issueKey := argString(args, "issueKey")
	query := argString(args, "query")
	if issueKey == "" && query == "" {
		return toolResult{}, fmt.Errorf("Provide issueKey (e.g. FOO-123) or query (free-text search).")
	}

	if issueKey == "" && query != "" {
		candidates, err := jira.findIssues(query, 10)
		if err != nil {
			return toolResult{}, err
		}
		if len(candidates) == 0 {
			return textResult(fmt.Sprintf("No Jira tickets found for: %q", query)), nil
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
				return textResult(fmt.Sprintf("Found %d tickets matching %q:\n%s\n\nRe-run start_work with the desired issueKey.", len(candidates), query, strings.Join(list, "\n"))), nil
			}
			if res.Action == "cancel" || res.Action == "decline" || (res.Content != nil && res.Content["ticket"] == "__cancel__") {
				return textResult("Cancelled."), nil
			}
			if res.Content != nil {
				if t, ok := res.Content["ticket"].(string); ok {
					issueKey = t
				}
			}
			if issueKey == "" || issueKey == "__cancel__" {
				return textResult("Cancelled."), nil
			}
		}
	}

	if issueKey == "" {
		return toolResult{}, fmt.Errorf("Could not resolve issue key.")
	}

	summary, status, issueType, err := jira.getIssueFields(issueKey)
	if err != nil {
		return toolResult{}, err
	}
	branchName := argString(args, "branchName")
	if branchName == "" {
		branchName = slugifyBranchName(issueKey, summary, issueType)
	}
	repoPath := repoRoot
	if repoPath == "" {
		return toolResult{}, fmt.Errorf("No repo resolved for branch creation. Pass repoPath, or connect a client that provides workspace roots.")
	}

	remote, err := checkRemoteBranch(branchName, repoPath)
	if err != nil {
		return toolResult{}, err
	}
	if remote.exists {
		var contextLines []string
		if remote.author != "" {
			contextLines = append(contextLines, "Last author: "+remote.author)
		}
		if remote.date != "" {
			commit := fmt.Sprintf("Last commit: %s — %s", remote.date, remote.message)
			if remote.sha != "" {
				commit += fmt.Sprintf(" (%s)", remote.sha)
			}
			contextLines = append(contextLines, commit)
		}
		messageLines := []string{
			fmt.Sprintf("Branch %q already exists on remote.", branchName),
			fmt.Sprintf("Ticket: %s — %s", issueKey, summary),
		}
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
			return textResult(strings.Join([]string{
				message, "",
				"Options:",
				fmt.Sprintf("  • Check out existing: git checkout --track origin/%s", branchName),
				"  • Use a different name: re-run start_work with branchName set",
			}, "\n")), nil
		}
		if res.Action == "cancel" || res.Action == "decline" {
			return textResult("Cancelled."), nil
		}
		if res.Action == "accept" {
			chosen, _ := res.Content["action"].(string)
			switch chosen {
			case "checkout":
				checkout := checkoutRemoteBranch(branchName, repoPath)
				return textResult(message + "\n\n" + checkout.Content[0].Text), nil
			case "cancel":
				return textResult("Cancelled."), nil
			}
			return textResult(message + "\n\nRe-run start_work with a custom branchName to proceed."), nil
		}
		return textResult("Cancelled."), nil
	}

	branchResult := createBranch(branchName, argString(args, "baseBranch"), repoPath, argBool(args, "push"))
	lines := []string{
		fmt.Sprintf("Ticket:  %s — %s", issueKey, summary),
		"Status:  " + status,
		branchResult.Content[0].Text,
	}
	if tn := argString(args, "transitionName"); tn != "" {
		if _, err := jira.mutateIssue(map[string]any{"issueKey": issueKey, "transitionName": tn}, repoRoot); err != nil {
			lines = append(lines, "Jira:    could not transition — "+err.Error())
		} else {
			lines = append(lines, "Jira:    transitioned → "+tn)
		}
	}

	if bitbucket != nil {
		remoteURL := safeGit(repoPath, "", "remote", "get-url", "origin")
		if parsed := parseBitbucketRemote(remoteURL); parsed != nil {
			if readme := bitbucket.fetchFileText(parsed.projectKey, parsed.repoSlug, "README.md"); readme != "" {
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
	return textResult(strings.Join(lines, "\n")), nil
}

// completeWork implements the complete_work tool.
func completeWork(args map[string]any, repoRoot string) (toolResult, error) {
	repoPath := repoRoot
	var lines []string

	issueKey := argString(args, "issueKey")
	resolvedPrID := 0
	hasPrID := has(args, "prId")
	if hasPrID {
		resolvedPrID = argInt(args, "prId")
	} else {
		if repoPath == "" {
			return toolResult{}, fmt.Errorf("No repo resolved. Provide prId, or pass repoPath / connect a client that provides workspace roots.")
		}
		branch := safeGit(repoPath, "", "rev-parse", "--abbrev-ref", "HEAD")
		if branch == "" || branch == "HEAD" {
			return toolResult{}, fmt.Errorf("Could not determine current branch. Provide prId or run from a checked-out branch.")
		}
		remote := safeGit(repoPath, "", "remote", "get-url", "origin")
		parsed := parseBitbucketRemote(remote)
		if parsed == nil {
			return toolResult{}, fmt.Errorf("Could not parse Bitbucket remote URL. Provide projectKey/repoSlug explicitly.")
		}
		pk := argString(args, "projectKey")
		if pk == "" {
			pk = parsed.projectKey
		}
		rs := argString(args, "repoSlug")
		if rs == "" {
			rs = parsed.repoSlug
		}
		pr, err := bitbucket.findOpenPrForBranch(pk, rs, branch)
		if err != nil {
			return toolResult{}, err
		}
		if pr == nil {
			return toolResult{}, fmt.Errorf("No open PR found for branch %q. Provide prId explicitly.", branch)
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

	mergeResult, err := bitbucket.mergePr(argString(args, "projectKey"), argString(args, "repoSlug"), repoRoot, resolvedPrID, argString(args, "mergeStrategy"), argString(args, "mergeMessage"))
	if err != nil {
		return toolResult{}, err
	}
	lines = append(lines, mergeResult.Content[0].Text)

	skip := argBool(args, "skipJiraTransition")
	if !skip && issueKey != "" {
		transitionName := argString(args, "transitionName")
		if transitionName == "" {
			transitionName = "Done"
		}
		if _, err := jira.mutateIssue(map[string]any{"issueKey": issueKey, "transitionName": transitionName}, repoRoot); err != nil {
			lines = append(lines, fmt.Sprintf("Jira:    could not transition %s — %s", issueKey, err.Error()))
		} else {
			lines = append(lines, fmt.Sprintf("Jira:    %s transitioned → %s", issueKey, transitionName))
		}
	} else if !skip {
		lines = append(lines, "Jira:    no issue key — skipped transition (provide issueKey to transition)")
	}

	return textResult(strings.Join(lines, "\n")), nil
}
