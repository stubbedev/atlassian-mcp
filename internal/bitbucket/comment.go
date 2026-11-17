package bitbucket

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/stubbedev/atlassian-mcp/internal/arg"
	"github.com/stubbedev/atlassian-mcp/internal/markup"
	"github.com/stubbedev/atlassian-mcp/internal/tool"
)

// CommentDispatch is the bitbucket_comment tool entry.
func (c *Client) CommentDispatch(reviews *PRReviews, args map[string]any, repoRoot string) (tool.Result, error) {
	action := arg.String(args, "action")
	if action == "" {
		action = "add"
	}
	switch action {
	case "update":
		return c.UpdatePrComment(args, repoRoot)
	case "delete":
		if arg.Bool(args, "userAuthored") {
			return tool.Result{}, errors.New("userAuthored only applies to add and update, which write comment text; a delete writes none.")
		}
		return c.deletePrComment(args, repoRoot)
	case "add":
		return c.addPrComment(reviews, args, repoRoot)
	default:
		return tool.Result{}, fmt.Errorf("Unknown comment action %q. Use add, update, or delete. To reply, use action=add with commentId.", action)
	}
}

func (c *Client) addPrComment(reviews *PRReviews, args map[string]any, repoRoot string) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	prID := arg.Int(args, "prId")
	hasReply := arg.Has(args, "commentId")
	replyToCommentID := arg.Int(args, "commentId")

	anchorKeys := []string{"filePath", "srcPath", "line", "lineType", "fileType", "multilineStartLine", "multilineStartLineType", "fromHash", "toHash"}
	if hasReply {
		for _, k := range anchorKeys {
			if arg.Has(args, k) {
				return tool.Result{}, errors.New("Replies must target an existing comment thread only. Omit filePath/line and other anchor fields when replying.")
			}
		}
	}

	if hasReply {
		parent, _ := bbDecode[bbComment](c, "GET", fmt.Sprintf("%s/pull-requests/%d/comments/%d", c.rp(pk, rs), prID, replyToCommentID), nil)
		if parent != nil {
			me, err := c.getCurrentUsername()
			if err != nil {
				return tool.Result{}, err
			}
			for _, r := range parent.Comments {
				if !r.Deleted && r.Author != nil && r.Author.Name == me {
					return tool.Result{}, fmt.Errorf("You already replied to comment #%d (your reply is #%d). Never post a second reply on the same thread — update your existing reply instead: action=update commentId=%d.", replyToCommentID, r.ID, r.ID)
				}
			}
		}
	}

	prFetched := false
	var prCache *PullRequest
	getPR := func() *PullRequest {
		if !prFetched {
			prFetched = true
			prCache, _ = bbDecode[PullRequest](c, "GET", fmt.Sprintf("%s/pull-requests/%d", c.rp(pk, rs), prID), nil)
		}
		return prCache
	}

	// Reviewing your own PR is not a thing: a new top-level comment from the
	// author is either an unrequested self-review or a misfire. Replies are
	// always allowed — answering a reviewer is the author's normal job.
	if !hasReply && !arg.Bool(args, "asAuthor") {
		if pr := getPR(); pr != nil && pr.Author.User.Name != "" {
			if me, merr := c.getCurrentUsername(); merr == nil && me == pr.Author.User.Name {
				return tool.Result{}, fmt.Errorf("You are the author of PR #%d, so this would be an unsolicited comment on your own PR. Reply to an existing thread with commentId instead, or pass asAuthor=true if the user explicitly asked you to comment on your own PR.", prID)
			}
		}
	}

	if !arg.Has(args, "text") && !arg.Has(args, "suggestion") {
		return tool.Result{}, errors.New("Either text or suggestion is required when adding a comment.")
	}
	commentText, err := composeCommentText(args)
	if err != nil {
		return tool.Result{}, err
	}

	if !hasReply {
		if dup, derr := c.duplicateComment(pk, rs, prID, commentText); derr == nil && dup != 0 {
			return tool.Result{}, fmt.Errorf("You already posted this exact comment on PR #%d as #%d. Edit it (action=update commentId=%d) instead of posting it again.", prID, dup, dup)
		}
	}

	if arg.String(args, "severity") == "BLOCKER" {
		return tool.Result{}, errors.New("Adding a comment never creates a task. Omit severity (comments post as NORMAL). To create a task, use bitbucket_pr_tasks (action=create) — only when the user explicitly asks for one.")
	}
	validText, err := validateCommentText(commentText)
	if err != nil {
		return tool.Result{}, err
	}
	validText = c.linkifyCommentRefs(validText, pk, rs, prID)
	var uploaded []bbUploadedAttachment
	if paths := arg.StrSlice(args, "attachments"); len(paths) > 0 {
		// Uploaded last, after every rejection above, so a refused comment does
		// not leave orphan files on the repo.
		validText, uploaded, err = c.attachToText(pk, rs, validText, paths)
		if err != nil {
			return tool.Result{}, err
		}
	}
	validText = markup.AppendAIMarkerMarkdown(validText, markup.ParseAuthorship(args, "userAuthored"))
	body := map[string]any{"text": validText}
	if sev := arg.String(args, "severity"); sev != "" {
		body["severity"] = sev
	}
	isPending := arg.Bool(args, "pending")
	if isPending {
		body["state"] = "PENDING" // unpublished draft-review comment; user publishes it from the Bitbucket UI
	}
	if hasReply {
		body["parent"] = map[string]any{"id": replyToCommentID}
	}

	var inlineAnchor map[string]any
	usedFallbackHashes := false
	usedRememberedHashes := false
	remapNote := ""
	hasInline := arg.Has(args, "filePath") || arg.Has(args, "line")
	if hasInline {
		if !arg.Has(args, "filePath") || !arg.Has(args, "line") {
			return tool.Result{}, errors.New("filePath and line must be provided together for inline comments.")
		}
		filePath := arg.String(args, "filePath")
		line := arg.Int(args, "line")
		fileType := arg.String(args, "fileType")
		if fileType == "" {
			fileType = "TO"
		}
		lineType := arg.String(args, "lineType")
		if lineType == "" {
			lineType = "ADDED"
		}
		inlineAnchor = map[string]any{
			"diffType": "EFFECTIVE",
			"fileType": fileType,
			"line":     line,
			"lineType": lineType,
			"path":     filePath,
		}
		if arg.Has(args, "srcPath") {
			inlineAnchor["srcPath"] = arg.String(args, "srcPath")
		}

		pr := getPR()
		currentToHash := ""
		currentFromHash := ""
		if pr != nil {
			currentToHash = pr.FromRef.LatestCommit
			currentFromHash = pr.ToRef.LatestCommit
		}
		// Hashes come from the arguments, else from what this client was last
		// shown for this PR, else from current head (which is only right if the
		// branch has not moved since the review).
		argFromHash, argToHash := arg.String(args, "fromHash"), arg.String(args, "toHash")
		if argFromHash == "" && argToHash == "" {
			if sf, st, ok := reviews.last(reviewKey(pk, rs, prID)); ok {
				argFromHash, argToHash = sf, st
				usedRememberedHashes = true
			}
		}
		fromHash := argFromHash
		if fromHash == "" {
			fromHash = currentFromHash
		}
		toHash := argToHash
		if toHash == "" {
			toHash = currentToHash
		}
		usedFallbackHashes = argFromHash == "" && argToHash == ""

		reviewedToHash := argToHash
		if argToHash != "" && currentToHash != "" && reviewedToHash != currentToHash && fileType == "TO" {
			remappedLine, lineOk := c.remapLineThroughDiff(pk, rs, filePath, reviewedToHash, currentToHash, line)
			remappedMultilineStart := 0
			multilineProvided := arg.Has(args, "multilineStartLine")
			multilineOk := true
			if multilineProvided {
				remappedMultilineStart, multilineOk = c.remapLineThroughDiff(pk, rs, filePath, reviewedToHash, currentToHash, arg.Int(args, "multilineStartLine"))
			}
			if lineOk && multilineOk {
				if remappedLine != line {
					short := currentToHash
					if len(short) > 8 {
						short = short[:8]
					}
					remapNote = fmt.Sprintf("Reviewed line %d remapped to %d on current head %s.", line, remappedLine, short)
				}
				inlineAnchor["line"] = remappedLine
				if multilineProvided {
					inlineAnchor["multilineStartLine"] = remappedMultilineStart
				}
				toHash = currentToHash
				if argFromHash == "" {
					fromHash = currentFromHash
				}
			} else {
				short := reviewedToHash
				if len(short) > 8 {
					short = short[:8]
				}
				remapNote = fmt.Sprintf("Reviewed line %d was modified or removed in interim commits; anchoring to reviewed commit %s (Bitbucket will mark the comment outdated, which is correct — the line you reviewed no longer exists at current head).", line, short)
			}
		}

		if fromHash != "" && toHash != "" {
			inlineAnchor["fromHash"] = fromHash
			inlineAnchor["toHash"] = toHash
		}

		if arg.Has(args, "multilineStartLine") {
			if _, ok := inlineAnchor["multilineStartLine"]; !ok {
				inlineAnchor["multilineStartLine"] = arg.Int(args, "multilineStartLine")
			}
		}
		if _, ok := inlineAnchor["multilineStartLine"]; ok {
			mlt := arg.String(args, "multilineStartLineType")
			if mlt == "" {
				mlt = lineType
			}
			inlineAnchor["multilineStartLineType"] = mlt
		}
		body["anchor"] = inlineAnchor
	}

	path := fmt.Sprintf("%s/pull-requests/%d/comments", c.rp(pk, rs), prID)
	created, err := bbDecode[bbComment](c, "POST", path, body)
	if err != nil {
		hasFrom := hasKey(inlineAnchor, "fromHash")
		hasTo := hasKey(inlineAnchor, "toHash")
		if inlineAnchor == nil || !strings.Contains(err.Error(), "Bitbucket 409") || !hasFrom || !hasTo {
			return tool.Result{}, err
		}
		delete(inlineAnchor, "fromHash")
		delete(inlineAnchor, "toHash")
		body["anchor"] = inlineAnchor
		created, err = bbDecode[bbComment](c, "POST", path, body)
		if err != nil {
			return tool.Result{}, err
		}
	}

	pendingNote := ""
	if isPending {
		pendingNote = " (pending — not yet published; publish the review from the Bitbucket UI to send it to the author)"
	}
	if created == nil {
		return tool.TextResult(fmt.Sprintf("Comment added to PR #%d%s.%s", prID, pendingNote, attachmentSummary(uploaded))), nil
	}
	if hasReply {
		return tool.TextResult(fmt.Sprintf("Reply #%d added to comment #%d on PR #%d%s.%s", created.ID, replyToCommentID, prID, pendingNote, attachmentSummary(uploaded))), nil
	}
	location := ""
	if arg.Has(args, "filePath") && arg.Has(args, "line") {
		location = fmt.Sprintf(" on %s:%d", arg.String(args, "filePath"), arg.Int(args, "line"))
	}
	var warnings []string
	if inlineAnchor != nil && usedFallbackHashes {
		warnings = append(warnings, "No fromHash/toHash passed and no reviewed diff on record for this PR — anchored to latest PR head. Read the PR with bitbucket_get_pr first and the reviewed commits are remembered for you.")
	}
	if usedRememberedHashes {
		warnings = append(warnings, "Anchored to the commits you were shown for this PR.")
	}
	if remapNote != "" {
		warnings = append(warnings, remapNote)
	}
	warnSuffix := ""
	if len(warnings) > 0 {
		warnSuffix = "\n\nNote: " + strings.Join(warnings, " ")
	}
	return tool.TextResult(fmt.Sprintf("Comment #%d added to PR #%d%s%s.%s%s", created.ID, prID, location, pendingNote, attachmentSummary(uploaded), warnSuffix)), nil
}

// ── Comment reference links ──────────────────────────────────────────────────

// A bare "#123" is not a link in Bitbucket's renderer, so a comment that refers
// to another comment that way is a dead end for the reader. These rewrite the
// reference into a markdown link, but only for ids that really are comments on
// this PR — "#2 in the list" or "issue #42" resolve to nothing and stay as they
// are.

var (
	bareCommentRefRe = regexp.MustCompile(`(^|[\s(])#(\d+)\b`)
	codeSpanRe       = regexp.MustCompile("(?s)```.*?```|`[^`\n]+`")
)

func (c *Client) commentURL(pk, rs string, prID, commentID int) string {
	return fmt.Sprintf("%s/projects/%s/repos/%s/pull-requests/%d/overview?commentId=%d",
		c.baseURL, url.PathEscape(pk), url.PathEscape(rs), prID, commentID)
}

// refersToSomethingElse reports whether the words before a "#123" name a
// different kind of thing, so the number is not a comment id.
func refersToSomethingElse(before string) bool {
	trimmed := strings.ToLower(strings.TrimRight(before, " \t("))
	for _, word := range []string{"pr", "pull request", "pull-request", "issue", "task", "build", "ticket"} {
		if strings.HasSuffix(trimmed, word) {
			return true
		}
	}
	return false
}

func (c *Client) linkifyCommentRefs(text, pk, rs string, prID int) string {
	if !strings.Contains(text, "#") {
		return text
	}
	// Mask code so ids inside code blocks are left alone, then restore.
	var spans []string
	masked := codeSpanRe.ReplaceAllStringFunc(text, func(m string) string {
		spans = append(spans, m)
		return fmt.Sprintf("\x00cs%d\x00", len(spans)-1)
	})
	exists := map[int]bool{}
	var out strings.Builder
	last := 0
	for _, loc := range bareCommentRefRe.FindAllStringSubmatchIndex(masked, -1) {
		whole, digits := masked[loc[0]:loc[1]], masked[loc[4]:loc[5]]
		out.WriteString(masked[last:loc[0]])
		last = loc[1]
		id, err := strconv.Atoi(digits)
		// "PR #42" and "issue #42" are not comment references, whatever ids exist.
		if err != nil || id <= 0 || refersToSomethingElse(masked[:loc[0]]) {
			out.WriteString(whole)
			continue
		}
		ok, seen := exists[id]
		if !seen {
			cmt, derr := bbDecode[bbComment](c, "GET", fmt.Sprintf("%s/pull-requests/%d/comments/%d", c.rp(pk, rs), prID, id), nil)
			ok = derr == nil && cmt != nil && cmt.ID == id
			exists[id] = ok
		}
		if !ok {
			out.WriteString(whole)
			continue
		}
		out.WriteString(masked[loc[0]:loc[4]-1] + fmt.Sprintf("[#%d](%s)", id, c.commentURL(pk, rs, prID, id)))
	}
	out.WriteString(masked[last:])
	masked = out.String()
	for i, span := range spans {
		masked = strings.ReplaceAll(masked, fmt.Sprintf("\x00cs%d\x00", i), span)
	}
	return masked
}

// duplicateComment returns the id of an existing non-deleted comment of mine on
// this PR whose text is identical, or 0. Re-running a review flow otherwise
// posts the same summary twice, which no instruction reliably prevents.
func (c *Client) duplicateComment(pk, rs string, prID int, text string) (int, error) {
	want := strings.TrimSpace(text)
	if want == "" {
		return 0, nil
	}
	me, err := c.getCurrentUsername()
	if err != nil {
		return 0, err
	}
	data, err := bbDecode[bbPaged[bbActivity]](c, "GET", fmt.Sprintf("%s/pull-requests/%d/activities?limit=100", c.rp(pk, rs), prID), nil)
	if err != nil || data == nil {
		return 0, err
	}
	activities := make([]bbActivity, 0, len(data.Values))
	activities = append(activities, data.Values...)
	for _, cmt := range uniqueCommentsFromActivities(activities) {
		if cmt.Deleted || cmt.Author == nil || cmt.Author.Name != me {
			continue
		}
		// The attribution marker is stripped on both sides: a comment posted
		// earlier carries it, the incoming text does not yet.
		if markup.StripAIMarker(cmt.Text) == want {
			return cmt.ID, nil
		}
	}
	return 0, nil
}

func composeCommentText(args map[string]any) (string, error) {
	text := arg.String(args, "text")
	if !arg.Has(args, "suggestion") {
		if err := validateSuggestionPlacement(text); err != nil {
			return "", err
		}
		return text, nil
	}
	suggestion := trimBlankLines(arg.String(args, "suggestion"))
	if suggestion == "" {
		return "", errors.New("suggestion must not be empty.")
	}
	block := "```suggestion\n" + suggestion + "\n```"
	prefix := strings.TrimSpace(text)
	if prefix == "" {
		return block, nil
	}
	return prefix + "\n\n" + block, nil
}

func trimBlankLines(s string) string {
	s = strings.TrimRightFunc(s, unicode.IsSpace)
	for {
		line, rest, found := strings.Cut(s, "\n")
		if !found || strings.TrimSpace(line) != "" {
			return s
		}
		s = rest
	}
}

func hasKey(m map[string]any, k string) bool {
	if m == nil {
		return false
	}
	_, ok := m[k]
	return ok
}

func (c *Client) UpdatePrComment(args map[string]any, repoRoot string) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	prID := arg.Int(args, "prId")
	commentID := arg.Int(args, "commentId")
	hasText := arg.Has(args, "text") || arg.Has(args, "suggestion")
	stateArg := arg.String(args, "state")
	severityArg := arg.String(args, "severity")
	hasThreadResolved := arg.Has(args, "threadResolved")
	attachments := arg.StrSlice(args, "attachments")
	if !hasText && stateArg == "" && severityArg == "" && !hasThreadResolved && len(attachments) == 0 {
		return tool.Result{}, errors.New("At least one field is required: text, suggestion, state, severity, threadResolved, or attachments")
	}
	by := markup.ParseAuthorship(args, "userAuthored")
	if by == markup.AuthoredByUser && !hasText && len(attachments) == 0 {
		return tool.Result{}, errors.New("userAuthored only applies when the update writes text, a suggestion or attachments.")
	}
	composedText := ""
	if hasText {
		if composedText, err = composeCommentText(args); err != nil {
			return tool.Result{}, err
		}
	}

	current, err := bbDecode[bbComment](c, "GET", fmt.Sprintf("%s/pull-requests/%d/comments/%d", c.rp(pk, rs), prID, commentID), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if current == nil {
		return tool.Result{}, fmt.Errorf("Comment #%d not found.", commentID)
	}
	currentSeverity := current.Severity
	if currentSeverity == "" {
		currentSeverity = "NORMAL"
	}
	targetSeverity := severityArg
	if targetSeverity == "" {
		targetSeverity = currentSeverity
	}
	if stateArg != "" && targetSeverity != "BLOCKER" {
		return tool.Result{}, errors.New("state is only supported for BLOCKER comments (tasks). Use threadResolved for normal comment threads.")
	}
	if hasThreadResolved && targetSeverity == "BLOCKER" {
		return tool.Result{}, errors.New("threadResolved is only supported for normal comments. Use state for BLOCKER comment tasks.")
	}

	commentPath := fmt.Sprintf("%s/pull-requests/%d/comments/%d", c.rp(pk, rs), prID, commentID)
	if targetSeverity == "BLOCKER" || current.Severity == "BLOCKER" {
		commentPath = fmt.Sprintf("%s/pull-requests/%d/blocker-comments/%d", c.rp(pk, rs), prID, commentID)
	}

	// Uploaded once, before the version-conflict retry loop, so a 409 does not
	// upload the same files twice. Attachments with no new text go onto the
	// text the comment already has.
	var uploaded []bbUploadedAttachment
	newText := ""
	if hasText || len(attachments) > 0 {
		if hasText {
			vt, verr := validateCommentText(composedText)
			if verr != nil {
				return tool.Result{}, verr
			}
			newText = c.linkifyCommentRefs(vt, pk, rs, prID)
		} else {
			newText = markup.StripAIMarker(current.Text)
		}
		if len(attachments) > 0 {
			newText, uploaded, err = c.attachToText(pk, rs, newText, attachments)
			if err != nil {
				return tool.Result{}, err
			}
		}
		newText = markup.AppendAIMarkerMarkdown(newText, by)
	}

	buildBody := func(version int) map[string]any {
		body := map[string]any{"version": version}
		if newText != "" {
			body["text"] = newText
		}
		if stateArg != "" && targetSeverity == "BLOCKER" {
			body["state"] = stateArg
		}
		if severityArg != "" {
			body["severity"] = severityArg
		}
		if hasThreadResolved {
			body["threadResolved"] = arg.Bool(args, "threadResolved")
		}
		return body
	}

	firstBody := buildBody(current.Version)
	updated, err := bbDecode[bbComment](c, "PUT", commentPath, firstBody)
	if err != nil {
		if !strings.Contains(err.Error(), "Bitbucket 409") {
			return tool.Result{}, err
		}
		latest, lerr := bbDecode[bbComment](c, "GET", commentPath, nil)
		if lerr != nil || latest == nil {
			return tool.Result{}, err
		}
		updated, err = bbDecode[bbComment](c, "PUT", commentPath, buildBody(latest.Version))
		if err != nil {
			return tool.Result{}, err
		}
	}
	if updated == nil {
		return tool.TextResult(fmt.Sprintf("Comment #%d updated.%s", commentID, attachmentSummary(uploaded))), nil
	}
	state := firstNonEmpty(updated.State, current.State, "OPEN")
	severity := firstNonEmpty(updated.Severity, current.Severity, "NORMAL")
	threadResolved := updated.ThreadResolved
	if threadResolved == nil {
		threadResolved = current.ThreadResolved
	}
	threadStatus := ""
	if threadResolved != nil {
		st := "OPEN"
		if *threadResolved {
			st = "RESOLVED"
		}
		threadStatus = ", thread=" + st
	}
	return tool.TextResult(fmt.Sprintf("Comment #%d updated (%s/%s%s).%s", updated.ID, state, severity, threadStatus, attachmentSummary(uploaded))), nil
}

func (c *Client) deletePrComment(args map[string]any, repoRoot string) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	prID := arg.Int(args, "prId")
	commentID := arg.Int(args, "commentId")
	current, err := bbDecode[bbComment](c, "GET", fmt.Sprintf("%s/pull-requests/%d/comments/%d", c.rp(pk, rs), prID, commentID), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if current == nil {
		return tool.Result{}, fmt.Errorf("Comment #%d not found.", commentID)
	}
	commentPath := fmt.Sprintf("%s/pull-requests/%d/comments/%d", c.rp(pk, rs), prID, commentID)
	if current.Severity == "BLOCKER" {
		commentPath = fmt.Sprintf("%s/pull-requests/%d/blocker-comments/%d", c.rp(pk, rs), prID, commentID)
	}
	if _, err := c.request("DELETE", fmt.Sprintf("%s?version=%d", commentPath, current.Version), nil); err != nil {
		return tool.Result{}, err
	}
	return tool.TextResult(fmt.Sprintf("Comment #%d deleted from PR #%d.", commentID, prID)), nil
}

// ── Tasks ────────────────────────────────────────────────────────────────────

func (c *Client) PrTasksDispatch(args map[string]any, repoRoot string) (tool.Result, error) {
	action := arg.String(args, "action")
	if action == "" {
		action = "list"
	}
	switch action {
	case "list":
		return c.getPrTasks(args, repoRoot)
	case "create", "resolve", "reopen", "delete":
		return c.mutatePrTask(action, args, repoRoot)
	default:
		return tool.Result{}, fmt.Errorf("Unknown task action %q. Use list, create, resolve, reopen, or delete.", action)
	}
}

func (c *Client) getPrTasks(args map[string]any, repoRoot string) (tool.Result, error) {
	pk, rs, err := c.ResolveProjectAndRepo(arg.String(args, "projectKey"), arg.String(args, "repoSlug"), repoRoot)
	if err != nil {
		return tool.Result{}, err
	}
	prID := arg.Int(args, "prId")
	data, err := bbDecode[bbPaged[bbTask]](c, "GET", fmt.Sprintf("%s/pull-requests/%d/tasks", c.rp(pk, rs), prID), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return tool.TextResult(fmt.Sprintf("No tasks on PR #%d.", prID)), nil
	}
	var lines []string
	open := 0
	for _, t := range data.Values {
		author := "Unknown"
		if t.Author != nil {
			if t.Author.DisplayName != "" {
				author = t.Author.DisplayName
			} else if t.Author.Name != "" {
				author = t.Author.Name
			}
		}
		date := ""
		if t.CreatedDate != 0 {
			date = " (" + formatBBDate(t.CreatedDate) + ")"
		}
		anchor := ""
		if t.Anchor != nil && t.Anchor.ID != 0 {
			anchor = fmt.Sprintf(" [on comment #%d]", t.Anchor.ID)
		}
		lines = append(lines, fmt.Sprintf("#%d [%s] %s%s%s: %s", t.ID, t.State, author, date, anchor, t.Text))
		if t.State == "OPEN" {
			open++
		}
	}
	return tool.TextResult(fmt.Sprintf("%d task(s) on PR #%d (%d open)%s:\n%s", len(data.Values), prID, open, pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n"))), nil
}

func (c *Client) mutatePrTask(action string, args map[string]any, repoRoot string) (tool.Result, error) {
	// Tasks use the global /tasks endpoint (anchored by prId/commentId/taskId),
	// so project/repo are not required here.
	_ = repoRoot
	prID := arg.IntPtr(args, "prId")
	commentID := arg.IntPtr(args, "commentId")
	taskID := arg.IntPtr(args, "taskId")

	if action == "create" {
		text := arg.String(args, "text")
		if text == "" {
			return tool.Result{}, errors.New("text is required to create a task.")
		}
		body := map[string]any{"text": text}
		switch {
		case commentID != nil:
			body["anchor"] = map[string]any{"id": *commentID, "type": "COMMENT"}
		case prID != nil:
			body["anchor"] = map[string]any{"id": *prID, "type": "PULL_REQUEST"}
		default:
			return tool.Result{}, errors.New("Provide prId or commentId to anchor the task.")
		}
		created, err := bbDecode[bbTask](c, "POST", "/tasks", body)
		if err != nil {
			return tool.Result{}, err
		}
		if created == nil {
			return tool.TextResult("Task created."), nil
		}
		return tool.TextResult(fmt.Sprintf("Task #%d created: %q", created.ID, created.Text)), nil
	}

	if taskID == nil {
		return tool.Result{}, errors.New("taskId is required for resolve/reopen/delete.")
	}

	if action == "delete" {
		task, err := bbDecode[bbTask](c, "GET", fmt.Sprintf("/tasks/%d", *taskID), nil)
		if err != nil {
			return tool.Result{}, err
		}
		if task == nil {
			return tool.Result{}, fmt.Errorf("Task #%d not found.", *taskID)
		}
		if prID != nil && task.Anchor != nil && task.Anchor.Type == "PULL_REQUEST" && task.Anchor.ID != *prID {
			return tool.Result{}, fmt.Errorf("Task #%d does not belong to PR #%d.", *taskID, *prID)
		}
		if _, err := c.request("DELETE", fmt.Sprintf("/tasks/%d?version=%d", *taskID, task.Version), nil); err != nil {
			return tool.Result{}, err
		}
		return tool.TextResult(fmt.Sprintf("Task #%d deleted.", *taskID)), nil
	}

	task, err := bbDecode[bbTask](c, "GET", fmt.Sprintf("/tasks/%d", *taskID), nil)
	if err != nil {
		return tool.Result{}, err
	}
	if task == nil {
		return tool.Result{}, fmt.Errorf("Task #%d not found.", *taskID)
	}
	if prID != nil && task.Anchor != nil && task.Anchor.Type == "PULL_REQUEST" && task.Anchor.ID != *prID {
		return tool.Result{}, fmt.Errorf("Task #%d does not belong to PR #%d.", *taskID, *prID)
	}
	newState := "OPEN"
	switch action {
	case "resolve":
		newState = "RESOLVED"
	case "reopen":
	default:
		return tool.Result{}, fmt.Errorf("Unknown task action %q. Use list, create, resolve, reopen, or delete.", action)
	}
	updated, err := bbDecode[bbTask](c, "PUT", fmt.Sprintf("/tasks/%d?version=%d", *taskID, task.Version), map[string]any{"id": task.ID, "state": newState, "text": task.Text})
	if err != nil {
		return tool.Result{}, err
	}
	if updated == nil {
		return tool.TextResult(fmt.Sprintf("Task #%d %s.", *taskID, newState)), nil
	}
	return tool.TextResult(fmt.Sprintf("Task #%d is now %s: %q", updated.ID, updated.State, updated.Text)), nil
}

// remapLineThroughDiff remaps a source-side line through an interim diff.
// Returns (destLine, true) if the line survives unchanged in a CONTEXT segment
// (or there is no interim diff), or (0, false) if it was modified/removed.
func (c *Client) remapLineThroughDiff(projectKey, repoSlug, filePath, sinceHash, untilHash string, sourceLine int) (int, bool) {
	path := fmt.Sprintf("%s/diff/%s?since=%s&until=%s&contextLines=0", c.rp(projectKey, repoSlug), encodePath(filePath), url.QueryEscape(sinceHash), url.QueryEscape(untilHash))
	diff, err := bbDecode[bbDiff](c, "GET", path, nil)
	if err != nil || diff == nil || len(diff.Diffs) == 0 {
		return sourceLine, true
	}
	offset := 0
	for _, fileDiff := range diff.Diffs {
		for _, hunk := range fileDiff.Hunks {
			srcStart := hunk.SourceLine
			srcSpan := hunk.SourceSpan
			srcEnd := srcStart + srcSpan - 1
			if srcSpan > 0 && sourceLine >= srcStart && sourceLine <= srcEnd {
				for _, segment := range hunk.Segments {
					if segment.Type == "ADDED" {
						continue
					}
					for _, ln := range segment.Lines {
						if ln.Source == sourceLine {
							if segment.Type == "CONTEXT" && ln.Destination != 0 {
								return ln.Destination, true
							}
							return 0, false
						}
					}
				}
				return 0, false
			}
			if sourceLine > srcEnd || srcSpan == 0 {
				dstSpan := hunk.DestinationSpan
				if srcSpan == 0 && hunk.DestinationLine <= sourceLine+offset {
					offset += dstSpan
				} else if sourceLine > srcEnd {
					offset += dstSpan - srcSpan
				}
			}
		}
	}
	return sourceLine + offset, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
