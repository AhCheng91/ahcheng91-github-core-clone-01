// The forge's operations that every surface shares: the JSON views GitHub answers with, issue,
// label, pull-request and merge logic, and the file diff. REST, GraphQL and MCP all call these,
// so a repository or issue made over one surface reads back over another.

import {
  type Issue,
  type IssueComment,
  type Label,
  type Pull,
  type Repo,
  VIEWER,
  blobData,
  branchRef,
  commitChanges,
  filesAt,
  stamp,
} from './store.ts';
import { parseCommit, peelShallow } from './gitobj.ts';

export const INSTALLED_VERSION = 'makefun-gh-forge/1.0';

export const USER = { login: VIEWER.login, id: VIEWER.id, type: 'User', site_admin: false };

export type Json = Record<string, unknown>;

export const issueState = (issue: Issue): string => issue.state;

export function labelJson(label: Label): Json {
  return { id: label.id, name: label.name, color: label.color, description: label.description, default: false };
}

export function issueJson(repo: Repo, issue: Issue): Json {
  return {
    id: issue.id,
    node_id: `I_${issue.id}`,
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

export function commentJson(comment: IssueComment): Json {
  return {
    id: comment.id,
    body: comment.body,
    user: comment.user,
    created_at: comment.createdAt,
    updated_at: comment.createdAt,
  };
}

export function pullJson(repo: Repo, pull: Pull): Json {
  return {
    id: pull.id,
    node_id: `PR_${pull.id}`,
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

export function repoJson(repo: Repo): Json {
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

export function branchJson(repo: Repo, name: string, sha: string): Json {
  return { name, commit: { sha, url: `${repo.owner}/${repo.name}/commits/${sha}` }, protected: false };
}

export function commitJson(repo: Repo, sha: string): Json {
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
// labels

export function findLabel(repo: Repo, name: string): Label | undefined {
  return repo.labels.find((label) => label.name === name);
}

export function addLabel(repo: Repo, name: string, color: string, description: string): Label {
  const label: Label = { id: repo.nextLabel, name, color: color === '' ? 'ededed' : color, description };
  repo.nextLabel += 1;
  repo.labels.push(label);
  return label;
}

// ---------------------------------------------------------------------------
// issues

export interface IssueInput {
  title: string;
  body?: string;
  labels?: readonly string[];
}

/** Create an issue. Returns the error message a surface should answer, or the issue. */
export function createIssueRecord(repo: Repo, input: IssueInput): { issue: Issue } | { error: string } {
  const title = input.title;
  if (title === '') return { error: 'Issue title is required' };
  const labels: Label[] = [];
  for (const name of input.labels ?? []) {
    const label = findLabel(repo, name);
    if (label === undefined) return { error: 'Validation Failed: label does not exist' };
    labels.push(label);
  }
  const when = stamp(repo);
  const issue: Issue = {
    id: 100000 + repo.nextIssue * 7,
    number: repo.nextIssue,
    title,
    body: input.body ?? '',
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
  return { issue };
}

export function addComment(repo: Repo, issue: Issue, body: string): IssueComment {
  const comment: IssueComment = { id: repo.nextComment, issueNumber: issue.number, body, createdAt: stamp(repo), user: USER };
  repo.nextComment += 1;
  repo.comments.push(comment);
  return comment;
}

export function closeIssue(repo: Repo, issue: Issue): void {
  issue.state = 'closed';
  issue.closedAt = stamp(repo);
  issue.updatedAt = issue.closedAt;
}

// ---------------------------------------------------------------------------
// diffs

function lines(data: Buffer | null): string[] {
  if (data === null) return [];
  const text = data.toString('utf8');
  if (text === '') return [];
  return text.split('\n');
}

/** Added and deleted line counts between two blobs, by counting unmatched lines. */
export function lineDelta(before: Buffer | null, after: Buffer | null): { additions: number; deletions: number } {
  const oldLines = lines(before);
  const newLines = lines(after);
  const counts = new Map<string, number>();
  for (const text of oldLines) counts.set(text, (counts.get(text) ?? 0) + 1);
  let matched = 0;
  for (const text of newLines) {
    const available = counts.get(text) ?? 0;
    if (available > 0) {
      counts.set(text, available - 1);
      matched += 1;
    }
  }
  return { additions: newLines.length - matched, deletions: oldLines.length - matched };
}

export interface FileDiff {
  filename: string;
  status: 'added' | 'removed' | 'modified';
  additions: number;
  deletions: number;
  changes: number;
}

/** The changed files between two commits, in path order. */
export function diffFiles(repo: Repo, baseSha: string | null, headSha: string | null): FileDiff[] {
  const before = filesAt(repo, baseSha);
  const after = filesAt(repo, headSha);
  const out: FileDiff[] = [];
  const paths = new Set<string>([...before.keys(), ...after.keys()]);
  for (const path of [...paths].sort()) {
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

// ---------------------------------------------------------------------------
// pull requests

export interface PullInput {
  title: string;
  body?: string;
  head: string;
  base?: string;
}

export function createPullRecord(repo: Repo, input: PullInput): { pull: Pull } | { error: string } {
  const head = input.head.includes(':') ? input.head.slice(input.head.indexOf(':') + 1) : input.head;
  const base = input.base === undefined || input.base === '' ? repo.defaultBranch : input.base;
  const headSha = repo.refs.get(branchRef(head));
  const baseSha = repo.refs.get(branchRef(base));
  if (headSha === undefined || baseSha === undefined) return { error: 'Validation Failed: head or base branch does not exist' };
  if (head === base) return { error: 'Validation Failed: head and base are the same branch' };
  const when = stamp(repo);
  const pull: Pull = {
    id: 200000 + repo.nextPull * 3,
    number: repo.nextPull,
    title: input.title,
    body: input.body ?? '',
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
  return { pull };
}

export function findPull(repo: Repo, number: number): Pull | undefined {
  return repo.pulls.find((pull) => pull.number === number);
}

/** Squash-merge a pull request onto its base branch. */
export function mergePullRecord(repo: Repo, pull: Pull): { sha: string } | { error: 'not-mergeable' | 'no-base' } {
  if (pull.merged) return { error: 'not-mergeable' };
  const baseTip = repo.refs.get(branchRef(pull.baseRef)) ?? null;
  if (baseTip === null) return { error: 'no-base' };
  const headTip = repo.refs.get(branchRef(pull.headRef)) ?? pull.headSha;
  const baseCommit = peelShallow(repo.objects, pull.baseSha);
  const headCommit = peelShallow(repo.objects, headTip);
  const currentBase = peelShallow(repo.objects, baseTip);
  if (currentBase !== baseCommit) {
    const pullPaths = new Set(diffFiles(repo, baseCommit, headCommit).map((file) => file.filename));
    const basePaths = diffFiles(repo, baseCommit, currentBase).map((file) => file.filename);
    if (basePaths.some((path) => pullPaths.has(path))) return { error: 'not-mergeable' };
  }
  const changed = diffFiles(repo, baseCommit, headCommit);
  const headFiles = filesAt(repo, headCommit);
  const changes = changed.map((file) => ({ path: file.filename, content: file.status === 'removed' ? null : (blobData(repo, headFiles.get(file.filename) ?? '') ?? Buffer.alloc(0)) }));
  const message = pull.title === '' ? `Merge pull request #${pull.number}` : `${pull.title}\n\n${pull.body}`.trim();
  const result = commitChanges(repo, pull.baseRef, changes, message);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = stamp(repo);
  pull.mergeCommitSha = result.commit;
  pull.updatedAt = pull.mergedAt;
  pull.headSha = headTip;
  return { sha: result.commit };
}
