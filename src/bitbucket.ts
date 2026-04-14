type ToolResult = { content: Array<{ type: 'text'; text: string }> };

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

interface BBActivity {
  action: string;
  user?: { displayName: string };
  comment?: { text: string; createdDate: number };
}

interface BBBranch {
  id: string;
  displayId: string;
  latestCommit: string;
  isDefault: boolean;
}

interface BBDiffSegmentLine {
  line: string;
}

interface BBDiffSegment {
  type: 'ADDED' | 'REMOVED' | 'CONTEXT';
  lines?: BBDiffSegmentLine[];
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

function text(t: string): ToolResult {
  return { content: [{ type: 'text', text: t }] };
}

function toRef(branch: string): string {
  return branch.startsWith('refs/') ? branch : `refs/heads/${branch}`;
}

function formatDate(ms: number): string {
  return new Date(ms).toISOString().slice(0, 10);
}

function pagination(data: BBPagedResult<unknown>): string {
  if (data.isLastPage) return '';
  return ` (use start=${data.nextPageStart} for next page)`;
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

  private async request<T>(method: string, path: string, body?: unknown): Promise<T | null> {
    const url = `${this.baseUrl}/rest/api/1.0${path}`;
    const opts: RequestInit = { method, headers: this.headers };
    if (body !== undefined) opts.body = JSON.stringify(body);
    const res = await fetch(url, opts);
    if (!res.ok) {
      const errText = await res.text();
      throw new Error(`Bitbucket ${res.status} ${method} ${path}: ${errText}`);
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
      throw new Error(`Bitbucket ${res.status} GET ${path}: ${errText}`);
    }
    return res.text();
  }

  async listRepos(args: { projectKey?: string; limit?: number; start?: number }): Promise<ToolResult> {
    const { projectKey, limit = 50, start = 0 } = args;
    const qs = `?limit=${limit}&start=${start}`;
    const path = projectKey ? `/projects/${projectKey}/repos${qs}` : `/repos${qs}`;
    const data = await this.request<BBPagedResult<BBRepo>>('GET', path);
    if (!data || data.values.length === 0) return text('No repositories found.');
    const lines = data.values.map((r, i) => `${start + i + 1}. ${r.project.key}/${r.slug} — ${r.name}`);
    return text(`${data.values.length} repo(s)${pagination(data)}:\n${lines.join('\n')}`);
  }

  async listPullRequests(args: {
    projectKey: string;
    repoSlug: string;
    state?: string;
    limit?: number;
    start?: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, state = 'OPEN', limit = 25, start = 0 } = args;
    const path = `/projects/${projectKey}/repos/${repoSlug}/pull-requests?state=${state}&limit=${limit}&start=${start}`;
    const data = await this.request<BBPagedResult<BBPullRequest>>('GET', path);
    if (!data || data.values.length === 0) return text(`No ${state} pull requests found.`);
    const lines = data.values.map(
      (pr) => `#${pr.id} [${pr.state}] ${pr.title} | ${pr.fromRef.displayId} → ${pr.toRef.displayId} | by ${pr.author.user.displayName}`
    );
    return text(`${data.values.length} PR(s) (${state})${pagination(data)}:\n${lines.join('\n')}`);
  }

  async myPrs(args: { limit?: number; start?: number }): Promise<ToolResult> {
    const { limit = 25, start = 0 } = args;
    const data = await this.request<BBPagedResult<BBPullRequest>>(
      'GET',
      `/inbox/pull-requests?limit=${limit}&start=${start}`
    );
    if (!data || data.values.length === 0) return text('No pull requests in your inbox.');
    const lines = data.values.map((pr) => {
      const repo = `${pr.toRef.repository.project.key}/${pr.toRef.repository.slug}`;
      return `#${pr.id} [${pr.state}] ${pr.title} | ${repo} | ${pr.fromRef.displayId} → ${pr.toRef.displayId}`;
    });
    return text(`${data.values.length} PR(s) in your inbox${pagination(data)}:\n${lines.join('\n')}`);
  }

  async getPullRequest(args: {
    projectKey: string;
    repoSlug: string;
    prId: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, prId } = args;
    const data = await this.request<BBPullRequest>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${prId}`
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

  async getPrDiff(args: {
    projectKey: string;
    repoSlug: string;
    prId: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, prId } = args;
    const data = await this.request<BBDiff>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${prId}/diff`
    );
    if (!data) return text('No diff found.');
    return text(formatDiff(data));
  }

  async createPullRequest(args: {
    projectKey: string;
    repoSlug: string;
    title: string;
    description?: string;
    fromBranch: string;
    toBranch?: string;
    reviewers?: string[];
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, title, description, fromBranch, toBranch = 'master', reviewers = [] } = args;
    const body = {
      title,
      description: description ?? '',
      fromRef: {
        id: toRef(fromBranch),
        repository: { slug: repoSlug, project: { key: projectKey } },
      },
      toRef: {
        id: toRef(toBranch),
        repository: { slug: repoSlug, project: { key: projectKey } },
      },
      reviewers: reviewers.map((name) => ({ user: { name } })),
    };
    const data = await this.request<BBPullRequest>(
      'POST',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests`,
      body
    );
    if (!data) return text('Pull request created.');
    const url = data.links?.self?.[0]?.href ?? '';
    return text(`Created PR #${data.id}: "${data.title}"${url ? `\n${url}` : ''}`);
  }

  async approvePr(args: {
    projectKey: string;
    repoSlug: string;
    prId: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, prId } = args;
    const data = await this.request<BBParticipant>(
      'POST',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${prId}/approve`
    );
    if (!data) return text(`Approved PR #${prId}.`);
    return text(`Approved PR #${prId} as ${data.user.displayName}.`);
  }

  async mergePr(args: {
    projectKey: string;
    repoSlug: string;
    prId: number;
    mergeStrategy?: 'MERGE_COMMIT' | 'SQUASH' | 'FAST_FORWARD';
    message?: string;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, prId, mergeStrategy, message } = args;
    // Fetch current version — required by the merge endpoint
    const pr = await this.request<BBPullRequest>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${prId}`
    );
    if (!pr) throw new Error(`PR #${prId} not found.`);
    const body: Record<string, unknown> = { version: pr.version };
    if (mergeStrategy) body.strategyId = mergeStrategy;
    if (message) body.message = message;
    const data = await this.request<BBPullRequest>(
      'POST',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${prId}/merge`,
      body
    );
    if (!data) return text(`Merged PR #${prId}.`);
    return text(`Merged PR #${prId}: "${data.title}" (${data.fromRef.displayId} → ${data.toRef.displayId}).`);
  }

  async getBranches(args: {
    projectKey: string;
    repoSlug: string;
    filter?: string;
    limit?: number;
    start?: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, filter, limit = 25, start = 0 } = args;
    const qs = new URLSearchParams({ limit: String(limit), start: String(start) });
    if (filter) qs.set('filterText', filter);
    const data = await this.request<BBPagedResult<BBBranch>>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/branches?${qs}`
    );
    if (!data || data.values.length === 0) return text('No branches found.');
    const lines = data.values.map((b) => {
      const def = b.isDefault ? ' (default)' : '';
      return `${b.displayId}${def} — ${b.latestCommit.slice(0, 8)}`;
    });
    return text(`${data.values.length} branch(es)${pagination(data)}:\n${lines.join('\n')}`);
  }

  async getFile(args: {
    projectKey: string;
    repoSlug: string;
    path: string;
    ref?: string;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, path: filePath, ref } = args;
    const qs = ref ? `?at=${encodeURIComponent(ref)}` : '';
    const encodedPath = filePath.split('/').map(encodeURIComponent).join('/');
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
    projectKey: string;
    repoSlug: string;
    prId: number;
    limit?: number;
    start?: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, prId, limit = 50, start = 0 } = args;
    const data = await this.request<BBPagedResult<BBActivity>>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${prId}/activities?limit=${limit}&start=${start}`
    );
    if (!data) return text('No activity found.');
    const comments = data.values.filter((a) => a.action === 'COMMENTED' && a.comment);
    if (comments.length === 0) return text('No comments on this pull request.');
    const blocks = comments.map((a) => {
      const user = a.user?.displayName ?? 'Unknown';
      const date = a.comment?.createdDate ? formatDate(a.comment.createdDate) : '';
      return `--- ${user}${date ? ` (${date})` : ''} ---\n${a.comment!.text}`;
    });
    return text(`${comments.length} comment(s) on PR #${prId}${pagination(data)}:\n\n${blocks.join('\n\n')}`);
  }

  async addPrComment(args: {
    projectKey: string;
    repoSlug: string;
    prId: number;
    text: string;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, prId } = args;
    await this.request(
      'POST',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${prId}/comments`,
      { text: args.text }
    );
    return text(`Comment added to PR #${prId}.`);
  }
}
