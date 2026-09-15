package main

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

// attach_files exists for the file that has no path to pass. A screenshot the
// user pasted or dragged into the conversation is not on disk anywhere this
// server could look, and MCP has no client→server file transfer to carry it
// (SEP-2631 would; it is still draft). Asking the user to save it somewhere and
// type the path is the workaround this replaces. It also covers a path the
// server turns out not to be allowed to read, on hosts that confine it.
//
// MCP Apps is the way in: the tool declares a ui:// resource, the host renders
// it as an iframe in the chat, and that iframe is an ordinary browser — its file
// picker, drop target and paste handler need no path and no filesystem access on
// our side. It hands the bytes back as data: URIs through tools/call, which
// resolveAttachmentSources already understands.
//
// The page is self-contained: the host serves it under `default-src 'none'`, so
// there is no CDN and no second resource to fetch. It carries its own ~2 KB MCP
// Apps bridge rather than the official SDK, which needs ~400 KB of zod to
// validate messages this page already treats as untrusted.
//
//go:embed ui/upload.html
var uploadWidgetHTML []byte

func uploadWidget() string { return string(uploadWidgetHTML) }

const uploadWidgetURI = "ui://atlassian-mcp/upload"

// attachFiles serves both halves of that round trip. Called by the model with a
// target and no files, it just opens the panel; called by the panel with the
// same target plus data: URIs, it performs the upload.
func attachFiles(session *sessionState, args map[string]any) (toolResult, error) {
	args = normalizeBitbucketArgs(args)
	issueKey := strings.TrimSpace(argString(args, "issueKey"))
	prID := argInt(args, "prId")
	files := argStrSlice(args, "files")

	if issueKey == "" && prID == 0 {
		return toolResult{}, errors.New("issueKey (a Jira issue) or prId (a Bitbucket PR) is required — it names where the files should land.")
	}
	if issueKey != "" && prID != 0 {
		return toolResult{}, errors.New("Pass issueKey or prId, not both — one upload goes to one place.")
	}
	if issueKey != "" && jira == nil {
		return toolResult{}, errors.New("Jira is not configured.")
	}
	if prID != 0 && bitbucket == nil {
		return toolResult{}, errors.New("Bitbucket is not configured.")
	}

	// No files yet: this is the model opening the panel. The host renders the
	// widget from the tool's _meta.ui.resourceUri alongside this text.
	if len(files) == 0 {
		// Graceful degradation, as the apps spec requires: a host that renders
		// no widget must get an answer it can act on rather than a panel that
		// never appears. Claude Code is such a host today — there, every path
		// and URL this server could already take still works unchanged.
		if !session.appsSupported() {
			return textResult(fmt.Sprintf("This client does not support MCP Apps, so there is no upload panel to show. Attach to %s with a file path or an http(s) URL instead — %s on jira_mutate, or bitbucket_mutate / bitbucket_comment for a PR. Ask the user for the path or URL if you do not have one.", attachTargetLabel(issueKey, prID), "`attachments`")), nil
		}
		return textResult(fmt.Sprintf("Upload panel open for %s. The user picks, drops or pastes files there and the panel uploads them itself — do not call this again and do not fill in `files` yourself. If the user says they see no panel, fall back to asking for a file path or URL and use the `attachments` argument on jira_mutate / bitbucket_mutate / bitbucket_comment.", attachTargetLabel(issueKey, prID))), nil
	}

	if issueKey != "" {
		names, err := jira.uploadAttachments(issueKey, files)
		if err != nil {
			return toolResult{}, err
		}
		return textResult(fmt.Sprintf("Attached %s to %s.", strings.Join(names, ", "), issueKey)), nil
	}

	// Bitbucket stores attachments on the repo, so an upload nothing references
	// is invisible — route through the same update path bitbucket_mutate uses so
	// the markup lands in the PR description (or a comment, when one is named).
	repoRoot := resolveRepoRoot(session, args)
	if commentID := argInt(args, "commentId"); commentID != 0 {
		return bitbucket.updatePrComment(map[string]any{
			"prId":        prID,
			"commentId":   commentID,
			"projectKey":  argString(args, "projectKey"),
			"repoSlug":    argString(args, "repoSlug"),
			"attachments": toAnySlice(files),
		}, repoRoot)
	}
	return bitbucket.updatePullRequest(argString(args, "projectKey"), argString(args, "repoSlug"), repoRoot, prID, nil, nil, nil, nil, false, files)
}

func attachTargetLabel(issueKey string, prID int) string {
	if issueKey != "" {
		return issueKey
	}
	return fmt.Sprintf("PR #%d", prID)
}
