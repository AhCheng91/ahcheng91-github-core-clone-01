# ahcheng91-github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/AhCheng91/ahcheng91-github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/AhCheng91/ahcheng91-github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge: one Node 22 process, its own in-memory store, no
dependencies and no UI. It serves the four surfaces GitHub itself exposes to a client —

* **git smart HTTP** at `/{owner}/{repo}.git` — upload-pack protocol v2 (`ls-refs`, `fetch`, shallow
  clones and `deepen`), receive-pack v0 with `report-status` inside `side-band-64k`, fast-forward
  enforcement, branch and tag creation and deletion. Packfiles are read and written with `node:zlib`
  and `node:crypto` alone: trees, commits, tags and both delta kinds (OFS_DELTA and REF_DELTA) are
  handled in-process, with no `git` binary anywhere.
* **REST v3** under `/api/v3` — repositories, branches, commits, the contents API (create, update,
  delete, directory listings, stale-sha conflicts), labels, issues, comments and pull requests with
  their files and a squash merge that lands a real commit on the default branch.
* **GraphQL v4** at `/api/graphql` — the documents `gh` and `github-mcp-server` send: viewer,
  repository fields, issue and pull-request connections with cursors and ordering, mutations
  (`createIssue`, `closeIssue`, `addComment`, `createPullRequest`, `mergePullRequest`), node lookup,
  introspection and operation batching.
* **MCP** at `/mcp` — JSON-RPC 2.0 over streamable HTTP, revision 2026-07-28 and the older handshake,
  with tools for repositories, issues and pull requests that act on the same store REST reads.

The store lives in memory and nothing reads the clock: a trial is deterministic from first request to
last.

## Run it

```sh
MAKEFUN_ENV_URL=http://store:8080 node --experimental-strip-types main.ts
```

`main.ts` binds an ephemeral port, registers it with the contest env, and exits once the env says the
trial is done. `app.ts` and `src/` hold the forge itself; `src/http.ts` is the only place that writes
headers, and `src/service.ts` is the one set of views and operations every surface shares.

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
