// Repository routes for REST v3: the API root, /user, /user/repos, and
// /repos/{owner}/{repo} with its branches and git refs.
//
// Every repo in this forge belongs to the single viewer account, so the owner
// segment is always that login; anything else is a 404, exactly like a repo
// that does not exist.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { Forge, VIEWER, type Repo } from './store.ts';
import { sendJson, isPlainObject } from './util.ts';

// The one account that owns repos here (the viewer's login).
const REPO_OWNER = 'adaiversen';
// Ref namespaces for branches and tags.
const HEADS_NS = 'refs/heads/';
const TAGS_NS = 'refs/tags/';
// A repo name: starts alphanumerically, then letters, digits, '.', '_' or '-',
// at most 100 characters in total.
const VALID_REPO_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/;
// A full 40-hex git object id.
const HEX_SHA40 = /^[0-9a-fA-F]{40}$/;

// What a route handler receives: the request, the reply, the store, the path
// segments below /api/v3, the query string, the parsed JSON body (when the
// method carries one), whether that body failed to parse, whether the caller
// is authenticated, and the Host header for building pagination links.
export interface Ctx {
  req: IncomingMessage;
  res: ServerResponse;
  forge: Forge;
  segments: string[];
  query: URLSearchParams;
  body: unknown;
  bodyMalformed: boolean;
  authed: boolean;
  host: string;
}

// A JSON object body as a plain record, or null when the body is not one.
function objectFields(value: unknown): Record<string, unknown> | null {
  return isPlainObject(value) ? (value as Record<string, unknown>) : null;
}

// Read per_page / page out of the query string: per_page defaults to 30 and
// is clamped to 1..100, page defaults to 1 and is clamped to at least 1.
// A value that is not a number leaves the default in place.
function pageParams(query: URLSearchParams): { perPage: number; page: number } {
  let perPage = 30;
  const rawPerPage = query.get('per_page');
  if (rawPerPage !== null) {
    const parsed = Number.parseInt(rawPerPage, 10);
    if (!Number.isNaN(parsed)) perPage = parsed;
  }
  perPage = Math.min(100, Math.max(1, perPage));

  let page = 1;
  const rawPage = query.get('page');
  if (rawPage !== null) {
    const parsed = Number.parseInt(rawPage, 10);
    if (!Number.isNaN(parsed)) page = parsed;
  }
  page = Math.max(1, page);

  return { perPage, page };
}

// Cut one page out of a list and, when more pages follow, add the Link header
// pointing at the next page of the viewer's repo list.
function pageSlice<T>(
  items: T[],
  query: URLSearchParams,
  host: string,
): { pageItems: T[]; headers: Record<string, string> } {
  const { perPage, page } = pageParams(query);
  const start = (page - 1) * perPage;
  const pageItems = items.slice(start, start + perPage);
  const headers: Record<string, string> = {};
  if (start + perPage < items.length) {
    headers['Link'] =
      `<http://${host}/api/v3/user/repos?per_page=${perPage}&page=${page + 1}>; rel="next"`;
  }
  return { pageItems, headers };
}

// The public JSON shape of a repo: ids and timestamps from the store, the
// owner block from the viewer, an empty description as null, and the fixed
// capability flags.
function repoPayload(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${REPO_OWNER}/${repo.name}`,
    owner: { login: VIEWER.login, id: VIEWER.id, type: VIEWER.type },
    private: repo.private,
    description: repo.description === '' ? null : repo.description,
    default_branch: repo.default_branch,
    html_url: `https://example.invalid/${REPO_OWNER}/${repo.name}`,
    clone_url: `https://example.invalid/${REPO_OWNER}/${repo.name}.git`,
    created_at: repo.created_at,
    updated_at: repo.updated_at,
    pushed_at: repo.updated_at,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
  };
}

// Every branch of a repo as { name, commit: { sha }, protected }, sorted by
// branch name. The tip of each branch is the oid its ref points at.
function listBranches(repo: Repo): { name: string; commit: { sha: string }; protected: boolean }[] {
  const found: { name: string; commit: { sha: string }; protected: boolean }[] = [];
  for (const [ref, oid] of repo.refs) {
    if (!ref.startsWith(HEADS_NS)) continue;
    found.push({
      name: ref.slice(HEADS_NS.length),
      commit: { sha: oid },
      protected: false,
    });
  }
  found.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return found;
}

// Handle one REST v3 request whose path starts below /api/v3. Returns true
// when this file owned the route (whether it answered or errored) and false
// when the request belongs to another route file.
export function repoRoutes(ctx: Ctx): boolean {
  const { res, forge, segments, query, host } = ctx;

  if (!ctx.authed) {
    sendJson(res, 401, { message: 'Requires authentication' });
    return true;
  }

  if (ctx.bodyMalformed) {
    sendJson(res, 400, { message: 'Invalid JSON body' });
    return true;
  }

  // The API root: the discovery document, plus the OAuth scopes header.
  if (segments.length === 0 || (segments.length === 1 && segments[0] === '')) {
    sendJson(
      res,
      200,
      {
        current_user_url: '/api/v3/user',
        rate_limit_url: '/api/v3/rate_limit',
        user_url: '/api/v3/users/{user}',
      },
      { 'x-oauth-scopes': 'repo, read:org' },
    );
    return true;
  }

  // Server metadata: version and password-auth support.
  if (segments.length === 1 && segments[0] === 'meta') {
    sendJson(res, 200, {
      installed_version: '1.0.0',
      verifiable_password_authentication: false,
    });
    return true;
  }

  // The authenticated user.
  if (segments.length === 1 && segments[0] === 'user') {
    sendJson(res, 200, VIEWER);
    return true;
  }

  // List or create the viewer's repos.
  if (segments.length === 2 && segments[0] === 'user' && segments[1] === 'repos') {
    if (ctx.req.method === 'GET') {
      const repos = forge.viewerRepos().map(repoPayload);
      const { pageItems, headers } = pageSlice(repos, query, host);
      sendJson(res, 200, pageItems, headers);
      return true;
    }

    if (ctx.req.method === 'POST') {
      const fields = objectFields(ctx.body);
      const name = fields && typeof fields.name === 'string' ? fields.name : '';
      if (name === '') {
        sendJson(res, 422, { message: 'Repository name is required' });
        return true;
      }
      if (!VALID_REPO_NAME.test(name)) {
        sendJson(res, 422, { message: 'Invalid repository name' });
        return true;
      }
      if (forge.getRepo(REPO_OWNER, name) !== undefined) {
        sendJson(res, 422, { message: 'Repository already exists' });
        return true;
      }
      const description = fields && typeof fields.description === 'string' ? fields.description : '';
      const isPrivate = fields !== null && fields.private === true;
      const defaultBranch =
        fields && typeof fields.default_branch === 'string' && fields.default_branch !== ''
          ? fields.default_branch
          : 'main';
      const repo = forge.createRepo(name, description, isPrivate, defaultBranch);
      sendJson(res, 201, repoPayload(repo));
      return true;
    }

    return false;
  }

  // Everything below here lives under /repos/{owner}/{name}.
  if (segments[0] !== 'repos') {
    return false;
  }

  const owner = segments[1];
  const name = segments[2];

  if (owner !== REPO_OWNER || typeof name !== 'string') {
    sendJson(res, 404, { message: 'Not Found' });
    return true;
  }

  const repo = forge.getRepo(owner, name);
  const rest = segments.slice(3);
  const method = ctx.req.method;

  // The repo itself: read it, patch its description, or delete it.
  if (rest.length === 0) {
    if (method === 'GET') {
      if (!repo) {
        sendJson(res, 404, { message: 'Not Found' });
        return true;
      }
      sendJson(res, 200, repoPayload(repo));
      return true;
    }

    if (method === 'PATCH') {
      if (!repo) {
        sendJson(res, 404, { message: 'Not Found' });
        return true;
      }
      const fields = objectFields(ctx.body);
      if (fields && typeof fields.description === 'string') {
        repo.description = fields.description;
      }
      sendJson(res, 200, repoPayload(repo));
      return true;
    }

    if (method === 'DELETE') {
      if (!repo) {
        sendJson(res, 404, { message: 'Not Found' });
        return true;
      }
      forge.deleteRepo(owner, name);
      res.writeHead(204, { 'x-github-request-id': 'req-0' });
      res.end();
      return true;
    }

    return false;
  }

  // List all branches.
  if (rest.length === 1 && rest[0] === 'branches') {
    if (method !== 'GET') return false;
    if (!repo) {
      sendJson(res, 404, { message: 'Not Found' });
      return true;
    }
    sendJson(res, 200, listBranches(repo));
    return true;
  }

  // One branch by name: its tip commit.
  if (rest.length === 2 && rest[0] === 'branches') {
    if (method !== 'GET') return false;
    if (!repo) {
      sendJson(res, 404, { message: 'Not Found' });
      return true;
    }
    const branch = rest[1];
    const oid = repo.refs.get(`${HEADS_NS}${branch}`);
    if (oid === undefined) {
      sendJson(res, 404, { message: 'Not Found' });
      return true;
    }
    sendJson(res, 200, { name: branch, commit: { sha: oid } });
    return true;
  }

  // Create a ref: POST /repos/{owner}/{repo}/git/refs with { ref, sha }.
  // The ref must live under refs/, the sha must be a known 40-hex object,
  // and the ref must not exist yet.
  if (rest.length === 2 && rest[0] === 'git' && rest[1] === 'refs') {
    if (method !== 'POST') return false;
    if (!repo) {
      sendJson(res, 404, { message: 'Not Found' });
      return true;
    }
    const fields = objectFields(ctx.body);
    const ref = fields && typeof fields.ref === 'string' ? fields.ref : '';
    if (!ref.startsWith('refs/')) {
      sendJson(res, 422, { message: 'Invalid ref' });
      return true;
    }
    const sha = fields && typeof fields.sha === 'string' ? fields.sha : '';
    if (!HEX_SHA40.test(sha) || !repo.objects.has(sha)) {
      sendJson(res, 422, { message: 'Invalid sha' });
      return true;
    }
    if (repo.refs.has(ref)) {
      sendJson(res, 422, { message: 'Reference already exists' });
      return true;
    }
    repo.refs.set(ref, sha);
    sendJson(res, 201, { ref, object: { sha, type: 'commit' } });
    return true;
  }

  // Read one ref: GET /repos/{owner}/{repo}/git/refs/{heads|tags}/{name}.
  // A branch ref reports a commit; a tag ref reports the stored object's
  // own type (tag when it points at an annotated tag, commit otherwise).
  if (
    rest.length === 4 &&
    rest[0] === 'git' &&
    rest[1] === 'refs' &&
    (rest[2] === 'heads' || rest[2] === 'tags')
  ) {
    if (method !== 'GET') return false;
    if (!repo) {
      sendJson(res, 404, { message: 'Not Found' });
      return true;
    }
    const kind = rest[2];
    const shortName = rest[3];
    const ref = kind === 'heads' ? `${HEADS_NS}${shortName}` : `${TAGS_NS}${shortName}`;
    const oid = repo.refs.get(ref);
    if (oid === undefined) {
      sendJson(res, 404, { message: 'Not Found' });
      return true;
    }
    if (kind === 'heads') {
      sendJson(res, 200, { ref, object: { sha: oid, type: 'commit' } });
      return true;
    }
    const stored = repo.objects.get(oid);
    const type = stored !== undefined && stored.type === 'tag' ? 'tag' : 'commit';
    sendJson(res, 200, { ref, object: { sha: oid, type } });
    return true;
  }

  return false;
}
