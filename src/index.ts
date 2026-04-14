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
    // ── Jira ──────────────────────────────────────────────────────────────
    {
      name: 'jira_search_issues',
      description: 'Search Jira issues. Use `query` for plain-text search (searches summary, description, and comments using Lucene full-text). Use `jql` for advanced queries. Combine `query` with `project`, `status`, `assignee`, or `issueType` to narrow results without writing JQL.',
      inputSchema: {
        type: 'object',
        properties: {
          query:     { type: 'string', description: 'Plain-text search across summary, description, and comments (fuzzy, stemmed)' },
          jql:       { type: 'string', description: 'Raw JQL query — overrides all other filters when provided' },
          project:   { type: 'string', description: 'Filter by project key, e.g. "FOO"' },
          status:    { type: 'string', description: 'Filter by status name, e.g. "In Progress"' },
          assignee:  { type: 'string', description: 'Filter by assignee username, e.g. "jsmith"' },
          issueType: { type: 'string', description: 'Filter by issue type, e.g. "Bug", "Story"' },
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
          startAt: { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
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
          projectKey: { type: 'string', description: 'Jira project key, e.g. "FOO"' },
          issueType: { type: 'string', description: 'Issue type name, e.g. "Bug", "Story", "Task"' },
          summary: { type: 'string', description: 'Issue title' },
          description: { type: 'string', description: 'Issue description (optional)' },
          assignee: { type: 'string', description: 'Username to assign to (optional)' },
          priority: { type: 'string', description: 'Priority name, e.g. "High", "Medium" (optional)' },
        },
        required: ['projectKey', 'issueType', 'summary'],
      },
    },
    {
      name: 'jira_update_issue',
      description: 'Update fields on an existing Jira issue',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey: { type: 'string', description: 'Jira issue key' },
          summary: { type: 'string', description: 'New summary (optional)' },
          description: { type: 'string', description: 'New description (optional)' },
          assignee: { type: 'string', description: 'New assignee username (optional)' },
          priority: { type: 'string', description: 'New priority name (optional)' },
        },
        required: ['issueKey'],
      },
    },
    {
      name: 'jira_assign_issue',
      description: 'Assign (or unassign) a Jira issue',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey: { type: 'string', description: 'Jira issue key' },
          assignee: {
            description: 'Username to assign to, or null to unassign',
            oneOf: [{ type: 'string' }, { type: 'null' }],
          },
        },
        required: ['issueKey', 'assignee'],
      },
    },
    {
      name: 'jira_get_comments',
      description: 'Get comments on a Jira issue',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey: { type: 'string', description: 'Jira issue key' },
          maxResults: { type: 'number', description: 'Max comments per page (default 50)', default: 50 },
          startAt: { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
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
          body: { type: 'string', description: 'Comment text (plain text or Jira wiki markup)' },
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
          issueKey: { type: 'string', description: 'Jira issue key' },
          transitionId: { type: 'string', description: 'Transition ID from jira_get_transitions' },
        },
        required: ['issueKey', 'transitionId'],
      },
    },
    // ── Bitbucket ─────────────────────────────────────────────────────────
    {
      name: 'bitbucket_list_repos',
      description: 'List Bitbucket repositories, optionally filtered by project key',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key (optional)' },
          limit: { type: 'number', description: 'Max repos per page (default 50)', default: 50 },
          start: { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
      },
    },
    {
      name: 'bitbucket_list_pull_requests',
      description: 'List pull requests for a Bitbucket repository',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key' },
          repoSlug: { type: 'string', description: 'Repository slug' },
          state: {
            type: 'string',
            enum: ['OPEN', 'MERGED', 'DECLINED', 'ALL'],
            description: 'PR state filter (default OPEN)',
            default: 'OPEN',
          },
          limit: { type: 'number', description: 'Max PRs per page (default 25)', default: 25 },
          start: { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
        required: ['projectKey', 'repoSlug'],
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
      description: 'Get details of a specific Bitbucket pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key' },
          repoSlug: { type: 'string', description: 'Repository slug' },
          prId: { type: 'number', description: 'Pull request ID' },
        },
        required: ['projectKey', 'repoSlug', 'prId'],
      },
    },
    {
      name: 'bitbucket_get_pr_diff',
      description: 'Get the code diff for a Bitbucket pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key' },
          repoSlug: { type: 'string', description: 'Repository slug' },
          prId: { type: 'number', description: 'Pull request ID' },
        },
        required: ['projectKey', 'repoSlug', 'prId'],
      },
    },
    {
      name: 'bitbucket_create_pull_request',
      description: 'Create a new Bitbucket pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key' },
          repoSlug: { type: 'string', description: 'Repository slug' },
          title: { type: 'string', description: 'PR title' },
          description: { type: 'string', description: 'PR description (optional)' },
          fromBranch: { type: 'string', description: 'Source branch name' },
          toBranch: { type: 'string', description: 'Target branch name (default: master)' },
          reviewers: {
            type: 'array',
            items: { type: 'string' },
            description: 'List of reviewer usernames (optional)',
          },
        },
        required: ['projectKey', 'repoSlug', 'title', 'fromBranch'],
      },
    },
    {
      name: 'bitbucket_approve_pr',
      description: 'Approve a Bitbucket pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key' },
          repoSlug: { type: 'string', description: 'Repository slug' },
          prId: { type: 'number', description: 'Pull request ID' },
        },
        required: ['projectKey', 'repoSlug', 'prId'],
      },
    },
    {
      name: 'bitbucket_merge_pr',
      description: 'Merge a Bitbucket pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key' },
          repoSlug: { type: 'string', description: 'Repository slug' },
          prId: { type: 'number', description: 'Pull request ID' },
          mergeStrategy: {
            type: 'string',
            enum: ['MERGE_COMMIT', 'SQUASH', 'FAST_FORWARD'],
            description: 'Merge strategy (optional, uses repo default if omitted)',
          },
          message: { type: 'string', description: 'Custom merge commit message (optional)' },
        },
        required: ['projectKey', 'repoSlug', 'prId'],
      },
    },
    {
      name: 'bitbucket_get_pr_comments',
      description: 'Get comments on a Bitbucket pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key' },
          repoSlug: { type: 'string', description: 'Repository slug' },
          prId: { type: 'number', description: 'Pull request ID' },
          limit: { type: 'number', description: 'Max items per page (default 50)', default: 50 },
          start: { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
        required: ['projectKey', 'repoSlug', 'prId'],
      },
    },
    {
      name: 'bitbucket_add_pr_comment',
      description: 'Add a comment to a Bitbucket pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key' },
          repoSlug: { type: 'string', description: 'Repository slug' },
          prId: { type: 'number', description: 'Pull request ID' },
          text: { type: 'string', description: 'Comment text' },
        },
        required: ['projectKey', 'repoSlug', 'prId', 'text'],
      },
    },
    {
      name: 'bitbucket_get_branches',
      description: 'List branches in a Bitbucket repository',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key' },
          repoSlug: { type: 'string', description: 'Repository slug' },
          filter: { type: 'string', description: 'Filter branches by name (optional)' },
          limit: { type: 'number', description: 'Max branches per page (default 25)', default: 25 },
          start: { type: 'number', description: 'Offset for pagination (default 0)', default: 0 },
        },
        required: ['projectKey', 'repoSlug'],
      },
    },
    {
      name: 'bitbucket_get_file',
      description: 'Get the raw content of a file from a Bitbucket repository',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key' },
          repoSlug: { type: 'string', description: 'Repository slug' },
          path: { type: 'string', description: 'File path, e.g. "src/index.ts"' },
          ref: { type: 'string', description: 'Branch, tag, or commit hash (defaults to default branch)' },
        },
        required: ['projectKey', 'repoSlug', 'path'],
      },
    },
    // ── Git ───────────────────────────────────────────────────────────────
    {
      name: 'git_get_context',
      description: 'Get git context: current branch, remote URL, recent commits, working tree status, and any Jira issue keys detected in the branch name',
      inputSchema: {
        type: 'object',
        properties: {
          repoPath: { type: 'string', description: 'Path to the git repository (defaults to cwd)' },
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
          limit: { type: 'number', description: 'Max commits to return (default 20)', default: 20 },
          branch: { type: 'string', description: 'Branch name to log (defaults to current branch)' },
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
          fromRef: { type: 'string', description: 'Base ref or commit (optional)' },
          toRef: { type: 'string', description: 'Target ref or commit (optional, requires fromRef)' },
        },
      },
    },
  ],
}));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const { name, arguments: args = {} } = request.params;
  try {
    switch (name) {
      // Jira
      case 'jira_search_issues':
        return await jira.searchIssues(args as Parameters<typeof jira.searchIssues>[0]);
      case 'jira_my_issues':
        return await jira.myIssues(args as Parameters<typeof jira.myIssues>[0]);
      case 'jira_get_projects':
        return await jira.getProjects(args as Parameters<typeof jira.getProjects>[0]);
      case 'jira_get_issue':
        return await jira.getIssue(args as Parameters<typeof jira.getIssue>[0]);
      case 'jira_create_issue':
        return await jira.createIssue(args as Parameters<typeof jira.createIssue>[0]);
      case 'jira_update_issue':
        return await jira.updateIssue(args as Parameters<typeof jira.updateIssue>[0]);
      case 'jira_assign_issue':
        return await jira.assignIssue(args as Parameters<typeof jira.assignIssue>[0]);
      case 'jira_get_comments':
        return await jira.getComments(args as Parameters<typeof jira.getComments>[0]);
      case 'jira_add_comment':
        return await jira.addComment(args as Parameters<typeof jira.addComment>[0]);
      case 'jira_get_transitions':
        return await jira.getTransitions(args as Parameters<typeof jira.getTransitions>[0]);
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
      case 'bitbucket_create_pull_request':
        return await bitbucket.createPullRequest(args as Parameters<typeof bitbucket.createPullRequest>[0]);
      case 'bitbucket_approve_pr':
        return await bitbucket.approvePr(args as Parameters<typeof bitbucket.approvePr>[0]);
      case 'bitbucket_merge_pr':
        return await bitbucket.mergePr(args as Parameters<typeof bitbucket.mergePr>[0]);
      case 'bitbucket_get_pr_comments':
        return await bitbucket.getPrComments(args as Parameters<typeof bitbucket.getPrComments>[0]);
      case 'bitbucket_add_pr_comment':
        return await bitbucket.addPrComment(args as Parameters<typeof bitbucket.addPrComment>[0]);
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
