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
import { getDevContext, createPrFromContext } from './context.js';

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
      description: 'One-shot developer context: reads the current git branch, fetches any linked Jira tickets detected in the branch name, and finds the open Bitbucket PR for the branch — all in a single call.',
      inputSchema: {
        type: 'object',
        properties: {
          repoPath: { type: 'string', description: 'Path to the git repository (defaults to cwd)' },
        },
      },
    },
    // ── Jira ──────────────────────────────────────────────────────────────
    {
      name: 'jira_search_issues',
      description: 'Search Jira issues. Use `query` for plain-text search (searches summary, description, and comments using Lucene full-text). Use `jql` for advanced queries. Combine `query` with `project`, `status`, `assignee`, or `issueType` to narrow results without writing JQL.',
      inputSchema: {
        type: 'object',
        properties: {
          query:      { type: 'string', description: 'Plain-text search across summary, description, and comments (fuzzy, stemmed)' },
          jql:        { type: 'string', description: 'Raw JQL query — overrides all other filters when provided' },
          project:    { type: 'string', description: 'Filter by project key, e.g. "FOO"' },
          status:     { type: 'string', description: 'Filter by status name, e.g. "In Progress"' },
          assignee:   { type: 'string', description: 'Filter by assignee username' },
          issueType:  { type: 'string', description: 'Filter by issue type, e.g. "Bug", "Story"' },
          maxResults: { type: 'number', description: 'Max results per page (default 20)', default: 20 },
          startAt:    { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'jira_my_issues',
      description: 'List issues assigned to the current user, ordered by last updated',
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
      description: 'List all accessible Jira projects',
      inputSchema: {
        type: 'object',
        properties: {
          maxResults: { type: 'number', description: 'Max results (default 50)', default: 50 },
        },
      },
    },
    {
      name: 'jira_get_issue_types',
      description: 'List issue types and their available statuses for a project — use before jira_create_issue to see valid options',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Jira project key' },
        },
        required: ['projectKey'],
      },
    },
    {
      name: 'jira_search_users',
      description: 'Search for Jira users by name or username — use to find the correct username before assigning an issue',
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
      description: 'Get details of a Jira issue by key',
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
      description: 'Create a new Jira issue',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:  { type: 'string', description: 'Jira project key' },
          issueType:   { type: 'string', description: 'Issue type name, e.g. "Bug", "Story", "Task"' },
          summary:     { type: 'string', description: 'Issue title' },
          description: { type: 'string', description: 'Issue description (optional)' },
          assignee:    { type: 'string', description: 'Username to assign to (optional)' },
          priority:    { type: 'string', description: 'Priority name, e.g. "High" (optional)' },
        },
        required: ['projectKey', 'issueType', 'summary'],
      },
    },
    {
      name: 'jira_update_issue',
      description: 'Update fields on an existing Jira issue (summary, description, assignee, priority)',
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
      description: 'Get comments on a Jira issue',
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
      description: 'Add a comment to a Jira issue',
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
      name: 'jira_get_transitions',
      description: 'List available status transitions for a Jira issue',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey: { type: 'string', description: 'Jira issue key' },
        },
        required: ['issueKey'],
      },
    },
    {
      name: 'jira_transition_issue',
      description: 'Change the status of a Jira issue (get transition IDs from jira_get_transitions)',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey:     { type: 'string', description: 'Jira issue key' },
          transitionId: { type: 'string', description: 'Transition ID from jira_get_transitions' },
        },
        required: ['issueKey', 'transitionId'],
      },
    },
    // ── Bitbucket ─────────────────────────────────────────────────────────
    {
      name: 'bitbucket_create_pr_from_context',
      description: 'Create a Bitbucket pull request using the current git repo — auto-detects project, repo, and branch from the git remote. Only requires a title.',
      inputSchema: {
        type: 'object',
        properties: {
          title:       { type: 'string', description: 'PR title' },
          description: { type: 'string', description: 'PR description (optional)' },
          toBranch:    { type: 'string', description: 'Target branch (default: master)' },
          reviewers:   { type: 'array', items: { type: 'string' }, description: 'Reviewer usernames (optional)' },
          repoPath:    { type: 'string', description: 'Path to the git repository (defaults to cwd)' },
        },
        required: ['title'],
      },
    },
    {
      name: 'bitbucket_list_repos',
      description: 'List Bitbucket repositories, optionally filtered by project key',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (optional)' },
          limit:      { type: 'number', description: 'Max repos per page (default 50)', default: 50 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'bitbucket_list_pull_requests',
      description: 'List pull requests for a repository',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          state:      { type: 'string', enum: ['OPEN', 'MERGED', 'DECLINED', 'ALL'], description: 'PR state filter (default OPEN)', default: 'OPEN' },
          limit:      { type: 'number', description: 'Max PRs per page (default 25)', default: 25 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'bitbucket_my_prs',
      description: 'List pull requests in your inbox (authored by you or awaiting your review)',
      inputSchema: {
        type: 'object',
        properties: {
          limit: { type: 'number', description: 'Max PRs per page (default 25)', default: 25 },
          start: { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'bitbucket_get_pull_request',
      description: 'Get details of a specific pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          prId:       { type: 'number', description: 'Pull request ID' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_get_pr_diff',
      description: 'Get the code diff for a pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          prId:       { type: 'number', description: 'Pull request ID' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_get_pr_commits',
      description: 'List commits included in a pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          prId:       { type: 'number', description: 'Pull request ID' },
          limit:      { type: 'number', description: 'Max commits (default 25)', default: 25 },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_create_pull_request',
      description: 'Create a new pull request (project and repo can be auto-detected from git remote)',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:  { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:    { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          title:       { type: 'string', description: 'PR title' },
          description: { type: 'string', description: 'PR description (optional)' },
          fromBranch:  { type: 'string', description: 'Source branch name' },
          toBranch:    { type: 'string', description: 'Target branch name (default: master)' },
          reviewers:   { type: 'array', items: { type: 'string' }, description: 'Reviewer usernames (optional)' },
        },
        required: ['title', 'fromBranch'],
      },
    },
    {
      name: 'bitbucket_approve_pr',
      description: 'Approve a pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          prId:       { type: 'number', description: 'Pull request ID' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_unapprove_pr',
      description: 'Retract your approval from a pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          prId:       { type: 'number', description: 'Pull request ID' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_decline_pr',
      description: 'Decline a pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          prId:       { type: 'number', description: 'Pull request ID' },
          message:    { type: 'string', description: 'Optional decline message' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_merge_pr',
      description: 'Merge a pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey:    { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:      { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          prId:          { type: 'number', description: 'Pull request ID' },
          mergeStrategy: { type: 'string', enum: ['MERGE_COMMIT', 'SQUASH', 'FAST_FORWARD'], description: 'Merge strategy (uses repo default if omitted)' },
          message:       { type: 'string', description: 'Custom merge commit message (optional)' },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_get_pr_comments',
      description: 'Get pull request comment threads with comment IDs and states (needed for replies and resolving)',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          prId:       { type: 'number', description: 'Pull request ID' },
          state:      { type: 'string', enum: ['OPEN', 'RESOLVED', 'PENDING'], description: 'Filter comment state (default OPEN)', default: 'OPEN' },
          limit:      { type: 'number', description: 'Max items per page (default 50)', default: 50 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
        required: ['prId'],
      },
    },
    {
      name: 'bitbucket_add_pr_comment',
      description: 'Add a top-level comment or reply to an existing comment in a pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          prId:       { type: 'number', description: 'Pull request ID' },
          parentCommentId: { type: 'number', description: 'Parent comment ID for reply mode (optional)' },
          text:       { type: 'string', description: 'Comment text' },
        },
        required: ['prId', 'text'],
      },
    },
    {
      name: 'bitbucket_update_pr_comment',
      description: 'Update comment text, state, and/or severity (convert comment <-> task)',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          prId:       { type: 'number', description: 'Pull request ID' },
          commentId:  { type: 'number', description: 'Comment ID to update' },
          text:       { type: 'string', description: 'New comment text (optional)' },
          state:      { type: 'string', enum: ['OPEN', 'RESOLVED'], description: 'Comment state (optional)' },
          severity:   { type: 'string', enum: ['NORMAL', 'BLOCKER'], description: 'Comment severity (optional, BLOCKER creates a task)' },
        },
        required: ['prId', 'commentId'],
      },
    },
    {
      name: 'bitbucket_delete_pr_comment',
      description: 'Delete a pull request comment by ID',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          prId:       { type: 'number', description: 'Pull request ID' },
          commentId:  { type: 'number', description: 'Comment ID to delete' },
        },
        required: ['prId', 'commentId'],
      },
    },
    {
      name: 'bitbucket_get_branches',
      description: 'List branches in a repository',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          filter:     { type: 'string', description: 'Filter branches by name (optional)' },
          limit:      { type: 'number', description: 'Max branches per page (default 25)', default: 25 },
          start:      { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'bitbucket_get_file',
      description: 'Get the raw content of a file from a repository',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (auto-detected from git remote if omitted)' },
          repoSlug:   { type: 'string', description: 'Repository slug (auto-detected from git remote if omitted)' },
          path:       { type: 'string', description: 'File path, e.g. "src/index.ts"' },
          ref:        { type: 'string', description: 'Branch, tag, or commit hash (defaults to default branch)' },
        },
        required: ['path'],
      },
    },
    // ── Git ───────────────────────────────────────────────────────────────
    {
      name: 'git_get_context',
      description: 'Get git context for a local repo: current branch, remote URL, recent commits, working tree status, and any Jira keys detected in the branch name',
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
      description: 'Get commit history for a branch with author and message details',
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
      description: 'Get git diff — uncommitted changes (vs HEAD) or between two refs',
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
      case 'jira_get_transitions':
        return await jira.getTransitions(args as Parameters<typeof jira.getTransitions>[0]);
      case 'jira_transition_issue':
        return await jira.transitionIssue(args as Parameters<typeof jira.transitionIssue>[0]);
      // Bitbucket
      case 'bitbucket_create_pr_from_context':
        return await createPrFromContext(args as Parameters<typeof createPrFromContext>[0], bitbucket);
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
