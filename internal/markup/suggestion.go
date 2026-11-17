package markup

import (
	"regexp"
)

var SuggestionBlockRe = regexp.MustCompile("(?s)```suggestion[^\\n]*\\n.*?\\n```")
