package bitbucket

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/stubbedev/atlassian-mcp/internal/arg"
	"github.com/stubbedev/atlassian-mcp/internal/gitrepo"
	"github.com/stubbedev/atlassian-mcp/internal/logx"
	"github.com/stubbedev/atlassian-mcp/internal/markup"
	"github.com/stubbedev/atlassian-mcp/internal/text"
	"github.com/stubbedev/atlassian-mcp/internal/tool"
)

// resolveReviewers maps caller-supplied reviewer strings to real Bitbucket
// usernames. A display name or an email prefix looks plausible and is silently
// dropped by Bitbucket, so an unmatched name is an error with the candidates
// attached rather than a PR that quietly has no reviewers. When the user lookup
// itself fails (no permission on the repo, for instance) the names are passed
// through untouched — validation must not be the reason a PR cannot be opened.
func (c *Client) resolveReviewers(pk, rs string, names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		users, err := c.SearchUsersRaw(pk, rs, name, 10)
		if err != nil {
			logx.Logf("reviewer lookup for %q failed, using it as given: %v", name, err)
			out = append(out, name)
			continue
		}
		matched := ""
		var candidates []string
		for _, u := range users {
			if strings.EqualFold(u.Name, name) {
				matched = u.Name
				break
			}
			candidates = append(candidates, fmt.Sprintf("%s (%s)", u.Name, u.DisplayName))
		}
		if matched == "" {
			if len(candidates) == 0 {
				return nil, fmt.Errorf("No Bitbucket user matches reviewer %q. Find the username with bitbucket_search resource=users.", name)
			}
			return nil, fmt.Errorf("Reviewer %q is not a Bitbucket username. Did you mean: %s? Usernames come from bitbucket_search resource=users.", name, strings.Join(candidates, ", "))
		}
		out = append(out, matched)
	}
	return text.Unique(out), nil
}

// createPullRequest opens a PR, or returns the existing open PR for the branch.
func (c *Client) createPullRequest(projectKey, repoSlug, repoRoot, title, description, fromBranch, toBranch string, reviewers, attachments []string) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	sourceBranch := fromBranch
	if sourceBranch == "" && repoRoot != "" {
		sourceBranch = gitrepo.CurrentBranch(repoRoot)
	}
	if sourceBranch == "" || sourceBranch == "HEAD" {
		return tool.Result{}, errors.New("Could not determine source branch. Provide create.fromBranch or pass repoPath / connect a client with workspace roots.")
	}
	sourceBranchName := branchDisplayID(sourceBranch)
	existing, err := c.FindOpenPrForBranch(pk, rs, sourceBranchName)
	if err != nil {
		return tool.Result{}, err
	}
	if existing != nil {
		u := c.pullRequestURL(pk, rs, existing.ID, existing)
		return tool.TextResult(fmt.Sprintf("Open PR already exists for branch %q: #%d %q\n%s", sourceBranchName, existing.ID, existing.Title, u)), nil
	}
	var toRef string
	if toBranch != "" {
		toRef = toBranchRef(toBranch)
	} else {
		toRef = c.getDefaultBranchRef(pk, rs, repoRoot)
	}
	resolvedReviewers, err := c.resolveReviewers(pk, rs, reviewers)
	if err != nil {
		return tool.Result{}, err
	}
	var uploaded []bbUploadedAttachment
	if len(attachments) > 0 {
		description, uploaded, err = c.attachToText(pk, rs, description, attachments)
		if err != nil {
			return tool.Result{}, err
		}
	}
	description = markup.AppendAIMarkerMarkdown(description, markup.AuthoredByAI)
	reviewerObjs := make([]map[string]any, 0, len(resolvedReviewers))
	for _, name := range resolvedReviewers {
		reviewerObjs = append(reviewerObjs, map[string]any{"user": map[string]any{"name": name}})
	}
	body := map[string]any{
		"title":       title,
		"description": description,
		"fromRef":     map[string]any{"id": toBranchRef(sourceBranch), "repository": map[string]any{"slug": rs, "project": map[string]any{"key": pk}}},
		"toRef":       map[string]any{"id": toRef, "repository": map[string]any{"slug": rs, "project": map[string]any{"key": pk}}},
		"reviewers":   reviewerObjs,
	}
	data, err := bbDecode[PullRequest](c, "POST", c.rp(pk, rs)+"/pull-requests", body)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil {
		return tool.TextResult("Pull request created." + attachmentSummary(uploaded)), nil
	}
	return tool.TextResult(fmt.Sprintf("Created PR #%d: %q\n%s%s", data.ID, data.Title, c.pullRequestURL(pk, rs, data.ID, data), attachmentSummary(uploaded))), nil
}

func (c *Client) UpdatePullRequest(projectKey, repoSlug, repoRoot string, prID int, titleP, descP, toBranchP *string, reviewersP *[]string, replaceReviewers bool, attachments []string) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	if titleP == nil && descP == nil && toBranchP == nil && reviewersP == nil && len(attachments) == 0 {
		return tool.Result{}, errors.New("At least one field is required: title, description, toBranch, reviewers, or attachments")
	}
	existing, err := bbDecode[PullRequest](c, "GET", fmt.Sprintf("%s/pull-requests/%d", c.rp(pk, rs), prID), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if existing == nil {
		return tool.Result{}, fmt.Errorf("PR #%d not found.", prID)
	}
	// Attachments without a new description go onto the description the PR
	// already has — an uploaded file nothing references is invisible.
	var uploaded []bbUploadedAttachment
	if len(attachments) > 0 {
		base := existing.Description
		if descP != nil {
			base = *descP
		}
		desc, ups, aerr := c.attachToText(pk, rs, base, attachments)
		if aerr != nil {
			return tool.Result{}, aerr
		}
		descP, uploaded = &desc, ups
	}
	if descP != nil {
		marked := markup.AppendAIMarkerMarkdown(*descP, markup.AuthoredByAI)
		descP = &marked
	}
	if reviewersP != nil {
		resolved, rerr := c.resolveReviewers(pk, rs, *reviewersP)
		if rerr != nil {
			return tool.Result{}, rerr
		}
		// A reviewer list is a replacement, not a merge, so a list that just
		// forgets someone silently un-invites them. Dropping a reviewer has to
		// be asked for.
		if !replaceReviewers {
			var dropped []string
			for _, r := range existing.Reviewers {
				keep := false
				for _, name := range resolved {
					if strings.EqualFold(name, r.User.Name) {
						keep = true
						break
					}
				}
				if !keep {
					dropped = append(dropped, r.User.Name)
				}
			}
			if len(dropped) > 0 {
				return tool.Result{}, fmt.Errorf("This would remove existing reviewer(s) %s from PR #%d. Include them in update.reviewers to keep them, or pass update.replaceReviewers=true if the user asked for them to be removed.", strings.Join(dropped, ", "), prID)
			}
		}
		reviewersP = &resolved
	}
	buildBody := func(pr *PullRequest) map[string]any {
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
	updated, err := bbDecode[PullRequest](c, "PUT", path, buildBody(existing))
	if err != nil {
		if !strings.Contains(err.Error(), "Bitbucket 409") {
			return tool.Result{}, err
		}
		latest, lerr := bbDecode[PullRequest](c, "GET", path, nil)
		if lerr != nil || latest == nil {
			return tool.Result{}, err
		}
		updated, err = bbDecode[PullRequest](c, "PUT", path, buildBody(latest))
		if err != nil {
			return tool.Result{}, err
		}
	}
	if updated == nil {
		return tool.TextResult(fmt.Sprintf("Updated PR #%d.%s", prID, attachmentSummary(uploaded))), nil
	}
	return tool.TextResult(fmt.Sprintf("Updated PR #%d: %q (%s → %s).\n%s%s", updated.ID, updated.Title, updated.FromRef.DisplayID, updated.ToRef.DisplayID, c.pullRequestURL(pk, rs, updated.ID, updated), attachmentSummary(uploaded))), nil
}

func (c *Client) MutatePullRequest(args map[string]any, repoRoot string) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot)
	if err != nil {
		// Resolution may fail when no repo is available and only `create` is
		// given with an explicit fromBranch — handle the create-only path below.
		if !arg.Has(args, "prId") && arg.Map(args, "create") != nil {
			create := arg.Map(args, "create")
			if arg.String(create, "fromBranch") != "" || repoRoot != "" {
				return c.createFromMap(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot, create)
			}
		}
		return tool.Result{}, err
	}
	update := arg.Map(args, "update")
	create := arg.Map(args, "create")

	titleP, descP, toBranchP, reviewersP := updatePointers(update)
	updateAttachments := arg.StrSlice(update, "attachments")
	hasUpdate := update != nil && (titleP != nil || descP != nil || toBranchP != nil || reviewersP != nil || len(updateAttachments) > 0)

	if arg.Has(args, "prId") {
		prID := arg.Int(args, "prId")
		if !hasUpdate {
			return c.getPullRequest(pk, rs, "", prID)
		}
		return c.UpdatePullRequest(pk, rs, "", prID, titleP, descP, toBranchP, reviewersP, arg.Bool(update, "replaceReviewers"), updateAttachments)
	}

	sourceBranch := ""
	if create != nil {
		sourceBranch = arg.String(create, "fromBranch")
	}
	if sourceBranch == "" && repoRoot != "" {
		sourceBranch = gitrepo.CurrentBranch(repoRoot)
	}
	if sourceBranch == "" || sourceBranch == "HEAD" {
		if create != nil {
			return c.createFromMap(pk, rs, repoRoot, create)
		}
		return tool.Result{}, errors.New("Could not determine source branch. Provide create.fromBranch, pass repoPath, or connect a client with workspace roots.")
	}

	existing, err := c.FindOpenPrForBranch(pk, rs, sourceBranch)
	if err != nil {
		return tool.Result{}, err
	}
	if existing != nil {
		if hasUpdate {
			return c.UpdatePullRequest(pk, rs, "", existing.ID, titleP, descP, toBranchP, reviewersP, arg.Bool(update, "replaceReviewers"), updateAttachments)
		}
		return c.getPullRequest(pk, rs, "", existing.ID)
	}
	if create == nil {
		return tool.Result{}, fmt.Errorf("No open PR found for branch %q. Provide create to open one.", branchDisplayID(sourceBranch))
	}
	return c.createFromMap(pk, rs, repoRoot, create)
}

func (c *Client) createFromMap(pk, rs, repoRoot string, create map[string]any) (tool.Result, error) {
	return c.createPullRequest(pk, rs, repoRoot, arg.String(create, "title"), arg.String(create, "description"), arg.String(create, "fromBranch"), arg.String(create, "toBranch"), arg.StrSlice(create, "reviewers"), arg.StrSlice(create, "attachments"))
}

func updatePointers(update map[string]any) (titleP, descP, toBranchP *string, reviewersP *[]string) {
	if update == nil {
		return nil, nil, nil, nil
	}
	if arg.Has(update, "title") {
		v := arg.String(update, "title")
		titleP = &v
	}
	if arg.Has(update, "description") {
		v := arg.String(update, "description")
		descP = &v
	}
	if arg.Has(update, "toBranch") {
		v := arg.String(update, "toBranch")
		toBranchP = &v
	}
	if arg.Has(update, "reviewers") {
		reviewersP = arg.StrSlicePtr(update, "reviewers")
	}
	return
}

func (c *Client) ApprovePr(projectKey, repoSlug, repoRoot string, prID int) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	data, err := bbDecode[bbParticipant](c, "POST", fmt.Sprintf("%s/pull-requests/%d/approve", c.rp(pk, rs), prID), nil)
	if err != nil {
		return tool.Result{}, err
	}
	u := c.pullRequestURL(pk, rs, prID, nil)
	if data == nil {
		return tool.TextResult(fmt.Sprintf("Approved PR #%d.\n%s", prID, u)), nil
	}
	return tool.TextResult(fmt.Sprintf("Approved PR #%d as %s.\n%s", prID, data.User.DisplayName, u)), nil
}

func (c *Client) UnapprovePr(projectKey, repoSlug, repoRoot string, prID int) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	if _, err := c.request("DELETE", fmt.Sprintf("%s/pull-requests/%d/approve", c.rp(pk, rs), prID), nil); err != nil {
		return tool.Result{}, err
	}
	return tool.TextResult(fmt.Sprintf("Approval removed from PR #%d.\n%s", prID, c.pullRequestURL(pk, rs, prID, nil))), nil
}

func (c *Client) NeedsWorkPr(projectKey, repoSlug, repoRoot string, prID int) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	userSlug, err := c.getCurrentUsername()
	if err != nil {
		return tool.Result{}, err
	}
	data, err := bbDecode[bbParticipant](c, "PUT",
		fmt.Sprintf("%s/pull-requests/%d/participants/%s", c.rp(pk, rs), prID, url.PathEscape(userSlug)),
		map[string]any{"user": map[string]any{"name": userSlug}, "approved": false, "status": "NEEDS_WORK"})
	if err != nil {
		return tool.Result{}, err
	}
	u := c.pullRequestURL(pk, rs, prID, nil)
	if data == nil {
		return tool.TextResult(fmt.Sprintf("Marked PR #%d as Needs work.\n%s", prID, u)), nil
	}
	return tool.TextResult(fmt.Sprintf("Marked PR #%d as Needs work as %s.\n%s", prID, data.User.DisplayName, u)), nil
}

func (c *Client) DeclinePr(projectKey, repoSlug, repoRoot string, prID int, message string) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	pr, err := bbDecode[PullRequest](c, "GET", fmt.Sprintf("%s/pull-requests/%d", c.rp(pk, rs), prID), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if pr == nil {
		return tool.Result{}, fmt.Errorf("PR #%d not found.", prID)
	}
	body := map[string]any{"version": pr.Version}
	if message != "" {
		body["message"] = message
	}
	data, err := bbDecode[PullRequest](c, "POST", fmt.Sprintf("%s/pull-requests/%d/decline", c.rp(pk, rs), prID), body)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil {
		return tool.TextResult(fmt.Sprintf("Declined PR #%d.\n%s", prID, c.pullRequestURL(pk, rs, prID, nil))), nil
	}
	return tool.TextResult(fmt.Sprintf("Declined PR #%d: %q.\n%s", data.ID, data.Title, c.pullRequestURL(pk, rs, data.ID, data))), nil
}

// MergeBlockers lists the reasons a PR looks unfinished: reviewers who have not
// approved, and failed builds on its head commit. Bitbucket only refuses a merge
// when the repository has merge checks configured, so a workflow tool that
// merges on the caller's behalf checks for itself rather than trusting prose.
func (c *Client) MergeBlockers(projectKey, repoSlug string, prID int) []string {
	pr, err := bbDecode[PullRequest](c, "GET", fmt.Sprintf("%s/pull-requests/%d", c.rp(projectKey, repoSlug), prID), nil)
	if err != nil || pr == nil {
		return nil
	}
	var blockers []string
	var pending []string
	for _, r := range pr.Reviewers {
		if !r.Approved {
			name := r.User.DisplayName
			if name == "" {
				name = r.User.Name
			}
			pending = append(pending, name)
		}
	}
	if len(pending) > 0 {
		blockers = append(blockers, fmt.Sprintf("%d reviewer(s) have not approved: %s", len(pending), strings.Join(pending, ", ")))
	}
	if pr.FromRef.LatestCommit != "" {
		if statuses, serr := c.requestBuildStatus("/commits/" + pr.FromRef.LatestCommit); serr == nil && statuses != nil {
			for _, st := range statuses.Values {
				if strings.EqualFold(st.State, "FAILED") {
					name := st.Name
					if name == "" {
						name = st.Key
					}
					blockers = append(blockers, "failed build: "+name)
				}
			}
		}
	}
	return blockers
}

func (c *Client) MergePr(projectKey, repoSlug, repoRoot string, prID int, mergeStrategy, message string) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(projectKey, repoSlug, repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	pr, err := bbDecode[PullRequest](c, "GET", fmt.Sprintf("%s/pull-requests/%d", c.rp(pk, rs), prID), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if pr == nil {
		return tool.Result{}, fmt.Errorf("PR #%d not found.", prID)
	}
	body := map[string]any{"version": pr.Version}
	if mergeStrategy != "" {
		body["strategyId"] = mergeStrategy
	}
	if message != "" {
		body["message"] = message
	}
	data, err := bbDecode[PullRequest](c, "POST", fmt.Sprintf("%s/pull-requests/%d/merge", c.rp(pk, rs), prID), body)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil {
		return tool.TextResult(fmt.Sprintf("Merged PR #%d.\n%s", prID, c.pullRequestURL(pk, rs, prID, nil))), nil
	}
	return tool.TextResult(fmt.Sprintf("Merged PR #%d: %q (%s → %s).\n%s", data.ID, data.Title, data.FromRef.DisplayID, data.ToRef.DisplayID, c.pullRequestURL(pk, rs, data.ID, data))), nil
}

var SchemaKeyRe = regexp.MustCompile(`[^a-zA-Z0-9]`)

func ToAnySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
