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
  // Explicit JQL wins — used as-is
  if (args.jql) return args.jql;

  const clauses: string[] = [];

  if (args.query) {
    // text ~ searches summary, description, comments, environment — Lucene full-text with stemming
    clauses.push(`text ~ ${JSON.stringify(args.query)}`);
  }
  if (args.project)   clauses.push(`project = "${args.project}"`);
  if (args.status)    clauses.push(`status = "${args.status}"`);
  if (args.assignee)  clauses.push(`assignee = "${args.assignee}"`);
  if (args.issueType) clauses.push(`issuetype = "${args.issueType}"`);

  if (clauses.length === 0) throw new Error('Provide at least one of: query, jql, project, status, assignee, issueType');

  return clauses.join(' AND ') + ' ORDER BY updated DESC';
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
      throw new Error(`Jira ${res.status} ${method} ${path}: ${errText}`);
    }
    return res.status === 204 ? null : (res.json() as Promise<T>);
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
    projectKey: string;
    issueType: string;
    summary: string;
    description?: string;
    assignee?: string;
    priority?: string;
  }): Promise<ToolResult> {
    const fields: Record<string, unknown> = {
      project: { key: args.projectKey },
      issuetype: { name: args.issueType },
      summary: args.summary,
    };
    if (args.description) fields.description = args.description;
    if (args.assignee) fields.assignee = { name: args.assignee };
    if (args.priority) fields.priority = { name: args.priority };
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
    if (args.summary !== undefined) fields.summary = args.summary;
    if (args.description !== undefined) fields.description = args.description;
    if (args.assignee !== undefined) fields.assignee = { name: args.assignee };
    if (args.priority !== undefined) fields.priority = { name: args.priority };
    if (Object.keys(fields).length === 0) return text('Nothing to update.');
    await this.request('PUT', `/issue/${args.issueKey}`, { fields });
    return text(`Updated ${args.issueKey}.`);
  }

  async assignIssue(args: { issueKey: string; assignee: string | null }): Promise<ToolResult> {
    await this.request('PUT', `/issue/${args.issueKey}/assignee`, { name: args.assignee });
    const msg = args.assignee ? `Assigned ${args.issueKey} to ${args.assignee}.` : `Unassigned ${args.issueKey}.`;
    return text(msg);
  }

  async getProjects(args: { maxResults?: number }): Promise<ToolResult> {
    const limit = args.maxResults ?? 50;
    const data = await this.request<JiraProject[]>('GET', `/project?maxResults=${limit}`);
    if (!data || data.length === 0) return text('No projects found.');
    const lines = data.map((p, i) => `${i + 1}. [${p.key}] ${p.name} (${p.projectTypeKey})`);
    return text(`${data.length} project(s):\n${lines.join('\n')}`);
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

  async getTransitions(args: { issueKey: string }): Promise<ToolResult> {
    const data = await this.request<JiraTransitionsResult>(
      'GET',
      `/issue/${args.issueKey}/transitions`
    );
    if (!data || data.transitions.length === 0) return text('No transitions available.');
    const lines = data.transitions.map((t) => `${t.id}: ${t.name} → ${t.to.name}`);
    return text(`Available transitions for ${args.issueKey}:\n${lines.join('\n')}`);
  }

  async transitionIssue(args: { issueKey: string; transitionId: string }): Promise<ToolResult> {
    await this.request('POST', `/issue/${args.issueKey}/transitions`, {
      transition: { id: args.transitionId },
    });
    return text(`Transitioned ${args.issueKey} using transition ${args.transitionId}.`);
  }
}
