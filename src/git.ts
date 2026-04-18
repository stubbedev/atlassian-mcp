import { execFileSync } from 'child_process';

type ToolResult = { content: Array<{ type: 'text'; text: string }> };

const JIRA_KEY_RE = /\b[A-Z][A-Z0-9]+-\d+\b/g;

// Allowlist for git refs (commits, branches used as refs in diff commands)
const SAFE_REF_RE = /^[a-zA-Z0-9/_.\-@{}~^:]+(\.\.\.[a-zA-Z0-9/_.\-@{}~^:]+)?$/;
// Allowlist for branch names (stricter — no range syntax)
const SAFE_BRANCH_RE = /^[a-zA-Z0-9/_.\-]+$/;

function text(t: string): ToolResult {
  return { content: [{ type: 'text', text: t }] };
}

function git(args: string[], cwd: string): string {
  return execFileSync('git', args, { cwd, encoding: 'utf-8', stdio: ['pipe', 'pipe', 'pipe'] }).trim();
}

function safeGit(args: string[], cwd: string, fallback = ''): string {
  try {
    return git(args, cwd);
  } catch {
    return fallback;
  }
}

function validateRepoPath(repoPath: string): void {
  try {
    execFileSync('git', ['rev-parse', '--git-dir'], { cwd: repoPath, encoding: 'utf-8', stdio: 'pipe' });
  } catch {
    throw new Error(`Not a git repository: ${repoPath}`);
  }
}

function validateBranch(branch: string, label: string): void {
  if (!SAFE_BRANCH_RE.test(branch)) {
    throw new Error(`Invalid ${label} "${branch}". Use only letters, numbers, /, _, ., -`);
  }
}

function validateRef(ref: string, label: string): void {
  if (!SAFE_REF_RE.test(ref)) {
    throw new Error(`Invalid ${label} "${ref}". Use only safe git ref characters.`);
  }
}

export function getContext(args: { repoPath?: string; commitLimit?: number; includeDiff?: boolean }): ToolResult {
  const repoPath = args.repoPath ?? process.cwd();
  const limit = Math.max(1, Math.min(args.commitLimit ?? 10, 100));
  try {
    validateRepoPath(repoPath);
    const branch = safeGit(['rev-parse', '--abbrev-ref', 'HEAD'], repoPath, '(unknown)');
    const remote = safeGit(['remote', 'get-url', 'origin'], repoPath, '(no remote)');
    const commits = safeGit(['log', '--oneline', `-${limit}`], repoPath, '(no commits)');
    const status = safeGit(['status', '--short'], repoPath, '');

    // Upstream tracking
    const upstream = safeGit(['rev-parse', '--abbrev-ref', '@{u}'], repoPath, '');
    let upstreamLine = '';
    if (upstream) {
      const ab = safeGit(['rev-list', '--left-right', '--count', `${upstream}...HEAD`], repoPath, '');
      if (ab.includes('\t')) {
        const [behind, ahead] = ab.split('\t').map(Number);
        const parts: string[] = [];
        if (ahead) parts.push(`${ahead} ahead`);
        if (behind) parts.push(`${behind} behind`);
        upstreamLine = `${upstream}${parts.length ? ` (${parts.join(', ')})` : ' (up to date)'}`;
      }
    }

    // Diff stat summary
    const diffStatLines = safeGit(['diff', 'HEAD', '--stat'], repoPath, '').split('\n').filter(Boolean);
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
      const diff = safeGit(['diff', 'HEAD'], repoPath, '');
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

// Not exposed as an MCP tool — internal/experimental use only
export function getCommits(args: {
  repoPath?: string;
  limit?: number;
  branch?: string;
}): ToolResult {
  const repoPath = args.repoPath ?? process.cwd();
  const limit = Math.max(1, Math.min(args.limit ?? 20, 200));
  const branch = args.branch ?? '';
  try {
    validateRepoPath(repoPath);
    if (branch) validateBranch(branch, 'branch');
    const format = '%H|%an|%ad|%s';
    const gitArgs = ['log', `--pretty=format:${format}`, '--date=short', `-${limit}`, ...(branch ? [branch] : [])];
    const raw = safeGit(gitArgs, repoPath, '');
    if (!raw) return text('No commits found.');
    const lines = raw.split('\n').map((line) => {
      const [hash, author, date, ...msgParts] = line.split('|');
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
    validateRepoPath(repoPath);
    let gitArgs: string[];
    if (args.fromRef && args.toRef) {
      validateRef(args.fromRef, 'fromRef');
      validateRef(args.toRef, 'toRef');
      gitArgs = ['diff', args.fromRef, args.toRef];
    } else if (args.fromRef) {
      validateRef(args.fromRef, 'fromRef');
      gitArgs = ['diff', args.fromRef];
    } else {
      gitArgs = ['diff', 'HEAD'];
    }
    const diff = safeGit(gitArgs, repoPath, '');
    if (!diff) return text('No differences found.');
    return text(diff);
  } catch (err) {
    return text(`Error reading diff: ${(err as Error).message}`);
  }
}

export function checkRemoteBranch(branchName: string, repoPath: string): {
  exists: boolean; author?: string; date?: string; message?: string; sha?: string;
} {
  validateBranch(branchName, 'branchName');
  const lsRemote = safeGit(['ls-remote', '--heads', 'origin', `refs/heads/${branchName}`], repoPath);
  if (!lsRemote) return { exists: false };
  const sha = lsRemote.split(/\s+/)[0]?.trim();
  // Fetch so we can read the log — failure is non-fatal (network/credentials issue)
  try {
    git(['fetch', 'origin', branchName], repoPath);
  } catch {
    return { exists: true, sha: sha?.slice(0, 8) };
  }
  const log = safeGit(['log', `origin/${branchName}`, '-1', '--format=%an%x09%ae%x09%ad%x09%s'], repoPath);
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
  const head = safeGit(['rev-parse', '--abbrev-ref', 'origin/HEAD'], repoPath);
  if (head && head.startsWith('origin/')) return head.slice('origin/'.length);
  // origin/HEAD not set — probe common defaults
  if (safeGit(['rev-parse', '--verify', 'origin/main'], repoPath)) return 'main';
  return 'master';
}

export function checkoutRemoteBranch(branchName: string, repoPath: string): ToolResult {
  try {
    validateBranch(branchName, 'branchName');
    const existing = safeGit(['branch', '--list', branchName], repoPath);
    if (existing.trim()) {
      git(['checkout', branchName], repoPath);
      return text(`Switched to existing local branch "${branchName}".`);
    }
    git(['checkout', '--track', `origin/${branchName}`], repoPath);
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
  try {
    validateRepoPath(repoPath);
    if (!SAFE_BRANCH_RE.test(branchName)) {
      return text(`Invalid branch name "${branchName}". Use only letters, numbers, /, _, ., -`);
    }
    const baseBranch = args.baseBranch ?? getDefaultBranch(repoPath);
    if (!SAFE_BRANCH_RE.test(baseBranch)) {
      return text(`Invalid base branch name "${baseBranch}". Use only letters, numbers, /, _, ., -`);
    }
    const existing = safeGit(['branch', '--list', branchName], repoPath);
    if (existing.trim()) {
      return text(`Branch "${branchName}" already exists locally. Switch with: git checkout ${branchName}`);
    }
    safeGit(['fetch', 'origin', baseBranch], repoPath);
    git(['checkout', '-b', branchName, `origin/${baseBranch}`], repoPath);
    const lines = [`Created and switched to branch "${branchName}" from origin/${baseBranch}.`];
    if (push) {
      git(['push', '-u', 'origin', branchName], repoPath);
      lines.push(`Pushed to origin/${branchName} and set upstream.`);
    }
    return text(lines.join('\n'));
  } catch (err) {
    return text(`Error creating branch: ${(err as Error).message}`);
  }
}
