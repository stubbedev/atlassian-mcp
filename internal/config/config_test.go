package config

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// ── config: unfilled .mcpb placeholders ──────────────────────────────────────

// TestUnsubstitutedManifestEnvIsBlank covers the Claude Desktop install path:
// a user_config field left blank arrives as the literal "${user_config.x}", not
// as an empty string. Taken at face value it configured Jira with a bogus URL
// and every call died with net/http's "unsupported protocol scheme".
func TestUnsubstitutedManifestEnvIsBlank(t *testing.T) {
	t.Setenv("JIRA_URL", "${user_config.jira_url}")
	t.Setenv("JIRA_ACCESS_TOKEN", "${user_config.jira_token}")
	t.Setenv("BITBUCKET_URL", "https://bb.example.com")
	t.Setenv("BITBUCKET_ACCESS_TOKEN", "real-token")
	t.Setenv("ATLASSIAN_MCP_REPO_ROOT", "${user_config.repo_root}")

	clearUnsubstitutedEnv()

	if v := os.Getenv("JIRA_URL"); v != "" {
		t.Errorf("JIRA_URL = %q, want cleared", v)
	}
	if v := os.Getenv("ATLASSIAN_MCP_REPO_ROOT"); v != "" {
		t.Errorf("ATLASSIAN_MCP_REPO_ROOT = %q, want cleared", v)
	}
	// A filled-in value must survive untouched.
	if v := os.Getenv("BITBUCKET_URL"); v != "https://bb.example.com" {
		t.Errorf("BITBUCKET_URL = %q, want it left alone", v)
	}
	if v := os.Getenv("BITBUCKET_ACCESS_TOKEN"); v != "real-token" {
		t.Errorf("BITBUCKET_ACCESS_TOKEN = %q, want it left alone", v)
	}
}

func TestPlaceholderMatching(t *testing.T) {
	for _, s := range []string{"${user_config.jira_url}", "${user_config.repo_root}", "${__dirname}"} {
		if !mcpbPlaceholderRe.MatchString(s) {
			t.Errorf("%q should be recognized as an unfilled placeholder", s)
		}
	}
	// Real values that merely resemble one must not be blanked.
	for _, s := range []string{
		"https://jira.example.com", "", "token-${weird", "$notaplaceholder",
		"https://jira.example.com/${x}", "${a b}",
	} {
		if mcpbPlaceholderRe.MatchString(s) {
			t.Errorf("%q must not be treated as a placeholder", s)
		}
	}
}

func TestInvalidServiceURLRejected(t *testing.T) {
	cases := map[string]bool{
		"https://jira.example.com": true,
		"http://localhost:8080":    true,
		"":                         true, // not configured, not an error
		"jira.example.com":         false,
		"${user_config.jira_url}":  false,
		"ftp://jira.example.com":   false,
		"https://":                 false,
	}
	for raw, ok := range cases {
		if got := invalidServiceURL(raw) == ""; got != ok {
			t.Errorf("invalidServiceURL(%q) usable = %v, want %v (%s)", raw, got, ok, invalidServiceURL(raw))
		}
	}
}

// TestBlankExtensionFieldsFallBackToConfigFile is the whole Claude Desktop
// story end to end: install the extension, leave the URL/token fields blank,
// and the server must pick up ~/.atlassian-mcp.json — which is what the install
// dialog promises. Before placeholders were cleared, the literal
// "${user_config.jira_url}" counted as "set" and shadowed the file.
func TestBlankExtensionFieldsFallBackToConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "atlassian-mcp.json")
	cfg := `{
	  "jira":      { "url": "https://jira.example.com", "token": "jira-secret" },
	  "bitbucket": { "url": "https://bb.example.com",   "token": "bb-secret"   }
	}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// Exactly what Claude Desktop passes for an all-blank install dialog.
	t.Setenv("ATLASSIAN_MCP_CONFIG", cfgPath)
	t.Setenv("JIRA_URL", "${user_config.jira_url}")
	t.Setenv("JIRA_ACCESS_TOKEN", "${user_config.jira_token}")
	t.Setenv("BITBUCKET_URL", "${user_config.bitbucket_url}")
	t.Setenv("BITBUCKET_ACCESS_TOKEN", "${user_config.bitbucket_token}")

	got := Load()
	if got.Jira == nil {
		t.Fatal("Jira not configured — the config file was not picked up")
	}
	if got.Jira.URL != "https://jira.example.com" || got.Jira.Token != "jira-secret" {
		t.Errorf("Jira = %+v, want the config file's values", *got.Jira)
	}
	if got.Bitbucket == nil {
		t.Fatal("Bitbucket not configured")
	}
	if got.Bitbucket.URL != "https://bb.example.com" || got.Bitbucket.Token != "bb-secret" {
		t.Errorf("Bitbucket = %+v, want the config file's values", *got.Bitbucket)
	}
}

// A filled-in dialog field still wins over the config file for that field,
// while the others keep falling back.
func TestFilledExtensionFieldOverridesConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "atlassian-mcp.json")
	if err := os.WriteFile(cfgPath, []byte(`{"jira":{"url":"https://from-file.example.com","token":"file-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATLASSIAN_MCP_CONFIG", cfgPath)
	t.Setenv("JIRA_URL", "${user_config.jira_url}")
	t.Setenv("JIRA_ACCESS_TOKEN", "${user_config.jira_token}")
	t.Setenv("BITBUCKET_URL", "https://typed-in.example.com")
	t.Setenv("BITBUCKET_ACCESS_TOKEN", "typed-token")

	got := Load()
	if got.Jira == nil || got.Jira.URL != "https://from-file.example.com" {
		t.Errorf("Jira should come from the file, got %+v", got.Jira)
	}
	if got.Bitbucket == nil || got.Bitbucket.URL != "https://typed-in.example.com" {
		t.Errorf("Bitbucket should come from the dialog, got %+v", got.Bitbucket)
	}
}

func TestCredentialsTokenForMatchesOrigin(t *testing.T) {
	creds := Config{
		Jira:      &ServiceConfig{URL: "https://jira.example.com/jira", Token: "j"},
		Bitbucket: &ServiceConfig{URL: "https://git.example.com:7990", Token: "b"},
	}.Credentials()
	for raw, want := range map[string]string{
		"https://jira.example.com/secure/attachment/1/a.png": "j",
		"https://JIRA.example.com:443/x":                     "j",
		"http://jira.example.com/x":                          "",
		"https://jira.example.com:8443/x":                    "",
		"https://git.example.com:7990/scm/eng/api.git":       "b",
		"https://git.example.com/scm/eng/api.git":            "",
		"https://evil.example.com/x":                         "",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := creds.TokenFor(u); got != want {
			t.Errorf("TokenFor(%s) = %q, want %q", raw, got, want)
		}
	}
}
