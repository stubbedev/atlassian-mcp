import { readFileSync, existsSync } from 'fs';
import { homedir } from 'os';
import { join, resolve } from 'path';

export interface ServiceConfig {
  url: string;
  token: string;
}

export interface Config {
  jira?: ServiceConfig;
  bitbucket?: ServiceConfig;
}

interface ConfigFile {
  jira?: { url?: string; token?: string };
  bitbucket?: { url?: string; token?: string };
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
  const config: Config = {};

  if (jiraUrl && jiraToken) {
    config.jira = { url: jiraUrl, token: jiraToken };
  } else if (jiraUrl || jiraToken) {
    // Partially configured — log which piece is missing so the user can fix it
    const missing: string[] = [];
    if (!jiraUrl) missing.push('jira.url (or JIRA_URL)');
    if (!jiraToken) missing.push('jira.token (or JIRA_ACCESS_TOKEN)');
    console.error(`[atlassian-mcp] Jira disabled: missing ${missing.join(', ')}`);
  }

  if (bitbucketUrl && bitbucketToken) {
    config.bitbucket = { url: bitbucketUrl, token: bitbucketToken };
  } else if (bitbucketUrl || bitbucketToken) {
    const missing: string[] = [];
    if (!bitbucketUrl) missing.push('bitbucket.url (or BITBUCKET_URL)');
    if (!bitbucketToken) missing.push('bitbucket.token (or BITBUCKET_ACCESS_TOKEN)');
    console.error(`[atlassian-mcp] Bitbucket disabled: missing ${missing.join(', ')}`);
  }

  return config;
}
