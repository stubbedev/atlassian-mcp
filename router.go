package main

// runTool routes a tools/call to the matching handler. Repo-dependent tools
// resolve their repo root from the client (repoPath arg → MCP roots → cwd in
// stdio) rather than the process cwd. Service-gated tools return MethodNotFound
// when their service is not configured.
func runTool(session *sessionState, name string, args map[string]any) (toolResult, error) {
	// The SDK does not validate arguments for untyped tools, so enums and
	// required fields are enforced here before any handler sees them.
	if rerr := validateToolArgs(name, args); rerr != nil {
		return toolResult{}, rerr
	}
	switch name {
	// ── Git (always available) ───────────────────────────────────────────
	case "git_get_context":
		return gitGetContext(args, resolveRepoRoot(session, args)), nil

	// ── Combined context ─────────────────────────────────────────────────
	case "get_dev_context":
		if jiraClient == nil && bitbucketClient == nil {
			return toolResult{}, errUnknownTool
		}
		return getDevContext(resolveRepoRoot(session, args))

	case "get_attachment":
		if jiraClient == nil && bitbucketClient == nil {
			return toolResult{}, errUnknownTool
		}
		return getAttachmentDispatch(session, args)

	case "attach_files":
		if jiraClient == nil && bitbucketClient == nil {
			return toolResult{}, errUnknownTool
		}
		return attachFiles(session, args)

	// ── Jira ─────────────────────────────────────────────────────────────
	case "start_work":
		if jiraClient == nil {
			return toolResult{}, errUnknownTool
		}
		return startWork(session, args, resolveRepoRoot(session, args))

	case "jira_search":
		if jiraClient == nil {
			return toolResult{}, errUnknownTool
		}
		return jiraClient.search(normalizeJiraProjectArgs(args), resolveRepoRoot(session, args))

	case "jira_get":
		if jiraClient == nil {
			return toolResult{}, errUnknownTool
		}
		opts, err := jiraClient.issueOverviewOptsFromArgs(args)
		if err != nil {
			return toolResult{}, err
		}
		return jiraClient.issueOverview(opts)

	case "jira_mutate":
		if jiraClient == nil {
			return toolResult{}, errUnknownTool
		}
		return jiraClient.mutateIssue(normalizeJiraMutateArgs(args), resolveRepoRoot(session, args))

	// ── Bitbucket ────────────────────────────────────────────────────────
	case "bitbucket_search":
		if bitbucketClient == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucketClient.search(normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_get_pr":
		if bitbucketClient == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucketClient.getPrOverview(session.prReviews(), normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_mutate":
		if bitbucketClient == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucketMutate(session, normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_comment":
		if bitbucketClient == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucketClient.commentDispatch(session.prReviews(), normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_get_file":
		if bitbucketClient == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucketClient.getFile(normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_pr_tasks":
		if bitbucketClient == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucketClient.prTasksDispatch(normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	// ── Combined workflow ────────────────────────────────────────────────
	case "complete_work":
		if jiraClient == nil || bitbucketClient == nil {
			return toolResult{}, errUnknownTool
		}
		return completeWork(args, resolveRepoRoot(session, args))

	default:
		return toolResult{}, errUnknownTool
	}
}
