// GitHub GraphQL v4: POST /api/graphql. Parses the document the suite sends (gh and github-mcp-server
// speak githubv4), resolves it against the shared store through src/service.ts so REST reads back
// exactly what GraphQL wrote, and answers {data} or {data, errors}. One request body is one operation
// {query, variables} (an array answers operation batching). Nothing here depends on the clock: global
// ids are base64 of "<Type>:<repoId>[:<itemId>]" and cursors are base64 of "cursor:<index>".

import { sendError, sendJson } from './http.ts';
import type { Issue, IssueComment, Pull, Repo, Store } from './store.ts';
import { VIEWER, branchRef, getRepo } from './store.ts';
import {
  addComment,
  createIssueRecord,
  createPullRecord,
  closeIssue,
  diffFiles,
  mergePullRecord,
} from './service.ts';
import { peelShallow } from './gitobj.ts';

// ---------------------------------------------------------------------------
// A small GraphQL document reader: fields with aliases, arguments (strings,
// ints, enums, lists, object literals and $variables), selection sets, inline
// fragments and comments. It covers what the suite sends.
// ---------------------------------------------------------------------------

type VNode =
  | { t: 'var'; name: string }
  | { t: 'str'; value: string }
  | { t: 'int'; value: number }
  | { t: 'float'; value: number }
  | { t: 'bool'; value: boolean }
  | { t: 'null' }
  | { t: 'name'; value: string }
  | { t: 'list'; items: VNode[] }
  | { t: 'obj'; fields: ArgNode[] };

interface ArgNode {
  name: string;
  value: VNode;
}

interface Sel {
  kind: 'field' | 'frag';
  alias?: string;
  name?: string;
  args?: ArgNode[];
  selections: Sel[];
  fragType?: string;
}

interface Op {
  opType: 'query' | 'mutation';
  selections: Sel[];
}

class Parser {
  private pos = 0;
  private readonly src: string;

  constructor(src: string) {
    this.src = src;
  }

  private skip(): void {
    const s = this.src;
    const n = s.length;
    for (;;) {
      let p = this.pos;
      while (p < n) {
        const c = s.charCodeAt(p);
        if (c === 32 || c === 9 || c === 10 || c === 13 || c === 44 || c === 0xfeff) {
          p += 1;
          continue;
        }
        break;
      }
      if (p < n && s[p] === '#') {
        while (p < n && s[p] !== '\n') p += 1;
        continue;
      }
      this.pos = p;
      return;
    }
  }

  private peek(): string {
    this.skip();
    return this.src[this.pos] ?? '';
  }

  private at(punct: string): boolean {
    this.skip();
    return this.src.startsWith(punct, this.pos);
  }

  private accept(punct: string): boolean {
    this.skip();
    if (this.src.startsWith(punct, this.pos)) {
      this.pos += punct.length;
      return true;
    }
    return false;
  }

  private expect(punct: string): void {
    this.skip();
    if (this.src.startsWith(punct, this.pos)) {
      this.pos += punct.length;
      return;
    }
    throw new Error(`GraphQL parse error: expected ${punct}`);
  }

  private name(): string {
    this.skip();
    const m = /^[A-Za-z_][A-Za-z0-9_]*/.exec(this.src.slice(this.pos));
    if (m === null) throw new Error('GraphQL parse error: expected a name');
    this.pos += m[0].length;
    return m[0];
  }

  parseDocument(): Op[] {
    const ops: Op[] = [];
    for (;;) {
      this.skip();
      if (this.pos >= this.src.length) break;
      if (this.at('fragment')) {
        this.maybeReadFragmentDefinition();
        continue;
      }
      ops.push(this.readOperation());
    }
    return ops;
  }

  private maybeReadFragmentDefinition(): void {
    // `fragment F on Type { ... }` — parse and drop the body; the suite never needs the definition.
    this.name(); // 'fragment'
    this.name(); // fragment name
    this.expect('on');
    this.name(); // type condition
    this.readDirectives();
    this.readSelectionSet();
  }

  private readOperation(): Op {
    this.skip();
    let opType: 'query' | 'mutation' = 'query';
    const first = this.peek();
    if (first !== '{') {
      const word = this.name();
      if (word === 'query' || word === 'mutation' || word === 'subscription') {
        opType = word === 'mutation' || word === 'subscription' ? 'mutation' : 'query';
        // optional operation name
        if (!this.at('(') && !this.at('{')) void this.name();
      }
    } else {
      return { opType: 'query', selections: this.readSelectionSet() };
    }
    if (this.at('(')) this.readVariableDefs();
    this.readDirectives();
    return { opType, selections: this.readSelectionSet() };
  }

  private readVariableDefs(): void {
    this.expect('(');
    for (;;) {
      this.skip();
      if (this.accept(')')) return;
      this.expect('$');
      this.name();
      this.expect(':');
      this.readType();
      if (this.accept('=')) void this.readValue();
      if (this.accept('&')) this.readType();
    }
  }

  private readType(): void {
    this.skip();
    if (this.accept('[')) {
      this.readType();
      this.expect(']');
    } else {
      this.name();
    }
    this.accept('!');
  }

  private readDirectives(): void {
    for (;;) {
      this.skip();
      if (!this.accept('@')) return;
      this.name();
      if (this.at('(')) this.readArgs();
    }
  }

  private readSelectionSet(): Sel[] {
    this.expect('{');
    const out: Sel[] = [];
    for (;;) {
      this.skip();
      if (this.accept('}')) return out;
      if (this.accept('@')) {
        this.name();
        if (this.at('(')) this.readArgs();
        continue;
      }
      if (this.at('...')) {
        this.pos += 3;
        this.skip();
        if (this.at('on')) {
          this.name(); // 'on'
          const type = this.name();
          this.readDirectives();
          out.push({ kind: 'frag', fragType: type, selections: this.readSelectionSet() });
        } else if (this.at('{')) {
          out.push({ kind: 'frag', selections: this.readSelectionSet() });
        } else {
          // a fragment spread: consume its name and directives.
          this.name();
          this.readDirectives();
        }
        continue;
      }
      out.push(this.readField());
    }
  }

  private readField(): Sel {
    const first = this.name();
    let alias: string | undefined;
    let name = first;
    if (this.accept(':')) {
      alias = first;
      name = this.name();
    }
    let args: ArgNode[] | undefined;
    if (this.at('(')) args = this.readArgs();
    if (this.accept('@')) {
      this.name();
      if (this.at('(')) this.readArgs();
    }
    let selections: Sel[] | undefined;
    if (this.at('{')) selections = this.readSelectionSet();
    return { kind: 'field', alias, name, args, selections: selections ?? [] };
  }

  private readArgs(): ArgNode[] {
    this.expect('(');
    const out: ArgNode[] = [];
    for (;;) {
      this.skip();
      if (this.accept(')')) return out;
      const name = this.name();
      this.expect(':');
      out.push({ name, value: this.readValue() });
    }
  }

  private readValue(): VNode {
    this.skip();
    const c = this.src[this.pos] ?? '';
    if (c === '$') {
      this.pos += 1;
      return { t: 'var', name: this.name() };
    }
    if (c === '[') {
      this.pos += 1;
      const items: VNode[] = [];
      for (;;) {
        this.skip();
        if (this.accept(']')) return { t: 'list', items };
        items.push(this.readValue());
      }
    }
    if (c === '{') {
      this.pos += 1;
      const fields: ArgNode[] = [];
      for (;;) {
        this.skip();
        if (this.accept('}')) return { t: 'obj', fields };
        const name = this.name();
        this.expect(':');
        fields.push({ name, value: this.readValue() });
      }
    }
    if (c === '"') return { t: 'str', value: this.readString() };
    if (c === '-' || (c >= '0' && c <= '9')) return this.readNumber();
    const word = this.name();
    if (word === 'true') return { t: 'bool', value: true };
    if (word === 'false') return { t: 'bool', value: false };
    if (word === 'null') return { t: 'null' };
    return { t: 'name', value: word };
  }

  private readString(): string {
    this.expect('"');
    let out = '';
    for (;;) {
      if (this.pos >= this.src.length || this.src[this.pos] === '"') {
        this.pos += 1;
        return out;
      }
      const c = this.src[this.pos];
      if (c === '\\') {
        const e = this.src[this.pos + 1] ?? '';
        if (e === 'n') out += '\n';
        else if (e === 't') out += '\t';
        else if (e === 'r') out += '\r';
        else if (e === '"') out += '"';
        else if (e === '\\') out += '\\';
        else if (e === '/') out += '/';
        else if (e === 'u') {
          out += this.src.slice(this.pos + 2, this.pos + 6);
          this.pos += 4;
        } else out += e;
        this.pos += 2;
        continue;
      }
      out += c;
      this.pos += 1;
    }
  }

  private readNumber(): VNode {
    this.skip();
    const m = /^(-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?)/.exec(this.src.slice(this.pos));
    if (m === null) throw new Error('GraphQL parse error: expected a number');
    this.pos += m[0].length;
    if (m[0].includes('.') || m[0].includes('e') || m[0].includes('E')) return { t: 'float', value: Number(m[0]) };
    return { t: 'int', value: Number(m[0]) };
  }
}

/** The runtime value of an argument node, with $variables substituted. */
function valueOf(node: VNode, vars: Record<string, unknown>): unknown {
  switch (node.t) {
    case 'var': {
      const v = vars[node.name];
      return v === undefined ? null : v;
    }
    case 'str':
    case 'int':
    case 'float':
    case 'bool':
    case 'name':
      return node.value;
    case 'null':
      return null;
    case 'list':
      return node.items.map((item) => valueOf(item, vars));
    case 'obj': {
      const o: Record<string, unknown> = {};
      for (const field of node.fields) o[field.name] = valueOf(field.value, vars);
      return o;
    }
  }
}

// ---------------------------------------------------------------------------
// Global ids and cursors
// ---------------------------------------------------------------------------

function repoGlobalId(id: number): string {
  return Buffer.from(`Repository:${id}`).toString('base64');
}

function issueGlobalId(repoId: number, issueId: number): string {
  return Buffer.from(`Issue:${repoId}:${issueId}`).toString('base64');
}

function pullGlobalId(repoId: number, pullId: number): string {
  return Buffer.from(`PullRequest:${repoId}:${pullId}`).toString('base64');
}

function commentGlobalId(repoId: number, commentId: number): string {
  return Buffer.from(`IssueComment:${repoId}:${commentId}`).toString('base64');
}

function decodeRootId(id: unknown, prefix: string): number | null {
  if (typeof id !== 'string' || id === '') return null;
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    if (!text.startsWith(`${prefix}:`)) return null;
    const first = text.slice(prefix.length + 1).split(':')[0];
    const n = Number(first);
    return Number.isInteger(n) && n > 0 ? n : null;
  } catch {
    return null;
  }
}

function decodeItemId(id: unknown): { repoId: number; itemId: number } | null {
  if (typeof id !== 'string' || id === '') return null;
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const m = /^Issue:(\d+):(\d+)$/.exec(text) ?? /^PullRequest:(\d+):(\d+)$/.exec(text);
    if (m === null) return null;
    return { repoId: Number(m[1]), itemId: Number(m[2]) };
  } catch {
    return null;
  }
}

function encodeCursor(index: number): string {
  return Buffer.from(`cursor:${index}`).toString('base64');
}

function decodeCursor(cursor: string): number | null {
  try {
    const text = Buffer.from(cursor, 'base64').toString('utf8');
    const m = /^cursor:(\d+)$/.exec(text);
    return m === null ? null : Number(m[1]);
  } catch {
    return null;
  }
}

function repoById(store: Store, id: number): Repo | undefined {
  for (const repo of store.repos.values()) {
    if (repo.id === id) return repo;
  }
  return undefined;
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

interface Ctx {
  store: Store;
  vars: Record<string, unknown>;
  errors: { message: string }[];
}

type StepKind =
  | 'scalar'
  | 'query'
  | 'viewer'
  | 'repository'
  | 'issue'
  | 'issueConn'
  | 'pageInfo'
  | 'user'
  | 'ref'
  | 'pull'
  | 'pullConn'
  | 'file'
  | 'filesConn'
  | 'node'
  | 'gqlType'
  | 'named'
  | 'createIssuePayload'
  | 'closeIssuePayload'
  | 'createPullPayload'
  | 'mergePullPayload'
  | 'addCommentPayload'
  | 'commentEdge'
  | 'comment'
  | 'list';

interface Step {
  kind: StepKind;
  typeName: string;
  value: unknown;
}

function scalarString(value: string): Step {
  return { kind: 'scalar', typeName: 'String', value };
}

function scalarNumber(value: number): Step {
  return { kind: 'scalar', typeName: 'Int', value };
}

function scalarBool(value: boolean): Step {
  return { kind: 'scalar', typeName: 'Boolean', value };
}

function errorStep(typeName: string, kind: StepKind): Step {
  return { kind, typeName, value: null };
}

// The introspection surface gh probes for, plus whatever a realistic schema holds.
interface TypeDef {
  kind: 'OBJECT' | 'ENUM' | 'INTERFACE' | 'SCALAR';
  fields: string[];
  enumValues: string[];
}

const TYPES: Record<string, TypeDef> = {
  Issue: {
    kind: 'OBJECT',
    fields: ['id', 'number', 'title', 'state', 'body'],
    enumValues: [],
  },
  PullRequest: {
    kind: 'OBJECT',
    fields: ['id', 'number', 'headRefName', 'baseRefName', 'mergeable', 'state', 'merged', 'files'],
    enumValues: [],
  },
  Repository: {
    kind: 'OBJECT',
    fields: [
      'id',
      'databaseId',
      'name',
      'nameWithOwner',
      'owner',
      'defaultBranchRef',
      'viewerPermission',
      'hasIssuesEnabled',
      'issue',
      'issues',
      'pullRequests',
      'pullRequest',
    ],
    enumValues: [],
  },
  SearchType: { kind: 'ENUM', fields: [], enumValues: ['ISSUE', 'REPOSITORY', 'USER', 'COMMIT', 'CODE'] },
  User: { kind: 'OBJECT', fields: ['login', 'id'], enumValues: [] },
  Viewer: { kind: 'OBJECT', fields: ['login'], enumValues: [] },
  Node: { kind: 'INTERFACE', fields: ['id'], enumValues: [] },
};

interface IssueConnValue {
  repo: Repo;
  nodes: Issue[];
  hasNext: boolean;
  endCursor: string | null;
}

function issueOrderDesc(a: Issue, b: Issue, field: string, commentCounts: Map<number, number>): number {
  let c = 0;
  if (field === 'UPDATED_AT') c = a.updatedAt < b.updatedAt ? 1 : a.updatedAt > b.updatedAt ? -1 : 0;
  else if (field === 'COMMENTS') c = (commentCounts.get(b.number) ?? 0) - (commentCounts.get(a.number) ?? 0);
  else c = a.createdAt < b.createdAt ? 1 : a.createdAt > b.createdAt ? -1 : 0;
  if (c === 0) c = b.number - a.number;
  return c;
}

function issuesConnection(repo: Repo, args: Record<string, unknown>): Step {
  let nodes = repo.issues;
  const states = args.states;
  if (states !== null && states !== undefined) {
    const wanted = (Array.isArray(states) ? states : [states]).map(String);
    if (wanted.length > 0) nodes = nodes.filter((issue) => wanted.includes(issue.state === 'open' ? 'OPEN' : 'CLOSED'));
  }
  const orderBy = args.orderBy !== null && typeof args.orderBy === 'object' ? (args.orderBy as Record<string, unknown>) : null;
  const field = orderBy !== null && typeof orderBy.field === 'string' ? orderBy.field : 'CREATED_AT';
  const direction = orderBy !== null && typeof orderBy.direction === 'string' ? orderBy.direction : 'DESC';
  const commentCounts = new Map<number, number>();
  for (const comment of repo.comments) {
    commentCounts.set(comment.issueNumber, (commentCounts.get(comment.issueNumber) ?? 0) + 1);
  }
  const sign = direction === 'ASC' ? -1 : 1;
  const sorted = nodes.slice().sort((a, b) => sign * issueOrderDesc(a, b, field, commentCounts));
  let start = 0;
  const after = args.after;
  if (typeof after === 'string' && after !== '') {
    const index = decodeCursor(after);
    if (index !== null) start = Math.min(index + 1, sorted.length);
  }
  const first = typeof args.first === 'number' && args.first > 0 ? args.first : sorted.length;
  const slice = sorted.slice(start, start + first);
  const hasNext = start + first < sorted.length;
  const endCursor = slice.length > 0 ? encodeCursor(start + slice.length - 1) : null;
  return { kind: 'issueConn', typeName: 'IssueConnection', value: { repo, nodes: slice, hasNext, endCursor } satisfies IssueConnValue };
}

function pullFilesField(repo: Repo, pull: Pull, args: Record<string, unknown>): Step {
  const base = peelShallow(repo.objects, pull.baseSha);
  const headTip = repo.refs.get(branchRef(pull.headRef)) ?? pull.headSha;
  const diff = diffFiles(repo, base, peelShallow(repo.objects, headTip));
  const first = typeof args.first === 'number' && args.first > 0 ? args.first : diff.length;
  const files = diff.slice(0, first).map((file) => ({
    kind: 'file' as const,
    typeName: 'PullRequestChangedFile',
    value: { path: file.filename, additions: file.additions, deletions: file.deletions },
  }));
  return { kind: 'filesConn', typeName: 'PullRequestChangedFileConnection', value: files };
}

function fieldOf(step: Step, name: string, args: Record<string, unknown>, ctx: Ctx): Step | null {
  switch (step.kind) {
    case 'query': {
      if (name === 'viewer') return { kind: 'viewer', typeName: 'Viewer', value: VIEWER.login };
      if (name === 'repository') {
        const owner = typeof args.owner === 'string' ? args.owner : '';
        const repoName = typeof args.name === 'string' ? args.name : '';
        return { kind: 'repository', typeName: 'Repository', value: getRepo(ctx.store, owner, repoName) ?? null };
      }
      if (name === 'node') {
        const id = typeof args.id === 'string' ? args.id : '';
        const repoId = decodeRootId(id, 'Repository');
        const repo = repoId === null ? undefined : repoById(ctx.store, repoId);
        return { kind: 'node', typeName: 'Node', value: { nodeId: id, repo: repo ?? null } };
      }
      if (name === '__type') {
        const typeName = typeof args.name === 'string' && args.name !== '' ? args.name : null;
        return { kind: 'gqlType', typeName: '__Type', value: typeName };
      }
      if (name === 'createIssue') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const repoId = decodeRootId(input.repositoryId, 'Repository');
        const repo = repoId === null ? undefined : repoById(ctx.store, repoId);
        if (repo === undefined) {
          ctx.errors.push({ message: 'Could not resolve to a Repository with the provided global ID.' });
          return errorStep('CreateIssuePayload', 'createIssuePayload');
        }
        const made = createIssueRecord(repo, {
          title: typeof input.title === 'string' ? input.title : '',
          body: typeof input.body === 'string' ? input.body : '',
        });
        if ('error' in made) {
          ctx.errors.push({ message: made.error });
          return errorStep('CreateIssuePayload', 'createIssuePayload');
        }
        return { kind: 'createIssuePayload', typeName: 'CreateIssuePayload', value: { repo, issue: made.issue } };
      }
      if (name === 'closeIssue') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const found = itemFromId(ctx.store, input.issueId);
        if (found === null) {
          ctx.errors.push({ message: 'Could not resolve to a Issue with the provided global ID.' });
          return errorStep('CloseIssuePayload', 'closeIssuePayload');
        }
        if (found.type !== 'issue') {
          ctx.errors.push({ message: 'Could not resolve to a Issue with the provided global ID.' });
          return errorStep('CloseIssuePayload', 'closeIssuePayload');
        }
        closeIssue(found.repo, found.item as Issue);
        return { kind: 'closeIssuePayload', typeName: 'CloseIssuePayload', value: { repo: found.repo, issue: found.item as Issue } };
      }
      if (name === 'addComment') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const found = itemFromId(ctx.store, input.subjectId);
        if (found === null || found.type !== 'issue') {
          ctx.errors.push({ message: 'Could not resolve to a Issue with the provided global ID.' });
          return errorStep('AddCommentPayload', 'addCommentPayload');
        }
        const comment = addComment(found.repo, found.item as Issue, typeof input.body === 'string' ? input.body : '');
        return { kind: 'addCommentPayload', typeName: 'AddCommentPayload', value: { repo: found.repo, comment } };
      }
      if (name === 'createPullRequest') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const repoId = decodeRootId(input.repositoryId, 'Repository');
        const repo = repoId === null ? undefined : repoById(ctx.store, repoId);
        if (repo === undefined) {
          ctx.errors.push({ message: 'Could not resolve to a Repository with the provided global ID.' });
          return errorStep('CreatePullRequestPayload', 'createPullPayload');
        }
        const made = createPullRecord(repo, {
          title: typeof input.title === 'string' ? input.title : '',
          head: typeof input.headRefName === 'string' ? input.headRefName : '',
          base: typeof input.baseRefName === 'string' ? input.baseRefName : '',
        });
        if ('error' in made) {
          ctx.errors.push({ message: made.error });
          return errorStep('CreatePullRequestPayload', 'createPullPayload');
        }
        return { kind: 'createPullPayload', typeName: 'CreatePullRequestPayload', value: { repo, pull: made.pull } };
      }
      if (name === 'mergePullRequest') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const found = itemFromId(ctx.store, input.pullRequestId);
        if (found === null || found.type !== 'pull') {
          ctx.errors.push({ message: 'Could not resolve to a PullRequest with the provided global ID.' });
          return errorStep('MergePullRequestPayload', 'mergePullPayload');
        }
        const result = mergePullRecord(found.repo, found.item as Pull);
        if ('error' in result) {
          ctx.errors.push({ message: 'Pull request is not mergeable' });
          return errorStep('MergePullRequestPayload', 'mergePullPayload');
        }
        return { kind: 'mergePullPayload', typeName: 'MergePullRequestPayload', value: { repo: found.repo, pull: found.item as Pull } };
      }
      return null;
    }
    case 'viewer':
      if (name === 'login') return scalarString(VIEWER.login);
      return null;
    case 'user': {
      const value = step.value as { login: string };
      if (name === 'login') return scalarString(value.login);
      if (name === 'id') return scalarNumber(1000);
      return null;
    }
    case 'repository': {
      const repo = step.value as Repo;
      if (name === 'id') return scalarString(repoGlobalId(repo.id));
      if (name === 'databaseId') return scalarNumber(repo.id);
      if (name === 'name') return scalarString(repo.name);
      if (name === 'nameWithOwner') return scalarString(`${repo.owner}/${repo.name}`);
      if (name === 'owner') return { kind: 'user', typeName: 'User', value: { login: repo.owner } };
      if (name === 'defaultBranchRef') return { kind: 'ref', typeName: 'Ref', value: { name: repo.defaultBranch } };
      if (name === 'viewerPermission') return scalarString('ADMIN');
      if (name === 'hasIssuesEnabled') return scalarBool(true);
      if (name === 'issue') {
        const number = typeof args.number === 'number' ? args.number : Number(args.number);
        const issue = repo.issues.find((candidate) => candidate.number === number);
        return issue === undefined
          ? { kind: 'issue', typeName: 'Issue', value: null }
          : { kind: 'issue', typeName: 'Issue', value: { repo, issue } };
      }
      if (name === 'issues') return issuesConnection(repo, args);
      if (name === 'pullRequests') {
        const first = typeof args.first === 'number' && args.first > 0 ? args.first : repo.pulls.length;
        return { kind: 'pullConn', typeName: 'PullRequestConnection', value: { repo, nodes: repo.pulls.slice(0, first) } };
      }
      if (name === 'pullRequest') {
        const number = typeof args.number === 'number' ? args.number : Number(args.number);
        const pull = repo.pulls.find((candidate) => candidate.number === number);
        return pull === undefined
          ? { kind: 'pull', typeName: 'PullRequest', value: null }
          : { kind: 'pull', typeName: 'PullRequest', value: { repo, pull } };
      }
      return null;
    }
    case 'issue': {
      const value = step.value as { repo: Repo; issue: Issue } | null;
      if (value === null) return null;
      const { repo, issue } = value;
      if (name === 'id') return scalarString(issueGlobalId(repo.id, issue.id));
      if (name === 'number') return scalarNumber(issue.number);
      if (name === 'title') return scalarString(issue.title);
      if (name === 'state') return scalarString(issue.state === 'open' ? 'OPEN' : 'CLOSED');
      if (name === 'body') return scalarString(issue.body);
      return null;
    }
    case 'issueConn': {
      const value = step.value as IssueConnValue;
      if (name === 'nodes') {
        return {
          kind: 'list',
          typeName: 'Issue',
          value: value.nodes.map((issue) => ({ kind: 'issue', typeName: 'Issue', value: { repo: value.repo, issue } })),
        };
      }
      if (name === 'pageInfo') return { kind: 'pageInfo', typeName: 'PageInfo', value: { hasNext: value.hasNext, endCursor: value.endCursor } };
      return null;
    }
    case 'pageInfo': {
      const value = step.value as { hasNext: boolean; endCursor: string | null };
      if (name === 'hasNextPage') return scalarBool(value.hasNext);
      if (name === 'endCursor') return value.endCursor === null ? { kind: 'scalar', typeName: 'String', value: null } : scalarString(value.endCursor);
      return null;
    }
    case 'ref': {
      const value = step.value as { name: string };
      if (name === 'name') return scalarString(value.name);
      if (name === 'id') return scalarString(Buffer.from(`Ref:${value.name}`).toString('base64'));
      return null;
    }
    case 'pullConn': {
      const value = step.value as { repo: Repo; nodes: Pull[] };
      if (name === 'nodes') {
        return {
          kind: 'list',
          typeName: 'PullRequest',
          value: value.nodes.map((pull) => ({ kind: 'pull', typeName: 'PullRequest', value: { repo: value.repo, pull } })),
        };
      }
      return null;
    }
    case 'pull': {
      const value = step.value as { repo: Repo; pull: Pull } | null;
      if (value === null) return null;
      const { repo, pull } = value;
      if (name === 'id') return scalarString(pullGlobalId(repo.id, pull.id));
      if (name === 'number') return scalarNumber(pull.number);
      if (name === 'state') return scalarString(pull.merged ? 'MERGED' : pull.state === 'open' ? 'OPEN' : 'CLOSED');
      if (name === 'merged') return scalarBool(pull.merged);
      if (name === 'headRefName') return scalarString(pull.headRef);
      if (name === 'baseRefName') return scalarString(pull.baseRef);
      if (name === 'mergeable') return scalarString('MERGEABLE');
      if (name === 'files') return pullFilesField(repo, pull, args);
      return null;
    }
    case 'filesConn': {
      const value = step.value as Step[];
      if (name === 'nodes') return { kind: 'list', typeName: 'PullRequestChangedFile', value };
      return null;
    }
    case 'file': {
      const value = step.value as { path: string; additions: number; deletions: number };
      if (name === 'path') return scalarString(value.path);
      if (name === 'additions') return scalarNumber(value.additions);
      if (name === 'deletions') return scalarNumber(value.deletions);
      return null;
    }
    case 'node': {
      const value = step.value as { nodeId: string; repo: Repo | null };
      if (name === 'id') return scalarString(value.nodeId);
      return null;
    }
    case 'gqlType': {
      const typeName = step.value as string;
      const typeDef = TYPES[typeName];
      if (name === 'name') return scalarString(typeName);
      if (name === 'kind') return scalarString(typeDef.kind);
      if (name === 'fields') {
        return {
          kind: 'list',
          typeName: '__Field',
          value: typeDef.fields.map((fieldName) => ({ kind: 'named', typeName: '__Field', value: { name: fieldName } })),
        };
      }
      if (name === 'enumValues') {
        return {
          kind: 'list',
          typeName: '__EnumValue',
          value: typeDef.enumValues.map((entry) => ({ kind: 'named', typeName: '__EnumValue', value: { name: entry } })),
        };
      }
      return null;
    }
    case 'named': {
      const value = step.value as { name: string };
      if (name === 'name') return scalarString(value.name);
      return null;
    }
    case 'createIssuePayload': {
      if (name === 'issue') {
        const value = step.value as { repo: Repo; issue: Issue } | null;
        return value === null
          ? { kind: 'issue', typeName: 'Issue', value: null }
          : { kind: 'issue', typeName: 'Issue', value: { repo: value.repo, issue: value.issue } };
      }
      return null;
    }
    case 'closeIssuePayload': {
      if (name === 'issue') {
        const value = step.value as { repo: Repo; issue: Issue } | null;
        return value === null
          ? { kind: 'issue', typeName: 'Issue', value: null }
          : { kind: 'issue', typeName: 'Issue', value: { repo: value.repo, issue: value.issue } };
      }
      return null;
    }
    case 'createPullPayload':
    case 'mergePullPayload': {
      if (name === 'pullRequest') {
        const value = step.value as { repo: Repo; pull: Pull } | null;
        return value === null
          ? { kind: 'pull', typeName: 'PullRequest', value: null }
          : { kind: 'pull', typeName: 'PullRequest', value: { repo: value.repo, pull: value.pull } };
      }
      return null;
    }
    case 'addCommentPayload': {
      if (name === 'commentEdge') {
        const value = step.value as { repo: Repo; comment: IssueComment } | null;
        return value === null
          ? { kind: 'commentEdge', typeName: 'IssueCommentEdge', value: null }
          : { kind: 'commentEdge', typeName: 'IssueCommentEdge', value };
      }
      return null;
    }
    case 'commentEdge': {
      const value = step.value as { repo: Repo; comment: IssueComment } | null;
      if (name === 'node') {
        return value === null
          ? { kind: 'comment', typeName: 'IssueComment', value: null }
          : { kind: 'comment', typeName: 'IssueComment', value: { id: commentGlobalId(value.repo.id, value.comment.id), body: value.comment.body } };
      }
      return null;
    }
    case 'comment': {
      const value = step.value as { id: string; body: string } | null;
      if (value === null) return null;
      if (name === 'id') return scalarString(value.id);
      if (name === 'body') return scalarString(value.body);
      return null;
    }
    default:
      return null;
  }
}

function itemFromId(store: Store, id: unknown): { repo: Repo; item: Issue | Pull; type: 'issue' | 'pull' } | null {
  const decoded = decodeItemId(id);
  if (decoded === null) return null;
  const repo = repoById(store, decoded.repoId);
  if (repo === undefined) return null;
  const issue = repo.issues.find((candidate) => candidate.id === decoded.itemId);
  if (issue !== undefined) return { repo, item: issue, type: 'issue' };
  const pull = repo.pulls.find((candidate) => candidate.id === decoded.itemId);
  if (pull !== undefined) return { repo, item: pull, type: 'pull' };
  return null;
}

function withFragment(step: Step, fragType: string): Step | null {
  if (fragType === step.typeName) return step;
  if (step.kind === 'node' && fragType === 'Repository') {
    const value = step.value as { nodeId: string; repo: Repo | null };
    if (value.repo === null) return null;
    return { kind: 'repository', typeName: 'Repository', value: value.repo };
  }
  return null;
}

function resolveOne(step: Step, selections: Sel[], ctx: Ctx): unknown {
  if (step.kind === 'scalar') return step.value;
  if (step.value === null || step.value === undefined) return null;
  if (step.kind === 'list') {
    const out: unknown[] = [];
    for (const item of step.value as Step[]) {
      out.push(resolveOne(item, selections, ctx));
    }
    return out;
  }
  const out: Record<string, unknown> = {};
  resolveStep(step, selections, out, ctx);
  return out;
}

function writeValue(out: Record<string, unknown>, key: string, value: Step, selections: Sel[], ctx: Ctx): void {
  out[key] = resolveOne(value, selections, ctx);
}

function resolveStep(step: Step, selections: Sel[], out: Record<string, unknown>, ctx: Ctx): void {
  for (const sel of selections) {
    if (sel.kind === 'frag') {
      if (sel.fragType === undefined) {
        resolveStep(step, sel.selections, out, ctx);
        continue;
      }
      const concrete = withFragment(step, sel.fragType);
      if (concrete === null) continue;
      resolveStep(concrete, sel.selections, out, ctx);
      continue;
    }
    const args: Record<string, unknown> = {};
    if (sel.args !== undefined) {
      for (const arg of sel.args) args[arg.name] = valueOf(arg.value, ctx.vars);
    }
    const fieldName = sel.name ?? '';
    const next = fieldOf(step, fieldName, args, ctx);
    if (next === null) {
      ctx.errors.push({ message: `Cannot query field "${fieldName}" on type "${step.typeName}".` });
      continue;
    }
    writeValue(out, sel.alias ?? fieldName, next, sel.selections, ctx);
  }
}

function execute(document: string, variables: Record<string, unknown>, store: Store): { data: Record<string, unknown> | null; errors: { message: string }[] } {
  let ops: Op[];
  try {
    ops = new Parser(document).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: `Cannot parse the operation document: ${err instanceof Error ? err.message : String(err)}` }] };
  }
  if (ops.length === 0) {
    return { data: null, errors: [{ message: 'Must provide an operation.' }] };
  }
  const ctx: Ctx = { store, vars: variables, errors: [] };
  const data: Record<string, unknown> = {};
  resolveStep({ kind: 'query', typeName: 'Query', value: store }, ops[0].selections, data, ctx);
  const envelope: { data: Record<string, unknown> | null; errors: { message: string }[] } = { data, errors: ctx.errors };
  return envelope;
}

// ---------------------------------------------------------------------------
// The HTTP entry point
// ---------------------------------------------------------------------------

export function handleGraphql(res: import('node:http').ServerResponse, store: Store, body: Buffer): boolean {
  let parsed: unknown;
  try {
    parsed = body.length === 0 ? null : JSON.parse(body.toString('utf8'));
  } catch {
    sendError(res, 400, 'Problems parsing JSON');
    return true;
  }
  if (Array.isArray(parsed)) {
    const responses: unknown[] = [];
    for (const entry of parsed) {
      if (entry === null || typeof entry !== 'object') {
        responses.push({ data: null, errors: [{ message: 'Each batched operation must be an object.' }] });
        continue;
      }
      const operation = entry as { query?: unknown; variables?: unknown };
      const query = typeof operation.query === 'string' ? operation.query : '';
      const variables = operation.variables !== null && typeof operation.variables === 'object' ? (operation.variables as Record<string, unknown>) : {};
      responses.push(execute(query, variables, store));
    }
    sendJson(res, 200, responses);
    return true;
  }
  if (parsed === null || typeof parsed !== 'object') {
    sendError(res, 400, 'Problems parsing JSON');
    return true;
  }
  const operation = parsed as { query?: unknown; variables?: unknown };
  const query = typeof operation.query === 'string' ? operation.query : '';
  const variables = operation.variables !== null && typeof operation.variables === 'object' ? (operation.variables as Record<string, unknown>) : {};
  sendJson(res, 200, execute(query, variables, store));
  return true;
}