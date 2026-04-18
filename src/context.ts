import { execSync } from 'child_process';
import type { JiraClient } from './jira.js';
import type { BitbucketClient } from './bitbucket.js';
import { parseBitbucketRemote } from './bitbucket.js';

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

  // Jira — fetch overview for any tickets referenced in the branch name
  const jiraKeys = [...new Set(branch.match(JIRA_KEY_RE) ?? [])];
  for (const key of jiraKeys) {
    try {
      const result = await jira.issueOverview({
        issueKey: key,
        includeComments: true,
        commentsMaxResults: 5,
        includeTransitions: true,
        includeSprint: true,
      });
      sections.push(`── Jira ${key} ──\n${result.content[0].text}`);
    } catch {
      sections.push(`── Jira ${key} ── (could not fetch)`);
    }
  }

  // Bitbucket — find the open PR for this branch (only if remote points to this instance)
  const parsed = bitbucket.isRemoteForThisInstance(remote) ? parseBitbucketRemote(remote) : null;
  if (parsed) {
    try {
      const pr = await bitbucket.findOpenPrForBranch(parsed.projectKey, parsed.repoSlug, branch);
      if (pr) {
        const approved = pr.reviewers.filter((r) => r.approved).length;
        const total = pr.reviewers.length;
        const reviewers = pr.reviewers.map((r) => `${r.user.displayName}${r.approved ? ' ✓' : ''}`).join(', ');
        const url = pr.links?.self?.[0]?.href ?? '';
        const descSnippet = pr.description
          ? pr.description.slice(0, 200) + (pr.description.length > 200 ? '…' : '')
          : '';
        const prLines = [
          `── PR #${pr.id}: ${pr.title} ──`,
          `State:     ${pr.state}`,
          `Author:    ${pr.author.user.displayName}`,
          `Branch:    ${pr.fromRef.displayId} → ${pr.toRef.displayId}`,
          `Reviewers: ${reviewers || 'none'} (${approved}/${total} approved)`,
        ];
        if (url) prLines.push(`URL:       ${url}`);
        if (descSnippet) prLines.push(``, descSnippet);

        // Workflow hint
        if (total > 0 && approved === total) {
          prLines.push(``, `✓ All reviewers approved — ready to merge: bitbucket_mutate {prId: ${pr.id}, action: "merge"}`);
        } else if (total > 0) {
          prLines.push(``, `→ ${total - approved} reviewer(s) pending — use bitbucket_get_pr {prId: ${pr.id}} to see open comments`);
        } else {
          prLines.push(``, `→ No reviewers assigned — use bitbucket_mutate {prId: ${pr.id}, update: {reviewers: [...]}} to add them`);
        }
        sections.push(prLines.join('\n'));
      } else {
        const noPrHint = [
          `── Bitbucket (${parsed.projectKey}/${parsed.repoSlug}) ── No open PR for branch "${branch}"`,
          `→ Create one: bitbucket_mutate {create: {title: "...", fromBranch: "${branch}"}}`,
        ];
        sections.push(noPrHint.join('\n'));
      }
    } catch {
      sections.push(`── Bitbucket (${parsed.projectKey}/${parsed.repoSlug}) ── (could not fetch PRs)`);
    }
  }

  return { content: [{ type: 'text', text: sections.join('\n\n') }] };
}
