package logx

import (
	"fmt"
	"os"
)

func Logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[atlassian-mcp] "+format+"\n", args...)
}
