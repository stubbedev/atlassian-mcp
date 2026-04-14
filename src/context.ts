import { execSync } from 'child_process';
import type { JiraClient } from './jira.js';
import type { BitbucketClient } from './bitbucket.js';

type ToolResult = { content: Array<{ type: 'text'; text: string }> };

const JIRA_KEY_RE = /\b[A-Z][A-Z0-9]+-\d+\b/g;

function safeExec(cmd: string, cwd: string): string {
  try {
    return execSync(cmd, { cwd, encoding: 'utf-8' }).trim();
  } catch {
    return '';
  }
}

/**
 * Parses a Bitbucket Server remote URL into projectKey + repoSlug.
 * Handles SSH (ssh://git@host/PROJ/repo.git), SCP-like (git@host:PROJ/repo.git),
 * and HTTP (https://host/scm/PROJ/repo.git) formats.
 */
export function parseBitbucketRemote(remoteUrl: string): { projectKey: string; repoSlug: string } | null {
  // SSH: ssh://git@host/PROJ/repo.git
  const sshUrl = remoteUrl.match(/ssh:\/\/[^/]+\/([^/]+)\/([^/]+?)(?:\.git)?$/);
  if (sshUrl) return { projectKey: sshUrl[1], repoSlug: sshUrl[2] };

  // SCP-like: git@host:PROJ/repo.git
  const scpUrl = remoteUrl.match(/^[^@]+@[^:]+:([^/]+)\/([^/]+?)(?:\.git)?$/);
  if (scpUrl) return { projectKey: scpUrl[1], repoSlug: scpUrl[2] };

  // HTTP: https://host/scm/PROJ/repo.git
  const httpUrl = remoteUrl.match(/\/scm\/([^/]+)\/([^/]+?)(?:\.git)?$/);
  if (httpUrl) return { projectKey: httpUrl[1], repoSlug: httpUrl[2] };

  return null;
}

/**
 * Unified developer context: git state + linked Jira issues + open PR for current branch.
 */
export async function getDevContext(
  args: { repoPath?: string },
  jira: JiraClient,
  bitbucket: BitbucketClient
): Promise<ToolResult> {
  const repoPath = args.repoPath ?? process.cwd();
  const sections: string[] = [];

  const branch = safeExec('git rev-parse --abbrev-ref HEAD', repoPath) || '(unknown)';
  const remote = safeExec('git remote get-url origin', repoPath) || '(no remote)';
  const recentCommits = safeExec('git log --oneline -5', repoPath) || '(none)';
  const status = safeExec('git status --short', repoPath) || '(clean)';

  sections.push([
    `Repository: ${repoPath}`,
    `Branch:     ${branch}`,
    `Remote:     ${remote}`,
    '',
    'Recent commits:',
    recentCommits,
    '',
    'Working tree:',
    status,
  ].join('\n'));

  // Jira — fetch any tickets referenced in the branch name
  const jiraKeys = [...new Set(branch.match(JIRA_KEY_RE) ?? [])];
  for (const key of jiraKeys) {
    try {
      const result = await jira.getIssue({ issueKey: key });
      sections.push(`── Jira ${key} ──\n${result.content[0].text}`);
    } catch {
      sections.push(`── Jira ${key} ── (could not fetch)`);
    }
  }

  // Bitbucket — find the open PR for this branch
  const parsed = parseBitbucketRemote(remote);
  if (parsed) {
    try {
      const pr = await bitbucket.findOpenPrForBranch(parsed.projectKey, parsed.repoSlug, branch);
      if (pr) {
        const reviewers = pr.reviewers.map((r) => `${r.user.displayName}${r.approved ? ' ✓' : ''}`).join(', ');
        const url = pr.links?.self?.[0]?.href ?? '';
        const prLines = [
          `── PR #${pr.id}: ${pr.title} ──`,
          `State:     ${pr.state}`,
          `Author:    ${pr.author.user.displayName}`,
          `Branch:    ${pr.fromRef.displayId} → ${pr.toRef.displayId}`,
          `Reviewers: ${reviewers || 'None'}`,
        ];
        if (url) prLines.push(`URL:       ${url}`);
        sections.push(prLines.join('\n'));
      } else {
        sections.push(`── Bitbucket (${parsed.projectKey}/${parsed.repoSlug}) ── No open PR for branch "${branch}"`);
      }
    } catch {
      sections.push(`── Bitbucket (${parsed.projectKey}/${parsed.repoSlug}) ── (could not fetch PRs)`);
    }
  }

  return { content: [{ type: 'text', text: sections.join('\n\n') }] };
}

/**
 * Creates a Bitbucket PR using the current git repo to auto-detect project, repo, and branch.
 */
export async function createPrFromContext(
  args: {
    repoPath?: string;
    title: string;
    description?: string;
    toBranch?: string;
    reviewers?: string[];
  },
  bitbucket: BitbucketClient
): Promise<ToolResult> {
  const repoPath = args.repoPath ?? process.cwd();

  const remote = safeExec('git remote get-url origin', repoPath);
  if (!remote) throw new Error('No git remote found — are you in a git repository?');

  const parsed = parseBitbucketRemote(remote);
  if (!parsed) throw new Error(`Could not parse Bitbucket project/repo from remote: ${remote}`);

  const branch = safeExec('git rev-parse --abbrev-ref HEAD', repoPath);
  if (!branch) throw new Error('Could not determine current branch.');
  if (branch === 'HEAD') throw new Error('Detached HEAD state — check out a branch first.');

  return bitbucket.createPullRequest({
    projectKey: parsed.projectKey,
    repoSlug: parsed.repoSlug,
    title: args.title,
    description: args.description,
    fromBranch: branch,
    toBranch: args.toBranch,
    reviewers: args.reviewers,
  });
}
