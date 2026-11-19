// Helpers shared by the HTTP handlers: reading bodies, base64, request ids,
// bearer auth, JSON replies, and URL parsing.

import type { IncomingMessage, ServerResponse } from 'node:http';

// Slurp the whole request body into one Buffer. Bodies larger than `limit`
// are refused: the promise rejects and the stream is torn down.
export function readBody(req: IncomingMessage, limit = 8 * 1024 * 1024): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const parts: Buffer[] = [];
    let seen = 0;
    req.on('data', (piece: Buffer) => {
      seen += piece.length;
      if (seen > limit) {
        reject(new Error('request body too large'));
        req.destroy();
        return;
      }
      parts.push(piece);
    });
    req.on('end', () => resolve(Buffer.concat(parts)));
    req.on('error', reject);
  });
}

// Turn a raw body into JSON. An empty body means "no value was sent" and is
// fine; anything that does not parse is reported with a fixed message.
export function parseJsonBody(raw: Buffer): { ok: true; value: unknown } | { ok: false; error: string } {
  if (raw.length === 0) return { ok: true, value: undefined };
  try {
    return { ok: true, value: JSON.parse(raw.toString('utf8')) };
  } catch {
    return { ok: false, error: 'Invalid JSON body' };
  }
}

// Base64 without line wrapping (Node's default): what the contents API needs.
export function b64encode(data: Buffer): string {
  return data.toString('base64');
}

// Base64 in, bytes out. Whitespace is stripped first so wrapped input works.
export function b64decode(text: string): Buffer {
  return Buffer.from(text.replace(/\s+/g, ''), 'base64');
}

// Request ids: a per-process counter, rendered as zero-padded hex. Deterministic
// within one run, which keeps replays comparable.
let requestSeq = 0;
export function nextRequestId(): string {
  requestSeq += 1;
  return `req-${requestSeq.toString(16).padStart(8, '0')}`;
}

// Pull the token out of an `Authorization: Bearer <token>` header; '' when the
// header is missing, malformed, or not a Bearer token at all.
export function bearerToken(req: IncomingMessage): string {
  const header = req.headers.authorization ?? '';
  if (typeof header !== 'string') return '';
  const found = /^Bearer\s+(.+)$/.exec(header.trim());
  return found ? (found[1] as string) : '';
}

// Answer with JSON plus the headers every reply carries: the content type, an
// exact length, and a fresh request id. Extra headers may be layered on top.
export function sendJson(
  res: ServerResponse,
  status: number,
  payload: unknown,
  extraHeaders: Readonly<Record<string, string>> = {},
): void {
  const text = `${JSON.stringify(payload)}\n`;
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': nextRequestId(),
    ...extraHeaders,
  });
  res.end(text);
}

// The standard error shape: a JSON body holding just a message.
export function sendError(res: ServerResponse, status: number, message: string): void {
  sendJson(res, status, { message });
}

// Split a request URL into its path, its decoded path segments, and its query
// string. A URL that will not parse degrades to the root with no query, so
// callers never have to guard against a throw here.
export function parseUrl(req: IncomingMessage): { path: string; segments: string[]; query: URLSearchParams } {
  try {
    const parsed = new URL(req.url ?? '/', 'http://entry');
    const path = parsed.pathname;
    const segments = path
      .split('/')
      .filter((piece) => piece !== '')
      .map((piece) => {
        try {
          return decodeURIComponent(piece);
        } catch {
          return piece;
        }
      });
    return { path, segments, query: parsed.searchParams };
  } catch {
    return { path: '/', segments: [], query: new URLSearchParams() };
  }
}

// True for a plain object (not null, not an array). Used to tell a JSON object
// body apart from a scalar or an array before reading fields off it.
export function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
