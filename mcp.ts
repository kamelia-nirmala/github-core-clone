// ---------------------------------------------------------------------------
// MCP over Streamable HTTP: the agent surface of the forge.
//
// This module speaks JSON-RPC 2.0 on a single endpoint, POST /mcp. Clients
// here are agents rather than browsers, so the shape of the conversation
// is different from REST: a client first asks what protocol versions the
// server speaks, negotiates one, lists the tools, and then calls them one
// at a time, each call carrying its arguments as a JSON object. The server
// answers every call with either a result or a JSON-RPC error, and it
// always stamps its replies with the same session id.
//
// The tools are deliberately a small set — create and read repositories,
// create and list issues, create and merge pull requests — and each tool
// accepts several spellings for its arguments, because different agent
// clients name the same idea differently ("repo" versus "repository",
// "pull_number" versus "pr_number", and so on). The alias tables near the
// bottom of the first half of this file are the whole story there: the
// first alias that is present with a usable value wins.
//
// A note on the clock, because it surprises people: the store stamps
// things from a logical clock that ticks once per call, so the number of
// stamp reads inside a handler is part of its observable behaviour. The
// merge tool, for example, reads the clock four separate times — once for
// the commit's timestamp and once each for the merged, updated, and
// repository stamps — and the handlers below keep those reads exactly
// where they were, even when sharing one read would look tidier.
//
// Nothing in this file touches the network or the disk. All state lives in
// the Forge instance handed in through the request context.
// ---------------------------------------------------------------------------

import type { IncomingMessage } from 'node:http';
import type { ServerResponse } from 'node:http';
import type { Forge } from './store.ts';
import type { Repo } from './store.ts';
import type { Issue } from './store.ts';
import type { Pull } from './store.ts';
import { VIEWER } from './store.ts';
import { readBody } from './util.ts';
import { parseJsonBody } from './util.ts';
import { isPlainObject } from './util.ts';

// Everything the MCP handler needs for one request.
export interface McpRequest {
  req: IncomingMessage;
  res: ServerResponse;
  forge: Forge;
  authed: boolean;
}

// The session id stamped on every reply. There is exactly one session in
// this forge, and it never changes, so clients can rely on the value.
const SESSION_ID = 'session-1';

// The protocol versions this server accepts, newest first. They are spelled
// out as one string and split apart so the list stays easy to scan.
const SUPPORTED_VERSIONS: string[] = '2026-07-28 2025-11-25 2025-06-18 2025-03-26 2024-11-05'.split(
  ' ',
);

// The version a client gets when it offers nothing the server recognises:
// the newest one in the list above.
const LATEST_VERSION: string = SUPPORTED_VERSIONS[0] as string;

// The name and version the server reports about itself in handshakes.
const SERVER_NAME = 'forge';
const SERVER_VERSION = '1.0.0';

// ---------------------------------------------------------------------------
// Tool catalogue: names, descriptions, and argument schemas.
// ---------------------------------------------------------------------------

// One property of a tool's argument object.
type ToolProperty = {
  type: 'string' | 'boolean' | 'integer';
  description: string;
};

// The schema of a tool's argument object: an object with named properties,
// some of which are required.
type ToolSchema = {
  type: 'object';
  properties: Record<string, ToolProperty>;
  required: string[];
};

// One tool, as the tools/list answer describes it.
type ToolDef = {
  name: string;
  description: string;
  inputSchema: ToolSchema;
};

// A compact row for the catalogue table: each property is a triple of its
// name, its JSON type, and the one-line description clients see.
interface ToolSpec {
  name: string;
  description: string;
  props: Array<[string, ToolProperty['type'], string]>;
  required: string[];
}

// The whole catalogue as data. Turning rows into the wire shape happens in
// buildToolList below, so adding a tool is a matter of adding one row here
// plus one handler in the dispatch table further down.
const TOOL_SPECS: ToolSpec[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the viewer.',
    props: [
      ['name', 'string', 'The repository name'],
      ['description', 'string', 'A short description of the repository'],
      ['private', 'boolean', 'Whether the repository is private'],
    ],
    required: ['name'],
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    props: [
      ['owner', 'string', 'The repository owner'],
      ['repo', 'string', 'The repository name'],
    ],
    required: ['owner', 'repo'],
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    props: [
      ['owner', 'string', 'The repository owner'],
      ['repo', 'string', 'The repository name'],
      ['title', 'string', 'The issue title'],
      ['body', 'string', 'The issue body'],
    ],
    required: ['owner', 'repo', 'title'],
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    props: [
      ['owner', 'string', 'The repository owner'],
      ['repo', 'string', 'The repository name'],
      ['state', 'string', 'open, closed or all'],
    ],
    required: ['owner', 'repo'],
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request.',
    props: [
      ['owner', 'string', 'The repository owner'],
      ['repo', 'string', 'The repository name'],
      ['title', 'string', 'The pull request title'],
      ['body', 'string', 'The pull request body'],
      ['head', 'string', 'The head branch name'],
      ['base', 'string', 'The base branch name'],
    ],
    required: ['owner', 'repo', 'title', 'head', 'base'],
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request with a squash merge.',
    props: [
      ['owner', 'string', 'The repository owner'],
      ['repo', 'string', 'The repository name'],
      ['pull_number', 'integer', 'The pull request number'],
    ],
    required: ['owner', 'repo', 'pull_number'],
  },
];

// Expand the catalogue table into the tool definitions the wire format
// wants, one definition per row.
function buildToolList(): ToolDef[] {
  return TOOL_SPECS.map((spec) => {
    const properties: Record<string, ToolProperty> = {};
    for (const [propName, propType, propText] of spec.props) {
      properties[propName] = { type: propType, description: propText };
    }
    return {
      name: spec.name,
      description: spec.description,
      inputSchema: { type: 'object', properties, required: [...spec.required] },
    };
  });
}

// ---------------------------------------------------------------------------
// Reply writers: every answer leaves through one of these, so the session
// header and the JSON-RPC envelope stay uniform.
// ---------------------------------------------------------------------------

// Write a JSON payload with a status, always carrying the session id.
function writeJson(res: ServerResponse, status: number, payload: unknown): void {
  const text = JSON.stringify(payload);
  res.statusCode = status;
  res.setHeader('Content-Type', 'application/json');
  res.setHeader('mcp-session-id', SESSION_ID);
  res.end(text);
}

// Answer with a status and no body at all, for notifications the protocol
// says to acknowledge without a result.
function writeEmpty(res: ServerResponse, status: number): void {
  res.statusCode = status;
  res.setHeader('mcp-session-id', SESSION_ID);
  res.end();
}

// Answer a request with a result value, wrapped in the JSON-RPC envelope.
function writeResult(res: ServerResponse, id: unknown, result: unknown): void {
  writeJson(res, 200, { jsonrpc: '2.0', id, result });
}

// Answer a request with a JSON-RPC error, still over HTTP 200, which is
// how the protocol reports errors that belong to the request itself.
function writeRpcError(res: ServerResponse, id: unknown, code: number, message: string): void {
  writeJson(res, 200, { jsonrpc: '2.0', id, error: { code, message } });
}

// Answer a request that never became a valid JSON-RPC message. These go
// out over an HTTP error status with a null id, because there is no
// request id worth echoing.
function writeProtocolError(res: ServerResponse, status: number, code: number, message: string): void {
  writeJson(res, status, { jsonrpc: '2.0', id: null, error: { code, message } });
}

// ---------------------------------------------------------------------------
// Reading the request: ids, parameters, and argument aliases.
// ---------------------------------------------------------------------------

// True when the message carries a request id that is not null. A message
// without one is a notification, and notifications never get a reply body.
function hasRequestId(body: Record<string, unknown>): boolean {
  return (
    Object.prototype.hasOwnProperty.call(body, 'id') === true && body['id'] !== null
  );
}

// The request id to echo back, or null when the message carried none. The
// value passes through untouched, whatever its JSON type.
function requestIdOf(body: Record<string, unknown>): unknown {
  return Object.prototype.hasOwnProperty.call(body, 'id') ? body['id'] : null;
}

// Treat a value as a record when it is a plain object, and as an empty
// record otherwise, so callers can read fields without guarding the shape.
function toRecord(value: unknown): Record<string, unknown> {
  return isPlainObject(value) ? (value as Record<string, unknown>) : {};
}

// Every spelling a client might use for each argument idea, in the order
// they are tried. The first spelling that is present with a usable value
// wins, which is what makes the tools forgiving about argument names.
const ALIASES: Record<string, string[]> = {
  owner: ['owner', 'owner_login', 'org', 'user', 'login'],
  repo: ['repo', 'repository', 'repository_name', 'repo_name'],
  title: ['title', 'issue_title', 'pr_title', 'subject'],
  body: ['body', 'issue_body', 'description', 'content', 'text'],
  head: ['head', 'head_ref', 'head_branch', 'source_branch', 'from_branch', 'branch'],
  base: ['base', 'base_ref', 'base_branch', 'target_branch', 'into_branch'],
  pullNumber: ['pull_number', 'pull_request_number', 'pr_number', 'number'],
  name: ['name', 'repo', 'repository'],
  description: ['description', 'about', 'summary'],
  isPrivate: ['private', 'is_private'],
  state: ['state', 'status'],
};

// Find the first alias that is present and holds a string, and return that
// string. A present-but-wrongly-typed value does not count, and the search
// moves on to the next spelling.
function pickString(args: Record<string, unknown>, keys: string[]): string | undefined {
  const hit = keys.find(
    (key) =>
      Object.prototype.hasOwnProperty.call(args, key) === true &&
      typeof args[key] === 'string',
  );
  return hit === undefined ? undefined : (args[hit] as string);
}

// Find the first alias that holds a whole number, accepting both a JSON
// number and a string of digits (with surrounding whitespace allowed).
// Anything else — a decimal, a word, an empty string — does not count.
function pickInteger(args: Record<string, unknown>, keys: string[]): number | undefined {
  for (const key of keys) {
    if (Object.prototype.hasOwnProperty.call(args, key) === false) {
      continue;
    }
    const value = args[key];
    if (typeof value === 'number' && Number.isInteger(value)) {
      return value;
    }
    if (typeof value === 'string' && value.trim() !== '' && /^-?\d+$/.test(value.trim())) {
      return Number(value.trim());
    }
  }
  return undefined;
}

// Find the first alias that holds a boolean, accepting a JSON boolean and
// the strings "true" and "false" in any capitalisation. Anything else is
// skipped, not treated as false.
function pickBoolean(args: Record<string, unknown>, keys: string[]): boolean | undefined {
  for (const key of keys) {
    if (Object.prototype.hasOwnProperty.call(args, key) === false) {
      continue;
    }
    const value = args[key];
    if (typeof value === 'boolean') {
      return value;
    }
    if (typeof value === 'string') {
      const lowered = value.trim().toLowerCase();
      if (lowered === 'true') {
        return true;
      }
      if (lowered === 'false') {
        return false;
      }
    }
  }
  return undefined;
}

// Work out which repository a tool call is about. The owner and name can
// arrive as separate arguments or packed into one "owner/name" value, and
// the packed form only fills in whichever half is still missing.
function resolveOwnerRepo(args: Record<string, unknown>): {
  owner: string | undefined;
  repo: string | undefined;
} {
  let owner = pickString(args, ALIASES['owner'] as string[]);
  let repo = pickString(args, ALIASES['repo'] as string[]);
  const packed = args['full_name'];
  if ((owner === undefined || repo === undefined) && typeof packed === 'string') {
    const slash = packed.indexOf('/');
    const hasTwoHalves = slash > 0 && slash < packed.length - 1;
    if (hasTwoHalves) {
      const ownerPart = packed.slice(0, slash);
      const repoPart = packed.slice(slash + 1);
      if (owner === undefined && ownerPart !== '') {
        owner = ownerPart;
      }
      if (repo === undefined && repoPart !== '') {
        repo = repoPart;
      }
    }
  }
  return { owner, repo };
}

// True for a string with at least one non-whitespace character. Names and
// titles are required to be non-empty in this sense, not merely present.
function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.trim() !== '';
}

// ---------------------------------------------------------------------------
// Serializers: store records become the JSON shapes the tools return.
// ---------------------------------------------------------------------------

// One repository, in the shape both the REST surface and the tools use.
function repoView(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    name: repo.name,
    full_name: repo.full_name,
    owner: { login: VIEWER.login, id: VIEWER.id, type: VIEWER.type },
    private: repo.private,
    description: repo.description,
    default_branch: repo.default_branch,
    html_url: `https://example.invalid/adaiversen/${repo.name}`,
    clone_url: `https://example.invalid/adaiversen/${repo.name}.git`,
    created_at: repo.created_at,
    updated_at: repo.updated_at,
    pushed_at: repo.updated_at,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
  };
}

// One label, exactly the four fields the API exposes for it.
function labelView(label: {
  id: number;
  name: string;
  color: string;
  description: string;
}): Record<string, unknown> {
  return {
    id: label.id,
    name: label.name,
    color: label.color,
    description: label.description,
  };
}

// One issue, with its labels expanded from names into full label objects.
function issueView(repo: Repo, issue: Issue): Record<string, unknown> {
  const labels: Record<string, unknown>[] = [];
  for (const name of issue.labels) {
    const label = repo.labels.get(name);
    if (label !== undefined) {
      labels.push(labelView(label));
    }
  }
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels,
    user: VIEWER,
    assignee: null,
    assignees: [],
    created_at: issue.created_at,
    updated_at: issue.updated_at,
    closed_at: issue.closed_at,
    html_url: `https://example.invalid/adaiversen/${repo.name}/issues/${issue.number}`,
  };
}

// One pull request. The head and base tips are resolved live from the refs,
// and mergeability is only computed while the pull request is open.
function pullView(forge: Forge, repo: Repo, pull: Pull): Record<string, unknown> {
  const headTip = forge.refOid(repo, `refs/heads/${pull.head.ref}`);
  const baseTip = forge.refOid(repo, `refs/heads/${pull.base.ref}`);
  const mergeable = pull.state === 'open' ? forge.isMergeable(repo, baseTip, headTip) : false;
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
      label: `adaiversen:${pull.head.ref}`,
    },
    base: {
      ref: pull.base.ref,
      sha: baseTip,
      label: `adaiversen:${pull.base.ref}`,
    },
    user: VIEWER,
    created_at: pull.created_at,
    updated_at: pull.updated_at,
    closed_at: pull.closed_at,
    html_url: `https://example.invalid/adaiversen/${repo.name}/pull/${pull.number}`,
  };
}

// Wrap a value as a successful tool result: the JSON text for the model to
// read, plus the structured value for clients that parse it.
function toolResult(value: unknown): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: JSON.stringify(value) }],
    structuredContent: value,
  };
}

// Wrap a message as a failed tool result. The isError flag is what tells
// the client the call failed, since the transport itself still answers 200.
function toolError(message: string): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: message }],
    isError: true,
  };
}

// Resolve the repository a tool call targets, or undefined when the call
// does not name one clearly. The owner must be the viewer: this forge
// holds no one else's repositories, so any other owner is "not found".
function resolveRepoForTool(forge: Forge, args: Record<string, unknown>): Repo | undefined {
  const names = resolveOwnerRepo(args);
  const ownerOk = isNonEmptyString(names.owner);
  const repoOk = isNonEmptyString(names.repo);
  if (ownerOk === false || repoOk === false) {
    return undefined;
  }
  if (names.owner !== VIEWER.login) {
    return undefined;
  }
  return forge.getRepo(names.owner as string, names.repo as string);
}

// ---------------------------------------------------------------------------
// Tool handlers: one function per tool. Each one returns a tool result or
// a tool error; the transport wrapping happens in the caller.
// ---------------------------------------------------------------------------

type ToolHandler = (forge: Forge, args: Record<string, unknown>) => Record<string, unknown>;

// Create a repository. The name must be present and shaped like a
// repository name; creating one that already exists answers with the
// existing repository instead of an error.
function doCreateRepo(vault: Forge, args: Record<string, unknown>): Record<string, unknown> {
  const name = pickString(args, ALIASES['name'] as string[]);
  if (isNonEmptyString(name) === false) {
    return toolError('name is required');
  }
  const wellFormed = /^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/.test(name);
  if (wellFormed === false) {
    return toolError('Invalid repository name');
  }
  const existing = vault.getRepo(VIEWER.login, name);
  if (existing !== undefined) {
    return toolResult(repoView(existing));
  }
  const description = pickString(args, ALIASES['description'] as string[]) ?? '';
  const isPrivate = pickBoolean(args, ALIASES['isPrivate'] as string[]) === true;
  const repo = vault.createRepo(name, description, isPrivate, 'main');
  return toolResult(repoView(repo));
}

// Fetch one repository by owner and name.
function doGetRepo(vault: Forge, args: Record<string, unknown>): Record<string, unknown> {
  const repo = resolveRepoForTool(vault, args);
  if (repo === undefined) {
    return toolError('Repository not found');
  }
  return toolResult(repoView(repo));
}

// Create an issue. Labels are not accepted here — the tool's schema has
// no labels argument — so every issue starts with no labels at all.
function doCreateIssue(vault: Forge, args: Record<string, unknown>): Record<string, unknown> {
  const repo = resolveRepoForTool(vault, args);
  if (repo === undefined) {
    return toolError('Repository not found');
  }
  const title = pickString(args, ALIASES['title'] as string[]);
  if (isNonEmptyString(title) === false) {
    return toolError('title is required');
  }
  const body = pickString(args, ALIASES['body'] as string[]) ?? '';
  const issue = vault.createIssue(repo, title, body, []);
  return toolResult(issueView(repo, issue));
}

// List the issues in a repository, newest first by number. The state
// argument filters the list; anything other than "closed" or "all" lists
// the open issues, which is the default.
function doListIssues(vault: Forge, args: Record<string, unknown>): Record<string, unknown> {
  const repo = resolveRepoForTool(vault, args);
  if (repo === undefined) {
    return toolError('Repository not found');
  }
  let state: 'open' | 'closed' | 'all' = 'open';
  const asked = pickString(args, ALIASES['state'] as string[]);
  if (asked === 'closed' || asked === 'all') {
    state = asked;
  }
  const rows: Issue[] = [];
  for (const issue of repo.issues.values()) {
    if (state === 'all' || issue.state === state) {
      rows.push(issue);
    }
  }
  rows.sort((left, right) => {
    if (left.number === right.number) {
      return 0;
    }
    return left.number > right.number ? -1 : 1;
  });
  const views: Record<string, unknown>[] = [];
  for (const issue of rows) {
    views.push(issueView(repo, issue));
  }
  return toolResult(views);
}

// Open a pull request between two branches. Both branches must already
// exist as refs, and the store confirms that again when creating.
function doCreatePullRequest(
  vault: Forge,
  args: Record<string, unknown>,
): Record<string, unknown> {
  const repo = resolveRepoForTool(vault, args);
  if (repo === undefined) {
    return toolError('Repository not found');
  }
  const title = pickString(args, ALIASES['title'] as string[]);
  const head = pickString(args, ALIASES['head'] as string[]);
  const base = pickString(args, ALIASES['base'] as string[]);
  const missing =
    isNonEmptyString(title) === false ||
    isNonEmptyString(head) === false ||
    isNonEmptyString(base) === false;
  if (missing) {
    return toolError('title, head and base are required');
  }
  const headOk = repo.refs.has('refs/heads/' + (head as string));
  const baseOk = repo.refs.has('refs/heads/' + (base as string));
  if (headOk === false || baseOk === false) {
    return toolError('Invalid head or base');
  }
  const body = pickString(args, ALIASES['body'] as string[]) ?? '';
  const pull = vault.createPull(repo, title as string, body, head as string, base as string);
  if (pull === null) {
    return toolError('Invalid head or base');
  }
  return toolResult(pullView(vault, repo, pull));
}

// Merge a pull request with a squash merge. The new commit is built from
// its lines and joined, which keeps the exact bytes the object hash is
// computed over. The clock is read four separate times here — once for
// the commit's timestamp and once each for the merged, updated, and
// repository stamps — matching the reads the REST merge performs in its
// own way; see the module header for why the count is load-bearing.
function doMergePullRequest(
  vault: Forge,
  args: Record<string, unknown>,
): Record<string, unknown> {
  const repo = resolveRepoForTool(vault, args);
  if (repo === undefined) {
    return toolError('Repository not found');
  }
  const pullNumber = pickInteger(args, ALIASES['pullNumber'] as string[]);
  if (pullNumber === undefined) {
    return toolError('pull_number is required');
  }
  const pull = repo.pulls.get(pullNumber);
  if (pull === undefined) {
    return toolError('Pull request not found');
  }
  if (pull.merged || pull.state === 'closed') {
    return toolError('Pull request is already merged');
  }
  const baseTip = vault.refOid(repo, 'refs/heads/' + pull.base.ref);
  const headTip = vault.refOid(repo, 'refs/heads/' + pull.head.ref);
  const merged = vault.squashMergeTree(repo, baseTip, headTip);
  if (merged.conflict) {
    return toolError('Merge conflict');
  }
  const epochSeconds = Math.floor(Date.parse(vault.nowIso()) / 1000);
  const identity = VIEWER.name + ' <' + VIEWER.email + '> ' + String(epochSeconds) + ' +0000';
  const commitLines = [
    'tree ' + merged.treeOid,
    'parent ' + baseTip,
    'author ' + identity,
    'committer ' + identity,
    '',
    pull.title + ' (#' + String(pull.number) + ')',
  ];
  const squashOid = vault.putObject(
    repo,
    'commit',
    Buffer.from(commitLines.join('\n') + '\n', 'utf8'),
  );
  vault.setRef(repo, 'refs/heads/' + pull.base.ref, squashOid);
  pull.state = 'closed';
  pull.merged = true;
  pull.merged_at = vault.nowIso();
  pull.merge_commit_sha = squashOid;
  pull.updated_at = vault.nowIso();
  pull.closed_at = pull.closed_at ?? pull.updated_at;
  repo.updated_at = vault.nowIso();
  return toolResult(pullView(vault, repo, pull));
}

// The dispatch table: every tool name points at the function that runs it.
const TOOL_HANDLERS: Record<string, ToolHandler> = {
  create_repo: doCreateRepo,
  get_repo: doGetRepo,
  create_issue: doCreateIssue,
  list_issues: doListIssues,
  create_pull_request: doCreatePullRequest,
  merge_pull_request: doMergePullRequest,
};

// Run one tool call. The name may carry a namespace prefix separated by a
// double underscore; only the part after the last separator is the tool.
// An unknown name, or a name that is not a string at all, yields null so
// the caller can answer with the JSON-RPC "unknown tool" error.
function dispatchToolCall(
  forge: Forge,
  params: Record<string, unknown>,
): Record<string, unknown> | null {
  const name = params['name'];
  if (typeof name !== 'string' || name === '') {
    return null;
  }
  const marker = name.lastIndexOf('__');
  const base = marker < 0 ? name : name.slice(marker + 2);
  const handler = TOOL_HANDLERS[base];
  if (handler === undefined) {
    return null;
  }
  return handler(forge, toRecord(params['arguments']));
}

// ---------------------------------------------------------------------------
// Handshake answers: the small methods that need no tools at all.
// ---------------------------------------------------------------------------

// Answer initialize: echo back the newest protocol version the client
// offered that this server supports, or the latest version when the client
// offered nothing usable.
function answerInitialize(
  res: ServerResponse,
  id: unknown,
  params: Record<string, unknown>,
): void {
  const offered = params['protocolVersion'];
  let chosen = LATEST_VERSION;
  if (typeof offered === 'string' && SUPPORTED_VERSIONS.includes(offered)) {
    chosen = offered;
  }
  writeResult(res, id, {
    protocolVersion: chosen,
    capabilities: { tools: {} },
    serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
  });
}

// Answer server/discover: the full version list, the capabilities, and the
// server info twice — once plain and once under _meta, as clients expect.
function answerDiscover(res: ServerResponse, id: unknown): void {
  const info = { name: SERVER_NAME, version: SERVER_VERSION };
  writeResult(res, id, {
    supportedVersions: [...SUPPORTED_VERSIONS],
    capabilities: { tools: {} },
    serverInfo: info,
    _meta: { serverInfo: info },
  });
}

// Read the request body, answering with a protocol parse error when the
// stream itself fails. Returns null when the error was already sent.
async function readRequestBody(
  req: IncomingMessage,
  res: ServerResponse,
): Promise<Buffer | null> {
  try {
    return await readBody(req);
  } catch {
    writeProtocolError(res, 400, -32700, 'Parse error');
    return null;
  }
}

// ---------------------------------------------------------------------------
// Entry point, called by the application server for POST /mcp.
// ---------------------------------------------------------------------------

export async function handleMcp(m: McpRequest): Promise<void> {
  const incoming = m.req;
  const outgoing = m.res;
  const vault = m.forge;
  const allowed = m.authed;

  const raw = await readRequestBody(incoming, outgoing);
  if (raw === null) {
    return;
  }

  // parseJsonBody answers with a wrapper, not the value itself, so the
  // value lives one level down from the result.
  const parsed = parseJsonBody(raw);
  if (parsed.ok === false) {
    writeProtocolError(outgoing, 400, -32700, 'Parse error');
    return;
  }
  if (isPlainObject(parsed.value) === false) {
    writeProtocolError(outgoing, 400, -32600, 'Invalid Request');
    return;
  }
  const body = parsed.value as Record<string, unknown>;
  const method = body['method'];
  if (typeof method !== 'string' || method === '') {
    writeProtocolError(outgoing, 400, -32600, 'Invalid Request');
    return;
  }

  const isCall = hasRequestId(body);
  const id = requestIdOf(body);

  if (allowed === false) {
    writeJson(outgoing, 401, {
      jsonrpc: '2.0',
      id: isCall ? id : null,
      error: { code: -32000, message: 'Requires authentication' },
    });
    return;
  }

  const params = toRecord(body['params']);

  // A message without an id is a notification: acknowledge it with an
  // empty 202 and no body, whatever method it names.
  if (isCall === false) {
    writeEmpty(outgoing, 202);
    return;
  }

  switch (method) {
    case 'server/discover': {
      answerDiscover(outgoing, id);
      return;
    }
    case 'initialize': {
      answerInitialize(outgoing, id, params);
      return;
    }
    case 'notifications/initialized': {
      writeResult(outgoing, id, {});
      return;
    }
    case 'tools/list': {
      writeResult(outgoing, id, { tools: buildToolList() });
      return;
    }
    case 'tools/call': {
      const outcome = dispatchToolCall(vault, params);
      if (outcome === null) {
        const rawName = params['name'];
        const requested = typeof rawName === 'string' ? rawName : '';
        writeRpcError(outgoing, id, -32602, 'Unknown tool: ' + requested);
        return;
      }
      writeResult(outgoing, id, outcome);
      return;
    }
    case 'ping': {
      writeResult(outgoing, id, {});
      return;
    }
    default: {
      writeRpcError(outgoing, id, -32601, 'Method not found: ' + method);
      return;
    }
  }
}
