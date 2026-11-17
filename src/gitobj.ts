// Git's object model in one place: hashing, the tree and commit encodings, and the packfile
// reader and writer. Nothing here touches HTTP; rest.ts, smart.ts and store.ts build on it.
//
// Everything is deterministic: a commit's bytes come from its tree, its parent, the caller's
// message and a caller-supplied timestamp, never from the clock.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export type ObjType = 'blob' | 'tree' | 'commit' | 'tag';

export interface GitObj {
  type: ObjType;
  data: Buffer;
}

/** The object table a repository owns: id -> bytes, with the type git stores beside them. */
export type ObjectTable = Map<string, GitObj>;

export const ZERO_OID = '0000000000000000000000000000000000000000';

export function hashObject(type: ObjType, data: Buffer): string {
  const h = createHash('sha1');
  h.update(`${type} ${data.length}\0`, 'utf8');
  h.update(data);
  return h.digest('hex');
}

export function putObject(table: ObjectTable, type: ObjType, data: Buffer): string {
  const id = hashObject(type, data);
  if (!table.has(id)) table.set(id, { type, data });
  return id;
}

export function getObject(table: ObjectTable, id: string): GitObj | undefined {
  return table.get(id);
}

// ---------------------------------------------------------------------------
// trees

export interface TreeEntry {
  mode: string;
  name: string;
  sha: string;
}

/** Read a tree's entries. A tree is `<mode> <name>\0<20 raw bytes>` repeated. */
export function parseTree(data: Buffer): TreeEntry[] {
  const out: TreeEntry[] = [];
  let i = 0;
  while (i < data.length) {
    const space = data.indexOf(0x20, i);
    if (space < 0) break;
    const nul = data.indexOf(0x00, space);
    if (nul < 0) break;
    const mode = data.toString('latin1', i, space);
    const name = data.toString('utf8', space + 1, nul);
    const sha = data.toString('hex', nul + 1, nul + 21);
    out.push({ mode, name, sha });
    i = nul + 21;
  }
  return out;
}

export function encodeTree(entries: TreeEntry[]): Buffer {
  const sorted = entries.slice().sort(compareTreeEntries);
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    const head = Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8');
    parts.push(head, Buffer.from(entry.sha, 'hex'));
  }
  return Buffer.concat(parts);
}

/** git's tree order: byte order of the name, with a directory's name compared as `name/`. */
function compareTreeEntries(a: TreeEntry, b: TreeEntry): number {
  const an = a.name + (a.mode === '40000' ? '/' : '');
  const bn = b.name + (b.mode === '40000' ? '/' : '');
  return Buffer.compare(Buffer.from(an, 'utf8'), Buffer.from(bn, 'utf8'));
}

export function writeTree(table: ObjectTable, entries: TreeEntry[]): string {
  return putObject(table, 'tree', encodeTree(entries));
}

/** Resolve `parts` inside a tree to the object id, or null. An empty `parts` is the tree itself. */
export function treeLookup(table: ObjectTable, rootSha: string | null, parts: readonly string[]): string | null {
  if (rootSha === null) return null;
  if (parts.length === 0) return rootSha;
  let current = rootSha;
  for (let i = 0; i < parts.length; i += 1) {
    const obj = table.get(current);
    if (obj === undefined || obj.type !== 'tree') return null;
    const entry = parseTree(obj.data).find((candidate) => candidate.name === parts[i]);
    if (entry === undefined) return null;
    if (i === parts.length - 1) return entry.sha;
    if (entry.mode !== '40000') return null;
    current = entry.sha;
  }
  return null;
}

/**
 * Write the tree that holds `parts` set to `sha` (or removed when `sha` is null), keeping every
 * other entry of `rootSha`. Returns the new root tree id (an empty tree when the last file goes).
 */
export function treeWith(table: ObjectTable, rootSha: string | null, parts: readonly string[], sha: string | null): string {
  const entries = rootSha === null ? [] : parseTree(table.get(rootSha)?.data ?? Buffer.alloc(0));
  const [head, ...rest] = parts;
  const index = entries.findIndex((entry) => entry.name === head);
  if (rest.length === 0) {
    if (index >= 0) entries.splice(index, 1);
    if (sha !== null) entries.push({ mode: '100644', name: head, sha });
    return writeTree(table, entries);
  }
  const childSha = index >= 0 && entries[index].mode === '40000' ? entries[index].sha : null;
  const nextChild = treeWith(table, childSha, rest, sha);
  if (index >= 0) entries.splice(index, 1);
  // Keep an empty directory out of the tree, as git does.
  if (parseTree(table.get(nextChild)?.data ?? Buffer.alloc(0)).length > 0) {
    entries.push({ mode: '40000', name: head, sha: nextChild });
  }
  return writeTree(table, entries);
}

/** Every file path below a tree with its blob id, as a flat map. */
export function flattenTree(table: ObjectTable, rootSha: string | null, prefix = ''): Map<string, string> {
  const out = new Map<string, string>();
  if (rootSha === null) return out;
  const obj = table.get(rootSha);
  if (obj === undefined) return out;
  for (const entry of parseTree(obj.data)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') {
      for (const [childPath, childSha] of flattenTree(table, entry.sha, path)) out.set(childPath, childSha);
    } else {
      out.set(path, entry.sha);
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// commits and tags

export interface CommitFields {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function encodeCommit(fields: CommitFields): Buffer {
  const lines: string[] = [`tree ${fields.tree}`];
  for (const parent of fields.parents) lines.push(`parent ${parent}`);
  lines.push(`author ${fields.author}`, `committer ${fields.committer}`, '', fields.message);
  return Buffer.from(lines.join('\n'), 'utf8');
}

export function parseCommit(data: Buffer): CommitFields {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const header = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  const fields: CommitFields = { tree: '', parents: [], author: '', committer: '', message };
  for (const line of header.split('\n')) {
    if (line.startsWith('tree ')) fields.tree = line.slice(5);
    else if (line.startsWith('parent ')) fields.parents.push(line.slice(7));
    else if (line.startsWith('author ')) fields.author = line.slice(7);
    else if (line.startsWith('committer ')) fields.committer = line.slice(10);
  }
  return fields;
}

export interface TagFields {
  object: string;
  type: string;
  tag: string;
  tagger: string;
  message: string;
}

export function parseTag(data: Buffer): TagFields {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const header = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  const fields: TagFields = { object: '', type: '', tag: '', tagger: '', message };
  for (const line of header.split('\n')) {
    if (line.startsWith('object ')) fields.object = line.slice(7);
    else if (line.startsWith('type ')) fields.type = line.slice(5);
    else if (line.startsWith('tag ')) fields.tag = line.slice(4);
    else if (line.startsWith('tagger ')) fields.tagger = line.slice(7);
  }
  return fields;
}

/** The commit a tag points at, following nested tags; the object id itself for anything else. */
export function peelShallow(table: ObjectTable, id: string): string {
  let current = id;
  for (let i = 0; i < 8; i += 1) {
    const obj = table.get(current);
    if (obj === undefined || obj.type !== 'tag') return current;
    current = parseTag(obj.data).object;
  }
  return current;
}

/** Walk a commit's first parents, newest first, stopping at a limit. */
export function commitChain(table: ObjectTable, tip: string, limit = 1000): string[] {
  const out: string[] = [];
  let current: string | undefined = tip;
  while (current !== undefined && out.length < limit) {
    const obj = table.get(current);
    if (obj === undefined || obj.type !== 'commit') break;
    out.push(current);
    current = parseCommit(obj.data).parents[0];
  }
  return out;
}

/** True when `ancestor` is reachable from `descendant` by parents (or is the same commit). */
export function isAncestor(table: ObjectTable, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const stack = [descendant];
  while (stack.length > 0) {
    const id = stack.pop() as string;
    if (id === ancestor) return true;
    if (seen.has(id)) continue;
    seen.add(id);
    const obj = table.get(id);
    if (obj === undefined || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) stack.push(parent);
  }
  return false;
}

/** Every object reachable from the given roots (commits, their trees, tags). */
export function reachable(table: ObjectTable, roots: readonly string[]): Set<string> {
  const seen = new Set<string>();
  const stack = roots.slice();
  while (stack.length > 0) {
    const id = stack.pop() as string;
    if (seen.has(id)) continue;
    const obj = table.get(id);
    if (obj === undefined) continue;
    seen.add(id);
    if (obj.type === 'commit') {
      const commit = parseCommit(obj.data);
      stack.push(commit.tree);
      for (const parent of commit.parents) stack.push(parent);
    } else if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) stack.push(entry.sha);
    } else if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
    }
  }
  return seen;
}

// ---------------------------------------------------------------------------
// packfiles

/** A pack object's data, plus the base it is a delta against when it is one. */
interface PackEntry {
  type: ObjType;
  data: Buffer;
}

export interface ReadPackResult {
  objects: Map<string, GitObj>;
  deltaCount: number;
}

/**
 * Read a version 2 packfile: header, entries (with OFS/REF deltas resolved against the pack or
 * `resolveBase`), and the SHA-1 trailer. Throws a message a caller can answer with.
 */
export function readPack(pack: Buffer, resolveBase?: (id: string) => GitObj | undefined): ReadPackResult {
  if (pack.length < 32 || pack.toString('latin1', 0, 4) !== 'PACK') throw new Error('not a packfile');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const objects = new Map<string, GitObj>();
  const byOffset = new Map<number, PackEntry>();
  let pos = 12;
  let deltaCount = 0;
  for (let index = 0; index < count; index += 1) {
    const entryStart = pos;
    let byte = pack[pos];
    pos += 1;
    const typeCode = (byte >> 4) & 7;
    let size = byte & 15;
    let shift = 4;
    while ((byte & 0x80) !== 0) {
      byte = pack[pos];
      pos += 1;
      size += (byte & 0x7f) * 2 ** shift;
      shift += 7;
    }
    let baseOffset: number | null = null;
    let baseId: string | null = null;
    if (typeCode === 6) {
      byte = pack[pos];
      pos += 1;
      let ofs = byte & 0x7f;
      while ((byte & 0x80) !== 0) {
        byte = pack[pos];
        pos += 1;
        ofs = (ofs + 1) * 128 + (byte & 0x7f);
      }
      baseOffset = entryStart - ofs;
    } else if (typeCode === 7) {
      baseId = pack.toString('hex', pos, pos + 20);
      pos += 20;
    }
    const inflated = inflateAt(pack, pos);
    pos = inflated.next;
    if (inflated.data.length !== size) throw new Error(`pack entry ${index} inflated to ${inflated.data.length} bytes, header says ${size}`);
    let type: ObjType;
    let data: Buffer;
    if (typeCode === 1) { type = 'commit'; data = inflated.data; }
    else if (typeCode === 2) { type = 'tree'; data = inflated.data; }
    else if (typeCode === 3) { type = 'blob'; data = inflated.data; }
    else if (typeCode === 4) { type = 'tag'; data = inflated.data; }
    else if (typeCode === 6 || typeCode === 7) {
      deltaCount += 1;
      let base: PackEntry | GitObj | undefined;
      if (baseOffset !== null) {
        base = byOffset.get(baseOffset);
        if (base === undefined) throw new Error(`pack delta at ${entryStart} names a base offset the pack does not hold`);
      } else if (baseId !== null) {
        base = objects.get(baseId) ?? resolveBase?.(baseId);
        if (base === undefined) throw new Error(`pack delta names a base object the server does not hold`);
      }
      if (base === undefined) throw new Error('pack delta has no base');
      data = applyDelta(base.data, inflated.data);
      type = base.type;
    } else {
      throw new Error(`unsupported pack object type ${typeCode}`);
    }
    const id = hashObject(type, data);
    const entry: PackEntry = { type, data };
    byOffset.set(entryStart, entry);
    if (!objects.has(id)) objects.set(id, { type, data });
  }
  const expected = pack.toString('hex', pack.length - 20, pack.length);
  const actual = createHash('sha1').update(pack.subarray(0, pack.length - 20)).digest('hex');
  if (expected !== actual) throw new Error('pack trailer does not match its bytes');
  return { objects, deltaCount };
}

/** Inflate the zlib stream that starts at `pos`; `next` is the first byte after it. */
function inflateAt(buf: Buffer, pos: number): { data: Buffer; next: number } {
  let result: { buffer: Buffer; engine: { bytesWritten: number } };
  try {
    result = inflateSync(buf.subarray(pos), { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
  } catch (err) {
    throw new Error(`zlib stream at ${pos} did not inflate: ${err instanceof Error ? err.message : String(err)}`);
  }
  const consumed = result.engine.bytesWritten;
  if (!consumed || consumed <= 0) throw new Error(`zlib stream at ${pos} consumed no bytes`);
  return { data: result.buffer, next: pos + consumed };
}

/** Apply a git delta (the base-size, result-size, copy/insert opcode format) to a base object. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const readSize = (): number => {
    let size = 0;
    let shift = 0;
    let byte = delta[pos];
    pos += 1;
    size = byte & 0x7f;
    while ((byte & 0x80) !== 0) {
      shift += 7;
      byte = delta[pos];
      pos += 1;
      size += (byte & 0x7f) * 2 ** shift;
    }
    return size;
  };
  const baseSize = readSize();
  const resultSize = readSize();
  if (baseSize !== base.length) throw new Error('delta base size does not match the base object');
  const out = Buffer.allocUnsafe(resultSize);
  let written = 0;
  while (pos < delta.length) {
    const opcode = delta[pos];
    pos += 1;
    if ((opcode & 0x80) !== 0) {
      let offset = 0;
      let size = 0;
      if ((opcode & 0x01) !== 0) { offset += delta[pos]; pos += 1; }
      if ((opcode & 0x02) !== 0) { offset += delta[pos] * 256; pos += 1; }
      if ((opcode & 0x04) !== 0) { offset += delta[pos] * 65536; pos += 1; }
      if ((opcode & 0x08) !== 0) { offset += delta[pos] * 16777216; pos += 1; }
      if ((opcode & 0x10) !== 0) { size += delta[pos]; pos += 1; }
      if ((opcode & 0x20) !== 0) { size += delta[pos] * 256; pos += 1; }
      if ((opcode & 0x40) !== 0) { size += delta[pos] * 65536; pos += 1; }
      if (size === 0) size = 0x10000;
      base.copy(out, written, offset, offset + size);
      written += size;
    } else if (opcode !== 0) {
      delta.copy(out, written, pos, pos + opcode);
      written += opcode;
      pos += opcode;
    } else {
      throw new Error('delta holds a reserved opcode');
    }
  }
  if (written !== resultSize) throw new Error(`delta wrote ${written} bytes, header says ${resultSize}`);
  return out;
}

const TYPE_CODE: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };

/** Build a version 2 packfile from objects (no deltas: every entry stands alone). */
export function writePack(objects: readonly GitObj[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'latin1');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const obj of objects) {
    const type = TYPE_CODE[obj.type];
    const size = obj.data.length;
    const bytes: number[] = [];
    let remaining = size;
    let first = (type << 4) | (remaining & 15);
    remaining = Math.floor(remaining / 16);
    while (remaining > 0) {
      bytes.push(first | 0x80);
      first = remaining & 0x7f;
      remaining = Math.floor(remaining / 128);
    }
    bytes.push(first);
    parts.push(Buffer.from(bytes), deflateSync(obj.data));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

// ---------------------------------------------------------------------------
// pack pkt-line helpers shared with smart.ts

/** Encode one pkt-line; the control packets are `flush`, `delim` and `response-end`. */
export function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const length = body.length + 4;
  const head = Buffer.from(length.toString(16).padStart(4, '0'), 'latin1');
  return Buffer.concat([head, body]);
}

export const PKT_FLUSH = Buffer.from('0000', 'latin1');
export const PKT_DELIM = Buffer.from('0001', 'latin1');
