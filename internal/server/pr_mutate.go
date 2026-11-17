package server

import (
	"errors"
	"fmt"

	"github.com/stubbedev/atlassian-mcp/internal/arg"
	"github.com/stubbedev/atlassian-mcp/internal/bitbucket"
	"github.com/stubbedev/atlassian-mcp/internal/tool"
)

// bitbucketMutate is the bitbucket_mutate tool entry: action routing, the
// interactive reviewer picker, then mutatePullRequest.
func bitbucketMutate(session *sessionState, args map[string]any, repoRoot string) (tool.Result, error) {
	pk := arg.String(args, "projectKey")
	rs := arg.String(args, "repoSlug")
	action := arg.String(args, "action")
	prID := arg.Int(args, "prId")
	switch action {
	case "approve":
		return bitbucketClient.ApprovePr(pk, rs, repoRoot, prID)
	case "unapprove":
		return bitbucketClient.UnapprovePr(pk, rs, repoRoot, prID)
	case "needs_work":
		return bitbucketClient.NeedsWorkPr(pk, rs, repoRoot, prID)
	case "decline":
		return bitbucketClient.DeclinePr(pk, rs, repoRoot, prID, arg.String(args, "declineMessage"))
	case "merge":
		return bitbucketClient.MergePr(pk, rs, repoRoot, prID, arg.String(args, "mergeStrategy"), arg.String(args, "mergeMessage"))
	case "create":
		if arg.Map(args, "create") == nil {
			return tool.Result{}, errors.New("action=create needs a create object (title, and optionally description/fromBranch/toBranch/reviewers).")
		}
	case "update":
		if arg.Map(args, "update") == nil {
			return tool.Result{}, errors.New("action=update needs an update object (title, description, toBranch, or reviewers).")
		}
	case "":
	default:
		return tool.Result{}, fmt.Errorf("Unknown action %q. Use create, update, approve, unapprove, needs_work, decline, or merge.", action)
	}

	// Interactive reviewer picker for PR creation.
	create := arg.Map(args, "create")
	if create != nil && arg.Bool(create, "pickReviewers") && !arg.Has(args, "prId") {
		users, err := bitbucketClient.SearchUsersRaw(pk, rs, "", 30)
		if err == nil && len(users) > 0 {
			properties := map[string]any{}
			schemaKeyToUser := map[string]string{}
			for _, u := range users {
				key := bitbucket.SchemaKeyRe.ReplaceAllString(u.Name, "_")
				schemaKeyToUser[key] = u.Name
				properties[key] = map[string]any{"type": "boolean", "title": fmt.Sprintf("%s (%s)", u.DisplayName, u.Name)}
			}
			res, eerr := elicit(session, "Select reviewers to add to this PR:", map[string]any{"type": "object", "properties": properties})
			if eerr == nil && res != nil && res.Action == "accept" && res.Content != nil {
				var selected []string
				for key, username := range schemaKeyToUser {
					if b, ok := res.Content[key].(bool); ok && b {
						selected = append(selected, username)
					}
				}
				if selected == nil {
					selected = []string{}
				}
				create["reviewers"] = bitbucket.ToAnySlice(selected)
			}
		}
	}

	return bitbucketClient.MutatePullRequest(args, repoRoot)
}
