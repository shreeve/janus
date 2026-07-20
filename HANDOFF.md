# HANDOFF — state snapshot and launch instructions

Snapshot date: **2026-07-20 08:15** (after the performance campaign,
before the Phase 7 / Hub push). This file is navigational: it tells a
fresh session where everything stands and where the authority lives.
The linked docs are authoritative; if this snapshot disagrees with
them, they win. Update or delete this file when Phase 7 lands.

## The two repos

| Repo | What it is | State |
| --- | --- | --- |
| `janus` (this repo) | Caddy module: TLS admission, host routing, worker-pool data plane, doorbell reload, heartbeats, TLS ask, micro-cache. Go. | Phases 1–6 + 8 shipped; cache capability shipped; **Phase 7 (Hub) is the only unbuilt phase** |
| `../rip` | Rip v4 monorepo. `packages/server` = `@rip-lang/server`: the Sinatra-style DSL + manager + worker pool that tenants Janus. | Complete and hardened; 122/122 package tests; deferred list cleared |

Read the operating rules FIRST in whichever repo you touch:
`AGENTS.md` in each. Non-negotiables: reject loudly; tests are the
contract (janus: `go test ./...` AND `./test.sh`; rip: `bun run test`,
completion claims against `bun run test:all`); every perf claim lands
with its measurement; plain git, no AI attribution in commits.

## Where the authority lives

| Topic | Document |
| --- | --- |
| The Janus↔Rip pool protocol (doorbell, ring, never-stale) — THE contract | `docs/20260719-002000-pool-protocol.md` |
| Phased build spec, acceptance boxes, capability order | `docs/20260718-191425-janus-build-spec.md` |
| Performance: grounding facts, closed doors, lever ledger, ALL measured results | `docs/20260719-165500-rip-server-performance.md` |
| Micro-cache capability (shipped, as-built) | `docs/20260720-033201-capability-microcache.md` |
| Capability docs: ping, control, tls-ask | `docs/20260718-*.md`, `docs/20260719-141200-tls-ask.md` |
| Raw bench provenance | `docs/20260720-*-bench-raw-*.txt` |
| Open items (all deliberate) | `../rip/TODO.md` — janus B-list + lexical `_` audit |
| Rip Server package docs (incl. the no-fork memory story) | `../rip/packages/server/README.md` |
| Future clean-room engine rewrite (post-v4) | `../rip/docs/REWRITE.md` |

## What was measured (trust ratios; absolutes pending the baseline)

- Full-stack peak ~99k RPS over HTTPS (M5 laptop, warm/loaded machine).
- `c` knob: capacity-exact; 7x clean 200s on a 5ms handler at c:8.
- Prebuild-once: ~3.7x smaller workers (~33–40MB), ~2.7x reloads
  (~170ms, no longer scales with `w`). Bytecode: not viable on Bun
  1.3.14 (recorded in the ledger; revisit on Bun upgrades).
- Micro-cache: ~320–380x on capacity-bound routes; stampede → exactly
  1 origin fill; generation fence caught straddling fills live.
- Hot-path lock collapse: throughput-neutral (landed on simplicity).
- DSL fast path: ~−30% worker CPU/request; route index −12–15% at 40
  routes.

## Immediate next actions, in order

1. **Canonical cold-machine baseline** (the machine was just rebooted
   for this). Follow the measurement discipline in the perf doc:
   full-stack HTTPS, sweep `w` then `c`, ping-class + 5ms handler,
   cache off AND on, one direct-UDS attribution row. Record as the
   canonical baseline section in the perf doc's Measured results —
   it supersedes all warm-machine absolutes and anchors every future
   A/B. Machine must be otherwise idle; `ulimit -n` 65k+.
2. **Phase 7 — the Hub** (WebSockets; Bam protocol ancestry). Run it
   like the pool protocol was run: design doc → adversarial review
   (multiple independent attackers; it caught critical bugs both
   times it was used) → revision → implementation against the written
   contract → acceptance in `test.sh` → measure. Design inputs already
   decided/flagged:
   - Bam ancestry: `@` join, `+` text, `-` leave (later `>` direct,
     `*` broadcast); bridge POSTs to the tenant's `bridge_path`;
     publish via `POST /1.0/apps/{id}/hub/publish`.
   - Connections terminate at Janus (the edge never restarts during
     app reloads); workers stay disposable. Heartbeat ≠ readiness
     already keeps hub state alive through dirty windows.
   - Multi-tenant host-claim scoping (see `../rip/TODO.md` janus
     B-list) is explicitly a Phase 7 design input.
   - The Bam reference implementation lives in `../bam` (~340 LOC
     Crystal). Protocol pins should port from its behavior.

## Pending policy calls (owner: Steve — ask, don't assume)

- `docs/bench/` subfolder for raw bench files (would amend the
  AGENTS "timestamped files under docs/ only" convention).
- Per-app cache byte quotas (spec's open question Q7) — multi-tenant
  product call; v1 doorkeeper + share cap is shipped and sufficient
  for single-tenant.

## Working style that proved itself (use it)

- Design → independent adversarial review → revise → implement →
  pin → measure. Both times this ran end-to-end it caught
  ship-blocking defects on paper instead of in production.
- One worker per shared-context task; parallelize only independent
  coverage (reviews) or independent repos. Same-file parallel workers
  caused tonight's only edit collision.
- Measure interleaved A/B, one change at a time, numbers in the
  landing commit. "Throughput-neutral, landed on simplicity" is an
  acceptable, honest outcome.
- Background agents stall or get killed by infra sometimes: judge
  liveness by filesystem deltas over time (not transcripts), demand
  handoffs, commit-and-push per completed group so nothing is ever
  lost twice.
