import { execSync } from 'child_process';
import { writeFile } from 'fs/promises';
import { resolve as resolvePath } from 'path';

type TextContent = { type: 'text'; text: string };
type ImageContent = { type: 'image'; data: string; mimeType: string };
type ToolResult = { content: Array<TextContent> };
type RichToolResult = { content: Array<TextContent | ImageContent> };
const EMOJI_RE = /\p{Extended_Pictographic}/u;
const ATTACHMENT_REF_RE = /!?\[([^\]]*)\]\(attachment:(\d+)\)/g;
const MAX_INLINE_BYTES = 5 * 1024 * 1024;

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function isTextMime(mimeType: string): boolean {
  const mt = mimeType.toLowerCase();
  if (mt.startsWith('text/')) return true;
  return [
    'application/json',
    'application/xml',
    'application/javascript',
    'application/x-yaml',
    'application/yaml',
    'application/x-sh',
    'application/sql',
  ].some((m) => mt === m || mt.startsWith(`${m};`));
}

interface AttachmentRef {
  id: string;
  filename: string;
  source: string;
}

function collectAttachmentRefs(input: string | undefined, source: string, out: Map<string, AttachmentRef>): void {
  if (!input) return;
  ATTACHMENT_REF_RE.lastIndex = 0;
  let match: RegExpExecArray | null;
  while ((match = ATTACHMENT_REF_RE.exec(input)) !== null) {
    const id = match[2];
    if (!out.has(id)) {
      out.set(id, { id, filename: match[1] || '(unnamed)', source });
    }
  }
}

function collectFromCommentTree(comment: BBComment, out: Map<string, AttachmentRef>): void {
  if (comment.deleted) return;
  collectAttachmentRefs(comment.text, `comment #${comment.id}`, out);
  for (const reply of comment.comments ?? []) collectFromCommentTree(reply, out);
}

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
  author: { user: { displayName: string; name: string; slug?: string } };
  fromRef: { displayId: string; latestCommit?: string; repository: { slug: string; project: { key: string } } };
  toRef: { displayId: string; latestCommit?: string; repository: { slug: string; project: { key: string } } };
  reviewers: Array<{ user: { displayName: string; name?: string }; approved: boolean }>;
  links?: { self?: Array<{ href: string }> };
}

interface BBComment {
  id: number;
  version: number;
  text: string;
  deleted?: boolean;
  threadResolved?: boolean;
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


interface BBBuildStatus {
  state: 'SUCCESSFUL' | 'FAILED' | 'INPROGRESS';
  key: string;
  name?: string;
  url?: string;
  description?: string;
  dateAdded?: number;
}

interface BBTask {
  id: number;
  version: number;
  text: string;
  state: 'OPEN' | 'RESOLVED';
  author?: { displayName?: string; name?: string };
  createdDate?: number;
  anchor?: { id: number; type?: string };
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

interface BBUser {
  name: string;
  displayName: string;
  emailAddress?: string;
  active?: boolean;
  slug?: string;
}

function text(t: string): ToolResult {
  return { content: [{ type: 'text', text: t }] };
}

function toBranchRef(branch: string): string {
  return branch.startsWith('refs/') ? branch : `refs/heads/${branch}`;
}

function branchDisplayId(branch: string): string {
  return branch.replace(/^refs\/heads\//, '');
}

function formatDate(ms: number): string {
  return new Date(ms).toISOString().slice(0, 10);
}

function formatCommentThread(comment: BBComment, indent = '', depth = 0): string[] {
  if (depth > 20) return [`${indent}... (deeply nested replies omitted)`];
  const author = comment.author?.displayName ?? comment.author?.name ?? 'Unknown';
  const date = comment.createdDate ? ` (${formatDate(comment.createdDate)})` : '';
  const state = comment.state ?? 'OPEN';
  const severity = comment.severity ?? 'NORMAL';
  const threadStatus = comment.threadResolved !== undefined
    ? ` thread=${comment.threadResolved ? 'RESOLVED' : 'OPEN'}`
    : '';
  const lines = [
    `${indent}#${comment.id} [${state}/${severity}${threadStatus}] ${author}${date} (v${comment.version})`,
    `${indent}${comment.text}`,
  ];

  if (comment.comments && comment.comments.length > 0) {
    for (const reply of comment.comments) {
      lines.push(...formatCommentThread(reply, `${indent}  `, depth + 1));
    }
  }

  return lines;
}

function commentMatchesState(comment: BBComment, state: 'OPEN' | 'RESOLVED' | 'PENDING'): boolean {
  if (state !== 'PENDING' && (comment.severity ?? 'NORMAL') !== 'BLOCKER' && comment.threadResolved !== undefined) {
    const threadState = comment.threadResolved ? 'RESOLVED' : 'OPEN';
    if (threadState === state) return true;
  }
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
    const comment = activity.comment;
    if (!comment) continue;
    const existing = byId.get(comment.id);
    if (!existing) {
      byId.set(comment.id, comment);
      continue;
    }

    const commentVersion = comment.version ?? -1;
    const existingVersion = existing.version ?? -1;

    if (commentVersion > existingVersion) {
      byId.set(comment.id, comment);
      continue;
    }

    if (commentVersion === existingVersion) {
      const commentUpdated = comment.updatedDate ?? comment.createdDate ?? 0;
      const existingUpdated = existing.updatedDate ?? existing.createdDate ?? 0;
      if (commentUpdated > existingUpdated) {
        byId.set(comment.id, comment);
      }
    }
  }
  return Array.from(byId.values())
    .filter((comment) => !comment.deleted)
    .sort((a, b) => (a.createdDate ?? 0) - (b.createdDate ?? 0));
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

function validateSuggestionPlacement(textValue: string): void {
  if (!textValue.includes('```suggestion')) return;

  const match = textValue.match(/```suggestion[^\n]*\n[\s\S]*?\n```/);
  if (!match || match.index === undefined) {
    throw new Error('Invalid suggestion block format. Use the suggestion field to post code suggestions.');
  }

  const trailingText = textValue.slice(match.index + match[0].length).trim();
  if (trailingText.length > 0) {
    throw new Error('When using ```suggestion```, do not add text after the closing code fence. Put any explanation before the suggestion block or use the suggestion field.');
  }
}

export class BitbucketClient {
  private baseUrl: string;
  private headers: Record<string, string>;
  private currentUsernameCache?: string;

  constructor(baseUrl: string, token: string) {
    this.baseUrl = baseUrl.replace(/\/$/, '');
    this.headers = {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      Accept: 'application/json',
    };
  }

  /** Returns the slug/username of the authenticated user via the X-AUSERNAME response header. */
  private async getCurrentUsername(): Promise<string> {
    if (this.currentUsernameCache) return this.currentUsernameCache;
    const url = `${this.baseUrl}/rest/api/1.0/application-properties`;
    const res = await fetch(url, { method: 'GET', headers: this.headers });
    const username = res.headers.get('X-AUSERNAME');
    if (!username) throw new Error('Could not determine current Bitbucket user. Check token permissions.');
    this.currentUsernameCache = username;
    return username;
  }

  /** Returns a URL-safe `/projects/.../repos/...` prefix for REST paths. */
  private rp(projectKey: string, repoSlug: string): string {
    return `/projects/${encodeURIComponent(projectKey)}/repos/${encodeURIComponent(repoSlug)}`;
  }

  private pullRequestUrl(projectKey: string, repoSlug: string, prId: number, pr?: BBPullRequest | null): string {
    const apiUrl = pr?.links?.self?.[0]?.href?.trim();
    if (apiUrl) {
      return apiUrl;
    }
    return `${this.baseUrl}/projects/${encodeURIComponent(projectKey)}/repos/${encodeURIComponent(repoSlug)}/pull-requests/${prId}`;
  }

  private configuredHostname(): string {
    try { return new URL(this.baseUrl).hostname.toLowerCase(); } catch { return ''; }
  }

  private remoteMatchesInstance(remote: string): boolean {
    const host = this.configuredHostname();
    if (!host) return true; // can't validate, allow
    return remote.toLowerCase().includes(host);
  }

  private resolveProjectAndRepo(
    projectKey?: string,
    repoSlug?: string
  ): { projectKey: string; repoSlug: string } {
    if (projectKey && repoSlug) return { projectKey, repoSlug };
    const remote = safeExec('git remote get-url origin');
    if (remote) {
      if (!this.remoteMatchesInstance(remote)) {
        throw new Error(
          `This repo's remote does not point to your configured Bitbucket instance (${this.baseUrl}). ` +
          `Bitbucket tools only work with repos hosted on that instance.`
        );
      }
      const parsed = parseBitbucketRemote(remote);
      if (parsed) {
        return {
          projectKey: projectKey ?? parsed.projectKey,
          repoSlug: repoSlug ?? parsed.repoSlug,
        };
      }
    }
    throw new Error(
      'Could not determine projectKey/repoSlug — provide them explicitly or run from a directory with a Bitbucket remote'
    );
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T | null> {
    const url = `${this.baseUrl}/rest/api/1.0${path}`;
    const opts: RequestInit = { method, headers: this.headers, signal: AbortSignal.timeout(30_000) };
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
      signal: AbortSignal.timeout(30_000),
    });
    if (!res.ok) {
      const errText = await res.text();
      const details = parseBitbucketErrorDetails(errText);
      throw new Error(formatBitbucketError(res.status, 'GET', path, details));
    }
    return res.text();
  }

  private async requestBuildStatus<T>(method: string, path: string, body?: unknown): Promise<T | null> {
    const url = `${this.baseUrl}/rest/build-status/1.0${path}`;
    const opts: RequestInit = { method, headers: this.headers, signal: AbortSignal.timeout(30_000) };
    if (body !== undefined) opts.body = JSON.stringify(body);
    const res = await fetch(url, opts);
    if (res.status === 404) return null; // no build status yet
    if (!res.ok) {
      const errText = await res.text();
      const details = parseBitbucketErrorDetails(errText);
      throw new Error(formatBitbucketError(res.status, method, path, details));
    }
    return res.status === 204 ? null : (res.json() as Promise<T>);
  }


  /** Returns true if the given remote URL belongs to this Bitbucket instance. */
  isRemoteForThisInstance(remoteUrl: string): boolean {
    return this.remoteMatchesInstance(remoteUrl);
  }

  // Used internally by context tools — finds the open PR for a given source branch.
  // Uses the `at` filter to avoid paginating all open PRs.
  async findOpenPrForBranch(projectKey: string, repoSlug: string, branch: string): Promise<BBPullRequest | null> {
    const atRef = encodeURIComponent(toBranchRef(branch));
    const encodedProject = encodeURIComponent(projectKey);
    const encodedRepo = encodeURIComponent(repoSlug);
    const data = await this.request<BBPagedResult<BBPullRequest>>(
      'GET',
      `/projects/${encodedProject}/repos/${encodedRepo}/pull-requests?state=OPEN&direction=OUTGOING&at=${atRef}&limit=1`
    );
    return data?.values[0] ?? null;
  }

  // Fallback: search branches matching filterText and check each for an open PR.
  // Used when exact branch name lookup yields no result (e.g. LLM provides a partial branch name).
  private async findOpenPrByBranchFilter(projectKey: string, repoSlug: string, filterText: string): Promise<BBPullRequest | null> {
    const branches = await this.request<BBPagedResult<BBBranch>>(
      'GET',
      `${this.rp(projectKey, repoSlug)}/branches?limit=25&filterText=${encodeURIComponent(filterText)}`
    );
    if (!branches?.values?.length) return null;
    for (const b of branches.values) {
      const pr = await this.findOpenPrForBranch(projectKey, repoSlug, b.displayId);
      if (pr) return pr;
    }
    return null;
  }

  async listRepos(args: { projectKey?: string; limit?: number; start?: number }): Promise<ToolResult> {
    const { limit = 50, start = 0 } = args;
    const qs = `?limit=${limit}&start=${start}`;
    const path = args.projectKey
      ? `/projects/${encodeURIComponent(args.projectKey)}/repos${qs}`
      : `/repos${qs}`;
    const data = await this.request<BBPagedResult<BBRepo>>('GET', path);
    if (!data || data.values.length === 0) return text('No repositories found.');
    const lines = data.values.map((r, i) => `${start + i + 1}. ${r.project.key}/${r.slug} — ${r.name}`);
    return text(`${data.values.length} repo(s)${pageHint(data)}:\n${lines.join('\n')}`);
  }

  async searchUsers(args: {
    projectKey?: string;
    repoSlug?: string;
    query?: string;
    limit?: number;
    start?: number;
  }): Promise<ToolResult> {
    const params = new URLSearchParams();
    if (args.query) params.set('filter', args.query);
    params.set('limit', String(args.limit ?? 25));
    if (args.start) params.set('start', String(args.start));

    type PermEntry = { user: BBUser; permission?: string };
    let path: string;
    if (args.projectKey && args.repoSlug) {
      path = `${this.rp(args.projectKey, args.repoSlug)}/permissions/users?${params}`;
    } else if (args.projectKey) {
      path = `/projects/${encodeURIComponent(args.projectKey)}/permissions/users?${params}`;
    } else {
      path = `/users?${params}`;
    }

    const data = await this.request<BBPagedResult<BBUser | PermEntry>>('GET', path);
    if (!data || data.values.length === 0) return text('No users found.');

    const lines = data.values.map((entry, i) => {
      const user: BBUser = (entry as PermEntry).user ?? (entry as BBUser);
      const parts = [`${i + 1}. ${user.displayName} (${user.name})`];
      if (user.emailAddress) parts.push(`— ${user.emailAddress}`);
      if (user.active === false) parts.push('[inactive]');
      return parts.join(' ');
    });
    return text(`${data.values.length} user(s)${pageHint(data)}:\n${lines.join('\n')}`);
  }

  async searchUsersRaw(args: {
    projectKey?: string;
    repoSlug?: string;
    query?: string;
    limit?: number;
  }): Promise<BBUser[]> {
    const params = new URLSearchParams();
    if (args.query) params.set('filter', args.query);
    params.set('limit', String(args.limit ?? 50));

    type PermEntry = { user: BBUser; permission?: string };
    let path: string;
    if (args.projectKey && args.repoSlug) {
      path = `${this.rp(args.projectKey, args.repoSlug)}/permissions/users?${params}`;
    } else if (args.projectKey) {
      path = `/projects/${encodeURIComponent(args.projectKey)}/permissions/users?${params}`;
    } else {
      path = `/users?${params}`;
    }

    const data = await this.request<BBPagedResult<BBUser | PermEntry>>('GET', path);
    return (data?.values ?? []).map((entry) => (entry as PermEntry).user ?? (entry as BBUser));
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
    if (fromBranch) {
      qs.set('at', toBranchRef(fromBranch));
      qs.set('direction', 'OUTGOING');
    }
    if (searchText) qs.set('filterText', searchText);
    const path = `/projects/${encodeURIComponent(projectKey)}/repos/${encodeURIComponent(repoSlug)}/pull-requests?${qs}`;
    const data = await this.request<BBPagedResult<BBPullRequest>>('GET', path);
    if (!data || data.values.length === 0) return text(`No ${state} pull requests found.`);
    const lines = data.values.map(
      (pr) => `#${pr.id} [${pr.state}] ${pr.title} | ${pr.fromRef.displayId} → ${pr.toRef.displayId} | by ${pr.author.user.displayName}`
    );
    return text(`${data.values.length} PR(s) (${state})${pageHint(data)}:\n${lines.join('\n')}`);
  }

  async myPrs(args: { limit?: number; start?: number; role?: 'author' | 'reviewer' | 'participant' }): Promise<ToolResult> {
    const { limit = 25, start = 0, role } = args;
    const userSlug = await this.getCurrentUsername();
    const qs = new URLSearchParams({ limit: String(limit), start: String(start), state: 'OPEN' });
    if (role) qs.set('role', role.toUpperCase());
    const data = await this.request<BBPagedResult<BBPullRequest>>(
      'GET',
      `/users/${encodeURIComponent(userSlug)}/pull-requests?${qs}`
    );
    if (!data || data.values.length === 0) return text('No pull requests found.');
    const lines = data.values.map((pr) => {
      const repo = `${pr.toRef.repository.project.key}/${pr.toRef.repository.slug}`;
      return `#${pr.id} [${pr.state}] ${pr.title} | ${repo} | ${pr.fromRef.displayId} → ${pr.toRef.displayId}`;
    });
    return text(`${data.values.length} PR(s)${pageHint(data)}:\n${lines.join('\n')}`);
  }

  async getPullRequest(args: { projectKey?: string; repoSlug?: string; prId: number }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const data = await this.request<BBPullRequest>(
      'GET',
      `/projects/${encodeURIComponent(projectKey)}/repos/${encodeURIComponent(repoSlug)}/pull-requests/${args.prId}`
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
    prId?: number;
    fromBranch?: string;
    includeCommits?: boolean;
    includeComments?: boolean;
    includeDiff?: boolean;
    includeBuildStatus?: boolean;
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
    const includeBuildStatus = args.includeBuildStatus ?? true;

    let prId = args.prId;
    if (prId === undefined) {
      const branch = args.fromBranch ?? safeExec('git rev-parse --abbrev-ref HEAD');
      if (!branch || branch === 'HEAD') {
        throw new Error('Provide prId or fromBranch, or run from a checked-out branch.');
      }
      let found = await this.findOpenPrForBranch(projectKey, repoSlug, branch);
      if (!found) {
        // Fallback: search branches matching the input text, then check those for open PRs
        found = await this.findOpenPrByBranchFilter(projectKey, repoSlug, branchDisplayId(branch));
      }
      if (!found) throw new Error(`No open PR found for branch "${branchDisplayId(branch)}".`);
      prId = found.id;
    }

    const pr = await this.request<BBPullRequest>(
      'GET',
      `${this.rp(projectKey, repoSlug)}/pull-requests/${prId}`
    );
    if (!pr) return text('Pull request not found.');

    const sections: string[] = [];
    const attachmentRefs = new Map<string, AttachmentRef>();
    collectAttachmentRefs(pr.description, 'description', attachmentRefs);
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

    if (includeBuildStatus && pr.fromRef.latestCommit) {
      const statuses = await this.requestBuildStatus<{ values: BBBuildStatus[] }>(
        'GET', `/commits/${pr.fromRef.latestCommit}`
      ).catch(() => null);
      if (statuses?.values?.length) {
        const statusLines = statuses.values.map((s) => {
          const icon = s.state === 'SUCCESSFUL' ? '✓' : s.state === 'FAILED' ? '✗' : '…';
          const urlPart = s.url ? `\n   URL: ${s.url}` : '';
          return `${icon} [${s.state}] ${s.name ?? s.key}${s.description ? ` — ${s.description}` : ''}${urlPart}`;
        });
        sections.push(`Build status (${pr.fromRef.latestCommit.slice(0, 8)}):\n${statusLines.join('\n')}`);
      } else {
        sections.push(`Build status: none reported for ${pr.fromRef.latestCommit.slice(0, 8)}`);
      }
    }

    if (includeCommits) {
      const commitsLimit = args.commitsLimit ?? 25;
      const commitsStart = args.commitsStart ?? 0;
      const data = await this.request<BBPagedResult<BBCommit>>(
        'GET',
        `${this.rp(projectKey, repoSlug)}/pull-requests/${prId}/commits?limit=${commitsLimit}&start=${commitsStart}`
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
          `${this.rp(projectKey, repoSlug)}/pull-requests/${prId}/blocker-comments?${qs}`
        );
        if (!data || data.values.length === 0) {
          sections.push(`Comments:\n(no ${commentsState} BLOCKER comments)`);
        } else {
          for (const comment of data.values) collectFromCommentTree(comment, attachmentRefs);
          const blocks = data.values.flatMap((comment) => formatCommentThread(comment));
          sections.push(`Comments (${data.values.length} BLOCKER thread(s))${pageHint(data)}:\n\n${blocks.join('\n\n')}`);
        }
      } else {
        const activityData = await this.request<BBPagedResult<BBActivity>>(
          'GET',
          `${this.rp(projectKey, repoSlug)}/pull-requests/${prId}/activities?limit=${commentsLimit}&start=${commentsStart}`
        );
        const comments = uniqueCommentsFromActivities(activityData?.values ?? []).filter((comment) => {
          const matchesState = commentMatchesState(comment, commentsState);
          return matchesState && commentMatchesSeverity(comment, commentsSeverity);
        });
        for (const comment of comments) collectFromCommentTree(comment, attachmentRefs);
        if (comments.length === 0) {
          sections.push('Comments:\n(no matching comments)');
        } else {
          const blocks = comments.flatMap((comment) => formatCommentThread(comment));
          const paging = activityData ? pageHint(activityData) : '';
          sections.push(`Comments (${comments.length} thread(s))${paging}:\n\n${blocks.join('\n\n')}`);
        }
      }
    }

    if (attachmentRefs.size > 0) {
      const lines = [`Attachments referenced: ${attachmentRefs.size}`];
      for (const ref of attachmentRefs.values()) {
        lines.push(`  #${ref.id} ${ref.filename} — in ${ref.source}`);
      }
      lines.push('Use bitbucket_get_attachment with attachmentId to view contents.');
      sections.push(lines.join('\n'));
    }

    if (includeDiff) {
      const data = await this.request<BBDiff>(
        'GET',
        `${this.rp(projectKey, repoSlug)}/pull-requests/${prId}/diff`
      );
      sections.push(`Diff:\n${data ? formatDiff(data, args.diffMaxChars ?? 8000) : '(no diff found)'}`);
    }

    return text(sections.join('\n\n'));
  }

  async getPrDiff(args: { projectKey?: string; repoSlug?: string; prId: number }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const data = await this.request<BBDiff>(
      'GET',
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/diff`
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
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/commits?limit=${limit}&start=${start}`
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

    const sourceBranchName = branchDisplayId(sourceBranch);
    const existing = await this.findOpenPrForBranch(projectKey, repoSlug, sourceBranchName);
    if (existing) {
      const url = this.pullRequestUrl(projectKey, repoSlug, existing.id, existing);
      return text(
        `Open PR already exists for branch "${sourceBranchName}": #${existing.id} "${existing.title}"\n${url}`
      );
    }

    const { title, description, reviewers = [] } = args;
    const toRef = args.toBranch
      ? toBranchRef(args.toBranch)
      : await this.getDefaultBranchRef(projectKey, repoSlug);
    const body = {
      title,
      description: description ?? '',
      fromRef: { id: toBranchRef(sourceBranch), repository: { slug: repoSlug, project: { key: projectKey } } },
      toRef:   { id: toRef,                     repository: { slug: repoSlug, project: { key: projectKey } } },
      reviewers: reviewers.map((name) => ({ user: { name } })),
    };
    const data = await this.request<BBPullRequest>(
      'POST',
      `${this.rp(projectKey, repoSlug)}/pull-requests`,
      body
    );
    if (!data) return text('Pull request created.');
    const url = this.pullRequestUrl(projectKey, repoSlug, data.id, data);
    return text(`Created PR #${data.id}: "${data.title}"\n${url}`);
  }

  async updatePullRequest(args: {
    projectKey?: string;
    repoSlug?: string;
    prId: number;
    title?: string;
    description?: string;
    toBranch?: string;
    reviewers?: string[];
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    if (
      args.title === undefined
      && args.description === undefined
      && args.toBranch === undefined
      && args.reviewers === undefined
    ) {
      throw new Error('At least one field is required: title, description, toBranch, or reviewers');
    }

    const existing = await this.request<BBPullRequest>(
      'GET',
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}`
    );
    if (!existing) throw new Error(`PR #${args.prId} not found.`);

    const buildBody = (pr: BBPullRequest): Record<string, unknown> => {
      const body: Record<string, unknown> = { version: pr.version };
      if (args.title !== undefined) body.title = args.title;
      if (args.description !== undefined) body.description = args.description;
      if (args.toBranch !== undefined) {
        body.toRef = {
          id: toBranchRef(args.toBranch),
          repository: { slug: repoSlug, project: { key: projectKey } },
        };
      }
      // Always include reviewers to avoid Bitbucket clearing them on PUT.
      // Only replace them when explicitly provided by the caller.
      body.reviewers = args.reviewers !== undefined
        ? args.reviewers.map((name) => ({ user: { name } }))
        : pr.reviewers.map((r) => ({ user: { name: r.user.name } }));
      return body;
    };

    let updated: BBPullRequest | null;
    try {
      updated = await this.request<BBPullRequest>(
        'PUT',
        `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}`,
        buildBody(existing)
      );
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (!message.includes('Bitbucket 409')) throw error;

      const latest = await this.request<BBPullRequest>(
        'GET',
        `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}`
      );
      if (!latest) throw error;

      updated = await this.request<BBPullRequest>(
        'PUT',
        `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}`,
        buildBody(latest)
      );
    }

    if (!updated) return text(`Updated PR #${args.prId}.`);
    const url = this.pullRequestUrl(projectKey, repoSlug, updated.id, updated);
    return text(`Updated PR #${updated.id}: "${updated.title}" (${updated.fromRef.displayId} → ${updated.toRef.displayId}).\n${url}`);
  }

  async mutatePullRequest(args: {
    projectKey?: string;
    repoSlug?: string;
    prId?: number;
    create?: {
      title: string;
      description?: string;
      fromBranch?: string;
      toBranch?: string;
      reviewers?: string[];
    };
    update?: {
      title?: string;
      description?: string;
      toBranch?: string;
      reviewers?: string[];
    };
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);

    const hasUpdate = args.update !== undefined && (
      args.update.title !== undefined
      || args.update.description !== undefined
      || args.update.toBranch !== undefined
      || args.update.reviewers !== undefined
    );

    if (args.prId !== undefined) {
      if (!hasUpdate) {
        return this.getPullRequest({ projectKey, repoSlug, prId: args.prId });
      }
      return this.updatePullRequest({
        projectKey,
        repoSlug,
        prId: args.prId,
        ...args.update,
      });
    }

    const sourceBranch = args.create?.fromBranch ?? safeExec('git rev-parse --abbrev-ref HEAD');
    if (!sourceBranch || sourceBranch === 'HEAD') {
      if (args.create) {
        return this.createPullRequest({
          projectKey,
          repoSlug,
          title: args.create.title,
          description: args.create.description,
          fromBranch: args.create.fromBranch,
          toBranch: args.create.toBranch,
          reviewers: args.create.reviewers,
        });
      }
      throw new Error('Could not determine source branch. Provide create.fromBranch or run from a checked-out branch.');
    }

    const existing = await this.findOpenPrForBranch(projectKey, repoSlug, sourceBranch);
    if (existing) {
      if (hasUpdate) {
        return this.updatePullRequest({
          projectKey,
          repoSlug,
          prId: existing.id,
          ...args.update,
        });
      }

      return this.getPullRequest({ projectKey, repoSlug, prId: existing.id });
    }

    if (!args.create) {
      throw new Error(
        `No open PR found for branch "${branchDisplayId(sourceBranch)}". Provide create to open one.`
      );
    }

    return this.createPullRequest({
      projectKey,
      repoSlug,
      title: args.create.title,
      description: args.create.description,
      fromBranch: args.create.fromBranch,
      toBranch: args.create.toBranch,
      reviewers: args.create.reviewers,
    });
  }

  async approvePr(args: { projectKey?: string; repoSlug?: string; prId: number }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const data = await this.request<BBParticipant>(
      'POST',
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/approve`
    );
    const url = this.pullRequestUrl(projectKey, repoSlug, args.prId);
    if (!data) return text(`Approved PR #${args.prId}.\n${url}`);
    return text(`Approved PR #${args.prId} as ${data.user.displayName}.\n${url}`);
  }

  async unapprovePr(args: { projectKey?: string; repoSlug?: string; prId: number }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    await this.request(
      'DELETE',
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/approve`
    );
    return text(`Approval removed from PR #${args.prId}.\n${this.pullRequestUrl(projectKey, repoSlug, args.prId)}`);
  }

  async declinePr(args: { projectKey?: string; repoSlug?: string; prId: number; message?: string }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const pr = await this.request<BBPullRequest>(
      'GET',
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}`
    );
    if (!pr) throw new Error(`PR #${args.prId} not found.`);
    const body: Record<string, unknown> = { version: pr.version };
    if (args.message) body.message = args.message;
    const data = await this.request<BBPullRequest>(
      'POST',
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/decline`,
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
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}`
    );
    if (!pr) throw new Error(`PR #${args.prId} not found.`);
    const body: Record<string, unknown> = { version: pr.version };
    if (args.mergeStrategy) body.strategyId = args.mergeStrategy;
    if (args.message) body.message = args.message;
    const data = await this.request<BBPullRequest>(
      'POST',
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/merge`,
      body
    );
    if (!data) return text(`Merged PR #${args.prId}.\n${this.pullRequestUrl(projectKey, repoSlug, args.prId)}`);
    return text(`Merged PR #${data.id}: "${data.title}" (${data.fromRef.displayId} → ${data.toRef.displayId}).\n${this.pullRequestUrl(projectKey, repoSlug, data.id, data)}`);
  }

  private async getDefaultBranchRef(projectKey: string, repoSlug: string): Promise<string> {
    const data = await this.request<{ displayId: string }>('GET', `${this.rp(projectKey, repoSlug)}/default-branch`);
    if (data?.displayId) return `refs/heads/${data.displayId}`;
    // Fallback: detect from local git
    const head = safeExec('git rev-parse --abbrev-ref origin/HEAD');
    if (head.startsWith('origin/')) return `refs/heads/${head.slice('origin/'.length)}`;
    return 'refs/heads/master';
  }

  async createBranch(args: {
    projectKey?: string;
    repoSlug?: string;
    branchName: string;
    startPoint?: string;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const startPoint = args.startPoint ?? await this.getDefaultBranchRef(projectKey, repoSlug);
    const data = await this.request<BBBranch>(
      'POST',
      `${this.rp(projectKey, repoSlug)}/branches`,
      { name: args.branchName, startPoint }
    );
    if (!data) return text(`Branch "${args.branchName}" created.`);
    return text(`Created branch "${data.displayId}" at ${data.latestCommit.slice(0, 8)} in ${projectKey}/${repoSlug}.`);
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
      `${this.rp(projectKey, repoSlug)}/branches?${qs}`
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
      `${this.rp(projectKey, repoSlug)}/raw/${encodedPath}${qs}`
    );
    const MAX_CHARS = 10000;
    if (content.length > MAX_CHARS) {
      return text(content.slice(0, MAX_CHARS) + `\n\n... (truncated, ${content.length - MAX_CHARS} more chars)`);
    }
    return text(content);
  }

  async getAttachment(args: {
    projectKey?: string;
    repoSlug?: string;
    attachmentId: string;
    saveTo?: string;
  }): Promise<RichToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const id = String(args.attachmentId ?? '').trim();
    if (!id) throw new Error('attachmentId is required.');

    const url = `${this.baseUrl}/rest/api/1.0${this.rp(projectKey, repoSlug)}/attachments/${encodeURIComponent(id)}`;
    const res = await fetch(url, {
      method: 'GET',
      headers: { Authorization: this.headers.Authorization },
      signal: AbortSignal.timeout(60_000),
    });
    if (!res.ok) {
      const errText = await res.text();
      throw new Error(formatBitbucketError(res.status, 'GET', `${this.rp(projectKey, repoSlug)}/attachments/${id}`, parseBitbucketErrorDetails(errText)));
    }
    const buffer = Buffer.from(await res.arrayBuffer());
    const contentDisposition = res.headers.get('content-disposition') ?? '';
    const filenameMatch = contentDisposition.match(/filename\*?=(?:UTF-8'')?"?([^";]+)"?/i);
    const filename = filenameMatch ? decodeURIComponent(filenameMatch[1]) : `attachment-${id}`;
    const mimeType = (res.headers.get('content-type') ?? 'application/octet-stream').split(';')[0].trim();
    const sizeLabel = formatBytes(buffer.length);
    const header = `${filename} — ${mimeType}, ${sizeLabel}`;

    if (args.saveTo) {
      const path = resolvePath(args.saveTo);
      await writeFile(path, buffer);
      return { content: [{ type: 'text', text: `Saved attachment #${id} (${header}) to ${path}` }] };
    }

    if (mimeType.toLowerCase().startsWith('image/') && buffer.length <= MAX_INLINE_BYTES) {
      return {
        content: [
          { type: 'text', text: `Attachment #${id}: ${header}` },
          { type: 'image', data: buffer.toString('base64'), mimeType },
        ],
      };
    }

    if (isTextMime(mimeType) && buffer.length <= MAX_INLINE_BYTES) {
      return { content: [{ type: 'text', text: `Attachment #${id}: ${header}\n\n${buffer.toString('utf-8')}` }] };
    }

    return {
      content: [{
        type: 'text',
        text: `${header}\nAttachment #${id} is${buffer.length > MAX_INLINE_BYTES ? ' larger than 5 MB or' : ''} not inline-renderable. Pass saveTo=/absolute/path to write it to disk.`,
      }],
    };
  }

  async fetchFileText(projectKey: string, repoSlug: string, filePath: string): Promise<string | null> {
    try {
      const encoded = filePath.split('/').map(encodeURIComponent).join('/');
      const content = await this.requestText(`${this.rp(projectKey, repoSlug)}/raw/${encoded}`);
      return content;
    } catch {
      return null;
    }
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
        `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/blocker-comments?${qs}`
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
        `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/blocker-comments?${qs}`
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
        `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/comments?${qs}`
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
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/activities?limit=${limit}&start=${start}`
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
    commentId?: number;
    text?: string;
    filePath?: string;
    srcPath?: string;
    line?: number;
    lineType?: 'ADDED' | 'REMOVED' | 'CONTEXT';
    fileType?: 'TO' | 'FROM';
    multilineStartLine?: number;
    multilineStartLineType?: 'ADDED' | 'REMOVED' | 'CONTEXT';
    suggestion?: string;
    severity?: 'NORMAL' | 'BLOCKER';
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);

    const replyToCommentId = args.commentId;
    if (
      replyToCommentId !== undefined
      && (
        args.filePath !== undefined
        || args.srcPath !== undefined
        || args.line !== undefined
        || args.lineType !== undefined
        || args.fileType !== undefined
        || args.multilineStartLine !== undefined
        || args.multilineStartLineType !== undefined
      )
    ) {
      throw new Error('Replies must target an existing comment thread only. Omit filePath/line and other anchor fields when replying.');
    }

    if (args.text === undefined && args.suggestion === undefined) {
      throw new Error('Either text or suggestion is required when adding a comment.');
    }

    let commentText = args.text ?? '';
    if (args.suggestion !== undefined) {
      const suggestion = args.suggestion.trim();
      if (!suggestion) {
        throw new Error('suggestion must not be empty.');
      }
      const suggestionBlock = `\`\`\`suggestion\n${suggestion}\n\`\`\``;
      const prefix = (args.text ?? '').trim();
      commentText = prefix ? `${prefix}\n\n${suggestionBlock}` : suggestionBlock;
    } else {
      validateSuggestionPlacement(commentText);
    }

    const body: Record<string, unknown> = { text: validateCommentText(commentText) };
    if (args.severity) body.severity = args.severity;
    if (replyToCommentId !== undefined) body.parent = { id: replyToCommentId };

    let inlineAnchor: Record<string, unknown> | undefined;
    if (args.filePath !== undefined || args.line !== undefined) {
      if (args.filePath === undefined || args.line === undefined) {
        throw new Error('filePath and line must be provided together for inline comments.');
      }

      const pr = await this.request<BBPullRequest>(
        'GET',
        `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}`
      );

      inlineAnchor = {
        diffType: 'EFFECTIVE',
        fileType: args.fileType ?? 'TO',
        line: args.line,
        lineType: args.lineType ?? 'ADDED',
        path: args.filePath,
      };

      if (args.srcPath !== undefined) {
        inlineAnchor.srcPath = args.srcPath;
      }

      const fromHash = pr?.toRef.latestCommit;
      const toHash = pr?.fromRef.latestCommit;
      if (fromHash && toHash) {
        inlineAnchor.fromHash = fromHash;
        inlineAnchor.toHash = toHash;
      }

      if (args.multilineStartLine !== undefined) {
        inlineAnchor.multilineStartLine = args.multilineStartLine;
        inlineAnchor.multilineStartLineType = args.multilineStartLineType ?? args.lineType ?? 'ADDED';
      }

      body.anchor = inlineAnchor;
    }

    let created: BBComment | null;
    try {
      created = await this.request<BBComment>(
        'POST',
        `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/comments`,
        body
      );
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (!inlineAnchor || !message.includes('Bitbucket 409') || !('fromHash' in inlineAnchor) || !('toHash' in inlineAnchor)) {
        throw error;
      }

      const { fromHash: _fromHash, toHash: _toHash, ...anchorWithoutHashes } = inlineAnchor;
      body.anchor = anchorWithoutHashes;
      created = await this.request<BBComment>(
        'POST',
        `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/comments`,
        body
      );
    }

    if (!created) return text(`Comment added to PR #${args.prId}.`);
    if (replyToCommentId !== undefined) {
      return text(`Reply #${created.id} added to comment #${replyToCommentId} on PR #${args.prId}.`);
    }
    const location = args.filePath && args.line ? ` on ${args.filePath}:${args.line}` : '';
    return text(`Comment #${created.id} added to PR #${args.prId}${location}.`);
  }

  async updatePrComment(args: {
    projectKey?: string;
    repoSlug?: string;
    prId: number;
    commentId: number;
    text?: string;
    state?: 'OPEN' | 'RESOLVED';
    severity?: 'NORMAL' | 'BLOCKER';
    threadResolved?: boolean;
  }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    if (!args.text && !args.state && !args.severity && args.threadResolved === undefined) {
      throw new Error('At least one field is required: text, state, severity, or threadResolved');
    }

    const current = await this.request<BBComment>(
      'GET',
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/comments/${args.commentId}`
    );
    if (!current) throw new Error(`Comment #${args.commentId} not found.`);
    const currentSeverity = current.severity ?? 'NORMAL';
    const targetSeverity = args.severity ?? currentSeverity;

    if (args.state && targetSeverity !== 'BLOCKER') {
      throw new Error('state is only supported for BLOCKER comments (tasks). Use threadResolved for normal comment threads.');
    }
    if (args.threadResolved !== undefined && targetSeverity === 'BLOCKER') {
      throw new Error('threadResolved is only supported for normal comments. Use state for BLOCKER comment tasks.');
    }

    const commentPath = (targetSeverity === 'BLOCKER' || current.severity === 'BLOCKER')
      ? `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/blocker-comments/${args.commentId}`
      : `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/comments/${args.commentId}`;

    const buildBody = (version: number): Record<string, unknown> => {
      const body: Record<string, unknown> = { version };
      if (args.text !== undefined) body.text = validateCommentText(args.text);
      if (args.state && targetSeverity === 'BLOCKER') body.state = args.state;
      if (args.severity) body.severity = args.severity;
      if (args.threadResolved !== undefined) {
        body.threadResolved = args.threadResolved;
      }
      return body;
    };

    let updated: BBComment | null;
    try {
      updated = await this.request<BBComment>(
        'PUT',
        commentPath,
        buildBody(current.version)
      );
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (!message.includes('Bitbucket 409')) throw error;

      const latest = await this.request<BBComment>('GET', commentPath);
      if (!latest) throw error;

      updated = await this.request<BBComment>(
        'PUT',
        commentPath,
        buildBody(latest.version)
      );
    }

    if (!updated) return text(`Comment #${args.commentId} updated.`);
    const state = updated.state ?? current.state ?? 'OPEN';
    const severity = updated.severity ?? current.severity ?? 'NORMAL';
    const threadResolved = updated.threadResolved ?? current.threadResolved;
    const threadStatus = threadResolved === undefined ? '' : `, thread=${threadResolved ? 'RESOLVED' : 'OPEN'}`;
    return text(`Comment #${updated.id} updated (${state}/${severity}${threadStatus}).`);
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
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/comments/${args.commentId}`
    );
    if (!current) throw new Error(`Comment #${args.commentId} not found.`);

    const commentPath = current.severity === 'BLOCKER'
      ? `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/blocker-comments/${args.commentId}`
      : `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/comments/${args.commentId}`;
    const path = `${commentPath}?version=${current.version}`;
    await this.request('DELETE', path);
    return text(`Comment #${args.commentId} deleted from PR #${args.prId}.`);
  }

  async getPrTasks(args: { projectKey?: string; repoSlug?: string; prId: number }): Promise<ToolResult> {
    const { projectKey, repoSlug } = this.resolveProjectAndRepo(args.projectKey, args.repoSlug);
    const data = await this.request<BBPagedResult<BBTask>>(
      'GET',
      `${this.rp(projectKey, repoSlug)}/pull-requests/${args.prId}/tasks`
    );
    if (!data || data.values.length === 0) return text(`No tasks on PR #${args.prId}.`);
    const lines = data.values.map((t) => {
      const author = t.author?.displayName ?? t.author?.name ?? 'Unknown';
      const date = t.createdDate ? ` (${formatDate(t.createdDate)})` : '';
      const anchor = t.anchor?.id ? ` [on comment #${t.anchor.id}]` : '';
      return `#${t.id} [${t.state}] ${author}${date}${anchor}: ${t.text}`;
    });
    const open = data.values.filter((t) => t.state === 'OPEN').length;
    return text(`${data.values.length} task(s) on PR #${args.prId} (${open} open)${pageHint(data)}:\n${lines.join('\n')}`);
  }

  async mutatePrTask(args: {
    action: 'create' | 'resolve' | 'reopen' | 'delete';
    projectKey?: string;
    repoSlug?: string;
    prId?: number;
    taskId?: number;
    text?: string;
    commentId?: number;
  }): Promise<ToolResult> {
    this.resolveProjectAndRepo(args.projectKey, args.repoSlug);

    if (args.action === 'create') {
      if (!args.text) throw new Error('text is required to create a task.');
      const body: Record<string, unknown> = { text: args.text };
      if (args.commentId !== undefined) {
        body.anchor = { id: args.commentId, type: 'COMMENT' };
      } else if (args.prId !== undefined) {
        body.anchor = { id: args.prId, type: 'PULL_REQUEST' };
      } else {
        throw new Error('Provide prId or commentId to anchor the task.');
      }
      const created = await this.request<BBTask>('POST', '/tasks', body);
      if (!created) return text('Task created.');
      return text(`Task #${created.id} created: "${created.text}"`);
    }

    if (!args.taskId) throw new Error('taskId is required for resolve/reopen/delete.');

    if (args.action === 'delete') {
      const task = await this.request<BBTask>('GET', `/tasks/${args.taskId}`);
      if (!task) throw new Error(`Task #${args.taskId} not found.`);
      // Verify the task belongs to the given PR (when anchor is a direct PR anchor)
      if (args.prId !== undefined && task.anchor?.type === 'PULL_REQUEST' && task.anchor.id !== args.prId) {
        throw new Error(`Task #${args.taskId} does not belong to PR #${args.prId}.`);
      }
      await this.request('DELETE', `/tasks/${args.taskId}?version=${task.version}`);
      return text(`Task #${args.taskId} deleted.`);
    }

    // resolve or reopen
    const task = await this.request<BBTask>('GET', `/tasks/${args.taskId}`);
    if (!task) throw new Error(`Task #${args.taskId} not found.`);
    if (args.prId !== undefined && task.anchor?.type === 'PULL_REQUEST' && task.anchor.id !== args.prId) {
      throw new Error(`Task #${args.taskId} does not belong to PR #${args.prId}.`);
    }
    const newState = args.action === 'resolve' ? 'RESOLVED' : 'OPEN';
    const updated = await this.request<BBTask>('PUT', `/tasks/${args.taskId}?version=${task.version}`, {
      id: task.id,
      state: newState,
      text: task.text,
    });
    if (!updated) return text(`Task #${args.taskId} ${newState}.`);
    return text(`Task #${updated.id} is now ${updated.state}: "${updated.text}"`);
  }

  async getBuildStatuses(args: { commitSha: string }): Promise<ToolResult> {
    const data = await this.requestBuildStatus<{ values: BBBuildStatus[] }>(
      'GET',
      `/commits/${args.commitSha}`
    );
    if (!data?.values?.length) return text(`No build statuses reported for ${args.commitSha}.`);
    const lines = data.values.map((s) => {
      const icon = s.state === 'SUCCESSFUL' ? '✓' : s.state === 'FAILED' ? '✗' : '…';
      const date = s.dateAdded ? ` (${new Date(s.dateAdded).toISOString().slice(0, 10)})` : '';
      return `${icon} [${s.state}] ${s.name ?? s.key}${date}${s.description ? `\n  ${s.description}` : ''}${s.url ? `\n  ${s.url}` : ''}`;
    });
    return text(`${data.values.length} build status(es) for ${args.commitSha.slice(0, 8)}:\n${lines.join('\n')}`);
  }
}
