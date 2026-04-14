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

export function getContext(args: { repoPath?: string; commitLimit?: number }): ToolResult {
  const repoPath = args.repoPath ?? process.cwd();
  const limit = args.commitLimit ?? 10;
  try {
    const branch = safeGit('rev-parse --abbrev-ref HEAD', repoPath, '(unknown)');
    const remote = safeGit('remote get-url origin', repoPath, '(no remote)');
    const commits = safeGit(`log --oneline -${limit}`, repoPath, '(no commits)');
    const status = safeGit('status --short', repoPath, '');

    const jiraKeys = [...new Set(branch.match(JIRA_KEY_RE) ?? [])];

    const lines = [
      `Repository: ${repoPath}`,
      `Branch:     ${branch}`,
      `Remote:     ${remote}`,
    ];

    if (jiraKeys.length > 0) {
      lines.push(`Jira issue(s) detected in branch: ${jiraKeys.join(', ')}`);
    }

    lines.push(
      '',
      `Recent commits (last ${limit}):`,
      commits || '(none)',
      '',
      'Working tree status:',
      status || '(clean)',
    );

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
  const MAX_CHARS = 8000;
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
    if (diff.length > MAX_CHARS) {
      return text(diff.slice(0, MAX_CHARS) + `\n\n... (truncated, ${diff.length - MAX_CHARS} more chars)`);
    }
    return text(diff);
  } catch (err) {
    return text(`Error reading diff: ${(err as Error).message}`);
  }
}
