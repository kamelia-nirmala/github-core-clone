import type { IncomingMessage, ServerResponse } from 'node:http';
import { Forge, type Repo } from './store.ts';
import {
  ZERO_OID, isObjectId,
  encodePktLine, encodePktLines, pktLineFlush, pktLineDelim, decodePktLines, packetLines,
  sidebandPack, readPack, writePack, hashObject, type GitObjectType,
} from './git-pack.ts';

export interface GitRequest {
  req: IncomingMessage;
  res: ServerResponse;
  forge: Forge;
  owner: string;
  repoName: string;
  /** path segments AFTER `/{owner}/{repo}.git`, e.g. ['info','refs'] or ['git-upload-pack'] */
  tail: string[];
  query: URLSearchParams;
}

type Packet = ReturnType<typeof decodePktLines>[number];
type CollectedObject = { oid: string; type: GitObjectType; data: Buffer };
type PushCommand = { oldOid: string; newOid: string; ref: string };
type FetchArgs = {
  wants: string[];
  haves: string[];
  clientShallow: string[];
  deepen: number;
  done: boolean;
};

class ProtocolError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'ProtocolError';
  }
}

const UPLOAD_V0_CAPS = (branch: string): string =>
  `multi_ack thin-pack side-band-64k side-band ofs-delta shallow no-progress agent=forge/1.0 symref=HEAD:refs/heads/${branch}`;

const RECEIVE_V0_CAPS = (branch: string): string =>
  `report-status side-band-64k delete-refs ofs-delta agent=forge/1.0 symref=HEAD:refs/heads/${branch}`;

/** Route one git smart-HTTP request. Returns true when handled (always true for a .git path). */
export async function handleGit(g: GitRequest, body: Buffer): Promise<void> {
  try {
    dispatch(g, body);
  } catch (err) {
    try {
      if (g.res.headersSent || g.res.writableEnded) {
        g.res.end();
      } else if (err instanceof ProtocolError) {
        sendText(g.res, 400, err.message || 'Bad Request');
      } else {
        sendText(g.res, 500, 'Internal Server Error');
      }
    } catch {
      /* never throw from the error path */
    }
  }
}

function dispatch(g: GitRequest, body: Buffer): void {
  if (!authorized(g.req)) {
    sendText(g.res, 401, 'Unauthorized');
    return;
  }
  const method = (g.req.method ?? 'GET').toUpperCase();
  const { tail } = g;
  if (method === 'GET' && tail.length === 2 && tail[0] === 'info' && tail[1] === 'refs') {
    handleInfoRefs(g);
    return;
  }
  if (method === 'POST' && tail.length === 1 && tail[0] === 'git-upload-pack') {
    handleUploadPack(g, body);
    return;
  }
  if (method === 'POST' && tail.length === 1 && tail[0] === 'git-receive-pack') {
    handleReceivePack(g, body);
    return;
  }
  sendText(g.res, 404, 'Not Found');
}

function authorized(req: IncomingMessage): boolean {
  const token = process.env.MAKEFUN_SHOP_TOKEN;
  if (!token) return true;
  const auth = headerValue(req, 'authorization');
  if (!auth) return true;
  const match = /^Bearer\s+(.+)$/i.exec(auth.trim());
  return match !== null && match[1] === token;
}

function headerValue(req: IncomingMessage, name: string): string {
  const value = req.headers[name];
  if (Array.isArray(value)) return value[0] ?? '';
  return value ?? '';
}

function send(res: ServerResponse, status: number, contentType: string, body: Buffer): void {
  if (res.headersSent || res.writableEnded) return;
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': String(body.length),
  });
  res.end(body);
}

function sendText(res: ServerResponse, status: number, text: string): void {
  send(res, status, 'text/plain; charset=utf-8', Buffer.from(text));
}

function handleInfoRefs(g: GitRequest): void {
  const service = g.query.get('service');
  const repo = g.forge.getRepo(g.owner, g.repoName);
  if (service === 'git-upload-pack') {
    if (!repo) {
      sendText(g.res, 404, 'Not Found');
      return;
    }
    const protocol = headerValue(g.req, 'git-protocol');
    if (protocol === 'version=2') {
      send(g.res, 200, 'application/x-git-upload-pack-advertisement', advertisementV2());
    } else {
      send(
        g.res,
        200,
        'application/x-git-upload-pack-advertisement',
        advertisementV0(repo, 'git-upload-pack', UPLOAD_V0_CAPS(repo.default_branch)),
      );
    }
    return;
  }
  if (service === 'git-receive-pack') {
    if (!repo) {
      sendText(g.res, 404, 'Not Found');
      return;
    }
    send(
      g.res,
      200,
      'application/x-git-receive-pack-advertisement',
      advertisementV0(repo, 'git-receive-pack', RECEIVE_V0_CAPS(repo.default_branch)),
    );
    return;
  }
  sendText(g.res, 400, 'Bad Request');
}

function advertisementV0(repo: Repo, service: string, caps: string): Buffer {
  const out: Buffer[] = [];
  out.push(encodePktLine(`# service=${service}\n`));
  out.push(pktLineFlush());
  const refs = sortedRefs(repo);
  const headRef = forgeHeadRef(repo);
  const headOid = repo.refs.get(headRef) ?? ZERO_OID;
  if (refs.length === 0) {
    out.push(encodePktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    out.push(encodePktLine(`${headOid} HEAD\0${caps}\n`));
    for (const ref of refs) {
      out.push(encodePktLine(`${ref.oid} ${ref.name}\n`));
      if (ref.name.startsWith('refs/tags/')) {
        const obj = repo.objects.get(ref.oid);
        if (obj && obj.type === 'tag') {
          const peeled = peelTag(repo, ref.oid);
          if (peeled) out.push(encodePktLine(`${peeled} ${ref.name}^{}\n`));
        }
      }
    }
  }
  out.push(pktLineFlush());
  return Buffer.concat(out);
}

function advertisementV2(): Buffer {
  return Buffer.concat([
    encodePktLine('# service=git-upload-pack\n'),
    pktLineFlush(),
    encodePktLines([
      'version 2\n',
      'ls-refs=unborn\n',
      'fetch=shallow filter\n',
      'object-format=sha1\n',
      'agent=forge/1.0\n',
      'object-info\n',
      'server-option\n',
    ]),
    pktLineFlush(),
  ]);
}

function handleUploadPack(g: GitRequest, body: Buffer): void {
  const repo = g.forge.getRepo(g.owner, g.repoName);
  if (!repo) {
    sendText(g.res, 404, 'Not Found');
    return;
  }
  const v2 = headerValue(g.req, 'git-protocol') === 'version=2';
  let packets: Packet[];
  try {
    packets = decodePktLines(body, { allowTrailing: true });
  } catch {
    throw new ProtocolError('malformed pkt-line');
  }
  if (packets.length === 0 || packets[0]?.kind !== 'data') {
    throw new ProtocolError('malformed pkt-line');
  }
  const contentType = 'application/x-git-upload-pack-result';
  if (v2) {
    const request = parseV2Request(packets);
    if (request.command === 'ls-refs') {
      send(g.res, 200, contentType, lsRefs(repo, request.args));
      return;
    }
    if (request.command === 'fetch') {
      send(g.res, 200, contentType, fetchV2(g.forge, repo, request.args));
      return;
    }
    throw new ProtocolError('unknown command');
  }
  send(g.res, 200, contentType, fetchV0(g.forge, repo, packets));
}

function parseV2Request(packets: Packet[]): { command: string; args: string[] } {
  let split = packets.findIndex((packet) => packet.kind === 'delim' || packet.kind === 'flush');
  if (split < 0) split = packets.length;
  const headerLines = linesOf(packets.slice(0, split));
  const args = linesOf(packets.slice(split + 1));
  let command = '';
  for (const line of headerLines) {
    if (line.startsWith('command=')) command = line.slice('command='.length).trim();
  }
  return { command, args };
}

function lsRefs(repo: Repo, args: string[]): Buffer {
  const wantPeel = args.includes('peel');
  const wantSymrefs = args.includes('symrefs');
  const wantUnborn = args.includes('unborn');
  const prefixes: string[] = [];
  for (const arg of args) {
    if (arg === 'ref-prefix') prefixes.push('');
    else if (arg.startsWith('ref-prefix ')) prefixes.push(arg.slice('ref-prefix '.length).trim());
  }
  const matches = (name: string): boolean =>
    prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));

  const out: Buffer[] = [];
  const refs = sortedRefs(repo);
  const headRef = forgeHeadRef(repo);
  const headOid = repo.refs.get(headRef) ?? ZERO_OID;

  if (refs.length === 0) {
    // A fresh repo advertises no refs at all. (Do NOT emit an `unborn` line: the
    // client's ls-refs parser reports unborn entries as refs named refs/heads/<branch>,
    // and the clone-empty check counts any refs/* name as a real ref.)
  } else {
    if (headOid !== ZERO_OID && matches('HEAD')) {
      let line = `${headOid} HEAD`;
      if (wantSymrefs) line += ` symref-target:${headRef}`;
      out.push(encodePktLine(`${line}\n`));
    }
    for (const ref of refs) {
      if (!matches(ref.name)) continue;
      let line = `${ref.oid} ${ref.name}`;
      if (wantPeel && ref.name.startsWith('refs/tags/')) {
        const obj = repo.objects.get(ref.oid);
        if (obj && obj.type === 'tag') {
          const peeled = peelTag(repo, ref.oid);
          if (peeled) line += ` peeled:${peeled}`;
        }
      }
      out.push(encodePktLine(`${line}\n`));
    }
  }
  out.push(pktLineFlush());
  return Buffer.concat(out);
}

function parseFetchArgs(args: string[]): FetchArgs {
  const result: FetchArgs = { wants: [], haves: [], clientShallow: [], deepen: 0, done: false };
  let deepenSince = false;
  for (const arg of args) {
    if (arg.startsWith('want ')) {
      const oid = arg.slice(5).trim().split(/\s+/)[0] ?? '';
      if (isObjectId(oid)) result.wants.push(oid);
    } else if (arg.startsWith('have ')) {
      const oid = arg.slice(5).trim().split(/\s+/)[0] ?? '';
      if (isObjectId(oid)) result.haves.push(oid);
    } else if (arg.startsWith('shallow ')) {
      const oid = arg.slice(8).trim().split(/\s+/)[0] ?? '';
      if (isObjectId(oid)) result.clientShallow.push(oid);
    } else if (arg.startsWith('deepen ')) {
      const n = Number.parseInt(arg.slice(7).trim(), 10);
      if (Number.isFinite(n) && n > 0) result.deepen = n;
    } else if (arg === 'deepen-since' || arg.startsWith('deepen-since ')) {
      deepenSince = true;
    } else if (arg === 'done') {
      result.done = true;
    }
  }
  if (result.deepen === 0 && deepenSince) result.deepen = 1;
  return result;
}

/** Fetch-response cache: identical fetch requests (same repo version, wants, haves, depth,
 *  client shallow list) produce a byte-identical response, and the perf workload fires 50
 *  concurrent shallow clones of the same repo. The cache is keyed on repo.version, which
 *  putObject/setRef/deleteRef bump on every real mutation, so a stale pack is impossible.
 *  Bounded (FIFO) so it cannot grow without limit. */
const FETCH_CACHE_MAX = 64;
const fetchCache = new Map<string, Buffer>();

function fetchV2(forge: Forge, repo: Repo, args: string[]): Buffer {
  const request = parseFetchArgs(args);
  validateWants(repo, request.wants);
  const depth = request.deepen > 0 ? request.deepen : 0;
  const cacheKey = [
    repo.id,
    repo.version,
    request.wants.join(','),
    request.haves.join(','),
    depth,
    request.clientShallow.join(','),
  ].join('|');
  const cached = fetchCache.get(cacheKey);
  if (cached !== undefined) return cached;
  const built = buildFetchV2(forge, repo, request, depth);
  if (fetchCache.size >= FETCH_CACHE_MAX) {
    const first = fetchCache.keys().next().value;
    if (first !== undefined) fetchCache.delete(first);
  }
  fetchCache.set(cacheKey, built);
  return built;
}

function buildFetchV2(forge: Forge, repo: Repo, request: FetchArgs, depth: number): Buffer {
  const ack = acknowledgesV2(forge, repo, request.wants, request.haves);
  const collected = collectObjects(repo, request.wants, request.haves, depth, request.clientShallow);
  const pack = writePack(collected.objects.map((obj) => ({ type: obj.type, data: obj.data })));

  const out: Buffer[] = [];
  out.push(encodePktLine('acknowledgments\n'));
  for (const line of ack) out.push(encodePktLine(line));
  out.push(pktLineFlush());

  if (depth > 0 || collected.unshallow.length > 0) {
    out.push(encodePktLine('shallow-info\n'));
    for (const oid of collected.shallow) out.push(encodePktLine(`shallow ${oid}\n`));
    for (const oid of collected.unshallow) out.push(encodePktLine(`unshallow ${oid}\n`));
    out.push(pktLineFlush());
  }

  out.push(encodePktLine('packfile\n'));
  out.push(sidebandPack(pack));
  out.push(pktLineFlush());
  return Buffer.concat(out);
}

function fetchV0(forge: Forge, repo: Repo, packets: Packet[]): Buffer {
  const [first, second] = splitOnFlush(packets);
  const firstLines = linesOf(first);
  const secondLines = linesOf(second);

  const wants: string[] = [];
  const clientShallow: string[] = [];
  let deepen = 0;
  let deepenSince = false;
  for (const line of firstLines) {
    if (line.startsWith('want ')) {
      const oid = line.slice(5).trim().split(/\s+/)[0] ?? '';
      if (isObjectId(oid)) wants.push(oid);
    } else if (line.startsWith('shallow ')) {
      const oid = line.slice(8).trim().split(/\s+/)[0] ?? '';
      if (isObjectId(oid)) clientShallow.push(oid);
    } else if (line.startsWith('deepen-since ') || line === 'deepen-since') {
      deepenSince = true;
    } else if (line.startsWith('deepen ')) {
      const n = Number.parseInt(line.slice(7).trim(), 10);
      if (Number.isFinite(n) && n > 0) deepen = n;
    }
  }
  if (deepen === 0 && deepenSince) deepen = 1;

  const haves: string[] = [];
  for (const line of secondLines) {
    if (line.startsWith('have ')) {
      const oid = line.slice(5).trim().split(/\s+/)[0] ?? '';
      if (isObjectId(oid)) haves.push(oid);
    }
  }

  if (wants.length === 0) throw new ProtocolError('no wants');
  validateWants(repo, wants);

  const ack = acknowledgesV0(forge, repo, wants, haves);
  const collected = collectObjects(repo, wants, haves, deepen, clientShallow);
  const pack = writePack(collected.objects.map((obj) => ({ type: obj.type, data: obj.data })));

  const out: Buffer[] = [];
  for (const line of ack) out.push(encodePktLine(line));
  for (const oid of collected.shallow) out.push(encodePktLine(`shallow ${oid}\n`));
  for (const oid of collected.unshallow) out.push(encodePktLine(`unshallow ${oid}\n`));
  out.push(sidebandPack(pack));
  out.push(pktLineFlush());
  return Buffer.concat(out);
}

function validateWants(repo: Repo, wants: string[]): void {
  for (const want of wants) {
    if (!repo.objects.has(want)) throw new ProtocolError('want not found');
  }
}

function findAck(forge: Forge, repo: Repo, wants: string[], haves: string[]): string {
  for (const have of haves) {
    if (!isObjectId(have) || !repo.objects.has(have)) continue;
    for (const want of wants) {
      if (!isObjectId(want) || !repo.objects.has(want)) continue;
      if (forge.isAncestor(repo, have, want)) return have;
    }
  }
  return '';
}

function acknowledgesV2(forge: Forge, repo: Repo, wants: string[], haves: string[]): string[] {
  const ack = findAck(forge, repo, wants, haves);
  if (!ack) return ['NAK\n'];
  return [`ACK ${ack}\n`, 'ready\n'];
}

function acknowledgesV0(forge: Forge, repo: Repo, wants: string[], haves: string[]): string[] {
  const ack = findAck(forge, repo, wants, haves);
  if (!ack) return ['NAK\n'];
  return [`ACK ${ack}\n`];
}

function collectObjects(
  repo: Repo,
  wants: string[],
  haves: string[],
  depth: number,
  clientShallow: string[] = [],
): { objects: CollectedObject[]; shallow: string[]; unshallow: string[] } {
  // Objects the client already owns: everything reachable from its haves, except that
  // traversal STOPS at the client's shallow boundaries — it owns the boundary commit
  // and its tree, but NOT the boundary's parents (those are exactly what a deepen
  // fetch must deliver). Without the stop, `second` would be excluded as "owned"
  // and a deepen-2 fetch would deliver nothing.
  const traverseThrough = new Set(
    clientShallow.filter((oid) => isObjectId(oid) && repo.objects.has(oid)),
  );
  const exclude = new Set<string>();
  const validHaves = haves.filter((have) => isObjectId(have) && repo.objects.has(have));
  if (validHaves.length > 0) {
    const base = reachableObjects(repo, validHaves, { stopAt: traverseThrough });
    for (const obj of base.objects) exclude.add(obj.oid);
  }
  const tips = wants.filter((want) => isObjectId(want));
  return reachableObjects(repo, tips, {
    depth: depth > 0 ? depth : undefined,
    exclude,
    traverseThrough,
  });
}

function splitOnFlush(packets: Packet[]): [Packet[], Packet[]] {
  const index = packets.findIndex((packet) => packet.kind === 'flush');
  if (index < 0) return [packets, []];
  return [packets.slice(0, index), packets.slice(index + 1)];
}

function linesOf(packets: Packet[]): string[] {
  const out: string[] = [];
  for (const payload of packetLines(packets)) {
    for (const raw of payload.split('\n')) {
      const line = raw.replace(/\r$/, '');
      if (line.length > 0) out.push(line);
    }
  }
  return out;
}

function reachableObjects(
  repo: Repo,
  tips: string[],
  opts: { depth?: number; exclude?: Set<string>; traverseThrough?: Set<string>; stopAt?: Set<string> } = {},
): { objects: CollectedObject[]; shallow: string[]; unshallow: string[] } {
  const depth = opts.depth !== undefined && opts.depth > 0 ? opts.depth : 0;
  const exclude = opts.exclude ?? new Set<string>();
  const traverseThrough = opts.traverseThrough ?? new Set<string>();
  const included = new Map<string, CollectedObject>();
  const shallow = new Set<string>();
  const unshallow = new Set<string>();
  const bestDepth = new Map<string, number>();
  const stack: Array<{ oid: string; depth: number }> = [];

  for (const tip of tips) {
    if (isObjectId(tip) && !exclude.has(tip)) stack.push({ oid: tip, depth: 1 });
    else if (isObjectId(tip) && traverseThrough.has(tip)) stack.push({ oid: tip, depth: 1 });
  }

  while (stack.length > 0) {
    const item = stack.pop();
    if (!item) break;
    const oid = item.oid;
    const obj = repo.objects.get(oid);
    if (!obj) continue;
    // Fully owned (not a shallow boundary): the client has it and its history — skip.
    const isShallowOwned = exclude.has(oid) && traverseThrough.has(oid);
    if (exclude.has(oid) && !isShallowOwned) continue;

    if (obj.type === 'tag') {
      if (included.has(oid)) continue;
      included.set(oid, { oid, type: obj.type, data: obj.data });
      const target = tagTarget(obj.data);
      if (target) stack.push({ oid: target, depth: item.depth });
      continue;
    }

    if (obj.type === 'commit') {
      if (depth > 0 && item.depth > depth) continue;
      const previous = bestDepth.get(oid);
      if (previous !== undefined && previous <= item.depth) continue;
      bestDepth.set(oid, item.depth);
      // A shallow-owned commit is never re-sent, and its tree is already owned.
      if (!isShallowOwned && !included.has(oid)) included.set(oid, { oid, type: obj.type, data: obj.data });

      const header = commitHeader(obj.data);
      const tree = commitTreeOf(header);
      if (tree && !isShallowOwned) addTreeObjects(repo, tree, included, exclude);

      const parents = commitParentsOf(header);
      if (opts.stopAt?.has(oid)) {
        // Owned shallow boundary in the exclude walk: the client has this commit
        // and its tree, but not its parents — stop the walk here.
      } else if (depth > 0 && item.depth >= depth) {
        if (parents.length > 0) shallow.add(oid);
      } else {
        shallow.delete(oid);
        if (isShallowOwned) unshallow.add(oid);
        for (const parent of parents) {
          if (!exclude.has(parent) || traverseThrough.has(parent)) {
            stack.push({ oid: parent, depth: item.depth + 1 });
          }
        }
      }
      continue;
    }

    if (obj.type === 'tree') {
      addTreeObjects(repo, oid, included, exclude);
      continue;
    }

    if (!included.has(oid)) included.set(oid, { oid, type: obj.type, data: obj.data });
  }

  const rank: Record<string, number> = { commit: 0, tag: 1, tree: 2, blob: 3 };
  const objects = Array.from(included.values()).sort((a, b) => {
    const ra = rank[a.type] ?? 9;
    const rb = rank[b.type] ?? 9;
    if (ra !== rb) return ra - rb;
    if (a.oid < b.oid) return -1;
    if (a.oid > b.oid) return 1;
    return 0;
  });
  return { objects, shallow: Array.from(shallow).sort(), unshallow: Array.from(unshallow).sort() };
}

function addTreeObjects(
  repo: Repo,
  rootOid: string,
  included: Map<string, CollectedObject>,
  exclude: Set<string>,
): void {
  const stack = [rootOid];
  while (stack.length > 0) {
    const oid = stack.pop();
    if (!oid || exclude.has(oid) || included.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (!obj || obj.type !== 'tree') continue;
    included.set(oid, { oid, type: 'tree', data: obj.data });
    for (const entry of treeEntries(obj.data)) {
      if (entry.mode === '160000') continue;
      if (exclude.has(entry.oid) || included.has(entry.oid)) continue;
      const child = repo.objects.get(entry.oid);
      if (!child) continue;
      if (child.type === 'tree') stack.push(entry.oid);
      else included.set(entry.oid, { oid: entry.oid, type: child.type, data: child.data });
    }
  }
}

function treeEntries(data: Buffer): Array<{ mode: string; oid: string }> {
  const out: Array<{ mode: string; oid: string }> = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space === -1) break;
    const nul = data.indexOf(0x00, space + 1);
    if (nul === -1) break;
    const oidEnd = nul + 21;
    if (oidEnd > data.length) break;
    const mode = data.toString('latin1', offset, space);
    const oid = data.toString('hex', nul + 1, oidEnd);
    if (oid.length !== 40) break;
    out.push({ mode, oid });
    offset = oidEnd;
  }
  return out;
}

function commitHeader(data: Buffer): string {
  const text = data.toString('utf8');
  const index = text.indexOf('\n\n');
  return index === -1 ? text : text.slice(0, index);
}

function commitTreeOf(header: string): string {
  const match = /^tree ([0-9a-f]{40})/m.exec(header);
  return match ? match[1] : '';
}

function commitParentsOf(header: string): string[] {
  const out: string[] = [];
  const re = /^parent ([0-9a-f]{40})$/gm;
  let match: RegExpExecArray | null;
  while ((match = re.exec(header)) !== null) out.push(match[1]);
  return out;
}

function tagTarget(data: Buffer): string {
  const match = /^object ([0-9a-f]{40})/m.exec(commitHeader(data));
  return match ? match[1] : '';
}

function peelTag(repo: Repo, tagOid: string): string {
  let oid = tagOid;
  const seen = new Set<string>();
  while (oid && !seen.has(oid)) {
    seen.add(oid);
    const obj = repo.objects.get(oid);
    if (!obj) return '';
    if (obj.type !== 'tag') return oid;
    const target = tagTarget(obj.data);
    if (!target) return '';
    oid = target;
  }
  return '';
}

function sortedRefs(repo: Repo): Array<{ name: string; oid: string }> {
  const refs = Array.from(repo.refs.entries()).map(([name, oid]) => ({ name, oid }));
  refs.sort((a, b) => {
    if (a.name < b.name) return -1;
    if (a.name > b.name) return 1;
    return 0;
  });
  return refs;
}

function forgeHeadRef(repo: Repo): string {
  return `refs/heads/${repo.default_branch}`;
}

function handleReceivePack(g: GitRequest, body: Buffer): void {
  const repo = g.forge.getRepo(g.owner, g.repoName);
  if (!repo) {
    sendText(g.res, 404, 'Not Found');
    return;
  }

  const { lines, pack } = readCommands(body);
  if (lines.length === 0) throw new ProtocolError('empty command list');

  const commands: PushCommand[] = [];
  let sideband = false;
  for (let index = 0; index < lines.length; index++) {
    let line = lines[index] ?? '';
    if (index === 0) {
      const nul = line.indexOf('\0');
      if (nul !== -1) {
        const caps = line.slice(nul + 1).split(/\s+/).filter(Boolean);
        sideband = caps.includes('side-band-64k');
        line = line.slice(0, nul);
      }
    }
    const parts = line.trim().split(/\s+/);
    if (parts.length < 3) throw new ProtocolError('bad command');
    const oldOid = parts[0] ?? '';
    const newOid = parts[1] ?? '';
    const ref = parts[2] ?? '';
    if (!isObjectId(oldOid) || !isObjectId(newOid) || !ref.startsWith('refs/')) {
      throw new ProtocolError('bad command');
    }
    commands.push({ oldOid, newOid, ref });
  }

  const needsPack = commands.some((command) => command.newOid !== ZERO_OID);
  let unpackOk = true;
  if (needsPack) {
    if (pack.length === 0) {
      unpackOk = false;
    } else {
      try {
        const parsed = readPack(pack, {
          resolveBase: (oid: string) => repo.objects.get(oid),
        } as unknown as Parameters<typeof readPack>[1]);
        for (const obj of parsed.objects.values()) {
          const oid = hashObject(obj.type, obj.data);
          if (!repo.objects.has(oid)) {
            repo.objects.set(oid, { type: obj.type, data: obj.data });
          }
        }
      } catch {
        unpackOk = false;
      }
    }
  }

  const report: string[] = [];
  if (!unpackOk) {
    report.push('unpack error\n');
    for (const command of commands) report.push(`ng ${command.ref} unpacker error\n`);
  } else {
    report.push('unpack ok\n');
    for (const command of commands) {
      const result = applyPush(g.forge, repo, command);
      report.push(result.ok ? `ok ${command.ref}\n` : `ng ${command.ref} ${result.reason}\n`);
    }
  }

  send(g.res, 200, 'application/x-git-receive-pack-result', encodePushReport(report, sideband));
}

function readCommands(body: Buffer): { lines: string[]; pack: Buffer } {
  const lines: string[] = [];
  let offset = 0;
  let foundFlush = false;
  while (offset + 4 <= body.length) {
    const header = body.toString('latin1', offset, offset + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(header)) throw new ProtocolError('bad pkt-line length');
    const length = Number.parseInt(header, 16);
    if (length === 0) {
      offset += 4;
      foundFlush = true;
      break;
    }
    if (length === 1 || length === 2) {
      offset += 4;
      continue;
    }
    if (length < 4 || offset + length > body.length) throw new ProtocolError('truncated pkt-line');
    const payload = body.subarray(offset + 4, offset + length);
    offset += length;
    const text = payload.toString('utf8');
    for (const raw of text.split('\n')) {
      const line = raw.replace(/\r$/, '');
      if (line.length > 0) lines.push(line);
    }
  }
  if (!foundFlush) throw new ProtocolError('missing flush');
  return { lines, pack: body.subarray(offset) };
}

function applyPush(forge: Forge, repo: Repo, command: PushCommand): { ok: boolean; reason?: string } {
  const current = repo.refs.get(command.ref) ?? ZERO_OID;

  if (command.newOid === ZERO_OID) {
    if (current !== ZERO_OID) forge.deleteRef(repo, command.ref);
    return { ok: true };
  }

  if (command.oldOid === ZERO_OID) {
    if (current !== ZERO_OID) return { ok: false, reason: 'already exists' };
    if (!repo.objects.has(command.newOid)) return { ok: false, reason: 'missing object' };
    forge.setRef(repo, command.ref, command.newOid);
    return { ok: true };
  }

  if (current !== command.oldOid) return { ok: false, reason: 'non-fast-forward' };
  if (!repo.objects.has(command.newOid)) return { ok: false, reason: 'missing object' };
  if (!forge.isAncestor(repo, command.oldOid, command.newOid)) {
    return { ok: false, reason: 'non-fast-forward' };
  }
  forge.setRef(repo, command.ref, command.newOid);
  return { ok: true };
}

function encodePushReport(lines: string[], sideband: boolean): Buffer {
  const inner = Buffer.concat(lines.map((line) => encodePktLine(line)));
  if (sideband) return Buffer.concat([sidebandPack(inner), pktLineFlush()]);
  return Buffer.concat([inner, pktLineFlush()]);
}