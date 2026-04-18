import { execSync } from 'child_process';

type ToolResult = { content: Array<{ type: 'text'; text: string }> };

interface JiraIssue {
  key: string;
  fields: {
    summary: string;
    description?: string;
    status: { name: string };
    issuetype: { name: string };
    priority?: { name: string };
    assignee?: { displayName: string };
    labels?: string[];
    components?: Array<{ name: string }>;
    parent?: { key: string; fields: { summary: string; issuetype: { name: string } } };
    fixVersions?: Array<{ id: string; name: string }>;
    issuelinks?: JiraIssueLink[];
    subtasks?: Array<{ key: string; fields: { summary: string; status: { name: string } } }>;
  };
}

interface JiraSearchResult {
  total: number;
  startAt: number;
  issues: JiraIssue[];
}

interface JiraComment {
  id: string;
  author: { name?: string; key?: string; displayName: string };
  created: string;
  body: string;
}

interface JiraCommentResult {
  total: number;
  startAt: number;
  comments: JiraComment[];
}

interface JiraTransition {
  id: string;
  name: string;
  to: { name: string };
}

interface JiraTransitionsResult {
  transitions: JiraTransition[];
}

interface JiraCreatedIssue {
  key: string;
  id: string;
}

interface JiraProject {
  key: string;
  name: string;
  projectTypeKey: string;
}

interface JiraIssueTypeStatuses {
  id: string;
  name: string;
  statuses: Array<{ id: string; name: string }>;
}

interface JiraUser {
  name: string;
  displayName: string;
  emailAddress: string;
  active: boolean;
}

interface JiraCurrentUser {
  name?: string;
  key?: string;
  displayName?: string;
}

interface JiraPage<T> {
  total?: number;
  startAt: number;
  maxResults: number;
  isLast?: boolean;
  values: T[];
}

interface JiraSprint {
  id: number;
  name: string;
  state: string;
  startDate?: string;
  endDate?: string;
  goal?: string;
}

interface JiraBoard {
  id: number;
  name: string;
  type: string;
  location?: { projectKey?: string; projectName?: string };
}

interface JiraIssueLink {
  id: string;
  type: { id: string; name: string; inward: string; outward: string };
  inwardIssue?: { key: string; fields: { summary: string; status: { name: string } } };
  outwardIssue?: { key: string; fields: { summary: string; status: { name: string } } };
}

interface JiraWorklog {
  id: string;
  author: { displayName: string };
  started: string;
  timeSpent: string;
  comment?: string;
}

interface JiraAgileIssue {
  key: string;
  fields?: {
    sprint?: JiraSprint;
    closedSprints?: JiraSprint[];
  };
}

interface JiraErrorPayload {
  errorMessages?: string[];
  errors?: Record<string, string>;
}

const JIRA_KEY_IN_BRANCH_RE = /\b([A-Z][A-Z0-9]+)-\d+\b/;
const EMOJI_RE = /\p{Extended_Pictographic}/u;

function text(t: string): ToolResult {
  return { content: [{ type: 'text', text: t }] };
}

function pagination(total: number, startAt: number, count: number): string {
  const end = startAt + count;
  return total > end ? ` (showing ${startAt + 1}–${end} of ${total}, use startAt=${end} for next page)` : '';
}

function buildJQL(args: {
  query?: string;
  jql?: string;
  project?: string;
  status?: string;
  assignee?: string;
  issueType?: string;
}): string {
  if (args.jql) {
    if (args.jql.length > 2000) throw new Error('JQL query too long (max 2000 characters).');
    return args.jql;
  }

  const clauses: string[] = [];
  if (args.query)     clauses.push(`text ~ ${JSON.stringify(args.query)}`);
  if (args.project)   clauses.push(`project = "${args.project}"`);
  if (args.status)    clauses.push(`status = "${args.status}"`);
  if (args.assignee)  clauses.push(`assignee = "${args.assignee}"`);
  if (args.issueType) clauses.push(`issuetype = "${args.issueType}"`);

  if (clauses.length === 0) throw new Error('Provide at least one of: query, jql, project, status, assignee, issueType');
  return clauses.join(' AND ') + ' ORDER BY updated DESC';
}

function safeExec(cmd: string): string {
  try {
    return execSync(cmd, { encoding: 'utf-8' }).trim();
  } catch {
    return '';
  }
}

function parseJiraErrorDetails(errText: string): string {
  const trimmed = errText.trim();
  if (!trimmed) return '';

  try {
    const parsed = JSON.parse(trimmed) as JiraErrorPayload;
    const parts: string[] = [];
    if (Array.isArray(parsed.errorMessages)) {
      for (const msg of parsed.errorMessages) {
        const clean = (msg ?? '').trim();
        if (clean) parts.push(clean);
      }
    }
    if (parsed.errors && typeof parsed.errors === 'object') {
      for (const [field, msg] of Object.entries(parsed.errors)) {
        const clean = (msg ?? '').trim();
        if (clean) parts.push(`${field}: ${clean}`);
      }
    }
    if (parts.length > 0) return parts.join(' | ');
  } catch {
    // Fallback to raw text below
  }

  return trimmed.length > 500 ? `${trimmed.slice(0, 500)}...` : trimmed;
}

function formatJiraError(status: number, method: string, path: string, details: string): string {
  const prefix = `Jira ${status} ${method} ${path}`;
  if (status === 400) return `${prefix}. Invalid request or parameters. ${details}`.trim();
  if (status === 401) return `${prefix}. Authentication failed. Check JIRA_ACCESS_TOKEN.`;
  if (status === 403) return `${prefix}. Permission denied. Check Jira permissions for this token.`;
  if (status === 404) return `${prefix}. Resource not found. Verify issue/project identifiers and access.`;
  if (status === 409) return `${prefix}. Conflict. Refresh and retry. ${details}`.trim();
  return details ? `${prefix}. ${details}` : prefix;
}

function validateCommentBody(body: string): string {
  const trimmed = body.trim();
  if (!trimmed) {
    throw new Error('Jira comment body must not be empty.');
  }
  if (EMOJI_RE.test(trimmed)) {
    throw new Error('Jira comments must not include emoji. Use concise Jira wiki markup or plain text only.');
  }
  return trimmed;
}

export class JiraClient {
  private baseUrl: string;
  private headers: Record<string, string>;
  private currentUserCache?: JiraCurrentUser;
  private projectsCache?: JiraProject[];

  constructor(baseUrl: string, token: string) {
    this.baseUrl = baseUrl.replace(/\/$/, '');
    this.headers = {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      Accept: 'application/json',
    };
  }

  private issueUrl(issueKey: string): string {
    return `${this.baseUrl}/browse/${encodeURIComponent(issueKey)}`;
  }

  private projectUrl(projectKey: string): string {
    return `${this.baseUrl}/projects/${encodeURIComponent(projectKey)}`;
  }

  private boardUrl(boardId: number): string {
    return `${this.baseUrl}/secure/RapidBoard.jspa?rapidView=${boardId}`;
  }

  private sprintUrl(boardId: number, sprintId: number): string {
    return `${this.boardUrl(boardId)}&sprint=${sprintId}`;
  }

  private async requestWithBase<T>(apiBase: string, method: string, path: string, body?: unknown): Promise<T | null> {
    const url = `${this.baseUrl}${apiBase}${path}`;
    const opts: RequestInit = { method, headers: this.headers, signal: AbortSignal.timeout(30_000) };
    if (body !== undefined) opts.body = JSON.stringify(body);
    const res = await fetch(url, opts);
    if (!res.ok) {
      const errText = await res.text();
      const details = parseJiraErrorDetails(errText);
      throw new Error(formatJiraError(res.status, method, path, details));
    }
    return res.status === 204 ? null : (res.json() as Promise<T>);
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T | null> {
    return this.requestWithBase<T>('/rest/api/2', method, path, body);
  }

  private async requestAgile<T>(method: string, path: string, body?: unknown): Promise<T | null> {
    return this.requestWithBase<T>('/rest/agile/1.0', method, path, body);
  }

  private normalizeIdentity(value?: string): string {
    return (value ?? '').trim().toLowerCase();
  }

  private async getCurrentUser(): Promise<JiraCurrentUser> {
    if (this.currentUserCache) return this.currentUserCache;
    const me = await this.request<JiraCurrentUser>('GET', '/myself');
    if (!me) {
      throw new Error('Could not determine current Jira user identity.');
    }
    this.currentUserCache = me;
    return me;
  }

  private async assertOwnComment(comment: JiraComment): Promise<void> {
    const me = await this.getCurrentUser();
    const commentAuthorName = this.normalizeIdentity(comment.author.name);
    const commentAuthorKey = this.normalizeIdentity(comment.author.key);
    const commentAuthorDisplayName = this.normalizeIdentity(comment.author.displayName);
    const meName = this.normalizeIdentity(me.name);
    const meKey = this.normalizeIdentity(me.key);
    const meDisplayName = this.normalizeIdentity(me.displayName);

    const hasStrongCommentIdentity = commentAuthorName.length > 0 || commentAuthorKey.length > 0;
    const hasStrongUserIdentity = meName.length > 0 || meKey.length > 0;

    const matchesByNameOrKey = (commentAuthorName.length > 0 && (commentAuthorName === meName || commentAuthorName === meKey))
      || (commentAuthorKey.length > 0 && (commentAuthorKey === meName || commentAuthorKey === meKey));

    const matchesByDisplayNameFallback = !hasStrongCommentIdentity
      && !hasStrongUserIdentity
      && commentAuthorDisplayName.length > 0
      && commentAuthorDisplayName === meDisplayName;

    if (matchesByNameOrKey || matchesByDisplayNameFallback) {
      return;
    }

    throw new Error(
      `You can only edit your own Jira comments. Comment ${comment.id} is authored by ${comment.author.displayName}.`
    );
  }

  private async addIssuesToSprintInternal(sprintId: number, issueKeys: string[]): Promise<void> {
    await this.requestAgile('POST', `/sprint/${sprintId}/issue`, { issues: issueKeys });
  }

  private async createIssueInternal(args: {
    projectKey?: string;
    issueType: string;
    summary: string;
    description?: string;
    assignee?: string;
    priority?: string;
    labels?: string[];
    fixVersion?: string;
    parent?: string;
  }): Promise<JiraCreatedIssue | null> {
    const projectKey = await this.resolveProjectKey(args.projectKey);
    const fields: Record<string, unknown> = {
      project: { key: projectKey },
      issuetype: { name: args.issueType },
      summary: args.summary,
    };
    if (args.description)            fields.description = args.description;
    if (args.assignee)               fields.assignee = { name: args.assignee };
    if (args.priority)               fields.priority = { name: args.priority };
    if (args.labels?.length)         fields.labels = args.labels;
    if (args.fixVersion)             fields.fixVersions = [{ name: args.fixVersion }];
    if (args.parent)                 fields.parent = { key: args.parent };
    return this.request<JiraCreatedIssue>('POST', '/issue', { fields });
  }

  private async updateIssueFieldsInternal(args: {
    issueKey: string;
    summary?: string;
    description?: string;
    assignee?: string;
    priority?: string;
    labels?: string[];
    fixVersion?: string;
  }): Promise<boolean> {
    const fields: Record<string, unknown> = {};
    if (args.summary !== undefined)     fields.summary = args.summary;
    if (args.description !== undefined) fields.description = args.description;
    if (args.assignee !== undefined)    fields.assignee = args.assignee ? { name: args.assignee } : null;
    if (args.priority !== undefined)    fields.priority = { name: args.priority };
    if (args.labels !== undefined)      fields.labels = args.labels;
    if (args.fixVersion !== undefined)  fields.fixVersions = args.fixVersion ? [{ name: args.fixVersion }] : [];
    if (Object.keys(fields).length === 0) return false;
    await this.request('PUT', `/issue/${encodeURIComponent(args.issueKey)}`, { fields });
    return true;
  }

  private async resolveTransitionId(issueKey: string, transitionId?: string, transitionName?: string): Promise<string> {
    if (transitionId) return transitionId;

    const requestedName = transitionName?.trim();
    if (!requestedName) {
      throw new Error('Provide transitionId or transitionName');
    }

    const data = await this.request<JiraTransitionsResult>('GET', `/issue/${encodeURIComponent(issueKey)}/transitions`);
    const transitions = data?.transitions ?? [];
    const lowered = requestedName.toLowerCase();
    const match = transitions.find((t) => t.name.toLowerCase() === lowered);

    if (!match) {
      const available = transitions.map((t) => t.name).join(', ') || '(none)';
      throw new Error(`Transition "${requestedName}" not found for ${issueKey}. Available: ${available}`);
    }

    return match.id;
  }

  private async transitionIssueInternal(issueKey: string, transitionId: string): Promise<void> {
    await this.request('POST', `/issue/${encodeURIComponent(issueKey)}/transitions`, {
      transition: { id: transitionId },
    });
  }

  private async resolveProjectKey(projectKey?: string): Promise<string> {
    if (projectKey) return projectKey;

    if (!this.projectsCache) {
      this.projectsCache = (await this.request<JiraProject[]>('GET', '/project?maxResults=100')) ?? [];
    }
    const projects = this.projectsCache;
    if (projects.length === 0) {
      throw new Error('No Jira projects found for your account.');
    }

    const keys = new Set(projects.map((p) => p.key));
    const branch = safeExec('git rev-parse --abbrev-ref HEAD');
    const branchMatch = branch.match(JIRA_KEY_IN_BRANCH_RE);
    const branchProjectKey = branchMatch?.[1];
    if (branchProjectKey && keys.has(branchProjectKey)) {
      return branchProjectKey;
    }

    if (projects.length === 1) {
      return projects[0].key;
    }

    const shown = projects.slice(0, 20);
    const lines = shown.map((p, i) => `${i + 1}. ${p.key} — ${p.name}`);
    const extra = projects.length > shown.length ? `\n...and ${projects.length - shown.length} more.` : '';
    throw new Error(
      `Please provide projectKey (or project) for this Jira action. Choose one of these project codes:\n${lines.join('\n')}${extra}`
    );
  }

  async searchIssues(args: {
    query?: string;
    jql?: string;
    project?: string;
    status?: string;
    assignee?: string;
    issueType?: string;
    maxResults?: number;
    startAt?: number;
  }): Promise<ToolResult> {
    const { maxResults = 20, startAt = 0 } = args;
    const jql = buildJQL(args);
    const params = new URLSearchParams({
      jql,
      maxResults: String(maxResults),
      startAt: String(startAt),
      fields: 'summary,status,assignee,priority,issuetype',
    });
    const data = await this.request<JiraSearchResult>('GET', `/search?${params}`);
    if (!data) return text('No results.');
    const lines = data.issues.map((i, idx) => {
      const assignee = i.fields.assignee?.displayName ?? 'Unassigned';
      return `${startAt + idx + 1}. [${i.key}] ${i.fields.summary} | ${i.fields.status.name} | ${assignee} | ${this.issueUrl(i.key)}`;
    });
    const page = pagination(data.total, startAt, data.issues.length);
    return text(`Found ${data.total} issues${page}:\n${lines.join('\n')}`);
  }

  async myIssues(args: { maxResults?: number; startAt?: number }): Promise<ToolResult> {
    return this.searchIssues({
      jql: 'assignee = currentUser() ORDER BY updated DESC',
      maxResults: args.maxResults,
      startAt: args.startAt,
    });
  }

  async getProjects(args: { maxResults?: number }): Promise<ToolResult> {
    const limit = args.maxResults ?? 50;
    const data = await this.request<JiraProject[]>('GET', `/project?maxResults=${limit}`);
    if (!data || data.length === 0) return text('No projects found.');
    const lines = data.map((p, i) => `${i + 1}. [${p.key}] ${p.name} (${p.projectTypeKey}) | ${this.projectUrl(p.key)}`);
    return text(`${data.length} project(s):\n${lines.join('\n')}`);
  }

  async getIssueTypes(args: { projectKey?: string }): Promise<ToolResult> {
    const projectKey = await this.resolveProjectKey(args.projectKey);
    const data = await this.request<JiraIssueTypeStatuses[]>('GET', `/project/${encodeURIComponent(projectKey)}/statuses`);
    if (!data || data.length === 0) return text('No issue types found.');
    const lines = data.map((t) => {
      const statuses = t.statuses.map((s) => s.name).join(', ');
      return `${t.name}: ${statuses}`;
    });
    return text(`Issue types and statuses for ${projectKey}:\n${lines.join('\n')}`);
  }

  async getSprints(args: {
    boardId: number;
    state?: string;
    maxResults?: number;
    startAt?: number;
  }): Promise<ToolResult> {
    const { boardId, maxResults = 20, startAt = 0 } = args;
    const params = new URLSearchParams({
      maxResults: String(maxResults),
      startAt: String(startAt),
    });
    if (args.state) params.set('state', args.state);

    const data = await this.requestAgile<JiraPage<JiraSprint>>('GET', `/board/${boardId}/sprint?${params}`);
    if (!data || data.values.length === 0) return text(`No sprints found for board ${boardId}.`);

    const lines = data.values.map((s, i) => {
      const window = [s.startDate?.slice(0, 10), s.endDate?.slice(0, 10)].filter(Boolean).join(' -> ');
      const goal = s.goal?.trim() ? ` | Goal: ${s.goal}` : '';
      return `${startAt + i + 1}. [${s.id}] ${s.name} | ${s.state}${window ? ` | ${window}` : ''}${goal} | ${this.sprintUrl(boardId, s.id)}`;
    });

    const rangeEnd = startAt + data.values.length;
    const page = data.isLast ? '' : ` (showing ${startAt + 1}-${rangeEnd}, use startAt=${rangeEnd} for next page)`;
    return text(`Sprints for board ${boardId}${page}:\nBoard URL: ${this.boardUrl(boardId)}\n${lines.join('\n')}`);
  }

  async searchUsers(args: { query: string; maxResults?: number }): Promise<ToolResult> {
    const params = new URLSearchParams({
      username: args.query,
      maxResults: String(args.maxResults ?? 10),
    });
    const data = await this.request<JiraUser[]>('GET', `/user/search?${params}`);
    if (!data || data.length === 0) return text('No users found.');
    const lines = data
      .filter((u) => u.active)
      .map((u, i) => `${i + 1}. ${u.displayName} (${u.name}) — ${u.emailAddress}`);
    return text(`${lines.length} user(s) found:\n${lines.join('\n')}`);
  }

  async getIssueFields(issueKey: string): Promise<{ summary: string; status: string; type: string }> {
    const data = await this.request<JiraIssue>('GET', `/issue/${encodeURIComponent(issueKey)}?fields=summary,status,issuetype`);
    if (!data) throw new Error(`Issue ${issueKey} not found`);
    return {
      summary: data.fields.summary,
      status: data.fields.status.name,
      type: data.fields.issuetype.name,
    };
  }

  async getIssue(args: { issueKey: string }): Promise<ToolResult> {
    const fields = 'summary,description,status,assignee,priority,issuetype,labels,components';
    const data = await this.request<JiraIssue>('GET', `/issue/${encodeURIComponent(args.issueKey)}?fields=${fields}`);
    if (!data) return text('Issue not found.');
    const f = data.fields;
    const lines = [
      `Issue: ${data.key} — ${f.summary}`,
      `URL:        ${this.issueUrl(data.key)}`,
      `Status:     ${f.status.name}`,
      `Type:       ${f.issuetype.name}`,
      `Priority:   ${f.priority?.name ?? 'None'}`,
      `Assignee:   ${f.assignee?.displayName ?? 'Unassigned'}`,
      `Labels:     ${f.labels?.join(', ') || 'None'}`,
      `Components: ${f.components?.map((c) => c.name).join(', ') || 'None'}`,
      '',
      'Description:',
      f.description ?? '(no description)',
    ];
    return text(lines.join('\n'));
  }

  async issueOverview(args: {
    issueKey: string;
    includeComments?: boolean;
    commentsMaxResults?: number;
    commentsStartAt?: number;
    includeTransitions?: boolean;
    includeSprint?: boolean;
  }): Promise<ToolResult> {
    const includeComments = args.includeComments ?? true;
    const includeTransitions = args.includeTransitions ?? true;
    const includeSprint = args.includeSprint ?? true;
    const commentsMaxResults = args.commentsMaxResults ?? 10;
    const commentsStartAt = args.commentsStartAt ?? 0;

    const fields = 'summary,description,status,assignee,priority,issuetype,labels,components,parent,fixVersions,issuelinks,subtasks';
    const issue = await this.request<JiraIssue>('GET', `/issue/${encodeURIComponent(args.issueKey)}?fields=${fields}`);
    if (!issue) return text('Issue not found.');

    const f = issue.fields;
    const lines = [
      `Issue: ${issue.key} — ${f.summary}`,
      `URL:        ${this.issueUrl(issue.key)}`,
      `Status:     ${f.status.name}`,
      `Type:       ${f.issuetype.name}`,
      `Priority:   ${f.priority?.name ?? 'None'}`,
      `Assignee:   ${f.assignee?.displayName ?? 'Unassigned'}`,
      `Labels:     ${f.labels?.join(', ') || 'None'}`,
      `Components: ${f.components?.map((c) => c.name).join(', ') || 'None'}`,
      ...(f.parent ? [`Parent:     [${f.parent.key}] ${f.parent.fields.summary} (${f.parent.fields.issuetype.name})`] : []),
      ...(f.fixVersions?.length ? [`Fix Vers:   ${f.fixVersions.map((v) => v.name).join(', ')}`] : []),
      ...(f.subtasks?.length ? [`Subtasks:   ${f.subtasks.map((s) => `[${s.key}] ${s.fields.summary} (${s.fields.status.name})`).join(', ')}`] : []),
      ...(f.issuelinks?.length ? [
        `Links:      ${f.issuelinks.map((l) => {
          if (l.outwardIssue) return `${l.type.outward} → [${l.outwardIssue.key}] ${l.outwardIssue.fields.summary}`;
          if (l.inwardIssue) return `${l.type.inward} ← [${l.inwardIssue.key}] ${l.inwardIssue.fields.summary}`;
          return l.type.name;
        }).join('; ')}`,
      ] : []),
    ];

    if (includeSprint) {
      try {
        const agileIssue = await this.requestAgile<JiraAgileIssue>('GET', `/issue/${encodeURIComponent(args.issueKey)}?fields=sprint,closedSprints`);
        const activeSprint = agileIssue?.fields?.sprint;
        const closedSprints = agileIssue?.fields?.closedSprints ?? [];
        if (activeSprint) {
          lines.push(`Sprint:     [${activeSprint.id}] ${activeSprint.name} (${activeSprint.state})`);
        } else {
          lines.push('Sprint:     None');
        }
        if (closedSprints.length > 0) {
          const closed = closedSprints.slice(0, 5).map((s) => `[${s.id}] ${s.name}`).join(', ');
          const more = closedSprints.length > 5 ? ` (+${closedSprints.length - 5} more)` : '';
          lines.push(`History:    ${closed}${more}`);
        }
      } catch (err) {
        lines.push(`Sprint:     unavailable (${(err as Error).message})`);
      }
    }

    if (includeTransitions) {
      const transitions = await this.request<JiraTransitionsResult>('GET', `/issue/${encodeURIComponent(args.issueKey)}/transitions`);
      const names = (transitions?.transitions ?? []).map((t) => `${t.name} -> ${t.to.name}`);
      lines.push(`Transitions: ${names.length > 0 ? names.join(', ') : '(none)'}`);
    }

    lines.push('', 'Description:', f.description ?? '(no description)');

    if (includeComments) {
      const comments = await this.request<JiraCommentResult>(
        'GET',
        `/issue/${encodeURIComponent(args.issueKey)}/comment?startAt=${commentsStartAt}&maxResults=${commentsMaxResults}`
      );
      const items = comments?.comments ?? [];
      const total = comments?.total ?? 0;
      const page = comments ? pagination(total, commentsStartAt, items.length) : '';
      lines.push('', `Comments: ${total}${page}`);
      if (items.length === 0) {
        lines.push('(none)');
      } else {
        for (const c of items) {
          const date = c.created.slice(0, 10);
          lines.push(`--- #${c.id} ${c.author.displayName} (${date}) ---`, c.body, '');
        }
      }
    }

    return text(lines.join('\n').trimEnd());
  }

  async boardOverview(args: {
    boardId: number;
    sprintState?: string;
    sprintMaxResults?: number;
    sprintStartAt?: number;
    includeIssues?: boolean;
    issueMaxResults?: number;
    issueStartAt?: number;
    assignee?: string;
    status?: string;
  }): Promise<ToolResult> {
    const includeIssues = args.includeIssues ?? true;
    const sprintMaxResults = args.sprintMaxResults ?? 10;
    const sprintStartAt = args.sprintStartAt ?? 0;
    const issueMaxResults = args.issueMaxResults ?? 25;
    const issueStartAt = args.issueStartAt ?? 0;

    const board = await this.requestAgile<JiraBoard>('GET', `/board/${args.boardId}`);
    const sprintParams = new URLSearchParams({
      maxResults: String(sprintMaxResults),
      startAt: String(sprintStartAt),
    });
    if (args.sprintState) {
      sprintParams.set('state', args.sprintState);
    } else {
      sprintParams.set('state', 'active,future');
    }

    const sprints = await this.requestAgile<JiraPage<JiraSprint>>('GET', `/board/${args.boardId}/sprint?${sprintParams}`);
    if (!sprints || sprints.values.length === 0) {
      return text(`Board ${args.boardId}${board?.name ? ` (${board.name})` : ''}: no matching sprints.`);
    }

    const issueFilterClauses: string[] = [];
    if (args.assignee) issueFilterClauses.push(`assignee = ${JSON.stringify(args.assignee)}`);
    if (args.status) issueFilterClauses.push(`status = ${JSON.stringify(args.status)}`);
    const issueJql = issueFilterClauses.length > 0 ? issueFilterClauses.join(' AND ') : '';

    const sprintIssueData = includeIssues
      ? await Promise.all(
        sprints.values.map(async (sprint) => {
          const params = new URLSearchParams({
            maxResults: String(issueMaxResults),
            startAt: String(issueStartAt),
            fields: 'summary,status,assignee,priority,issuetype',
          });
          if (issueJql) params.set('jql', issueJql);
          const issues = await this.requestAgile<JiraSearchResult>('GET', `/sprint/${sprint.id}/issue?${params}`);
          return { sprintId: sprint.id, issues };
        })
      )
      : [];

    const issueBySprint = new Map(sprintIssueData.map((entry) => [entry.sprintId, entry.issues]));

    const lines = [
      `Board: [${args.boardId}] ${board?.name ?? '(unknown)'} | ${board?.type ?? '(unknown type)'}`,
      `URL: ${this.boardUrl(args.boardId)}`,
      `Sprints: ${sprints.values.length}`,
      '',
    ];

    sprints.values.forEach((sprint, idx) => {
      const window = [sprint.startDate?.slice(0, 10), sprint.endDate?.slice(0, 10)].filter(Boolean).join(' -> ');
      lines.push(`${sprintStartAt + idx + 1}. [${sprint.id}] ${sprint.name} | ${sprint.state}${window ? ` | ${window}` : ''} | ${this.sprintUrl(args.boardId, sprint.id)}`);
      if (sprint.goal?.trim()) {
        lines.push(`   Goal: ${sprint.goal}`);
      }

      if (includeIssues) {
        const issueData = issueBySprint.get(sprint.id);
        const issues = issueData?.issues ?? [];
        lines.push(`   Issues: ${issueData?.total ?? 0}`);
        for (const issue of issues) {
          const assignee = issue.fields.assignee?.displayName ?? 'Unassigned';
          lines.push(`   - [${issue.key}] ${issue.fields.summary} | ${issue.fields.status.name} | ${assignee} | ${this.issueUrl(issue.key)}`);
        }
        if ((issueData?.total ?? 0) > issues.length) {
          lines.push(`   ...and ${(issueData?.total ?? 0) - issues.length} more (adjust issueStartAt/issueMaxResults).`);
        }
      }

      lines.push('');
    });

    const sprintRangeEnd = sprintStartAt + sprints.values.length;
    if (!sprints.isLast) {
      lines.push(`More sprints available: use sprintStartAt=${sprintRangeEnd}.`);
    }

    return text(lines.join('\n').trimEnd());
  }

  async createIssue(args: {
    projectKey?: string;
    issueType: string;
    summary: string;
    description?: string;
    assignee?: string;
    priority?: string;
    sprintId?: number;
  }): Promise<ToolResult> {
    const data = await this.createIssueInternal(args);
    if (!data) return text('Issue created.');
    const url = this.issueUrl(data.key);

    if (args.sprintId !== undefined) {
      await this.addIssuesToSprintInternal(args.sprintId, [data.key]);
      return text(`Created ${data.key} and added it to sprint ${args.sprintId}.\n${url}`);
    }

    return text(`Created ${data.key}.\n${url}`);
  }

  async updateIssue(args: {
    issueKey: string;
    summary?: string;
    description?: string;
    assignee?: string;
    priority?: string;
    sprintId?: number;
  }): Promise<ToolResult> {
    const hasFieldUpdates = await this.updateIssueFieldsInternal(args);

    if (!hasFieldUpdates && args.sprintId === undefined) return text('Nothing to update.');
    if (args.sprintId !== undefined) {
      await this.addIssuesToSprintInternal(args.sprintId, [args.issueKey]);
    }

    if (hasFieldUpdates && args.sprintId !== undefined) {
      return text(`Updated ${args.issueKey} and added it to sprint ${args.sprintId}.\n${this.issueUrl(args.issueKey)}`);
    }
    if (hasFieldUpdates) {
      return text(`Updated ${args.issueKey}.\n${this.issueUrl(args.issueKey)}`);
    }
    return text(`Added ${args.issueKey} to sprint ${args.sprintId}.\n${this.issueUrl(args.issueKey)}`);
  }

  async addIssuesToSprint(args: {
    sprintId: number;
    issueKey?: string;
    issueKeys?: string[];
  }): Promise<ToolResult> {
    const keys = new Set<string>();
    if (args.issueKey?.trim()) keys.add(args.issueKey.trim());
    for (const issueKey of args.issueKeys ?? []) {
      const trimmed = issueKey.trim();
      if (trimmed) keys.add(trimmed);
    }

    if (keys.size === 0) {
      throw new Error('Provide issueKey or issueKeys with at least one Jira issue key.');
    }

    const issueKeys = Array.from(keys);
    await this.addIssuesToSprintInternal(args.sprintId, issueKeys);
    if (issueKeys.length === 1) {
      return text(`Added ${issueKeys[0]} to sprint ${args.sprintId}.\n${this.issueUrl(issueKeys[0])}`);
    }
    const lines = [
      `Added ${issueKeys.length} issue(s) to sprint ${args.sprintId}.`,
      ...issueKeys.map((issueKey) => `${issueKey}: ${this.issueUrl(issueKey)}`),
    ];
    return text(lines.join('\n'));
  }

  async mutateIssue(args: {
    issueKey?: string;
    create?: {
      projectKey?: string;
      issueType: string;
      summary: string;
      description?: string;
      assignee?: string;
      priority?: string;
      labels?: string[];
      fixVersion?: string;
      parent?: string;
    };
    update?: {
      summary?: string;
      description?: string;
      assignee?: string;
      priority?: string;
      labels?: string[];
      fixVersion?: string;
    };
    sprintId?: number;
    removeFromSprint?: boolean;
    transitionId?: string;
    transitionName?: string;
    comment?: string;
    link?: { linkType: string; targetIssueKey: string; direction?: 'outward' | 'inward' };
    worklog?: { timeSpent: string; comment?: string; started?: string };
  }): Promise<ToolResult> {
    let issueKey = args.issueKey?.trim();
    const actions: string[] = [];

    if (args.create) {
      const created = await this.createIssueInternal(args.create);
      if (!created) throw new Error('Issue creation did not return an issue key.');
      issueKey = created.key;
      actions.push('created issue');
    }

    if (!issueKey) {
      throw new Error('Provide issueKey, or provide create with issueType and summary.');
    }

    if (args.update) {
      const updated = await this.updateIssueFieldsInternal({ issueKey, ...args.update });
      if (updated) actions.push('updated fields');
    }

    if (args.sprintId !== undefined) {
      await this.addIssuesToSprintInternal(args.sprintId, [issueKey]);
      actions.push(`added to sprint ${args.sprintId}`);
    }

    if (args.removeFromSprint) {
      await this.requestAgile('POST', '/backlog/issue', { issues: [issueKey] });
      actions.push('moved to backlog');
    }

    if (args.transitionId || args.transitionName) {
      const transitionId = await this.resolveTransitionId(issueKey, args.transitionId, args.transitionName);
      await this.transitionIssueInternal(issueKey, transitionId);
      actions.push(`transitioned via ${transitionId}`);
    }

    if (args.comment !== undefined) {
      await this.request('POST', `/issue/${encodeURIComponent(issueKey)}/comment`, { body: validateCommentBody(args.comment) });
      actions.push('added comment');
    }

    if (args.link) {
      const dir = args.link.direction ?? 'outward';
      await this.request('POST', '/issueLink', {
        type: { name: args.link.linkType },
        outwardIssue: { key: dir === 'outward' ? issueKey : args.link.targetIssueKey },
        inwardIssue:  { key: dir === 'outward' ? args.link.targetIssueKey : issueKey },
      });
      actions.push(`linked ${args.link.linkType} → ${args.link.targetIssueKey}`);
    }

    if (args.worklog) {
      const wBody: Record<string, unknown> = { timeSpent: args.worklog.timeSpent };
      if (args.worklog.comment) wBody.comment = args.worklog.comment;
      if (args.worklog.started) wBody.started = args.worklog.started;
      await this.request('POST', `/issue/${encodeURIComponent(issueKey)}/worklog`, wBody);
      actions.push(`logged ${args.worklog.timeSpent}`);
    }

    if (actions.length === 0) {
      return text('Nothing to mutate.');
    }

    return text(`Mutated ${issueKey}: ${actions.join(', ')}.\n${this.issueUrl(issueKey)}`);
  }

  async getComments(args: { issueKey: string; maxResults?: number; startAt?: number }): Promise<ToolResult> {
    const { issueKey, maxResults = 50, startAt = 0 } = args;
    const data = await this.request<JiraCommentResult>(
      'GET',
      `/issue/${encodeURIComponent(issueKey)}/comment?startAt=${startAt}&maxResults=${maxResults}`
    );
    if (!data || data.comments.length === 0) return text('No comments found.');
    const blocks = data.comments.map((c) => {
      const date = c.created.slice(0, 10);
      return `--- #${c.id} ${c.author.displayName} (${date}) ---\n${c.body}`;
    });
    const page = pagination(data.total, startAt, data.comments.length);
    return text(`${data.total} comment(s) on ${issueKey}${page}:\n\n${blocks.join('\n\n')}`);
  }

  async addComment(args: { issueKey: string; body: string }): Promise<ToolResult> {
    await this.request('POST', `/issue/${encodeURIComponent(args.issueKey)}/comment`, { body: validateCommentBody(args.body) });
    return text(`Comment added to ${args.issueKey}.`);
  }

  async editComment(args: { issueKey: string; commentId: string | number; body: string }): Promise<ToolResult> {
    const commentId = String(args.commentId ?? '').trim();
    if (!commentId || commentId === 'undefined' || commentId === 'null') {
      throw new Error('commentId is required.');
    }

    const path = `/issue/${encodeURIComponent(args.issueKey)}/comment/${encodeURIComponent(commentId)}`;
    const current = await this.request<JiraComment>('GET', path);
    if (!current) throw new Error(`Comment ${commentId} not found on ${args.issueKey}.`);
    await this.assertOwnComment(current);

    await this.request('PUT', path, { body: validateCommentBody(args.body) });
    return text(`Comment ${commentId} updated on ${args.issueKey}.`);
  }

  async deleteComment(args: { issueKey: string; commentId: string | number }): Promise<ToolResult> {
    const commentId = String(args.commentId ?? '').trim();
    if (!commentId || commentId === 'undefined' || commentId === 'null') {
      throw new Error('commentId is required.');
    }

    const path = `/issue/${encodeURIComponent(args.issueKey)}/comment/${encodeURIComponent(commentId)}`;
    const current = await this.request<JiraComment>('GET', path);
    if (!current) throw new Error(`Comment ${commentId} not found on ${args.issueKey}.`);
    await this.assertOwnComment(current);

    await this.request('DELETE', path);
    return text(`Comment ${commentId} deleted from ${args.issueKey}.`);
  }

  async getBoards(args: { projectKey?: string; maxResults?: number; startAt?: number }): Promise<ToolResult> {
    const params = new URLSearchParams({
      maxResults: String(args.maxResults ?? 25),
      startAt: String(args.startAt ?? 0),
    });
    if (args.projectKey) params.set('projectKeyOrId', args.projectKey);
    const data = await this.requestAgile<JiraPage<JiraBoard>>('GET', `/board?${params}`);
    if (!data || data.values.length === 0) return text('No boards found.');
    const lines = data.values.map((b, i) => {
      const projectHint = b.location?.projectKey ? ` [${b.location.projectKey}]` : '';
      return `${(args.startAt ?? 0) + i + 1}. [${b.id}] ${b.name} (${b.type})${projectHint} | ${this.boardUrl(b.id)}`;
    });
    const page = data.isLast ? '' : ` (use startAt=${(args.startAt ?? 0) + data.values.length} for next page)`;
    return text(`${data.values.length} board(s)${page}:\n${lines.join('\n')}`);
  }

  async transitionIssue(args: { issueKey: string; transitionId?: string; transitionName?: string }): Promise<ToolResult> {
    const transitionId = await this.resolveTransitionId(args.issueKey, args.transitionId, args.transitionName);
    await this.transitionIssueInternal(args.issueKey, transitionId);
    return text(`Transitioned ${args.issueKey} using transition ${transitionId}.\n${this.issueUrl(args.issueKey)}`);
  }
}
