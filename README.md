# atlassian-mcp

A [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server for **self-hosted Jira** (Server / Data Center) and **self-hosted Bitbucket** (Server / Data Center). Exposes tools for natural-language workflows around tickets, pull requests, review threads, and git context.

> **Note:** This server only supports self-hosted instances. Jira Cloud and Bitbucket Cloud use different APIs and are not supported.

---

## Tools

### Workflow

| Tool | Description |
|---|---|
| `get_dev_context` | Master entry point: git state + linked Jira ticket + open PR with reviewer/blocker status and next-step hints |
| `start_work` | Start a Jira ticket: resolves it by key or free-text `query` (with a picker when several match), creates a local branch (`feature/FOO-123-slug`) off the repository default branch, fetches the project README from Bitbucket so commit/PR conventions are in context, and optionally transitions the ticket |
| `complete_work` | Close out finished work: merges the open PR and transitions the Jira ticket to Done. Refuses to merge while reviewers have not approved or a build failed (`force=true` overrides) |

### Git

| Tool | Description |
|---|---|
| `git_get_context` | Branch, upstream state, remote URL, recent commits, working tree status, diff stat, and Jira keys in branch name. Pass `fromRef`/`toRef` for a diff between refs instead, paged via `charOffset` |

### Jira

| Tool | Description |
|---|---|
| `jira_search` | Discover resources: `issues`, `projects`, `issue_types`, `boards`, `sprints`, `board_overview`, `versions`, `components`, `fields`, or `users` via `resource` param |
| `jira_get` | Full details for one issue: summary, description, status, sprint, transitions, comments, and attachment list |
| `jira_mutate` | Create, update, transition, comment (`commentAction`: `add` / `update` / `delete`), upload local files as attachments, link, add to sprint, log work, change issue type, set any custom field by name (`create.customFields` / `update.customFields`), or manage a fix version (`version.action`: `create` / `update` / `release` / `archive` / `delete`) — several in one call. Markdown in any text field is converted to Jira wiki markup |

### Bitbucket

| Tool | Description |
|---|---|
| `bitbucket_search` | Discover resources: `pull_requests` (default), `repos`, `branches`, or `users` via `resource` param; `mine=true` for your inbox, narrowed with `role=author` / `reviewer` / `participant` |
| `bitbucket_get_pr` | Full PR details: metadata, commits, comments, blockers, build status, optional diff, and any attachments referenced from the description or comments |
| `bitbucket_mutate` | Create/update a PR, or perform lifecycle actions: `approve`, `unapprove`, `needs_work`, `merge`, `decline`. Reviewer names are verified against Bitbucket, and an update that would drop existing reviewers needs `update.replaceReviewers=true`. `create.attachments` / `update.attachments` upload local files (screenshots, logs) to the repo and reference them from the description |
| `bitbucket_comment` | Add, update, or delete a PR comment; for code changes use `suggestion` so Bitbucket shows Apply suggestion. Enforced here: one reply per thread, no new top-level comment on your own PR (`asAuthor=true` to override), `#123` references rewritten as links. `pending=true` posts an unpublished draft-review comment. `attachments` uploads local files and references them from the comment |
| `bitbucket_get_file` | Raw file content at a branch, tag, or commit — or pass `prId` to read the PR source branch. Every response names the path and ref it came from, and pages via `maxChars`/`charOffset` |
| `bitbucket_pr_tasks` | Manage PR tasks (checklist items): `list`, `create`, `resolve`, `reopen`, `delete` |

### Shared

| Tool | Description |
|---|---|
| `get_attachment` | Fetch an attachment by ID from Jira (`source=jira`, IDs from `jira_get`) or Bitbucket (`source=bitbucket`, IDs from `bitbucket_get_pr`). Images, videos, animated images (GIF/APNG/animated WebP), audio, and PDFs are decoded inline so the model can see/hear them; text/JSON inline. Oversized or non-renderable attachments are auto-saved to a temp file and the path is returned. `saveTo=/absolute/path` streams the original to disk |

### Resources

| URI | Description |
|---|---|
| `dev-context://current` | The same live report as `get_dev_context` — branch state, linked Jira tickets, open PR — as an MCP resource. Re-read it for fresh state instead of spending another tool call. The repo is resolved per read from the caller's session, so the static URI serves whatever workspace the client is in |

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
- "create version 9.1.0 in PAY" → `jira_mutate` with `version.action=create`, `version.projectKey=PAY`, `version.name=9.1.0`
- "list releases for PAY" → `jira_search` with `resource=versions`, `project=PAY`
- "release version 12345" → `jira_mutate` with `version.action=release`, `version.id=12345`
- "set fix version 9.1.0 on FOO-123" → `jira_mutate` with `update.fixVersion=9.1.0`
- "create a task under epic FOO-100" → `jira_mutate` with `create.issueType=Task`, `create.parent=FOO-100` (auto-detects Epic and sets Epic Link)
- "move FOO-123 under epic FOO-100" → `jira_mutate` with `update.epicLink=FOO-100`
- "create an epic" → `jira_mutate` with `create.issueType=Epic` (Epic Name defaults to the summary)
- "set story points to 5" → `jira_mutate` with `update.customFields={"Story Points": 5}` — values are plain (option label, username, date, array of labels); the server wraps them per the field schema
- "what can I set on this ticket / on an Epic?" → `jira_search resource=fields` with `issueKey=FOO-123` (edit screen) or `project=FOO`+`issueType=Epic` (create screen): required and optional fields, value shapes, allowed values

---

## What the server enforces

These are guarantees in the code, not advice in a tool description — a client
cannot get them wrong, and they need no prompting:

- **Arguments are validated before a call runs.** Enum values and required
  fields are checked against each tool's schema, with case and `-`/`_`
  differences normalised. An unknown `action`/`resource` is an error, never a
  silent fallback to some default branch of the handler.
- **Names are resolved before anything is written.** Jira `assignee`/`reporter`,
  components and fix versions, and Bitbucket reviewers are checked first; a bad
  one comes back with the valid options instead of an opaque 400.
- **Markdown is converted to Jira wiki markup** on every Jira write (comments,
  descriptions, worklogs). Text that is already wiki markup is left alone.
- **PR comment hygiene:** one reply per thread per author, no duplicate of a
  comment you already posted, no new top-level comment on a PR you authored
  (`asAuthor=true` to override), no tasks via `severity`, no emoji, and bare
  `#123` references are rewritten as links to that comment.
- **Inline comments anchor to what was reviewed.** Reading a PR records the
  commit pair for that session; inline comments bind to it and are remapped onto
  current head when the branch has moved, so a comment never lands on unrelated
  code.
- **Reviewers are never dropped by accident** — an update that would remove one
  needs `update.replaceReviewers=true`.
- **`complete_work` will not merge** while reviewers have not approved or a build
  on the PR head has failed, unless `force=true`.
- **Truncated output always says how to continue**, naming the argument that
  fetches the rest. `bitbucket_get_file` also states the path and ref it read,
  so reading the wrong branch is visible rather than silent.
- **Tool annotations** (`readOnlyHint`, `destructiveHint`, `idempotentHint`) are
  published for every tool, so hosts can gate confirmation on metadata.

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
- `bitbucket_mutate` with `create` auto-detects `fromBranch` from your current branch and returns the existing open PR if one already exists for that branch. Other Bitbucket tools auto-target that PR when `prId` is omitted.
- Jira project-scoped calls accept `projectKey` and work best when provided.
- If `projectKey` is omitted for Jira issue creation/type lookup, the server tries to infer it from your current branch ticket key, falls back to auto-select when only one project is visible, and otherwise returns a numbered project list to pick from.

Alternatively, use environment variables (or a `.env` file in this directory):

```env
JIRA_URL=https://jira.example.com
JIRA_ACCESS_TOKEN=your-jira-personal-access-token
BITBUCKET_URL=https://bitbucket.example.com
BITBUCKET_ACCESS_TOKEN=your-bitbucket-personal-access-token
```

Config is resolved in this order: `--config <path>` CLI arg → `ATLASSIAN_MCP_CONFIG` env var → `~/.atlassian-mcp.json` → `$XDG_CONFIG_HOME/atlassian-mcp/config.json` (default `~/.config/atlassian-mcp/config.json`) → `.atlassian-mcp.json` in cwd → environment variables. A leading `~` in the first two is expanded by the server, so a client that spawns it without a shell still resolves the path. Within a file, per-field: a value in the config file wins, environment variables fill the gaps.

### 2. Connect to your AI tool

No cloning or building required — just point your tool at `npx @stubbedev/atlassian-mcp@latest` and it will install and run automatically.

CLI-driven clients need one line:

```bash
claude mcp add atlassian -- npx -y @stubbedev/atlassian-mcp@latest       # Claude Code
codex mcp add atlassian -- npx -y @stubbedev/atlassian-mcp@latest        # Codex CLI / IDE / app
code --add-mcp '{"name":"atlassian","command":"npx","args":["-y","@stubbedev/atlassian-mcp@latest"]}'   # VS Code
```

Desktop apps: **Claude Desktop** installs a one-click [`.mcpb` bundle](#claude-desktop) —
no Node, no JSON. Everything else takes a config file; see below.

> Note: `--prefer-online` can break MCP startup in some clients. Keep the command simple and use the update steps below when you want to refresh.

---

#### Claude Code

```bash
claude mcp add atlassian -- npx -y @stubbedev/atlassian-mcp@latest --config ~/.atlassian-mcp.json
```

---

#### Claude Desktop

**One-click (recommended).** Grab the `.mcpb` bundle for your platform from the
[latest release](https://github.com/stubbedev/atlassian-mcp/releases/latest) —
`atlassian-mcp_darwin_arm64.mcpb` (Apple Silicon), `atlassian-mcp_darwin_amd64.mcpb`
(Intel Mac), `atlassian-mcp_windows_amd64.mcpb` — then **double-click it**, drag it onto the
Claude Desktop window, or use **Settings → Extensions → Advanced settings → Install
Extension…**. The install dialog asks for Jira/Bitbucket URL and token (tokens are stored
by Claude Desktop, not in a file) plus **Repository**, the working tree the git and PR tools
default to. Leaving URL/token blank reuses an existing `~/.atlassian-mcp.json`.

The bundle carries the binary, so there is no Node, no `npx`, no `PATH` to fix and no JSON
to edit. [MCP Bundles](https://github.com/modelcontextprotocol/mcpb) are a Claude Desktop
feature today; other clients use the config files below.

**Manual config.** Claude Desktop is a GUI app: it launches the server with a minimal
`PATH`, no shell, and `/` as the working directory. So `command` must be an **absolute
path** (a bare `npx` fails with `spawn npx ENOENT`), a `.env` file or relative
`--config` path never resolves, and nothing expands `~` for you — the server expands a
leading `~` in `--config` / `ATLASSIAN_MCP_CONFIG` itself, but a client that inserts `~`
anywhere else will not. Config file:

- macOS: `~/Library/Application Support/Claude/claude_desktop_config.json`
- Windows: `%APPDATA%\Claude\claude_desktop_config.json`

```json
{
  "mcpServers": {
    "atlassian": {
      "command": "/absolute/path/to/atlassian-mcp",
      "env": {
        "JIRA_URL": "https://jira.example.com",
        "JIRA_ACCESS_TOKEN": "your-jira-personal-access-token",
        "BITBUCKET_URL": "https://bitbucket.example.com",
        "BITBUCKET_ACCESS_TOKEN": "your-bitbucket-personal-access-token",
        "ATLASSIAN_MCP_REPO_ROOT": "/Users/you/code/my-repo"
      }
    }
  }
}
```

To keep `npx`, set `command` to the absolute path of your launcher (`which npx`, e.g.
`/opt/homebrew/bin/npx`) with `"args": ["-y", "@stubbedev/atlassian-mcp@latest"]`.

`ATLASSIAN_MCP_REPO_ROOT` is what makes `get_dev_context`, `git_get_context`,
`start_work`, `complete_work` and Bitbucket repo auto-detection usable here: a desktop app
has no workspace, so it advertises no MCP roots and there is no useful cwd to fall back to.
Comma-separate several worktrees (first git repo wins); a per-call `repoPath` still
overrides it.

On Windows, Git is frequently absent from a GUI app's `PATH`. The server probes the usual
install locations before giving up; set `ATLASSIAN_MCP_GIT_PATH` if yours lives elsewhere.

Server stderr is logged to `~/Library/Logs/Claude/mcp-server-atlassian.log` (macOS) or
`%APPDATA%\Claude\logs\mcp-server-atlassian.log` (Windows) — read that first when a
connection fails.

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
      "command": ["npx", "-y", "@stubbedev/atlassian-mcp@latest", "--config", "/home/you/.atlassian-mcp.json"],
      "environment": { "ATLASSIAN_MCP_REPO_ROOT": "/home/you/code/my-repo" }
    }
  }
}
```

`environment` also accepts the `JIRA_*` / `BITBUCKET_*` variables if you would rather not
keep a config file. Set `"type": "remote"` with `"url"` and `"headers"` to point at a
shared [HTTP server](#running-as-an-http-server-shared--behind-a-proxy) instead.

---

#### Codex (CLI, IDE extension, app)

One command — it writes the config for all three:

```bash
codex mcp add atlassian -- npx -y @stubbedev/atlassian-mcp@latest
```

Or edit `~/.codex/config.toml` directly (`.codex/config.toml` in a trusted project for a
project-scoped server). Note the TOML table name is `mcp_servers`, with an underscore:

```toml
[mcp_servers.atlassian]
command = "npx"
args = ["-y", "@stubbedev/atlassian-mcp@latest", "--config", "/home/you/.atlassian-mcp.json"]

# Optional — instead of a config file, and to pin the repo for the git/PR tools:
[mcp_servers.atlassian.env]
JIRA_URL = "https://jira.example.com"
JIRA_ACCESS_TOKEN = "…"
ATLASSIAN_MCP_REPO_ROOT = "/home/you/code/my-repo"
```

Codex picks the transport from the keys present: `command` means stdio, `url` means
streamable HTTP. To share one [HTTP server](#running-as-an-http-server-shared--behind-a-proxy):

```toml
[mcp_servers.atlassian]
url = "http://127.0.0.1:7337/mcp"
bearer_token_env_var = "ATLASSIAN_MCP_HTTP_TOKEN"
```

---

#### VS Code / GitHub Copilot

```bash
code --add-mcp '{"name":"atlassian","command":"npx","args":["-y","@stubbedev/atlassian-mcp@latest"]}'
```

Or commit `.vscode/mcp.json` with a `servers` object of the same shape to share it with the
repo.

---

#### Any other MCP-compatible tool

Most clients accept the Claude Desktop shape — an `mcpServers` object keyed by name, with
`command`, `args` and `env`:

```json
{
  "mcpServers": {
    "atlassian": {
      "command": "npx",
      "args": ["-y", "@stubbedev/atlassian-mcp@latest"],
      "env": {
        "JIRA_URL": "https://jira.example.com",
        "JIRA_ACCESS_TOKEN": "…",
        "BITBUCKET_URL": "https://bitbucket.example.com",
        "BITBUCKET_ACCESS_TOKEN": "…",
        "ATLASSIAN_MCP_REPO_ROOT": "/home/you/code/my-repo"
      }
    }
  }
}
```

LM Studio uses exactly that shape in its own `mcp.json` (edit it from the app's plugin
panel); Cherry Studio, Witsy, Jan and 5ire have in-app MCP dialogs with the same fields.

Goose is the exception — its `~/.config/goose/config.yaml` uses `extensions:` with `cmd`
rather than `command`:

```yaml
extensions:
  atlassian:
    enabled: true
    type: stdio
    cmd: npx
    args: ["-y", "@stubbedev/atlassian-mcp@latest"]
    envs:
      ATLASSIAN_MCP_REPO_ROOT: /home/you/code/my-repo
```

Every GUI client brings the caveats from the [Claude Desktop](#claude-desktop) section:
absolute `command` path, no usable cwd, no MCP roots — so set `ATLASSIAN_MCP_REPO_ROOT`.

---

#### ChatGPT (desktop / web) — not supported

ChatGPT connectors accept **remote HTTPS MCP servers only** (streamable HTTP or SSE, with
OAuth or no auth); it cannot spawn a local stdio server. This server's `--http` mode speaks
the right protocol, but making it work would mean exposing an endpoint that reaches your
self-hosted Jira/Bitbucket to OpenAI's servers, and ChatGPT offers no place for the static
bearer token this server uses. Use a client from the list above.

### Updating existing installs

If your MCP client is already configured and you want the newest package version:

```bash
npx clear-npx-cache
```

Then restart your MCP client.

---

### Install without npm

The server is a single static Go binary. The `npx` path above downloads the prebuilt
binary for your platform on first run; these alternatives skip Node entirely — as does the
[`.mcpb` bundle](#claude-desktop) for Claude Desktop, and the per-platform binaries
attached to every [release](https://github.com/stubbedev/atlassian-mcp/releases/latest):

```bash
# Go toolchain — installs to $GOBIN / $GOPATH/bin
go install github.com/stubbedev/atlassian-mcp@latest

# Nix flake
nix run github:stubbedev/atlassian-mcp -- --config ~/.atlassian-mcp.json
```

Then point your MCP client's `command` at the resulting `atlassian-mcp` binary
instead of `npx`. On these Node-free paths (`go install`, Nix, a release binary or the
`.mcpb` bundle) `ffmpeg`/`ffprobe` must be available on `PATH` for video and
animated-image attachments (or set `ATLASSIAN_MCP_FFMPEG_PATH` /
`ATLASSIAN_MCP_FFPROBE_PATH`); the npm wrapper bundles them automatically. Everything
else — still images, PDF text, JSON/text — is pure Go and needs nothing extra.

### Running as an HTTP server (shared / behind a proxy)

By default the server speaks MCP over **stdio** (one process per client, launched
by your editor). It can instead run as a long-lived **Streamable HTTP** server that
many clients share — useful behind a reverse proxy:

```bash
atlassian-mcp --http                 # binds 127.0.0.1:7337
atlassian-mcp --http 127.0.0.1:9000  # custom address
ATLASSIAN_MCP_HTTP=1 atlassian-mcp   # same, via env
```

- Single endpoint `POST /mcp` (JSON-RPC) plus an optional `GET /mcp` SSE stream that
  carries server→client requests (`roots/list`, elicitation). The server is **stateful**:
  `initialize` mints a session and returns an `Mcp-Session-Id` header, which the client
  **must** echo on every subsequent request and on the SSE stream. Requests with a
  missing/unknown/expired session id get **HTTP 404** so the client re-initializes
  (standard MCP-client behaviour). Each connected client/worktree is an isolated session;
  per-session state (cached roots, PR review anchors) is dropped once the session ends.
- **Auth:** on a loopback bind no token is needed. Binding a non-loopback address
  **requires** `ATLASSIAN_MCP_HTTP_TOKEN` (sent by clients as `Authorization: Bearer …`);
  the server refuses to start otherwise. Terminate TLS at your proxy.
- **`GET /healthz`** is an unauthenticated liveness probe (returns `ok`) for proxies/load
  balancers.

**Repo context comes from the client, not the server's working directory.** Tools that
need a repo (`git_get_context`, `get_dev_context`, `start_work`, `complete_work`, and
Bitbucket project/repo auto-detection) resolve it in this order: an explicit `repoPath`
argument → a **root pinned via request header** (see below) → **`ATLASSIAN_MCP_REPO_ROOT`**
(comma-separated for several worktrees — the only workspace signal a GUI desktop client
can give) → the client's **MCP workspace roots** (the server asks via `roots/list`, caches
per session, and refreshes on `notifications/roots/list_changed`) → the process cwd (stdio
only). So one shared HTTP server handles many worktrees: each client's own workspace drives
its calls. When a session exposes **several** roots (multiple worktrees), a tool with no
`repoPath` uses the first git-repo root; pass `repoPath` (an absolute path, or a worktree
name/basename that matches one of the roots) to target a specific worktree. For Bitbucket,
passing `projectKey`+`repoSlug` explicitly skips repo detection entirely. The repos must be
reachable on the server's host (the git tools run `git` locally).

**Pinning the root via a request header (HTTP).** A reverse proxy or harness that already
knows the working tree can hand it to the server directly, skipping the `roots/list`
round-trip (and working even when the client never advertised the `roots` capability).
Send a `file://` URI or absolute path (comma-separated for multiple; first git repo wins):

```
X-Repo-Root: /srv/myrepo
X-Mcp-Root: file:///srv/myrepo
X-Mcp-Roots: /srv/a, /srv/b
```

Accepted header names, in precedence order: `X-Repo-Root`, `X-Mcp-Roots`, `X-Mcp-Root`,
`Mcp-Roots`, `Mcp-Root`. A header value is authoritative — it takes precedence over
`roots/list` and survives `list_changed`.

> **Protocol note:** MCP revision **2026-07-28** (SEP-2322/2575) forbids server-initiated
> JSON-RPC requests, so `roots/list` is unavailable on that revision — the server says so
> explicitly instead of hanging. On 2026-07-28 clients, a root **header** (or an explicit
> `repoPath` / `projectKey`+`repoSlug`) is the only way to give the server repo context.

Client config for an already-running HTTP server (Claude Code example):

```bash
claude mcp add --transport http atlassian http://127.0.0.1:7337/mcp
```

### Attachment decoding pipeline

The `get_attachment` tool decodes binary attachments into model-readable content before returning them:

| Input | What gets returned | How |
| --- | --- | --- |
| Static images (PNG/JPEG/WebP/BMP/TIFF/GIF/SVG…) | Resized image content blocks | native Go (`imaging`, long edge ≤ `maxDimension`, default 1568; EXIF auto-orient; PNG for alpha, else JPEG) |
| Animated images (GIF/APNG/animated WebP) | N sampled frames as image content blocks | `ffmpeg` + native Go re-encode (default 6 frames @ 768 px) |
| Video (mp4/webm/mov/…) | N sampled frames as image content blocks | `ffmpeg`/`ffprobe`. Uniform or scene-change sampling. Re-call with `start`, `end`, `frames`, `mode`, `sceneThreshold` to zoom in |
| Audio (mp3/wav/ogg/…) | MCP audio content block | passthrough |
| PDFs | Extracted text — or rasterized pages if text is empty (scanned PDFs) | native Go text extraction (`ledongthuc/pdf`); rasterization shells to `pdftoppm`/`mutool` if present, else the original is saved to disk |
| Text-like (json/xml/yaml/…) | Text content block | passthrough |
| Everything else (or oversized) | Auto-saved to a temp file; path is returned | `os.TempDir()` with `atlmcp-` prefix |

Auto-saved files are periodically pruned by TTL and total-size quota — see *Environment overrides* below.

### External tools (optional)

Image and PDF-text decoding are pure Go and need nothing extra. The two pipelines that have no
pure-Go implementation shell out to external binaries:

- **`ffmpeg` + `ffprobe`** — video and animated-image frame sampling. The npm wrapper bundles
  [`ffmpeg-static`](https://www.npmjs.com/package/ffmpeg-static) /
  [`ffprobe-static`](https://www.npmjs.com/package/ffprobe-static) and injects their paths, so the
  npx install path is zero-config. On every Node-free path (`go install`, Nix, release binary,
  `.mcpb` bundle), install `ffmpeg` (it provides `ffprobe`) or set the env vars below.
- **`pdftoppm` (poppler) or `mutool` (MuPDF)** — only needed to rasterize *scanned* PDFs that have no
  extractable text. If neither is on `PATH`, such PDFs are saved to disk instead.

### Environment overrides

| Variable | Purpose | Default |
| --- | --- | --- |
| `ATLASSIAN_MCP_HTTP` | Run as a Streamable HTTP server instead of stdio. `1`/`true` → `127.0.0.1:7337`; or set an explicit `host:port`. Same as `--http`. | unset (stdio) |
| `ATLASSIAN_MCP_HTTP_TOKEN` | Bearer token for HTTP mode. Optional on loopback binds; **required** on non-loopback binds. | unset |
| `ATLASSIAN_MCP_REPO_ROOT` | Default workspace root(s) for the git/PR tools, comma-separated. `file://` URIs, absolute paths, `~/…` and Windows drive paths all work. Needed by clients that expose no MCP roots (desktop apps). Overridden by a `repoPath` argument or a root header. | unset |
| `ATLASSIAN_MCP_GIT_PATH` | Path to the `git` executable. Only needed when `git` is off the host app's `PATH`; the server already probes the usual install locations. | `git` on `PATH` |
| `ATLASSIAN_MCP_FFMPEG_PATH` | Path to `ffmpeg` binary. | npm: bundled `ffmpeg-static`; otherwise `ffmpeg` on `PATH` |
| `ATLASSIAN_MCP_FFPROBE_PATH` | Path to `ffprobe` binary. | npm: bundled `ffprobe-static`; otherwise `ffprobe` on `PATH` |
| `ATLASSIAN_MCP_TMP_TTL_DAYS` | Auto-saved attachments older than this are pruned. | `7` |
| `ATLASSIAN_MCP_TMP_MAX_BYTES` | Total-size quota for auto-saved attachments in `os.tmpdir()`. When exceeded, oldest are evicted. | `1073741824` (1 GB) |

---

## Releases (Maintainers)

This package is published to npm as `@stubbedev/atlassian-mcp`.

Use semantic versioning for releases. Breaking tool-surface changes should bump the minor version while `<1.0.0` (for example `0.0.x` -> `0.1.0`).

On a pushed `v*` tag, `.github/workflows/publish.yml` cross-compiles the Go binary for 14
OS/arch targets, packs six of them into `.mcpb` bundles for one-click desktop install
(`packaging/mcpb/pack.sh`, macOS/Windows/Linux × amd64/arm64), attaches everything to a
GitHub release, and publishes the npm wrapper (which downloads the matching binary on
install). `just bundle` builds a bundle for the host platform locally.

Release flow (`just` drives it; it refuses to run on a dirty tree):

```bash
just release-preview       # show the next patch/minor/major versions
just release-patch         # or release-minor / release-major
```

`just release-<level>` bumps the version in `package.json`, re-syncs the Nix `vendorHash`
(`just sync-flake`), runs the gates (`just check`), commits `release: vX.Y.Z`, tags, and
pushes both the branch and the tag. The tag push triggers `publish.yml`.

`package.json` is the single source of truth for the version: the binary embeds it via
`go:embed` (no `-ldflags`) and `flake.nix` reads it, so one bump moves everything.
The equivalent npm scripts (`npm run release:patch` / `:minor` / `:major`) still work.

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

The server is a single Go module at the repo root (no `src/` tree).

Tasks live in the `justfile` and mirror the CI gates, so a green `just check` predicts
green CI:

```bash
just            # list tasks
just check      # vet + test + build (what ci.yml runs)
just fmt        # gofmt -w .
just sync-flake # recompute the Nix vendorHash after a dependency change

# Or the raw commands
go build -o atlassian-mcp .
./atlassian-mcp --config /path/to/config.json
go vet ./... && go test ./...

# Quick release smoke check (build + tools/list validation; CI also does a full stdio handshake)
npm run smoke
```

Tool schemas live in `tools.json` (embedded into the binary) and the MCP protocol layer is
the official [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk);
the Go files at the repo root hold the tool logic.
