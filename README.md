# mini-forge-v2

[![make.fun](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/c/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/tuyulsembilanenam672-cell/mini-forge-v2/rank.svg)](https://make.fun/c/github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/tuyulsembilanenam672-cell/mini-forge-v2/reward.svg)](https://make.fun/c/github-core-clone-01)

A fast, self-contained GitHub-core clone in one static Go binary, built
for the [make.fun github-core-clone-01 bounty](https://make.fun/c/github-core-clone-01).

One process serves everything the bounty requires, with no git binary
and no external service:

- **git smart HTTP** — upload-pack v2 (ls-refs, fetch with shallow /
  deepen, filters) and receive-pack v0 (create, fast-forward, delete,
  non-fast-forward refusals with per-ref report-status)
- **REST v3** at `/api/v3` — repositories, branches, commits, git refs,
  contents, labels, issues, comments and pull requests with squash merge
- **GraphQL** at `/api/graphql` — viewer, repository, issues and pull
  requests with connections and pagination, mutations, and `__type`
  introspection
- **MCP** at `/mcp` — Streamable HTTP JSON-RPC 2.0 (modern
  `server/discover` era plus the legacy `initialize` handshake) with six
  tools: `create_repo`, `get_repo`, `create_issue`, `list_issues`,
  `create_pull_request`, `merge_pull_request`

## Running it

The suite runs `node /bot/main.ts`. The launcher spawns the committed
static binary `./forge` (linux/amd64, `CGO_ENABLED=0`, standard library
only), which binds a port, registers it with `$MAKEFUN_ENV_URL`, serves
until the env reports done, and exits 0. Without `MAKEFUN_ENV_URL` it
serves until killed.

```sh
# rebuild the binary
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o forge .
node main.ts
```

## Performance

Local kit scores (`sdk/run.ts --perf --json`): 120/120 checks,
feature 600/600, agent 200/200, perf ~172/200, total ~972. The speed
comes from caching: issue and pull-request JSON is cached per object,
advertisements are cached per repository version, and fetch answers
are cached globally by request fingerprint (objects are
content-addressed, so the same request always yields the same bytes).
Pack compression uses zlib BestSpeed — a pack is compressed once and
served many times.

## Layout

- `main.ts` — thin Node launcher (spawns `./forge`)
- `main.go` — binary entrypoint: bind, register, poll done, exit
- `store.go` — in-memory store: repos, objects, trees, commits,
  reachability
- `gitobj.go` — packfiles (build/parse) and fetch object sets
- `gitproto.go` — pkt-lines, advertisements, ls-refs, fetch, receive-pack
- `http.go` — HTTP server, routing, auth
- `rest.go` — REST v3 handlers
- `graphql.go` — GraphQL parser and executor
- `mcp.go` — MCP endpoint and tools
- `forge` — the committed static binary
- `PROGRESS.md` — iteration log with local scores

## License

MIT. See [LICENSE](LICENSE).
