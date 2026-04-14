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
      description: 'Search Jira issues using JQL',
      inputSchema: {
        type: 'object',
        properties: {
          jql: { type: 'string', description: 'JQL query, e.g. "project = FOO AND status = Open"' },
          maxResults: { type: 'number', description: 'Max results (default 20, max 100)', default: 20 },
        },
        required: ['jql'],
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
      name: 'jira_get_comments',
      description: 'Get comments on a Jira issue',
      inputSchema: {
        type: 'object',
        properties: {
          issueKey: { type: 'string', description: 'Jira issue key' },
          maxResults: { type: 'number', description: 'Max comments to return (default 50)', default: 50 },
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
      description: 'Change the status of a Jira issue using a transition ID (get IDs from jira_get_transitions)',
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
          projectKey: { type: 'string', description: 'Bitbucket project key to filter repos (optional)' },
          limit: { type: 'number', description: 'Max repos to return (default 50)', default: 50 },
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
          limit: { type: 'number', description: 'Max PRs to return (default 25)', default: 25 },
        },
        required: ['projectKey', 'repoSlug'],
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
          toBranch: { type: 'string', description: 'Target branch name (default: main)' },
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
      name: 'bitbucket_get_pr_comments',
      description: 'Get comments on a Bitbucket pull request',
      inputSchema: {
        type: 'object',
        properties: {
          projectKey: { type: 'string', description: 'Bitbucket project key' },
          repoSlug: { type: 'string', description: 'Repository slug' },
          prId: { type: 'number', description: 'Pull request ID' },
          limit: { type: 'number', description: 'Max activity items (default 50)', default: 50 },
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
    // ── Git ───────────────────────────────────────────────────────────────
    {
      name: 'git_get_context',
      description: 'Get git context for a repo: current branch, remote URL, recent commits, and working tree status',
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
      case 'jira_get_issue':
        return await jira.getIssue(args as Parameters<typeof jira.getIssue>[0]);
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
      case 'bitbucket_get_pull_request':
        return await bitbucket.getPullRequest(args as Parameters<typeof bitbucket.getPullRequest>[0]);
      case 'bitbucket_create_pull_request':
        return await bitbucket.createPullRequest(args as Parameters<typeof bitbucket.createPullRequest>[0]);
      case 'bitbucket_get_pr_comments':
        return await bitbucket.getPrComments(args as Parameters<typeof bitbucket.getPrComments>[0]);
      case 'bitbucket_add_pr_comment':
        return await bitbucket.addPrComment(args as Parameters<typeof bitbucket.addPrComment>[0]);
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
