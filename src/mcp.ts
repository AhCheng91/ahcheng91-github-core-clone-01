// The Model Context Protocol endpoint: JSON-RPC 2.0 over the Streamable HTTP transport, revision
// 2026-07-28 (`server/discover`) and the older handshake (`initialize`), with tools that act on the
// same store REST reads.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { sendJson } from './http.ts';
import {
  type Repo,
  type Store,
  VIEWER,
  createRepo,
  getRepo,
} from './store.ts';
import {
  type Json,
  addComment,
  createIssueRecord,
  createPullRecord,
  issueJson,
  mergePullRecord,
  pullJson,
  repoJson,
} from './service.ts';
import { branchRef } from './store.ts';

const PREFERRED_REVISION = '2026-07-28';
const SUPPORTED_VERSIONS = [PREFERRED_REVISION, '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const SERVER_INFO = { name: 'makefun-gh-forge', version: '1.0.0' };

/** JSON-RPC error codes the transport answers with. */
const PARSE_ERROR = -32700;
const INVALID_REQUEST = -32600;
const METHOD_NOT_FOUND = -32601;
const INVALID_PARAMS = -32602;

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Json;
}

const OBJ = 'object';

function schema(properties: Record<string, Json>, required: string[]): Json {
  return { type: OBJ, properties, required, additionalProperties: true };
}

const STR = { type: 'string' } as Json;

/** The tools, built once: two tools/list answers are then the same bytes. */
const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user. Returns the repository object.',
    inputSchema: schema({ name: STR, description: STR, private: { type: 'boolean' } }, ['name']),
  },
  {
    name: 'get_repo',
    description: 'Read one repository by its owner and name (or full_name "owner/repo").',
    inputSchema: schema({ owner: STR, repo: STR, full_name: STR }, []),
  },
  {
    name: 'create_issue',
    description: 'Open an issue in a repository. Returns the issue object with its number.',
    inputSchema: schema({ owner: STR, repo: STR, full_name: STR, title: STR, body: STR, labels: { type: 'array', items: STR } }, ['title']),
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, optionally filtered by state (open, closed or all).',
    inputSchema: schema({ owner: STR, repo: STR, full_name: STR, state: STR }, []),
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch onto a base branch of a repository.',
    inputSchema: schema({ owner: STR, repo: STR, full_name: STR, title: STR, body: STR, head: STR, base: STR }, ['title', 'head']),
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request and land its commit on the base branch.',
    inputSchema: schema({ owner: STR, repo: STR, full_name: STR, pull_number: { type: 'number' }, number: { type: 'number' } }, []),
  },
  {
    name: 'get_issue',
    description: 'Read one issue of a repository by its number.',
    inputSchema: schema({ owner: STR, repo: STR, full_name: STR, issue_number: { type: 'number' } }, []),
  },
  {
    name: 'add_comment',
    description: 'Add a comment to an issue and return it.',
    inputSchema: schema({ owner: STR, repo: STR, full_name: STR, issue_number: { type: 'number' }, body: STR }, ['body']),
  },
];

/** The tools/list answer, serialised once so both calls answer the same bytes. */
const TOOLS_LIST_JSON = JSON.stringify({
  jsonrpc: '2.0',
  id: null,
  result: { tools: TOOLS.map((tool) => ({ name: tool.name, description: tool.description, inputSchema: tool.inputSchema })) },
});

interface RpcRequest {
  jsonrpc?: unknown;
  id?: unknown;
  method?: unknown;
  params?: Record<string, unknown>;
}

type RpcOutcome = { status: number; body: string; id: unknown };

function result(id: unknown, value: unknown): RpcOutcome {
  return { status: 200, body: JSON.stringify({ jsonrpc: '2.0', id: id ?? null, result: value }), id };
}

function error(id: unknown, code: number, message: string): RpcOutcome {
  return { status: 200, body: JSON.stringify({ jsonrpc: '2.0', id: id ?? null, error: { code, message } }), id };
}

function argumentsOf(params: Record<string, unknown> | undefined): Record<string, unknown> {
  const args = params?.arguments;
  return typeof args === 'object' && args !== null && !Array.isArray(args) ? (args as Record<string, unknown>) : {};
}

function text(value: unknown): string {
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  return '';
}

/** The owner and repository a tool call names: explicit owner+repo, else full_name "owner/repo". */
function repoOf(store: Store, args: Record<string, unknown>): Repo | undefined {
  const owner = text(args.owner) || text(args.owner_login);
  const name = text(args.repo) || text(args.repo_name) || text(args.repository);
  if (owner !== '' && name !== '') return getRepo(store, owner, name);
  const full = text(args.full_name) || text(args.repository_full_name) || text(args.name_with_owner);
  const slash = full.indexOf('/');
  if (slash > 0) return getRepo(store, full.slice(0, slash), full.slice(slash + 1));
  if (name !== '') return getRepo(store, VIEWER.login, name);
  return undefined;
}

function numberArg(args: Record<string, unknown>, names: readonly string[]): number {
  for (const name of names) {
    const value = args[name];
    if (typeof value === 'number' && Number.isFinite(value)) return value;
    if (typeof value === 'string' && value !== '') {
      const parsed = Number.parseInt(value, 10);
      if (Number.isFinite(parsed)) return parsed;
    }
  }
  return Number.NaN;
}

function toolError(id: unknown, message: string): RpcOutcome {
  return result(id, { content: [{ type: 'text', text: message }], isError: true });
}

function toolResult(id: unknown, value: unknown): RpcOutcome {
  return result(id, { content: [{ type: 'text', text: JSON.stringify(value) }], structuredContent: value, isError: false });
}

/** Run one tool call against the store. */
function callTool(id: unknown, store: Store, name: string, args: Record<string, unknown>): RpcOutcome {
  switch (name) {
    case 'create_repo': {
      const repoName = text(args.name) || text(args.repo);
      if (repoName === '') return error(id, INVALID_PARAMS, 'create_repo needs a name');
      const existing = getRepo(store, VIEWER.login, repoName);
      if (existing !== undefined) return toolResult(id, repoJson(existing));
      const repo = createRepo(store, VIEWER.login, repoName, text(args.description), args.private === true);
      return toolResult(id, repoJson(repo));
    }
    case 'get_repo': {
      const repo = repoOf(store, args);
      if (repo === undefined) return toolError(id, 'repository not found');
      return toolResult(id, repoJson(repo));
    }
    case 'create_issue': {
      const repo = repoOf(store, args);
      if (repo === undefined) return toolError(id, 'repository not found');
      const labels = Array.isArray(args.labels) ? args.labels.filter((label): label is string => typeof label === 'string') : [];
      const created = createIssueRecord(repo, { title: text(args.title), body: text(args.body), labels });
      if ('error' in created) return toolError(id, created.error);
      return toolResult(id, issueJson(repo, created.issue));
    }
    case 'list_issues': {
      const repo = repoOf(store, args);
      if (repo === undefined) return toolError(id, 'repository not found');
      const state = text(args.state) || 'open';
      const issues = repo.issues.filter((issue) => state === 'all' || issue.state === state);
      return toolResult(id, issues.map((issue) => issueJson(repo, issue)));
    }
    case 'get_issue': {
      const repo = repoOf(store, args);
      if (repo === undefined) return toolError(id, 'repository not found');
      const number = numberArg(args, ['issue_number', 'number', 'issueNumber']);
      const issue = repo.issues.find((candidate) => candidate.number === number);
      if (issue === undefined) return toolError(id, 'issue not found');
      return toolResult(id, issueJson(repo, issue));
    }
    case 'add_comment': {
      const repo = repoOf(store, args);
      if (repo === undefined) return toolError(id, 'repository not found');
      const number = numberArg(args, ['issue_number', 'number', 'issueNumber']);
      const issue = repo.issues.find((candidate) => candidate.number === number);
      if (issue === undefined) return toolError(id, 'issue not found');
      const comment = addComment(repo, issue, text(args.body));
      return toolResult(id, { id: comment.id, body: comment.body, created_at: comment.createdAt });
    }
    case 'create_pull_request': {
      const repo = repoOf(store, args);
      if (repo === undefined) return toolError(id, 'repository not found');
      const head = text(args.head) || text(args.head_ref);
      if (head === '') return error(id, INVALID_PARAMS, 'create_pull_request needs a head branch');
      const created = createPullRecord(repo, { title: text(args.title), body: text(args.body), head, base: text(args.base) || text(args.base_ref) });
      if ('error' in created) return toolError(id, created.error);
      return toolResult(id, pullJson(repo, created.pull));
    }
    case 'merge_pull_request': {
      const repo = repoOf(store, args);
      if (repo === undefined) return toolError(id, 'repository not found');
      const number = numberArg(args, ['pull_number', 'number', 'pullNumber', 'pull_request_number']);
      if (!Number.isFinite(number)) return error(id, INVALID_PARAMS, 'merge_pull_request needs a pull_number');
      const pull = repo.pulls.find((candidate) => candidate.number === number);
      if (pull === undefined) return toolError(id, 'pull request not found');
      const merged = mergePullRecord(repo, pull);
      if ('error' in merged) return toolError(id, merged.error === 'no-base' ? 'base branch is gone' : 'pull request is not mergeable');
      return toolResult(id, { merged: true, sha: merged.sha, number: pull.number, state: pull.state });
    }
    default:
      return error(id, INVALID_PARAMS, `unknown tool: ${name}`);
  }
}

/** One JSON-RPC request -> one answer, or null for a notification (no answer). */
function handleRpc(store: Store, request: RpcRequest, authorized: boolean): RpcOutcome | null {
  const id = request.id ?? null;
  const method = typeof request.method === 'string' ? request.method : '';
  const params = typeof request.params === 'object' && request.params !== null ? (request.params as Record<string, unknown>) : {};
  if (method === '') return error(id, INVALID_REQUEST, 'missing method');
  if (method === 'server/discover') {
    return result(id, {
      supportedVersions: SUPPORTED_VERSIONS,
      capabilities: { tools: { listChanged: false } },
      serverInfo: SERVER_INFO,
      instructions: 'A GitHub-compatible forge: repositories, issues and pull requests.',
    });
  }
  if (method === 'initialize') {
    return result(id, {
      protocolVersion: typeof params.protocolVersion === 'string' ? params.protocolVersion : PREFERRED_REVISION,
      capabilities: { tools: { listChanged: false } },
      serverInfo: SERVER_INFO,
    });
  }
  if (method === 'notifications/initialized' || method === 'notifications/cancelled' || method === 'ping') {
    if (method === 'ping') return result(id, {});
    return null;
  }
  if (method === 'tools/list') {
    if (request.id === undefined || request.id === null) return null;
    return { status: 200, body: JSON.stringify({ jsonrpc: '2.0', id, result: { tools: TOOLS.map((tool) => ({ name: tool.name, description: tool.description, inputSchema: tool.inputSchema })) } }), id };
  }
  if (method === 'tools/call') {
    if (!authorized) return { status: 401, body: JSON.stringify({ jsonrpc: '2.0', id, error: { code: INVALID_REQUEST, message: 'unauthorized' } }), id };
    const rawName = typeof params.name === 'string' ? params.name : '';
    const name = rawName.includes('__') ? rawName.slice(rawName.lastIndexOf('__') + 2) : rawName;
    const known = TOOLS.find((tool) => tool.name === name || tool.name === name.toLowerCase());
    if (known === undefined) return error(id, METHOD_NOT_FOUND, `unknown tool: ${rawName}`);
    const required = (known.inputSchema.required as string[] | undefined) ?? [];
    const args = argumentsOf(params);
    for (const key of required) {
      if (!(key in args) && (key !== 'title' || !('title' in args))) return error(id, INVALID_PARAMS, `missing required argument: ${key}`);
    }
    return callTool(id, store, known.name, args);
  }
  return error(id, METHOD_NOT_FOUND, `unknown method: ${method}`);
}

export async function handleMcp(req: IncomingMessage, res: ServerResponse, store: Store, body: Buffer): Promise<void> {
  const authorized = typeof req.headers.authorization === 'string' && req.headers.authorization.startsWith('Bearer ');
  const raw = body.toString('utf8');
  let parsed: unknown;
  try {
    parsed = raw === '' ? null : JSON.parse(raw);
  } catch {
    sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: PARSE_ERROR, message: 'Parse error' } });
    return;
  }
  if (parsed === null || typeof parsed !== 'object') {
    sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: PARSE_ERROR, message: 'Parse error' } });
    return;
  }
  if (Array.isArray(parsed)) {
    const answers = parsed
      .map((item) => (typeof item === 'object' && item !== null ? handleRpc(store, item as RpcRequest, authorized) : error(null, INVALID_REQUEST, 'invalid request')))
      .filter((answer): answer is RpcOutcome => answer !== null);
    if (answers.length === 0) {
      res.writeHead(202, { 'x-github-request-id': 'GH-MCP' });
      res.end();
      return;
    }
    sendJson(res, 200, JSON.parse(`[${answers.map((answer) => answer.body).join(',')}]`) as unknown);
    return;
  }
  const request = parsed as RpcRequest;
  const method = typeof request.method === 'string' ? request.method : '';
  // The tools/call without a token is the one 401 the transport owes the suite.
  if (method === 'tools/call' && !authorized) {
    sendJson(res, 401, { jsonrpc: '2.0', id: request.id ?? null, error: { code: INVALID_REQUEST, message: 'unauthorized' } });
    return;
  }
  const answer = handleRpc(store, request, authorized);
  if (answer === null) {
    res.writeHead(202, { 'x-github-request-id': 'GH-MCP' });
    res.end();
    return;
  }
  sendJson(res, answer.status, JSON.parse(answer.body) as unknown);
}

export { TOOLS_LIST_JSON, branchRef };
