# atlassian-mcp

A [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server for **self-hosted Jira** (Server / Data Center) and **self-hosted Bitbucket** (Server / Data Center). Exposes tools for natural-language workflows around tickets, pull requests, review threads, and git context.

> **Note:** This server only supports self-hosted instances. Jira Cloud and Bitbucket Cloud use different APIs and are not supported.

---

## Tools

### Workflow

| Tool | Description |
|---|---|
| `get_dev_context` | Master entry point: git state + linked Jira ticket + open PR with reviewer/blocker status and next-step hints |
| `start_work` | Start a Jira ticket: fetches it, creates a local branch (`feature/FOO-123-slug`), and optionally transitions the ticket |
| `complete_work` | Close out finished work: merges the open PR and transitions the Jira ticket to Done |

### Git

| Tool | Description |
|---|---|
| `git_get_context` | Branch, upstream state, remote URL, recent commits, working tree status, diff stat, and Jira keys in branch name |
| `git_get_diff` | Diff of uncommitted changes or between two refs; supports paging via `charOffset` |

### Jira

| Tool | Description |
|---|---|
| `jira_search` | Discover resources: `issues`, `projects`, `issue_types`, `boards`, `sprints`, `board_overview`, or `users` via `resource` param |
| `jira_get` | Full details for one issue: summary, description, status, sprint, transitions, comments, and attachment list |
| `jira_get_attachment` | Fetch a Jira attachment by ID; images are auto-resized via sharp and returned inline so the model can see them, text/JSON inline, larger/binary files via `saveTo` |
| `jira_mutate` | Create, update, transition, comment, link, add to sprint, or log work — all in one call |
| `jira_comment` | Add, update, or delete a comment on an issue (`action`: `add` / `update` / `delete`) |

### Bitbucket

| Tool | Description |
|---|---|
| `bitbucket_search` | Discover resources: `pull_requests` (default), `repos`, or `branches` via `resource` param; `mine=true` for your inbox |
| `bitbucket_get_pr` | Full PR details: metadata, commits, comments, blockers, build status, optional diff, and any attachments referenced from the description or comments |
| `bitbucket_get_attachment` | Fetch a repo attachment by ID (images auto-resized inline via sharp; text inline; binary/large via `saveTo`) |
| `bitbucket_mutate` | Create/update a PR, or perform lifecycle actions: `approve`, `unapprove`, `merge`, `decline` |
| `bitbucket_comment` | Add, update, or delete a PR comment; for code changes use `suggestion` so Bitbucket shows Apply suggestion (no trailing text after a suggestion block) |
| `bitbucket_get_file` | Raw file content from Bitbucket at a branch, tag, or commit |
| `bitbucket_pr_tasks` | Manage PR tasks (checklist items): `list`, `create`, `resolve`, `reopen`, `delete` |

### Natural language examples

- "what am I working on?" → `get_dev_context`
- "make a branch for FOO-123" → `start_work`
- "ship this / merge and close the ticket" → `complete_work`
- "show my PRs waiting for review" → `bitbucket_search` with `mine=true`
- "list open PRs for this repo from feature/ABC-123" → `bitbucket_search` with `fromBranch`
- "give me a full overview of PR 42" → `bitbucket_get_pr`
- "open a PR from my current branch to master" → `bitbucket_mutate` with `create`
- "approve / merge / decline PR 42" → `bitbucket_mutate` with `action`
- "reply to comment 123 on PR 42" → `bitbucket_comment` with `commentId=123`
- "resolve this blocker on PR 42" → `bitbucket_comment` with `action=update`, `severity=BLOCKER`, `state=RESOLVED`
- "list PR checklist tasks" → `bitbucket_pr_tasks` with `action=list`
- "find bugs assigned to me in PAY project" → `jira_search` with `mine=true`, `issueType=Bug`
- "what's in the current sprint?" → `jira_search` with `resource=board_overview`
- "move FOO-123 to In Progress" → `jira_mutate` with `transitionName="In Progress"`
- "log 2h on FOO-123" → `jira_mutate` with `worklog`

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
- You can also use ergonomic aliases:
  - Jira: `project` (alias of `projectKey`)
  - Bitbucket: `project` and `repo` (aliases of `projectKey` and `repoSlug`)
- For Bitbucket tools, `projectKey` and `repoSlug` are usually auto-detected from your local `origin` remote.
- `bitbucket_create_pull_request` also auto-detects `fromBranch` from your current branch and returns the existing open PR if one already exists for that branch.
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

### Native dependency: `sharp`

Image attachments are downscaled and re-encoded with [`sharp`](https://sharp.pixelplumbing.com/) before being returned to the model so they fit in context. Sharp ships prebuilt binaries for glibc Linux (x64/arm64), macOS, and Windows — no extra setup needed on those. Alpine / musl users may need `npm install --cpu=x64 --os=linux --libc=musl sharp`.

---

## Releases (Maintainers)

This package is published to npm as `@stubbedev/atlassian-mcp`.

Use semantic versioning for releases. Breaking tool-surface changes should bump the minor version while `<1.0.0` (for example `0.0.x` -> `0.1.0`).

Automatic publish is configured in `.github/workflows/publish.yml` and runs when a new version tag is pushed.

Release flow:

```bash
# choose one: patch | minor | major
increment=patch

# bumps package.json + package-lock.json,
# creates a version commit, and creates a git tag (for example v0.1.17)
npm version "$increment"

# push commit and tag to GitHub
git push origin HEAD --follow-tags
```

GitHub Actions will publish the npm release from that pushed tag.

- The workflow is configured for npm Trusted Publisher (OIDC), so no `NPM_TOKEN` secret is required

Required npm setup (one-time):

- In npm package settings, add this GitHub repo/workflow as a Trusted Publisher

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

# Quick release smoke check
npm run smoke
```

To use a specific config file:

```bash
node dist/index.js --config /path/to/config.json
```
