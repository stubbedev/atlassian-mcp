package config

import (
	"net/url"
)

// FileURIToPath converts a file:// URI to a local filesystem path. Returns ""
// for non-file URIs.
func FileURIToPath(uri string) string {
	if uri == "" {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		// Some clients send bare paths; accept absolute ones.
		if len(uri) > 0 && uri[0] == '/' {
			return uri
		}
		// Windows drive path (C:\repo, C:/repo) — url.Parse reads "C:" as a
		// scheme, so it never reaches the file:// branch above.
		if len(uri) >= 3 && uri[1] == ':' && (uri[2] == '\\' || uri[2] == '/') {
			return uri
		}
		return ""
	}
	p := u.Path
	// On Windows, "file:///C:/x" parses to "/C:/x"; strip the leading slash
	// so the drive letter starts the path.
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return p
}
