# mini-forge-v2 — PROGRESS

Rewrite of the github-core-clone-01 entry, perf focus only. Static Go core
+ thin Node launcher (`main.ts` spawns the committed `./forge` binary).
Feature and agent were already MAX (600 + 200); this rewrite keeps them
and pushes perf.

## Local scores (kit `sdk/run.ts --perf --json`, fresh server per run)

| run | checks | feature | agent | perf | total | perf_p50_us | push | clone | read | write |
|-----|--------|---------|-------|------|-------|-------------|------|-------|------|-------|
| 1 (first full run) | 85/120 | 425 | 200 | 84 | 709 | 390913 | 0 | 0 | 41 | 43 |
| 2 | 90/120 | 450 | 200 | 132 | 782 | 399942 | 50 | 0 | 41 | 41 |
| 3 | 92/120 | 460 | 200 | 131 | 791 | 415049 | 48 | 0 | 41 | 42 |
| 4 | 116/120 | 580 | 200 | 171 | 951 | 293654 | 50 | 37 | 41 | 42 |
| 5 | 120/120 | 600 | 200 | 172 | 972 | 295577 | 50 | 40 | 41 | 41 |
| 6 | 120/120 | 600 | 200 | 170 | 970 | — | 50 | 41 | 40 | 39 |
| 7 | 120/120 | 600 | 200 | 169 | 969 | 306442 | 50 | 40 | 40 | 39 |
| final-1 | 120/120 | 600 | 200 | 172 | 972 | 297992 | 50 | 40 | 41 | 41 |
| final-2 | 120/120 | 600 | 200 | 173 | 973 | 290985 | 50 | 43 | 41 | 39 |

Best local: **973** (feature 600/600 MAX, agent 200/200 MAX, perf 173/200).
Old entry: 924 (perf 124). Target was ≥967 local. Beaten.

## What each iteration fixed

- Run 1 → 2: fixed git routing — `info/refs` is two path segments
  (`/<owner>/<repo>.git/info/refs`), the handler required exactly 3
  segments and 404'd every git call. Git group 0/30 → 5/30.
- Run 2 → 3: fixed v2 request parsing — `agent=` / `object-format=`
  lines before the delim packet were rejected as "malformed". Git 5 → 7.
- Run 3 → 4: ls-refs POST must answer content-type
  `application/x-git-upload-pack-result` (not `-advertisement`).
  Git 7 → 27, GraphQL 15 → 19.
- Run 4 → 5 (four fixes):
  - Empty-repo ls-refs advertises nothing (the clone-empty check
    rejects any `refs/*` line, unborn or not).
  - Fetch: tag wants peel to their commit AND the tag object travels
    in the pack (annotated-tag clone).
  - Fetch: the client's shallow boundaries are cutoffs for the held
    set, so a deepen-2 still delivers the parent commit.
  - GraphQL PullRequestFile needs `path` and `changeType` fields.
  Git 27 → 30, GraphQL 19 → 20. All 120 checks pass.
- Run 5 → 6: fetch pack builds serialized per key (singleflight) so
  50 concurrent clones don't each compress the same pack.
- Run 6 → 7: global fetch-answer cache keyed by request fingerprint
  (objects are content-addressed, so the same request yields the same
  bytes even after the repo is recreated); zlib BestSpeed for pack
  compression (build time matters more than size on localhost).
- Final runs: no code changes; stable 972–973.

## Perf notes (why local perf caps near ~173)

Per-workload 50-pt bars: push ≤410 ms, clone ≤66.7 ms, read ≤690 ms,
write ≤87 ms. Measured server-side cost is ~16 µs/request (cached
issue JSON, cached advertisements, cached fetch packs). The remaining
latency is the bench's Node client + round-trips on this VM: plain
Node `fetch` measures 504–719 µs/req sequentially (Python's
http.client: 258–274 µs/req), and the VM is CPU-throttled (4 vCPUs
visible, 1.5 shared cores). Read/write/push are therefore
client-bound locally; the server pod (2 dedicated CPUs) may measure
faster. The old entry's server slowdown (~1.8× local time) need not
apply equally here: its server-side cost was far higher (Node, no
caching), while this entry's server cost is ~16 µs/req.

## Build

`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o forge .`
(Go 1.25.4, stdlib only, no dependencies.) Binary ~6.7 MB, static,
linux/amd64. Launcher: `node /bot/main.ts` spawns `./forge`.

## Notes for bundle / submit (parent)

- `record` does NOT apply to this competition: the kit server refused
  it with HTTP 400 ("github-core-clone-01 does not take recorded
  transcripts: its entries upload their transcripts (a bundle or loose
  files)"). Bundle the builder session instead.
- Kit init anchor (in this session, before the first code write):
  `makefun anchor 8cfa49d97bfbd6c10cc6884119c787fc3441c63f round 34070108`.
- No code from the old entry was ever read; all Go/TS written fresh.
- Prizes are NOT escrowed for this competition; this is a rank chase.
  Force_Last: if a resubmission ever scores worse than the 924 entry,
  restore with `final github-core-clone-01
  01a1e848-48a5-7096-8f6e-9d5d09f2e61d`.
- Deadline: 21 Nov 2026 13:30 WIB.
