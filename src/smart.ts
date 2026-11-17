// The git smart-HTTP surface: protocol v2 (capability advertisement, ls-refs, fetch) and protocol v0
// (the upload-pack advertisement and the receive-pack advertisement, plus push). The packfile writer
// and reader and the object helpers in gitobj.ts do all the heavy lifting; this file only speaks the
// pkt-line dialect on top of them.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { sendBuffer, sendError } from './http.ts';
import {
  type GitObj,
  PKT_DELIM,
  PKT_FLUSH,
  type ObjectTable,
  ZERO_OID,
  isAncestor,
  parseCommit,
  parseTag,
  parseTree,
  pktLine,
  readPack,
  reachable,
  writePack,
} from './gitobj.ts';
import { type Repo, type Store, getRepo } from './store.ts';

/** The agent string this forge offers where git lets a server name itself. */
const AGENT = 'agent=makefun-forge/1.0';

const UPLOAD_ADVERT = 'application/x-git-upload-pack-advertisement';
const UPLOAD_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_ADVERT = 'application/x-git-receive-pack-advertisement';
const RECEIVE_RESULT = 'application/x-git-receive-pack-result';

/** One decoded pkt-line from a request: a data frame carrying payload bytes, or a control frame. */
type Frame =
  | { kind: 'data'; payload: Buffer }
  | { kind: 'flush' }
  | { kind: 'delim' }
  | { kind: 'response-end' };

/**
 * Decode a buffer as a stream of pkt-lines. The scan stops as soon as the bytes no longer look like
 * framing (a push's packfile riding after the command list, or garbage); whatever was not consumed
 * comes back whole as `rest`.
 */
function splitPackets(buf: Buffer): { frames: Frame[]; rest: Buffer; ended: boolean } {
  const frames: Frame[] = [];
  let cursor = 0;
  while (cursor + 4 <= buf.length) {
    const size = Number.parseInt(buf.toString('latin1', cursor, cursor + 4), 16);
    if (Number.isNaN(size)) break;
    if (size === 0) {
      frames.push({ kind: 'flush' });
      cursor += 4;
      continue;
    }
    if (size === 1) {
      frames.push({ kind: 'delim' });
      cursor += 4;
      continue;
    }
    if (size === 2) {
      frames.push({ kind: 'response-end' });
      cursor += 4;
      continue;
    }
    if (cursor + size > buf.length) break;
    frames.push({ kind: 'data', payload: buf.subarray(cursor + 4, cursor + size) });
    cursor += size;
    if (size < 4) break;
  }
  return { frames, rest: buf.subarray(cursor), ended: cursor >= buf.length };
}

/** The text a data frame carries, its trailing newline removed; empty for control frames. */
function frameText(frame: Frame): string {
  return frame.kind === 'data' ? frame.payload.toString('utf8').replace(/\n$/, '') : '';
}

/** Emit `payload` as band-1 sideband pkt-lines (≤ `max` payload bytes each) and close with a flush. */
function sidebandPack(payload: Buffer, max = 65500): Buffer {
  const out: Buffer[] = [];
  for (let at = 0; at < payload.length; at += max) {
    const chunk = payload.subarray(at, Math.min(payload.length, at + max));
    out.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
  }
  out.push(PKT_FLUSH);
  return Buffer.concat(out);
}

/** The repository's refs ordered by name, each as a `{ name, sha }` record. */
function sortedRefs(repo: Repo): Array<{ name: string; sha: string }> {
  const records: Array<{ name: string; sha: string }> = [];
  for (const [name, sha] of repo.refs) records.push({ name, sha });
  records.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return records;
}

/** Follow a tag object (and nested tags) to the object it names; null when the id is not a tag. */
function unwindTag(repo: Repo, oid: string): string | null {
  const object = repo.objects.get(oid);
  if (object === undefined || object.type !== 'tag') return null;
  let target = parseTag(object.data).object;
  for (let hop = 0; hop < 8; hop += 1) {
    const next = repo.objects.get(target);
    if (next === undefined || next.type !== 'tag') return target;
    target = parseTag(next.data).object;
  }
  return target;
}

// ---------------------------------------------------------------------------
// the request router
// ---------------------------------------------------------------------------

export async function handleGit(
  req: IncomingMessage,
  res: ServerResponse,
  store: Store,
  owner: string,
  name: string,
  suffix: string,
  query: URLSearchParams,
  body: Buffer,
): Promise<void> {
  const repo = getRepo(store, owner, name);
  if (repo === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  const service = query.get('service');

  if (suffix === 'info/refs') {
    if (service === 'git-receive-pack') {
      sendBuffer(res, 200, receiveAdvert(repo), RECEIVE_ADVERT);
      return;
    }
    if (service === 'git-upload-pack') {
      const asksForV2 = String(req.headers['git-protocol'] ?? '').includes('version=2');
      sendBuffer(res, 200, asksForV2 ? uploadAdvertV2() : uploadAdvertV0(repo), UPLOAD_ADVERT);
      return;
    }
    sendError(res, 400, 'Unknown service');
    return;
  }

  if (suffix === 'git-upload-pack' && req.method === 'POST') {
    sendBuffer(res, 200, runUploadPack(repo, body), UPLOAD_RESULT);
    return;
  }

  if (suffix === 'git-receive-pack' && req.method === 'POST') {
    sendBuffer(res, 200, runReceivePack(repo, body), RECEIVE_RESULT);
    return;
  }

  sendError(res, 404, 'Not Found');
}

// ---------------------------------------------------------------------------
// advertisements
// ---------------------------------------------------------------------------

/** The capability list advertised to a protocol v2 client. */
function v2CapabilityList(): string[] {
  return ['version 2', 'agent=git/makefun-forge-1.0', 'ls-refs=unborn', 'fetch=shallow wait-for-done', 'server-option', 'object-format=sha1'];
}

/** The protocol v2 advertisement: the service banner, the capability lines, then a flush. */
function uploadAdvertV2(): Buffer {
  const out: Buffer[] = [pktLine('# service=git-upload-pack\n'), PKT_FLUSH];
  for (const capability of v2CapabilityList()) out.push(pktLine(`${capability}\n`));
  out.push(PKT_FLUSH);
  return Buffer.concat(out);
}

/** The protocol v0 upload-pack advertisement: every ref with the capability set on the first line. */
function uploadAdvertV0(repo: Repo): Buffer {
  const caps = ['multi_ack_detailed', 'side-band-64k', 'thin-pack', 'ofs-delta', 'shallow', 'no-progress', 'object-format=sha1', AGENT, 'symref=HEAD:refs/heads/main'];
  const out: Buffer[] = [pktLine('# service=git-upload-pack\n'), PKT_FLUSH];
  const refs = sortedRefs(repo);
  if (refs.length === 0) {
    out.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps.join(' ')}\n`));
  } else {
    const headOid = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? refs[0].sha;
    out.push(pktLine(`${headOid} HEAD\0${caps.join(' ')}\n`));
    for (const ref of refs) out.push(pktLine(`${ref.sha} ${ref.name}\n`));
  }
  out.push(PKT_FLUSH);
  return Buffer.concat(out);
}

/** The receive-pack advertisement: capabilities ride on the first ref, then plain ref lines. */
function receiveAdvert(repo: Repo): Buffer {
  const caps = ['report-status', 'side-band-64k', 'delete-refs', 'ofs-delta', 'quiet', 'object-format=sha1', AGENT];
  const out: Buffer[] = [pktLine('# service=git-receive-pack\n'), PKT_FLUSH];
  const refs = sortedRefs(repo);
  if (refs.length === 0) {
    out.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps.join(' ')}\n`));
  } else {
    refs.forEach((ref, index) => {
      const text = index === 0 ? `${ref.sha} ${ref.name}\0${caps.join(' ')}\n` : `${ref.sha} ${ref.name}\n`;
      out.push(pktLine(text));
    });
  }
  out.push(PKT_FLUSH);
  return Buffer.concat(out);
}

// ---------------------------------------------------------------------------
// upload-pack: ls-refs and fetch
// ---------------------------------------------------------------------------

/** Dispatch one POSTed upload-pack command. Anything unknown gets a bare flush. */
function runUploadPack(repo: Repo, body: Buffer): Buffer {
  const { frames } = splitPackets(body);
  const command = frameText(frames[0] ?? { kind: 'flush' });
  if (command === 'command=ls-refs') return listRefs(repo, frames);
  if (command === 'command=fetch') return doFetch(repo, frames);
  return PKT_FLUSH;
}

/** The v2 ls-refs reply: HEAD with its symref, then each matching ref, honouring peel/symrefs/unborn. */
function listRefs(repo: Repo, frames: Frame[]): Buffer {
  let argumentsOpen = false;
  let askPeel = false;
  let askSymrefs = false;
  let askUnborn = false;
  const prefixes: string[] = [];
  for (const frame of frames.slice(1)) {
    if (frame.kind === 'delim') {
      argumentsOpen = true;
      continue;
    }
    if (frame.kind !== 'data' || !argumentsOpen) continue;
    const text = frameText(frame);
    if (text === 'peel') askPeel = true;
    else if (text === 'symrefs') askSymrefs = true;
    else if (text === 'unborn') askUnborn = true;
    else if (text.startsWith('ref-prefix ')) prefixes.push(text.slice('ref-prefix '.length));
  }
  const matchesPrefix = (refName: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => refName.startsWith(prefix));
  const out: Buffer[] = [];
  const headOid = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
  if (matchesPrefix('HEAD')) {
    if (headOid !== null) {
      const suffix = askSymrefs ? ` symref-target:refs/heads/${repo.defaultBranch}` : '';
      out.push(pktLine(`${headOid} HEAD${suffix}\n`));
    } else if (askUnborn) {
      out.push(pktLine(`unborn HEAD symref-target:refs/heads/${repo.defaultBranch}\n`));
    }
  }
  for (const ref of sortedRefs(repo)) {
    if (!matchesPrefix(ref.name)) continue;
    let text = `${ref.sha} ${ref.name}`;
    if (askPeel) {
      const target = unwindTag(repo, ref.sha);
      if (target !== null) text += ` peeled:${target}`;
    }
    out.push(pktLine(`${text}\n`));
  }
  out.push(PKT_FLUSH);
  return Buffer.concat(out);
}

/** The named arguments a fetch command carries. */
interface FetchArguments {
  wants: string[];
  haves: string[];
  depth: number | null;
  shallow: string[];
  done: boolean;
}

/** Walk a fetch command's frames and pull out its arguments from after the delim. */
function readFetchArguments(frames: Frame[]): FetchArguments {
  const argumentsView: FetchArguments = { wants: [], haves: [], depth: null, shallow: [], done: false };
  let readingArguments = false;
  for (const frame of frames) {
    if (frame.kind === 'delim') {
      readingArguments = true;
      continue;
    }
    if (frame.kind !== 'data' || !readingArguments) continue;
    const text = frameText(frame);
    if (text.startsWith('want ')) argumentsView.wants.push(text.slice(5));
    else if (text.startsWith('have ')) argumentsView.haves.push(text.slice(5));
    else if (text.startsWith('shallow ')) argumentsView.shallow.push(text.slice(8));
    else if (text.startsWith('deepen ')) argumentsView.depth = Number.parseInt(text.slice(7), 10);
    else if (text === 'done') argumentsView.done = true;
  }
  return argumentsView;
}

/** Walk an id through tags until a commit is found; null when a non-tag blocks the path. */
function targetCommit(repo: Repo, id: string): string | null {
  let current = id;
  for (let hop = 0; hop < 8; hop += 1) {
    const object = repo.objects.get(current);
    if (object === undefined) return null;
    if (object.type === 'commit') return current;
    if (object.type !== 'tag') return null;
    current = parseTag(object.data).object;
  }
  return null;
}

/**
 * Choose the objects a fetch sends. Wants are walked down to `depth` (their chain is shallow-cut and
 * reported as a boundary); whatever the client's haves already cover is set aside, then every wanted
 * commit's tree and its reachable contents are gathered.
 */
function chooseFetchObjects(repo: Repo, argument: FetchArguments): { objects: GitObj[]; shallow: string[]; unshallow: string[] } {
  const table: ObjectTable = repo.objects;
  const depth = argument.depth !== null && Number.isFinite(argument.depth) && argument.depth > 0 ? argument.depth : null;
  const commits: string[] = [];
  const frontier = new Set<string>();
  const touched = new Set<string>();
  const extra: string[] = [];
  const descend = (oid: string, level: number): void => {
    if (touched.has(oid)) return;
    const object = table.get(oid);
    if (object === undefined) return;
    touched.add(oid);
    let commitId = oid;
    if (object.type === 'tag') {
      extra.push(oid);
      const target = targetCommit(repo, oid);
      if (target === null) return;
      commitId = target;
      if (touched.has(commitId)) return;
    }
    const commit = table.get(commitId);
    if (commit === undefined || commit.type !== 'commit') return;
    touched.add(commitId);
    commits.push(commitId);
    const parents = parseCommit(commit.data).parents;
    if (parents.length === 0) return;
    if (depth !== null && level >= depth) {
      frontier.add(commitId);
      return;
    }
    for (const parent of parents) descend(parent, level + 1);
  };
  for (const want of argument.wants) descend(want, 1);

  // Mark every object the client states it holds, so it is not shipped again. The client's shallow
  // set stops the walk there: those commits count as held, but their ancestors do not.
  const held = new Set<string>();
  if (argument.haves.length > 0) {
    const stops = new Set(argument.shallow);
    const pending = argument.haves.slice();
    while (pending.length > 0) {
      const id = pending.pop() as string;
      if (held.has(id)) continue;
      const object = table.get(id);
      if (object === undefined) continue;
      held.add(id);
      if (object.type === 'commit') {
        const commit = parseCommit(object.data);
        pending.push(commit.tree);
        if (!stops.has(id)) for (const parent of commit.parents) pending.push(parent);
      } else if (object.type === 'tree') {
        for (const entry of parseTree(object.data)) pending.push(entry.sha);
      } else if (object.type === 'tag') {
        pending.push(parseTag(object.data).object);
      }
    }
  }

  const toSend = new Set<string>(extra);
  for (const commitId of commits) {
    toSend.add(commitId);
    const commit = table.get(commitId);
    if (commit === undefined) continue;
    for (const id of reachable(table, [parseCommit(commit.data).tree])) toSend.add(id);
  }
  const objects: GitObj[] = [];
  for (const id of toSend) {
    if (held.has(id)) continue;
    const object = table.get(id);
    if (object !== undefined) objects.push(object);
  }
  const shallow = [...frontier].filter((oid) => !argument.shallow.includes(oid));
  const unshallow = argument.shallow.filter((oid) => !frontier.has(oid));
  return { objects, shallow, unshallow };
}

/** Answer a fetch: the packfile section (sideband-wrapped) after any shallow-info boundary move. */
function doFetch(repo: Repo, frames: Frame[]): Buffer {
  const argument = readFetchArguments(frames);
  const out: Buffer[] = [];
  if (argument.wants.length === 0) {
    out.push(PKT_FLUSH);
    return Buffer.concat(out);
  }
  const { objects, shallow, unshallow } = chooseFetchObjects(repo, argument);
  if (shallow.length > 0 || unshallow.length > 0) {
    out.push(pktLine('shallow-info\n'));
    for (const oid of shallow) out.push(pktLine(`shallow ${oid}\n`));
    for (const oid of unshallow) out.push(pktLine(`unshallow ${oid}\n`));
    out.push(PKT_DELIM);
  }
  out.push(pktLine('packfile\n'));
  out.push(sidebandPack(writePack(objects)));
  return Buffer.concat(out);
}

// ---------------------------------------------------------------------------
// receive-pack: the push
// ---------------------------------------------------------------------------

/** One command from a push command list. */
interface PushRef {
  oldOid: string;
  newOid: string;
  ref: string;
}

/** Parse the command list, take the pack off the tail, store it and report per-ref verdicts. */
function runReceivePack(repo: Repo, body: Buffer): Buffer {
  const { frames, rest } = splitPackets(body);
  const commands: PushRef[] = [];
  const capabilitySet = new Set<string>();
  for (const frame of frames) {
    if (frame.kind !== 'data') continue;
    const raw = frame.payload.toString('utf8');
    const nul = raw.indexOf('\0');
    const bodyless = (nul < 0 ? raw : raw.slice(0, nul)).replace(/\n$/, '');
    if (nul >= 0) {
      for (const capability of raw.slice(nul + 1).replace(/\n$/, '').split(' ')) {
        if (capability !== '') capabilitySet.add(capability);
      }
    }
    const [oldOid, newOid, ...refParts] = bodyless.split(' ');
    if (oldOid === undefined || newOid === undefined || refParts.length === 0) continue;
    commands.push({ oldOid, newOid, ref: refParts.join(' ') });
  }

  let unpacked = 'ok';
  if (rest.length > 0) {
    try {
      const opened = readPack(rest);
      for (const [oid, object] of opened.objects) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, object);
      }
    } catch (err) {
      unpacked = err instanceof Error ? err.message : 'unpack failed';
    }
  }

  const reportLines: Buffer[] = [pktLine(`unpack ${unpacked}\n`)];
  if (unpacked === 'ok') {
    for (const command of commands) {
      const current = repo.refs.get(command.ref) ?? ZERO_OID;
      let refusal: string | null = null;
      if (command.newOid === ZERO_OID) {
        repo.refs.delete(command.ref);
      } else if (current !== command.oldOid) {
        refusal = 'non-fast-forward';
      } else if (current !== ZERO_OID && !isAncestor(repo.objects, current, command.newOid)) {
        refusal = 'non-fast-forward';
      } else if (!repo.objects.has(command.newOid)) {
        refusal = 'missing object';
      } else {
        repo.refs.set(command.ref, command.newOid);
        if (command.ref.startsWith('refs/heads/') && !repo.refs.has(`refs/heads/${repo.defaultBranch}`)) {
          repo.defaultBranch = command.ref.slice('refs/heads/'.length);
        }
      }
      reportLines.push(pktLine(refusal === null ? `ok ${command.ref}\n` : `ng ${command.ref} ${refusal}\n`));
    }
  }
  reportLines.push(PKT_FLUSH);
  const report = Buffer.concat(reportLines);
  if (capabilitySet.has('side-band-64k')) {
    const framed: Buffer[] = [];
    for (let at = 0; at < report.length; at += 32768) {
      framed.push(pktLine(Buffer.concat([Buffer.from([1]), report.subarray(at, Math.min(report.length, at + 32768))])));
    }
    framed.push(PKT_FLUSH);
    return Buffer.concat(framed);
  }
  return report;
}