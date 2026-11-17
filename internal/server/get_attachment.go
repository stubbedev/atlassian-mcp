package server

import (
	"errors"

	"github.com/stubbedev/atlassian-mcp/internal/arg"
	"github.com/stubbedev/atlassian-mcp/internal/tool"
)

// getAttachmentDispatch is the get_attachment tool entry: one tool over both
// services, since the two only differ in how the bytes are fetched. source is
// optional when only one service is configured — there is nothing to disambiguate.
func getAttachmentDispatch(session *sessionState, args map[string]any) (tool.Result, error) {
	args = normalizeBitbucketArgs(args)
	if rerr := validateAttachmentArgs(args); rerr != nil {
		return tool.Result{}, rerr
	}
	source := arg.String(args, "source")
	if source == "" {
		switch {
		case jiraClient != nil && bitbucketClient == nil:
			source = "jira"
		case bitbucketClient != nil && jiraClient == nil:
			source = "bitbucket"
		default:
			return tool.Result{}, errors.New("source is required: \"jira\" for an issue attachment (IDs from jira_get) or \"bitbucket\" for a repo attachment (IDs from bitbucket_get_pr).")
		}
	}
	if source == "jira" {
		if jiraClient == nil {
			return tool.Result{}, errors.New("Jira is not configured.")
		}
		return jiraClient.GetAttachment(args)
	}
	if bitbucketClient == nil {
		return tool.Result{}, errors.New("Bitbucket is not configured.")
	}
	return bitbucketClient.GetAttachment(args, resolveRepoRoot(session, args))
}
