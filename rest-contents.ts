// ---------------------------------------------------------------------------
// Repository contents, commit history, and git refs over the REST v3
// surface.
//
// This module owns three families of routes below /api/v3/repos/{owner}/
// {repo}:
//
//   - GET .../commits — the commit history, walked from a tip backwards
//     through parents, newest first in depth-first order.
//   - GET / PUT / DELETE .../contents/{path} — reading a file or a
//     directory listing, creating or updating a file, and deleting one.
//     Every write is a real commit: a new blob, a rebuilt tree, and a
//     commit object on the branch, exactly the way a push would do it.
//   - GET / DELETE .../git/refs/{heads,tags}/{name} — reading one ref, and
//     deleting a branch.
//
// How a request flows through here, in plain language:
//
//   1. The gate checks run before anything route-shaped is considered:
//      a missing or wrong token is a 401, and a body that failed to parse
//      is a 400. Both answers come first even for paths this module does
//      not own, because the application server reaches this module before
//      it has decided the path belongs to anyone.
//   2. The route shape is then recognised: commits, contents, or refs,
//      each with the methods it accepts. A shape this module does not
//      recognise is left alone so the next module gets its turn.
//   3. The owner must be the viewer and the repository must exist, and
//      only then does the handler for the matched route run.
//
// A note on the clock, because it surprises people: the store stamps
// things from a logical clock that ticks once per call, so the number of
// stamp reads inside a handler is part of its observable behaviour.
// Writing a file, for example, reads the clock once for the commit's
// timestamp and once more for the repository's updated stamp. The
// handlers below keep those reads exactly where they were, even when
// sharing one read would look tidier, because downstream checks compare
// the stamps the trial produces against the stamps it expects.
//
// Nothing in this file touches the network or the disk. All state lives in
// the Forge instance handed in through the request context, and every
// answer is built from that state plus the request itself.
// ---------------------------------------------------------------------------

import type { IncomingMessage } from 'node:http';
import type { ServerResponse } from 'node:http';
import type { Forge } from './store.ts';
import type { Repo } from './store.ts';
import type { FlatTree } from './store.ts';
import { VIEWER } from './store.ts';
import { sendJson } from './util.ts';
import { sendError } from './util.ts';
import { isPlainObject } from './util.ts';
import { b64encode } from './util.ts';
import { b64decode } from './util.ts';
import { hashObject } from './git-pack.ts';
import { ZERO_OID } from './git-pack.ts';

// Everything a handler needs to answer one request. The application server
// assembles this from the raw request before dispatching; the fields are
// deliberately boring so that every route module can share the same shape.
export interface Ctx {
  // The raw Node request, kept for the method and for nothing else here.
  req: IncomingMessage;
  // The raw Node response the answer is written to.
  res: ServerResponse;
  // The in-memory store holding every repository and git object.
  forge: Forge;
  // The request path split on slashes, without the leading /api/v3 part.
  segments: string[];
  // The parsed query string, for the ref and sha parameters.
  query: URLSearchParams;
  // The parsed JSON body, or undefined when the request carried no body.
  body: unknown;
  // True when a body was present but its JSON would not parse.
  bodyMalformed: boolean;
  // True when the caller presented the token the server expects.
  authed: boolean;
  // The Host header value, unused by these routes but part of the shape.
  host: string;
}

// A commit object, taken apart: the tree it points at, its parents in
// order, its message, and the author's timestamp as seconds since epoch.
interface CommitInfo {
  tree: string;
  parents: string[];
  message: string;
  epoch: number;
}

// A 40-hex-character object id, upper or lower case. Object ids are
// stored lower case, so an upper-case spelling resolves only as far as
// the lookup below lets it — which is nowhere — and that is deliberate.
const HEX40 = /^[0-9a-fA-F]{40}$/;

// Answer with the standard error envelope: a JSON object with one message.
function deny(res: ServerResponse, status: number, message: string): void {
  sendError(res, status, message);
}

// Answer with a JSON payload at the given status.
function ok(res: ServerResponse, status: number, payload: unknown): void {
  sendJson(res, status, payload);
}

// Read the request body as a plain object, or fall back to an empty object
// when the caller sent a scalar, an array, or nothing at all.
function bodyAsRecord(env: Ctx): Record<string, unknown> {
  const payload = env.body;
  if (isPlainObject(payload)) {
    return payload as Record<string, unknown>;
  }
  return {};
}

// Take a raw commit object apart. The header and the message are divided
// by the first blank line; a message that ends with a newline loses that
// one trailing newline, and a commit with no blank line at all has an
// empty message. The author line carries the timestamp as its second to
// last whitespace-separated word, between the angle-bracketed address and
// the timezone offset.
function takeApartCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const splitAt = text.indexOf('\n\n');
  const header = splitAt === -1 ? text : text.slice(0, splitAt);
  let message = splitAt === -1 ? '' : text.slice(splitAt + 2);
  if (message.endsWith('\n')) {
    message = message.slice(0, -1);
  }
  let tree = '';
  let epoch = 0;
  const parents: string[] = [];
  for (const line of header.split('\n')) {
    if (line.startsWith('tree ')) {
      tree = line.slice(5).trim();
    } else if (line.startsWith('parent ')) {
      parents.push(line.slice(7).trim());
    } else if (line.startsWith('author ')) {
      const words = line.trim().split(/\s+/);
      const stampWord =
        words.length >= 2 ? (words[words.length - 2] as string).replace(/[<>]/g, '') : '';
      if (/^\d+$/.test(stampWord)) {
        epoch = Number.parseInt(stampWord, 10);
      }
    }
  }
  return { tree, parents, message, epoch };
}

// Build and store a commit object on top of a tree, with the given parents
// and message, stamped from the logical clock. The identity lines use the
// viewer's name and address, and the layout is one header line per field,
// a blank line, then the message. Returns the new commit's id.
function mintCommit(
  repo: Repo,
  vault: Forge,
  treeOid: string,
  parents: string[],
  message: string,
): string {
  const epochSeconds = Math.floor(Date.parse(vault.nowIso()) / 1000);
  const identity =
    VIEWER.name + ' <' + VIEWER.email + '> ' + String(epochSeconds) + ' +0000';
  const lines = ['tree ' + treeOid];
  for (const parent of parents) {
    lines.push('parent ' + parent);
  }
  lines.push('author ' + identity);
  lines.push('committer ' + identity);
  lines.push('');
  lines.push(message);
  const text = lines.join('\n') + '\n';
  return vault.putObject(repo, 'commit', Buffer.from(text, 'utf8'));
}

// Work out which commit a request is talking about. With no ref at all,
// the request means the default branch's tip. A 40-hex value is tried as
// a commit id first, and anything else is tried as a branch name. Every
// path that fails — an unborn branch, a missing object, an object that is
// not a commit — resolves to nothing, and the caller answers 404.
function resolveTip(repo: Repo, vault: Forge, ref?: string): string | null {
  if (ref === undefined) {
    const head = vault.headRef(repo);
    const oid = vault.refOid(repo, head);
    if (oid === '' || oid === ZERO_OID) {
      return null;
    }
    const obj = vault.getObject(repo, oid);
    if (obj === undefined || obj.type !== 'commit') {
      return null;
    }
    return oid;
  }
  if (HEX40.test(ref)) {
    const obj = vault.getObject(repo, ref);
    if (obj !== undefined && obj.type === 'commit') {
      return ref;
    }
  }
  const branchOid = vault.refOid(repo, 'refs/heads/' + ref);
  if (branchOid !== '' && branchOid !== ZERO_OID) {
    const obj = vault.getObject(repo, branchOid);
    if (obj !== undefined && obj.type === 'commit') {
      return branchOid;
    }
  }
  return null;
}

// Flatten the tree at a commit into path-to-entry form, or an empty map
// when the commit has no tree to flatten. The store does the flattening;
// this is a thin guard around it for commits that do not resolve.
function flatAtTip(repo: Repo, vault: Forge, commitOid: string): FlatTree {
  if (commitOid === '') {
    return new Map();
  }
  const treeOid = vault.commitTree(repo, commitOid);
  if (treeOid === '') {
    return new Map();
  }
  return new Map(vault.flattenTree(repo, treeOid));
}

// Walk down a tree following one path segment per level, and return the
// tree id of the directory the whole path names, or nothing when any
// level is missing or is a file instead of a directory.
function treeOidAtPath(
  repo: Repo,
  vault: Forge,
  root: string,
  parts: string[],
): string | null {
  let current = root;
  for (const part of parts) {
    const obj = vault.getObject(repo, current);
    if (obj === undefined || obj.type !== 'tree') {
      return null;
    }
    const entry = vault.parseTree(obj.data).find((candidate) => candidate.name === part);
    if (entry === undefined) {
      return null;
    }
    if (entry.mode !== '40000' && entry.mode !== '040000') {
      return null;
    }
    current = entry.oid;
  }
  return current;
}

// List one directory of a tree: each entry names its kind, its name, its
// full path, its id, and — for files — its size in bytes, plus the URL
// the contents route would answer it at. Directories report size zero.
// The entries come back sorted by name.
function listDirectory(
  repo: Repo,
  vault: Forge,
  treeOid: string,
  prefix: string,
  owner: string,
  name: string,
): unknown[] {
  const obj = vault.getObject(repo, treeOid);
  if (obj === undefined || obj.type !== 'tree') {
    return [];
  }
  const out = vault.parseTree(obj.data).map((entry) => {
    const isDir = entry.mode === '40000' || entry.mode === '040000';
    const full = prefix === '' ? entry.name : prefix + '/' + entry.name;
    let size = 0;
    if (isDir === false) {
      const blob = vault.getObject(repo, entry.oid);
      if (blob !== undefined && blob.type === 'blob') {
        size = blob.data.length;
      }
    }
    return {
      type: isDir ? 'dir' : 'file',
      name: entry.name,
      path: full,
      sha: entry.oid,
      size,
      url: '/api/v3/repos/' + owner + '/' + name + '/contents/' + full,
    };
  });
  out.sort((a, b) => {
    if (a.name < b.name) {
      return -1;
    }
    if (a.name > b.name) {
      return 1;
    }
    return 0;
  });
  return out;
}

// Answer GET .../commits. The history is walked depth-first from the tip:
// each commit is reported once, and its parents are pushed in reverse so
// the first parent is visited first. An unborn default branch answers
// with an empty list when no sha was asked for, and 404 when one was.
function handleCommits(env: Ctx, repo: Repo): true {
  const { res, forge: vault, query } = env;
  const rawSha = query.get('sha');
  const sha = rawSha !== null && rawSha.length > 0 ? rawSha : undefined;
  const tip = resolveTip(repo, vault, sha);
  if (tip === null) {
    if (sha === undefined) {
      ok(res, 200, []);
      return true;
    }
    deny(res, 404, 'Not Found');
    return true;
  }
  const seen = new Set<string>();
  const stack: string[] = [tip];
  const out: unknown[] = [];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) {
      continue;
    }
    seen.add(oid);
    const obj = vault.getObject(repo, oid);
    if (obj === undefined || obj.type !== 'commit') {
      continue;
    }
    const parsed = takeApartCommit(obj.data);
    const date = new Date(parsed.epoch * 1000).toISOString();
    out.push({
      sha: oid,
      commit: {
        message: parsed.message,
        author: { name: VIEWER.name, email: VIEWER.email, date },
        committer: { name: VIEWER.name, email: VIEWER.email, date },
      },
      author: VIEWER,
      committer: VIEWER,
      parents: parsed.parents.map((parent) => {
        return { sha: parent };
      }),
    });
    for (let i = parsed.parents.length - 1; i >= 0; i--) {
      stack.push(parsed.parents[i] as string);
    }
  }
  ok(res, 200, out);
  return true;
}

// Answer the contents routes for one repository: reading a file or a
// directory, writing a file, and deleting a file. Which branch a write
// lands on comes from the body's branch field, or the repository's
// default branch when the body names none.
function handleContents(
  env: Ctx,
  repo: Repo,
  owner: string,
  name: string,
  method: string,
): true {
  const { res, forge: vault, segments, query } = env;
  const pathParts = segments.slice(4);
  const path = pathParts.join('/');

  // ---- Reading: a file, a directory, or the repository root. ----
  if (method === 'GET') {
    const rawRef = query.get('ref');
    const ref = rawRef !== null && rawRef.length > 0 ? rawRef : undefined;
    const tip = resolveTip(repo, vault, ref);
    if (tip === null) {
      deny(res, 404, 'Not Found');
      return true;
    }
    const treeOid = vault.commitTree(repo, tip);
    if (treeOid === '') {
      deny(res, 404, 'Not Found');
      return true;
    }
    if (pathParts.length === 0) {
      ok(res, 200, listDirectory(repo, vault, treeOid, '', owner, name));
      return true;
    }
    const flat = vault.flattenTree(repo, treeOid);
    const fileEntry = flat.get(path);
    if (fileEntry !== undefined) {
      const blob = vault.getObject(repo, fileEntry.oid);
      const data = blob !== undefined && blob.type === 'blob' ? blob.data : Buffer.alloc(0);
      ok(res, 200, {
        type: 'file',
        encoding: 'base64',
        content: b64encode(data) + '\n',
        sha: fileEntry.oid,
        size: data.length,
        name: pathParts[pathParts.length - 1],
        path,
        url: '/api/v3/repos/' + owner + '/' + name + '/contents/' + path,
      });
      return true;
    }
    let isDir = false;
    for (const key of flat.keys()) {
      if (key.startsWith(path + '/')) {
        isDir = true;
        break;
      }
    }
    if (isDir) {
      const dirTreeOid = treeOidAtPath(repo, vault, treeOid, pathParts);
      if (dirTreeOid !== null) {
        ok(res, 200, listDirectory(repo, vault, dirTreeOid, path, owner, name));
        return true;
      }
    }
    deny(res, 404, 'Not Found');
    return true;
  }

  // ---- Writing and deleting share the branch and its tip. ----
  const body = bodyAsRecord(env);
  const branch =
    typeof body['branch'] === 'string' && (body['branch'] as string).length > 0
      ? (body['branch'] as string)
      : repo.default_branch;
  const tipRaw = vault.refOid(repo, 'refs/heads/' + branch);
  const tip = tipRaw !== '' && tipRaw !== ZERO_OID ? tipRaw : '';

  // ---- Writing: create a file, or replace one that is already there. ----
  if (method === 'PUT') {
    const content = body['content'];
    if (typeof content !== 'string') {
      deny(res, 422, 'content is required');
      return true;
    }
    let bytes: Buffer;
    try {
      bytes = b64decode(content);
    } catch {
      deny(res, 422, 'content is not valid base64');
      return true;
    }
    const message =
      typeof body['message'] === 'string' ? (body['message'] as string) : 'Update ' + path;
    const blobOid = hashObject('blob', bytes);
    const flat: FlatTree = tip !== '' ? flatAtTip(repo, vault, tip) : new Map();
    const existed = flat.has(path);
    if (typeof body['sha'] === 'string') {
      if (existed === false) {
        deny(res, 422, 'sha does not match');
        return true;
      }
      const current = flat.get(path);
      if (current === undefined || current.oid !== body['sha']) {
        deny(res, 409, 'sha does not match');
        return true;
      }
    }
    flat.set(path, { oid: blobOid, mode: '100644' });
    vault.putObject(repo, 'blob', bytes);
    const newTreeOid = vault.buildTreeFromFlat(repo, flat);
    const parents = tip !== '' ? [tip] : [];
    const commitOid = mintCommit(repo, vault, newTreeOid, parents, message);
    vault.setRef(repo, 'refs/heads/' + branch, commitOid);
    repo.updated_at = vault.nowIso();
    ok(res, existed ? 200 : 201, {
      content: {
        name: pathParts.length > 0 ? (pathParts[pathParts.length - 1] as string) : '',
        path,
        sha: blobOid,
        size: bytes.length,
        type: 'file',
        encoding: 'base64',
      },
      commit: { sha: commitOid, message },
    });
    return true;
  }

  // ---- Deleting: the file must exist, and the caller must name its id. ----
  const sha = body['sha'];
  if (typeof sha !== 'string') {
    deny(res, 422, 'sha is required');
    return true;
  }
  if (tip === '') {
    deny(res, 404, 'Not Found');
    return true;
  }
  const message =
    typeof body['message'] === 'string' ? (body['message'] as string) : 'Delete ' + path;
  const flat = flatAtTip(repo, vault, tip);
  const existing = flat.get(path);
  if (existing === undefined) {
    deny(res, 404, 'Not Found');
    return true;
  }
  if (existing.oid !== sha) {
    deny(res, 409, 'sha does not match');
    return true;
  }
  flat.delete(path);
  const newTreeOid = vault.buildTreeFromFlat(repo, flat);
  const commitOid = mintCommit(repo, vault, newTreeOid, [tip], message);
  vault.setRef(repo, 'refs/heads/' + branch, commitOid);
  repo.updated_at = vault.nowIso();
  ok(res, 200, { commit: { sha: commitOid, message } });
  return true;
}

// Answer the git ref routes: reading one branch or tag, and deleting a
// branch. A tag that points at a tag object reports its type as "tag";
// anything else, including a tag that points straight at a commit,
// reports "commit". Deleting answers with an empty 204.
function handleRefs(
  env: Ctx,
  repo: Repo,
  kind: 'heads' | 'tags',
  branch: string,
  method: string,
): true {
  const { res, forge: vault } = env;
  const full = 'refs/' + kind + '/' + branch;
  if (method === 'DELETE') {
    const removed = vault.deleteRef(repo, full);
    if (removed === false) {
      deny(res, 404, 'Not Found');
      return true;
    }
    res.writeHead(204, { 'x-github-request-id': 'req-0' });
    res.end();
    return true;
  }
  const oid = vault.refOid(repo, full);
  if (oid === '' || oid === ZERO_OID) {
    deny(res, 404, 'Not Found');
    return true;
  }
  const obj = vault.getObject(repo, oid);
  const type = kind === 'tags' && obj !== undefined && obj.type === 'tag' ? 'tag' : 'commit';
  ok(res, 200, { ref: full, object: { sha: oid, type } });
  return true;
}

// ---------------------------------------------------------------------------
// Entry point, called by the application server for every REST v3 request.
// Returns true when this module owned the request (matched or answered),
// false when the path is not ours and the next module should try.
// ---------------------------------------------------------------------------

export function contentRoutes(env: Ctx): boolean {
  const { req, res, forge: vault, segments } = env;

  // The gates run before the route shape is even looked at, so an
  // unauthenticated or malformed request is answered here no matter
  // which path it asked for.
  if (env.authed === false) {
    deny(res, 401, 'Requires authentication');
    return true;
  }
  if (env.bodyMalformed) {
    deny(res, 400, 'Invalid JSON body');
    return true;
  }

  const s = segments;
  if (s.length < 4 || s[0] !== 'repos') {
    return false;
  }
  const method = (req.method ?? 'GET').toUpperCase();
  const isCommits = s.length === 4 && s[3] === 'commits';
  const isContents = s[3] === 'contents';
  const isRefs =
    s.length === 7 && s[3] === 'git' && s[4] === 'refs' && (s[5] === 'heads' || s[5] === 'tags');
  if (isCommits && method !== 'GET') {
    return false;
  }
  if (isContents && method !== 'GET' && method !== 'PUT' && method !== 'DELETE') {
    return false;
  }
  if (isRefs) {
    const isHeads = s[5] === 'heads';
    const readable = method === 'GET';
    const deletableBranch = method === 'DELETE' && isHeads;
    if (readable === false && deletableBranch === false) {
      return false;
    }
  }
  if (isCommits === false && isContents === false && isRefs === false) {
    return false;
  }

  const owner = s[1] as string;
  const name = s[2] as string;
  if (owner !== VIEWER.login) {
    deny(res, 404, 'Not Found');
    return true;
  }
  const repo = vault.getRepo(owner, name);
  if (repo === undefined) {
    deny(res, 404, 'Not Found');
    return true;
  }
  if (isCommits) {
    return handleCommits(env, repo);
  }
  if (isContents) {
    return handleContents(env, repo, owner, name, method);
  }
  const kind = s[5] === 'tags' ? 'tags' : 'heads';
  return handleRefs(env, repo, kind, s[6] as string, method);
}
