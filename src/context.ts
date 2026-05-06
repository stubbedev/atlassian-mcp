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

interface Committer {
  name: string;
  email: string;
  commits: number;
}

/** Top recent committers from the last `lookback` commits, ranked by count. */
export function getTopCommitters(repoPath: string, lookback = 50, top = 5): Committer[] {
  const raw = safeExec(`git log -n ${lookback} --format=%aN%x09%aE`, repoPath);
  if (!raw) return [];
  const counts = new Map<string, Committer>();
  for (const line of raw.split('\n')) {
    const [name, email] = line.split('\t');
    if (!name) continue;
    const key = (email || name).toLowerCase();
    const existing = counts.get(key);
    if (existing) existing.commits++;
    else counts.set(key, { name, email: email ?? '', commits: 1 });
  }
  return [...counts.values()]
    .sort((a, b) => b.commits - a.commits)
    .slice(0, top);
}

/**
 * Unified developer context: git state + linked Jira issues + open PR for current branch.
 * Either jira or bitbucket may be null when only one product is configured.
 */
export async function getDevContext(
  args: { repoPath?: string },
  jira: JiraClient | null,
  bitbucket: BitbucketClient | null
): Promise<ToolResult> {
  const repoPath = args.repoPath ?? process.cwd();
  const sections: string[] = [];

  const branch = safeExec('git rev-parse --abbrev-ref HEAD', repoPath) || '(unknown)';
  const remote = safeExec('git remote get-url origin', repoPath) || '(no remote)';
  const recentCommits = safeExec('git log --oneline -5', repoPath) || '(none)';
  const status = safeExec('git status --short', repoPath) || '(clean)';
  const committers = getTopCommitters(repoPath, 50, 5);
  const parsed = bitbucket?.isRemoteForThisInstance(remote) ? parseBitbucketRemote(remote) : null;

  // Upstream ahead/behind
  const upstream = safeExec('git rev-parse --abbrev-ref @{u}', repoPath);
  let upstreamLine = '';
  if (upstream) {
    const ab = safeExec(`git rev-list --left-right --count ${upstream}...HEAD`, repoPath);
    if (ab.includes('\t')) {
      const [behind, ahead] = ab.split('\t').map(Number);
      const parts: string[] = [];
      if (ahead) parts.push(`${ahead} ahead`);
      if (behind) parts.push(`${behind} behind`);
      upstreamLine = `${upstream}${parts.length ? ` (${parts.join(', ')})` : ' (up to date)'}`;
    }
  }

  // Identity — best-effort, parallel
  const [jiraMe, bbMe] = await Promise.all([
    jira ? jira.whoami().catch(() => null) : Promise.resolve(null),
    bitbucket ? bitbucket.whoami().catch(() => null) : Promise.resolve(null),
  ]);
  const youParts: string[] = [];
  if (jiraMe) youParts.push(`Jira ${jiraMe.name ?? jiraMe.key ?? '(unknown)'}${jiraMe.displayName ? ` "${jiraMe.displayName}"` : ''}`);
  if (bbMe)   youParts.push(`Bitbucket ${bbMe}`);

  const headerLines: string[] = [
    `Repository: ${repoPath}`,
    `Branch:     ${branch}`,
    ...(upstreamLine ? [`Upstream:   ${upstreamLine}`] : []),
    `Remote:     ${remote}`,
    ...(parsed ? [`Bitbucket:  ${parsed.projectKey}/${parsed.repoSlug}`] : []),
    ...(youParts.length ? [`You:        ${youParts.join(' · ')}`] : []),
  ];
  if (committers.length) {
    headerLines.push('');
    headerLines.push('Top recent committers (last 50):');
    for (const c of committers) {
      const ident = c.email ? `${c.name} <${c.email}>` : c.name;
      headerLines.push(`  ${c.commits.toString().padStart(3)} — ${ident}`);
    }
    headerLines.push('  (look up usernames via jira_search resource=users / bitbucket_search resource=users — do NOT shell out to git/gh/bb)');
  }
  headerLines.push('');
  headerLines.push('Recent commits:');
  headerLines.push(recentCommits);
  headerLines.push('');
  headerLines.push('Working tree:');
  headerLines.push(status);

  sections.push(headerLines.join('\n'));

  // Jira — fetch overview for any tickets referenced in the branch name (parallel)
  const jiraKeys = jira ? [...new Set(branch.match(JIRA_KEY_RE) ?? [])] : [];
  const jiraResults = await Promise.all(
    jiraKeys.map(async (key) => {
      try {
        const result = await jira!.issueOverview({
          issueKey: key,
          includeComments: true,
          commentsMaxResults: 5,
          includeTransitions: true,
          includeSprint: true,
        });
        return `── Jira ${key} ──\n${result.content[0].text}`;
      } catch {
        return `── Jira ${key} ── (could not fetch)`;
      }
    })
  );
  sections.push(...jiraResults);

  // Bitbucket — find the open PR for this branch (only if remote points to this instance)
  if (parsed) {
    try {
      const pr = await bitbucket!.findOpenPrForBranch(parsed.projectKey, parsed.repoSlug, branch);
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
