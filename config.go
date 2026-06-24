package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	Jira      *ServiceConfig
	Bitbucket *ServiceConfig
}

type configFile struct {
	Jira struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	} `json:"jira"`
	Bitbucket struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	} `json:"bitbucket"`
}

func readJSONFile(path string) *configFile {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cf configFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		return nil
	}
	return &cf
}

// getConfigPath resolves the config file location in priority order:
// --config <path> → ATLASSIAN_MCP_CONFIG → ~/.atlassian-mcp.json → ./.atlassian-mcp.json
func getConfigPath() string {
	args := os.Args[1:]
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			p, _ := filepath.Abs(args[i+1])
			return p
		}
		if strings.HasPrefix(a, "--config=") {
			p, _ := filepath.Abs(strings.TrimPrefix(a, "--config="))
			return p
		}
	}
	if env := os.Getenv("ATLASSIAN_MCP_CONFIG"); env != "" {
		p, _ := filepath.Abs(env)
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

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// loadDotEnv loads KEY=VALUE pairs from a .env file in cwd into the process
// environment without overwriting variables that are already set. Mirrors the
// dotenv behaviour of the previous TypeScript implementation.
func loadDotEnv() {
	raw, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
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

// loadConfig resolves configuration from the config file then environment
// variables. A service is enabled only when both url and token are present;
// a partial configuration logs which piece is missing, matching config.ts.
func loadConfig() Config {
	loadDotEnv()

	var file *configFile
	if path := getConfigPath(); path != "" {
		file = readJSONFile(path)
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

	return cfg
}
