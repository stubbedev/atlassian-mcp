package main

import (
	"fmt"
	"net/url"
	"strings"
)

// commentDispatch is the bitbucket_comment tool entry.
func (c *BitbucketClient) commentDispatch(args map[string]any, repoRoot string) (toolResult, error) {
	action := argString(args, "action")
	if action == "" {
		action = "add"
	}
	switch action {
	case "update":
		return c.updatePrComment(args, repoRoot)
	case "delete":
		return c.deletePrComment(args, repoRoot)
	default:
		return c.addPrComment(args, repoRoot)
	}
}

func (c *BitbucketClient) addPrComment(args map[string]any, repoRoot string) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(argString(args, "projectKey"), argString(args, "repoSlug"), repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	prID := argInt(args, "prId")
	hasReply := has(args, "commentId")
	replyToCommentID := argInt(args, "commentId")

	anchorKeys := []string{"filePath", "srcPath", "line", "lineType", "fileType", "multilineStartLine", "multilineStartLineType", "fromHash", "toHash"}
	if hasReply {
		for _, k := range anchorKeys {
			if has(args, k) {
				return toolResult{}, fmt.Errorf("Replies must target an existing comment thread only. Omit filePath/line and other anchor fields when replying.")
			}
		}
	}

	if hasReply {
		parent, _ := bbDecode[bbComment](c, "GET", fmt.Sprintf("%s/pull-requests/%d/comments/%d", c.rp(pk, rs), prID, replyToCommentID), nil)
		if parent != nil {
			me, err := c.getCurrentUsername()
			if err != nil {
				return toolResult{}, err
			}
			for _, r := range parent.Comments {
				if !r.Deleted && r.Author != nil && r.Author.Name == me {
					return toolResult{}, fmt.Errorf("You already replied to comment #%d (your reply is #%d). Never post a second reply on the same thread — update your existing reply instead: action=update commentId=%d.", replyToCommentID, r.ID, r.ID)
				}
			}
		}
	}

	hasText := has(args, "text")
	hasSuggestion := has(args, "suggestion")
	if !hasText && !hasSuggestion {
		return toolResult{}, fmt.Errorf("Either text or suggestion is required when adding a comment.")
	}

	commentText := argString(args, "text")
	if hasSuggestion {
		suggestion := strings.TrimSpace(argString(args, "suggestion"))
		if suggestion == "" {
			return toolResult{}, fmt.Errorf("suggestion must not be empty.")
		}
		suggestionBlock := "```suggestion\n" + suggestion + "\n```"
		prefix := strings.TrimSpace(argString(args, "text"))
		if prefix != "" {
			commentText = prefix + "\n\n" + suggestionBlock
		} else {
			commentText = suggestionBlock
		}
	} else {
		if err := validateSuggestionPlacement(commentText); err != nil {
			return toolResult{}, err
		}
	}

	if argString(args, "severity") == "BLOCKER" {
		return toolResult{}, fmt.Errorf("Adding a comment never creates a task. Omit severity (comments post as NORMAL). To create a task, use bitbucket_pr_tasks (action=create) — only when the user explicitly asks for one.")
	}
	validText, err := validateCommentText(commentText)
	if err != nil {
		return toolResult{}, err
	}
	body := map[string]any{"text": validText}
	if sev := argString(args, "severity"); sev != "" {
		body["severity"] = sev
	}
	isPending := argBool(args, "pending")
	if isPending {
		body["state"] = "PENDING" // unpublished draft-review comment; user publishes it from the Bitbucket UI
	}
	if hasReply {
		body["parent"] = map[string]any{"id": replyToCommentID}
	}

	var inlineAnchor map[string]any
	usedFallbackHashes := false
	remapNote := ""
	hasInline := has(args, "filePath") || has(args, "line")
	if hasInline {
		if !has(args, "filePath") || !has(args, "line") {
			return toolResult{}, fmt.Errorf("filePath and line must be provided together for inline comments.")
		}
		filePath := argString(args, "filePath")
		line := argInt(args, "line")
		fileType := argString(args, "fileType")
		if fileType == "" {
			fileType = "TO"
		}
		lineType := argString(args, "lineType")
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
		if has(args, "srcPath") {
			inlineAnchor["srcPath"] = argString(args, "srcPath")
		}

		pr, _ := bbDecode[bbPullRequest](c, "GET", fmt.Sprintf("%s/pull-requests/%d", c.rp(pk, rs), prID), nil)
		currentToHash := ""
		currentFromHash := ""
		if pr != nil {
			currentToHash = pr.FromRef.LatestCommit
			currentFromHash = pr.ToRef.LatestCommit
		}
		fromHash := argString(args, "fromHash")
		if !has(args, "fromHash") {
			fromHash = currentFromHash
		}
		toHash := argString(args, "toHash")
		if !has(args, "toHash") {
			toHash = currentToHash
		}
		usedFallbackHashes = !has(args, "fromHash") && !has(args, "toHash")

		reviewedToHash := argString(args, "toHash")
		if has(args, "toHash") && currentToHash != "" && reviewedToHash != currentToHash && fileType == "TO" {
			remappedLine, lineOk := c.remapLineThroughDiff(pk, rs, filePath, reviewedToHash, currentToHash, line)
			remappedMultilineStart := 0
			multilineProvided := has(args, "multilineStartLine")
			multilineOk := true
			if multilineProvided {
				remappedMultilineStart, multilineOk = c.remapLineThroughDiff(pk, rs, filePath, reviewedToHash, currentToHash, argInt(args, "multilineStartLine"))
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
				if !has(args, "fromHash") {
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

		if has(args, "multilineStartLine") {
			if _, ok := inlineAnchor["multilineStartLine"]; !ok {
				inlineAnchor["multilineStartLine"] = argInt(args, "multilineStartLine")
			}
		}
		if _, ok := inlineAnchor["multilineStartLine"]; ok {
			mlt := argString(args, "multilineStartLineType")
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
		_, hasFrom := mapHas(inlineAnchor, "fromHash")
		_, hasTo := mapHas(inlineAnchor, "toHash")
		if inlineAnchor == nil || !strings.Contains(err.Error(), "Bitbucket 409") || !hasFrom || !hasTo {
			return toolResult{}, err
		}
		delete(inlineAnchor, "fromHash")
		delete(inlineAnchor, "toHash")
		body["anchor"] = inlineAnchor
		created, err = bbDecode[bbComment](c, "POST", path, body)
		if err != nil {
			return toolResult{}, err
		}
	}

	pendingNote := ""
	if isPending {
		pendingNote = " (pending — not yet published; publish the review from the Bitbucket UI to send it to the author)"
	}
	if created == nil {
		return textResult(fmt.Sprintf("Comment added to PR #%d%s.", prID, pendingNote)), nil
	}
	if hasReply {
		return textResult(fmt.Sprintf("Reply #%d added to comment #%d on PR #%d%s.", created.ID, replyToCommentID, prID, pendingNote)), nil
	}
	location := ""
	if has(args, "filePath") && has(args, "line") {
		location = fmt.Sprintf(" on %s:%d", argString(args, "filePath"), argInt(args, "line"))
	}
	var warnings []string
	if inlineAnchor != nil && usedFallbackHashes {
		warnings = append(warnings, "No fromHash/toHash passed — anchored to latest PR head. If you reviewed an older commit, the line may now point at unrelated code. Pass fromHash/toHash from bitbucket_pr_diff or bitbucket_get_pr to bind comments to the exact commit you reviewed.")
	}
	if remapNote != "" {
		warnings = append(warnings, remapNote)
	}
	warnSuffix := ""
	if len(warnings) > 0 {
		warnSuffix = "\n\nNote: " + strings.Join(warnings, " ")
	}
	return textResult(fmt.Sprintf("Comment #%d added to PR #%d%s%s.%s", created.ID, prID, location, pendingNote, warnSuffix)), nil
}

func mapHas(m map[string]any, k string) (any, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := m[k]
	return v, ok
}

func (c *BitbucketClient) updatePrComment(args map[string]any, repoRoot string) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(argString(args, "projectKey"), argString(args, "repoSlug"), repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	prID := argInt(args, "prId")
	commentID := argInt(args, "commentId")
	hasText := has(args, "text")
	stateArg := argString(args, "state")
	severityArg := argString(args, "severity")
	hasThreadResolved := has(args, "threadResolved")
	if !hasText && stateArg == "" && severityArg == "" && !hasThreadResolved {
		return toolResult{}, fmt.Errorf("At least one field is required: text, state, severity, or threadResolved")
	}

	current, err := bbDecode[bbComment](c, "GET", fmt.Sprintf("%s/pull-requests/%d/comments/%d", c.rp(pk, rs), prID, commentID), nil)
	if err != nil {
		return toolResult{}, err
	}
	if current == nil {
		return toolResult{}, fmt.Errorf("Comment #%d not found.", commentID)
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
		return toolResult{}, fmt.Errorf("state is only supported for BLOCKER comments (tasks). Use threadResolved for normal comment threads.")
	}
	if hasThreadResolved && targetSeverity == "BLOCKER" {
		return toolResult{}, fmt.Errorf("threadResolved is only supported for normal comments. Use state for BLOCKER comment tasks.")
	}

	commentPath := fmt.Sprintf("%s/pull-requests/%d/comments/%d", c.rp(pk, rs), prID, commentID)
	if targetSeverity == "BLOCKER" || current.Severity == "BLOCKER" {
		commentPath = fmt.Sprintf("%s/pull-requests/%d/blocker-comments/%d", c.rp(pk, rs), prID, commentID)
	}

	buildBody := func(version int) (map[string]any, error) {
		body := map[string]any{"version": version}
		if hasText {
			vt, err := validateCommentText(argString(args, "text"))
			if err != nil {
				return nil, err
			}
			body["text"] = vt
		}
		if stateArg != "" && targetSeverity == "BLOCKER" {
			body["state"] = stateArg
		}
		if severityArg != "" {
			body["severity"] = severityArg
		}
		if hasThreadResolved {
			body["threadResolved"] = argBool(args, "threadResolved")
		}
		return body, nil
	}

	firstBody, err := buildBody(current.Version)
	if err != nil {
		return toolResult{}, err
	}
	updated, err := bbDecode[bbComment](c, "PUT", commentPath, firstBody)
	if err != nil {
		if !strings.Contains(err.Error(), "Bitbucket 409") {
			return toolResult{}, err
		}
		latest, lerr := bbDecode[bbComment](c, "GET", commentPath, nil)
		if lerr != nil || latest == nil {
			return toolResult{}, err
		}
		retryBody, berr := buildBody(latest.Version)
		if berr != nil {
			return toolResult{}, berr
		}
		updated, err = bbDecode[bbComment](c, "PUT", commentPath, retryBody)
		if err != nil {
			return toolResult{}, err
		}
	}
	if updated == nil {
		return textResult(fmt.Sprintf("Comment #%d updated.", commentID)), nil
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
	return textResult(fmt.Sprintf("Comment #%d updated (%s/%s%s).", updated.ID, state, severity, threadStatus)), nil
}

func (c *BitbucketClient) deletePrComment(args map[string]any, repoRoot string) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(argString(args, "projectKey"), argString(args, "repoSlug"), repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	prID := argInt(args, "prId")
	commentID := argInt(args, "commentId")
	current, err := bbDecode[bbComment](c, "GET", fmt.Sprintf("%s/pull-requests/%d/comments/%d", c.rp(pk, rs), prID, commentID), nil)
	if err != nil {
		return toolResult{}, err
	}
	if current == nil {
		return toolResult{}, fmt.Errorf("Comment #%d not found.", commentID)
	}
	commentPath := fmt.Sprintf("%s/pull-requests/%d/comments/%d", c.rp(pk, rs), prID, commentID)
	if current.Severity == "BLOCKER" {
		commentPath = fmt.Sprintf("%s/pull-requests/%d/blocker-comments/%d", c.rp(pk, rs), prID, commentID)
	}
	if _, err := c.request("DELETE", fmt.Sprintf("%s?version=%d", commentPath, current.Version), nil); err != nil {
		return toolResult{}, err
	}
	return textResult(fmt.Sprintf("Comment #%d deleted from PR #%d.", commentID, prID)), nil
}

// ── Tasks ────────────────────────────────────────────────────────────────────

func (c *BitbucketClient) prTasksDispatch(args map[string]any, repoRoot string) (toolResult, error) {
	action := argString(args, "action")
	if action == "" {
		action = "list"
	}
	if action == "list" {
		return c.getPrTasks(args, repoRoot)
	}
	return c.mutatePrTask(action, args, repoRoot)
}

func (c *BitbucketClient) getPrTasks(args map[string]any, repoRoot string) (toolResult, error) {
	pk, rs, err := c.resolveProjectAndRepo(argString(args, "projectKey"), argString(args, "repoSlug"), repoRoot)
	if err != nil {
		return toolResult{}, err
	}
	prID := argInt(args, "prId")
	data, err := bbDecode[bbPaged[bbTask]](c, "GET", fmt.Sprintf("%s/pull-requests/%d/tasks", c.rp(pk, rs), prID), nil)
	if err != nil {
		return toolResult{}, err
	}
	if data == nil || len(data.Values) == 0 {
		return textResult(fmt.Sprintf("No tasks on PR #%d.", prID)), nil
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
	return textResult(fmt.Sprintf("%d task(s) on PR #%d (%d open)%s:\n%s", len(data.Values), prID, open, pageHintPaged(data.IsLastPage, data.NextPageStart), strings.Join(lines, "\n"))), nil
}

func (c *BitbucketClient) mutatePrTask(action string, args map[string]any, repoRoot string) (toolResult, error) {
	// Tasks use the global /tasks endpoint (anchored by prId/commentId/taskId),
	// so project/repo are not required here.
	_ = repoRoot
	prID := argIntPtr(args, "prId")
	commentID := argIntPtr(args, "commentId")
	taskID := argIntPtr(args, "taskId")

	if action == "create" {
		text := argString(args, "text")
		if text == "" {
			return toolResult{}, fmt.Errorf("text is required to create a task.")
		}
		body := map[string]any{"text": text}
		switch {
		case commentID != nil:
			body["anchor"] = map[string]any{"id": *commentID, "type": "COMMENT"}
		case prID != nil:
			body["anchor"] = map[string]any{"id": *prID, "type": "PULL_REQUEST"}
		default:
			return toolResult{}, fmt.Errorf("Provide prId or commentId to anchor the task.")
		}
		created, err := bbDecode[bbTask](c, "POST", "/tasks", body)
		if err != nil {
			return toolResult{}, err
		}
		if created == nil {
			return textResult("Task created."), nil
		}
		return textResult(fmt.Sprintf("Task #%d created: %q", created.ID, created.Text)), nil
	}

	if taskID == nil {
		return toolResult{}, fmt.Errorf("taskId is required for resolve/reopen/delete.")
	}

	if action == "delete" {
		task, err := bbDecode[bbTask](c, "GET", fmt.Sprintf("/tasks/%d", *taskID), nil)
		if err != nil {
			return toolResult{}, err
		}
		if task == nil {
			return toolResult{}, fmt.Errorf("Task #%d not found.", *taskID)
		}
		if prID != nil && task.Anchor != nil && task.Anchor.Type == "PULL_REQUEST" && task.Anchor.ID != *prID {
			return toolResult{}, fmt.Errorf("Task #%d does not belong to PR #%d.", *taskID, *prID)
		}
		if _, err := c.request("DELETE", fmt.Sprintf("/tasks/%d?version=%d", *taskID, task.Version), nil); err != nil {
			return toolResult{}, err
		}
		return textResult(fmt.Sprintf("Task #%d deleted.", *taskID)), nil
	}

	task, err := bbDecode[bbTask](c, "GET", fmt.Sprintf("/tasks/%d", *taskID), nil)
	if err != nil {
		return toolResult{}, err
	}
	if task == nil {
		return toolResult{}, fmt.Errorf("Task #%d not found.", *taskID)
	}
	if prID != nil && task.Anchor != nil && task.Anchor.Type == "PULL_REQUEST" && task.Anchor.ID != *prID {
		return toolResult{}, fmt.Errorf("Task #%d does not belong to PR #%d.", *taskID, *prID)
	}
	newState := "OPEN"
	if action == "resolve" {
		newState = "RESOLVED"
	}
	updated, err := bbDecode[bbTask](c, "PUT", fmt.Sprintf("/tasks/%d?version=%d", *taskID, task.Version), map[string]any{"id": task.ID, "state": newState, "text": task.Text})
	if err != nil {
		return toolResult{}, err
	}
	if updated == nil {
		return textResult(fmt.Sprintf("Task #%d %s.", *taskID, newState)), nil
	}
	return textResult(fmt.Sprintf("Task #%d is now %s: %q", updated.ID, updated.State, updated.Text)), nil
}

// remapLineThroughDiff remaps a source-side line through an interim diff.
// Returns (destLine, true) if the line survives unchanged in a CONTEXT segment
// (or there is no interim diff), or (0, false) if it was modified/removed.
func (c *BitbucketClient) remapLineThroughDiff(projectKey, repoSlug, filePath, sinceHash, untilHash string, sourceLine int) (int, bool) {
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
