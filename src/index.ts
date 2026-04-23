#!/usr/bin/env node
import 'dotenv/config';
import { execFileSync } from 'child_process';
import { readFileSync } from 'fs';
import { fileURLToPath } from 'url';
import { dirname, join } from 'path';
import { Server } from '@modelcontextprotocol/sdk/server/index.js';

const _pkg = JSON.parse(
  readFileSync(join(dirname(fileURLToPath(import.meta.url)), '../package.json'), 'utf-8')
) as { version: string };
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
  McpError,
  ErrorCode,
} from '@modelcontextprotocol/sdk/types.js';
import { loadConfig } from './config.js';
import { JiraClient } from './jira.js';
import { BitbucketClient, parseBitbucketRemote } from './bitbucket.js';
import { getContext, getDiff, createBranch, checkRemoteBranch, checkoutRemoteBranch } from './git.js';
import { getDevContext } from './context.js';

function currentGitRemote(): string {
  try { return execFileSync('git', ['remote', 'get-url', 'origin'], { encoding: 'utf-8' }).trim(); } catch { return ''; }
}

function remoteMatchesBitbucketInstance(remote: string, bitbucketUrl: string): boolean {
  if (!remote) return false;
  try {
    const host = new URL(bitbucketUrl).hostname.toLowerCase();
    return remote.toLowerCase().includes(host);
  } catch { return false; }
}

const config = loadConfig();
const jira = config.jira ? new JiraClient(config.jira.url, config.jira.token) : null;

const _remote = currentGitRemote();
const bitbucket = (
  config.bitbucket && remoteMatchesBitbucketInstance(_remote, config.bitbucket.url)
) ? new BitbucketClient(config.bitbucket.url, config.bitbucket.token) : null;

if (config.bitbucket && !bitbucket) {
  console.error(`[atlassian-mcp] Bitbucket configured but remote "${_remote || '(none)'}" does not match ${config.bitbucket.url} — Bitbucket tools disabled for this repo.`);
}

const server = new Server(
  { name: 'atlassian-mcp', version: _pkg.version },
  { capabilities: { tools: {} } }
);

server.onerror = (error) => console.error('[MCP Error]', error);

function normalizeBitbucketArgs(args: unknown): Record<string, unknown> {
  const src = (args && typeof args === 'object') ? (args as Record<string, unknown>) : {};
  const out: Record<string, unknown> = { ...src };
  if (typeof out.project === 'string' && typeof out.projectKey !== 'string') out.projectKey = out.project;
  if (typeof out.repo === 'string' && typeof out.repoSlug !== 'string') out.repoSlug = out.repo;
  return out;
}

function normalizeJiraProjectArgs(args: unknown): Record<string, unknown> {
  const src = (args && typeof args === 'object') ? (args as Record<string, unknown>) : {};
  const out: Record<string, unknown> = { ...src };
  if (typeof out.project === 'string' && typeof out.projectKey !== 'string') out.projectKey = out.project;
  return out;
}

function normalizeJiraMutateArgs(args: unknown): Record<string, unknown> {
  const out = normalizeJiraProjectArgs(args);
  if (out.create && typeof out.create === 'object') {
    const create = { ...(out.create as Record<string, unknown>) };
    if (typeof create.project === 'string' && typeof create.projectKey !== 'string') create.projectKey = create.project;
    out.create = create;
  }
  return out;
}

const JIRA_WIKI_MARKUP_HINT = 'Use Jira wiki markup (Atlassian renderer syntax), not GitHub/CommonMark markdown.';

function issueTypePrefix(issueType: string): string {
  const t = issueType.toLowerCase();
  if (t === 'bug' || t === 'bugfix' || t === 'defect') return 'bugfix';
  if (t === 'hotfix') return 'hotfix';
  if (t === 'task' || t === 'sub-task' || t === 'subtask') return 'task';
  return 'feature'; // story, feature, epic, improvement, etc.
}

function slugifyBranchName(issueKey: string, summary: string, issueType: string): string {
  const prefix = issueTypePrefix(issueType);
  const slug = summary
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '')
    .slice(0, 40)
    .replace(/-$/, '');
  return `${prefix}/${issueKey}-${slug}`;
}

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    // ── Git (always available) ────────────────────────────────────────────
    {
      name: 'git_get_context',
      description: 'Start here for any coding or review task: current branch, upstream ahead/behind, remote URL, recent commits, working tree status, diff stat summary, and Jira keys detected in the branch name. Pass includeDiff=true to also include the full uncommitted diff.',
      inputSchema: {
        type: 'object',
        properties: {
          repoPath:     { type: 'string', description: 'Path to the git repository (defaults to cwd)' },
          commitLimit:  { type: 'number', description: 'Number of recent commits to show (default 10)', default: 10 },
          includeDiff:  { type: 'boolean', description: 'Include full uncommitted diff (default false)', default: false },
        },
      },
    },
    {
      name: 'git_get_diff',
      description: 'Get a diff between two git refs or commits. Use when you need to compare a feature branch to main, inspect a specific commit range, or review changes between two refs. For large diffs, increase maxChars or use charOffset to page through them.',
      inputSchema: {
        type: 'object',
        properties: {
          repoPath:   { type: 'string', description: 'Path to the git repository (defaults to cwd)' },
          fromRef:    { type: 'string', description: 'Base ref or commit' },
          toRef:      { type: 'string', description: 'Target ref or commit (requires fromRef)' },
          maxChars:   { type: 'number', description: 'Max characters to return (default 8000). Increase for large diffs.', default: 8000 },
          charOffset: { type: 'number', description: 'Skip this many characters from the start (for paging large diffs)', default: 0 },
        },
      },
    },
    // ── Combined context (jira + bitbucket, or either alone) ─────────────
    ...(jira || bitbucket ? [{
      name: 'get_dev_context',
      description: 'Master entry point. Use when asked "what am I working on?", "what\'s the status?", "show me the context", or before any review or coding task. Returns: git branch + upstream state, Jira ticket overview (status, transitions, sprint, comments), open PR with reviewer approvals, and actionable next-step hints (create PR, merge, address blockers).',
      inputSchema: {
        type: 'object',
        properties: {
          repoPath: { type: 'string', description: 'Local path to the git repo (defaults to cwd)' },
        },
      },
    }] : []),
    // ── Jira ──────────────────────────────────────────────────────────────
    ...(jira ? [
    {
      name: 'start_work',
      description: 'Start working on a Jira ticket: fetches the ticket, creates a local git branch with an auto-generated name (e.g. feature/FOO-123-add-payment-gateway), and optionally transitions the ticket. Use when told "make a branch for FOO-123", "start working on this ticket", "check out a branch for this issue", or "begin work on FOO-123".',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey:       { type: 'string', description: 'Jira issue key, e.g. FOO-123' },
          repoPath:       { type: 'string', description: 'Local repo path (defaults to cwd)' },
          baseBranch:     { type: 'string', description: 'Branch to base off (default: master)' },
          branchName:     { type: 'string', description: 'Override the generated branch name' },
          transitionName: { type: 'string', description: 'Jira transition to apply, e.g. "In Progress" (optional)' },
          push:           { type: 'boolean', description: 'Push branch to remote after creation (default false)', default: false },
        },
        required: ['issueKey'],
      },
    },
    {
      name: 'jira_search',
      description: 'Discover Jira resources. Use when asked "find tickets for...", "what\'s in the backlog", "show me my issues", "list projects", or "which board is for project X". Set resource:\n• "issues" (default) — search by text, JQL, project, status, assignee, issue type, or mine=true for your queue\n• "projects" — list all projects and their keys\n• "issue_types" — valid types and statuses for a project\n• "boards" — list boards (pass project to filter by project key); use this to find the boardId before fetching sprints or board_overview\n• "sprints" — sprints for a board (pass boardId); if you don\'t know the boardId, first use resource=boards\n• "board_overview" — active/future sprints with their issues for a board (pass boardId); use when asked "what\'s in the sprint", "show me the board", or "what\'s everyone working on"\n• "users" — find users by name/email (pass query)',
      inputSchema: {
        type: 'object',
        properties: {
          resource:        { type: 'string', enum: ['issues', 'projects', 'issue_types', 'boards', 'sprints', 'board_overview', 'users'], description: 'What to search (default: issues)' },
          mine:            { type: 'boolean', description: 'Return issues assigned to you (resource=issues only)' },
          query:           { type: 'string', description: 'Text search or user name query' },
          jql:             { type: 'string', description: 'Raw JQL (resource=issues only, overrides other filters)' },
          project:         { type: 'string', description: 'Project key filter or scope for issue_types/boards' },
          status:          { type: 'string', description: 'Status filter (issues only, or board_overview to filter issues by status)' },
          assignee:        { type: 'string', description: 'Assignee username filter (issues only, or board_overview to filter issues by assignee)' },
          issueType:       { type: 'string', description: 'Issue type filter (issues only)' },
          boardId:         { type: 'number', description: 'Board ID (required for resource=sprints or board_overview)' },
          sprintState:     { type: 'string', description: 'Sprint state filter: active, future, closed (sprints and board_overview)' },
          includeIssues:   { type: 'boolean', description: 'Include issues per sprint in board_overview (default true)', default: true },
          maxResults:      { type: 'number', description: 'Max results (default 20)', default: 20 },
          startAt:         { type: 'number', description: 'Pagination offset (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'jira_get',
      description: 'Full details for one Jira issue: summary, description, status, assignee, sprint, available transitions, and recent comments. Use when asked to "show me FOO-123", "what does this ticket say", "get the details for this issue", or after discovering a key from get_dev_context or jira_search.',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey:           { type: 'string', description: 'Jira issue key, e.g. FOO-123' },
          includeComments:    { type: 'boolean', description: 'Include comments (default true)', default: true },
          commentsMaxResults: { type: 'number', description: 'Max comments (default 10)', default: 10 },
          commentsStartAt:    { type: 'number', description: 'Comment pagination offset (default 0)', default: 0 },
          includeTransitions: { type: 'boolean', description: 'Include available transitions (default true)', default: true },
          includeSprint:      { type: 'boolean', description: 'Include sprint data (default true)', default: true },
        },
        required: ['issueKey'],
      },
    },
    {
      name: 'jira_mutate',
      description: `Use when asked to "create a ticket", "log a bug", "move FOO-123 to In Progress", "close this issue", "assign to X", "add a comment on FOO-123", "FOO-123 blocks BAR-456", "log 2h on this ticket", or "add a sub-task". Bundles create/update/transition/comment/link/worklog in one call. ${JIRA_WIKI_MARKUP_HINT}`,
      inputSchema: {
        type: 'object',
        properties: {
          issueKey: { type: 'string', description: 'Existing issue key to mutate (optional if create is provided)' },
          create: {
            type: 'object',
            properties: {
              projectKey:  { type: 'string', description: 'Jira project code (optional, auto-resolved when omitted)' },
              project:     { type: 'string', description: 'Alias for projectKey' },
              issueType:   { type: 'string', description: 'Issue type name, e.g. Bug, Story, Task, Sub-task' },
              summary:     { type: 'string', description: 'Issue title' },
              description: { type: 'string', description: `Issue description (optional). ${JIRA_WIKI_MARKUP_HINT}` },
              assignee:    { type: 'string', description: 'Username to assign to (optional)' },
              priority:    { type: 'string', description: 'Priority name (optional)' },
              labels:      { type: 'array', items: { type: 'string' }, description: 'Labels to apply (optional)' },
              fixVersion:  { type: 'string', description: 'Fix version name (optional)' },
              parent:      { type: 'string', description: 'Parent issue key for subtasks (optional)' },
            },
            required: ['issueType', 'summary'],
          },
          update: {
            type: 'object',
            properties: {
              summary:     { type: 'string', description: 'New summary (optional)' },
              description: { type: 'string', description: `New description (optional). ${JIRA_WIKI_MARKUP_HINT}` },
              assignee:    { type: 'string', description: 'New assignee username, or empty string to unassign (optional)' },
              priority:    { type: 'string', description: 'New priority name (optional)' },
              labels:      { type: 'array', items: { type: 'string' }, description: 'Replace label set (pass [] to clear)' },
              fixVersion:  { type: 'string', description: 'Fix version name, or empty string to clear (optional)' },
            },
          },
          sprintId:        { type: 'number', description: 'Sprint ID to add the issue into (optional)' },
          removeFromSprint:{ type: 'boolean', description: 'Move the issue to the backlog (remove from any sprint)' },
          transitionId:    { type: 'string', description: 'Transition ID (optional if transitionName provided)' },
          transitionName:  { type: 'string', description: 'Transition name, e.g. "In Progress" (optional if transitionId provided)' },
          comment:         { type: 'string', description: `Comment to add after other mutations (optional). ${JIRA_WIKI_MARKUP_HINT}` },
          link: {
            type: 'object',
            description: 'Create an issue link, e.g. "FOO-123 blocks BAR-456"',
            properties: {
              linkType:      { type: 'string', description: 'Link type name, e.g. "Blocks", "Relates to", "Duplicates"' },
              targetIssueKey:{ type: 'string', description: 'The other issue in the relationship' },
              direction:     { type: 'string', enum: ['outward', 'inward'], description: 'outward (default): issueKey → target; inward: target → issueKey' },
            },
            required: ['linkType', 'targetIssueKey'],
          },
          worklog: {
            type: 'object',
            description: 'Log time spent on this issue',
            properties: {
              timeSpent: { type: 'string', description: 'Time in Jira format, e.g. "2h 30m" or "1d"' },
              comment:   { type: 'string', description: 'Work description (optional)' },
              started:   { type: 'string', description: 'ISO 8601 datetime when work started (defaults to now)' },
            },
            required: ['timeSpent'],
          },
        },
      },
    },
    {
      name: 'jira_comment',
      description: `Add, update, or delete a comment on a Jira issue. Use when asked to "edit my comment on FOO-123", "delete comment 12345", or "update that comment". action defaults to "add". Can only edit/delete your own comments. ${JIRA_WIKI_MARKUP_HINT}`,
      inputSchema: {
        type: 'object',
        properties: {
          action:    { type: 'string', enum: ['add', 'update', 'delete'], description: 'Operation (default: add)' },
          issueKey:  { type: 'string', description: 'Jira issue key, e.g. FOO-123' },
          commentId: { type: 'string', description: 'Comment ID (required for update/delete)' },
          body:      { type: 'string', description: `Comment text. ${JIRA_WIKI_MARKUP_HINT} Required for add/update.` },
        },
        required: ['issueKey'],
      },
    }] : []),
    ...(bitbucket ? [{
      name: 'bitbucket_search',
      description: 'Discover Bitbucket resources. Use when asked "what PRs are open?", "show me the repos", "find the PR for this branch", or "list branches". Set resource:\n• "pull_requests" (default) — list PRs by state/branch/text; mine=true for your inbox\n• "repos" — list repositories in a project\n• "branches" — list or filter branches in a repo\n• "users" — find users by name/email (pass query); add projectKey+repoSlug to restrict to users with repo access. ALWAYS use this to look up valid usernames before adding reviewers to a PR.',
      inputSchema: {
        type: 'object',
        properties: {
          resource:   { type: 'string', enum: ['pull_requests', 'repos', 'branches', 'users'], description: 'What to search (default: pull_requests)' },
          mine:       { type: 'boolean', description: 'Return your own PRs by role (resource=pull_requests only)' },
          role:       { type: 'string', enum: ['author', 'reviewer', 'participant'], description: 'Your role filter when mine=true' },
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG"' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          query:      { type: 'string', description: 'Name or email filter (resource=users only)' },
          state:      { type: 'string', enum: ['OPEN', 'MERGED', 'DECLINED'], description: 'PR state filter (default OPEN)' },
          fromBranch: { type: 'string', description: 'Filter PRs from this source branch' },
          text:       { type: 'string', description: 'Filter PRs by title/description text' },
          filter:     { type: 'string', description: 'Branch name filter (resource=branches only)' },
          limit:      { type: 'number', description: 'Max results per page (default 25)', default: 25 },
          start:      { type: 'number', description: 'Pagination offset (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'bitbucket_get_pr',
      description: 'Full details for one PR: metadata, commits, open comments, blockers, and optional diff. Use when asked to "review this PR", "show me the review comments", "what\'s blocking the merge", or after get_dev_context surfaces a prId. IMPORTANT: The PR branch is often not the locally checked-out branch. Do NOT read files with local tools (Read, git_get_diff, etc.) for PR context — use bitbucket_get_file with the PR\'s source branch instead. The response includes a "Viewing as" line — if it says "you are the author", do NOT add review comments or a summary unless explicitly asked; just answer questions about the PR. If it says "you are a reviewer", default to posting inline comments for suggested changes and a final summary comment.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:        { type: 'string', description: 'Bitbucket project code (usually auto-detected)' },
          project:           { type: 'string', description: 'Alias for projectKey' },
          repoSlug:          { type: 'string', description: 'Repository slug (usually auto-detected)' },
          repo:              { type: 'string', description: 'Alias for repoSlug' },
          prId:              { type: 'number', description: 'Pull request number (optional if fromBranch provided or running from a checked-out branch)' },
          fromBranch:        { type: 'string', description: 'Source branch — auto-resolves the open PR; omit to use current checked-out branch' },
          includeCommits:    { type: 'boolean', description: 'Include commit list (default true)', default: true },
          includeComments:   { type: 'boolean', description: 'Include review comments and blockers (default true)', default: true },
          includeDiff:       { type: 'boolean', description: 'Include diff text (default false)', default: false },
          includeBuildStatus:{ type: 'boolean', description: 'Include CI/build status for the head commit (default true)', default: true },
          commentsState:     { type: 'string', enum: ['OPEN', 'RESOLVED', 'PENDING'], description: 'Comment state filter (default OPEN)', default: 'OPEN' },
          commentsSeverity:  { type: 'string', enum: ['ALL', 'NORMAL', 'BLOCKER'], description: 'Comment severity filter (default ALL)', default: 'ALL' },
          commentsLimit:     { type: 'number', description: 'Max comments (default 50)', default: 50 },
          commentsStart:     { type: 'number', description: 'Comment pagination offset (default 0)', default: 0 },
          commitsLimit:      { type: 'number', description: 'Max commits (default 25)', default: 25 },
          diffMaxChars:      { type: 'number', description: 'Max diff chars when includeDiff=true (default 8000)', default: 8000 },
        },
      },
    },
    {
      name: 'bitbucket_mutate',
      description: 'Use when asked to "open a PR for this branch", "create a pull request", "approve this PR", "merge it", "ship it", or "decline this PR". Auto-targets the open PR for the current branch when prId is omitted. Handles create, update, approve/unapprove, decline, and merge in one call.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:    { type: 'string', description: 'Bitbucket project code (usually auto-detected)' },
          project:       { type: 'string', description: 'Alias for projectKey' },
          repoSlug:      { type: 'string', description: 'Repository slug (usually auto-detected)' },
          repo:          { type: 'string', description: 'Alias for repoSlug' },
          prId:          { type: 'number', description: 'Target PR number (optional, auto-resolved from branch)' },
          action:        { type: 'string', enum: ['approve', 'unapprove', 'decline', 'merge'], description: 'Lifecycle action to perform (optional)' },
          mergeStrategy: { type: 'string', enum: ['MERGE_COMMIT', 'SQUASH', 'FAST_FORWARD'], description: 'Merge strategy (action=merge only)' },
          mergeMessage:  { type: 'string', description: 'Custom merge commit message (action=merge only)' },
          declineMessage:{ type: 'string', description: 'Decline message (action=decline only)' },
          create: {
            type: 'object',
            properties: {
              title:       { type: 'string', description: 'PR title' },
              description: { type: 'string', description: 'PR description (optional)' },
              fromBranch:  { type: 'string', description: 'Source branch (defaults to current branch)' },
              toBranch:    { type: 'string', description: 'Target branch (default: master)' },
              reviewers:   { type: 'array', items: { type: 'string' }, description: 'Reviewer usernames. Use bitbucket_search resource=users to look up valid usernames before setting this.' },
            },
            required: ['title'],
          },
          update: {
            type: 'object',
            properties: {
              title:       { type: 'string', description: 'Updated PR title (optional)' },
              description: { type: 'string', description: 'Updated description, or empty string to clear (optional)' },
              toBranch:    { type: 'string', description: 'Updated target branch (optional)' },
              reviewers:   { type: 'array', items: { type: 'string' }, description: 'Updated reviewer usernames. Empty array clears reviewers. Use bitbucket_search resource=users to look up valid usernames before setting this.' },
            },
          },
        },
      },
    },
    {
      name: 'bitbucket_comment',
      description: `Add, update, or delete a PR comment. action defaults to "add". For code changes, ALWAYS use inline comments with suggestion when exact replacement code is available. Keep any explanatory text before the suggestion block only (never after), or Bitbucket may hide Apply suggestion. Replies MUST use commentId. Keep comments concise, no emojis. Only call proactively (without being asked) when you are a reviewer on the PR (i.e. "Viewing as" says "you are a reviewer") — never post unsolicited comments on PRs you authored.`,
      inputSchema: {
        type: 'object',
        properties: {
          action:                 { type: 'string', enum: ['add', 'update', 'delete'], description: 'Operation (default: add)' },
          projectKey:             { type: 'string', description: 'Bitbucket project code (usually auto-detected)' },
          project:                { type: 'string', description: 'Alias for projectKey' },
          repoSlug:               { type: 'string', description: 'Repository slug (usually auto-detected)' },
          repo:                   { type: 'string', description: 'Alias for repoSlug' },
          prId:                   { type: 'number', description: 'Pull request number' },
          commentId:              { type: 'number', description: 'Comment ID to reply to, update, or delete' },
          text:                   { type: 'string', description: 'Comment text for add/update. No filler, no emojis. If suggestion is used, keep this optional and brief; it is placed before the suggestion block.' },
          filePath:               { type: 'string', description: 'File path for inline comment (must pair with line)' },
          srcPath:                { type: 'string', description: 'Source path if file was renamed (optional, defaults to filePath)' },
          line:                   { type: 'number', description: 'Line number to anchor inline comment (must pair with filePath)' },
          lineType:               { type: 'string', enum: ['ADDED', 'REMOVED', 'CONTEXT'], description: 'Diff line type (default ADDED)' },
          fileType:               { type: 'string', enum: ['TO', 'FROM'], description: 'Diff side: TO (new, default) or FROM (old)' },
          multilineStartLine:     { type: 'number', description: 'First line of multiline anchor (pair with line as last line)' },
          multilineStartLineType: { type: 'string', enum: ['ADDED', 'REMOVED', 'CONTEXT'], description: 'Line type for multilineStartLine' },
          suggestion:             { type: 'string', description: 'Replacement code to suggest. Use whenever proposing a concrete code change. Posted as the final ```suggestion``` block so Apply suggestion appears. Requires filePath + line.' },
          state:                  { type: 'string', enum: ['OPEN', 'RESOLVED'], description: 'Task state for BLOCKER comments (update only)' },
          threadResolved:         { type: 'boolean', description: 'Resolve/reopen normal comment thread (update only)' },
          severity:               { type: 'string', enum: ['NORMAL', 'BLOCKER'], description: 'Comment severity. BLOCKER = checklist task.' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_get_file',
      description: 'Raw file content from Bitbucket at a branch, tag, or commit. CRITICAL: if the PR branch being reviewed is NOT the currently checked-out local branch, ALL additional file context for that review MUST come from this tool — never from local Read, git_get_diff, or any tool that reads local disk. Pass the PR source branch as ref.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          path:       { type: 'string', description: 'File path, e.g. "src/index.ts"' },
          ref:        { type: 'string', description: 'Branch, tag, or commit hash (defaults to default branch)' },
        },
        required: ['path'],
      },
    },
    {
      name: 'bitbucket_pr_tasks',
      description: 'Manage PR tasks (checklist items). Use when asked to "list the tasks on this PR", "create a task for FOO-123", "mark task #5 as done", or "add a checklist item". Tasks are distinct from comments — they appear as a checklist in the PR sidebar.',
      inputSchema: {
        type: 'object',
        properties: {
          action:     { type: 'string', enum: ['list', 'create', 'resolve', 'reopen', 'delete'], description: 'Operation (default: list)' },
          projectKey: { type: 'string', description: 'Bitbucket project code (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          prId:       { type: 'number', description: 'Pull request number' },
          taskId:     { type: 'number', description: 'Task ID (required for resolve/reopen/delete)' },
          text:       { type: 'string', description: 'Task description (required for create)' },
          commentId:  { type: 'number', description: 'Anchor the task to a specific comment ID (optional for create)' },
        },
        required: ['prId'],
      },
    },
] : []),
    // ── Combined workflow ─────────────────────────────────────────────────
    ...(jira && bitbucket ? [{
      name: 'complete_work',
      description: 'Close the loop on a finished branch: merges the open PR and transitions the Jira ticket to Done (or a named transition). Use when asked to "ship this", "close out FOO-123", "merge and close the ticket", or "done with this branch". Mirrors start_work.',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey:          { type: 'string', description: 'Jira issue key to transition (auto-detected from branch name if omitted)' },
          prId:              { type: 'number', description: 'PR to merge (auto-detected from current branch if omitted)' },
          repoPath:          { type: 'string', description: 'Local repo path (defaults to cwd)' },
          transitionName:    { type: 'string', description: 'Jira transition to apply after merge (default: "Done")' },
          mergeStrategy:     { type: 'string', enum: ['MERGE_COMMIT', 'SQUASH', 'FAST_FORWARD'], description: 'Merge strategy (optional)' },
          mergeMessage:      { type: 'string', description: 'Custom merge commit message (optional)' },
          projectKey:        { type: 'string', description: 'Bitbucket project code (usually auto-detected)' },
          repoSlug:          { type: 'string', description: 'Repository slug (usually auto-detected)' },
          skipJiraTransition:{ type: 'boolean', description: 'Skip transitioning the Jira ticket (default false)' },
        },
      },
    }] : [])
  ],
}));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const { name, arguments: args = {} } = request.params;
  try {
    switch (name) {
      // Git
      case 'git_get_context':
        return getContext(args as Parameters<typeof getContext>[0]);
      case 'git_get_diff': {
        const diffArgs = args as Parameters<typeof getDiff>[0] & { maxChars?: number; charOffset?: number };
        const result = getDiff(diffArgs);
        const raw = result.content[0].text;
        const offset = diffArgs.charOffset ?? 0;
        const limit = diffArgs.maxChars ?? 8000;
        if (offset === 0 && raw.length <= limit) return result;
        const chunk = raw.slice(offset, offset + limit);
        const remaining = raw.length - offset - chunk.length;
        const suffix = remaining > 0 ? `\n\n... (${remaining} more chars, use charOffset=${offset + chunk.length})` : '';
        return { content: [{ type: 'text', text: chunk + suffix }] };
      }
      // Combined context + workflow
      case 'get_dev_context':
        if (!jira && !bitbucket) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        return await getDevContext(args as { repoPath?: string }, jira, bitbucket);
      case 'start_work': {
        if (!jira) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        const a = args as {
          issueKey: string; repoPath?: string; baseBranch?: string;
          branchName?: string; transitionName?: string; push?: boolean;
        };
        const fields = await jira.getIssueFields(a.issueKey);
        const branchName = a.branchName ?? slugifyBranchName(a.issueKey, fields.summary, fields.type);
        const repoPath = a.repoPath ?? process.cwd();

        // Check if branch already exists on remote before creating
        const remote = checkRemoteBranch(branchName, repoPath);
        if (remote.exists) {
          const authorLine = remote.author ? `Last author: ${remote.author}` : null;
          const commitLine = remote.date
            ? `Last commit: ${remote.date} — ${remote.message ?? ''}${remote.sha ? ` (${remote.sha})` : ''}`
            : null;
          const contextLines = [authorLine, commitLine].filter(Boolean).join('\n');

          const message = [
            `Branch "${branchName}" already exists on remote.`,
            `Ticket: ${a.issueKey} — ${fields.summary}`,
            contextLines,
          ].filter(Boolean).join('\n');

          try {
            const result = await server.elicitInput({
              message,
              requestedSchema: {
                type: 'object',
                properties: {
                  action: {
                    type: 'string',
                    title: 'What would you like to do?',
                    oneOf: [
                      { const: 'checkout', title: `Check out existing branch "${branchName}"` },
                      { const: 'new_name', title: 'Use a different branch name (re-run start_work with branchName)' },
                      { const: 'cancel',   title: 'Cancel' },
                    ],
                  },
                },
                required: ['action'],
              },
            });

            if (result.action === 'cancel' || result.action === 'decline') {
              return { content: [{ type: 'text', text: 'Cancelled.' }] };
            }
            if (result.action === 'accept') {
              const chosen = result.content?.action;
              if (chosen === 'checkout') {
                const checkout = checkoutRemoteBranch(branchName, repoPath);
                return { content: [{ type: 'text', text: `${message}\n\n${checkout.content[0].text}` }] };
              }
              if (chosen === 'cancel') {
                return { content: [{ type: 'text', text: 'Cancelled.' }] };
              }
              // new_name — instruct the model to re-run with a custom name
              return {
                content: [{
                  type: 'text',
                  text: `${message}\n\nRe-run start_work with a custom branchName to proceed.`,
                }],
              };
            }
            // Fallback: unknown action
            return { content: [{ type: 'text', text: 'Cancelled.' }] };
          } catch {
            // Client doesn't support elicitation — fall back to informational text
            return {
              content: [{
                type: 'text',
                text: [
                  message,
                  '',
                  'Options:',
                  `  • Check out existing: git checkout --track origin/${branchName}`,
                  `  • Use a different name: re-run start_work with branchName set`,
                ].join('\n'),
              }],
            };
          }
        }

        const branchResult = createBranch({
          branchName,
          baseBranch: a.baseBranch,
          repoPath,
          push: a.push ?? false,
        });
        const lines = [
          `Ticket:  ${a.issueKey} — ${fields.summary}`,
          `Status:  ${fields.status}`,
          branchResult.content[0].text,
        ];
        if (a.transitionName) {
          try {
            await jira.mutateIssue({ issueKey: a.issueKey, transitionName: a.transitionName });
            lines.push(`Jira:    transitioned → ${a.transitionName}`);
          } catch (err) {
            lines.push(`Jira:    could not transition — ${(err as Error).message}`);
          }
        }
        if (bitbucket) lines.push(``, `Next: push commits then use bitbucket_mutate to open a PR.`);
        return { content: [{ type: 'text', text: lines.join('\n') }] };
      }
      // Jira
      case 'jira_search': {
        if (!jira) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        const a = args as {
          resource?: string; mine?: boolean; query?: string; jql?: string;
          project?: string; projectKey?: string; status?: string; assignee?: string;
          issueType?: string; boardId?: number; sprintState?: string;
          maxResults?: number; startAt?: number;
        };
        const resource = a.resource ?? 'issues';
        if (resource === 'projects')     return await jira.getProjects({ maxResults: a.maxResults });
        if (resource === 'issue_types')  return await jira.getIssueTypes({ projectKey: a.projectKey ?? a.project });
        if (resource === 'boards')       return await jira.getBoards({ projectKey: a.projectKey ?? a.project, maxResults: a.maxResults, startAt: a.startAt });
        if (resource === 'sprints')       return await jira.getSprints({ boardId: a.boardId!, state: a.sprintState, maxResults: a.maxResults, startAt: a.startAt });
        if (resource === 'board_overview') return await jira.boardOverview({ boardId: a.boardId!, sprintState: a.sprintState, sprintMaxResults: a.maxResults, sprintStartAt: a.startAt, includeIssues: (a as { includeIssues?: boolean }).includeIssues, assignee: a.assignee, status: a.status });
        if (resource === 'users')         return await jira.searchUsers({ query: a.query ?? '', maxResults: a.maxResults });
        // issues (default)
        if (a.mine) return await jira.myIssues({ maxResults: a.maxResults, startAt: a.startAt });
        return await jira.searchIssues(a as Parameters<typeof jira.searchIssues>[0]);
      }
      case 'jira_get':
        if (!jira) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        return await jira.issueOverview(args as Parameters<typeof jira.issueOverview>[0]);
      case 'jira_mutate':
        if (!jira) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        return await jira.mutateIssue(normalizeJiraMutateArgs(args) as Parameters<typeof jira.mutateIssue>[0]);
      case 'jira_comment': {
        if (!jira) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        const a = normalizeJiraProjectArgs(args) as { action?: string; issueKey: string; commentId?: string; body?: string };
        const action = a.action ?? 'add';
        if (action === 'update') return await jira.editComment({ issueKey: a.issueKey, commentId: a.commentId!, body: a.body! });
        if (action === 'delete') return await jira.deleteComment({ issueKey: a.issueKey, commentId: a.commentId! });
        return await jira.addComment({ issueKey: a.issueKey, body: a.body! });
      }
      // Bitbucket
      case 'bitbucket_search': {
        if (!bitbucket) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        const a = normalizeBitbucketArgs(args) as {
          resource?: string; mine?: boolean; role?: string;
          projectKey?: string; repoSlug?: string; state?: string;
          fromBranch?: string; text?: string; filter?: string; query?: string;
          limit?: number; start?: number;
        };
        const resource = a.resource ?? 'pull_requests';
        if (resource === 'repos')    return await bitbucket.listRepos(a as Parameters<typeof bitbucket.listRepos>[0]);
        if (resource === 'branches') return await bitbucket.getBranches(a as Parameters<typeof bitbucket.getBranches>[0]);
        if (resource === 'users')    return await bitbucket.searchUsers({ projectKey: a.projectKey, repoSlug: a.repoSlug, query: a.query, limit: a.limit, start: a.start });
        // pull_requests (default)
        if (a.mine) return await bitbucket.myPrs({ limit: a.limit, start: a.start, role: a.role as 'author' | 'reviewer' | 'participant' | undefined });
        return await bitbucket.listPullRequests(a as Parameters<typeof bitbucket.listPullRequests>[0]);
      }
      case 'bitbucket_get_pr':
        if (!bitbucket) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        return await bitbucket.getPrOverview(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.getPrOverview>[0]);
      case 'bitbucket_mutate': {
        if (!bitbucket) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        const a = normalizeBitbucketArgs(args) as { action?: string; mergeMessage?: string; declineMessage?: string; [k: string]: unknown };
        const action = a.action as string | undefined;
        if (action === 'approve')   return await bitbucket.approvePr(a as Parameters<typeof bitbucket.approvePr>[0]);
        if (action === 'unapprove') return await bitbucket.unapprovePr(a as Parameters<typeof bitbucket.unapprovePr>[0]);
        if (action === 'decline')   return await bitbucket.declinePr({ ...a, message: a.declineMessage } as Parameters<typeof bitbucket.declinePr>[0]);
        if (action === 'merge')     return await bitbucket.mergePr({ ...a, message: a.mergeMessage, mergeStrategy: a.mergeStrategy as string | undefined } as Parameters<typeof bitbucket.mergePr>[0]);
        return await bitbucket.mutatePullRequest(a as Parameters<typeof bitbucket.mutatePullRequest>[0]);
      }
      case 'bitbucket_comment': {
        if (!bitbucket) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        const a = normalizeBitbucketArgs(args) as { action?: string; [k: string]: unknown };
        const action = a.action ?? 'add';
        if (action === 'update') return await bitbucket.updatePrComment(a as Parameters<typeof bitbucket.updatePrComment>[0]);
        if (action === 'delete') return await bitbucket.deletePrComment(a as Parameters<typeof bitbucket.deletePrComment>[0]);
        return await bitbucket.addPrComment(a as Parameters<typeof bitbucket.addPrComment>[0]);
      }
      case 'bitbucket_get_file':
        if (!bitbucket) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        return await bitbucket.getFile(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.getFile>[0]);
      case 'bitbucket_pr_tasks': {
        if (!bitbucket) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        const a = normalizeBitbucketArgs(args) as { action?: string; prId: number; taskId?: number; text?: string; commentId?: number; [k: string]: unknown };
        const action = (a.action ?? 'list') as string;
        if (action === 'list') return await bitbucket.getPrTasks(a as Parameters<typeof bitbucket.getPrTasks>[0]);
        return await bitbucket.mutatePrTask({ ...a, action: action as 'create' | 'resolve' | 'reopen' | 'delete' });
      }
      case 'complete_work': {
        if (!jira || !bitbucket) throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
        const a = args as {
          issueKey?: string; prId?: number; repoPath?: string;
          transitionName?: string; mergeStrategy?: string; mergeMessage?: string;
          projectKey?: string; repoSlug?: string; skipJiraTransition?: boolean;
        };
        const repoPath = a.repoPath ?? process.cwd();
        const lines: string[] = [];

        // Resolve PR — by prId or current branch
        let resolvedPrId = a.prId;
        if (resolvedPrId === undefined) {
          const branch = (() => { try { return execFileSync('git', ['rev-parse', '--abbrev-ref', 'HEAD'], { cwd: repoPath, encoding: 'utf-8' }).trim(); } catch { return ''; } })();
          if (!branch || branch === 'HEAD') {
            throw new Error('Could not determine current branch. Provide prId or run from a checked-out branch.');
          }
          const remote = (() => { try { return execFileSync('git', ['remote', 'get-url', 'origin'], { cwd: repoPath, encoding: 'utf-8' }).trim(); } catch { return ''; } })();
          const parsed = parseBitbucketRemote(remote);
          if (!parsed) throw new Error('Could not parse Bitbucket remote URL. Provide projectKey/repoSlug explicitly.');
          const projectKey = a.projectKey ?? parsed.projectKey;
          const repoSlug = a.repoSlug ?? parsed.repoSlug;
          const pr = await bitbucket.findOpenPrForBranch(projectKey, repoSlug, branch);
          if (!pr) throw new Error(`No open PR found for branch "${branch}". Provide prId explicitly.`);
          resolvedPrId = pr.id;
          lines.push(`Branch:  ${branch} → PR #${resolvedPrId}`);

          // Auto-detect Jira issue key from branch if not provided
          if (!a.issueKey) {
            const JIRA_KEY_RE = /\b([A-Z][A-Z0-9]+-\d+)\b/;
            const match = branch.match(JIRA_KEY_RE);
            if (match) {
              (a as { issueKey?: string }).issueKey = match[1];
              lines.push(`Jira:    auto-detected ${a.issueKey} from branch name`);
            }
          }
        }

        // Merge the PR
        const mergeResult = await bitbucket.mergePr({
          prId: resolvedPrId,
          projectKey: a.projectKey,
          repoSlug: a.repoSlug,
          mergeStrategy: a.mergeStrategy as 'MERGE_COMMIT' | 'SQUASH' | 'FAST_FORWARD' | undefined,
          message: a.mergeMessage,
        });
        lines.push(mergeResult.content[0].text);

        // Transition Jira ticket
        if (!a.skipJiraTransition && a.issueKey) {
          const transitionName = a.transitionName ?? 'Done';
          try {
            await jira.mutateIssue({ issueKey: a.issueKey, transitionName });
            lines.push(`Jira:    ${a.issueKey} transitioned → ${transitionName}`);
          } catch (err) {
            lines.push(`Jira:    could not transition ${a.issueKey} — ${(err as Error).message}`);
          }
        } else if (!a.skipJiraTransition) {
          lines.push('Jira:    no issue key — skipped transition (provide issueKey to transition)');
        }

        return { content: [{ type: 'text', text: lines.join('\n') }] };
      }
      default:
        throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
    }
  } catch (err) {
    if (err instanceof McpError) throw err;
    return {
      content: [{ type: 'text', text: `Error: ${(err as Error).message}` }],
      isError: true,
    };
  }
});

async function shutdown() {
  await server.close();
  process.exit(0);
}
process.on('SIGINT', shutdown);
process.on('SIGTERM', shutdown);

const transport = new StdioServerTransport();
await server.connect(transport);
