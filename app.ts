// The forge HTTP server: one Forge store, dispatching to git smart HTTP, the
// REST v3 routes, GraphQL, and MCP.
//
// app.ts is the server; main.ts is the entry <-> env lifecycle and stays small.
// This file only builds the server — it binds nothing until the caller listens.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Forge } from './store.ts';
import { readBody, parseJsonBody, bearerToken, sendJson, sendError, parseUrl } from './util.ts';
import { handleGit } from './git-http.ts';
import { repoRoutes } from './rest-repos.ts';
import { contentRoutes } from './rest-contents.ts';
import { issuePullRoutes } from './rest-issues.ts';
import { handleMcp } from './mcp.ts';
import { handleGraphQL } from './graphql.ts';

// The media type every JSON reply uses.
const JSON_MIME = 'application/json; charset=utf-8';
void JSON_MIME;

// The bearer token the env hands out, or '' when the server runs standalone
// with no token configured at all.
function shopToken(): string {
  return process.env.MAKEFUN_SHOP_TOKEN ?? '';
}

// True when the request carries the expected Bearer token — or when no token
// is configured, in which case everything is accepted.
function requestIsAuthed(req: IncomingMessage): boolean {
  const expected = shopToken();
  if (expected === '') return true;
  return bearerToken(req) === expected;
}

// The pathname of a request URL, or '/' when it will not parse. Never throws,
// so request handling never dies on a bad URL.
function requestPath(req: IncomingMessage): string {
  try {
    return new URL(req.url ?? '/', 'http://entry').pathname;
  } catch {
    return '/';
  }
}

// Build the server. It binds nothing until the caller listens; the entry point
// listens on an ephemeral port chosen by the kernel.
export function createApp(): Server {
  const forge = new Forge();

  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      const { path, segments, query } = parseUrl(req);
      const method = req.method ?? 'GET';
      const host = req.headers.host ?? 'localhost';

      // The env's boot check calls this twice and wants 200 {"ok":true}. No auth.
      if (method === 'GET' && path === '/healthz') {
        sendJson(res, 200, { ok: true });
        return;
      }

      // ---- Git smart HTTP: /{owner}/{repo}.git/* ----
      if (segments.length >= 2 && segments[1] !== undefined && segments[1].endsWith('.git')) {
        // Git auth is lenient: no token at all is fine, but a wrong token is 401.
        const expected = shopToken();
        const token = bearerToken(req);
        if (expected !== '' && token !== '' && token !== expected) {
          sendError(res, 401, 'Requires authentication');
          return;
        }
        const owner = segments[0] as string;
        const repoName = (segments[1] as string).slice(0, -4);
        const tail = segments.slice(2);
        let body: Buffer = Buffer.alloc(0);
        try {
          body = await readBody(req);
        } catch {
          // A body that cannot be read (too large, aborted mid-flight) counts
          // as empty here; the git handler answers with a protocol error
          // instead of the server crashing.
          body = Buffer.alloc(0);
        }
        await handleGit({ req, res, forge, owner, repoName, tail, query }, body);
        return;
      }

      // ---- REST v3: /api/v3/* ----
      if (segments.length >= 2 && segments[0] === 'api' && segments[1] === 'v3') {
        const restSegments = segments.slice(2);
        const authed = requestIsAuthed(req);
        let body: unknown;
        let bodyMalformed = false;
        if (method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE') {
          let raw: Buffer = Buffer.alloc(0);
          try {
            raw = await readBody(req);
          } catch {
            sendError(res, 400, 'Invalid JSON body');
            return;
          }
          const parsed = parseJsonBody(raw);
          if (!parsed.ok) {
            bodyMalformed = true;
          } else {
            body = parsed.value;
          }
        }
        const ctx = { req, res, forge, segments: restSegments, query, body, bodyMalformed, authed, host };
        if (repoRoutes(ctx)) return;
        if (contentRoutes(ctx)) return;
        if (issuePullRoutes(ctx)) return;
        // No route claimed the request: a JSON 404 — but the auth answer comes
        // first when the caller sent no (valid) token.
        if (!authed) {
          sendError(res, 401, 'Requires authentication');
          return;
        }
        sendError(res, 404, 'Not Found');
        return;
      }

      // ---- MCP: POST /mcp (JSON-RPC 2.0 over Streamable HTTP) ----
      if (requestPath(req) === '/mcp') {
        await handleMcp({ req, res, forge, authed: requestIsAuthed(req) });
        return;
      }

      // ---- GraphQL: POST /api/graphql ----
      if (requestPath(req) === '/api/graphql') {
        const authed = requestIsAuthed(req);
        let body: unknown;
        let bodyMalformed = false;
        if (method === 'POST' || method === 'PUT' || method === 'PATCH') {
          let raw: Buffer = Buffer.alloc(0);
          try {
            raw = await readBody(req);
          } catch {
            bodyMalformed = true;
          }
          if (!bodyMalformed) {
            const parsed = parseJsonBody(raw);
            if (!parsed.ok) bodyMalformed = true;
            else body = parsed.value;
          }
        }
        handleGraphQL({ req, res, forge, body, bodyMalformed, authed });
        return;
      }

      // Anything else, any method: GitHub's plain 404 body.
      sendJson(res, 404, { message: 'Not Found' });
    })().catch((err: unknown) => {
      // Last-resort guard: never leave a request hanging, never crash the process.
      try {
        if (!res.headersSent && !res.writableEnded) {
          sendError(res, 500, err instanceof Error ? err.message : 'Internal Server Error');
        } else {
          res.end();
        }
      } catch {
        // The socket is already gone; there is nothing left to do.
      }
    });
  });
}
