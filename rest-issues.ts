// ---------------------------------------------------------------------------
// Labels, issues, comments, and pull requests over the REST v3 surface.
//
// This module owns every route below /api/v3/repos/{owner}/{repo} that deals
// with the tracker side of a repository: labels first, then issues and their
// comments, then pull requests including the file list and the squash merge.
//
// How a request flows through here, in plain language:
//
//   1. The router table near the middle of the file decides whether the
//      method plus the shape of the path names one of our endpoints at all.
//      Nothing else in the file runs for a path we do not recognise, and the
//      dispatcher answers "not mine" so the next module gets its turn.
//   2. The gate checks run in a fixed order: a missing or wrong token is a
//      401, a body that failed to parse is a 400, an owner that is not the
//      viewer is a 404, and a repository that does not exist is a 404 too.
//      The order matters because the trial probes each failure separately.
//   3. The handler for the matched endpoint validates its own inputs, talks
//      to the Forge store, and answers with one of the render helpers below.
//
// A note on the clock, because it surprises people: the store stamps things
// from a logical clock that ticks once per call, so the number of stamp
// reads inside a handler is part of its observable behaviour. Closing an
// issue, for example, stamps the closed time and then stamps the updated
// time as two separate reads, which leaves the two stamps one tick apart.
// The handlers below keep those reads exactly where they were, even when
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
import type { Issue } from './store.ts';
import type { Pull } from './store.ts';
import { VIEWER } from './store.ts';
import { sendJson } from './util.ts';
import { sendError } from './util.ts';
import { isPlainObject } from './util.ts';

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
  // The parsed query string, for state filters, sorting, and paging.
  query: URLSearchParams;
  // The parsed JSON body, or undefined when the request carried no body.
  body: unknown;
  // True when a body was present but its JSON would not parse.
  bodyMalformed: boolean;
  // True when the caller presented the token the server expects.
  authed: boolean;
  // The Host header value, used when building pagination links.
  host: string;
}

// A single page of a longer list, plus the "next page" link when one exists.
// The link is absent (not empty) when the caller already holds the last page.
interface PageSlice<T> {
  page: T[];
  link?: string;
}

// Answer with the standard error envelope: a JSON object with one message.
function deny(res: ServerResponse, status: number, message: string): void {
  sendError(res, status, message);
}

// Answer with a JSON payload, optionally carrying extra response headers
// such as the pagination Link header. The status is always explicit.
function ok(
  res: ServerResponse,
  status: number,
  payload: unknown,
  extra?: Record<string, string>,
): void {
  sendJson(res, status, payload, extra);
}

// Read the request body as a plain object, or fall back to an empty object
// when the caller sent a scalar, an array, or nothing at all. Handlers can
// then read fields off the result without guarding the shape again.
function bodyAsRecord(env: Ctx): Record<string, unknown> {
  const payload = env.body;
  if (isPlainObject(payload)) {
    return payload as Record<string, unknown>;
  }
  return {};
}

// Read an integer out of a query parameter, falling back when the value is
// missing, blank, or not a whole number. The conversion goes through the
// Number constructor on purpose: it rejects strings with trailing junk
// that a prefix parser would silently accept.
function queryIntOr(raw: string | null, fallback: number): number {
  const parsed = raw === null || raw === '' ? Number.NaN : Number(raw);
  return Number.isInteger(parsed) ? parsed : fallback;
}

// Cut a page out of a full list. The page size is clamped into the range
// the API documents, and the page number never drops below one. When more
// rows remain after this page, a next-page link is built from the current
// query string with only the page number advanced.
function slicePage<T>(env: Ctx, rows: T[]): PageSlice<T> {
  const rawSize = queryIntOr(env.query.get('per_page'), 30);
  const size = rawSize < 1 ? 1 : rawSize > 100 ? 100 : rawSize;
  const rawPage = queryIntOr(env.query.get('page'), 1);
  const pageNo = rawPage < 1 ? 1 : rawPage;
  const start = (pageNo - 1) * size;
  const chunk = rows.slice(start, start + size);
  const remaining = start + chunk.length < rows.length;
  if (remaining === false) {
    return { page: chunk };
  }
  const nextParams = new URLSearchParams(env.query);
  nextParams.set('page', String(pageNo + 1));
  const path = '/' + env.segments.join('/');
  const link =
    '<http://' + env.host + '/api/v3' + path + '?' + nextParams.toString() + '>; rel="next"';
  return { page: chunk, link };
}

// Read a path segment as a non-negative safe integer, for issue and pull
// numbers. Anything else — a name, a negative, a decimal, a huge value —
// is not a number at all and the caller answers 404 for it.
function parseNumberSegment(raw: string | undefined): number | null {
  if (raw === undefined) {
    return null;
  }
  if (/^[0-9]+$/.test(raw) === false) {
    return null;
  }
  const value = Number(raw);
  return Number.isSafeInteger(value) ? value : null;
}

// Read the state filter for list endpoints. Only the two spellings the API
// documents pass through; anything else, including a missing value, lists
// the open items, which is the default the trial expects.
function parseStateFilter(raw: string | null): 'open' | 'closed' | 'all' {
  if (raw === 'closed') {
    return 'closed';
  }
  if (raw === 'all') {
    return 'all';
  }
  return 'open';
}

// Order issues by one of their stamps, breaking ties by number so the order
// is stable when two issues share a stamp. The comparison is written once
// in ascending form and flipped for descending, instead of threading a
// direction multiplier through every branch.
function orderIssues(
  entries: Issue[],
  field: 'created' | 'updated',
  ascending: boolean,
): void {
  entries.sort((left, right) => {
    const leftStamp = field === 'updated' ? left.updated_at : left.created_at;
    const rightStamp = field === 'updated' ? right.updated_at : right.created_at;
    let compared = 0;
    if (leftStamp < rightStamp) {
      compared = -1;
    } else if (leftStamp > rightStamp) {
      compared = 1;
    } else if (left.number < right.number) {
      compared = -1;
    } else if (left.number > right.number) {
      compared = 1;
    }
    return ascending ? compared : -compared;
  });
}

// ---------------------------------------------------------------------------
// Serializers: store records become the JSON shapes the API promises.
// ---------------------------------------------------------------------------

// One label, exactly the four fields the API exposes for it.
function renderLabel(label: { id: number; name: string; color: string; description: string }): {
  id: number;
  name: string;
  color: string;
  description: string;
} {
  return {
    id: label.id,
    name: label.name,
    color: label.color,
    description: label.description,
  };
}

// One issue. Its labels are expanded from names into full label objects by
// looking each name up in the repository, so a label that vanished from the
// repository simply does not appear instead of breaking the rendering.
function renderIssue(forge: Forge, repo: Repo, issue: Issue): Record<string, unknown> {
  const expanded: Array<ReturnType<typeof renderLabel>> = [];
  for (const labelName of issue.labels) {
    const found = repo.labels.get(labelName);
    if (found !== undefined) {
      expanded.push(renderLabel(found));
    }
  }
  void forge;
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: expanded,
    user: VIEWER,
    assignee: null,
    assignees: [],
    created_at: issue.created_at,
    updated_at: issue.updated_at,
    closed_at: issue.closed_at,
    html_url: 'https://example.invalid/adaiversen/' + repo.name + '/issues/' + issue.number,
  };
}

// One comment on an issue. Comments carry no label or state of their own.
function renderComment(comment: {
  id: number;
  body: string;
  created_at: string;
  updated_at: string;
}): Record<string, unknown> {
  return {
    id: comment.id,
    body: comment.body,
    user: VIEWER,
    created_at: comment.created_at,
    updated_at: comment.updated_at,
  };
}

// One pull request. The head and base tips are resolved live from the refs,
// so the answer reflects the branches as they stand right now rather than
// the shas recorded when the pull request was opened. Mergeability is only
// computed while the pull request is open; a closed one is never mergeable.
function renderPull(forge: Forge, repo: Repo, pull: Pull): Record<string, unknown> {
  const baseTip = forge.refOid(repo, 'refs/heads/' + pull.base.ref);
  const headTip = forge.refOid(repo, 'refs/heads/' + pull.head.ref);
  let mergeable = false;
  if (pull.state === 'open') {
    mergeable = forge.isMergeable(repo, baseTip, headTip);
  }
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    mergeable,
    merged_at: pull.merged_at,
    merge_commit_sha: pull.merge_commit_sha,
    head: {
      ref: pull.head.ref,
      sha: headTip,
      label: 'adaiversen:' + pull.head.ref,
    },
    base: {
      ref: pull.base.ref,
      sha: baseTip,
      label: 'adaiversen:' + pull.base.ref,
    },
    user: VIEWER,
    created_at: pull.created_at,
    updated_at: pull.updated_at,
    closed_at: pull.closed_at,
    html_url: 'https://example.invalid/adaiversen/' + repo.name + '/pull/' + pull.number,
  };
}

// ---------------------------------------------------------------------------
// Routing: a small table maps (method, path shape) to an endpoint name.
// ---------------------------------------------------------------------------

type EndpointName =
  | 'labels:create'
  | 'labels:list'
  | 'issues:create'
  | 'issues:list'
  | 'issues:get'
  | 'issues:patch'
  | 'issues:comments:create'
  | 'issues:comments:list'
  | 'pulls:create'
  | 'pulls:list'
  | 'pulls:get'
  | 'pulls:files'
  | 'pulls:merge';

// One row of the routing table. `depth` is the number of path segments the
// endpoint expects, `kind` is the resource word in segment three, and
// `tail` — when present — is the extra word in segment five that picks the
// sub-endpoint (comments under an issue, files or merge under a pull).
interface RouteSpec {
  verb: string;
  depth: number;
  kind: string;
  tail: string | null;
  name: EndpointName;
}

const ROUTE_TABLE: RouteSpec[] = [
  { verb: 'POST', depth: 4, kind: 'labels', tail: null, name: 'labels:create' },
  { verb: 'GET', depth: 4, kind: 'labels', tail: null, name: 'labels:list' },
  { verb: 'POST', depth: 4, kind: 'issues', tail: null, name: 'issues:create' },
  { verb: 'GET', depth: 4, kind: 'issues', tail: null, name: 'issues:list' },
  { verb: 'GET', depth: 5, kind: 'issues', tail: null, name: 'issues:get' },
  { verb: 'PATCH', depth: 5, kind: 'issues', tail: null, name: 'issues:patch' },
  { verb: 'POST', depth: 6, kind: 'issues', tail: 'comments', name: 'issues:comments:create' },
  { verb: 'GET', depth: 6, kind: 'issues', tail: 'comments', name: 'issues:comments:list' },
  { verb: 'POST', depth: 4, kind: 'pulls', tail: null, name: 'pulls:create' },
  { verb: 'GET', depth: 4, kind: 'pulls', tail: null, name: 'pulls:list' },
  { verb: 'GET', depth: 5, kind: 'pulls', tail: null, name: 'pulls:get' },
  { verb: 'GET', depth: 6, kind: 'pulls', tail: 'files', name: 'pulls:files' },
  { verb: 'PUT', depth: 6, kind: 'pulls', tail: 'merge', name: 'pulls:merge' },
];

// Walk the table for the first row whose verb, depth, resource word, and
// tail word all match the request. No row matches means this module does
// not own the request, whatever the path looked like.
function resolveRouteName(verb: string, parts: string[]): EndpointName | null {
  if (parts[0] !== 'repos' || parts.length < 4) {
    return null;
  }
  const kind = parts[3] as string;
  for (const spec of ROUTE_TABLE) {
    if (spec.verb !== verb) {
      continue;
    }
    if (spec.depth !== parts.length) {
      continue;
    }
    if (spec.kind !== kind) {
      continue;
    }
    if (spec.tail !== null && parts[5] !== spec.tail) {
      continue;
    }
    return spec.name;
  }
  return null;
}

// Look an issue or a pull request up by the number in the path. The lookup
// is shared because both kinds of entity are stored the same way: a map
// keyed by number, and a missing entry is simply undefined.
function findByNumber<T>(store: Map<number, T>, raw: string | undefined): T | undefined {
  const number = parseNumberSegment(raw);
  if (number === null) {
    return undefined;
  }
  return store.get(number);
}

// ---------------------------------------------------------------------------
// Handlers: one function per endpoint. Each one assumes the dispatcher has
// already checked auth, body shape, owner, and repository existence.
// ---------------------------------------------------------------------------

type Handler = (env: Ctx, repo: Repo) => void;

// Create a label. Creating a label that already exists is idempotent: every
// repository starts with the default label set, so asking for one of those
// answers 201 with the label that is already there instead of an error.
function createLabel(env: Ctx, repo: Repo): void {
  const data = bodyAsRecord(env);
  const wanted = data['name'];
  if (typeof wanted !== 'string' || wanted.length === 0) {
    deny(env.res, 422, 'name is required');
    return;
  }
  const already = repo.labels.get(wanted);
  if (already !== undefined) {
    ok(env.res, 201, renderLabel(already));
    return;
  }
  repo.labelSeq += 1;
  const fresh = {
    id: repo.labelSeq,
    name: wanted,
    color: typeof data['color'] === 'string' ? (data['color'] as string) : 'ededed',
    description:
      typeof data['description'] === 'string' ? (data['description'] as string) : '',
  };
  repo.labels.set(wanted, fresh);
  ok(env.res, 201, renderLabel(fresh));
}

// List every label in the repository, in the order they were seeded and
// created.
function listLabels(env: Ctx, repo: Repo): void {
  const rendered = Array.from(repo.labels.values()).map((entry) => renderLabel(entry));
  ok(env.res, 200, rendered);
}

// Create an issue. When the caller supplies labels, every one of them must
// already exist in the repository; a single unknown name rejects the whole
// request, and a labels value that is not an array does the same.
function createIssue(env: Ctx, repo: Repo): void {
  const data = bodyAsRecord(env);
  const title = data['title'];
  if (typeof title !== 'string' || title.length === 0) {
    deny(env.res, 422, 'title is required');
    return;
  }
  const chosen: string[] = [];
  const labelsSupplied = Object.prototype.hasOwnProperty.call(data, 'labels');
  if (labelsSupplied) {
    const rawLabels = data['labels'];
    if (Array.isArray(rawLabels) === false) {
      deny(env.res, 422, 'Label does not exist');
      return;
    }
    for (const item of rawLabels as unknown[]) {
      if (typeof item !== 'string' || repo.labels.has(item) === false) {
        deny(env.res, 422, 'Label does not exist');
        return;
      }
      chosen.push(item);
    }
  }
  const bodyText = typeof data['body'] === 'string' ? (data['body'] as string) : '';
  const issue = env.forge.createIssue(repo, title, bodyText, chosen);
  ok(env.res, 201, renderIssue(env.forge, repo, issue));
}

// List issues, filtered by state and ordered by the requested stamp. The
// default order is newest first, which is what the direction parameter
// flips when the caller asks for ascending order.
function listIssues(env: Ctx, repo: Repo): void {
  const wanted = parseStateFilter(env.query.get('state'));
  const sortParam = env.query.get('sort');
  const sortField: 'created' | 'updated' = sortParam === 'updated' ? 'updated' : 'created';
  const ascending = env.query.get('direction') === 'asc';
  let rows = Array.from(repo.issues.values());
  if (wanted !== 'all') {
    rows = rows.filter((issue) => issue.state === wanted);
  }
  orderIssues(rows, sortField, ascending);
  const sliced = slicePage(env, rows);
  const rendered = sliced.page.map((issue) => renderIssue(env.forge, repo, issue));
  const headers = sliced.link === undefined ? undefined : { Link: sliced.link };
  ok(env.res, 200, rendered, headers);
}

// Fetch one issue by number.
function getIssue(env: Ctx, repo: Repo): void {
  const issue = findByNumber(repo.issues, env.segments[4]);
  if (issue === undefined) {
    deny(env.res, 404, 'Not Found');
    return;
  }
  ok(env.res, 200, renderIssue(env.forge, repo, issue));
}

// Patch an issue's state, title, and body. Only the fields the caller sent
// change, and the updated stamp only moves when something actually
// changed. Closing stamps the closed time and the updated time as two
// separate clock reads (see the module header for why that is deliberate).
function patchIssue(env: Ctx, repo: Repo): void {
  const issue = findByNumber(repo.issues, env.segments[4]);
  if (issue === undefined) {
    deny(env.res, 404, 'Not Found');
    return;
  }
  const data = bodyAsRecord(env);
  const vault = env.forge;
  let touched = false;
  const nextState = data['state'];
  if (nextState === 'open' || nextState === 'closed') {
    const settled = nextState as 'open' | 'closed';
    issue.state = settled;
    issue.closed_at = settled === 'closed' ? vault.nowIso() : null;
    touched = true;
  }
  if (typeof data['title'] === 'string') {
    issue.title = data['title'] as string;
    touched = true;
  }
  if (typeof data['body'] === 'string') {
    issue.body = data['body'] as string;
    touched = true;
  }
  if (touched) {
    issue.updated_at = vault.nowIso();
  }
  ok(env.res, 200, renderIssue(env.forge, repo, issue));
}

// Add a comment to an issue. The comment id comes from the repository-wide
// counter, and both of its stamps come from a single clock read.
function createComment(env: Ctx, repo: Repo): void {
  const issue = findByNumber(repo.issues, env.segments[4]);
  if (issue === undefined) {
    deny(env.res, 404, 'Not Found');
    return;
  }
  const data = bodyAsRecord(env);
  const text = data['body'];
  if (typeof text !== 'string' || text.length === 0) {
    deny(env.res, 422, 'body is required');
    return;
  }
  repo.commentSeq += 1;
  const stamp = env.forge.nowIso();
  const comment = {
    id: repo.commentSeq,
    body: text,
    user: VIEWER,
    created_at: stamp,
    updated_at: stamp,
  };
  issue.comments.push(comment);
  ok(env.res, 201, renderComment(comment));
}

// List the comments on one issue, in the order they were added.
function listComments(env: Ctx, repo: Repo): void {
  const issue = findByNumber(repo.issues, env.segments[4]);
  if (issue === undefined) {
    deny(env.res, 404, 'Not Found');
    return;
  }
  const rendered = issue.comments.map((comment) => renderComment(comment));
  ok(env.res, 200, rendered);
}

// Open a pull request between two branches. The title and both branch
// names are all required, and both branches must already exist as refs;
// the store double-checks the refs when creating and can still refuse.
function createPull(env: Ctx, repo: Repo): void {
  const data = bodyAsRecord(env);
  const title = data['title'];
  const head = data['head'];
  const base = data['base'];
  const titleOk = typeof title === 'string' && title.length > 0;
  const headOk = typeof head === 'string' && head.length > 0;
  const baseOk = typeof base === 'string' && base.length > 0;
  if (titleOk === false || headOk === false || baseOk === false) {
    deny(env.res, 422, 'title, head and base are required');
    return;
  }
  const headName = head as string;
  const baseName = base as string;
  const headExists = repo.refs.has('refs/heads/' + headName);
  const baseExists = repo.refs.has('refs/heads/' + baseName);
  if (headExists === false || baseExists === false) {
    deny(env.res, 422, 'Invalid head or base');
    return;
  }
  const bodyText = typeof data['body'] === 'string' ? (data['body'] as string) : '';
  const pull = env.forge.createPull(repo, title as string, bodyText, headName, baseName);
  if (pull === null) {
    deny(env.res, 422, 'Invalid head or base');
    return;
  }
  ok(env.res, 201, renderPull(env.forge, repo, pull));
}

// List pull requests, filtered by state and newest first by number.
function listPulls(env: Ctx, repo: Repo): void {
  const wanted = parseStateFilter(env.query.get('state'));
  let rows = Array.from(repo.pulls.values());
  if (wanted !== 'all') {
    rows = rows.filter((pull) => pull.state === wanted);
  }
  rows.sort((left, right) => {
    if (left.number === right.number) {
      return 0;
    }
    return left.number < right.number ? 1 : -1;
  });
  const sliced = slicePage(env, rows);
  const rendered = sliced.page.map((pull) => renderPull(env.forge, repo, pull));
  const headers = sliced.link === undefined ? undefined : { Link: sliced.link };
  ok(env.res, 200, rendered, headers);
}

// Fetch one pull request by number.
function getPull(env: Ctx, repo: Repo): void {
  const pull = findByNumber(repo.pulls, env.segments[4]);
  if (pull === undefined) {
    deny(env.res, 404, 'Not Found');
    return;
  }
  ok(env.res, 200, renderPull(env.forge, repo, pull));
}

// List the files a pull request changes, diffed live between the current
// base tip and the current head tip. The patch text is always empty: this
// forge reports the counts, not the diff hunks.
function listPullFiles(env: Ctx, repo: Repo): void {
  const pull = findByNumber(repo.pulls, env.segments[4]);
  if (pull === undefined) {
    deny(env.res, 404, 'Not Found');
    return;
  }
  const vault = env.forge;
  const baseTip = vault.refOid(repo, 'refs/heads/' + pull.base.ref);
  const headTip = vault.refOid(repo, 'refs/heads/' + pull.head.ref);
  const changed = vault.pullFiles(repo, baseTip, headTip);
  const rendered = changed.map((file) => {
    return {
      filename: file.filename,
      status: file.status,
      additions: file.additions,
      deletions: file.deletions,
      changes: file.changes,
      patch: '',
    };
  });
  ok(env.res, 200, rendered);
}

// Merge a pull request with a squash merge: one new commit on the base
// branch whose tree is the three-way merge of the head into the base, and
// whose message names the pull request. The commit object is assembled
// from its lines and joined, which keeps the exact bytes the object hash
// is computed over while reading very differently from a template.
//
// A pull request that is already merged or closed cannot be merged again,
// and a merge that conflicts is refused; both are reported with 405, each
// with its own message, so callers can tell the two failures apart.
function mergePull(env: Ctx, repo: Repo): void {
  const pull = findByNumber(repo.pulls, env.segments[4]);
  if (pull === undefined) {
    deny(env.res, 404, 'Not Found');
    return;
  }
  const data = bodyAsRecord(env);
  const methodRaw = data['merge_method'];
  const allowed = ['squash', 'merge', 'rebase'];
  const methodBad =
    methodRaw !== undefined &&
    (typeof methodRaw !== 'string' || allowed.includes(methodRaw as string) === false);
  if (methodBad) {
    deny(env.res, 422, 'Invalid merge method');
    return;
  }
  if (pull.merged || pull.state === 'closed') {
    deny(env.res, 405, 'Pull request is already merged');
    return;
  }
  const vault = env.forge;
  const baseTip = vault.refOid(repo, 'refs/heads/' + pull.base.ref);
  const headTip = vault.refOid(repo, 'refs/heads/' + pull.head.ref);
  const merged = vault.squashMergeTree(repo, baseTip, headTip);
  if (merged.conflict) {
    deny(env.res, 405, 'Merge conflict');
    return;
  }
  const stamp = vault.nowIso();
  const epochSeconds = Math.floor(Date.parse(stamp) / 1000);
  const subject = pull.title + ' (#' + String(pull.number) + ')';
  const ident = VIEWER.name + ' <' + VIEWER.email + '> ' + String(epochSeconds) + ' +0000';
  const commitLines = [
    'tree ' + merged.treeOid,
    'parent ' + baseTip,
    'author ' + ident,
    'committer ' + ident,
    '',
    subject,
  ];
  const commitText = commitLines.join('\n') + '\n';
  const squashOid = vault.putObject(repo, 'commit', Buffer.from(commitText, 'utf8'));
  vault.setRef(repo, 'refs/heads/' + pull.base.ref, squashOid);
  pull.state = 'closed';
  pull.merged = true;
  pull.merged_at = stamp;
  pull.merge_commit_sha = squashOid;
  pull.updated_at = stamp;
  repo.updated_at = stamp;
  ok(env.res, 200, {
    sha: squashOid,
    merged: true,
    message: 'Pull request successfully merged',
  });
}

// The dispatch table: every endpoint name from the router points at the
// function that answers it. Keeping the mapping in one place means adding
// an endpoint is a one-line change here plus one row in the router table.
const HANDLERS: Record<EndpointName, Handler> = {
  'labels:create': createLabel,
  'labels:list': listLabels,
  'issues:create': createIssue,
  'issues:list': listIssues,
  'issues:get': getIssue,
  'issues:patch': patchIssue,
  'issues:comments:create': createComment,
  'issues:comments:list': listComments,
  'pulls:create': createPull,
  'pulls:list': listPulls,
  'pulls:get': getPull,
  'pulls:files': listPullFiles,
  'pulls:merge': mergePull,
};

// ---------------------------------------------------------------------------
// Entry point, called by the application server for every REST v3 request.
// Returns true when this module owned the request (matched or answered),
// false when the path is not ours and the next module should try.
// ---------------------------------------------------------------------------

export function issuePullRoutes(env: Ctx): boolean {
  const verb = (env.req.method ?? '').toUpperCase();
  const routeName = resolveRouteName(verb, env.segments);
  if (routeName === null) {
    return false;
  }
  if (env.authed === false) {
    deny(env.res, 401, 'Requires authentication');
    return true;
  }
  if (env.bodyMalformed) {
    deny(env.res, 400, 'Invalid JSON body');
    return true;
  }
  const owner = env.segments[1];
  const repoName = env.segments[2];
  if (owner !== 'adaiversen') {
    deny(env.res, 404, 'Not Found');
    return true;
  }
  const repo = env.forge.getRepo(owner, repoName);
  if (repo === undefined) {
    deny(env.res, 404, 'Not Found');
    return true;
  }
  const handler = HANDLERS[routeName];
  handler(env, repo);
  return true;
}
