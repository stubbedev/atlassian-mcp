package bitbucket

import (
	"net/url"
	"strings"
)

// ── git_get_context ──────────────────────────────────────────────────────────

func remoteMatchesBitbucketInstance(remote, bitbucketURL string) bool {
	if remote == "" {
		return false
	}
	u, err := url.Parse(bitbucketURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	return strings.Contains(strings.ToLower(remote), strings.ToLower(u.Hostname()))
}
