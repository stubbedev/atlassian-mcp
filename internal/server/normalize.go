package server

import (
	"github.com/stubbedev/atlassian-mcp/internal/arg"
	"github.com/stubbedev/atlassian-mcp/internal/tool"
)

// ── Alias normalisation (project→projectKey, repo→repoSlug) ───────────────────

func normalizeBitbucketArgs(args map[string]any) map[string]any {
	if _, ok := args["projectKey"].(string); !ok {
		if p, ok := args["project"].(string); ok {
			args["projectKey"] = p
		}
	}
	if _, ok := args["repoSlug"].(string); !ok {
		if r, ok := args["repo"].(string); ok {
			args["repoSlug"] = r
		}
	}
	return args
}

func normalizeJiraProjectArgs(args map[string]any) map[string]any {
	if _, ok := args["projectKey"].(string); !ok {
		if p, ok := args["project"].(string); ok {
			args["projectKey"] = p
		}
	}
	return args
}

func normalizeJiraMutateArgs(args map[string]any) map[string]any {
	normalizeJiraProjectArgs(args)
	if create := arg.Map(args, "create"); create != nil {
		if _, ok := create["projectKey"].(string); !ok {
			if p, ok := create["project"].(string); ok {
				create["projectKey"] = p
			}
		}
	}
	return args
}

// validateAttachmentArgs enforces the same numeric ranges as the TS server,
// returning a protocol-level InvalidParams error on violation.
func validateAttachmentArgs(a map[string]any) *tool.RPCError {
	bad := func(msg string) *tool.RPCError { return &tool.RPCError{Code: tool.CodeInvalidParams, Message: msg} }
	if arg.Has(a, "frames") {
		f := arg.Float(a, "frames")
		if f < 1 || f > 60 {
			return bad("frames must be a number between 1 and 60.")
		}
	}
	if arg.Has(a, "start") {
		if arg.Float(a, "start") < 0 {
			return bad("start must be a non-negative number of seconds.")
		}
	}
	if arg.Has(a, "end") {
		if arg.Float(a, "end") <= 0 {
			return bad("end must be a positive number of seconds.")
		}
	}
	if arg.Has(a, "start") && arg.Has(a, "end") {
		if arg.Float(a, "end") <= arg.Float(a, "start") {
			return bad("end must be greater than start.")
		}
	}
	if arg.Has(a, "mode") {
		m := arg.String(a, "mode")
		if m != "uniform" && m != "scenes" {
			return bad(`mode must be "uniform" or "scenes".`)
		}
	}
	if arg.Has(a, "maxDimension") {
		d := arg.Float(a, "maxDimension")
		if d < 64 || d > 4096 {
			return bad("maxDimension must be a number between 64 and 4096.")
		}
	}
	if arg.Has(a, "quality") {
		q := arg.Float(a, "quality")
		if q < 1 || q > 100 {
			return bad("quality must be a number between 1 and 100.")
		}
	}
	if arg.Has(a, "sceneThreshold") {
		s := arg.Float(a, "sceneThreshold")
		if s <= 0 || s > 1 {
			return bad("sceneThreshold must be a number in (0, 1].")
		}
	}
	return nil
}
