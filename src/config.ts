import { readFileSync, existsSync } from 'fs';
import { homedir } from 'os';
import { join, resolve } from 'path';

export interface Config {
  jira: {
    url: string;
    token: string;
    defaultProject?: string;
  };
  bitbucket: {
    url: string;
    token: string;
    defaultProject?: string;
    defaultRepo?: string;
  };
}

interface ConfigFile {
  jira?: { url?: string; token?: string; defaultProject?: string };
  bitbucket?: { url?: string; token?: string; defaultProject?: string; defaultRepo?: string };
}

function readJsonFile(filePath: string): ConfigFile | null {
  try {
    if (!existsSync(filePath)) return null;
    return JSON.parse(readFileSync(filePath, 'utf-8')) as ConfigFile;
  } catch {
    return null;
  }
}

function getConfigPath(): string | null {
  const configArgIndex = process.argv.indexOf('--config');
  if (configArgIndex !== -1 && process.argv[configArgIndex + 1]) {
    return resolve(process.argv[configArgIndex + 1]);
  }
  if (process.env.ATLASSIAN_MCP_CONFIG) {
    return resolve(process.env.ATLASSIAN_MCP_CONFIG);
  }
  const homeConfig = join(homedir(), '.atlassian-mcp.json');
  if (existsSync(homeConfig)) return homeConfig;
  const cwdConfig = join(process.cwd(), '.atlassian-mcp.json');
  if (existsSync(cwdConfig)) return cwdConfig;
  return null;
}

export function loadConfig(): Config {
  const configPath = getConfigPath();
  const file = configPath ? readJsonFile(configPath) : null;

  const jiraUrl = file?.jira?.url ?? process.env.JIRA_URL ?? '';
  const jiraToken = file?.jira?.token ?? process.env.JIRA_ACCESS_TOKEN ?? '';
  const bitbucketUrl = file?.bitbucket?.url ?? process.env.BITBUCKET_URL ?? '';
  const bitbucketToken = file?.bitbucket?.token ?? process.env.BITBUCKET_ACCESS_TOKEN ?? '';

  const missing: string[] = [];
  if (!jiraUrl) missing.push('jira.url (or JIRA_URL)');
  if (!jiraToken) missing.push('jira.token (or JIRA_ACCESS_TOKEN)');
  if (!bitbucketUrl) missing.push('bitbucket.url (or BITBUCKET_URL)');
  if (!bitbucketToken) missing.push('bitbucket.token (or BITBUCKET_ACCESS_TOKEN)');

  if (missing.length > 0) {
    throw new Error(
      `Missing required configuration: ${missing.join(', ')}.\n` +
      'Provide a config file (~/.atlassian-mcp.json or --config <path>) or set environment variables.'
    );
  }

  return {
    jira: {
      url: jiraUrl,
      token: jiraToken,
      defaultProject: file?.jira?.defaultProject ?? process.env.JIRA_DEFAULT_PROJECT,
    },
    bitbucket: {
      url: bitbucketUrl,
      token: bitbucketToken,
      defaultProject: file?.bitbucket?.defaultProject ?? process.env.BITBUCKET_DEFAULT_PROJECT,
      defaultRepo: file?.bitbucket?.defaultRepo ?? process.env.BITBUCKET_DEFAULT_REPO,
    },
  };
}
