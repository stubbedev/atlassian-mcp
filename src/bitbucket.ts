type ToolResult = { content: Array<{ type: 'text'; text: string }> };

interface BBRepo {
  slug: string;
  name: string;
  project: { key: string; name: string };
  links?: { clone?: Array<{ href: string; name: string }> };
}

interface BBPagedResult<T> {
  values: T[];
  size: number;
  isLastPage: boolean;
  start: number;
}

interface BBPullRequest {
  id: number;
  title: string;
  description?: string;
  state: string;
  author: { user: { displayName: string; name: string } };
  fromRef: { displayId: string };
  toRef: { displayId: string };
  reviewers: Array<{ user: { displayName: string }; approved: boolean }>;
  links?: { self?: Array<{ href: string }> };
}

interface BBActivity {
  action: string;
  user?: { displayName: string };
  comment?: { text: string; createdDate: number };
  createdDate?: number;
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

  async listRepos(args: { projectKey?: string; limit?: number }): Promise<ToolResult> {
    const { projectKey, limit = 50 } = args;
    const path = projectKey
      ? `/projects/${projectKey}/repos?limit=${limit}`
      : `/repos?limit=${limit}`;
    const data = await this.request<BBPagedResult<BBRepo>>('GET', path);
    if (!data || data.values.length === 0) return text('No repositories found.');
    const lines = data.values.map((r, i) => `${i + 1}. ${r.project.key}/${r.slug} — ${r.name}`);
    return text(`${data.values.length} repo(s):\n${lines.join('\n')}`);
  }

  async listPullRequests(args: {
    projectKey: string;
    repoSlug: string;
    state?: string;
    limit?: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, state = 'OPEN', limit = 25 } = args;
    const path = `/projects/${projectKey}/repos/${repoSlug}/pull-requests?state=${state}&limit=${limit}`;
    const data = await this.request<BBPagedResult<BBPullRequest>>('GET', path);
    if (!data || data.values.length === 0) return text(`No ${state} pull requests found.`);
    const lines = data.values.map(
      (pr) =>
        `#${pr.id} [${pr.state}] ${pr.title} | ${pr.fromRef.displayId} → ${pr.toRef.displayId} | by ${pr.author.user.displayName}`
    );
    return text(`${data.values.length} PR(s) (${state}):\n${lines.join('\n')}`);
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

  async createPullRequest(args: {
    projectKey: string;
    repoSlug: string;
    title: string;
    description?: string;
    fromBranch: string;
    toBranch?: string;
    reviewers?: string[];
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, title, description, fromBranch, toBranch = 'main', reviewers = [] } = args;
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

  async getPrComments(args: {
    projectKey: string;
    repoSlug: string;
    prId: number;
    limit?: number;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug, prId, limit = 50 } = args;
    const data = await this.request<BBPagedResult<BBActivity>>(
      'GET',
      `/projects/${projectKey}/repos/${repoSlug}/pull-requests/${prId}/activities?limit=${limit}`
    );
    if (!data) return text('No activity found.');
    const comments = data.values.filter((a) => a.action === 'COMMENTED' && a.comment);
    if (comments.length === 0) return text('No comments on this pull request.');
    const blocks = comments.map((a) => {
      const user = a.user?.displayName ?? 'Unknown';
      const date = a.comment?.createdDate ? formatDate(a.comment.createdDate) : '';
      return `--- ${user}${date ? ` (${date})` : ''} ---\n${a.comment!.text}`;
    });
    return text(`${comments.length} comment(s) on PR #${prId}:\n\n${blocks.join('\n\n')}`);
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
