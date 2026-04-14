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
  };
}

interface JiraSearchResult {
  total: number;
  startAt: number;
  issues: JiraIssue[];
}

interface JiraComment {
  author: { displayName: string };
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

interface JiraErrorPayload {
  errorMessages?: string[];
  errors?: Record<string, string>;
}

const JIRA_KEY_IN_BRANCH_RE = /\b([A-Z][A-Z0-9]+)-\d+\b/;

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
  if (args.jql) return args.jql;

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

export class JiraClient {
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
    const url = `${this.baseUrl}/rest/api/2${path}`;
    const opts: RequestInit = { method, headers: this.headers };
    if (body !== undefined) opts.body = JSON.stringify(body);
    const res = await fetch(url, opts);
    if (!res.ok) {
      const errText = await res.text();
      const details = parseJiraErrorDetails(errText);
      throw new Error(formatJiraError(res.status, method, path, details));
    }
    return res.status === 204 ? null : (res.json() as Promise<T>);
  }

  private async resolveProjectKey(projectKey?: string): Promise<string> {
    if (projectKey) return projectKey;

    const projects = (await this.request<JiraProject[]>('GET', '/project?maxResults=100')) ?? [];
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
      return `${startAt + idx + 1}. [${i.key}] ${i.fields.summary} | ${i.fields.status.name} | ${assignee}`;
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
    const lines = data.map((p, i) => `${i + 1}. [${p.key}] ${p.name} (${p.projectTypeKey})`);
    return text(`${data.length} project(s):\n${lines.join('\n')}`);
  }

  async getIssueTypes(args: { projectKey?: string }): Promise<ToolResult> {
    const projectKey = await this.resolveProjectKey(args.projectKey);
    const data = await this.request<JiraIssueTypeStatuses[]>('GET', `/project/${projectKey}/statuses`);
    if (!data || data.length === 0) return text('No issue types found.');
    const lines = data.map((t) => {
      const statuses = t.statuses.map((s) => s.name).join(', ');
      return `${t.name}: ${statuses}`;
    });
    return text(`Issue types and statuses for ${projectKey}:\n${lines.join('\n')}`);
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

  async getIssue(args: { issueKey: string }): Promise<ToolResult> {
    const fields = 'summary,description,status,assignee,priority,issuetype,labels,components';
    const data = await this.request<JiraIssue>('GET', `/issue/${args.issueKey}?fields=${fields}`);
    if (!data) return text('Issue not found.');
    const f = data.fields;
    const lines = [
      `Issue: ${data.key} — ${f.summary}`,
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

  async createIssue(args: {
    projectKey?: string;
    issueType: string;
    summary: string;
    description?: string;
    assignee?: string;
    priority?: string;
  }): Promise<ToolResult> {
    const projectKey = await this.resolveProjectKey(args.projectKey);
    const fields: Record<string, unknown> = {
      project: { key: projectKey },
      issuetype: { name: args.issueType },
      summary: args.summary,
    };
    if (args.description) fields.description = args.description;
    if (args.assignee)    fields.assignee = { name: args.assignee };
    if (args.priority)    fields.priority = { name: args.priority };
    const data = await this.request<JiraCreatedIssue>('POST', '/issue', { fields });
    if (!data) return text('Issue created.');
    return text(`Created ${data.key}.`);
  }

  async updateIssue(args: {
    issueKey: string;
    summary?: string;
    description?: string;
    assignee?: string;
    priority?: string;
  }): Promise<ToolResult> {
    const fields: Record<string, unknown> = {};
    if (args.summary !== undefined)     fields.summary = args.summary;
    if (args.description !== undefined) fields.description = args.description;
    if (args.assignee !== undefined)    fields.assignee = { name: args.assignee };
    if (args.priority !== undefined)    fields.priority = { name: args.priority };
    if (Object.keys(fields).length === 0) return text('Nothing to update.');
    await this.request('PUT', `/issue/${args.issueKey}`, { fields });
    return text(`Updated ${args.issueKey}.`);
  }

  async getComments(args: { issueKey: string; maxResults?: number; startAt?: number }): Promise<ToolResult> {
    const { issueKey, maxResults = 50, startAt = 0 } = args;
    const data = await this.request<JiraCommentResult>(
      'GET',
      `/issue/${issueKey}/comment?startAt=${startAt}&maxResults=${maxResults}`
    );
    if (!data || data.comments.length === 0) return text('No comments found.');
    const blocks = data.comments.map((c) => {
      const date = c.created.slice(0, 10);
      return `--- ${c.author.displayName} (${date}) ---\n${c.body}`;
    });
    const page = pagination(data.total, startAt, data.comments.length);
    return text(`${data.total} comment(s) on ${issueKey}${page}:\n\n${blocks.join('\n\n')}`);
  }

  async addComment(args: { issueKey: string; body: string }): Promise<ToolResult> {
    await this.request('POST', `/issue/${args.issueKey}/comment`, { body: args.body });
    return text(`Comment added to ${args.issueKey}.`);
  }

  async transitionIssue(args: { issueKey: string; transitionId?: string; transitionName?: string }): Promise<ToolResult> {
    let transitionId = args.transitionId;

    if (!transitionId) {
      const requestedName = args.transitionName?.trim();
      if (!requestedName) {
        throw new Error('Provide transitionId or transitionName');
      }

      const data = await this.request<JiraTransitionsResult>('GET', `/issue/${args.issueKey}/transitions`);
      const transitions = data?.transitions ?? [];
      const lowered = requestedName.toLowerCase();
      const match = transitions.find((t) => t.name.toLowerCase() === lowered);

      if (!match) {
        const available = transitions.map((t) => t.name).join(', ') || '(none)';
        throw new Error(`Transition "${requestedName}" not found for ${args.issueKey}. Available: ${available}`);
      }

      transitionId = match.id;
    }

    await this.request('POST', `/issue/${args.issueKey}/transitions`, {
      transition: { id: transitionId },
    });
    return text(`Transitioned ${args.issueKey} using transition ${transitionId}.`);
  }
}
