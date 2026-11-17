# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/AhCheng91/ahcheng91-github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/AhCheng91/ahcheng91-github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge written in Go: one static binary, its own in-memory store,
no dependencies, no `git` binary, no UI. It serves the four surfaces GitHub itself exposes to a
client:

* **git smart HTTP** at `/{owner}/{repo}.git` — upload-pack protocol v2 (`ls-refs`, `fetch`, shallow
  clones and `deepen`), receive-pack v0 with `report-status` inside `side-band-64k`, fast-forward
  enforcement, and branch and tag creation and deletion. Packfiles are read and written in-process
  with the standard library alone: trees, commits, tags and both delta kinds (OFS_DELTA and
  REF_DELTA) handled without a `git` binary anywhere.
* **REST v3** under `/api/v3` — repositories, branches, commits, the contents API (create, update,
  delete, directory listings, stale-sha conflicts), labels, issues, comments and pull requests with
  their files and a squash merge that lands a real commit on the default branch.
* **GraphQL v4** at `/api/graphql` — the documents `gh` and `github-mcp-server` send: viewer,
  repository fields, issue and pull-request connections with cursors and ordering, mutations
  (`createIssue`, `closeIssue`, `addComment`, `createPullRequest`, `mergePullRequest`), node lookup,
  introspection and operation batching.
* **MCP** at `/mcp` — JSON-RPC 2.0 over streamable HTTP, revision 2026-07-28 and the older handshake,
  with tools for repositories, issues and pull requests that act on the same store REST reads.

The store lives in memory and nothing reads the clock or a random source: a trial is deterministic
from first request to last.

## Run it

```sh
MAKEFUN_ENV_URL=http://store:8080 node --experimental-strip-types main.ts
```

`main.ts` is only a launcher: it spawns `./forge` from the same directory, inherits its stdio and
exits with the binary's status. The Go binary does everything else — bind an ephemeral port
(`0.0.0.0:0`, never 8080), print `[entry] listening on port N`, register `{"port": N}` with the
contest env over `POST $MAKEFUN_ENV_URL/register` (retrying; 200 and 409 are success), serve, poll
`GET $MAKEFUN_ENV_URL/done` until it answers `{"done": true}` and then exit 0. `GET /healthz`
answers `{"ok": true}` without a token.

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o forge .
```

The binary is committed to the repository. The standard library is the only dependency (`go.mod`
declares a module with no imports), cgo is off, and the `-trimpath -s -w` build keeps the binary
well under the 8 MiB entry-size limit.

## Determinism

The store is in memory, nothing depends on the wall clock and nothing uses randomness. Timestamps
come from `stamp(repo)` exactly as the store defines it — a fixed epoch (2025-01-01T00:00:00Z) plus
one second per call per repository. Every list is kept in insertion-ordered slices and maps are
sorted before any serialization, so two runs of the same trial produce byte-identical results,
including the order of everything that is serialised.

## Source layout

* `main.go` — the entry-to-env lifecycle: bind the ephemeral port, register with the env, poll
  `/done`, exit.
* `app.go` — the HTTP surface: the router, the auth checks, the error handling and the shared
  request context every route goes through.
* `http.go` — the HTTP helpers every surface replies through: reading bodies and sending `application/json`
  with `X-GitHub-Request-Id`, kept allocation-light on the hot path.
* `store.go` — the data: the single authed user, every repository (its refs, objects, issues,
  labels, comments and pulls) and `stamp(repo)`.
* `service.go` — the operations and JSON shapes every surface shares, so an issue created over one
  surface reads back the same over the others.
* `gitobj.go` — git's object model: hashing, tree and commit encodings, and the packfile reader and
  writer.
* `smart.go` — the git smart-HTTP surface: protocol v2 (capability advertisement, `ls-refs`, `fetch`)
  and protocol v0 (upload-pack advertisement, receive-pack advertisement and push).
* `rest.go` — GitHub REST v3.
* `graphql.go` — GitHub GraphQL v4 at `/api/graphql`.
* `mcp.go` — the MCP endpoint at `/mcp`.
* `main.ts` — the launcher the harness runs; it starts `./forge` and nothing else.
* `package.json` — declares `"type": "module"` so `main.ts` can resolve `import.meta.dirname`.

MIT licensed — see `LICENSE`.