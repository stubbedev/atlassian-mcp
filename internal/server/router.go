package server

import "github.com/stubbedev/atlassian-mcp/internal/tool"

// runTool routes a tools/call to the matching handler. Repo-dependent tools
// resolve their repo root from the client (repoPath arg → MCP roots → cwd in
// stdio) rather than the process cwd. Service-gated tools return MethodNotFound
// when their service is not configured.
func runTool(session *sessionState, name string, args map[string]any) (tool.Result, error) {
	// The SDK does not validate arguments for untyped tools, so enums and
	// required fields are enforced here before any handler sees them.
	if rerr := validateToolArgs(name, args); rerr != nil {
		return tool.Result{}, rerr
	}
	switch name {
	// ── Git (always available) ───────────────────────────────────────────
	case "git_get_context":
		return gitGetContext(args, resolveRepoRoot(session, args)), nil

	// ── Combined context ─────────────────────────────────────────────────
	case "get_dev_context":
		if jiraClient == nil && bitbucketClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return getDevContext(resolveRepoRoot(session, args))

	case "get_attachment":
		if jiraClient == nil && bitbucketClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return getAttachmentDispatch(session, args)

	case "attach_files":
		if jiraClient == nil && bitbucketClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return attachFiles(session, args)

	// ── Jira ─────────────────────────────────────────────────────────────
	case "start_work":
		if jiraClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return startWork(session, args, resolveRepoRoot(session, args))

	case "jira_search":
		if jiraClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return jiraClient.Search(normalizeJiraProjectArgs(args), resolveRepoRoot(session, args))

	case "jira_get":
		if jiraClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		opts, err := jiraClient.IssueOverviewOptsFromArgs(args)
		if err != nil {
			return tool.Result{}, err
		}
		return jiraClient.IssueOverview(opts)

	case "jira_mutate":
		if jiraClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return jiraClient.MutateIssue(normalizeJiraMutateArgs(args), resolveRepoRoot(session, args))

	// ── Bitbucket ────────────────────────────────────────────────────────
	case "bitbucket_search":
		if bitbucketClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return bitbucketClient.Search(normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_get_pr":
		if bitbucketClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return bitbucketClient.GetPrOverview(session.prReviews(), normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_mutate":
		if bitbucketClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return bitbucketMutate(session, normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_comment":
		if bitbucketClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return bitbucketClient.CommentDispatch(session.prReviews(), normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_get_file":
		if bitbucketClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return bitbucketClient.GetFile(normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	case "bitbucket_pr_tasks":
		if bitbucketClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return bitbucketClient.PrTasksDispatch(normalizeBitbucketArgs(args), resolveRepoRoot(session, args))

	// ── Combined workflow ────────────────────────────────────────────────
	case "complete_work":
		if jiraClient == nil || bitbucketClient == nil {
			return tool.Result{}, tool.ErrUnknownTool
		}
		return completeWork(args, resolveRepoRoot(session, args))

	default:
		return tool.Result{}, tool.ErrUnknownTool
	}
}
