import { execSync } from 'child_process';

type ToolResult = { content: Array<{ type: 'text'; text: string }> };

const JIRA_KEY_RE = /\b[A-Z][A-Z0-9]+-\d+\b/g;

function text(t: string): ToolResult {
  return { content: [{ type: 'text', text: t }] };
}

function git(cmd: string, cwd: string): string {
  return execSync(`git ${cmd}`, { cwd, encoding: 'utf-8' }).trim();
}

function safeGit(cmd: string, cwd: string, fallback = ''): string {
  try {
    return git(cmd, cwd);
  } catch {
    return fallback;
  }
}

export function getContext(args: { repoPath?: string; commitLimit?: number; includeDiff?: boolean }): ToolResult {
  const repoPath = args.repoPath ?? process.cwd();
  const limit = args.commitLimit ?? 10;
  try {
    const branch = safeGit('rev-parse --abbrev-ref HEAD', repoPath, '(unknown)');
    const remote = safeGit('remote get-url origin', repoPath, '(no remote)');
    const commits = safeGit(`log --oneline -${limit}`, repoPath, '(no commits)');
    const status = safeGit('status --short', repoPath, '');

    // Upstream tracking
    const upstream = safeGit('rev-parse --abbrev-ref @{u}', repoPath, '');
    let upstreamLine = '';
    if (upstream) {
      const ab = safeGit(`rev-list --left-right --count ${upstream}...HEAD`, repoPath, '');
      if (ab.includes('\t')) {
        const [behind, ahead] = ab.split('\t').map(Number);
        const parts: string[] = [];
        if (ahead) parts.push(`${ahead} ahead`);
        if (behind) parts.push(`${behind} behind`);
        upstreamLine = `${upstream}${parts.length ? ` (${parts.join(', ')})` : ' (up to date)'}`;
      }
    }

    // Diff stat summary
    const diffStatLines = safeGit('diff HEAD --stat', repoPath, '').split('\n').filter(Boolean);
    const diffStat = diffStatLines[diffStatLines.length - 1]?.trim() ?? '';

    const jiraKeys = [...new Set(branch.match(JIRA_KEY_RE) ?? [])];

    const lines = [
      `Repository: ${repoPath}`,
      `Branch:     ${branch}`,
      ...(upstreamLine ? [`Upstream:   ${upstreamLine}`] : []),
      `Remote:     ${remote}`,
      ...(jiraKeys.length ? [`Jira:       ${jiraKeys.join(', ')}`] : []),
      '',
      `Recent commits (last ${limit}):`,
      commits || '(none)',
      '',
      'Working tree:',
    ];

    if (status) {
      lines.push(status);
      if (diffStat) lines.push('', `Diff stat:  ${diffStat}`);
    } else {
      lines.push('(clean)');
    }

    if (args.includeDiff && status) {
      const diff = safeGit('diff HEAD', repoPath, '');
      if (diff) {
        const MAX = 6000;
        lines.push(
          '',
          '── Uncommitted diff ──',
          diff.length > MAX ? diff.slice(0, MAX) + `\n\n... (truncated, ${diff.length - MAX} more chars)` : diff,
        );
      }
    }

    return text(lines.join('\n'));
  } catch (err) {
    return text(`Error reading git context: ${(err as Error).message}`);
  }
}

export function getCommits(args: {
  repoPath?: string;
  limit?: number;
  branch?: string;
}): ToolResult {
  const repoPath = args.repoPath ?? process.cwd();
  const limit = args.limit ?? 20;
  const branch = args.branch ?? '';
  try {
    const format = '%H|%an|%ad|%s';
    const cmd = `log --pretty=format:"${format}" --date=short -${limit}${branch ? ` ${branch}` : ''}`;
    const raw = safeGit(cmd, repoPath, '');
    if (!raw) return text('No commits found.');
    const lines = raw.split('\n').map((line) => {
      const [hash, author, date, ...msgParts] = line.replace(/^"|"$/g, '').split('|');
      return `${hash?.slice(0, 8)} ${date} ${author}: ${msgParts.join('|')}`;
    });
    return text(`Last ${lines.length} commit(s)${branch ? ` on ${branch}` : ''}:\n${lines.join('\n')}`);
  } catch (err) {
    return text(`Error reading commits: ${(err as Error).message}`);
  }
}

export function getDiff(args: {
  repoPath?: string;
  fromRef?: string;
  toRef?: string;
}): ToolResult {
  const repoPath = args.repoPath ?? process.cwd();
  try {
    let cmd: string;
    if (args.fromRef && args.toRef) {
      cmd = `diff ${args.fromRef} ${args.toRef}`;
    } else if (args.fromRef) {
      cmd = `diff ${args.fromRef}`;
    } else {
      cmd = 'diff HEAD';
    }
    const diff = safeGit(cmd, repoPath, '');
    if (!diff) return text('No differences found.');
    return text(diff);
  } catch (err) {
    return text(`Error reading diff: ${(err as Error).message}`);
  }
}

export function checkRemoteBranch(branchName: string, repoPath: string): {
  exists: boolean; author?: string; date?: string; message?: string; sha?: string;
} {
  const lsRemote = safeGit(`ls-remote --heads origin refs/heads/${branchName}`, repoPath);
  if (!lsRemote) return { exists: false };
  const sha = lsRemote.split(/\s+/)[0]?.trim();
  // Fetch so we can read the log
  safeGit(`fetch origin ${branchName}`, repoPath);
  const log = safeGit(`log origin/${branchName} -1 --format=%an%x09%ae%x09%ad%x09%s`, repoPath);
  if (!log) return { exists: true, sha: sha?.slice(0, 8) };
  const [author, email, date, ...msgParts] = log.split('\t');
  return {
    exists: true,
    sha: sha?.slice(0, 8),
    author: email ? `${author} <${email}>` : author,
    date,
    message: msgParts.join('\t'),
  };
}

function getDefaultBranch(repoPath: string): string {
  const head = safeGit('rev-parse --abbrev-ref origin/HEAD', repoPath);
  if (head && head.startsWith('origin/')) return head.slice('origin/'.length);
  // origin/HEAD not set — probe common defaults
  if (safeGit('rev-parse --verify origin/main', repoPath)) return 'main';
  return 'master';
}

export function checkoutRemoteBranch(branchName: string, repoPath: string): ToolResult {
  try {
    const existing = safeGit(`branch --list ${branchName}`, repoPath);
    if (existing.trim()) {
      git(`checkout ${branchName}`, repoPath);
      return text(`Switched to existing local branch "${branchName}".`);
    }
    git(`checkout --track origin/${branchName}`, repoPath);
    return text(`Checked out "${branchName}" tracking origin/${branchName}.`);
  } catch (err) {
    return text(`Error checking out branch: ${(err as Error).message}`);
  }
}

export function createBranch(args: {
  branchName: string;
  baseBranch?: string;
  repoPath?: string;
  push?: boolean;
}): ToolResult {
  const repoPath = args.repoPath ?? process.cwd();
  const { branchName, push = false } = args;
  const baseBranch = args.baseBranch ?? getDefaultBranch(repoPath);
  try {
    if (!/^[a-zA-Z0-9/_.\-]+$/.test(branchName)) {
      return text(`Invalid branch name "${branchName}". Use only letters, numbers, /, _, ., -`);
    }
    const existing = safeGit(`branch --list ${branchName}`, repoPath);
    if (existing.trim()) {
      return text(`Branch "${branchName}" already exists locally. Switch with: git checkout ${branchName}`);
    }
    safeGit(`fetch origin ${baseBranch}`, repoPath);
    git(`checkout -b ${branchName} origin/${baseBranch}`, repoPath);
    const lines = [`Created and switched to branch "${branchName}" from origin/${baseBranch}.`];
    if (push) {
      git(`push -u origin ${branchName}`, repoPath);
      lines.push(`Pushed to origin/${branchName} and set upstream.`);
    }
    return text(lines.join('\n'));
  } catch (err) {
    return text(`Error creating branch: ${(err as Error).message}`);
  }
}
