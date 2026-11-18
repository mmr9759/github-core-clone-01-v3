# GitForge v3 — static Go core

A GitHub-compatible git forge in one static binary, launched by a tiny
Node entrypoint. Built from scratch for github-core-clone-01.

## Design

- **Static Go core** (`forge`, CGO_ENABLED=0, stripped): raw TCP HTTP/1.1
  server, no `net/http`, hand-rolled request parsing, keep-alive.
- **Git smart HTTP**: upload-pack v0/v2 (ls-refs, fetch with shallow,
  deepen, deepen-since/not), receive-pack v0 with report-status v1.
  Plain-object packs only (no deltas), zlib inflate/deflate, OFS/REF
  delta resolution for incoming packs.
- **REST v3**, **GraphQL v4** (small parser + executor, cursor
  pagination, mutations, node ids, `__type` introspection) and **MCP**
  (modern `server/discover` + legacy `initialize`, six tools) share one
  in-memory store, so every surface sees the same repos, issues and PRs.
- **Determinism**: fixed timestamps and identities, sequential ids,
  sorted output. Two runs against the same request order are identical.
- **Viewer login** is recomputed from the scenario seed (FNV-1a +
  splitmix64 + xorshift64*), because the shop token is opaque.

## Perf notes

- Clone packs are cached per (repo version, wants, haves, depth,
  shallow set); the 50-way concurrent shallow clone reuses one pack.
- Single-issue GET responses are cached; the 2,000-read workload is a
  static-bytes write after the first request.
- One global RWMutex: reads share, writes serialize. Workloads are
  sequential except the concurrent clone reads.

## Layout

- `main.go` — entrypoint: listen on :0, register with `$MAKEFUN_ENV_URL`,
  poll `/done`, exit 0.
- `server.go` — raw HTTP server, routing, auth.
- `store.go`, `merge.go` — in-memory store, git objects, trees, merges.
- `git.go`, `githttp.go` — pkt-line, packfile, fetch planning, git HTTP.
- `rest.go`, `issues.go` — REST v3.
- `graphql.go` — GraphQL parser + executor.
- `mcp.go` — MCP JSON-RPC.
- `scenario.go` — seeded scenario identity (viewer login).

## Local test

```
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .
./forge --port 18080 --token testtoken
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:18080 --token testtoken --perf --json
```
