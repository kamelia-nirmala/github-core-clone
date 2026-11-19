# github-core-clone-01 — a GitHub-compatible git forge (Go)

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/kamelia-nirmala/github-core-clone/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/kamelia-nirmala/github-core-clone/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A single-process, dependency-free GitHub-compatible forge for [the contest](https://make.fun/bounty/github-core-clone-01): git smart HTTP (protocol v0 and v2), GitHub REST v3, GraphQL v4, and an MCP endpoint over streamable HTTP — all backed by one in-memory store, all deterministic (a logical clock, no wall clock, no unseeded randomness).

The forge itself is a static Go binary (`forge-go`, built from the `.go` sources in this repo). `main.ts` is a thin Node launcher: it spawns the binary, reads the `PORT=<n>` line it prints, registers that port with the trial environment, and waits for the trial to finish. Because a file pushed through the GitHub Contents API lands without its exec bit, the launcher falls back to copying the binary to the temp dir and chmodding it on `EACCES`.

## Run it

```sh
node main.ts
```

The platform runs `node /bot/main.ts` on Node 22.18 or later. There is nothing to install: the launcher imports Node builtins only, and the trial has no package manager and no internet.

## The lifecycle (main.ts)

The env is a server the harness starts beside yours. Its origin is in `MAKEFUN_ENV_URL` (`http://store:8080` in a trial).

1. **Start the forge.** `main.ts` spawns `./forge-go`, which binds a kernel-chosen port and prints `PORT=<n>`.
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

The score is `feature + agent + perf` (0..1000): the conformance checks over that surface, the MCP checks again as the agent-native column, and four timed workloads.

## Layout

- `main.ts`: the launcher (lifecycle above). It spawns the binary and talks to the env.
- `forge-go`: the vendored static Go binary — the forge itself.
- `main.go`, `server.go`: entry point and HTTP server (single-threaded, `GOMAXPROCS=1`).
- `store.go`: the in-memory data model (repos, refs, objects, issues, pulls) with the logical clock.
- `gitpack.go`, `gitobj.go`, `githttp.go`: packfile read/write, object hashing, git smart HTTP.
- `rest.go`, `issues.go`: REST v3 routes (repos, branches, commits, contents, refs, labels, issues, comments, pulls, merges).
- `graphql.go`: the GraphQL parser and executor (queries, mutations, introspection, batching).
- `mcp.go`: the MCP endpoint (discover, initialize, tools/list, tools/call).
- `util.go`: shared helpers. `go.mod`: the module file. `prof_test.go`: profiling benchmarks.
- `LICENSE` (MIT): the entry's licence.

## Local score (SDK runner, 2026-11-19)

feature 600/600 (120/120 checks), agent 200/200 (MCP 15/15), perf 199/200 — total **999/1000** unconstrained. The house reference is ~900.

## Rules and limits

The rules, the scoring weights, the deadline and the prizes are on the contest page: **https://make.fun/bounty/github-core-clone-01** (its Rules section). In short: one process, no dependencies at run time, no internet and no `git` binary in the trial; your repository is mounted read-only at `/bot` and you may write under `/tmp` only. The env at `MAKEFUN_ENV_URL` is the service you answer. The trial's CPU, memory and process limits are the rules'; its wall budget is 900 seconds.

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
