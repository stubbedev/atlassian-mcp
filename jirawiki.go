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
	wikiBlockRe    = regexp.MustCompile(`(?s)\{code(?::[^}]*)?\}.*?\{code\}|\{noformat\}.*?\{noformat\}`)
	mdInlineDblRe  = regexp.MustCompile("``((?:[^`\n]|`[^`\n])+)``")
	mdInlineRe     = regexp.MustCompile("`([^`\n]+)`")
	mdHeadingRe    = regexp.MustCompile(`(?m)^(#{2,6})[ \t]+(.+)$`)
	mdBoldItRe     = regexp.MustCompile(`\*\*\*([^*\n]+)\*\*\*`)
	mdBoldItUlRe   = regexp.MustCompile(`___([^_\n]+)___`)
	mdAutolinkRe   = regexp.MustCompile(`<(https?://[^>\s]+)>`)
	mdQuoteLineRe  = regexp.MustCompile(`^([ \t]*)((?:>[ \t]?)+)(.*)$`)
	mdBoldRe       = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdBoldUlRe     = regexp.MustCompile(`__([^_\n]+)__`)
	mdStrikeRe     = regexp.MustCompile(`~~([^~\n]+)~~`)
	mdImageRe      = regexp.MustCompile(`!\[[^\]\n]*\]\(([^)\s]+)\)`)
	mdLinkRe       = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
	mdBulletRe     = regexp.MustCompile(`(?m)^([ \t]*)[-*+][ \t]+(.+)$`)
	mdOrderedRe    = regexp.MustCompile(`(?m)^([ \t]*)\d+[.)][ \t]+(.+)$`)
	mdLineBlockRe  = regexp.MustCompile("^(?:#{1,6}[ \\t]|[-*+][ \\t]|\\d+[.)][ \\t]|`{3,}|~{3,})")
	wikiListLineRe = regexp.MustCompile(`^[ \t]*[*#]+[ \t]`)
	mdHrRe         = regexp.MustCompile(`(?m)^\s*(---|\*\*\*|___)\s*$`)
	tableSepCellRe = regexp.MustCompile(`^:?-+:?$`)
	placeholderF   = "\x00mdc%d\x00"
	placeholderR   = regexp.MustCompile("\x00mdc(\\d+)\x00")
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

	// Code first: nothing inside a code block should be rewritten. Quotes are
	// unwrapped between two fence passes, so a quoted fence ("> ```") converts
	// too, while ">" lines inside an already-stashed fence body stay literal.
	text = convertFences(text, stash)
	text = convertQuotes(text)
	text = convertFences(text, stash)
	text = mdInlineDblRe.ReplaceAllStringFunc(text, func(m string) string {
		return stash("{{" + mdInlineDblRe.FindStringSubmatch(m)[1] + "}}")
	})
	text = mdInlineRe.ReplaceAllStringFunc(text, func(m string) string {
		return stash("{{" + mdInlineRe.FindStringSubmatch(m)[1] + "}}")
	})
	// Tables run after the inline-code stash so a `|` inside a code span
	// cannot split a cell in two.
	text = convertTables(text)

	text = convertHeadings(text)
	text = mdImageRe.ReplaceAllString(text, "!$1!")
	text = mdLinkRe.ReplaceAllString(text, "[$1|$2]")
	text = mdAutolinkRe.ReplaceAllString(text, "[$1|$1]")
	// Bold+italic must run before the plain bold passes or ***x*** would
	// convert to literal asterisks around *x*.
	text = mdBoldItRe.ReplaceAllString(text, "*_${1}_*")
	text = mdBoldItUlRe.ReplaceAllString(text, "*_${1}_*")
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
		_, _ = fmt.Sscanf(m, "\x00mdc%d\x00", &i)
		if i < 0 || i >= len(blocks) {
			return m
		}
		return blocks[i]
	})
	return text, text != original
}

// convertHeadings rewrites markdown "##".."######" headings to wiki h2.-h6.
// with one guard the regex alone cannot express: wiki ordered lists nest as
// "#", "##", "###" exactly like markdown headings, so a ## line that sits
// inside a wiki list block is a nested list item, not a heading. Markdown
// never marks lists with #, so only heading intent is lost, never list intent.
func convertHeadings(text string) string {
	lines := strings.Split(text, "\n")
	inList := false
	for i, line := range lines {
		switch {
		case strings.TrimSpace(line) == "":
			inList = false
		case mdHeadingRe.MatchString(line):
			if inList {
				// stays a nested wiki list marker
				continue
			}
			g := mdHeadingRe.FindStringSubmatch(line)
			lines[i] = fmt.Sprintf("h%d. %s", len(g[1]), g[2])
			inList = false
		case wikiListLineRe.MatchString(line):
			inList = true
		case line[0] == ' ' || line[0] == '\t':
			// indented continuation lines belong to the list item above them
		default:
			inList = false
		}
	}
	return strings.Join(lines, "\n")
}

// convertFences rewrites markdown fenced code blocks to {code} macros in one
// line-oriented pass. A plain ``` regex is not enough: it cannot pair a
// closing fence with its opener (RE2 has no backreferences), so four-backtick
// fences convert with stray backticks left around them, tilde fences and info
// strings like "c#" or "shell script" do not match at all — those are the code
// blocks that used to reach Jira as literal punctuation. An unterminated fence
// runs to the end of the text, which is how CommonMark reads it, so raw fence
// markers never survive into wiki markup.
func convertFences(text string, stash func(string) string) string {
	lines := strings.Split(text, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		indent, marker, info, ok := splitFenceOpen(strings.TrimRight(lines[i], " \t\r"))
		if !ok {
			out = append(out, lines[i])
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if isFenceClose(strings.TrimRight(lines[j], " \t\r"), marker[0], len(marker)) {
				end = j
				break
			}
		}
		body := strings.TrimRight(strings.Join(lines[i+1:end], "\n"), " \t\r\n")
		open := "{code}"
		if lang := fenceLang(info); lang != "" {
			open = "{code:" + lang + "}"
		}
		// The closing macro inherits the opener's indent so a fence nested in a
		// list item stays inside the list item when rendered.
		block := indent + open + "\n"
		if body != "" {
			block += body + "\n"
		}
		out = append(out, stash(block+indent+"{code}"))
		i = end
	}
	return strings.Join(out, "\n")
}

// splitFenceOpen reports whether a line opens a fenced code block, returning
// the line's indentation, the fence marker (```/````/~~~) and the info string.
// A backtick fence's info string may not contain backticks.
func splitFenceOpen(line string) (indent, marker, info string, ok bool) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	rest := line[i:]
	if len(rest) < 3 || (rest[0] != '`' && rest[0] != '~') {
		return "", "", "", false
	}
	n := 0
	for n < len(rest) && rest[n] == rest[0] {
		n++
	}
	if n < 3 || (rest[0] == '`' && strings.ContainsRune(rest[n:], '`')) {
		return "", "", "", false
	}
	return line[:i], rest[:n], strings.TrimSpace(rest[n:]), true
}

// isFenceClose reports whether a line closes a fence of the given character:
// the marker repeated at least as many times as the opening fence, followed by
// nothing but whitespace.
func isFenceClose(line string, fenceChar byte, openLen int) bool {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	n := 0
	for i+n < len(line) && line[i+n] == fenceChar {
		n++
	}
	return n >= openLen && strings.TrimSpace(line[i+n:]) == ""
}

// fenceLang distills a fence info string ("c#", "shell script", "go
// startline=3") into one {code:lang} token: the first whitespace-separated
// word reduced to characters the code macro's language parameter tolerates.
func fenceLang(info string) string {
	fields := strings.Fields(info)
	if len(fields) == 0 {
		return ""
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '+', r == '-', r == '#', r == '.':
			return r
		}
		return -1
	}, fields[0])
}

// convertQuotes rewrites markdown blockquotes ("> text", possibly nested or
// spanning several lines) into wiki quote markup: bq. for a single line,
// {quote} for a block. Wiki markup has no ">" line syntax, so text that is
// already wiki markup cannot be mangled; nested ">>" markers are flattened.
func convertQuotes(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		m := mdQuoteLineRe.FindStringSubmatch(lines[i])
		if m == nil {
			out = append(out, lines[i])
			continue
		}
		indent := m[1]
		var body []string
		for i < len(lines) {
			m := mdQuoteLineRe.FindStringSubmatch(lines[i])
			if m == nil {
				break
			}
			body = append(body, m[3])
			i++
		}
		i--
		switch {
		case len(body) == 1 && strings.TrimSpace(body[0]) != "" && !mdLineBlockRe.MatchString(body[0]):
			out = append(out, indent+"bq. "+body[0])
		case len(body) == 1 && strings.TrimSpace(body[0]) == "":
			// ">" alone is an empty quote: nothing to render.
		case len(body) == 1:
			// A one-liner that itself needs line context (heading, bullet, list,
			// fence) keeps the {quote} form so the line-oriented passes can
			// still convert its content.
			out = append(out, indent+"{quote}", indent+body[0], indent+"{quote}")
		default:
			out = append(out, indent+"{quote}")
			out = append(out, body...)
			out = append(out, indent+"{quote}")
		}
	}
	return strings.Join(out, "\n")
}

// convertTables rewrites markdown tables into wiki tables:
//
//	| a | b |           || a || b ||
//	| --- | --- |  -->  | 1 | 2 |
//	| 1 | 2 |
//
// The delimiter row is what marks the block as a table, so prose that merely
// contains pipes is never touched, and an existing wiki table (which has no
// delimiter row) passes through. Body rows are padded and clipped to the
// header's column count.
func convertTables(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		header := strings.TrimSpace(lines[i])
		if !strings.Contains(header, "|") || i+1 >= len(lines) || !isTableSeparator(strings.TrimSpace(lines[i+1])) {
			out = append(out, lines[i])
			continue
		}
		cells := splitTableCells(header)
		if len(cells) == 0 || len(cells) != len(splitTableCells(strings.TrimSpace(lines[i+1]))) {
			out = append(out, lines[i])
			continue
		}
		cols := len(cells)
		out = append(out, "|| "+strings.Join(cells, " || ")+" ||")
		i++
		for i+1 < len(lines) {
			row := strings.TrimSpace(lines[i+1])
			if row == "" || !strings.Contains(row, "|") {
				break
			}
			cells := splitTableCells(row)
			for len(cells) < cols {
				cells = append(cells, "")
			}
			out = append(out, "| "+strings.Join(cells[:cols], " | ")+" |")
			i++
		}
	}
	return strings.Join(out, "\n")
}

// splitTableCells splits a markdown table row into cells, tolerating missing
// outer pipes and \| escapes, and trimming each cell.
func splitTableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	var cells []string
	var cell strings.Builder
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			if i+1 < len(line) && line[i+1] == '|' {
				cell.WriteByte('|')
				i++
				continue
			}
			cell.WriteByte('\\')
		case '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cell.String()))
}

// isTableSeparator reports whether a line is a markdown table delimiter row
// (| --- | :---: | ---: |).
func isTableSeparator(line string) bool {
	if !strings.Contains(line, "|") || !strings.Contains(line, "-") {
		return false
	}
	for _, cell := range splitTableCells(line) {
		if !tableSepCellRe.MatchString(cell) {
			return false
		}
	}
	return true
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
	depth := min(width/2+1, 6)
	return depth
}
