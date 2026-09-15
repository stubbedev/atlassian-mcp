package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ServiceConfig holds a resolved Jira or Bitbucket connection.
type ServiceConfig struct {
	URL   string
	Token string
}

// Config is the top-level resolved configuration. A nil field means that
// service is not configured (or, for Bitbucket, disabled for this cwd).
type Config struct {
	Jira       *ServiceConfig
	Bitbucket  *ServiceConfig
	MarkAIText bool
}

type configFile struct {
	MarkAIText *bool `json:"markAIText"`
	Jira       struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	} `json:"jira"`
	Bitbucket struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	} `json:"bitbucket"`
}

// readJSONFile loads the config file, logging why it did not when it did not.
// The reason matters on hosts that confine the server: "permission denied" on a
// file that is plainly there is the clearest evidence of a filesystem sandbox,
// and stderr is the only channel a GUI client gives us to say so.
func readJSONFile(path string) *configFile {
	raw, err := os.ReadFile(path)
	if err != nil {
		logf("Config file %s could not be read: %v", path, err)
		return nil
	}
	var cf configFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		logf("Config file %s is not valid JSON: %v", path, err)
		return nil
	}
	logf("Config file: %s", path)
	return &cf
}

// getConfigPath resolves the config file location in priority order:
// --config <path> → ATLASSIAN_MCP_CONFIG → ~/.atlassian-mcp.json → ./.atlassian-mcp.json
// A leading ~ in the explicit paths is expanded (see expandHome).
func getConfigPath() string {
	args := os.Args[1:]
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			p, _ := filepath.Abs(expandHome(args[i+1]))
			return p
		}
		if after, ok := strings.CutPrefix(a, "--config="); ok {
			p, _ := filepath.Abs(expandHome(after))
			return p
		}
	}
	if env := os.Getenv("ATLASSIAN_MCP_CONFIG"); env != "" {
		p, _ := filepath.Abs(expandHome(env))
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		homeConfig := filepath.Join(home, ".atlassian-mcp.json")
		if fileExists(homeConfig) {
			return homeConfig
		}
	}
	// XDG location: $XDG_CONFIG_HOME/atlassian-mcp/config.json (default ~/.config).
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		if home, err := os.UserHomeDir(); err == nil {
			xdg = filepath.Join(home, ".config")
		}
	}
	if xdg != "" {
		xdgConfig := filepath.Join(xdg, "atlassian-mcp", "config.json")
		if fileExists(xdgConfig) {
			return xdgConfig
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		cwdConfig := filepath.Join(cwd, ".atlassian-mcp.json")
		if fileExists(cwdConfig) {
			return cwdConfig
		}
	}
	return ""
}

// expandHome expands a leading ~ in a path. GUI desktop clients spawn the
// server without a shell, so a ~ in a config path or repo root arrives here
// literally instead of already expanded.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimLeft(p[1:], `/\`))
		}
	}
	return p
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// loadDotEnv loads KEY=VALUE pairs from a .env file in cwd into the process
// environment without overwriting variables that are already set. Mirrors the
// dotenv behavior of the previous TypeScript implementation.
func loadDotEnv() {
	raw, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

// mcpbPlaceholderRe matches an MCP Bundle manifest substitution that the host
// never filled in, e.g. "${user_config.jira_url}".
var mcpbPlaceholderRe = regexp.MustCompile(`^\$\{[A-Za-z0-9_.]+\}$`)

// manifestEnvVars are the variables the .mcpb manifest wires to user_config
// fields. Claude Desktop substitutes a blank field with the literal
// "${user_config.x}" rather than an empty string, so anything reading these
// raw would take a placeholder for a real value — a blank Jira URL became
// POST "${user_config.jira_url}/rest/api/2/..." and failed with "unsupported
// protocol scheme". Blank is supposed to mean "fall back to the config file",
// so clear them before any of it is read.
var manifestEnvVars = []string{
	"JIRA_URL", "JIRA_ACCESS_TOKEN",
	"BITBUCKET_URL", "BITBUCKET_ACCESS_TOKEN",
	"ATLASSIAN_MCP_REPO_ROOT", "ATLASSIAN_MCP_GIT_PATH", "ATLASSIAN_MCP_MARK_AI_TEXT",
	"ATLASSIAN_MCP_CONFIG", "ATLASSIAN_MCP_HTTP_TOKEN",
	"ATLASSIAN_MCP_FFMPEG_PATH", "ATLASSIAN_MCP_FFPROBE_PATH",
}

// clearUnsubstitutedEnv unsets manifest variables still holding a placeholder.
func clearUnsubstitutedEnv() {
	var cleared []string
	for _, k := range manifestEnvVars {
		if mcpbPlaceholderRe.MatchString(strings.TrimSpace(os.Getenv(k))) {
			_ = os.Unsetenv(k)
			cleared = append(cleared, k)
		}
	}
	if len(cleared) > 0 {
		logf("Ignoring unfilled extension settings (%s) — treating them as blank. Fill them in the extension's settings, or leave them blank to use a config file.", strings.Join(cleared, ", "))
	}
}

// invalidServiceURL describes what is wrong with a configured service URL, or
// "" when it is usable. An empty URL is not an error here — it means the
// service is simply not configured.
func invalidServiceURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Sprintf("%q is not a valid URL: %v", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Sprintf("%q needs an http:// or https:// prefix", raw)
	}
	if u.Host == "" {
		return fmt.Sprintf("%q has no host", raw)
	}
	return ""
}

// loadConfig resolves configuration from the config file then environment
// variables. A service is enabled only when both url and token are present;
// a partial configuration logs which piece is missing, matching config.ts.
func loadConfig() Config {
	clearUnsubstitutedEnv()
	loadDotEnv()

	var file *configFile
	if path := getConfigPath(); path != "" {
		file = readJSONFile(path)
	} else {
		logf("No config file found (looked for --config/ATLASSIAN_MCP_CONFIG, ~/.atlassian-mcp.json, $XDG_CONFIG_HOME/atlassian-mcp/config.json, ./.atlassian-mcp.json); using environment variables only.")
	}

	pick := func(fileVal, env string) string {
		if fileVal != "" {
			return fileVal
		}
		return os.Getenv(env)
	}

	var jiraURL, jiraToken, bbURL, bbToken string
	if file != nil {
		jiraURL = pick(file.Jira.URL, "JIRA_URL")
		jiraToken = pick(file.Jira.Token, "JIRA_ACCESS_TOKEN")
		bbURL = pick(file.Bitbucket.URL, "BITBUCKET_URL")
		bbToken = pick(file.Bitbucket.Token, "BITBUCKET_ACCESS_TOKEN")
	} else {
		jiraURL = os.Getenv("JIRA_URL")
		jiraToken = os.Getenv("JIRA_ACCESS_TOKEN")
		bbURL = os.Getenv("BITBUCKET_URL")
		bbToken = os.Getenv("BITBUCKET_ACCESS_TOKEN")
	}

	cfg := Config{}

	// A URL that is not absolute http(s) would otherwise surface much later as
	// net/http's "unsupported protocol scheme", naming the request rather than
	// the setting that was wrong.
	if bad := invalidServiceURL(jiraURL); bad != "" {
		logf("Jira disabled: %s", bad)
		jiraURL = ""
	}
	if bad := invalidServiceURL(bbURL); bad != "" {
		logf("Bitbucket disabled: %s", bad)
		bbURL = ""
	}

	if jiraURL != "" && jiraToken != "" {
		cfg.Jira = &ServiceConfig{URL: strings.TrimRight(jiraURL, "/"), Token: jiraToken}
	} else if jiraURL != "" || jiraToken != "" {
		var missing []string
		if jiraURL == "" {
			missing = append(missing, "jira.url (or JIRA_URL)")
		}
		if jiraToken == "" {
			missing = append(missing, "jira.token (or JIRA_ACCESS_TOKEN)")
		}
		logf("Jira disabled: missing %s", strings.Join(missing, ", "))
	}

	if bbURL != "" && bbToken != "" {
		cfg.Bitbucket = &ServiceConfig{URL: strings.TrimRight(bbURL, "/"), Token: bbToken}
	} else if bbURL != "" || bbToken != "" {
		var missing []string
		if bbURL == "" {
			missing = append(missing, "bitbucket.url (or BITBUCKET_URL)")
		}
		if bbToken == "" {
			missing = append(missing, "bitbucket.token (or BITBUCKET_ACCESS_TOKEN)")
		}
		logf("Bitbucket disabled: missing %s", strings.Join(missing, ", "))
	}

	// AI text marking defaults to on; an explicit value in the config file
	// wins, then the env var. Pointer so "markAIText": false is honored.
	cfg.MarkAIText = true
	if file != nil && file.MarkAIText != nil {
		cfg.MarkAIText = *file.MarkAIText
	} else if env := strings.TrimSpace(os.Getenv("ATLASSIAN_MCP_MARK_AI_TEXT")); env != "" {
		if b, err := strconv.ParseBool(env); err == nil {
			cfg.MarkAIText = b
		} else {
			logf("ATLASSIAN_MCP_MARK_AI_TEXT=%q is not a boolean — leaving AI text marking enabled", env)
		}
	}

	return cfg
}
