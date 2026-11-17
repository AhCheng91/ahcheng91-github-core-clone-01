// GitHub REST v3: repositories, branches, commits, contents, labels, issues, comments and pull
// requests. Every reply is JSON with an X-GitHub-Request-Id; errors carry a `message`.

import type { ServerResponse } from 'node:http';
import { sendError, sendJson, sendJsonText, sendNoContent } from './http.ts';
import {
  type Issue,
  type IssueComment,
  type Label,
  type Pull,
  type Repo,
  type Store,
  VIEWER,
  blobAt,
  blobData,
  branchRef,
  commitChanges,
  commitTree,
  createRepo,
  deleteRepo,
  filesAt,
  getRepo,
  resolveCommit,
  stamp,
} from './store.ts';
import { type TreeEntry, parseCommit, parseTree, peelShallow } from './gitobj.ts';

export interface RestRequest {
  store: Store;
  method: string;
  segments: string[];
  query: URLSearchParams;
  base: string;
  body: Buffer;
  res: ServerResponse;
}

const INSTALLED_VERSION = 'makefun-forge/1.0';
const USER = { login: VIEWER.login, id: VIEWER.id, type: 'User', site_admin: false };

// ---------------------------------------------------------------------------
// JSON views

function labelJson(label: Label): Record<string, unknown> {
  return { id: label.id, name: label.name, color: label.color, description: label.description, default: false };
}

function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map(labelJson),
    user: issue.user,
    assignee: null,
    assignees: [],
    milestone: null,
    comments: 0,
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.closedAt,
    html_url: `${repo.owner}/${repo.name}#${issue.number}`,
    locked: false,
  };
}

function commentJson(comment: IssueComment): Record<string, unknown> {
  return {
    id: comment.id,
    body: comment.body,
    user: comment.user,
    created_at: comment.createdAt,
    updated_at: comment.createdAt,
  };
}

function pullJson(repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    number: pull.number,
    state: pull.state,
    title: pull.title,
    body: pull.body,
    user: pull.user,
    head: { ref: pull.headRef, sha: pull.headSha, label: `${repo.owner}:${pull.headRef}`, user: USER },
    base: { ref: pull.baseRef, sha: pull.baseSha, label: `${repo.owner}:${pull.baseRef}`, user: USER },
    merge_commit_sha: pull.mergeCommitSha,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    mergeable: !pull.merged,
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    html_url: `${repo.owner}/${repo.name}/pull/${pull.number}`,
  };
}

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: `R_${repo.id}`,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: repo.owner === VIEWER.login ? VIEWER.id : 2000, type: 'User' },
    private: repo.private,
    description: repo.description,
    fork: false,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    pushed_at: repo.updatedAt,
    size: 0,
    open_issues_count: repo.issues.filter((issue) => issue.state === 'open').length,
    has_issues: true,
    html_url: `${repo.owner}/${repo.name}`,
  };
}

function branchJson(repo: Repo, name: string, sha: string): Record<string, unknown> {
  return { name, commit: { sha, url: `${repo.owner}/${repo.name}/commits/${sha}` }, protected: false };
}

function commitJson(repo: Repo, sha: string): Record<string, unknown> {
  const obj = repo.objects.get(sha);
  const fields = obj === undefined ? { tree: '', parents: [] as string[], author: '', committer: '', message: '' } : parseCommit(obj.data);
  return {
    sha,
    node_id: sha,
    commit: {
      message: fields.message,
      author: { name: 'Ada Iversen', email: VIEWER.email, date: repo.createdAt },
      committer: { name: 'Ada Iversen', email: VIEWER.email, date: repo.createdAt },
      tree: { sha: fields.tree },
    },
    parents: fields.parents.map((parent) => ({ sha: parent })),
  };
}

// ---------------------------------------------------------------------------
// helpers

function numberParam(query: URLSearchParams, name: string, fallback: number): number {
  const raw = query.get(name);
  if (raw === null) return fallback;
  const value = Number.parseInt(raw, 10);
  return Number.isFinite(value) && value > 0 ? value : fallback;
}

function pageItems<T>(items: readonly T[], query: URLSearchParams): { slice: T[]; page: number; perPage: number } {
  const perPage = Math.min(100, numberParam(query, 'per_page', 30));
  const page = numberParam(query, 'page', 1);
  const start = (page - 1) * perPage;
  return { slice: items.slice(start, start + perPage), page, perPage };
}

function linkHeader(base: string, path: string, query: URLSearchParams, page: number, perPage: number, hasMore: boolean): Record<string, string> | undefined {
  if (!hasMore) return undefined;
  const next = new URLSearchParams(query);
  next.set('per_page', String(perPage));
  next.set('page', String(page + 1));
  return { link: `<${base}${path}?${next.toString()}>; rel="next"` };
}

function parseJsonBody(body: Buffer): Record<string, unknown> | null {
  if (body.length === 0) return {};
  try {
    const parsed: unknown = JSON.parse(body.toString('utf8'));
    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) return null;
    return parsed as Record<string, unknown>;
  } catch {
    return null;
  }
}

function str(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function lines(data: Buffer | null): string[] {
  if (data === null) return [];
  const text = data.toString('utf8');
  if (text === '') return [];
  return text.split('\n');
}

/** Added and deleted line counts between two blobs, by counting unmatched lines. */
function lineDelta(before: Buffer | null, after: Buffer | null): { additions: number; deletions: number } {
  const oldLines = lines(before);
  const newLines = lines(after);
  const counts = new Map<string, number>();
  for (const line of oldLines) counts.set(line, (counts.get(line) ?? 0) + 1);
  let matched = 0;
  for (const line of newLines) {
    const available = counts.get(line) ?? 0;
    if (available > 0) {
      counts.set(line, available - 1);
      matched += 1;
    }
  }
  return { additions: newLines.length - matched, deletions: oldLines.length - matched };
}

interface FileDiff {
  filename: string;
  status: 'added' | 'removed' | 'modified';
  additions: number;
  deletions: number;
  changes: number;
}

/** The changed files between two commits, in path order. */
function diffFiles(repo: Repo, baseSha: string | null, headSha: string | null): FileDiff[] {
  const before = filesAt(repo, baseSha);
  const after = filesAt(repo, headSha);
  const out: FileDiff[] = [];
  const paths = new Set<string>([...before.keys(), ...after.keys()]);
  const sorted = [...paths].sort();
  for (const path of sorted) {
    const oldSha = before.get(path) ?? null;
    const newSha = after.get(path) ?? null;
    if (oldSha === newSha) continue;
    if (oldSha === null && newSha !== null) {
      const counts = lineDelta(null, blobData(repo, newSha));
      out.push({ filename: path, status: 'added', additions: counts.additions, deletions: 0, changes: counts.additions });
    } else if (oldSha !== null && newSha === null) {
      const counts = lineDelta(blobData(repo, oldSha), null);
      out.push({ filename: path, status: 'removed', additions: 0, deletions: counts.deletions, changes: counts.deletions });
    } else if (oldSha !== null && newSha !== null) {
      const counts = lineDelta(blobData(repo, oldSha), blobData(repo, newSha));
      out.push({ filename: path, status: 'modified', additions: counts.additions, deletions: counts.deletions, changes: counts.additions + counts.deletions });
    }
  }
  return out;
}

function findLabel(repo: Repo, name: string): Label | undefined {
  return repo.labels.find((label) => label.name === name);
}

function ensureLabel(repo: Repo, name: string): Label {
  const existing = findLabel(repo, name);
  if (existing !== undefined) return existing;
  const label: Label = { id: repo.nextLabel, name, color: 'ededed', description: '' };
  repo.nextLabel += 1;
  repo.labels.push(label);
  return label;
}

// ---------------------------------------------------------------------------
// the router

/** Answer a `/api/v3` request. Returns false when no route matched (the caller sends the 404). */
export function handleRest(request: RestRequest): boolean {
  const { method, segments, query, res, store } = request;
  if (segments.length === 0) {
    if (method === 'GET') {
      sendJson(res, 200, { current_user_url: '/api/v3/user', repositories_url: '/api/v3/user/repos' }, { 'x-oauth-scopes': 'repo, read:org' });
      return true;
    }
    return false;
  }
  const head = segments[0];
  if (head === 'meta') {
    sendJson(res, 200, { installed_version: INSTALLED_VERSION });
    return true;
  }
  if (head === 'user') {
    if (segments.length === 1) {
      if (method !== 'GET') return false;
      sendJson(res, 200, { ...USER, name: VIEWER.name, email: VIEWER.email, public_repos: 0 });
      return true;
    }
    if (segments[1] === 'repos') {
      if (segments.length !== 2) return false;
      if (method === 'GET') return listRepos(request);
      if (method === 'POST') return createRepoRoute(request);
      return false;
    }
    return false;
  }
  if (head === 'repos' && segments.length >= 3) {
    const owner = decodeURIComponent(segments[1]);
    const name = decodeURIComponent(segments[2]);
    const repo = getRepo(store, owner, name);
    if (repo === undefined) {
      sendError(res, 404, 'Not Found');
      return true;
    }
    return handleRepo(request, repo, segments.slice(3));
  }
  if (head === 'users' && segments.length === 2) {
    sendJson(res, 200, { ...USER, login: decodeURIComponent(segments[1]) });
    return true;
  }
  return false;
}

function listRepos(request: RestRequest): boolean {
  const { store, query, res, base } = request;
  const all = store.repoNames.map((key) => store.repos.get(key)).filter((repo): repo is Repo => repo !== undefined && repo.owner === VIEWER.login);
  const { slice, page, perPage } = pageItems(all, query);
  const extra = linkHeader(base, '/api/v3/user/repos', query, page, perPage, page * perPage < all.length);
  sendJson(res, 200, slice.map(repoJson), extra);
  return true;
}

function createRepoRoute(request: RestRequest): boolean {
  const { store, res } = request;
  const body = parseJsonBody(request.body);
  if (body === null) {
    sendError(res, 400, 'Problems parsing JSON');
    return true;
  }
  const name = str(body.name);
  if (name === '') {
    sendError(res, 422, 'Repository name is required');
    return true;
  }
  if (getRepo(store, VIEWER.login, name) !== undefined) {
    sendError(res, 422, 'Repository creation failed: name already exists');
    return true;
  }
  const repo = createRepo(store, VIEWER.login, name, str(body.description), body.private === true);
  sendJson(res, 201, repoJson(repo), { location: `${request.base}/api/v3/repos/${repo.owner}/${repo.name}` });
  return true;
}

function handleRepo(request: RestRequest, repo: Repo, rest: string[]): boolean {
  const { method, res, store } = request;
  if (rest.length === 0) {
    if (method === 'GET') {
      sendJson(res, 200, repoJson(repo));
      return true;
    }
    if (method === 'PATCH') {
      const body = parseJsonBody(request.body);
      if (body === null) {
        sendError(res, 400, 'Problems parsing JSON');
        return true;
      }
      if (typeof body.description === 'string') repo.description = body.description;
      if (typeof body.name === 'string' && body.name !== '' && body.name !== repo.name) {
        if (getRepo(store, repo.owner, body.name) !== undefined) {
          sendError(res, 422, 'Repository creation failed: name already exists');
          return true;
        }
        store.repos.delete(`${repo.owner}/${repo.name}`);
        const index = store.repoNames.indexOf(`${repo.owner}/${repo.name}`);
        if (index >= 0) store.repoNames.splice(index, 1, `${repo.owner}/${body.name}`);
        repo.name = body.name;
        store.repos.set(`${repo.owner}/${repo.name}`, repo);
      }
      repo.updatedAt = stamp(repo);
      sendJson(res, 200, repoJson(repo));
      return true;
    }
    if (method === 'DELETE') {
      deleteRepo(store, repo.owner, repo.name);
      sendNoContent(res);
      return true;
    }
    return false;
  }

  const head = rest[0];
  if (head === 'branches' && rest.length === 1 && method === 'GET') {
    const branches: Record<string, unknown>[] = [];
    for (const [ref, sha] of repo.refs) {
      if (!ref.startsWith('refs/heads/')) continue;
      branches.push(branchJson(repo, ref.slice('refs/heads/'.length), sha));
    }
    sendJson(res, 200, branches);
    return true;
  }

  if (head === 'commits' && rest.length === 1 && method === 'GET') {
    const wanted = query_ref(request);
    const tip = resolveCommit(repo, wanted);
    const out: Record<string, unknown>[] = [];
    let current = tip;
    const seen = new Set<string>();
    while (current !== null && out.length < 100 && !seen.has(current)) {
      seen.add(current);
      const obj = repo.objects.get(current);
      if (obj === undefined || obj.type !== 'commit') break;
      out.push(commitJson(repo, current));
      current = parseCommit(obj.data).parents[0] ?? null;
    }
    sendJson(res, 200, out);
    return true;
  }

  if (head === 'git' && rest[1] === 'refs') {
    if (rest.length === 2 && method === 'GET') {
      const refs = [...repo.refs].map(([ref, sha]) => ({ ref, object: { sha, type: 'commit' } }));
      sendJson(res, 200, refs);
      return true;
    }
    if (rest.length === 2 && method === 'POST') {
      const body = parseJsonBody(request.body);
      if (body === null) {
        sendError(res, 400, 'Problems parsing JSON');
        return true;
      }
      const ref = str(body.ref);
      const sha = str(body.sha);
      if (!ref.startsWith('refs/') || !/^[0-9a-f]{40}$/.test(sha)) {
        sendError(res, 422, 'Reference update failed');
        return true;
      }
      if (!repo.objects.has(sha)) {
        sendError(res, 422, 'Object does not exist');
        return true;
      }
      repo.refs.set(ref, sha);
      sendJson(res, 201, { ref, object: { sha, type: 'commit', url: `${request.base}/api/v3/repos/${repo.owner}/${repo.name}/git/refs` } });
      return true;
    }
    return false;
  }

  if (head === 'contents') return handleContents(request, repo, rest.slice(1));
  if (head === 'labels') return handleLabels(request, repo, rest.slice(1));
  if (head === 'issues') return handleIssues(request, repo, rest.slice(1));
  if (head === 'pulls') return handlePulls(request, repo, rest.slice(1));
  return false;
}

function query_ref(request: RestRequest): string | null {
  const sha = request.query.get('sha');
  return sha === null || sha === '' ? null : sha;
}

// ---------------------------------------------------------------------------
// contents

function handleContents(request: RestRequest, repo: Repo, pathParts: string[]): boolean {
  const { method, res } = request;
  const path = pathParts.join('/');
  const ref = request.query.get('ref');
  if (path === '') {
    sendError(res, 404, 'Not Found');
    return true;
  }
  if (method === 'GET') {
    const commit = resolveCommit(repo, ref);
    const tree = commit === null ? null : commitTree(repo, commit);
    if (commit === null || tree === null) {
      sendError(res, 404, 'Not Found');
      return true;
    }
    const parts = path.split('/').filter((part) => part !== '');
    let current = tree;
    for (let i = 0; i < parts.length; i += 1) {
      const entries = parseTree(repo.objects.get(current)?.data ?? Buffer.alloc(0));
      const entry = entries.find((candidate) => candidate.name === parts[i]);
      if (entry === undefined) {
        sendError(res, 404, 'Not Found');
        return true;
      }
      if (i === parts.length - 1) {
        if (entry.mode === '40000') {
          const listing = parseTree(repo.objects.get(entry.sha)?.data ?? Buffer.alloc(0)).map((child) => ({
            type: child.mode === '40000' ? 'dir' : 'file',
            name: child.name,
            path: `${path}/${child.name}`,
            sha: child.sha,
            size: child.mode === '40000' ? 0 : (repo.objects.get(child.sha)?.data.length ?? 0),
            mode: child.mode === '40000' ? '040000' : child.mode,
          }));
          sendJson(res, 200, listing);
          return true;
        }
        const data = repo.objects.get(entry.sha)?.data ?? Buffer.alloc(0);
        sendJson(res, 200, {
          type: 'file',
          name: entry.name,
          path,
          sha: entry.sha,
          size: data.length,
          encoding: 'base64',
          content: data.toString('base64'),
          mode: entry.mode === '100755' ? '100755' : '100644',
        });
        return true;
      }
      if (entry.mode !== '40000') {
        sendError(res, 404, 'Not Found');
        return true;
      }
      current = entry.sha;
    }
    sendError(res, 404, 'Not Found');
    return true;
  }

  if (method === 'PUT') {
    const body = parseJsonBody(request.body);
    if (body === null) {
      sendError(res, 400, 'Problems parsing JSON');
      return true;
    }
    const content = typeof body.content === 'string' ? Buffer.from(body.content, 'base64') : null;
    if (content === null) {
      sendError(res, 422, 'Content is required');
      return true;
    }
    const branch = str(body.branch) === '' ? repo.defaultBranch : str(body.branch);
    const currentSha = blobAt(repo, branch, path);
    const supplied = str(body.sha);
    if (supplied !== '') {
      if (currentSha !== supplied) {
        sendError(res, 409, 'sha does not match the current file');
        return true;
      }
    } else if (currentSha !== null) {
      sendError(res, 409, 'sha is required to update an existing file');
      return true;
    }
    const message = str(body.message);
    const result = commitChanges(repo, branch, [{ path, content }], message === '' ? `Update ${path}` : message);
    const blobSha = result.blobs.get(path) ?? '';
    sendJson(res, currentSha === null ? 201 : 200, {
      content: { name: path.split('/').pop() ?? path, path, sha: blobSha, size: content.length, encoding: 'base64', content: content.toString('base64'), type: 'file' },
      commit: { sha: result.commit, message, author: { name: 'Ada Iversen', email: VIEWER.email } },
    });
    return true;
  }

  if (method === 'DELETE') {
    const body = parseJsonBody(request.body);
    if (body === null) {
      sendError(res, 400, 'Problems parsing JSON');
      return true;
    }
    const branch = str(body.branch) === '' ? repo.defaultBranch : str(body.branch);
    const currentSha = blobAt(repo, branch, path);
    if (currentSha === null) {
      sendError(res, 404, 'Not Found');
      return true;
    }
    const supplied = str(body.sha);
    if (supplied !== '' && supplied !== currentSha) {
      sendError(res, 409, 'sha does not match the current file');
      return true;
    }
    const message = str(body.message);
    const result = commitChanges(repo, branch, [{ path, content: null }], message === '' ? `Delete ${path}` : message);
    sendJson(res, 200, { content: null, commit: { sha: result.commit, message } });
    return true;
  }
  return false;
}

// ---------------------------------------------------------------------------
// labels, issues, comments

function handleLabels(request: RestRequest, repo: Repo, rest: string[]): boolean {
  const { method, res } = request;
  if (rest.length === 0) {
    if (method === 'GET') {
      sendJson(res, 200, repo.labels.map(labelJson));
      return true;
    }
    if (method === 'POST') {
      const body = parseJsonBody(request.body);
      if (body === null) {
        sendError(res, 400, 'Problems parsing JSON');
        return true;
      }
      const name = str(body.name);
      if (name === '') {
        sendError(res, 422, 'Label name is required');
        return true;
      }
      const existing = findLabel(repo, name);
      if (existing !== undefined) {
        sendJson(res, 422, { message: 'Validation Failed', errors: [{ resource: 'Label', code: 'already_exists', field: 'name' }] });
        return true;
      }
      const label: Label = { id: repo.nextLabel, name, color: str(body.color) === '' ? 'ededed' : str(body.color), description: str(body.description) };
      repo.nextLabel += 1;
      repo.labels.push(label);
      sendJson(res, 201, labelJson(label));
      return true;
    }
    return false;
  }
  const name = rest.map((part) => decodeURIComponent(part)).join('/');
  if (rest.length >= 1 && method === 'GET') {
    const label = findLabel(repo, name);
    if (label === undefined) {
      sendError(res, 404, 'Not Found');
      return true;
    }
    sendJson(res, 200, labelJson(label));
    return true;
  }
  return false;
}

function handleIssues(request: RestRequest, repo: Repo, rest: string[]): boolean {
  const { method, res, query } = request;
  if (rest.length === 0) {
    if (method === 'GET') {
      const state = query.get('state') ?? 'open';
      let items = repo.issues;
      if (state !== 'all') items = items.filter((issue) => issue.state === state);
      const sort = query.get('sort') ?? 'created';
      const direction = query.get('direction') === 'asc' ? 1 : -1;
      const sorted = items.slice().sort((a, b) => {
        if (sort === 'created') return (a.number - b.number) * direction;
        if (sort === 'updated') return (a.updatedAt < b.updatedAt ? -1 : a.updatedAt > b.updatedAt ? 1 : 0) * direction;
        return (a.number - b.number) * direction;
      });
      const { slice } = pageItems(sorted, query);
      sendJson(res, 200, slice.map((issue) => issueJson(repo, issue)));
      return true;
    }
    if (method === 'POST') {
      const body = parseJsonBody(request.body);
      if (body === null) {
        sendError(res, 400, 'Problems parsing JSON');
        return true;
      }
      const title = str(body.title);
      if (title === '') {
        sendError(res, 422, 'Issue title is required');
        return true;
      }
      const names = Array.isArray(body.labels) ? body.labels.filter((label): label is string => typeof label === 'string') : [];
      const labels: Label[] = [];
      for (const name of names) {
        const label = findLabel(repo, name);
        if (label === undefined) {
          sendError(res, 422, 'Validation Failed: label does not exist');
          return true;
        }
        labels.push(label);
      }
      const when = stamp(repo);
      const issue: Issue = {
        id: 100000 + repo.nextIssue * 7,
        number: repo.nextIssue,
        title,
        body: str(body.body),
        state: 'open',
        labels,
        createdAt: when,
        updatedAt: when,
        closedAt: null,
        user: USER,
      };
      repo.nextIssue += 1;
      repo.issues.push(issue);
      repo.updatedAt = when;
      sendJson(res, 201, issueJson(repo, issue));
      return true;
    }
    return false;
  }

  const number = Number.parseInt(rest[0], 10);
  const issue = repo.issues.find((candidate) => candidate.number === number);
  if (rest.length === 1) {
    if (issue === undefined) {
      sendError(res, 404, 'Not Found');
      return true;
    }
    if (method === 'GET') {
      sendJson(res, 200, issueJson(repo, issue));
      return true;
    }
    if (method === 'PATCH') {
      const body = parseJsonBody(request.body);
      if (body === null) {
        sendError(res, 400, 'Problems parsing JSON');
        return true;
      }
      if (typeof body.title === 'string') issue.title = body.title;
      if (typeof body.body === 'string') issue.body = body.body;
      if (body.state === 'open' || body.state === 'closed') {
        issue.state = body.state;
        issue.closedAt = body.state === 'closed' ? stamp(repo) : null;
      }
      if (Array.isArray(body.labels)) {
        const wanted: Label[] = [];
        for (const name of body.labels) {
          if (typeof name !== 'string') continue;
          const label = findLabel(repo, name);
          if (label === undefined) {
            sendError(res, 422, 'Validation Failed: label does not exist');
            return true;
          }
          wanted.push(label);
        }
        issue.labels = wanted;
      }
      issue.updatedAt = stamp(repo);
      sendJson(res, 200, issueJson(repo, issue));
      return true;
    }
    return false;
  }

  if (rest[1] === 'comments') {
    if (issue === undefined) {
      sendError(res, 404, 'Not Found');
      return true;
    }
    if (rest.length === 2 && method === 'GET') {
      sendJson(res, 200, repo.comments.filter((comment) => comment.issueNumber === issue.number).map(commentJson));
      return true;
    }
    if (rest.length === 2 && method === 'POST') {
      const body = parseJsonBody(request.body);
      if (body === null) {
        sendError(res, 400, 'Problems parsing JSON');
        return true;
      }
      const when = stamp(repo);
      const comment: IssueComment = {
        id: repo.nextComment,
        issueNumber: issue.number,
        body: str(body.body),
        createdAt: when,
        user: USER,
      };
      repo.nextComment += 1;
      repo.comments.push(comment);
      sendJson(res, 201, commentJson(comment));
      return true;
    }
    return false;
  }
  return false;
}

// ---------------------------------------------------------------------------
// pull requests

function findPull(repo: Repo, number: number): Pull | undefined {
  return repo.pulls.find((pull) => pull.number === number);
}

function headBranch(ref: string): string {
  const colon = ref.indexOf(':');
  return colon < 0 ? ref : ref.slice(colon + 1);
}

function handlePulls(request: RestRequest, repo: Repo, rest: string[]): boolean {
  const { method, res, query } = request;
  if (rest.length === 0) {
    if (method === 'GET') {
      const state = query.get('state') ?? 'open';
      let items = repo.pulls;
      if (state === 'open') items = items.filter((pull) => pull.state === 'open');
      else if (state === 'closed') items = items.filter((pull) => pull.state === 'closed');
      const { slice } = pageItems(items, query);
      sendJson(res, 200, slice.map((pull) => pullJson(repo, pull)));
      return true;
    }
    if (method === 'POST') {
      const body = parseJsonBody(request.body);
      if (body === null) {
        sendError(res, 400, 'Problems parsing JSON');
        return true;
      }
      const head = headBranch(str(body.head));
      const base = str(body.base) === '' ? repo.defaultBranch : str(body.base);
      const headSha = repo.refs.get(branchRef(head));
      const baseSha = repo.refs.get(branchRef(base));
      if (headSha === undefined || baseSha === undefined) {
        sendError(res, 422, 'Validation Failed: head or base branch does not exist');
        return true;
      }
      if (head === base) {
        sendError(res, 422, 'Validation Failed: head and base are the same branch');
        return true;
      }
      const when = stamp(repo);
      const pull: Pull = {
        id: 200000 + repo.nextPull * 3,
        number: repo.nextPull,
        title: str(body.title),
        body: str(body.body),
        state: 'open',
        headRef: head,
        baseRef: base,
        headSha,
        baseSha,
        mergeCommitSha: null,
        merged: false,
        mergedAt: null,
        createdAt: when,
        updatedAt: when,
        user: USER,
      };
      repo.nextPull += 1;
      repo.pulls.push(pull);
      sendJson(res, 201, pullJson(repo, pull));
      return true;
    }
    return false;
  }

  const number = Number.parseInt(rest[0], 10);
  const pull = findPull(repo, number);
  if (pull === undefined) {
    sendError(res, 404, 'Not Found');
    return true;
  }
  if (rest.length === 1) {
    if (method === 'GET') {
      sendJson(res, 200, pullJson(repo, pull));
      return true;
    }
    if (method === 'PATCH') {
      const body = parseJsonBody(request.body);
      if (body === null) {
        sendError(res, 400, 'Problems parsing JSON');
        return true;
      }
      if (typeof body.title === 'string') pull.title = body.title;
      if (typeof body.body === 'string') pull.body = body.body;
      if (body.state === 'open' || body.state === 'closed') pull.state = body.state;
      pull.updatedAt = stamp(repo);
      sendJson(res, 200, pullJson(repo, pull));
      return true;
    }
    return false;
  }
  if (rest[1] === 'files' && method === 'GET') {
    const headSha = repo.refs.get(branchRef(pull.headRef)) ?? pull.headSha;
    const files = diffFiles(repo, peelShallow(repo.objects, pull.baseSha), peelShallow(repo.objects, headSha));
    sendJson(res, 200, files);
    return true;
  }
  if (rest[1] === 'merge' && method === 'PUT') {
    return mergePull(request, repo, pull);
  }
  return false;
}

function mergePull(request: RestRequest, repo: Repo, pull: Pull): boolean {
  const { res } = request;
  if (pull.merged) {
    sendError(res, 405, 'Pull request is not mergeable');
    return true;
  }
  const baseTip = repo.refs.get(branchRef(pull.baseRef)) ?? null;
  if (baseTip === null) {
    sendError(res, 409, 'Base branch is gone');
    return true;
  }
  const headTip = repo.refs.get(branchRef(pull.headRef)) ?? pull.headSha;
  const baseCommit = peelShallow(repo.objects, pull.baseSha);
  const headCommit = peelShallow(repo.objects, headTip);
  if (baseTip !== peelShallow(repo.objects, pull.baseSha)) {
    // The base moved after the pull was opened: refuse when both sides touched the same paths.
    const pullPaths = new Set(diffFiles(repo, baseCommit, headCommit).map((file) => file.filename));
    const basePaths = diffFiles(repo, baseCommit, peelShallow(repo.objects, baseTip)).map((file) => file.filename);
    if (basePaths.some((path) => pullPaths.has(path))) {
      sendError(res, 405, 'Pull Request is not mergeable');
      return true;
    }
  }
  const changed = diffFiles(repo, baseCommit, headCommit);
  const changes = changed.map((file) => ({ path: file.filename, content: file.status === 'removed' ? null : blobData(repo, filesAt(repo, headCommit).get(file.filename) ?? '') }));
  const message = pull.title === '' ? `Merge pull request #${pull.number}` : `${pull.title}\n\n${pull.body}`.trim();
  const result = commitChanges(repo, pull.baseRef, changes, message);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = stamp(repo);
  pull.mergeCommitSha = result.commit;
  pull.updatedAt = pull.mergedAt;
  pull.headSha = headTip;
  sendJson(res, 200, { sha: result.commit, merged: true, message: 'Pull request successfully merged' });
  return true;
}
