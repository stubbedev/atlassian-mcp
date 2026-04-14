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

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    // ── Context ───────────────────────────────────────────────────────────
    {
      name: 'get_dev_context',
      description: 'Use when you want one quick snapshot before coding or reviewing: current git branch/status, Jira tickets detected from branch name, and the open Bitbucket PR for that branch.',
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
      description: 'Use when preparing to create tickets and you need valid issue types and statuses. If projectKey is omitted, the server auto-picks from branch context or asks you to choose.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Jira project code, e.g. "PAY" from PAY-123 (optional)' },
        },
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
      name: 'jira_create_issue',
      description: 'Use when you want to create a new Jira ticket (bug, story, task, etc.). If projectKey is omitted, the server auto-picks from branch context or asks you to choose.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:  { type: 'string', description: 'Jira project code, e.g. "PAY" from PAY-123 (optional)' },
          issueType:   { type: 'string', description: 'Issue type name, for example "Bug", "Story", or "Task"' },
          summary:     { type: 'string', description: 'Issue title' },
          description: { type: 'string', description: 'Issue description (optional)' },
          assignee:    { type: 'string', description: 'Username to assign to (optional)' },
          priority:    { type: 'string', description: 'Priority name, e.g. "High" (optional)' },
        },
        required: ['issueType', 'summary'],
      },
    },
    {
      name: 'jira_update_issue',
      description: 'Use when you want to edit an existing Jira ticket: title, description, assignee, or priority.',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey:    { type: 'string', description: 'Jira issue key' },
          summary:     { type: 'string', description: 'New summary (optional)' },
          description: { type: 'string', description: 'New description (optional)' },
          assignee:    { type: 'string', description: 'New assignee username, or empty string to unassign (optional)' },
          priority:    { type: 'string', description: 'New priority name (optional)' },
        },
        required: ['issueKey'],
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
      description: 'Use when you want to leave a comment on a Jira ticket.',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey: { type: 'string', description: 'Jira issue key' },
          body:     { type: 'string', description: 'Comment text (plain text or Jira wiki markup)' },
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
      description: 'Use when you want to browse repositories in Bitbucket, optionally scoped to a project code.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (optional)' },
          limit:      { type: 'number', description: 'Max repos per page (default 50)', default: 50 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'bitbucket_list_pull_requests',
      description: 'Use when you want pull requests for a repo (open, merged, or declined) with pagination.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
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
      description: 'Use when you want your personal PR inbox (reviews requested, authored by you, or participated PRs).',
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
      description: 'Use when you want metadata for one PR: title, branches, author, reviewers, and description.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_get_pr_diff',
      description: 'Use when you want the code changes for one PR as a unified diff.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_get_pr_commits',
      description: 'Use when you want commit history included in a PR.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
          limit:      { type: 'number', description: 'Max commits (default 25)', default: 25 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_create_pull_request',
      description: 'Use when you want to open a new PR. Project/repo auto-detect from git remote, and source branch auto-detects from current branch if omitted.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:  { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:    { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
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
      name: 'bitbucket_approve_pr',
      description: 'Use when you want to approve a PR.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_unapprove_pr',
      description: 'Use when you want to remove your PR approval.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_decline_pr',
      description: 'Use when you want to decline/close a PR without merging.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
          message:    { type: 'string', description: 'Optional decline message' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_merge_pr',
      description: 'Use when you want to merge/land/ship a PR.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:    { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:      { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          prId:          { type: 'number', description: 'Pull request number (PR ID)' },
          mergeStrategy: { type: 'string', enum: ['MERGE_COMMIT', 'SQUASH', 'FAST_FORWARD'], description: 'Merge strategy (uses repo default if omitted)' },
          message:       { type: 'string', description: 'Custom merge commit message (optional)' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_get_pr_comments',
      description: 'Use when you want PR review discussion in bulk: comment threads, blocker comments, and blocker counts with pagination.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
          path:       { type: 'string', description: 'Optional file path filter, e.g. "src/index.ts"' },
          state:      { type: 'string', enum: ['OPEN', 'RESOLVED', 'PENDING'], description: 'Comment state filter (default OPEN; BLOCKER mode supports OPEN/RESOLVED)', default: 'OPEN' },
          severity:   { type: 'string', enum: ['ALL', 'NORMAL', 'BLOCKER'], description: 'Comment severity filter. Use BLOCKER for blocker comments.', default: 'ALL' },
          countOnly:  { type: 'boolean', description: 'When true with severity=BLOCKER, returns counts instead of comment bodies', default: false },
          limit:      { type: 'number', description: 'Max items per page (default 50)', default: 50 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_add_pr_comment',
      description: 'Use when you want to add a PR review comment or reply to an existing thread.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
          parentCommentId: { type: 'number', description: 'Parent comment ID for reply mode (optional)' },
          text:       { type: 'string', description: 'Comment text' },
        },
        required: ['prId', 'text'],
      },
    },
    {
      name: 'bitbucket_update_pr_comment',
      description: 'Use when you want to edit PR comments, resolve/reopen them, or set severity to BLOCKER.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
          commentId:  { type: 'number', description: 'Comment ID to update' },
          text:       { type: 'string', description: 'New comment text (optional)' },
          state:      { type: 'string', enum: ['OPEN', 'RESOLVED'], description: 'Comment state (optional)' },
          severity:   { type: 'string', enum: ['NORMAL', 'BLOCKER'], description: 'Comment severity (optional)' },
        },
        required: ['prId', 'commentId'],
      },
    },
    {
      name: 'bitbucket_delete_pr_comment',
      description: 'Use when you want to delete a PR comment by comment ID.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          prId:       { type: 'number', description: 'Pull request number (PR ID)' },
          commentId:  { type: 'number', description: 'Comment ID to delete' },
        },
        required: ['prId', 'commentId'],
      },
    },
    {
      name: 'bitbucket_get_branches',
      description: 'Use when you want to browse repository branches or find a branch by name.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
          filter:     { type: 'string', description: 'Filter branches by name (optional)' },
          limit:      { type: 'number', description: 'Max branches per page (default 25)', default: 25 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'bitbucket_get_file',
      description: 'Use when you want raw file content from Bitbucket at a branch, tag, or commit.',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project code, e.g. "ENG" (usually auto-detected)' },
          repoSlug:   { type: 'string', description: 'Repository slug, e.g. "payments-service" (usually auto-detected)' },
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
        return await jira.getIssueTypes(args as Parameters<typeof jira.getIssueTypes>[0]);
      case 'jira_search_users':
        return await jira.searchUsers(args as Parameters<typeof jira.searchUsers>[0]);
      case 'jira_get_issue':
        return await jira.getIssue(args as Parameters<typeof jira.getIssue>[0]);
      case 'jira_create_issue':
        return await jira.createIssue(args as Parameters<typeof jira.createIssue>[0]);
      case 'jira_update_issue':
        return await jira.updateIssue(args as Parameters<typeof jira.updateIssue>[0]);
      case 'jira_get_comments':
        return await jira.getComments(args as Parameters<typeof jira.getComments>[0]);
      case 'jira_add_comment':
        return await jira.addComment(args as Parameters<typeof jira.addComment>[0]);
      case 'jira_transition_issue':
        return await jira.transitionIssue(args as Parameters<typeof jira.transitionIssue>[0]);
      // Bitbucket
      case 'bitbucket_list_repos':
        return await bitbucket.listRepos(args as Parameters<typeof bitbucket.listRepos>[0]);
      case 'bitbucket_list_pull_requests':
        return await bitbucket.listPullRequests(args as Parameters<typeof bitbucket.listPullRequests>[0]);
      case 'bitbucket_my_prs':
        return await bitbucket.myPrs(args as Parameters<typeof bitbucket.myPrs>[0]);
      case 'bitbucket_get_pull_request':
        return await bitbucket.getPullRequest(args as Parameters<typeof bitbucket.getPullRequest>[0]);
      case 'bitbucket_get_pr_diff':
        return await bitbucket.getPrDiff(args as Parameters<typeof bitbucket.getPrDiff>[0]);
      case 'bitbucket_get_pr_commits':
        return await bitbucket.getPrCommits(args as Parameters<typeof bitbucket.getPrCommits>[0]);
      case 'bitbucket_create_pull_request':
        return await bitbucket.createPullRequest(args as Parameters<typeof bitbucket.createPullRequest>[0]);
      case 'bitbucket_approve_pr':
        return await bitbucket.approvePr(args as Parameters<typeof bitbucket.approvePr>[0]);
      case 'bitbucket_unapprove_pr':
        return await bitbucket.unapprovePr(args as Parameters<typeof bitbucket.unapprovePr>[0]);
      case 'bitbucket_decline_pr':
        return await bitbucket.declinePr(args as Parameters<typeof bitbucket.declinePr>[0]);
      case 'bitbucket_merge_pr':
        return await bitbucket.mergePr(args as Parameters<typeof bitbucket.mergePr>[0]);
      case 'bitbucket_get_pr_comments':
        return await bitbucket.getPrComments(args as Parameters<typeof bitbucket.getPrComments>[0]);
      case 'bitbucket_add_pr_comment':
        return await bitbucket.addPrComment(args as Parameters<typeof bitbucket.addPrComment>[0]);
      case 'bitbucket_update_pr_comment':
        return await bitbucket.updatePrComment(args as Parameters<typeof bitbucket.updatePrComment>[0]);
      case 'bitbucket_delete_pr_comment':
        return await bitbucket.deletePrComment(args as Parameters<typeof bitbucket.deletePrComment>[0]);
      case 'bitbucket_get_branches':
        return await bitbucket.getBranches(args as Parameters<typeof bitbucket.getBranches>[0]);
      case 'bitbucket_get_file':
        return await bitbucket.getFile(args as Parameters<typeof bitbucket.getFile>[0]);
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
