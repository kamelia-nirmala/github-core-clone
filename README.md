# github-core-clone-01 — a GitHub-compatible git forge

A single-process, dependency-free GitHub-compatible forge for [the contest](https://make.fun/bounty/github-core-clone-01): git smart HTTP (protocol v0 and v2), GitHub REST v3, GraphQL v4, and an MCP endpoint over streamable HTTP — all backed by one in-memory store, all deterministic (a logical clock, no wall clock, no unseeded randomness).

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs `node /bot/main.ts` on Node 22.18 or later, which strips TypeScript types on its own; the flag is harmless there and makes an older local Node work too. There is nothing to install: the program imports `node:http`, `node:crypto`, `node:zlib` and its own modules only, and the trial has no package manager and no internet.

## The lifecycle (main.ts)

The env is a server the harness starts beside yours. Its origin is in `MAKEFUN_ENV_URL` (`http://store:8080` in a trial).

1. **Bind an ephemeral port.** `server.listen(0)` lets the kernel pick one; read it with `server.address().port`. Never bind 8080: that is the env's port, and on the one-pod layout it is on the same loopback.
2. **Register.** `POST $MAKEFUN_ENV_URL/register` with `{"port": <the port you bound>}`, as JSON. The env records the address the request came from plus that port as your base URL and dials it from then on. The first register wins: a later one is answered `409` and does not move the base URL.
3. **Wait for done.** `GET $MAKEFUN_ENV_URL/done` answers at once with `{"done":false}` while the suite runs and `{"done":true}` once its export is written. Poll it with a short backoff; never hold a request open, and never exit before it says `true`.
4. **Exit 0** when it does. Closing the server first is tidy; the harness stops the env afterwards, and the env's export is what the scorer reads.

A failed poll or register is retried with a short exponential backoff. If `MAKEFUN_ENV_URL` is unset there is nothing to talk to, so `main.ts` exits 1 at once.

## The surface

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok":true}`: the env's boot probe, called twice |
| `GET /api/v3/` | 200, `X-Oauth-Scopes: repo, read:org`, a JSON API root |
| `/api/v3/...` | GitHub REST v3: repositories, branches, commits, contents, issues, comments, labels, pull requests, files, merges |
| `POST /api/graphql` | GitHub GraphQL v4: the documents `gh` and `github-mcp-server` send (queries, mutations, `node(id:)`, `__type` introspection, batched operations) |
| `POST /mcp` | Model Context Protocol over streamable HTTP: `server/discover`, `initialize`, `tools/list`, `tools/call` (create_repo, get_repo, create_issue, list_issues, create_pull_request, merge_pull_request) |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack` (v0 + v2, incl. shallow/deepen), `git-receive-pack` |
| anything else | 404 `{"message":"Not Found"}` |

The env presents every API call with `Authorization: Bearer $MAKEFUN_SHOP_TOKEN` (the same token is in your environment); `/healthz` is the boot probe and takes none. Git reads are lenient (no token allowed); a wrong token is refused with 401 everywhere.

The score is `feature + agent + perf` (0..1000): the conformance checks over that surface, the MCP checks again as the agent-native column, and four timed workloads. The version of the suite the trial runs is in the kit under `program/`, so you can grade the same checks locally before you submit.

## Rules and limits

The rules, the scoring weights, the deadline and the prizes are on the contest page: **https://make.fun/bounty/github-core-clone-01** (its Rules section). In short: Node 22 only, no dependencies at run time, no internet and no `git` binary in the trial; your repository is mounted read-only at `/bot` and you may write under `/tmp` only. The env at `MAKEFUN_ENV_URL` is the service you answer; the model gateway, if your entry calls a model, is the only other endpoint it can reach. The trial's CPU, memory and process limits are the rules'; its wall budget is 900 seconds.

## Layout

- `main.ts`: the lifecycle above. Keep it as it is.
- `app.ts`: the server and router.
- `store.ts`: the in-memory data model (repos, refs, objects, issues, pulls) with the logical clock and the git helpers (trees, merges, diffs).
- `git-pack.ts`: pkt-line framing, sideband, packfile read/write (OFS/REF deltas), object hashing.
- `git-http.ts`: git smart HTTP — advertisements, upload-pack v0/v2, receive-pack.
- `rest-repos.ts`, `rest-contents.ts`, `rest-issues.ts`: REST v3 routes (repos, branches, commits, contents, refs, labels, issues, comments, pulls, merges).
- `graphql.ts`: the GraphQL parser and executor (queries, mutations, introspection, batching).
- `mcp.ts`: the MCP endpoint (discover, initialize, tools/list, tools/call).
- `util.ts`: shared helpers (bodies, base64, request ids, auth).
- `package.json`, `tsconfig.json`, `LICENSE` (MIT): the entry's metadata and licence.

## Local score (SDK runner, 2026-11-19)

feature 600/600 (120/120 checks), agent 200/200 (MCP 15/15), perf 187/200 — total **987/1000**. The house reference is ~900.
