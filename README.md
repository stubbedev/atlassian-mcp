# atlassian-mcp

A [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server for **self-hosted Jira** (Server / Data Center) and **self-hosted Bitbucket** (Server / Data Center). Exposes tools for natural-language workflows around tickets, pull requests, review threads, and git context.

> **Note:** This server only supports self-hosted instances. Jira Cloud and Bitbucket Cloud use different APIs and are not supported.

---

## Tools

### Workflow

| Tool | Description |
|---|---|
| `get_dev_context` | Master entry point: git state + linked Jira ticket + open PR with reviewer/blocker status and next-step hints |
| `start_work` | Start a Jira ticket: fetches it, creates a local branch (`feature/FOO-123-slug`) off the repository default branch, and optionally transitions the ticket |
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
| `jira_mutate` | Create, update, transition, comment (`commentAction`: `add` / `update` / `delete`), attach, link, add to sprint, log work, or manage a fix version (`version.action`: `create` / `update` / `release` / `archive` / `delete`) — several in one call. Markdown in any text field is converted to Jira wiki markup |

### Bitbucket

| Tool | Description |
|---|---|
| `bitbucket_search` | Discover resources: `pull_requests` (default), `repos`, `branches`, or `users` via `resource` param; `mine=true` for your inbox |
| `bitbucket_get_pr` | Full PR details: metadata, commits, comments, blockers, build status, optional diff, and any attachments referenced from the description or comments |
| `bitbucket_mutate` | Create/update a PR, or perform lifecycle actions: `approve`, `unapprove`, `needs_work`, `merge`, `decline`. Reviewer names are verified against Bitbucket, and an update that would drop existing reviewers needs `update.replaceReviewers=true` |
| `bitbucket_comment` | Add, update, or delete a PR comment; for code changes use `suggestion` so Bitbucket shows Apply suggestion. Enforced here: one reply per thread, no new top-level comment on your own PR (`asAuthor=true` to override), `#123` references rewritten as links |
| `bitbucket_get_file` | Raw file content at a branch, tag, or commit — or pass `prId` to read the PR source branch. Every response names the path and ref it came from, and pages via `maxChars`/`charOffset` |
| `bitbucket_pr_tasks` | Manage PR tasks (checklist items): `list`, `create`, `resolve`, `reopen`, `delete` |

### Shared

| Tool | Description |
|---|---|
| `get_attachment` | Fetch an attachment by ID from Jira (`source=jira`, IDs from `jira_get`) or Bitbucket (`source=bitbucket`, IDs from `bitbucket_get_pr`). Images, videos, animated images (GIF/APNG/animated WebP), audio, and PDFs are decoded inline so the model can see/hear them; text/JSON inline. Oversized or non-renderable attachments are auto-saved to a temp file and the path is returned. `saveTo=/absolute/path` streams the original to disk |

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

Config is resolved in this order: `--config <path>` CLI arg → `ATLASSIAN_MCP_CONFIG` env var → `~/.atlassian-mcp.json` → `$XDG_CONFIG_HOME/atlassian-mcp/config.json` (default `~/.config/atlassian-mcp/config.json`) → `.atlassian-mcp.json` in cwd → environment variables.

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

### Install without npm

The server is a single static Go binary. The `npx` path above downloads the prebuilt
binary for your platform on first run; these alternatives skip Node entirely:

```bash
# Go toolchain — installs to $GOBIN / $GOPATH/bin
go install github.com/stubbedev/atlassian-mcp@latest

# Nix flake
nix run github:stubbedev/atlassian-mcp -- --config ~/.atlassian-mcp.json
```

Then point your MCP client's `command` at the resulting `atlassian-mcp` binary
instead of `npx`. On these paths `ffmpeg`/`ffprobe` must be available on `PATH`
(or set `ATLASSIAN_MCP_FFMPEG_PATH` / `ATLASSIAN_MCP_FFPROBE_PATH`); the npm
wrapper bundles them automatically.

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
  (standard MCP-client behaviour). Each connected client/worktree is an isolated session.
- **Auth:** on a loopback bind no token is needed. Binding a non-loopback address
  **requires** `ATLASSIAN_MCP_HTTP_TOKEN` (sent by clients as `Authorization: Bearer …`);
  the server refuses to start otherwise. Terminate TLS at your proxy.
- **`GET /healthz`** is an unauthenticated liveness probe (returns `ok`) for proxies/load
  balancers. Idle sessions are evicted after 1h.

**Repo context comes from the client, not the server's working directory.** Tools that
need a repo (`git_get_context`, `get_dev_context`, `start_work`, `complete_work`, and
Bitbucket project/repo auto-detection) resolve it in this order: an explicit `repoPath`
argument → a **root pinned via request header** (see below) → the client's **MCP workspace
roots** (the server asks via `roots/list`, caches per session, and refreshes on
`notifications/roots/list_changed`) → the process cwd (stdio
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
X-Mcp-Root: file:///srv/myrepo
X-Mcp-Roots: /srv/a, /srv/b
```

Accepted header names: `X-Mcp-Roots`, `X-Mcp-Root`, `Mcp-Roots`, `Mcp-Root`. A header value
is authoritative — it takes precedence over `roots/list` and survives `list_changed`.

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
  npx install path is zero-config. On the `go install` / Nix paths, install `ffmpeg` (it provides
  `ffprobe`) or set the env vars below.
- **`pdftoppm` (poppler) or `mutool` (MuPDF)** — only needed to rasterize *scanned* PDFs that have no
  extractable text. If neither is on `PATH`, such PDFs are saved to disk instead.

### Environment overrides

| Variable | Purpose | Default |
| --- | --- | --- |
| `ATLASSIAN_MCP_HTTP` | Run as a Streamable HTTP server instead of stdio. `1`/`true` → `127.0.0.1:7337`; or set an explicit `host:port`. Same as `--http`. | unset (stdio) |
| `ATLASSIAN_MCP_HTTP_TOKEN` | Bearer token for HTTP mode. Optional on loopback binds; **required** on non-loopback binds. | unset |
| `ATLASSIAN_MCP_FFMPEG_PATH` | Path to `ffmpeg` binary. | npm: bundled `ffmpeg-static`; otherwise `ffmpeg` on `PATH` |
| `ATLASSIAN_MCP_FFPROBE_PATH` | Path to `ffprobe` binary. | npm: bundled `ffprobe-static`; otherwise `ffprobe` on `PATH` |
| `ATLASSIAN_MCP_TMP_TTL_DAYS` | Auto-saved attachments older than this are pruned. | `7` |
| `ATLASSIAN_MCP_TMP_MAX_BYTES` | Total-size quota for auto-saved attachments in `os.tmpdir()`. When exceeded, oldest are evicted. | `1073741824` (1 GB) |

---

## Releases (Maintainers)

This package is published to npm as `@stubbedev/atlassian-mcp`.

Use semantic versioning for releases. Breaking tool-surface changes should bump the minor version while `<1.0.0` (for example `0.0.x` -> `0.1.0`).

On a pushed `v*` tag, `.github/workflows/publish.yml` cross-compiles the Go binary for 14
OS/arch targets, attaches them to a GitHub release, and publishes the npm wrapper (which
downloads the matching binary on install).

Release flow:

```bash
# choose one: patch | minor | major (also: npm run release:patch / :minor / :major)
npm version patch          # bumps package.json, commits, tags vX.Y.Z
git push origin HEAD --follow-tags
```

`flake.nix` reads its version from `package.json`, so the Nix package tracks the same bump
automatically. GitHub Actions builds + publishes from the pushed tag.

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

```bash
# Build the binary
go build -o atlassian-mcp .

# Run it
./atlassian-mcp --config /path/to/config.json

# Vet + unit tests
go vet ./...
go test ./...

# Test the tool list
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' | ./atlassian-mcp

# Quick release smoke check (build + tools/list validation)
npm run smoke
```
