// Git smart HTTP: the v2 upload-pack advertisement, ls-refs and fetch, the v0 receive-pack
// advertisement and push (report-status inside side-band-64k). No git binary: the packfile
// reader and writer in gitobj.ts do that work.

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

const AGENT = 'agent=makefun-forge/1.0';

const UPLOAD_ADVERT = 'application/x-git-upload-pack-advertisement';
const UPLOAD_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_ADVERT = 'application/x-git-receive-pack-advertisement';
const RECEIVE_RESULT = 'application/x-git-receive-pack-result';

/** One decoded pkt-line: a payload, or one of the three control packets. */
type Packet = { kind: 'data'; payload: Buffer } | { kind: 'flush' } | { kind: 'delim' } | { kind: 'response-end' };

/** Split a pkt-line stream into packets. Trailing bytes (a packfile) are returned unconsumed. */
function decodePackets(buf: Buffer): { packets: Packet[]; rest: Buffer; ended: boolean } {
  const packets: Packet[] = [];
  let pos = 0;
  while (pos + 4 <= buf.length) {
    const length = Number.parseInt(buf.toString('latin1', pos, pos + 4), 16);
    if (Number.isNaN(length)) break;
    if (length === 0) {
      packets.push({ kind: 'flush' });
      pos += 4;
      continue;
    }
    if (length === 1) {
      packets.push({ kind: 'delim' });
      pos += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'response-end' });
      pos += 4;
      continue;
    }
    if (pos + length > buf.length) break;
    packets.push({ kind: 'data', payload: buf.subarray(pos + 4, pos + length) });
    pos += length;
    if (length < 4) break;
  }
  return { packets, rest: buf.subarray(pos), ended: pos >= buf.length };
}

function line(packet: Packet): string {
  return packet.kind === 'data' ? packet.payload.toString('utf8').replace(/\n$/, '') : '';
}

/** Wrap bytes in side-band-64k band 1 chunks, then close the stream. */
function sideband(payload: Buffer, max = 65500): Buffer {
  const parts: Buffer[] = [];
  for (let offset = 0; offset < payload.length; offset += max) {
    const chunk = payload.subarray(offset, Math.min(payload.length, offset + max));
    parts.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

function refLines(repo: Repo): Array<{ name: string; sha: string }> {
  const out: Array<{ name: string; sha: string }> = [];
  for (const [ref, sha] of repo.refs) out.push({ name: ref, sha });
  out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return out;
}

/** Peel a ref's object to a commit id when it is a tag. */
function peeled(repo: Repo, sha: string): string | null {
  const obj = repo.objects.get(sha);
  if (obj === undefined || obj.type !== 'tag') return null;
  let current = parseTag(obj.data).object;
  for (let i = 0; i < 8; i += 1) {
    const next = repo.objects.get(current);
    if (next === undefined || next.type !== 'tag') return current;
    current = parseTag(next.data).object;
  }
  return current;
}

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
      const wantsV2 = String(req.headers['git-protocol'] ?? '').includes('version=2');
      if (wantsV2) {
        sendBuffer(res, 200, uploadAdvertV2(), UPLOAD_ADVERT);
        return;
      }
      sendBuffer(res, 200, uploadAdvertV0(repo), UPLOAD_ADVERT);
      return;
    }
    sendError(res, 400, 'Unknown service');
    return;
  }

  if (suffix === 'git-upload-pack' && req.method === 'POST') {
    const result = handleUploadPack(repo, body);
    sendBuffer(res, 200, result, UPLOAD_RESULT);
    return;
  }

  if (suffix === 'git-receive-pack' && req.method === 'POST') {
    const result = handleReceivePack(repo, body);
    sendBuffer(res, 200, result, RECEIVE_RESULT);
    return;
  }

  sendError(res, 404, 'Not Found');
}

// ---------------------------------------------------------------------------
// advertisements

function v2Capabilities(): string[] {
  return ['version 2', 'agent=git/makefun-forge-1.0', 'ls-refs=unborn', 'fetch=shallow wait-for-done', 'server-option', 'object-format=sha1'];
}

function uploadAdvertV2(): Buffer {
  const parts: Buffer[] = [pktLine('# service=git-upload-pack\n'), PKT_FLUSH];
  for (const capability of v2Capabilities()) parts.push(pktLine(`${capability}\n`));
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

function uploadAdvertV0(repo: Repo): Buffer {
  const caps = ['multi_ack_detailed', 'side-band-64k', 'thin-pack', 'ofs-delta', 'shallow', 'no-progress', 'object-format=sha1', 'agent=makefun-forge/1.0', 'symref=HEAD:refs/heads/main'];
  const parts: Buffer[] = [pktLine('# service=git-upload-pack\n'), PKT_FLUSH];
  const refs = refLines(repo);
  if (refs.length === 0) {
    parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps.join(' ')}\n`));
  } else {
    const headSha = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? refs[0].sha;
    parts.push(pktLine(`${headSha} HEAD\0${caps.join(' ')}\n`));
    for (const ref of refs) parts.push(pktLine(`${ref.sha} ${ref.name}\n`));
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

function receiveAdvert(repo: Repo): Buffer {
  const caps = ['report-status', 'side-band-64k', 'delete-refs', 'ofs-delta', 'quiet', 'object-format=sha1', 'agent=makefun-forge/1.0'];
  const parts: Buffer[] = [pktLine('# service=git-receive-pack\n'), PKT_FLUSH];
  const refs = refLines(repo);
  if (refs.length === 0) {
    parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps.join(' ')}\n`));
  } else {
    refs.forEach((ref, index) => {
      const lineText = index === 0 ? `${ref.sha} ${ref.name}\0${caps.join(' ')}\n` : `${ref.sha} ${ref.name}\n`;
      parts.push(pktLine(lineText));
    });
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------
// upload-pack: ls-refs and fetch

function handleUploadPack(repo: Repo, body: Buffer): Buffer {
  const { packets } = decodePackets(body);
  const command = line(packets[0] ?? { kind: 'flush' });
  if (command === 'command=ls-refs') return lsRefs(repo, packets);
  if (command === 'command=fetch') return fetch(repo, packets);
  return PKT_FLUSH;
}

function lsRefs(repo: Repo, packets: Packet[]): Buffer {
  let sawDelim = false;
  let wantPeel = false;
  let wantSymrefs = false;
  let wantUnborn = false;
  const prefixes: string[] = [];
  for (const packet of packets.slice(1)) {
    if (packet.kind === 'delim') {
      sawDelim = true;
      continue;
    }
    if (packet.kind !== 'data' || !sawDelim) continue;
    const text = line(packet);
    if (text === 'peel') wantPeel = true;
    else if (text === 'symrefs') wantSymrefs = true;
    else if (text === 'unborn') wantUnborn = true;
    else if (text.startsWith('ref-prefix ')) prefixes.push(text.slice('ref-prefix '.length));
  }
  const matches = (refName: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => refName.startsWith(prefix));
  const parts: Buffer[] = [];
  const headSha = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
  if (matches('HEAD')) {
    if (headSha !== null) {
      const extra = wantSymrefs ? ` symref-target:refs/heads/${repo.defaultBranch}` : '';
      parts.push(pktLine(`${headSha} HEAD${extra}\n`));
    } else if (wantUnborn) {
      parts.push(pktLine(`unborn HEAD symref-target:refs/heads/${repo.defaultBranch}\n`));
    }
  }
  for (const ref of refLines(repo)) {
    if (!matches(ref.name)) continue;
    let text = `${ref.sha} ${ref.name}`;
    if (wantPeel) {
      const target = peeled(repo, ref.sha);
      if (target !== null) text += ` peeled:${target}`;
    }
    parts.push(pktLine(`${text}\n`));
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

interface FetchRequest {
  wants: string[];
  haves: string[];
  depth: number | null;
  shallow: string[];
  done: boolean;
}

function parseFetch(packets: Packet[]): FetchRequest {
  const request: FetchRequest = { wants: [], haves: [], depth: null, shallow: [], done: false };
  let inArgs = false;
  for (const packet of packets) {
    if (packet.kind === 'delim') {
      inArgs = true;
      continue;
    }
    if (packet.kind !== 'data' || !inArgs) continue;
    const text = line(packet);
    if (text.startsWith('want ')) request.wants.push(text.slice(5));
    else if (text.startsWith('have ')) request.haves.push(text.slice(5));
    else if (text.startsWith('shallow ')) request.shallow.push(text.slice(8));
    else if (text.startsWith('deepen ')) request.depth = Number.parseInt(text.slice(7), 10);
    else if (text === 'done') request.done = true;
  }
  return request;
}

/** The commits a fetch sends: the wants walked to `depth`, plus their trees and blobs. */
function fetchObjects(repo: Repo, request: FetchRequest): { objects: GitObj[]; shallow: string[]; unshallow: string[] } {
  const table: ObjectTable = repo.objects;
  const depth = request.depth !== null && Number.isFinite(request.depth) && request.depth > 0 ? request.depth : null;
  const commits: string[] = [];
  const boundary = new Set<string>();
  const seen = new Set<string>();
  const extra: string[] = [];
  const visit = (sha: string, level: number): void => {
    if (seen.has(sha)) return;
    const obj = table.get(sha);
    if (obj === undefined) return;
    seen.add(sha);
    let commitSha = sha;
    if (obj.type === 'tag') {
      extra.push(sha);
      const peeled = peelToCommit(repo, sha);
      if (peeled === null) return;
      commitSha = peeled;
      if (seen.has(commitSha)) return;
    }
    const commit = table.get(commitSha);
    if (commit === undefined || commit.type !== 'commit') return;
    seen.add(commitSha);
    commits.push(commitSha);
    const parents = parseCommit(commit.data).parents;
    if (parents.length === 0) return;
    if (depth !== null && level >= depth) {
      boundary.add(commitSha);
      return;
    }
    for (const parent of parents) visit(parent, level + 1);
  };
  for (const want of request.wants) visit(want, 1);

  // Objects the client already holds. A shallow boundary stops the walk: the client has that
  // commit but not its parents, so the parents' objects still have to be sent.
  const excluded = new Set<string>();
  if (request.haves.length > 0) {
    const stop = new Set(request.shallow);
    const stack = request.haves.slice();
    while (stack.length > 0) {
      const id = stack.pop() as string;
      if (excluded.has(id)) continue;
      const obj = table.get(id);
      if (obj === undefined) continue;
      excluded.add(id);
      if (obj.type === 'commit') {
        const commit = parseCommit(obj.data);
        stack.push(commit.tree);
        if (!stop.has(id)) for (const parent of commit.parents) stack.push(parent);
      } else if (obj.type === 'tree') {
        for (const entry of parseTree(obj.data)) stack.push(entry.sha);
      } else if (obj.type === 'tag') {
        stack.push(parseTag(obj.data).object);
      }
    }
  }

  const wanted = new Set<string>(extra);
  for (const commitSha of commits) {
    wanted.add(commitSha);
    const commit = table.get(commitSha);
    if (commit === undefined) continue;
    for (const id of reachable(table, [parseCommit(commit.data).tree])) wanted.add(id);
  }
  const objects: GitObj[] = [];
  for (const id of wanted) {
    if (excluded.has(id)) continue;
    const obj = table.get(id);
    if (obj !== undefined) objects.push(obj);
  }
  const shallow = [...boundary].filter((sha) => !request.shallow.includes(sha));
  const unshallow = request.shallow.filter((sha) => !boundary.has(sha));
  return { objects, shallow, unshallow };
}

function peelToCommit(repo: Repo, id: string): string | null {
  let current = id;
  for (let i = 0; i < 8; i += 1) {
    const obj = repo.objects.get(current);
    if (obj === undefined) return null;
    if (obj.type === 'commit') return current;
    if (obj.type !== 'tag') return null;
    current = parseTag(obj.data).object;
  }
  return null;
}

function fetch(repo: Repo, packets: Packet[]): Buffer {
  const request = parseFetch(packets);
  const parts: Buffer[] = [];
  if (request.wants.length === 0) {
    parts.push(PKT_FLUSH);
    return Buffer.concat(parts);
  }
  const { objects, shallow, unshallow } = fetchObjects(repo, request);
  if (shallow.length > 0 || unshallow.length > 0) {
    parts.push(pktLine('shallow-info\n'));
    for (const sha of shallow) parts.push(pktLine(`shallow ${sha}\n`));
    for (const sha of unshallow) parts.push(pktLine(`unshallow ${sha}\n`));
    parts.push(PKT_DELIM);
  }
  parts.push(pktLine('packfile\n'));
  parts.push(sideband(writePack(objects)));
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------
// receive-pack: push

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

function handleReceivePack(repo: Repo, body: Buffer): Buffer {
  const { packets, rest } = decodePackets(body);
  const commands: PushCommand[] = [];
  const capabilities = new Set<string>();
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    const raw = packet.payload.toString('utf8');
    const nul = raw.indexOf('\0');
    const text = (nul < 0 ? raw : raw.slice(0, nul)).replace(/\n$/, '');
    if (nul >= 0) {
      for (const capability of raw.slice(nul + 1).replace(/\n$/, '').split(' ')) {
        if (capability !== '') capabilities.add(capability);
      }
    }
    const [oldOid, newOid, ...refParts] = text.split(' ');
    if (oldOid === undefined || newOid === undefined || refParts.length === 0) continue;
    commands.push({ oldOid, newOid, ref: refParts.join(' ') });
  }

  let unpack = 'ok';
  if (rest.length > 0) {
    try {
      const read = readPack(rest.subarray(0, findPackEnd(rest)));
      for (const [id, object] of read.objects) {
        if (!repo.objects.has(id)) repo.objects.set(id, object);
      }
    } catch (err) {
      unpack = err instanceof Error ? err.message : 'unpack failed';
    }
  }

  const reportLines: Buffer[] = [pktLine(`unpack ${unpack}\n`)];
  if (unpack === 'ok') {
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
  if (capabilities.has('side-band-64k')) {
    const parts: Buffer[] = [];
    for (let offset = 0; offset < report.length; offset += 32768) {
      parts.push(pktLine(Buffer.concat([Buffer.from([1]), report.subarray(offset, Math.min(report.length, offset + 32768))])));
    }
    parts.push(PKT_FLUSH);
    return Buffer.concat(parts);
  }
  return report;
}

/** The packfile inside a push body ends where its own length says: the trailer is the last 20 bytes. */
function findPackEnd(buf: Buffer): number {
  return buf.length;
}
