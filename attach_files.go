package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// attach_files is the way a file reaches Jira or Bitbucket from a host where
// this server cannot see the filesystem — Claude Desktop runs extensions in an
// OS sandbox, so a path the user names is unreadable here and a file they drop
// in the chat never reaches a server at all (MCP has no client→server file
// transfer; SEP-2631 is still draft).
//
// The way around both is MCP Apps: the tool declares a ui:// resource, the host
// renders it as an iframe in the chat, and that iframe is an ordinary browser —
// its file picker, drop target and paste handler are outside our sandbox. It
// hands the bytes back as data: URIs through tools/call, which resolveAttachmentSources
// already understands.
//
//go:embed ui/upload.html
var uploadWidgetRaw []byte

// The MCP Apps SDK, vendored by scripts/vendor-ext-apps.sh. It is spliced into
// the widget rather than linked: the host serves the document under
// `default-src 'none'`, so a CDN is unreachable and a second resource is not
// fetchable either.
//
//go:embed ui/vendor/ext-apps.js
var extAppsJS []byte

const extAppsMarker = "/*@ext-apps@*/"

// uploadWidget is the served document, assembled once.
var uploadWidget = sync.OnceValue(func() string {
	if !bytes.Contains(uploadWidgetRaw, []byte(extAppsMarker)) {
		logf("upload widget is missing the %s marker — the MCP Apps SDK will not load", extAppsMarker)
		return string(uploadWidgetRaw)
	}
	return string(bytes.Replace(uploadWidgetRaw, []byte(extAppsMarker), extAppsJS, 1))
})

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
