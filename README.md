<p align="center"><img src="assets/logo.svg" alt="" width="128"></p>

# atlassian-mcp

An [MCP](https://modelcontextprotocol.io) server for **self-hosted Jira and Bitbucket**
(Server / Data Center): tickets, sprints, pull requests, review threads and local git
context, in one static binary with no runtime dependencies.

> Jira Cloud and Bitbucket Cloud use different APIs and are not supported.

## Tools

| Tool | What it does |
|---|---|
| `get_dev_context` | Start here: git state, the linked Jira ticket and the open PR with approvals, blockers and next steps. Also readable as the resource `dev-context://current` |
| `start_work` | Resolve a ticket (key or free text), branch off the default branch, load the repo README, optionally transition the ticket |
| `complete_work` | Merge the PR and move the ticket to Done; refuses while approvals are missing or a build failed (`force=true` overrides) |
| `git_get_context` | Branch, upstream, recent commits, status, diff stat; or a paged diff between refs |
| `jira_search` | `issues`, `projects`, `issue_types`, `boards`, `sprints`, `board_overview`, `field_history`, `versions`, `components`, `fields`, `users` |
| `jira_get` | One issue in full: fields, sprint, transitions, comments, attachments; `customFields` for any field, `includeChangelog` for its history |
| `jira_mutate` | Create, update, transition, comment, link, log work, add to sprint, set custom fields, attach files, delete, manage fix versions; several in one call |
| `bitbucket_search` | `pull_requests` (default), `repos`, `branches`, `users`; `mine=true` for your inbox |
| `bitbucket_get_pr` | PR metadata, commits, comments, blockers, builds, optional diff, attachments |
| `bitbucket_mutate` | Create or update a PR; `approve`, `unapprove`, `needs_work`, `merge`, `decline` |
| `bitbucket_comment` | Add, edit or delete PR comments, including suggestions and draft-review comments |
| `bitbucket_get_file` | A file at a branch, tag, commit or PR source branch, paged |
| `bitbucket_pr_tasks` | List, create, resolve, reopen or delete PR tasks |
| `get_attachment` | A Jira or Bitbucket attachment, decoded so the model can see it: images, video and animation frames, audio, PDFs, text |
| `attach_files` | An in-chat upload panel for files the server cannot read itself, e.g. a pasted screenshot (hosts with MCP Apps) |

Things to ask for:

- "what am I working on?" · "make a branch for FOO-123" · "ship this"
- "what's in the current sprint?" · "which estimates changed since the sprint started?"
- "show my PRs waiting for review" · "approve PR 42" · "reply to comment 123 on PR 42"
- "move FOO-123 to In Progress" · "log 2h on FOO-123" · "set story points to 5"
- "what can I set on an Epic in PAY?" · "create version 9.1.0 in PAY and release it"

## Guarantees

Enforced in code, not left to the model:

- Arguments are checked against each tool's schema; an unknown `action` or `resource` is an error, never a fallback.
- Assignees, components, fix versions and reviewers are resolved before anything is written; a bad one comes back with the valid choices.
- Markdown is converted to Jira wiki markup on every Jira write.
- PR comments: one reply per thread, no duplicates, no new top-level comment on your own PR (`asAuthor=true` overrides), `#123` becomes a link.
- Inline comments stay anchored to the commit you reviewed, remapped if the branch moved.
- Reviewers are only dropped with `update.replaceReviewers=true`.
- Truncated output always names the argument that fetches the rest.
- Text the agent writes ends with a bold `[AI]` line, unless the user dictated it (`commentUserAuthored` / `userAuthored`) or marking is off (`"markAIText": false`).

## Setup

### 1. Configure

Create `~/.atlassian-mcp.json` with a [personal access token](#personal-access-tokens) for each service:

```json
{
  "$schema": "https://raw.githubusercontent.com/stubbedev/atlassian-mcp/master/atlassian-mcp.schema.json",
  "jira": { "url": "https://jira.example.com", "token": "…" },
  "bitbucket": { "url": "https://bitbucket.example.com", "token": "…" }
}
```

Or set `JIRA_URL`, `JIRA_ACCESS_TOKEN`, `BITBUCKET_URL`, `BITBUCKET_ACCESS_TOKEN`. The
config is looked up at `--config <path>`, `$ATLASSIAN_MCP_CONFIG`, `~/.atlassian-mcp.json`,
`$XDG_CONFIG_HOME/atlassian-mcp/config.json`, then `./.atlassian-mcp.json`; values in the
file win, environment variables fill the gaps.

Bitbucket project and repo are detected from your `origin` remote, and the Jira project from
the ticket key in your branch, so most calls need neither.

### 2. Install

| Option | Install | Command for your client |
|---|---|---|
| npx (Node 18+) | nothing | `npx -y @stubbedev/atlassian-mcp@latest` |
| npm | `npm install -g @stubbedev/atlassian-mcp` | `atlassian-mcp` |
| Composer (PHP 8.1+) | `composer require --dev stubbedev/atlassian-mcp` | `vendor/stubbedev/atlassian-mcp/bin/atlassian-mcp-native` |
| Claude Desktop | open the `.mcpb` from the [latest release](https://github.com/stubbedev/atlassian-mcp/releases/latest) | — |
| Binary | `atlassian-mcp_<os>_<arch>` from the [latest release](https://github.com/stubbedev/atlassian-mcp/releases/latest) | its path |
| Go | `go install -tags nodynamic github.com/stubbedev/atlassian-mcp@latest` | `atlassian-mcp` |
| Nix | `nix profile install github:stubbedev/atlassian-mcp` | `atlassian-mcp` |

npm and Composer download the release binary for your platform, verify its checksum and
cache it per version under `$XDG_CACHE_HOME/atlassian-mcp/`. Composer asks once to trust the
plugin (`composer config allow-plugins.stubbedev/atlassian-mcp true` for CI); `composer
global require` works too. To update, re-run the install, or `npx clear-npx-cache` for npx.

### 3. Connect

```bash
claude mcp add atlassian -- npx -y @stubbedev/atlassian-mcp@latest
codex mcp add atlassian -- npx -y @stubbedev/atlassian-mcp@latest
code --add-mcp '{"name":"atlassian","command":"npx","args":["-y","@stubbedev/atlassian-mcp@latest"]}'
```

Other clients take the common `mcpServers` shape:

```json
{
  "mcpServers": {
    "atlassian": {
      "command": "npx",
      "args": ["-y", "@stubbedev/atlassian-mcp@latest", "--config", "/home/you/.atlassian-mcp.json"],
      "env": { "ATLASSIAN_MCP_REPO_ROOT": "/home/you/code/my-repo" }
    }
  }
}
```

| Client | Where |
|---|---|
| Claude Desktop | `.mcpb` bundle, or `claude_desktop_config.json` (macOS `~/Library/Application Support/Claude/`, Windows `%APPDATA%\Claude\`) |
| Cursor | `~/.cursor/mcp.json` or `.cursor/mcp.json` |
| Windsurf | `~/.codeium/windsurf/mcp_config.json` |
| LM Studio, Cherry Studio, Witsy, Jan, 5ire | in-app MCP settings, same fields |

<details>
<summary>Zed, OpenCode, Codex and Goose use their own shapes</summary>

```jsonc
// Zed: ~/.config/zed/settings.json
{ "context_servers": { "atlassian": { "command": { "path": "npx", "args": ["-y", "@stubbedev/atlassian-mcp@latest"] } } } }

// OpenCode: opencode.json
{ "mcp": { "atlassian": { "type": "local", "command": ["npx", "-y", "@stubbedev/atlassian-mcp@latest"] } } }
```

```toml
# Codex: ~/.codex/config.toml (note the underscore)
[mcp_servers.atlassian]
command = "npx"
args = ["-y", "@stubbedev/atlassian-mcp@latest"]
```

```yaml
# Goose: ~/.config/goose/config.yaml
extensions:
  atlassian: { enabled: true, type: stdio, cmd: npx, args: ["-y", "@stubbedev/atlassian-mcp@latest"] }
```

</details>

**Desktop apps** start the server with no shell, a minimal `PATH` and no workspace. Use
absolute paths (`which npx`), and set `ATLASSIAN_MCP_REPO_ROOT` so the git and PR tools know
which repository you mean (comma-separate several; `repoPath` on a call still wins). Claude
Desktop logs server output to `~/Library/Logs/Claude/mcp-server-atlassian.log`.

ChatGPT only accepts remote HTTPS servers with OAuth, so it is not supported.

### Personal access tokens

- **Jira 8.14+**: profile avatar → **Profile** → **Personal Access Tokens** → **Create token**.
- **Bitbucket 5.5+**: profile avatar → **Manage account** → **Personal access tokens** →
  **Create a token**, with Projects: Read and Repositories: Write (to open PRs and comment).

## HTTP mode

One long-lived server can serve many clients over Streamable HTTP:

```bash
atlassian-mcp --http                  # 127.0.0.1:7337, same as ATLASSIAN_MCP_HTTP=1
claude mcp add --transport http atlassian http://127.0.0.1:7337/mcp
```

The endpoint is `POST /mcp` with an optional `GET /mcp` event stream; sessions are isolated
by `Mcp-Session-Id`, and `GET /healthz` is an unauthenticated liveness probe. Binding a
non-loopback address requires `ATLASSIAN_MCP_HTTP_TOKEN` (sent as a bearer token); terminate
TLS at your proxy.

The repository a call works on comes from, in order: `repoPath`, a root header
(`X-Repo-Root`, `X-Mcp-Roots`, `X-Mcp-Root`, `Mcp-Roots` or `Mcp-Root`; a path or `file://`
URI), `ATLASSIAN_MCP_REPO_ROOT`, the client's MCP roots, then the working directory (stdio
only). Repositories must be on the server's machine. MCP revision 2026-07-28 drops
server-to-client requests, so there a root header or `repoPath` is the only way to name one.

## Attachments

`attachments` arguments on `jira_mutate`, `bitbucket_mutate` and `bitbucket_comment` accept a
local path, an `http(s)` URL, or a `data:` URI. URLs on your Jira or Bitbucket host are
fetched with your token, which is never sent anywhere else; downloads are capped at 250 MB.
Files pasted into the chat have no path, so use `attach_files` for those.

`get_attachment` decodes in-process, with no `ffmpeg` or other tools: images (including
AVIF, HEIC and JPEG XL) are resized; animations and video (H.264, Motion JPEG, VP8 keyframes)
become sampled frames; PDFs become text, or page images when scanned. Anything else is saved
to a temp file whose path is returned; `saveTo` keeps the original.

Git also runs in-process. Fetch and push use ssh-agent, `~/.ssh` keys and `known_hosts`,
`~/.git-credentials`, `~/.netrc`, or the Bitbucket token; credential helpers, hooks and LFS
do not run.

## Environment

| Variable | Purpose | Default |
|---|---|---|
| `ATLASSIAN_MCP_CONFIG` | Config file path | see [Configure](#1-configure) |
| `ATLASSIAN_MCP_REPO_ROOT` | Default repository root(s), comma-separated | unset |
| `ATLASSIAN_MCP_HTTP` | Serve HTTP: `1` or `host:port` | stdio |
| `ATLASSIAN_MCP_HTTP_TOKEN` | Bearer token; required off loopback | unset |
| `ATLASSIAN_MCP_MARK_AI_TEXT` | `false` drops the `[AI]` attribution line | `true` |
| `ATLASSIAN_MCP_TMP_TTL_DAYS` | Age limit for auto-saved attachments | `7` |
| `ATLASSIAN_MCP_TMP_MAX_BYTES` | Size limit for auto-saved attachments | 1 GB |
| `ATLASSIAN_MCP_SKIP_DOWNLOAD` | `1` skips the npm/Composer install-time download | unset |

---

Development, releases and internals: [MAINTAINERS.md](MAINTAINERS.md).
