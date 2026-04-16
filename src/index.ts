#!/usr/bin/env node
import 'dotenv/config';
import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
  McpError,
  ErrorCode,
} from '@modelcontextprotocol/sdk/types.js';
import { loadConfig } from './config.js';
import { JiraClient } from './jira.js';
import { BitbucketClient } from './bitbucket.js';
import { getContext, getCommits, getDiff } from './git.js';
import { getDevContext } from './context.js';

const config = loadConfig();
const jira = new JiraClient(config.jira.url, config.jira.token);
const bitbucket = new BitbucketClient(config.bitbucket.url, config.bitbucket.token);

const server = new Server(
  { name: 'atlassian-mcp', version: '1.0.0' },
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

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    // ── Context ───────────────────────────────────────────────────────────
    {
      name: 'get_dev_context',
      description: 'Use when you want one quick snapshot before coding or reviewing: current git branch/status, Jira tickets detected from branch name, and the open Bitbucket PR for that branch. For review requests, use this first to resolve the branch and PR without assuming it is in your personal inbox.',
      inputSchema: {
        type: 'object',
        properties: {
          repoPath: { type: 'string', description: 'Local path to the git repo (defaults to current working directory)' },
        },
      },
    },
    // ── Jira ──────────────────────────────────────────────────────────────
    {
      name: 'jira_search_issues',
      description: 'Use when you want to find Jira tickets by natural language, keywords, or filters. Supports plain text search and advanced JQL.',
      inputSchema: {
        type: 'object',
        properties: {
          query:      { type: 'string', description: 'Plain-text ticket search across summary, description, and comments' },
          jql:        { type: 'string', description: 'Raw JQL query (overrides other filters when provided)' },
          project:    { type: 'string', description: 'Project key filter, for example "FOO"' },
          status:     { type: 'string', description: 'Status filter, for example "In Progress"' },
          assignee:   { type: 'string', description: 'Assignee username filter' },
          issueType:  { type: 'string', description: 'Issue type filter, for example "Bug" or "Story"' },
          maxResults: { type: 'number', description: 'Max results per page (default 20)', default: 20 },
          startAt:    { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'jira_my_issues',
      description: 'Use when you want your Jira work queue: tickets assigned to you, newest updates first.',
      inputSchema: {
        type: 'object',
        properties: {
          maxResults: { type: 'number', description: 'Max results per page (default 20)', default: 20 },
          startAt:    { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'jira_get_projects',
      description: 'Use when you need available Jira projects and project codes before creating or searching tickets.',
      inputSchema: {
        type: 'object',
        properties: {
          maxResults: { type: 'number', description: 'Max results (default 50)', default: 50 },
        },
      },
    },
    {
      name: 'jira_get_issue_types',
      description: 'Use when preparing to create tickets and you need valid issue types and statuses. If projectKey/project is omitted, the server auto-picks from branch context or asks you to choose.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Jira project code, e.g. "PAY" from PAY-123 (optional)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
        },
      },
    },
    {
      name: 'jira_get_sprints',
      description: 'Use when you need sprint IDs for planning or assignment. Returns sprints for a Jira board.',
      inputSchema: {
        type: 'object',
        properties: {
          boardId:    { type: 'number', description: 'Jira board ID' },
          state:      { type: 'string', description: 'Optional sprint state filter, e.g. "active", "future", or "closed"' },
          maxResults: { type: 'number', description: 'Max sprints per page (default 20)', default: 20 },
          startAt:    { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
        required: ['boardId'],
      },
    },
    {
      name: 'jira_search_users',
      description: 'Use when assigning tickets and you need to find the correct Jira username by name or email.',
      inputSchema: {
        type: 'object',
        properties: {
          query:      { type: 'string', description: 'Name or username to search for' },
          maxResults: { type: 'number', description: 'Max results (default 10)', default: 10 },
        },
        required: ['query'],
      },
    },
    {
      name: 'jira_get_issue',
      description: 'Use when you want full details for a specific Jira ticket by key (for example FOO-123).',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey: { type: 'string', description: 'Jira issue key, e.g. "FOO-123"' },
        },
        required: ['issueKey'],
      },
    },
    {
      name: 'jira_issue_overview',
      description: 'Use when you want one Jira issue snapshot in a single call: details, transitions, sprint context, and optional comments.',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey:          { type: 'string', description: 'Jira issue key, e.g. "FOO-123"' },
          includeComments:   { type: 'boolean', description: 'Include comments in the overview (default true)', default: true },
          commentsMaxResults:{ type: 'number', description: 'Max comments when includeComments=true (default 10)', default: 10 },
          commentsStartAt:   { type: 'number', description: 'Comment pagination offset (default 0)', default: 0 },
          includeTransitions:{ type: 'boolean', description: 'Include available transitions (default true)', default: true },
          includeSprint:     { type: 'boolean', description: 'Include sprint data via Jira Agile API (default true)', default: true },
        },
        required: ['issueKey'],
      },
    },
    {
      name: 'jira_create_issue',
      description: `Use when you want to create a new Jira ticket (bug, story, task, etc.). If projectKey/project is omitted, the server auto-picks from branch context or asks you to choose. ${JIRA_WIKI_MARKUP_HINT}`,
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:  { type: 'string', description: 'Jira project code, e.g. "PAY" from PAY-123 (optional)' },
          project:     { type: 'string', description: 'Alias for projectKey' },
          issueType:   { type: 'string', description: 'Issue type name, for example "Bug", "Story", or "Task"' },
          summary:     { type: 'string', description: 'Issue title' },
          description: { type: 'string', description: `Issue description (optional). ${JIRA_WIKI_MARKUP_HINT}` },
          assignee:    { type: 'string', description: 'Username to assign to (optional)' },
          priority:    { type: 'string', description: 'Priority name, e.g. "High" (optional)' },
          sprintId:    { type: 'number', description: 'Sprint ID to immediately add the new issue into (optional)' },
        },
        required: ['issueType', 'summary'],
      },
    },
    {
      name: 'jira_update_issue',
      description: `Use when you want to edit an existing Jira ticket: title, description, assignee, or priority. ${JIRA_WIKI_MARKUP_HINT}`,
      inputSchema: {
        type: 'object',
        properties: {
          issueKey:    { type: 'string', description: 'Jira issue key' },
          summary:     { type: 'string', description: 'New summary (optional)' },
          description: { type: 'string', description: `New description (optional). ${JIRA_WIKI_MARKUP_HINT}` },
          assignee:    { type: 'string', description: 'New assignee username, or empty string to unassign (optional)' },
          priority:    { type: 'string', description: 'New priority name (optional)' },
          sprintId:    { type: 'number', description: 'Sprint ID to add this issue into (optional)' },
        },
        required: ['issueKey'],
      },
    },
    {
      name: 'jira_add_issues_to_sprint',
      description: 'Use when you want to assign one or more Jira issues to a sprint by sprint ID.',
      inputSchema: {
        type: 'object',
        properties: {
          sprintId:  { type: 'number', description: 'Sprint ID' },
          issueKey:  { type: 'string', description: 'Single issue key (optional)' },
          issueKeys: { type: 'array', items: { type: 'string' }, description: 'Multiple issue keys (optional)' },
        },
        required: ['sprintId'],
      },
    },
    {
      name: 'jira_mutate_issue',
      description: `Use when you want to bundle Jira mutations in one call: create or target an issue, then optional update, sprint assignment, transition, and comment. ${JIRA_WIKI_MARKUP_HINT}`,
      inputSchema: {
        type: 'object',
        properties: {
          issueKey: { type: 'string', description: 'Existing issue key to mutate (optional if create is provided)' },
          create: {
            type: 'object',
            properties: {
              projectKey:  { type: 'string', description: 'Jira project code (optional, auto-resolved when omitted)' },
              project:     { type: 'string', description: 'Alias for projectKey' },
              issueType:   { type: 'string', description: 'Issue type name, e.g. Bug, Story, Task' },
              summary:     { type: 'string', description: 'Issue title' },
              description: { type: 'string', description: `Issue description (optional). ${JIRA_WIKI_MARKUP_HINT}` },
              assignee:    { type: 'string', description: 'Username to assign to (optional)' },
              priority:    { type: 'string', description: 'Priority name (optional)' },
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
            },
          },
          sprintId:       { type: 'number', description: 'Sprint ID to add the issue into (optional)' },
          transitionId:   { type: 'string', description: 'Transition ID (optional if transitionName is provided)' },
          transitionName: { type: 'string', description: 'Transition name, e.g. In Progress (optional if transitionId is provided)' },
          comment:        { type: 'string', description: `Comment to add after other mutations (optional, no emoji). ${JIRA_WIKI_MARKUP_HINT}` },
        },
      },
    },
    {
      name: 'jira_board_overview',
      description: 'Use when you want one board-level planning snapshot: board info, sprints, and optional sprint issues in one call.',
      inputSchema: {
        type: 'object',
        properties: {
          boardId:         { type: 'number', description: 'Jira board ID' },
          sprintState:     { type: 'string', description: 'Sprint state filter, e.g. "active,future" (default active,future)' },
          sprintMaxResults:{ type: 'number', description: 'Max sprints per page (default 10)', default: 10 },
          sprintStartAt:   { type: 'number', description: 'Sprints pagination offset (default 0)', default: 0 },
          includeIssues:   { type: 'boolean', description: 'Include sprint issues (default true)', default: true },
          issueMaxResults: { type: 'number', description: 'Max issues per sprint when includeIssues=true (default 25)', default: 25 },
          issueStartAt:    { type: 'number', description: 'Issue pagination offset per sprint (default 0)', default: 0 },
          assignee:        { type: 'string', description: 'Optional assignee filter for sprint issues' },
          status:          { type: 'string', description: 'Optional status filter for sprint issues' },
        },
        required: ['boardId'],
      },
    },
    {
      name: 'jira_get_comments',
      description: 'Use when you want the discussion thread on a Jira ticket, with pagination for long threads.',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey:   { type: 'string', description: 'Jira issue key' },
          maxResults: { type: 'number', description: 'Max comments per page (default 50)', default: 50 },
          startAt:    { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
        required: ['issueKey'],
      },
    },
    {
      name: 'jira_add_comment',
      description: `Use when you want to leave a comment on a Jira ticket. Keep comments concise and free of filler. Never include emojis. ${JIRA_WIKI_MARKUP_HINT}`,
      inputSchema: {
        type: 'object',
        properties: {
          issueKey: { type: 'string', description: 'Jira issue key' },
          body:     { type: 'string', description: `Concise comment text. No filler. Do not include emojis. ${JIRA_WIKI_MARKUP_HINT}` },
        },
        required: ['issueKey', 'body'],
      },
    },
    {
      name: 'jira_transition_issue',
      description: 'Use when you want to move a Jira ticket to another status. Provide a transition name (for example "In Progress") or a transition ID.',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey:       { type: 'string', description: 'Jira issue key, for example "FOO-123"' },
          transitionId:   { type: 'string', description: 'Transition ID (optional if transitionName is provided)' },
          transitionName: { type: 'string', description: 'Transition name, for example "In Progress" (optional if transitionId is provided)' },
        },
        required: ['issueKey'],
      },
    },
    // ── Bitbucket ─────────────────────────────────────────────────────────
    {
      name: 'bitbucket_list_repos',
      description: 'Use when you want to browse repositories in Bitbucket, optionally scoped to a project code. You can pass projectKey or project.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (optional)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          limit:      { type: 'number', description: 'Max repos per page (default 50)', default: 50 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'bitbucket_list_pull_requests',
      description: 'Use when you want pull requests for a repo (open, merged, or declined) with pagination. Primary review-discovery flow: resolve the branch, then filter by fromBranch to find the PR for that branch (do not assume it is your own PR). You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          state:      { type: 'string', enum: ['OPEN', 'MERGED', 'DECLINED'], description: 'PR state filter (default OPEN)', default: 'OPEN' },
          fromBranch: { type: 'string', description: 'Filter to PRs from this source branch (optional)' },
          text:       { type: 'string', description: 'Filter PRs where title or description contains this text (optional)' },
          limit:      { type: 'number', description: 'Max PRs per page (default 25)', default: 25 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'bitbucket_my_prs',
      description: 'Use only when you explicitly want your personal PR inbox (reviews requested, authored by you, or participated PRs). Do not use this for branch-based review targeting.',
      inputSchema: {
        type: 'object',
        properties: {
          limit: { type: 'number', description: 'Max PRs per page (default 25)', default: 25 },
          start: { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
          role:  { type: 'string', enum: ['author', 'reviewer', 'participant'], description: 'Inbox role filter (default server behavior)' },
        },
      },
    },
    {
      name: 'bitbucket_get_pull_request',
      description: 'Use when you want metadata for one PR: title, branches, author, reviewers, and description. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_get_pr_overview',
      description: 'Use when you want one bulk PR snapshot in a single call: metadata, commits, comments, task-style BLOCKER comments, and optional diff. If prId is unknown during review, discover it from branch context first (get_dev_context or bitbucket_list_pull_requests with fromBranch). For complete file context while reviewing, follow up with bitbucket_get_file at the PR source ref instead of relying on local checkout state.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:      { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:         { type: 'string', description: 'Alias for projectKey' },
          repoSlug:        { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:            { type: 'string', description: 'Alias for repoSlug' },
          prId:            { type: 'number', description: 'Pull request number (PR ID)' },
          includeCommits:  { type: 'boolean', description: 'Include commit list (default true)', default: true },
          includeComments: { type: 'boolean', description: 'Include review comments/blockers (default true)', default: true },
          includeDiff:     { type: 'boolean', description: 'Include diff text (default false)', default: false },
          commentsState:   { type: 'string', enum: ['OPEN', 'RESOLVED', 'PENDING'], description: 'Comment state filter (default OPEN)', default: 'OPEN' },
          commentsSeverity:{ type: 'string', enum: ['ALL', 'NORMAL', 'BLOCKER'], description: 'Comment severity filter (default ALL). BLOCKER means task/checklist-style review comments.', default: 'ALL' },
          commentsLimit:   { type: 'number', description: 'Max comments per page (default 50)', default: 50 },
          commentsStart:   { type: 'number', description: 'Comment pagination offset (default 0)', default: 0 },
          commitsLimit:    { type: 'number', description: 'Max commits per page (default 25)', default: 25 },
          commitsStart:    { type: 'number', description: 'Commit pagination offset (default 0)', default: 0 },
          diffMaxChars:    { type: 'number', description: 'Max diff characters when includeDiff=true (default 8000)', default: 8000 },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_get_pr_diff',
      description: 'Use when you want the code changes for one PR as a unified diff. During review, treat this as patch context only and fetch full files with bitbucket_get_file (using the PR source ref) before making final judgments. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_get_pr_commits',
      description: 'Use when you want commit history included in a PR. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
          limit:      { type: 'number', description: 'Max commits (default 25)', default: 25 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_create_pull_request',
      description: 'Use when you want to open a new PR. Project/repo auto-detect from git remote, source branch auto-detects from current branch if omitted, and this call first checks for an existing open PR from that source branch.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:  { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:     { type: 'string', description: 'Alias for projectKey' },
          repoSlug:    { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:        { type: 'string', description: 'Alias for repoSlug' },
          title:       { type: 'string', description: 'PR title' },
          description: { type: 'string', description: 'PR description (optional)' },
          fromBranch:  { type: 'string', description: 'Source branch name (defaults to current branch)' },
          toBranch:    { type: 'string', description: 'Target branch name (default: master)' },
          reviewers:   { type: 'array', items: { type: 'string' }, description: 'Reviewer usernames (optional)' },
        },
        required: ['title'],
      },
    },
    {
      name: 'bitbucket_update_pull_request',
      description: 'Use when you want to update an existing PR title, description, destination branch, or reviewers. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:  { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:     { type: 'string', description: 'Alias for projectKey' },
          repoSlug:    { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:        { type: 'string', description: 'Alias for repoSlug' },
          prId:        { type: 'number', description: 'Pull request number (PR ID)' },
          title:       { type: 'string', description: 'Updated PR title (optional)' },
          description: { type: 'string', description: 'Updated PR description, or empty string to clear (optional)' },
          toBranch:    { type: 'string', description: 'Updated target branch name (optional)' },
          reviewers:   { type: 'array', items: { type: 'string' }, description: 'Updated reviewer usernames (optional). Pass an empty array to clear reviewers.' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_mutate_pull_request',
      description: 'Use when you want one ergonomic PR mutation call: target a PR by prId, or auto-target the open PR from create.fromBranch/current branch, then optionally update it; if none exists and create is provided, create a new PR.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:  { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:     { type: 'string', description: 'Alias for projectKey' },
          repoSlug:    { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:        { type: 'string', description: 'Alias for repoSlug' },
          prId:        { type: 'number', description: 'Target pull request number (optional). If omitted, tool targets the open PR for create.fromBranch/current branch.' },
          create: {
            type: 'object',
            properties: {
              title:       { type: 'string', description: 'PR title' },
              description: { type: 'string', description: 'PR description (optional)' },
              fromBranch:  { type: 'string', description: 'Source branch to target/create from (defaults to current branch)' },
              toBranch:    { type: 'string', description: 'Destination branch (default: master when creating)' },
              reviewers:   { type: 'array', items: { type: 'string' }, description: 'Reviewer usernames (optional)' },
            },
            required: ['title'],
          },
          update: {
            type: 'object',
            properties: {
              title:       { type: 'string', description: 'Updated PR title (optional)' },
              description: { type: 'string', description: 'Updated PR description, or empty string to clear (optional)' },
              toBranch:    { type: 'string', description: 'Updated target branch (optional)' },
              reviewers:   { type: 'array', items: { type: 'string' }, description: 'Updated reviewer usernames (optional). Pass an empty array to clear reviewers.' },
            },
          },
        },
      },
    },
    {
      name: 'bitbucket_approve_pr',
      description: 'Use when you want to approve a PR. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_unapprove_pr',
      description: 'Use when you want to remove your PR approval. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_decline_pr',
      description: 'Use when you want to decline/close a PR without merging. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
          message:    { type: 'string', description: 'Optional decline message' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_merge_pr',
      description: 'Use when you want to merge/land/ship a PR. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:    { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:       { type: 'string', description: 'Alias for projectKey' },
          repoSlug:      { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:          { type: 'string', description: 'Alias for repoSlug' },
          prId:          { type: 'number', description: 'Pull request number (PR ID)' },
          mergeStrategy: { type: 'string', enum: ['MERGE_COMMIT', 'SQUASH', 'FAST_FORWARD'], description: 'Merge strategy (uses repo default if omitted)' },
          message:       { type: 'string', description: 'Custom merge commit message (optional)' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_get_pr_comments',
      description: 'Use when you want PR review discussion in bulk: comment threads, task-style BLOCKER comments, and blocker counts with pagination. The returned comment IDs are used as commentId in bitbucket_add_pr_comment (reply mode), bitbucket_update_pr_comment, and bitbucket_delete_pr_comment. For review tasks, locate PR by branch first instead of assuming it is in your inbox. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
          path:       { type: 'string', description: 'Optional file path filter, e.g. "src/index.ts"' },
          state:      { type: 'string', enum: ['OPEN', 'RESOLVED', 'PENDING'], description: 'State filter. For normal comments this maps to threadResolved (OPEN/RESOLVED). For BLOCKER tasks this uses task state (OPEN/RESOLVED; PENDING allowed only for non-BLOCKER).', default: 'OPEN' },
          severity:   { type: 'string', enum: ['ALL', 'NORMAL', 'BLOCKER'], description: 'Comment severity filter. BLOCKER means task/checklist-style review comments.', default: 'ALL' },
          countOnly:  { type: 'boolean', description: 'When true with severity=BLOCKER, returns counts instead of comment bodies', default: false },
          limit:      { type: 'number', description: 'Max items per page (default 50)', default: 50 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_add_pr_comment',
      description: `Add a PR review comment or reply to an existing thread.

REPLIES TO SPECIFIC COMMENTS ARE MANDATORY: If your remark is about an existing comment/thread, you MUST reply in that thread by setting commentId. Do not post a new top-level PR comment or a new inline thread for follow-up remarks on an existing comment.

FOR REVIEW FEEDBACK, DEFAULT TO INLINE + EXAMPLE: Prefer anchored inline comments with a concrete code change example (suggestion) over top-level prose. Use top-level comments only for overall PR-wide feedback.

INLINE COMMENTS ARE STRONGLY PREFERRED: Whenever your comment refers to a specific line or block of code, you MUST provide filePath and line to anchor it as an inline comment on the diff. General top-level comments (no filePath/line) should only be used for overall PR feedback that does not relate to any particular line.

SUGGESTIONS ARE STRONGLY PREFERRED OVER PLAIN COMMENTS: When you are pointing out something that should be changed, always provide a suggestion (the corrected code) rather than describing the change in words. A suggestion lets the author apply the fix with one click. Only omit suggestion if you are asking a question or raising a concern that has no clear single answer.

Keep comments concise, plain text, and free of filler. Never include emojis. You can pass projectKey/repoSlug or project/repo.`,
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:             { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:                { type: 'string', description: 'Alias for projectKey' },
          repoSlug:               { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:                   { type: 'string', description: 'Alias for repoSlug' },
          prId:                   { type: 'number', description: 'Pull request number (PR ID)' },
          commentId:              { type: 'number', description: 'Comment ID to reply to. Same identifier style as update/delete tools (optional).' },
          text:                   { type: 'string', description: 'Concise comment text. No filler. Do not include emojis. If suggestion is also provided, this text appears above the suggestion block.' },
          filePath:               { type: 'string', description: 'Destination file path for inline comment, e.g. "src/index.ts". Must be provided together with line.' },
          srcPath:                { type: 'string', description: 'Source file path. Only needed when the file was renamed; otherwise omit (defaults to filePath).' },
          line:                   { type: 'number', description: 'Line number in the file to anchor the comment to. Must be provided together with filePath.' },
          lineType:               { type: 'string', enum: ['ADDED', 'REMOVED', 'CONTEXT'], description: 'The type of the anchored line in the diff. Defaults to ADDED. Use CONTEXT for unchanged lines, REMOVED for deleted lines.' },
          fileType:               { type: 'string', enum: ['TO', 'FROM'], description: 'Which side of the diff the anchor refers to: TO (destination/new file, default) or FROM (source/old file).' },
          multilineStartLine:     { type: 'number', description: 'First line of a multiline anchor. Set together with line (last line) to span multiple lines.' },
          multilineStartLineType: { type: 'string', enum: ['ADDED', 'REMOVED', 'CONTEXT'], description: 'Line type for the multilineStartLine. Defaults to lineType.' },
          suggestion:             { type: 'string', description: 'Replacement code to suggest. Rendered as a suggestion block the author can apply with one click. STRONGLY PREFERRED whenever you are proposing a code change. Must be used with filePath and line.' },
        },
        required: ['prId', 'text'],
      },
    },
    {
      name: 'bitbucket_update_pr_comment',
      description: 'Use when you want to edit PR comments, resolve/reopen normal discussion threads, or manage task-style BLOCKER comments. Hint: for normal comments, resolve/reopen means threadResolved; for BLOCKER tasks, resolve/reopen uses state. Keep comments concise, plain text, and free of filler. Never include emojis. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
          commentId:  { type: 'number', description: 'Comment ID to update' },
          text:       { type: 'string', description: 'New concise comment text only. No filler. Do not include emojis. (optional)' },
          state:      { type: 'string', enum: ['OPEN', 'RESOLVED'], description: 'Task state for BLOCKER comments only (optional). Rejected for normal comments; use threadResolved instead.' },
          threadResolved: { type: 'boolean', description: 'Resolve/reopen normal comment threads in Bitbucket UI only (optional). Rejected for BLOCKER comments; use state instead.' },
          severity:   { type: 'string', enum: ['NORMAL', 'BLOCKER'], description: 'Comment severity (optional). BLOCKER marks it as a task/checklist item.' },
        },
        required: ['prId', 'commentId'],
      },
    },
    {
      name: 'bitbucket_delete_pr_comment',
      description: 'Use when you want to delete a PR comment by comment ID. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
          commentId:  { type: 'number', description: 'Comment ID to delete' },
        },
        required: ['prId', 'commentId'],
      },
    },
    {
      name: 'bitbucket_get_branches',
      description: 'Use when you want to browse repository branches or find a branch by name. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          filter:     { type: 'string', description: 'Filter branches by name (optional)' },
          limit:      { type: 'number', description: 'Max branches per page (default 25)', default: 25 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'bitbucket_get_file',
      description: 'Use when you want raw file content from Bitbucket at a branch, tag, or commit. In PR reviews, this is the preferred way to get full-file context so review quality does not depend on having the reviewed branch checked out locally. You can pass projectKey/repoSlug or project/repo.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          project:    { type: 'string', description: 'Alias for projectKey' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          repo:       { type: 'string', description: 'Alias for repoSlug' },
          path:       { type: 'string', description: 'File path, e.g. "src/index.ts"' },
          ref:        { type: 'string', description: 'Branch, tag, or commit hash (defaults to default branch)' },
        },
        required: ['path'],
      },
    },
    // ── Git ───────────────────────────────────────────────────────────────
    {
      name: 'git_get_context',
      description: 'Use when you want a quick local git snapshot: branch, remote, recent commits, working tree status, and Jira keys in branch names.',
      inputSchema: {
        type: 'object',
        properties: {
          repoPath:    { type: 'string', description: 'Path to the git repository (defaults to cwd)' },
          commitLimit: { type: 'number', description: 'Number of recent commits to show (default 10)', default: 10 },
        },
      },
    },
    {
      name: 'git_get_commits',
      description: 'Use when you want commit history for a branch with author, date, and message.',
      inputSchema: {
        type: 'object',
        properties: {
          repoPath: { type: 'string', description: 'Path to the git repository (defaults to cwd)' },
          limit:    { type: 'number', description: 'Max commits to return (default 20)', default: 20 },
          branch:   { type: 'string', description: 'Branch name to log (defaults to current branch)' },
        },
      },
    },
    {
      name: 'git_get_diff',
      description: 'Use when you want uncommitted changes or a diff between two refs/commits.',
      inputSchema: {
        type: 'object',
        properties: {
          repoPath: { type: 'string', description: 'Path to the git repository (defaults to cwd)' },
          fromRef:  { type: 'string', description: 'Base ref or commit (optional)' },
          toRef:    { type: 'string', description: 'Target ref or commit (optional, requires fromRef)' },
        },
      },
    },
  ],
}));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const { name, arguments: args = {} } = request.params;
  try {
    switch (name) {
      // Context
      case 'get_dev_context':
        return await getDevContext(args as { repoPath?: string }, jira, bitbucket);
      // Jira
      case 'jira_search_issues':
        return await jira.searchIssues(args as Parameters<typeof jira.searchIssues>[0]);
      case 'jira_my_issues':
        return await jira.myIssues(args as Parameters<typeof jira.myIssues>[0]);
      case 'jira_get_projects':
        return await jira.getProjects(args as Parameters<typeof jira.getProjects>[0]);
      case 'jira_get_issue_types':
        return await jira.getIssueTypes(normalizeJiraProjectArgs(args) as Parameters<typeof jira.getIssueTypes>[0]);
      case 'jira_get_sprints':
        return await jira.getSprints(args as Parameters<typeof jira.getSprints>[0]);
      case 'jira_search_users':
        return await jira.searchUsers(args as Parameters<typeof jira.searchUsers>[0]);
      case 'jira_get_issue':
        return await jira.getIssue(args as Parameters<typeof jira.getIssue>[0]);
      case 'jira_issue_overview':
        return await jira.issueOverview(args as Parameters<typeof jira.issueOverview>[0]);
      case 'jira_create_issue':
        return await jira.createIssue(normalizeJiraProjectArgs(args) as Parameters<typeof jira.createIssue>[0]);
      case 'jira_update_issue':
        return await jira.updateIssue(args as Parameters<typeof jira.updateIssue>[0]);
      case 'jira_add_issues_to_sprint':
        return await jira.addIssuesToSprint(args as Parameters<typeof jira.addIssuesToSprint>[0]);
      case 'jira_mutate_issue':
        return await jira.mutateIssue(normalizeJiraMutateArgs(args) as Parameters<typeof jira.mutateIssue>[0]);
      case 'jira_board_overview':
        return await jira.boardOverview(args as Parameters<typeof jira.boardOverview>[0]);
      case 'jira_get_comments':
        return await jira.getComments(args as Parameters<typeof jira.getComments>[0]);
      case 'jira_add_comment':
        return await jira.addComment(args as Parameters<typeof jira.addComment>[0]);
      case 'jira_transition_issue':
        return await jira.transitionIssue(args as Parameters<typeof jira.transitionIssue>[0]);
      // Bitbucket
      case 'bitbucket_list_repos':
        return await bitbucket.listRepos(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.listRepos>[0]);
      case 'bitbucket_list_pull_requests':
        return await bitbucket.listPullRequests(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.listPullRequests>[0]);
      case 'bitbucket_my_prs':
        return await bitbucket.myPrs(args as Parameters<typeof bitbucket.myPrs>[0]);
      case 'bitbucket_get_pull_request':
        return await bitbucket.getPullRequest(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.getPullRequest>[0]);
      case 'bitbucket_get_pr_overview':
        return await bitbucket.getPrOverview(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.getPrOverview>[0]);
      case 'bitbucket_get_pr_diff':
        return await bitbucket.getPrDiff(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.getPrDiff>[0]);
      case 'bitbucket_get_pr_commits':
        return await bitbucket.getPrCommits(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.getPrCommits>[0]);
      case 'bitbucket_create_pull_request':
        return await bitbucket.createPullRequest(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.createPullRequest>[0]);
      case 'bitbucket_update_pull_request':
        return await bitbucket.updatePullRequest(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.updatePullRequest>[0]);
      case 'bitbucket_mutate_pull_request':
        return await bitbucket.mutatePullRequest(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.mutatePullRequest>[0]);
      case 'bitbucket_approve_pr':
        return await bitbucket.approvePr(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.approvePr>[0]);
      case 'bitbucket_unapprove_pr':
        return await bitbucket.unapprovePr(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.unapprovePr>[0]);
      case 'bitbucket_decline_pr':
        return await bitbucket.declinePr(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.declinePr>[0]);
      case 'bitbucket_merge_pr':
        return await bitbucket.mergePr(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.mergePr>[0]);
      case 'bitbucket_get_pr_comments':
        return await bitbucket.getPrComments(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.getPrComments>[0]);
      case 'bitbucket_add_pr_comment':
        return await bitbucket.addPrComment(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.addPrComment>[0]);
      case 'bitbucket_update_pr_comment':
        return await bitbucket.updatePrComment(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.updatePrComment>[0]);
      case 'bitbucket_delete_pr_comment':
        return await bitbucket.deletePrComment(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.deletePrComment>[0]);
      case 'bitbucket_get_branches':
        return await bitbucket.getBranches(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.getBranches>[0]);
      case 'bitbucket_get_file':
        return await bitbucket.getFile(normalizeBitbucketArgs(args) as Parameters<typeof bitbucket.getFile>[0]);
      // Git
      case 'git_get_context':
        return getContext(args as Parameters<typeof getContext>[0]);
      case 'git_get_commits':
        return getCommits(args as Parameters<typeof getCommits>[0]);
      case 'git_get_diff':
        return getDiff(args as Parameters<typeof getDiff>[0]);
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

process.on('SIGINT', async () => {
  await server.close();
  process.exit(0);
});

const transport = new StdioServerTransport();
await server.connect(transport);
