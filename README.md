# atlassian-mcp

A [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server for **self-hosted Jira** (Server / Data Center) and **self-hosted Bitbucket** (Server / Data Center). Exposes 15 tools to Claude for reading and managing issues, pull requests, comments, and git context.

> **Note:** This server only supports self-hosted instances. Jira Cloud and Bitbucket Cloud use different APIs and are not supported.

---

## Tools

### Jira

| Tool | Description |
|---|---|
| `jira_search_issues` | Search issues using JQL |
| `jira_my_issues` | List issues assigned to you, ordered by last updated |
| `jira_get_projects` | List all accessible projects |
| `jira_get_issue` | Get issue details by key |
| `jira_create_issue` | Create a new issue |
| `jira_update_issue` | Update summary, description, assignee, or priority |
| `jira_assign_issue` | Assign or unassign an issue |
| `jira_get_comments` | List comments on an issue |
| `jira_add_comment` | Add a comment to an issue |
| `jira_get_transitions` | List available status transitions |
| `jira_transition_issue` | Change issue status via transition ID |

### Bitbucket

| Tool | Description |
|---|---|
| `bitbucket_list_repos` | List repositories (optionally by project) |
| `bitbucket_list_pull_requests` | List pull requests for a repository |
| `bitbucket_my_prs` | List PRs in your inbox (authored by you or awaiting review) |
| `bitbucket_get_pull_request` | Get pull request details |
| `bitbucket_get_pr_diff` | Get the code diff for a pull request |
| `bitbucket_create_pull_request` | Create a new pull request |
| `bitbucket_approve_pr` | Approve a pull request |
| `bitbucket_merge_pr` | Merge a pull request |
| `bitbucket_get_pr_comments` | Get comments on a pull request |
| `bitbucket_add_pr_comment` | Add a comment to a pull request |
| `bitbucket_get_branches` | List branches in a repository |
| `bitbucket_get_file` | Get raw file content at a given ref |

### Git

| Tool | Description |
|---|---|
| `git_get_context` | Branch, remote, recent commits, working tree status, and any Jira keys detected in the branch name |
| `git_get_commits` | Commit history for a branch with author and message |
| `git_get_diff` | Diff of uncommitted changes or between two refs |

All list tools support `limit` and `start`/`startAt` for pagination.

---

## Setup

### 1. Create a config file

Create `~/.atlassian-mcp.json`:

```json
{
  "$schema": "https://raw.githubusercontent.com/stubbedev/atlassian-mcp/master/atlassian-mcp.schema.json",
  "jira": {
    "url": "https://jira.example.com",
    "token": "your-jira-personal-access-token"
  },
  "bitbucket": {
    "url": "https://bitbucket.example.com",
    "token": "your-bitbucket-personal-access-token"
  }
}
```

The `$schema` field is optional but enables editor autocomplete and validation.

Alternatively, use environment variables (or a `.env` file in this directory):

```env
JIRA_URL=https://jira.example.com
JIRA_ACCESS_TOKEN=your-jira-personal-access-token
BITBUCKET_URL=https://bitbucket.example.com
BITBUCKET_ACCESS_TOKEN=your-bitbucket-personal-access-token
```

Config is resolved in this order: `--config <path>` CLI arg → `ATLASSIAN_MCP_CONFIG` env var → `~/.atlassian-mcp.json` → `.atlassian-mcp.json` in cwd → environment variables.

### 2. Connect to your AI tool

No cloning or building required — just point your tool at `npx github:stubbedev/atlassian-mcp` and it will install and run automatically.

---

#### Claude Code

```bash
claude mcp add atlassian -- npx -y github:stubbedev/atlassian-mcp --config ~/.atlassian-mcp.json
```

---

#### Cursor

Add to `~/.cursor/mcp.json` (global) or `.cursor/mcp.json` (project-only):

```json
{
  "mcpServers": {
    "atlassian": {
      "command": "npx",
      "args": ["-y", "github:stubbedev/atlassian-mcp", "--config", "/Users/you/.atlassian-mcp.json"]
    }
  }
}
```

---

#### Windsurf

Add to `~/.codeium/windsurf/mcp_config.json`:

```json
{
  "mcpServers": {
    "atlassian": {
      "command": "npx",
      "args": ["-y", "github:stubbedev/atlassian-mcp", "--config", "/Users/you/.atlassian-mcp.json"]
    }
  }
}
```

---

#### Zed

Add to `~/.config/zed/settings.json`:

```json
{
  "context_servers": {
    "atlassian": {
      "command": {
        "path": "npx",
        "args": ["-y", "github:stubbedev/atlassian-mcp", "--config", "/home/you/.atlassian-mcp.json"]
      }
    }
  }
}
```

---

#### OpenCode

Add to `opencode.json` in your project root (or `~/.config/opencode/opencode.json` for global):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "atlassian": {
      "type": "local",
      "command": ["npx", "-y", "github:stubbedev/atlassian-mcp", "--config", "/home/you/.atlassian-mcp.json"]
    }
  }
}
```

---

#### Codex CLI

Add to `~/.codex/config.yaml`:

```yaml
mcpServers:
  atlassian:
    command: npx
    args:
      - -y
      - github:stubbedev/atlassian-mcp
      - --config
      - /home/you/.atlassian-mcp.json
```

---

#### Any other MCP-compatible tool

Most tools that support MCP accept the same JSON format. Use `npx` as the command with `["-y", "github:stubbedev/atlassian-mcp", "--config", "/path/to/config.json"]` as the args.

---

### Manual install (optional)

If you prefer to clone and run locally:

```bash
git clone git@github.com:stubbedev/atlassian-mcp.git
cd atlassian-mcp
npm install
```

Then use `node /path/to/atlassian-mcp/dist/index.js` instead of the `npx` command in the configs above.

---

## Creating Personal Access Tokens

### Jira Server / Data Center

Personal Access Tokens are supported from **Jira 8.14** onwards.

1. Log in to your Jira instance.
2. Click your profile avatar in the top-right corner and select **Profile**.
3. In the left sidebar, click **Personal Access Tokens**.
4. Click **Create token**.
5. Give the token a name (e.g. `atlassian-mcp`) and optionally set an expiry date.
6. Click **Create** and copy the token — it will only be shown once.

Paste the token as the `token` value under `jira` in your config file.

> If your Jira version is older than 8.14, you can use HTTP Basic Auth instead — but this server only supports Bearer token (PAT) authentication.

### Bitbucket Server / Data Center

Personal Access Tokens are supported from **Bitbucket Server 5.5** onwards.

1. Log in to your Bitbucket instance.
2. Click your profile avatar in the top-right corner and select **Manage account**.
3. In the left sidebar, under **Security**, click **Personal access tokens**.
4. Click **Create a token**.
5. Give the token a name (e.g. `atlassian-mcp`).
6. Set the permissions:
   - **Projects**: Read
   - **Repositories**: Read + Write (Write is needed to create pull requests and add comments)
7. Optionally set an expiry date.
8. Click **Create** and copy the token — it will only be shown once.

Paste the token as the `token` value under `bitbucket` in your config file.

---

## Development

```bash
# Watch mode — recompiles on file changes
npm run dev

# Run the built server directly
node dist/index.js

# Test the tool list
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' | node dist/index.js
```

To use a specific config file:

```bash
node dist/index.js --config /path/to/config.json
```
