package main

// runTool routes a tools/call to the matching handler. Repo-dependent tools
// resolve their repo root from the client (repoPath arg → MCP roots → cwd in
// stdio) rather than the process cwd. Service-gated tools return MethodNotFound
// when their service is not configured.
func runTool(session *sessionState, name string, args map[string]any) (toolResult, error) {
	switch name {

	// ── Git (always available) ───────────────────────────────────────────
	case "git_get_context":
		return gitGetContext(args, resolveRepoRoot(session, args)), nil
	case "git_get_diff":
		return gitGetDiffPaged(args, resolveRepoRoot(session, args)), nil

	// ── Combined context ─────────────────────────────────────────────────
	case "get_dev_context":
		if jira == nil && bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return getDevContext(resolveRepoRoot(session, args))

	// ── Jira ─────────────────────────────────────────────────────────────
	case "start_work":
		if jira == nil {
			return toolResult{}, errUnknownTool
		}
		return startWork(session, args, resolveRepoRoot(session, args))

	case "jira_search":
		if jira == nil {
			return toolResult{}, errUnknownTool
		}
		return jira.search(normalizeJiraProjectArgs(args), resolveRepoRoot(session, args))

	case "jira_get":
		if jira == nil {
			return toolResult{}, errUnknownTool
		}
		return jira.issueOverview(issueOverviewOptsFromArgs(args))

	case "jira_mutate":
		if jira == nil {
			return toolResult{}, errUnknownTool
		}
		return jira.mutateIssue(normalizeJiraMutateArgs(args), resolveRepoRoot(session, args))

	case "jira_get_attachment":
		if jira == nil {
			return toolResult{}, errUnknownTool
		}
		if rerr := validateAttachmentArgs(args); rerr != nil {
			return toolResult{}, rerr
		}
		return jira.getAttachment(args)

	case "jira_comment":
		if jira == nil {
			return toolResult{}, errUnknownTool
		}
		return jira.comment(normalizeJiraProjectArgs(args))

	case "jira_version":
		if jira == nil {
			return toolResult{}, errUnknownTool
		}
		return jira.mutateVersion(normalizeJiraProjectArgs(args), resolveRepoRoot(session, args))

	// ── Bitbucket ────────────────────────────────────────────────────────
	case "bitbucket_search":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucket.search(normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_get_pr":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucket.getPrOverview(normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_mutate":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucketMutate(session, normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_comment":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucket.commentDispatch(normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_get_file":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucket.getFile(normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_get_attachment":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		args = normalizeBitbucketArgs(args)
		if rerr := validateAttachmentArgs(args); rerr != nil {
			return toolResult{}, rerr
		}
		return bitbucket.getAttachment(args, resolveRepoRoot(session, args))

	case "bitbucket_pr_tasks":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucket.prTasksDispatch(normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	// ── Combined workflow ────────────────────────────────────────────────
	case "complete_work":
		if jira == nil || bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return completeWork(args, resolveRepoRoot(session, args))

	default:
		return toolResult{}, errUnknownTool
	}
}
