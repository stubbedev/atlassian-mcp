package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Jira renders wiki markup, not CommonMark: a comment posted with ``` fences,
// **bold** and [text](url) renders as literal punctuation. Telling the caller
// "use wiki markup" in a tool description only works until it doesn't, so the
// conversion happens here instead — every Jira write path runs its text through
// markdownToJiraWiki.

var (
	wikiBlockRe  = regexp.MustCompile(`(?s)\{code(?::[^}]*)?\}.*?\{code\}|\{noformat\}.*?\{noformat\}`)
	mdFenceRe    = regexp.MustCompile("(?s)```([a-zA-Z0-9_+-]*)[ \t]*\r?\n(.*?)```")
	mdInlineRe   = regexp.MustCompile("`([^`\n]+)`")
	mdHeadingRe  = regexp.MustCompile(`(?m)^(#{2,6})[ \t]+(.+)$`)
	mdBoldRe     = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdBoldUlRe   = regexp.MustCompile(`__([^_\n]+)__`)
	mdStrikeRe   = regexp.MustCompile(`~~([^~\n]+)~~`)
	mdImageRe    = regexp.MustCompile(`!\[[^\]\n]*\]\(([^)\s]+)\)`)
	mdLinkRe     = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
	mdBulletRe   = regexp.MustCompile(`(?m)^([ \t]*)-[ \t]+(.+)$`)
	mdOrderedRe  = regexp.MustCompile(`(?m)^([ \t]*)\d+\.[ \t]+(.+)$`)
	mdHrRe       = regexp.MustCompile(`(?m)^\s*(---|\*\*\*|___)\s*$`)
	placeholderF = "\x00mdc%d\x00"
	placeholderR = regexp.MustCompile("\x00mdc(\\d+)\x00")
)

// markdownToJiraWiki rewrites the CommonMark constructs models actually emit
// into Jira wiki markup and reports whether anything changed. Text that is
// already wiki markup passes through untouched: none of the patterns below
// match {code}, h2., *bold*, [text|url] or {{mono}}.
//
// ponytail: a single leading "#" is left alone — it is a Jira ordered-list
// marker as often as it is a markdown h1, and mangling real wiki markup is
// worse than leaving one heading style unconverted. "##".."######" convert.
func markdownToJiraWiki(text string) (string, bool) {
	if text == "" {
		return text, false
	}
	original := text
	var blocks []string
	stash := func(s string) string {
		blocks = append(blocks, s)
		return fmt.Sprintf(placeholderF, len(blocks)-1)
	}

	// Existing wiki code/noformat blocks are literal too — text inside them is
	// not markdown waiting to be converted.
	text = wikiBlockRe.ReplaceAllStringFunc(text, stash)

	// Code next: nothing inside a code block should be rewritten.
	text = mdFenceRe.ReplaceAllStringFunc(text, func(m string) string {
		g := mdFenceRe.FindStringSubmatch(m)
		lang, body := g[1], strings.TrimRight(g[2], "\r\n")
		open := "{code}"
		if lang != "" {
			open = "{code:" + lang + "}"
		}
		return stash(open + "\n" + body + "\n{code}")
	})
	text = mdInlineRe.ReplaceAllStringFunc(text, func(m string) string {
		return stash("{{" + mdInlineRe.FindStringSubmatch(m)[1] + "}}")
	})

	text = mdHeadingRe.ReplaceAllStringFunc(text, func(m string) string {
		g := mdHeadingRe.FindStringSubmatch(m)
		return fmt.Sprintf("h%d. %s", len(g[1]), g[2])
	})
	text = mdImageRe.ReplaceAllString(text, "!$1!")
	text = mdLinkRe.ReplaceAllString(text, "[$1|$2]")
	text = mdBoldRe.ReplaceAllString(text, "*$1*")
	text = mdBoldUlRe.ReplaceAllString(text, "*$1*")
	text = mdStrikeRe.ReplaceAllString(text, "-$1-")
	text = mdHrRe.ReplaceAllString(text, "----")
	text = mdBulletRe.ReplaceAllStringFunc(text, func(m string) string {
		g := mdBulletRe.FindStringSubmatch(m)
		return strings.Repeat("*", listDepth(g[1])) + " " + g[2]
	})
	text = mdOrderedRe.ReplaceAllStringFunc(text, func(m string) string {
		g := mdOrderedRe.FindStringSubmatch(m)
		return strings.Repeat("#", listDepth(g[1])) + " " + g[2]
	})

	text = placeholderR.ReplaceAllStringFunc(text, func(m string) string {
		var i int
		fmt.Sscanf(m, "\x00mdc%d\x00", &i)
		if i < 0 || i >= len(blocks) {
			return m
		}
		return blocks[i]
	})
	return text, text != original
}

// listDepth maps markdown indentation to wiki nesting level (1-based).
func listDepth(indent string) int {
	width := 0
	for _, r := range indent {
		if r == '\t' {
			width += 4
			continue
		}
		width++
	}
	depth := width/2 + 1
	if depth > 6 {
		depth = 6
	}
	return depth
}
