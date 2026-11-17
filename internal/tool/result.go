package tool

import (
	"errors"
)

// ── Errors ───────────────────────────────────────────────────────────────────

// RPCError is a structured error a tool may return for invalid params; the MCP
// layer surfaces it as an isError tool result. Kept as a small typed error so
// validation helpers can carry a code + message.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

const CodeInvalidParams = -32602

// ── Tool result types ────────────────────────────────────────────────────────

type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type Result struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

func TextResult(s string) Result {
	return Result{Content: []ContentBlock{{Type: "text", Text: s}}}
}

var ErrUnknownTool = errors.New("unknown tool")
