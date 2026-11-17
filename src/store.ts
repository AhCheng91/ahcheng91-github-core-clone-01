// The forge's own store: users, repositories, their git objects and refs, issues, labels,
// comments and pull requests, all in memory. One process owns it for the whole trial.

import {
  type ObjectTable,
  type ObjType,
  encodeCommit,
  flattenTree,
  parseCommit,
  peelShallow,
  putObject,
  treeLookup,
  treeWith,
  writeTree,
} from './gitobj.ts';

/** The one user the trial's bearer token names (the suite's fixed scenario). */
export const VIEWER = {
  login: 'adaiversen',
  id: 1000,
  name: 'ada iversen',
  email: 'adaiversen@example.invalid',
} as const;

/** The git identity every commit and tag this forge writes carries. */
export const IDENTITY = 'Ada Iversen <adaiversen@example.invalid>';

/** Every generated timestamp counts seconds from here: 2025-01-01T00:00:00Z, git's epoch offset. */
const BASE_SECONDS = 1_735_689_600;

export interface Label {
  id: number;
  name: string;
  color: string;
  description: string;
}

export interface IssueComment {
  id: number;
  issueNumber: number;
  body: string;
  createdAt: string;
  user: { login: string; id: number };
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
  user: { login: string; id: number };
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  mergeCommitSha: string | null;
  merged: boolean;
  mergedAt: string | null;
  createdAt: string;
  updatedAt: string;
  user: { login: string; id: number };
}

export interface Repo {
  id: number;
  owner: string;
  name: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  createdAt: string;
  updatedAt: string;
  objects: ObjectTable;
  refs: Map<string, string>;
  labels: Label[];
  issues: Issue[];
  comments: IssueComment[];
  pulls: Pull[];
  nextIssue: number;
  nextPull: number;
  nextComment: number;
  nextLabel: number;
  tick: number;
}

export interface Store {
  repos: Map<string, Repo>;
  repoNames: string[];
  nextRepoId: number;
}

export function createStore(): Store {
  return { repos: new Map(), repoNames: [], nextRepoId: 1 };
}

export function repoKey(owner: string, name: string): string {
  return `${owner}/${name}`;
}

export function getRepo(store: Store, owner: string, name: string): Repo | undefined {
  return store.repos.get(repoKey(owner, name));
}

/** A deterministic ISO timestamp: the base plus this repository's tick, one second each. */
export function stamp(repo: Repo): string {
  repo.tick += 1;
  return new Date((BASE_SECONDS + repo.tick) * 1000).toISOString().replace('.000Z', 'Z');
}

export function createRepo(store: Store, owner: string, name: string, description: string, isPrivate: boolean): Repo {
  const repo: Repo = {
    id: 5000 + store.nextRepoId,
    owner,
    name,
    description,
    private: isPrivate,
    defaultBranch: 'main',
    createdAt: '',
    updatedAt: '',
    objects: new Map(),
    refs: new Map(),
    labels: [],
    issues: [],
    comments: [],
    pulls: [],
    nextIssue: 1,
    nextPull: 1,
    nextComment: 1,
    nextLabel: 1,
    tick: 0,
  };
  store.nextRepoId += 1;
  repo.createdAt = stamp(repo);
  repo.updatedAt = repo.createdAt;
  store.repos.set(repoKey(owner, name), repo);
  store.repoNames.push(repoKey(owner, name));
  return repo;
}

export function deleteRepo(store: Store, owner: string, name: string): boolean {
  const key = repoKey(owner, name);
  if (!store.repos.delete(key)) return false;
  const index = store.repoNames.indexOf(key);
  if (index >= 0) store.repoNames.splice(index, 1);
  return true;
}

/** The ref name of a branch. */
export function branchRef(branch: string): string {
  return `refs/heads/${branch}`;
}

export function refSha(repo: Repo, ref: string): string | null {
  return repo.refs.get(ref) ?? null;
}

/** The commit a ref or an object id names, peeled through tags; null when the repository is unborn. */
export function resolveCommit(repo: Repo, refOrSha: string | null): string | null {
  if (refOrSha === null || refOrSha === '') return refSha(repo, branchRef(repo.defaultBranch));
  if (refOrSha.startsWith('refs/')) return refSha(repo, refOrSha) ?? refSha(repo, branchRef(refOrSha.replace(/^refs\/heads\//, '')));
  if (/^[0-9a-f]{40}$/.test(refOrSha)) return peelShallow(repo.objects, refOrSha);
  return refSha(repo, branchRef(refOrSha));
}

/** The root tree of a commit, or null. */
export function commitTree(repo: Repo, commitSha: string): string | null {
  const obj = repo.objects.get(commitSha);
  if (obj === undefined || obj.type !== 'commit') return null;
  return parseCommit(obj.data).tree;
}

export interface FileChange {
  path: string;
  content: Buffer | null;
}

/**
 * Write `changes` into the tree of `branch` and commit it, with the branch tip (or nothing, for an
 * unborn branch) as the parent. Returns the new commit id and every blob id written.
 */
export function commitChanges(repo: Repo, branch: string, changes: readonly FileChange[], message: string): { commit: string; blobs: Map<string, string> } {
  const ref = branchRef(branch);
  const parent = repo.refs.get(ref) ?? null;
  const parentTree = parent === null ? null : commitTree(repo, parent);
  const blobs = new Map<string, string>();
  let tree = parentTree;
  for (const change of changes) {
    const parts = change.path.split('/').filter((part) => part !== '');
    let blobSha: string | null = null;
    if (change.content !== null) {
      blobSha = putObject(repo.objects, 'blob', change.content);
      blobs.set(change.path, blobSha);
    }
    tree = treeWith(repo.objects, tree, parts, blobSha);
  }
  const when = stamp(repo);
  const body = encodeCommit({
    tree: tree ?? writeTree(repo.objects, []),
    parents: parent === null ? [] : [parent],
    author: `${IDENTITY} ${Math.floor((BASE_SECONDS + repo.tick) / 1) } +0000`,
    committer: `${IDENTITY} ${Math.floor(BASE_SECONDS + repo.tick)} +0000`,
    message,
  });
  const commit = putObject(repo.objects, 'commit', body);
  repo.refs.set(ref, commit);
  repo.updatedAt = when;
  return { commit, blobs };
}

/** The blob id at a path on a ref, or null. */
export function blobAt(repo: Repo, refOrSha: string | null, path: string): string | null {
  const commit = resolveCommit(repo, refOrSha);
  if (commit === null) return null;
  const tree = commitTree(repo, commit);
  const parts = path.split('/').filter((part) => part !== '');
  const found = treeLookup(repo.objects, tree, parts);
  if (found === null) return null;
  const obj = repo.objects.get(found);
  if (obj === undefined || obj.type === 'blob') return found;
  return null;
}

/** Every file path of a commit with its blob id. */
export function filesAt(repo: Repo, commitSha: string | null): Map<string, string> {
  if (commitSha === null) return new Map();
  return flattenTree(repo.objects, commitTree(repo, commitSha));
}

/** Blob bytes, or null. */
export function blobData(repo: Repo, sha: string): Buffer | null {
  const obj = repo.objects.get(sha);
  return obj !== undefined && obj.type === 'blob' ? obj.data : null;
}

/** Store one object of whatever type (used by the smart-HTTP push path). */
export function storeObject(repo: Repo, type: ObjType, data: Buffer): string {
  return putObject(repo.objects, type, data);
}
