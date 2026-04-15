import { execSync } from 'child_process';

type ToolResult = { content: Array<{ type: 'text'; text: string }> };
const EMOJI_RE = /\p{Extended_Pictographic}/u;

function safeExec(cmd: string): string {
  try {
    return execSync(cmd, { encoding: 'utf-8' }).trim();
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
  const sshUrl = remoteUrl.match(/ssh:\/\/[^/]+\/([^/]+)\/([^/]+?)(?:\.git)?$/);
  if (sshUrl) return { projectKey: sshUrl[1], repoSlug: sshUrl[2] };
  const scpUrl = remoteUrl.match(/^[^@]+@[^:]+:([^/]+)\/([^/]+?)(?:\.git)?$/);
  if (scpUrl) return { projectKey: scpUrl[1], repoSlug: scpUrl[2] };
  const httpUrl = remoteUrl.match(/\/scm\/([^/]+)\/([^/]+?)(?:\.git)?$/);
  if (httpUrl) return { projectKey: httpUrl[1], repoSlug: httpUrl[2] };
  return null;
}

interface BBRepo {
  slug: string;
  name: string;
  project: { key: string; name: string };
}

interface BBPagedResult<T> {
  values: T[];
  size: number;
  isLastPage: boolean;
  nextPageStart?: number;
  start: number;
}

interface BBPullRequest {
  id: number;
  version: number;
  title: string;
  description?: string;
  state: string;
  author: { user: { displayName: string; name: string } };
  fromRef: { displayId: string; repository: { slug: string; project: { key: string } } };
  toRef: { displayId: string; repository: { slug: string; project: { key: string } } };
  reviewers: Array<{ user: { displayName: string }; approved: boolean }>;
  links?: { self?: Array<{ href: string }> };
}

interface BBComment {
  id: number;
  version: number;
  text: string;
  state?: 'OPEN' | 'RESOLVED' | 'PENDING';
  severity?: 'NORMAL' | 'BLOCKER';
  author?: { displayName?: string; name?: string };
  createdDate?: number;
  updatedDate?: number;
  comments?: BBComment[];
}

interface BBActivity {
  action: string;
  comment?: BBComment;
}

interface BBTaskCount {
  open?: number;
  resolved?: number;
  values?: Array<{ state?: string; count?: number }>;
}

interface BBBranch {
  id: string;
  displayId: string;
  latestCommit: string;
  isDefault: boolean;
}

interface BBDiffSegment {
  type: 'ADDED' | 'REMOVED' | 'CONTEXT';
  lines?: Array<{ line: string }>;
}

interface BBDiff {
  diffs: Array<{
    source?: { toString: string };
    destination?: { toString: string };
    hunks?: Array<{ segments?: BBDiffSegment[] }>;
  }>;
}

interface BBParticipant {
  user: { displayName: string };
  approved: boolean;
  status: string;
}

interface BBCommit {
  id: string;
  displayId: string;
  author: { name: string };
  authorTimestamp: number;
  message: string;
}

interface BitbucketErrorPayload {
  errors?: Array<{ message?: string; context?: string }>;
}

function text(t: string): ToolResult {
  return { content: [{ type: 'text', text: t }] };
}

function toBranchRef(branch: string): string {
  return branch.startsWith('refs/') ? branch : `refs/heads/${branch}`;
}

function formatDate(ms: number): string {
  return new Date(ms).toISOString().slice(0, 10);
}

function formatCommentThread(comment: BBComment, indent = ''): string[] {
  const author = comment.author?.displayName ?? comment.author?.name ?? 'Unknown';
  const date = comment.createdDate ? ` (${formatDate(comment.createdDate)})` : '';
  const state = comment.state ?? 'OPEN';
  const severity = comment.severity ?? 'NORMAL';
  const lines = [
    `${indent}#${comment.id} [${state}/${severity}] ${author}${date} (v${comment.version})`,
    `${indent}${comment.text}`,
  ];

  if (comment.comments && comment.comments.length > 0) {
    for (const reply of comment.comments) {
      lines.push(...formatCommentThread(reply, `${indent}  `));
    }
  }

  return lines;
}

function commentMatchesState(comment: BBComment, state: 'OPEN' | 'RESOLVED' | 'PENDING'): boolean {
  const currentState = comment.state ?? 'OPEN';
  if (currentState === state) return true;
  return (comment.comments ?? []).some((child) => commentMatchesState(child, state));
}

function commentMatchesSeverity(comment: BBComment, severity: 'ALL' | 'NORMAL' | 'BLOCKER'): boolean {
  if (severity === 'ALL') return true;
  const currentSeverity = comment.severity ?? 'NORMAL';
  if (currentSeverity === severity) return true;
  return (comment.comments ?? []).some((child) => commentMatchesSeverity(child, severity));
}

function uniqueCommentsFromActivities(activities: BBActivity[]): BBComment[] {
  const byId = new Map<number, BBComment>();
  for (const activity of activities) {
    if (activity.action === 'COMMENTED' && activity.comment && !byId.has(activity.comment.id)) {
      byId.set(activity.comment.id, activity.comment);
    }
  }
  return Array.from(byId.values()).sort((a, b) => (a.createdDate ?? 0) - (b.createdDate ?? 0));
}

function pageHint(data: BBPagedResult<unknown>): string {
  return data.isLastPage ? '' : ` (use start=${data.nextPageStart} for next page)`;
}

function formatDiff(data: BBDiff, maxChars = 8000): string {
  const parts: string[] = [];
  for (const diff of data.diffs) {
    const from = diff.source?.toString ?? '/dev/null';
    const to = diff.destination?.toString ?? '/dev/null';
    parts.push(`--- a/${from}\n+++ b/${to}`);
    for (const hunk of diff.hunks ?? []) {
      for (const segment of hunk.segments ?? []) {
        const prefix = segment.type === 'ADDED' ? '+' : segment.type === 'REMOVED' ? '-' : ' ';
        for (const line of segment.lines ?? []) {
          parts.push(`${prefix}${line.line}`);
        }
      }
    }
  }
  const result = parts.join('\n');
  if (!result) return '(no diff)';
  if (result.length > maxChars) {
    return result.slice(0, maxChars) + `\n\n... (truncated, ${result.length - maxChars} more chars)`;
  }
  return result;
}

function parseBitbucketErrorDetails(errText: string): string {
  const trimmed = errText.trim();
  if (!trimmed) return '';

  try {
    const parsed = JSON.parse(trimmed) as BitbucketErrorPayload;
    if (Array.isArray(parsed.errors) && parsed.errors.length > 0) {
      const messages = parsed.errors
        .map((e) => {
          const msg = e.message?.trim() ?? '';
          if (!msg) return '';
          return e.context ? `${e.context}: ${msg}` : msg;
        })
        .filter((m) => m.length > 0);
      if (messages.length > 0) return messages.join(' | ');
    }
  } catch {
    // Fallback to raw text below
  }

  return trimmed.length > 500 ? `${trimmed.slice(0, 500)}...` : trimmed;
}

function formatBitbucketError(status: number, method: string, path: string, details: string): string {
  const prefix = `Bitbucket ${status} ${method} ${path}`;
  if (status === 400) return `${prefix}. Invalid request or parameters. ${details}`.trim();
  if (status === 401) return `${prefix}. Authentication failed. Check BITBUCKET_ACCESS_TOKEN.`;
  if (status === 403) return `${prefix}. Permission denied. Check repository/project permissions for this token.`;
  if (status === 404) return `${prefix}. Resource not found. Verify project/repo/PR identifiers and access.`;
  if (status === 409) return `${prefix}. Conflict (often stale version/state). Refresh and retry. ${details}`.trim();
  return details ? `${prefix}. ${details}` : prefix;
}

function validateCommentText(textValue: string): string {
  const trimmed = textValue.trim();
  if (!trimmed) {
    throw new Error('Bitbucket comment text must not be empty.');
  }
  if (EMOJI_RE.test(trimmed)) {
    throw new Error('Bitbucket comments must not include emoji. Use concise plain text only.');
  }
  return trimmed;
}

export class BitbucketClient {
  private baseUrl: string;
  private headers: Record<string, string>;

  constructor(baseUrl: string, token: string) {
    this.baseUrl = baseUrl.replace(/\/$/, '');
    this.headers = {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      Accept: 'application/json',
    };
  }

  private pullRequestUrl(projectKey: string, repoSlug: string, prId: number, pr?: BBPullRequest | null): string {
    const apiUrl = pr?.links?.self?.[0]?.href?.trim();
    if (apiUrl) {
      return apiUrl;
    }
    return `${this.baseUrl}/projects/${encodeURIComponent(projectKey)}/repos/${encodeURIComponent(repoSlug)}/pull-requests/${prId}`;
  }

  private resolveProjectAndRepo(
    projectKey?: string,
    repoSlug?: string
  ): { projectKey: string; repoSlug: string } {
    if (projectKey && repoSlug) return { projectKey, repoSlug };
    const remote = safeExec('git remote get-url origin');
    if (remote) {
      const parsed = parseBitbucketRemote(remote);
      if (parsed) {
        return {
          projectKey: projectKey ?? parsed.projectKey,
          repoSlug: repoSlug ?? parsed.repoSlug,
        };
      }
    }
    throw new Error(
      'Could not determine projectKey/repoSlug — provide them explicitly or run from a directory inside a git repo with a Bitbucket remote'
    );
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T | null> {
    const url = `${this.baseUrl}/rest/api/1.0${path}`;
    const opts: RequestInit = { method, headers: this.headers };
    if (body !== undefined) opts.body = JSON.stringify(body);
    const res = await fetch(url, opts);
    if (!res.ok) {
      const errText = await res.text();
      const details = parseBitbucketErrorDetails(errText);
      throw new Error(formatBitbucketError(res.status, method, path, details));
    }
    return res.status === 204 ? null : (res.json() as Promise<T>);
  }

  private async requestText(path: string): Promise<string> {
    const url = `${this.baseUrl}/rest/api/1.0${path}`;
    const res = await fetch(url, {
      method: 'GET',
      headers: { Authorization: this.headers.Authorization },
    });
    if (!res.ok) {
      const errText = await res.text();
      const details = parseBitbucketErrorDetails(errText);
      throw new Error(formatBitbucketError(res.status, 'GET', path, details));
    }
    return res.text();
  }

  // Used internally by context tools — finds the open PR for a given source branch
  async findOpenPrForBranch(projectKey: string, repoSlug: string, branch: string): Promise<BBPullRequest | null> {
    const data = await this.request<BBPagedResult<BBPullRequest>>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests?state=OPEN&limit=50`
    );
    return data?.values.find((pr) => pr.fromRef.displayId === branch) ?? null;
  }

  async listRepos(args: { projectKey?: string; limit?: number; start?: number }): Promise<ToolResult> {
    const { limit = 50, start = 0 } = args;
    const qs = `?limit=${limit}&start=${start}`;
    const path = args.projectKey
      ? `/projects/${args.projectKey}/repos${qs}`
      : `/repos${qs}`;
    const data = await this.request<BBPagedResult<BBRepo>>('GET', path);
    if (!data || data.values.length === 0) return text('No repositories found.');
    const lines = data.values.map((r, i) => `${start + i + 1}. ${r.project.key}/${r.slug} — ${r.name}`);
    return text(`${data.values.length} repo(s)${pageHint(data)}:\n${lines.join('\n')}`);
  }

  async listPullRequests(args: {
    projectKey?: string;
    repoSlug?: string;
    state?: 'OPEN' | 'MERGED' | 'DECLINED';
    fromBranch?: string;
    text?: string;
    limit?: number;
    start?: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const { state = 'OPEN', fromBranch, text: searchText, limit = 25, start = 0 } = args;
    const qs = new URLSearchParams({ state, limit: String(limit), start: String(start) });
    if (fromBranch) qs.set('at', toBranchRef(fromBranch));
    if (searchText) qs.set('filterText', searchText);
    const path = `/projects/${projectKey}/repos/${repoSlug}/pull-requests?${qs}`;
    const data = await this.request<BBPagedResult<BBPullRequest>>('GET', path);
    if (!data || data.values.length === 0) return text(`No ${state} pull requests found.`);
    const lines = data.values.map(
      (pr) => `#${pr.id} [${pr.state}] ${pr.title} | ${pr.fromRef.displayId} → ${pr.toRef.displayId} | by ${pr.author.user.displayName}`
    );
    return text(`${data.values.length} PR(s) (${state})${pageHint(data)}:\n${lines.join('\n')}`);
  }

  async myPrs(args: { limit?: number; start?: number; role?: 'author' | 'reviewer' | 'participant' }): Promise<ToolResult> {
    const { limit = 25, start = 0, role } = args;
    const qs = new URLSearchParams({ limit: String(limit), start: String(start) });
    if (role) qs.set('role', role);
    const data = await this.request<BBPagedResult<BBPullRequest>>(
      'GET',
      `/inbox/pull-requests?${qs}`
    );
    if (!data || data.values.length === 0) return text('No pull requests in your inbox.');
    const lines = data.values.map((pr) => {
      const repo = `${pr.toRef.repository.project.key}/${pr.toRef.repository.slug}`;
      return `#${pr.id} [${pr.state}] ${pr.title} | ${repo} | ${pr.fromRef.displayId} → ${pr.toRef.displayId}`;
    });
    return text(`${data.values.length} PR(s) in your inbox${pageHint(data)}:\n${lines.join('\n')}`);
  }

  async getPullRequest(args: { projectKey?: string; repoSlug?: string; prId: number }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const data = await this.request<BBPullRequest>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}`
    );
    if (!data) return text('Pull request not found.');
    const reviewers = data.reviewers
      .map((r) => `${r.user.displayName}${r.approved ? ' ✓' : ''}`)
      .join(', ');
    const lines = [
      `PR #${data.id}: ${data.title}`,
      `State:     ${data.state}`,
      `Author:    ${data.author.user.displayName}`,
      `Branch:    ${data.fromRef.displayId} → ${data.toRef.displayId}`,
      `Reviewers: ${reviewers || 'None'}`,
      '',
      'Description:',
      data.description ?? '(no description)',
    ];
    return text(lines.join('\n'));
  }

  async getPrOverview(args: {
    projectKey?: string;
    repoSlug?: string;
    prId: number;
    includeCommits?: boolean;
    includeComments?: boolean;
    includeDiff?: boolean;
    commentsState?: 'OPEN' | 'RESOLVED' | 'PENDING';
    commentsSeverity?: 'ALL' | 'NORMAL' | 'BLOCKER';
    commentsLimit?: number;
    commentsStart?: number;
    commitsLimit?: number;
    commitsStart?: number;
    diffMaxChars?: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const includeCommits = args.includeCommits ?? true;
    const includeComments = args.includeComments ?? true;
    const includeDiff = args.includeDiff ?? false;

    const pr = await this.request<BBPullRequest>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}`
    );
    if (!pr) return text('Pull request not found.');

    const sections: string[] = [];
    const reviewers = pr.reviewers.map((r) => `${r.user.displayName}${r.approved ? ' ✓' : ''}`).join(', ');
    const url = pr.links?.self?.[0]?.href;
    const header = [
      `PR #${pr.id}: ${pr.title}`,
      `State:     ${pr.state}`,
      `Author:    ${pr.author.user.displayName}`,
      `Branch:    ${pr.fromRef.displayId} → ${pr.toRef.displayId}`,
      `Reviewers: ${reviewers || 'None'}`,
      url ? `URL:       ${url}` : '',
      '',
      'Description:',
      pr.description ?? '(no description)',
    ].filter((line) => line !== '');
    sections.push(header.join('\n'));

    if (includeCommits) {
      const commitsLimit = args.commitsLimit ?? 25;
      const commitsStart = args.commitsStart ?? 0;
      const data = await this.request<BBPagedResult<BBCommit>>(
        'GET',
        `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/commits?limit=${commitsLimit}&start=${commitsStart}`
      );
      if (!data || data.values.length === 0) {
        sections.push('Commits:\n(no commits found)');
      } else {
        const lines = data.values.map(
          (c) => `${c.displayId} ${formatDate(c.authorTimestamp)} ${c.author.name}: ${c.message.split('\n')[0]}`
        );
        sections.push(`Commits (${data.values.length})${pageHint(data)}:\n${lines.join('\n')}`);
      }
    }

    if (includeComments) {
      const commentsLimit = args.commentsLimit ?? 50;
      const commentsStart = args.commentsStart ?? 0;
      const commentsState = args.commentsState ?? 'OPEN';
      const commentsSeverity = args.commentsSeverity ?? 'ALL';

      if (commentsSeverity === 'BLOCKER' && commentsState === 'PENDING') {
        throw new Error('commentsState=PENDING is not valid when commentsSeverity=BLOCKER. Use OPEN or RESOLVED.');
      }

      if (commentsSeverity === 'BLOCKER') {
        const qs = new URLSearchParams({ limit: String(commentsLimit), start: String(commentsStart), state: commentsState });
        const data = await this.request<BBPagedResult<BBComment>>(
          'GET',
          `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/blocker-comments?${qs}`
        );
        if (!data || data.values.length === 0) {
          sections.push(`Comments:\n(no ${commentsState} BLOCKER comments)`);
        } else {
          const blocks = data.values.flatMap((comment) => formatCommentThread(comment));
          sections.push(`Comments (${data.values.length} BLOCKER thread(s))${pageHint(data)}:\n\n${blocks.join('\n\n')}`);
        }
      } else {
        const activityData = await this.request<BBPagedResult<BBActivity>>(
          'GET',
          `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/activities?limit=${commentsLimit}&start=${commentsStart}`
        );
        const comments = uniqueCommentsFromActivities(activityData?.values ?? []).filter((comment) => {
          const matchesState = commentMatchesState(comment, commentsState);
          return matchesState && commentMatchesSeverity(comment, commentsSeverity);
        });
        if (comments.length === 0) {
          sections.push('Comments:\n(no matching comments)');
        } else {
          const blocks = comments.flatMap((comment) => formatCommentThread(comment));
          const paging = activityData ? pageHint(activityData) : '';
          sections.push(`Comments (${comments.length} thread(s))${paging}:\n\n${blocks.join('\n\n')}`);
        }
      }
    }

    if (includeDiff) {
      const data = await this.request<BBDiff>(
        'GET',
        `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/diff`
      );
      sections.push(`Diff:\n${data ? formatDiff(data, args.diffMaxChars ?? 8000) : '(no diff found)'}`);
    }

    return text(sections.join('\n\n'));
  }

  async getPrDiff(args: { projectKey?: string; repoSlug?: string; prId: number }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const data = await this.request<BBDiff>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/diff`
    );
    if (!data) return text('No diff found.');
    return text(formatDiff(data));
  }

  async getPrCommits(args: { projectKey?: string; repoSlug?: string; prId: number; limit?: number; start?: number }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const limit = args.limit ?? 25;
    const start = args.start ?? 0;
    const data = await this.request<BBPagedResult<BBCommit>>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/commits?limit=${limit}&start=${start}`
    );
    if (!data || data.values.length === 0) return text('No commits found.');
    const lines = data.values.map(
      (c) => `${c.displayId} ${formatDate(c.authorTimestamp)} ${c.author.name}: ${c.message.split('\n')[0]}`
    );
    return text(`${data.values.length} commit(s)${pageHint(data)}:\n${lines.join('\n')}`);
  }

  async createPullRequest(args: {
    projectKey?: string;
    repoSlug?: string;
    title: string;
    description?: string;
    fromBranch?: string;
    toBranch?: string;
    reviewers?: string[];
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const sourceBranch = args.fromBranch ?? safeExec('git rev-parse --abbrev-ref HEAD');
    if (!sourceBranch || sourceBranch === 'HEAD') {
      throw new Error('Could not determine source branch. Provide fromBranch or run from a checked-out branch.');
    }
    const { title, description, toBranch = 'master', reviewers = [] } = args;
    const body = {
      title,
      description: description ?? '',
      fromRef: { id: toBranchRef(sourceBranch), repository: { slug: repoSlug, project: { key: projectKey } } },
      toRef:   { id: toBranchRef(toBranch),   repository: { slug: repoSlug, project: { key: projectKey } } },
      reviewers: reviewers.map((name) => ({ user: { name } })),
    };
    const data = await this.request<BBPullRequest>(
      'POST',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests`,
      body
    );
    if (!data) return text('Pull request created.');
    const url = this.pullRequestUrl(projectKey, repoSlug, data.id, data);
    return text(`Created PR #${data.id}: "${data.title}"\n${url}`);
  }

  async approvePr(args: { projectKey?: string; repoSlug?: string; prId: number }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const data = await this.request<BBParticipant>(
      'POST',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/approve`
    );
    const url = this.pullRequestUrl(projectKey, repoSlug, args.prId);
    if (!data) return text(`Approved PR #${args.prId}.\n${url}`);
    return text(`Approved PR #${args.prId} as ${data.user.displayName}.\n${url}`);
  }

  async unapprovePr(args: { projectKey?: string; repoSlug?: string; prId: number }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    await this.request(
      'DELETE',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/approve`
    );
    return text(`Approval removed from PR #${args.prId}.\n${this.pullRequestUrl(projectKey, repoSlug, args.prId)}`);
  }

  async declinePr(args: { projectKey?: string; repoSlug?: string; prId: number; message?: string }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const pr = await this.request<BBPullRequest>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}`
    );
    if (!pr) throw new Error(`PR #${args.prId} not found.`);
    const body: Record<string, unknown> = { version: pr.version };
    if (args.message) body.message = args.message;
    const data = await this.request<BBPullRequest>(
      'POST',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/decline`,
      body
    );
    if (!data) return text(`Declined PR #${args.prId}.\n${this.pullRequestUrl(projectKey, repoSlug, args.prId)}`);
    return text(`Declined PR #${data.id}: "${data.title}".\n${this.pullRequestUrl(projectKey, repoSlug, data.id, data)}`);
  }

  async mergePr(args: {
    projectKey?: string;
    repoSlug?: string;
    prId: number;
    mergeStrategy?: 'MERGE_COMMIT' | 'SQUASH' | 'FAST_FORWARD';
    message?: string;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const pr = await this.request<BBPullRequest>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}`
    );
    if (!pr) throw new Error(`PR #${args.prId} not found.`);
    const body: Record<string, unknown> = { version: pr.version };
    if (args.mergeStrategy) body.strategyId = args.mergeStrategy;
    if (args.message) body.message = args.message;
    const data = await this.request<BBPullRequest>(
      'POST',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/merge`,
      body
    );
    if (!data) return text(`Merged PR #${args.prId}.\n${this.pullRequestUrl(projectKey, repoSlug, args.prId)}`);
    return text(`Merged PR #${data.id}: "${data.title}" (${data.fromRef.displayId} → ${data.toRef.displayId}).\n${this.pullRequestUrl(projectKey, repoSlug, data.id, data)}`);
  }

  async getBranches(args: {
    projectKey?: string;
    repoSlug?: string;
    filter?: string;
    limit?: number;
    start?: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const qs = new URLSearchParams({ limit: String(args.limit ?? 25), start: String(args.start ?? 0) });
    if (args.filter) qs.set('filterText', args.filter);
    const data = await this.request<BBPagedResult<BBBranch>>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/branches?${qs}`
    );
    if (!data || data.values.length === 0) return text('No branches found.');
    const lines = data.values.map((b) => `${b.displayId}${b.isDefault ? ' (default)' : ''} — ${b.latestCommit.slice(0, 8)}`);
    return text(`${data.values.length} branch(es)${pageHint(data)}:\n${lines.join('\n')}`);
  }

  async getFile(args: {
    projectKey?: string;
    repoSlug?: string;
    path: string;
    ref?: string;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const qs = args.ref ? `?at=${encodeURIComponent(args.ref)}` : '';
    const encodedPath = args.path.split('/').map(encodeURIComponent).join('/');
    const content = await this.requestText(
      `/projects/${projectKey}/repos/${repoSlug}/raw/${encodedPath}${qs}`
    );
    const MAX_CHARS = 10000;
    if (content.length > MAX_CHARS) {
      return text(content.slice(0, MAX_CHARS) + `\n\n... (truncated, ${content.length - MAX_CHARS} more chars)`);
    }
    return text(content);
  }

  async getPrComments(args: {
    projectKey?: string;
    repoSlug?: string;
    prId: number;
    path?: string;
    state?: 'OPEN' | 'RESOLVED' | 'PENDING';
    severity?: 'ALL' | 'NORMAL' | 'BLOCKER';
    countOnly?: boolean;
    limit?: number;
    start?: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const limit = args.limit ?? 50;
    const start = args.start ?? 0;
    const severity = args.severity ?? 'ALL';
    const state = args.state ?? (args.countOnly ? undefined : 'OPEN');

    if (args.countOnly) {
      if (severity !== 'BLOCKER') {
        throw new Error('countOnly is supported only for BLOCKER severity. Set severity="BLOCKER".');
      }
      if (state === 'PENDING') {
        throw new Error('PENDING is not valid for blocker comment counts. Use OPEN or RESOLVED.');
      }

      const qs = new URLSearchParams({ count: 'true' });
      if (state) qs.set('state', state);
      const data = await this.request<BBTaskCount>(
        'GET',
        `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/blocker-comments?${qs}`
      );

      let open = data?.open ?? 0;
      let resolved = data?.resolved ?? 0;

      if ((open === 0 && resolved === 0) && data?.values && data.values.length > 0) {
        for (const v of data.values) {
          if ((v.state ?? '').toUpperCase() === 'OPEN') open = v.count ?? open;
          if ((v.state ?? '').toUpperCase() === 'RESOLVED') resolved = v.count ?? resolved;
        }
      }

      if (state === 'OPEN') return text(`PR #${args.prId} BLOCKER comments: OPEN=${open}`);
      if (state === 'RESOLVED') return text(`PR #${args.prId} BLOCKER comments: RESOLVED=${resolved}`);
      return text(`PR #${args.prId} BLOCKER comments: OPEN=${open}, RESOLVED=${resolved}`);
    }

    if (severity === 'BLOCKER' && !args.path) {
      if (state === 'PENDING') {
        throw new Error('PENDING is not valid for blocker comments. Use OPEN or RESOLVED.');
      }
      const qs = new URLSearchParams({ limit: String(limit), start: String(start) });
      if (state) qs.set('state', state);
      const data = await this.request<BBPagedResult<BBComment>>(
        'GET',
        `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/blocker-comments?${qs}`
      );
      if (!data || data.values.length === 0) {
        return text(`No ${state ?? 'OPEN/RESOLVED'} BLOCKER comments on PR #${args.prId}.`);
      }
      const blocks = data.values.flatMap((comment) => formatCommentThread(comment));
      return text(`${data.values.length} ${state ?? 'OPEN/RESOLVED'} BLOCKER comment thread(s) on PR #${args.prId}${pageHint(data)}:\n\n${blocks.join('\n\n')}`);
    }

    if (args.path) {
      const qs = new URLSearchParams({
        limit: String(limit),
        start: String(start),
        path: args.path,
      });
      if (state) qs.set('state', state);
      const data = await this.request<BBPagedResult<BBComment>>(
        'GET',
        `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/comments?${qs}`
      );
      const filtered = (data?.values ?? []).filter((comment) => {
        const matchesState = state ? commentMatchesState(comment, state) : true;
        return matchesState && commentMatchesSeverity(comment, severity);
      });
      if (filtered.length === 0) {
        return text(`No matching comments on PR #${args.prId} for path ${args.path}.`);
      }
      const blocks = filtered.flatMap((comment) => formatCommentThread(comment));
      const paging = data ? pageHint(data) : '';
      return text(`${filtered.length} comment thread(s) on PR #${args.prId} for ${args.path}${paging}:\n\n${blocks.join('\n\n')}`);
    }

    const activityData = await this.request<BBPagedResult<BBActivity>>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/activities?limit=${limit}&start=${start}`
    );
    const comments = uniqueCommentsFromActivities(activityData?.values ?? []).filter((comment) => {
      const matchesState = state ? commentMatchesState(comment, state) : true;
      return matchesState && commentMatchesSeverity(comment, severity);
    });
    if (comments.length === 0) {
      return text(`No matching comments on PR #${args.prId}.`);
    }
    const blocks = comments.flatMap((comment) => formatCommentThread(comment));
    const paging = activityData ? pageHint(activityData) : '';
    return text(`${comments.length} comment thread(s) on PR #${args.prId}${paging}:\n\n${blocks.join('\n\n')}`);
  }

  async addPrComment(args: {
    projectKey?: string;
    repoSlug?: string;
    prId: number;
    parentCommentId?: number;
    text: string;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const body: Record<string, unknown> = { text: validateCommentText(args.text) };
    if (args.parentCommentId) body.parent = { id: args.parentCommentId };
    const created = await this.request<BBComment>(
      'POST',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/comments`,
      body
    );
    if (!created) return text(`Comment added to PR #${args.prId}.`);
    if (args.parentCommentId) {
      return text(`Reply #${created.id} added to comment #${args.parentCommentId} on PR #${args.prId}.`);
    }
    return text(`Comment #${created.id} added to PR #${args.prId}.`);
  }

  async updatePrComment(args: {
    projectKey?: string;
    repoSlug?: string;
    prId: number;
    commentId: number;
    text?: string;
    state?: 'OPEN' | 'RESOLVED';
    severity?: 'NORMAL' | 'BLOCKER';
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    if (!args.text && !args.state && !args.severity) {
      throw new Error('At least one field is required: text, state, or severity');
    }

    const current = await this.request<BBComment>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/comments/${args.commentId}`
    );
    if (!current) throw new Error(`Comment #${args.commentId} not found.`);

    const body: Record<string, unknown> = {
      version: current.version,
      text: args.text !== undefined ? validateCommentText(args.text) : current.text,
    };
    if (args.state) body.state = args.state;
    if (args.severity) body.severity = args.severity;

    const updated = await this.request<BBComment>(
      'PUT',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/comments/${args.commentId}`,
      body
    );

    if (!updated) return text(`Comment #${args.commentId} updated.`);
    const state = updated.state ?? current.state ?? 'OPEN';
    const severity = updated.severity ?? current.severity ?? 'NORMAL';
    return text(`Comment #${updated.id} updated (${state}/${severity}).`);
  }

  async deletePrComment(args: {
    projectKey?: string;
    repoSlug?: string;
    prId: number;
    commentId: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const current = await this.request<BBComment>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/comments/${args.commentId}`
    );
    if (!current) throw new Error(`Comment #${args.commentId} not found.`);

    const path = `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${args.prId}/comments/${args.commentId}?version=${current.version}`;
    await this.request('DELETE', path);
    return text(`Comment #${args.commentId} deleted from PR #${args.prId}.`);
  }
}
