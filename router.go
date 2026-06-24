package main

// runTool routes a tools/call to the matching handler. Service-gated tools
// return MethodNotFound when their service is not configured, exactly as the
// TypeScript dispatcher did.
func runTool(name string, args map[string]any) (toolResult, error) {
	switch name {

	// ── Git (always available) ───────────────────────────────────────────
	case "git_get_context":
		return gitGetContext(args), nil
	case "git_get_diff":
		return gitGetDiffPaged(args), nil

	// ── Combined context ─────────────────────────────────────────────────
	case "get_dev_context":
		if jira == nil && bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return getDevContext(argString(args, "repoPath"))

	// ── Jira ─────────────────────────────────────────────────────────────
	case "start_work":
		if jira == nil {
			return toolResult{}, errUnknownTool
		}
		return startWork(args)

	case "jira_search":
		if jira == nil {
			return toolResult{}, errUnknownTool
		}
		return jira.search(normalizeJiraProjectArgs(args))

	case "jira_get":
		if jira == nil {
			return toolResult{}, errUnknownTool
		}
		return jira.issueOverview(issueOverviewOptsFromArgs(args))

	case "jira_mutate":
		if jira == nil {
			return toolResult{}, errUnknownTool
		}
		return jira.mutateIssue(normalizeJiraMutateArgs(args))

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
		return jira.mutateVersion(normalizeJiraProjectArgs(args))

	// ── Bitbucket ────────────────────────────────────────────────────────
	case "bitbucket_search":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucket.search(normalizeBitbucketArgs(args))

	case "bitbucket_get_pr":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucket.getPrOverview(normalizeBitbucketArgs(args))

	case "bitbucket_mutate":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucketMutate(normalizeBitbucketArgs(args))

	case "bitbucket_comment":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucket.commentDispatch(normalizeBitbucketArgs(args))

	case "bitbucket_get_file":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucket.getFile(normalizeBitbucketArgs(args))

	case "bitbucket_get_attachment":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		args = normalizeBitbucketArgs(args)
		if rerr := validateAttachmentArgs(args); rerr != nil {
			return toolResult{}, rerr
		}
		return bitbucket.getAttachment(args)

	case "bitbucket_pr_tasks":
		if bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return bitbucket.prTasksDispatch(normalizeBitbucketArgs(args))

	// ── Combined workflow ────────────────────────────────────────────────
	case "complete_work":
		if jira == nil || bitbucket == nil {
			return toolResult{}, errUnknownTool
		}
		return completeWork(args)

	default:
		return toolResult{}, errUnknownTool
	}
}
