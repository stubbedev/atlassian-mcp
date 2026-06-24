package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// createPullRequest opens a PR, or returns the existing open PR for the branch.
func (c *BitbucketClient) createPullRequest(projectKey, repoSlug, title, description, fromBranch, toBranch string, reviewers []string) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(projectKey, repoSlug)
	if err != nil {
		return toolResult{}, err
	}
	sourceBranch := fromBranch
	if sourceBranch == "" {
		sourceBranch = safeExecGit("rev-parse", "--abbrev-ref", "HEAD")
	}
	if sourceBranch == "" || sourceBranch == "HEAD" {
		return toolResult{}, fmt.Errorf("Could not determine source branch. Provide fromBranch or run from a checked-out branch.")
	}
	sourceBranchName := branchDisplayID(sourceBranch)
	existing, err := c.findOpenPrForBranch(pk, rs, sourceBranchName)
	if err != nil {
		return toolResult{}, err
	}
	if existing != nil {
		u := c.pullRequestURL(pk, rs, existing.ID, existing)
		return textResult(fmt.Sprintf("Open PR already exists for branch %q: #%d %q\n%s", sourceBranchName, existing.ID, existing.Title, u)), nil
	}
	var toRef string
	if toBranch != "" {
		toRef = toBranchRef(toBranch)
	} else {
		toRef, err = c.getDefaultBranchRef(pk, rs)
		if err != nil {
			return toolResult{}, err
		}
	}
	reviewerObjs := make([]map[string]any, 0, len(reviewers))
	for _, name := range reviewers {
		reviewerObjs = append(reviewerObjs, map[string]any{"user": map[string]any{"name": name}})
	}
	body := map[string]any{
		"title":       title,
		"description": description,
		"fromRef":     map[string]any{"id": toBranchRef(sourceBranch), "repository": map[string]any{"slug": rs, "project": map[string]any{"key": pk}}},
		"toRef":       map[string]any{"id": toRef, "repository": map[string]any{"slug": rs, "project": map[string]any{"key": pk}}},
		"reviewers":   reviewerObjs,
	}
	data, err := bbDecode[bbPullRequest](c, "POST", c.rp(pk, rs)+"/pull-requests", body)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil {
		return textResult("Pull request created."), nil
	}
	return textResult(fmt.Sprintf("Created PR #%d: %q\n%s", data.ID, data.Title, c.pullRequestURL(pk, rs, data.ID, data))), nil
}

func (c *BitbucketClient) updatePullRequest(projectKey, repoSlug string, prID int, titleP, descP, toBranchP *string, reviewersP *[]string) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(projectKey, repoSlug)
	if err != nil {
		return toolResult{}, err
	}
	if titleP == nil && descP == nil && toBranchP == nil && reviewersP == nil {
		return toolResult{}, fmt.Errorf("At least one field is required: title, description, toBranch, or reviewers")
	}
	existing, err := bbDecode[bbPullRequest](c, "GET", fmt.Sprintf("%s/pull-requests/%d", c.rp(pk, rs), prID), nil)
	if err != nil {
		return toolResult{}, err
	}
	if existing == nil {
		return toolResult{}, fmt.Errorf("PR #%d not found.", prID)
	}
	buildBody := func(pr *bbPullRequest) map[string]any {
		body := map[string]any{"version": pr.Version}
		if titleP != nil {
			body["title"] = *titleP
		}
		if descP != nil {
			body["description"] = *descP
		}
		if toBranchP != nil {
			body["toRef"] = map[string]any{"id": toBranchRef(*toBranchP), "repository": map[string]any{"slug": rs, "project": map[string]any{"key": pk}}}
		}
		// Always include reviewers so Bitbucket does not clear them on PUT; only
		// replace them when explicitly provided.
		var revs []map[string]any
		if reviewersP != nil {
			for _, name := range *reviewersP {
				revs = append(revs, map[string]any{"user": map[string]any{"name": name}})
			}
		} else {
			for _, r := range pr.Reviewers {
				revs = append(revs, map[string]any{"user": map[string]any{"name": r.User.Name}})
			}
		}
		if revs == nil {
			revs = []map[string]any{}
		}
		body["reviewers"] = revs
		return body
	}
	path := fmt.Sprintf("%s/pull-requests/%d", c.rp(pk, rs), prID)
	updated, err := bbDecode[bbPullRequest](c, "PUT", path, buildBody(existing))
	if err != nil {
		if !strings.Contains(err.Error(), "Bitbucket 409") {
			return toolResult{}, err
		}
		latest, lerr := bbDecode[bbPullRequest](c, "GET", path, nil)
		if lerr != nil || latest == nil {
			return toolResult{}, err
		}
		updated, err = bbDecode[bbPullRequest](c, "PUT", path, buildBody(latest))
		if err != nil {
			return toolResult{}, err
		}
	}
	if updated == nil {
		return textResult(fmt.Sprintf("Updated PR #%d.", prID)), nil
	}
	return textResult(fmt.Sprintf("Updated PR #%d: %q (%s → %s).\n%s", updated.ID, updated.Title, updated.FromRef.DisplayID, updated.ToRef.DisplayID, c.pullRequestURL(pk, rs, updated.ID, updated))), nil
}

func (c *BitbucketClient) mutatePullRequest(args map[string]any) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(argString(args, "projectKey"), argString(args, "repoSlug"))
	if err != nil {
		return toolResult{}, err
	}
	update := argMap(args, "update")
	create := argMap(args, "create")

	titleP, descP, toBranchP, reviewersP := updatePointers(update)
	hasUpdate := update != nil && (titleP != nil || descP != nil || toBranchP != nil || reviewersP != nil)

	if has(args, "prId") {
		prID := argInt(args, "prId")
		if !hasUpdate {
			return c.getPullRequest(pk, rs, prID)
		}
		return c.updatePullRequest(pk, rs, prID, titleP, descP, toBranchP, reviewersP)
	}

	sourceBranch := ""
	if create != nil {
		sourceBranch = argString(create, "fromBranch")
	}
	if sourceBranch == "" {
		sourceBranch = safeExecGit("rev-parse", "--abbrev-ref", "HEAD")
	}
	if sourceBranch == "" || sourceBranch == "HEAD" {
		if create != nil {
			return c.createFromMap(pk, rs, create)
		}
		return toolResult{}, fmt.Errorf("Could not determine source branch. Provide create.fromBranch or run from a checked-out branch.")
	}

	existing, err := c.findOpenPrForBranch(pk, rs, sourceBranch)
	if err != nil {
		return toolResult{}, err
	}
	if existing != nil {
		if hasUpdate {
			return c.updatePullRequest(pk, rs, existing.ID, titleP, descP, toBranchP, reviewersP)
		}
		return c.getPullRequest(pk, rs, existing.ID)
	}
	if create == nil {
		return toolResult{}, fmt.Errorf("No open PR found for branch %q. Provide create to open one.", branchDisplayID(sourceBranch))
	}
	return c.createFromMap(pk, rs, create)
}

func (c *BitbucketClient) createFromMap(pk, rs string, create map[string]any) (toolResult, error) {
	return c.createPullRequest(pk, rs, argString(create, "title"), argString(create, "description"), argString(create, "fromBranch"), argString(create, "toBranch"), argStrSlice(create, "reviewers"))
}

func updatePointers(update map[string]any) (titleP, descP, toBranchP *string, reviewersP *[]string) {
	if update == nil {
		return nil, nil, nil, nil
	}
	if has(update, "title") {
		v := argString(update, "title")
		titleP = &v
	}
	if has(update, "description") {
		v := argString(update, "description")
		descP = &v
	}
	if has(update, "toBranch") {
		v := argString(update, "toBranch")
		toBranchP = &v
	}
	if has(update, "reviewers") {
		reviewersP = argStrSlicePtr(update, "reviewers")
	}
	return
}

func (c *BitbucketClient) approvePr(projectKey, repoSlug string, prID int) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(projectKey, repoSlug)
	if err != nil {
		return toolResult{}, err
	}
	data, err := bbDecode[bbParticipant](c, "POST", fmt.Sprintf("%s/pull-requests/%d/approve", c.rp(pk, rs), prID), nil)
	if err != nil {
		return toolResult{}, err
	}
	u := c.pullRequestURL(pk, rs, prID, nil)
	if data == nil {
		return textResult(fmt.Sprintf("Approved PR #%d.\n%s", prID, u)), nil
	}
	return textResult(fmt.Sprintf("Approved PR #%d as %s.\n%s", prID, data.User.DisplayName, u)), nil
}

func (c *BitbucketClient) unapprovePr(projectKey, repoSlug string, prID int) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(projectKey, repoSlug)
	if err != nil {
		return toolResult{}, err
	}
	if _, err := c.request("DELETE", fmt.Sprintf("%s/pull-requests/%d/approve", c.rp(pk, rs), prID), nil); err != nil {
		return toolResult{}, err
	}
	return textResult(fmt.Sprintf("Approval removed from PR #%d.\n%s", prID, c.pullRequestURL(pk, rs, prID, nil))), nil
}

func (c *BitbucketClient) needsWorkPr(projectKey, repoSlug string, prID int) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(projectKey, repoSlug)
	if err != nil {
		return toolResult{}, err
	}
	userSlug, err := c.getCurrentUsername()
	if err != nil {
		return toolResult{}, err
	}
	data, err := bbDecode[bbParticipant](c, "PUT",
		fmt.Sprintf("%s/pull-requests/%d/participants/%s", c.rp(pk, rs), prID, url.PathEscape(userSlug)),
		map[string]any{"user": map[string]any{"name": userSlug}, "approved": false, "status": "NEEDS_WORK"})
	if err != nil {
		return toolResult{}, err
	}
	u := c.pullRequestURL(pk, rs, prID, nil)
	if data == nil {
		return textResult(fmt.Sprintf("Marked PR #%d as Needs work.\n%s", prID, u)), nil
	}
	return textResult(fmt.Sprintf("Marked PR #%d as Needs work as %s.\n%s", prID, data.User.DisplayName, u)), nil
}

func (c *BitbucketClient) declinePr(projectKey, repoSlug string, prID int, message string) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(projectKey, repoSlug)
	if err != nil {
		return toolResult{}, err
	}
	pr, err := bbDecode[bbPullRequest](c, "GET", fmt.Sprintf("%s/pull-requests/%d", c.rp(pk, rs), prID), nil)
	if err != nil {
		return toolResult{}, err
	}
	if pr == nil {
		return toolResult{}, fmt.Errorf("PR #%d not found.", prID)
	}
	body := map[string]any{"version": pr.Version}
	if message != "" {
		body["message"] = message
	}
	data, err := bbDecode[bbPullRequest](c, "POST", fmt.Sprintf("%s/pull-requests/%d/decline", c.rp(pk, rs), prID), body)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil {
		return textResult(fmt.Sprintf("Declined PR #%d.\n%s", prID, c.pullRequestURL(pk, rs, prID, nil))), nil
	}
	return textResult(fmt.Sprintf("Declined PR #%d: %q.\n%s", data.ID, data.Title, c.pullRequestURL(pk, rs, data.ID, data))), nil
}

func (c *BitbucketClient) mergePr(projectKey, repoSlug string, prID int, mergeStrategy, message string) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(projectKey, repoSlug)
	if err != nil {
		return toolResult{}, err
	}
	pr, err := bbDecode[bbPullRequest](c, "GET", fmt.Sprintf("%s/pull-requests/%d", c.rp(pk, rs), prID), nil)
	if err != nil {
		return toolResult{}, err
	}
	if pr == nil {
		return toolResult{}, fmt.Errorf("PR #%d not found.", prID)
	}
	body := map[string]any{"version": pr.Version}
	if mergeStrategy != "" {
		body["strategyId"] = mergeStrategy
	}
	if message != "" {
		body["message"] = message
	}
	data, err := bbDecode[bbPullRequest](c, "POST", fmt.Sprintf("%s/pull-requests/%d/merge", c.rp(pk, rs), prID), body)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil {
		return textResult(fmt.Sprintf("Merged PR #%d.\n%s", prID, c.pullRequestURL(pk, rs, prID, nil))), nil
	}
	return textResult(fmt.Sprintf("Merged PR #%d: %q (%s → %s).\n%s", data.ID, data.Title, data.FromRef.DisplayID, data.ToRef.DisplayID, c.pullRequestURL(pk, rs, data.ID, data))), nil
}

// bitbucketMutate is the bitbucket_mutate tool entry: action routing, the
// interactive reviewer picker, then mutatePullRequest.
func bitbucketMutate(args map[string]any) (toolResult, error) {
	action := argString(args, "action")
	prID := argInt(args, "prId")
	switch action {
	case "approve":
		return bitbucket.approvePr(argString(args, "projectKey"), argString(args, "repoSlug"), prID)
	case "unapprove":
		return bitbucket.unapprovePr(argString(args, "projectKey"), argString(args, "repoSlug"), prID)
	case "needs_work":
		return bitbucket.needsWorkPr(argString(args, "projectKey"), argString(args, "repoSlug"), prID)
	case "decline":
		return bitbucket.declinePr(argString(args, "projectKey"), argString(args, "repoSlug"), prID, argString(args, "declineMessage"))
	case "merge":
		return bitbucket.mergePr(argString(args, "projectKey"), argString(args, "repoSlug"), prID, argString(args, "mergeStrategy"), argString(args, "mergeMessage"))
	}

	// Interactive reviewer picker for PR creation.
	create := argMap(args, "create")
	if create != nil && argBool(create, "pickReviewers") && !has(args, "prId") {
		users, err := bitbucket.searchUsersRaw(argString(args, "projectKey"), argString(args, "repoSlug"), "", 30)
		if err == nil && len(users) > 0 {
			properties := map[string]any{}
			schemaKeyToUser := map[string]string{}
			for _, u := range users {
				key := schemaKeyRe.ReplaceAllString(u.Name, "_")
				schemaKeyToUser[key] = u.Name
				properties[key] = map[string]any{"type": "boolean", "title": fmt.Sprintf("%s (%s)", u.DisplayName, u.Name)}
			}
			res, eerr := elicit("Select reviewers to add to this PR:", map[string]any{"type": "object", "properties": properties})
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
				create["reviewers"] = toAnySlice(selected)
			}
		}
	}

	return bitbucket.mutatePullRequest(args)
}

var schemaKeyRe = regexp.MustCompile(`[^a-zA-Z0-9]`)

func toAnySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
