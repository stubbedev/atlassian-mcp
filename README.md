# atlassian-mcp

A [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server for **self-hosted Jira** (Server / Data Center) and **self-hosted Bitbucket** (Server / Data Center). Exposes 32 tools for natural-language workflows around tickets, pull requests, review threads, and git context.

> **Note:** This server only supports self-hosted instances. Jira Cloud and Bitbucket Cloud use different APIs and are not supported.

---

## Tools

### Context

| Tool | Description |
|---|---|
| `get_dev_context` | One-shot coding context: local git state + linked Jira tickets + open PR for current branch |

### Jira

| Tool | Description |
|---|---|
| `jira_search_issues` | Find tickets by plain language, JQL, project, status, assignee, or type |
| `jira_my_issues` | List issues assigned to you, ordered by last updated |
| `jira_get_projects` | List all accessible projects |
| `jira_get_issue_types` | List issue types and their available statuses for a project |
| `jira_get_issue` | Get issue details by key |
| `jira_create_issue` | Create a new issue |
| `jira_update_issue` | Update summary, description, assignee, or priority |
| `jira_search_users` | Search for users by name or email |
| `jira_get_comments` | List comments on an issue |
| `jira_add_comment` | Add a comment to an issue |
| `jira_transition_issue` | Move issue status via transition name or transition ID |

### Bitbucket

| Tool | Description |
|---|---|
| `bitbucket_list_repos` | List repositories (optionally by project) |
| `bitbucket_list_pull_requests` | List repository pull requests (filter by state, source branch, or text) |
| `bitbucket_my_prs` | List PRs in your inbox (authored by you or awaiting review) |
| `bitbucket_get_pull_request` | Get pull request details |
| `bitbucket_get_pr_diff` | Get the code diff for a pull request |
| `bitbucket_create_pull_request` | Create a new pull request |
| `bitbucket_approve_pr` | Approve a pull request |
| `bitbucket_unapprove_pr` | Remove your approval from a pull request |
| `bitbucket_merge_pr` | Merge a pull request |
| `bitbucket_decline_pr` | Decline a pull request |
| `bitbucket_get_pr_comments` | Get PR comment threads in bulk, including blocker comments and blocker counts |
| `bitbucket_add_pr_comment` | Add a top-level PR comment or reply to an existing comment |
| `bitbucket_update_pr_comment` | Update comment text, state, or severity (`NORMAL` / `BLOCKER`) |
| `bitbucket_delete_pr_comment` | Delete a PR comment by comment ID |
| `bitbucket_get_pr_commits` | List commits included in a pull request |
| `bitbucket_get_branches` | List branches in a repository |
| `bitbucket_get_file` | Get raw file content at a given ref |

### Git

| Tool | Description |
|---|---|
| `git_get_context` | Branch, remote, recent commits, working tree status, and any Jira keys detected in the branch name |
| `git_get_commits` | Commit history for a branch with author and message |
| `git_get_diff` | Diff of uncommitted changes or between two refs |

All list tools support `limit` and `start`/`startAt` for pagination.

### Natural language examples

- "show my PRs waiting for review" → `bitbucket_my_prs`
- "list open PRs for this repo from branch feature/ABC-123" → `bitbucket_list_pull_requests`
- "open a PR from my current branch to master" → `bitbucket_create_pull_request`
- "show review comments on PR 42" → `bitbucket_get_pr_comments`
- "how many open blockers are on PR 42" → `bitbucket_get_pr_comments` with `severity=BLOCKER` and `countOnly=true`
- "move FOO-123 to In Progress" → `jira_transition_issue` with `transitionName="In Progress"`
- "find bugs assigned to me in PAY project" → `jira_search_issues`
- "give me my coding context for this branch" → `get_dev_context`

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

- `projectKey` means a project code:
  - Jira example: `PAY` in ticket `PAY-123`
  - Bitbucket example: project `ENG` in repo path `ENG/payments-service`
- For Bitbucket tools, `projectKey` and `repoSlug` are usually auto-detected from your local `origin` remote.
- `bitbucket_create_pull_request` also auto-detects `fromBranch` from your current branch.
- Jira project-scoped calls accept `projectKey` and work best when provided.
- If `projectKey` is omitted for Jira issue creation/type lookup, the server tries to infer it from your current branch ticket key, falls back to auto-select when only one project is visible, and otherwise returns a numbered project list to pick from.

Alternatively, use environment variables (or a `.env` file in this directory):

```env
JIRA_URL=https://jira.example.com
JIRA_ACCESS_TOKEN=your-jira-personal-access-token
BITBUCKET_URL=https://bitbucket.example.com
BITBUCKET_ACCESS_TOKEN=your-bitbucket-personal-access-token
```

Config is resolved in this order: `--config <path>` CLI arg → `ATLASSIAN_MCP_CONFIG` env var → `~/.atlassian-mcp.json` → `.atlassian-mcp.json` in cwd → environment variables.

### 2. Connect to your AI tool

No cloning or building required — just point your tool at `npx @stubbedev/atlassian-mcp@latest` and it will install and run automatically.

> Note: `--prefer-online` can break MCP startup in some clients. Keep the command simple and use the update steps below when you want to refresh.

---

#### Claude Code

```bash
claude mcp add atlassian -- npx -y @stubbedev/atlassian-mcp@latest --config ~/.atlassian-mcp.json
```

---

#### Cursor

Add to `~/.cursor/mcp.json` (global) or `.cursor/mcp.json` (project-only):

```json
{
  "mcpServers": {
    "atlassian": {
      "command": "npx",
      "args": ["-y", "@stubbedev/atlassian-mcp@latest", "--config", "/Users/you/.atlassian-mcp.json"]
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
      "args": ["-y", "@stubbedev/atlassian-mcp@latest", "--config", "/Users/you/.atlassian-mcp.json"]
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
        "args": ["-y", "@stubbedev/atlassian-mcp@latest", "--config", "/home/you/.atlassian-mcp.json"]
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
      "command": ["npx", "-y", "@stubbedev/atlassian-mcp@latest", "--config", "/home/you/.atlassian-mcp.json"]
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
      - @stubbedev/atlassian-mcp@latest
      - --config
      - /home/you/.atlassian-mcp.json
```

---

#### Any other MCP-compatible tool

Most tools that support MCP accept the same JSON format. Use `npx` as the command with `["-y", "@stubbedev/atlassian-mcp@latest", "--config", "/path/to/config.json"]` as the args.

### Updating existing installs

If your MCP client is already configured and you want the newest package version:

```bash
npx clear-npx-cache
```

Then restart your MCP client.

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

## Releases (Maintainers)

This package is published to npm as `@stubbedev/atlassian-mcp`.

Automatic publish is configured in `.github/workflows/publish.yml`:

- Push a tag like `v1.0.1` to publish from CI
- Or run the workflow manually via **Actions → Publish Package**

- The workflow is configured for npm Trusted Publisher (OIDC), so no `NPM_TOKEN` secret is required

Required npm setup (one-time):

- In npm package settings, add this GitHub repo/workflow as a Trusted Publisher

Manual publish from local machine:

```bash
npm run build
npm publish --access public
```

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
