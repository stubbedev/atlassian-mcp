package markup

import (
	"strings"

	"github.com/stubbedev/atlassian-mcp/internal/arg"
)

// Text posted through this server lands under the user's own account, so
// readers cannot tell AI writing from human writing, and neither Jira nor
// Bitbucket Server has a built-in attribution field. Every write path instead
// appends a bold [AI] line. Disabled with "markAIText": false in the config
// file or ATLASSIAN_MCP_MARK_AI_TEXT=false, and skipped per comment for text
// the user explicitly asked to have posted in their name (authoredByUser).

const (
	AIMarkerWiki     = "*[AI]*"
	AIMarkerMarkdown = "**[AI]**"
)

var MarkAITextEnabled = true

type Authorship int

const (
	AuthoredByAI Authorship = iota
	AuthoredByUser
)

func ParseAuthorship(args map[string]any, key string) Authorship {
	if arg.Bool(args, key) {
		return AuthoredByUser
	}
	return AuthoredByAI
}

// AppendAIMarkerWiki adds the attribution line to Jira-bound text, which is
// wiki markup by the time it gets here (markdownToJiraWiki runs first), so the
// marker can never be rewritten or swallowed by conversion. A literal
// {code}/{noformat} block left open would run to the end of the field — a
// marker appended after it would render inside the block, so it is skipped.
func AppendAIMarkerWiki(text string, by Authorship) string {
	if by == AuthoredByUser || !MarkAITextEnabled || strings.TrimSpace(text) == "" {
		return text
	}
	if strings.HasSuffix(strings.TrimRight(text, " \t\r\n"), AIMarkerWiki) {
		return text
	}
	remainder := wikiBlockRe.ReplaceAllString(text, "")
	if strings.Contains(remainder, "{code") || strings.Contains(remainder, "{noformat") {
		return text
	}
	return strings.TrimRight(text, " \t\r\n") + "\n\n" + AIMarkerWiki
}

// AppendAIMarkerMarkdown adds the attribution line to Bitbucket-bound text.
// Bitbucket renders markdown as sent, so two guards the wiki path gets for
// free from running after conversion are needed here: an unterminated code
// fence would swallow the marker, and a trailing ```suggestion block must stay
// last for Bitbucket's apply-suggestion UI, so the marker moves in front of it.
func AppendAIMarkerMarkdown(text string, by Authorship) string {
	if by == AuthoredByUser || !MarkAITextEnabled || strings.TrimSpace(text) == "" {
		return text
	}
	trimmed := strings.TrimRight(text, " \t\r\n")
	if strings.HasSuffix(trimmed, AIMarkerMarkdown) {
		return text
	}
	if endsInsideMarkdownFence(trimmed) {
		return text
	}
	if start, ok := endsWithSuggestionBlock(trimmed); ok {
		prefix := strings.TrimRight(trimmed[:start], "\n")
		gap := "\n\n"
		if prefix == "" {
			gap = ""
		}
		return prefix + gap + AIMarkerMarkdown + "\n\n" + trimmed[start:]
	}
	return trimmed + "\n\n" + AIMarkerMarkdown
}

// StripAIMarker removes the attribution line — wherever it sits, since the
// suggestion layout puts it before the trailing suggestion block — so text
// this server posted earlier compares equal to the same text before it was
// marked.
func StripAIMarker(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimRight(line, " \t\r"); trimmed == AIMarkerMarkdown || trimmed == AIMarkerWiki {
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// endsInsideMarkdownFence reports whether the text ends inside an open fenced
// code block. Mirrors CommonMark closely enough for posted text: fences open
// with ```/~~~ (3+ markers, up to 3 leading spaces) and close with a fence of
// the same character, at least as long, with nothing after it.
func endsInsideMarkdownFence(text string) bool {
	inFence := false
	var fence byte
	openLen := 0
	for raw := range strings.SplitSeq(text, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		indent := 0
		for indent < len(line) && indent < 3 && line[indent] == ' ' {
			indent++
		}
		rest := line[indent:]
		if len(rest) < 3 || (rest[0] != '`' && rest[0] != '~') {
			continue
		}
		n := 0
		for n < len(rest) && rest[n] == rest[0] {
			n++
		}
		if n < 3 {
			continue
		}
		info := strings.TrimSpace(rest[n:])
		if !inFence {
			if rest[0] == '`' && strings.Contains(info, "`") {
				continue
			}
			inFence, fence, openLen = true, rest[0], n
			continue
		}
		if rest[0] == fence && n >= openLen && info == "" {
			inFence = false
		}
	}
	return inFence
}

// endsWithSuggestionBlock reports whether the text ends with a ```suggestion
// block, returning the index where the block starts.
func endsWithSuggestionBlock(text string) (int, bool) {
	locs := SuggestionBlockRe.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return 0, false
	}
	last := locs[len(locs)-1]
	return last[0], last[1] == len(text)
}
