package markup

import (
	"strings"
	"testing"
)

func TestMarkdownToJiraWiki(t *testing.T) {
	in := "## Heading\n" +
		"Some **bold** and `code` and [link](http://x/y).\n" +
		"- one\n" +
		"  - nested\n" +
		"1. first\n" +
		"```go\nx := **not bold**\n```\n"
	got, converted := MarkdownToJiraWiki(in)
	if !converted {
		t.Fatal("should report a conversion")
	}
	for _, want := range []string{"h2. Heading", "*bold*", "{{code}}", "[link|http://x/y]", "\n* one", "\n** nested", "\n# first", "{code:go}", "x := **not bold**"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Text that is already wiki markup must survive untouched, including
	// markdown-looking punctuation inside a wiki code block.
	wiki := "h2. Heading\n*bold* and {{code}} and [link|http://x/y]\n* one\n{code:go}\nx := **1** - 2\n{code}"
	if out, converted := MarkdownToJiraWiki(wiki); converted || out != wiki {
		t.Errorf("wiki markup was rewritten:\n%s", out)
	}
}

func TestMarkdownCodeFenceVariants(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain fence", "```go\nx\n```", "{code:go}\nx\n{code}"},
		{"lang with hash", "```c#\nx\n```", "{code:c#}\nx\n{code}"},
		{"info with extra words", "```shell script\nx\n```", "{code:shell}\nx\n{code}"},
		{"info with attributes", "```js copy startline=3\nx\n```", "{code:js}\nx\n{code}"},
		{"unrecognized lang kept", "```fantom-lang\nx\n```", "{code:fantom-lang}\nx\n{code}"},
		{"tilde fence", "~~~python\nprint(1)\n~~~", "{code:python}\nprint(1)\n{code}"},
		{"bare tilde fence", "~~~\nx\n~~~", "{code}\nx\n{code}"},
		{"four backticks", "````\n```\nx\n```\n````", "{code}\n```\nx\n```\n{code}"},
		{"no stray backticks", "````\nx\n````", "{code}\nx\n{code}"},
		{"closing fence trailing spaces", "```go\nx\n```   ", "{code:go}\nx\n{code}"},
		{"empty body", "```\n```", "{code}\n{code}"},
		{"unterminated runs to end", "```go\nx\nstill code", "{code:go}\nx\nstill code\n{code}"},
		{"crlf fences", "```go\r\nx\r\n```", "{code:go}\nx\n{code}"},
		{"indented fence keeps indent", "- item\n  ```go\n  x\n  ```", "* item\n  {code:go}\n  x\n  {code}"},
		{"inline code inside body untouched", "```go\n`not mono`\n```", "{code:go}\n`not mono`\n{code}"},
		{"trailing blank lines trimmed", "```go\nx\n\n\n```", "{code:go}\nx\n{code}"},
	} {
		got, converted := MarkdownToJiraWiki(tc.in)
		if !converted || got != tc.want {
			t.Errorf("%s:\nmarkdownToJiraWiki(%q) = %q, %v; want %q", tc.name, tc.in, got, converted, tc.want)
		}
	}
}

func TestMarkdownInlineCodeVariants(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		converted      bool
	}{
		{name: "single backticks", in: "use `make all` here", want: "use {{make all}} here", converted: true},
		{name: "double backticks", in: "use ``a`b`` here", want: "use {{a`b}} here", converted: true},
		{name: "double with space", in: "use `` `escaped` `` here", want: "use {{`escaped`}} here", converted: true},
		{name: "unpaired backtick stays", in: "a ` b", want: "a ` b"},
		{name: "bold inside inline code stays literal", in: "`**x**`", want: "{{&#42;&#42;x&#42;&#42;}}", converted: true},
		{name: "braces are not a macro", in: "Route `/folder/{folder}` here", want: "Route {{/folder/&#123;folder&#125;}} here", converted: true},
		{name: "glob star", in: "pattern `KEYS prefix:*`", want: "pattern {{KEYS prefix&#58;&#42;}}", converted: true},
		{name: "issue 30 specials", in: "`{x} * [a] |`", want: "{{&#123;x&#125; &#42; &#91;a&#93; &#124;}}", converted: true},
		{name: "every special", in: "`{}[]*_+^~-|!?\\&():`", want: "{{&#123;&#125;&#91;&#93;&#42;&#95;&#43;&#94;&#126;&#45;&#124;&#33;&#63;&#92;&#38;&#40;&#41;&#58;}}", converted: true},
		{name: "backslash is not a line break", in: "`a\\\\b`", want: "{{a&#92;&#92;b}}", converted: true},
		{name: "literal entity stays literal", in: "`&#42;`", want: "{{&#38;#42;}}", converted: true},
		{name: "emoticons stay text", in: "`:) (/)`", want: "{{&#58;&#41; &#40;/&#41;}}", converted: true},
		{name: "single space pair stripped", in: "` x `", want: "{{x}}", converted: true},
		{name: "edge spaces kept as entities", in: "``  x  ``", want: "{{&#32;x&#32;}}", converted: true},
		{name: "one-sided space kept", in: "` x`", want: "{{&#32;x}}", converted: true},
		{name: "all spaces kept", in: "`  `", want: "{{&#32;&#32;}}", converted: true},
		{name: "inline across lines stays", in: "a ` b\nc ` d", want: "a ` b\nc ` d"},
	} {
		got, converted := MarkdownToJiraWiki(tc.in)
		if converted != tc.converted || got != tc.want {
			t.Errorf("%s:\nmarkdownToJiraWiki(%q) = %q, %v; want %q, %v", tc.name, tc.in, got, converted, tc.want, tc.converted)
		}
	}
}

func TestMarkdownTables(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		converted      bool
	}{
		{name: "simple table", in: "| a | b |\n| --- | --- |\n| 1 | 2 |", want: "|| a || b ||\n| 1 | 2 |", converted: true},
		{name: "no outer pipes", in: "a | b\n--- | ---\n1 | 2", want: "|| a || b ||\n| 1 | 2 |", converted: true},
		{name: "alignment dropped", in: "| a | b |\n| :-- | --: |\n| 1 | 2 |", want: "|| a || b ||\n| 1 | 2 |", converted: true},
		{name: "short rows padded", in: "| a | b |\n| --- | --- |\n| 1 |", want: "|| a || b ||\n| 1 |  |", converted: true},
		{name: "long rows clipped", in: "| a |\n| --- |\n| 1 | 2 |", want: "|| a ||\n| 1 |", converted: true},
		{name: "header only", in: "| a |\n| --- |", want: "|| a ||", converted: true},
		{name: "escaped pipe in cell", in: "| a \\| b |\n| --- |\n| c |", want: "|| a | b ||\n| c |", converted: true},
		{name: "cell count mismatch left alone", in: "| a |\n| --- | --- |\n| 1 |", want: "| a |\n| --- | --- |\n| 1 |"},
		{name: "wiki table passthrough", in: "|| a || b ||\n| 1 | 2 |", want: "|| a || b ||\n| 1 | 2 |"},
		{name: "pipes in prose left alone", in: "either | or\nnothing to see", want: "either | or\nnothing to see"},
	} {
		got, converted := MarkdownToJiraWiki(tc.in)
		if converted != tc.converted || got != tc.want {
			t.Errorf("%s:\nmarkdownToJiraWiki(%q) = %q, %v; want %q, %v", tc.name, tc.in, got, converted, tc.want, tc.converted)
		}
	}
	// Inline formatting inside cells survives the table rewrite.
	got, _ := MarkdownToJiraWiki("| `x` | **y** | [z](http://u) |\n| --- | --- | --- |")
	for _, want := range []string{"|| {{x}} || *y* || [z|http://u] ||"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// A `|` inside an inline code span does not split the cell.
	if got, _ := MarkdownToJiraWiki("| `a|b` |\n| --- |"); !strings.Contains(got, "{{a&#124;b}}") {
		t.Errorf("code span with pipe broken by table scan: %q", got)
	}
}

func TestMarkdownQuotesAndEmphasis(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		converted      bool
	}{
		{name: "single line quote", in: "> note", want: "bq. note", converted: true},
		{name: "multi line quote", in: "> a\n> b", want: "{quote}\na\nb\n{quote}", converted: true},
		{name: "nested markers flatten", in: ">> deep", want: "bq. deep", converted: true},
		{name: "blank line inside quote", in: "> a\n>\n> b", want: "{quote}\na\n\nb\n{quote}", converted: true},
		{name: "two quote blocks", in: "> a\n\n> b", want: "bq. a\n\nbq. b", converted: true},
		{name: "heading inside quote block", in: "> ## H\n> body", want: "{quote}\nh2. H\nbody\n{quote}", converted: true},
		{name: "bullet inside quote block", in: "> - item", want: "{quote}\n* item\n{quote}", converted: true},
		{name: "emphasis inside quote", in: "> **x** and [l](http://u)", want: "bq. *x* and [l|http://u]", converted: true},
		{name: "quoted fence", in: "> ```go\n> x\n> ```", want: "{quote}\n{code:go}\nx\n{code}\n{quote}", converted: true},
		{name: "gt inside fence body literal", in: "```\n> literal\n```", want: "{code}\n> literal\n{code}", converted: true},
		{name: "bold italic", in: "***x***", want: "*_x_*", converted: true},
		{name: "bold italic underscores", in: "___x___", want: "*_x_*", converted: true},
		{name: "autolink", in: "see <https://a.b/c> now", want: "see [https://a.b/c|https://a.b/c] now", converted: true},
		{name: "paren ordered list", in: "1) first\n2) second", want: "# first\n# second", converted: true},
		{name: "dot ordered list unchanged path", in: "1. first", want: "# first", converted: true},
	} {
		got, converted := MarkdownToJiraWiki(tc.in)
		if converted != tc.converted || got != tc.want {
			t.Errorf("%s:\nmarkdownToJiraWiki(%q) = %q, %v; want %q, %v", tc.name, tc.in, got, converted, tc.want, tc.converted)
		}
	}
}

func TestMarkdownKitchenSink(t *testing.T) {
	in := "## Deploy notes\n" +
		"Run `make build` (see [docs](https://x/d)) or <https://x/d>.\n" +
		"- step **one**\n" +
		"  1) check `foo`\n" +
		"> warning: ***read this***\n" +
		"| env | status |\n" +
		"| --- | --- |\n" +
		"| prod | ~~down~~ |\n" +
		"```bash\nmake deploy # **literal**\n```"
	want := "h2. Deploy notes\n" +
		"Run {{make build}} (see [docs|https://x/d]) or [https://x/d|https://x/d].\n" +
		"* step *one*\n" +
		"## check {{foo}}\n" +
		"bq. warning: *_read this_*\n" +
		"|| env || status ||\n" +
		"| prod | -down- |\n" +
		"{code:bash}\nmake deploy # **literal**\n{code}"
	if got, converted := MarkdownToJiraWiki(in); !converted || got != want {
		t.Errorf("markdownToJiraWiki kitchen sink =\n%s\nwant:\n%s", got, want)
	}
}

func TestWikiMarkupPassesThroughUntouched(t *testing.T) {
	// Everything a wiki-savvy caller might write must survive byte for byte,
	// including "##" nested ordered lists, which look like markdown headings.
	wiki := "* bullet\n** nested\n*** deeper\n# one\n## two\n_italic_\n-strike-\n" +
		"[link|http://x]\n{quote}\nq *b*\n{quote}\nbq. quoted\n----\n|| a || b ||\n| 1 | 2 |\n" +
		"{noformat}\n**raw**\n{noformat}\nh3. Head\n{{mono}}"
	if out, converted := MarkdownToJiraWiki(wiki); converted || out != wiki {
		t.Errorf("wiki markup was rewritten:\n%s", out)
	}
	// A markdown heading is still converted once the list block has ended.
	md := "# one\n## two\n\n## Real Heading"
	want := "# one\n## two\n\nh2. Real Heading"
	if out, _ := MarkdownToJiraWiki(md); out != want {
		t.Errorf("heading after list = %q, want %q", out, want)
	}
}

func TestConsecutiveMarkdownHeadings(t *testing.T) {
	in := "## A\n## B\n\ntext\n# item\n## nested"
	want := "h2. A\nh2. B\n\ntext\n# item\n## nested"
	if out, _ := MarkdownToJiraWiki(in); out != want {
		t.Errorf("consecutive headings = %q, want %q", out, want)
	}
}

func TestAppendAIMarkerWiki(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "Crashes on save", "Crashes on save\n\n" + AIMarkerWiki},
		{"already marked", "x\n\n" + AIMarkerWiki, "x\n\n" + AIMarkerWiki},
		{"trailing whitespace trimmed", "x  \n", "x\n\n" + AIMarkerWiki},
		{"after closed code block", "{code}\nselect 1\n{code}", "{code}\nselect 1\n{code}\n\n" + AIMarkerWiki},
		{"empty stays empty", "  ", "  "},
	}
	for _, c := range cases {
		if got := AppendAIMarkerWiki(c.in, AuthoredByAI); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// An unclosed {code} runs to the end of the field: a marker appended after
	// it would render inside the block, so the text is left unmarked.
	if got := AppendAIMarkerWiki("{code:sql\nselect 1", AuthoredByAI); got != "{code:sql\nselect 1" {
		t.Errorf("unclosed code block: got %q", got)
	}
	// The real write path converts markdown first, so an unterminated markdown
	// fence is closed into {code} and the marker lands after it, never inside.
	wiki, _ := MarkdownToJiraWiki("look:\n```go\nfmt.Println(1)")
	if got := AppendAIMarkerWiki(wiki, AuthoredByAI); !strings.HasSuffix(got, "{code}\n\n"+AIMarkerWiki) {
		t.Errorf("converted fence: got %q", got)
	}
	MarkAITextEnabled = false
	defer func() { MarkAITextEnabled = true }()
	if got := AppendAIMarkerWiki("x", AuthoredByAI); got != "x" {
		t.Errorf("disabled: got %q", got)
	}
}

func TestAppendAIMarkerMarkdown(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "LGTM", "LGTM\n\n" + AIMarkerMarkdown},
		{"already marked", "LGTM\n\n" + AIMarkerMarkdown, "LGTM\n\n" + AIMarkerMarkdown},
		{"after closed fence", "```go\nx\n```", "```go\nx\n```\n\n" + AIMarkerMarkdown},
		{"inline code span intact", "use `foo` here", "use `foo` here\n\n" + AIMarkerMarkdown},
		{"empty stays empty", " ", " "},
	}
	for _, c := range cases {
		if got := AppendAIMarkerMarkdown(c.in, AuthoredByAI); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// An unterminated fence would swallow the marker, so it is skipped.
	open := "```python\nprint(1)"
	if got := AppendAIMarkerMarkdown(open, AuthoredByAI); got != open {
		t.Errorf("open fence: got %q", got)
	}
	// A trailing suggestion block must stay last; the marker goes before it.
	sugg := "drop the loop\n\n```suggestion\nx := 1\n```"
	want := "drop the loop\n\n" + AIMarkerMarkdown + "\n\n```suggestion\nx := 1\n```"
	if got := AppendAIMarkerMarkdown(sugg, AuthoredByAI); got != want {
		t.Errorf("suggestion: got %q, want %q", got, want)
	}
	if got := StripAIMarker(want); got != sugg {
		t.Errorf("stripAIMarker: got %q, want %q", got, sugg)
	}
}

func TestAppendAIMarkerSkipsUserAuthored(t *testing.T) {
	if got := AppendAIMarkerWiki("Ship it", AuthoredByUser); got != "Ship it" {
		t.Errorf("wiki: got %q", got)
	}
	if got := AppendAIMarkerMarkdown("Ship it", AuthoredByUser); got != "Ship it" {
		t.Errorf("markdown: got %q", got)
	}
}
