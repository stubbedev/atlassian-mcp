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
  issues: JiraIssue[];
}

interface JiraComment {
  author: { displayName: string };
  created: string;
  body: string;
}

interface JiraCommentResult {
  total: number;
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

function text(t: string): ToolResult {
  return { content: [{ type: 'text', text: t }] };
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

  async searchIssues(args: { jql: string; maxResults?: number }): Promise<ToolResult> {
    const { jql, maxResults = 20 } = args;
    const params = new URLSearchParams({
      jql,
      maxResults: String(maxResults),
      fields: 'summary,status,assignee,priority,issuetype',
    });
    const data = await this.request<JiraSearchResult>('GET', `/search?${params}`);
    if (!data) return text('No results.');
    const lines = data.issues.map((i, idx) => {
      const assignee = i.fields.assignee?.displayName ?? 'Unassigned';
      return `${idx + 1}. [${i.key}] ${i.fields.summary} | ${i.fields.status.name} | ${assignee}`;
    });
    return text(`Found ${data.total} issues (showing ${data.issues.length}):\n${lines.join('\n')}`);
  }

  async getIssue(args: { issueKey: string }): Promise<ToolResult> {
    const fields = 'summary,description,status,assignee,priority,issuetype,labels,components';
    const data = await this.request<JiraIssue>('GET', `/issue/${args.issueKey}?fields=${fields}`);
    if (!data) return text('Issue not found.');
    const f = data.fields;
    const lines = [
      `Issue: ${data.key} — ${f.summary}`,
      `Status:   ${f.status.name}`,
      `Type:     ${f.issuetype.name}`,
      `Priority: ${f.priority?.name ?? 'None'}`,
      `Assignee: ${f.assignee?.displayName ?? 'Unassigned'}`,
      `Labels:   ${f.labels?.join(', ') || 'None'}`,
      `Components: ${f.components?.map((c) => c.name).join(', ') || 'None'}`,
      '',
      'Description:',
      f.description ?? '(no description)',
    ];
    return text(lines.join('\n'));
  }

  async getComments(args: { issueKey: string; maxResults?: number }): Promise<ToolResult> {
    const { issueKey, maxResults = 50 } = args;
    const data = await this.request<JiraCommentResult>(
      'GET',
      `/issue/${issueKey}/comment?startAt=0&maxResults=${maxResults}`
    );
    if (!data || data.comments.length === 0) return text('No comments found.');
    const blocks = data.comments.map((c) => {
      const date = c.created.slice(0, 10);
      return `--- ${c.author.displayName} (${date}) ---\n${c.body}`;
    });
    return text(`${data.total} comment(s) on ${issueKey}:\n\n${blocks.join('\n\n')}`);
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
