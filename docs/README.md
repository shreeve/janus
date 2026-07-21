# docs/ — what is authoritative, what is history

Files are timestamped (`YYYYMMDD-HHMMSS-…`) and append-only: each is a
point-in-time contract or record, never rewritten after review. This
index says which is which.

## Contracts (what the code implements against)

| Doc | Role |
| --- | --- |
| [`20260718-191425-janus-build-spec.md`](20260718-191425-janus-build-spec.md) | The phased build contract; every phase's acceptance boxes |
| [`20260719-002000-pool-protocol.md`](20260719-002000-pool-protocol.md) | THE Janus↔tenant pool protocol: doorbell, ring, never-stale |
| [`20260718-204255-capability-ping.md`](20260718-204255-capability-ping.md) | Capability 1: ping (and the cascade rules every capability follows) |
| [`20260718-203749-capability-control.md`](20260718-203749-capability-control.md) | Capability 2: control (`/1.0` listeners) |
| [`20260720-033201-capability-microcache.md`](20260720-033201-capability-microcache.md) | Capability 3: micro-cache + request coalescing |
| [`20260720-162350-hub-design.md`](20260720-162350-hub-design.md) | Capability 4: hub (per-app WebSocket fan-out) |
| [`20260719-141200-tls-ask.md`](20260719-141200-tls-ask.md) | On-demand TLS gating via `/1.0/tls/ask` |

## Measurements (claims and their evidence)

| Doc | Role |
| --- | --- |
| [`20260719-165500-rip-server-performance.md`](20260719-165500-rip-server-performance.md) | The performance ledger: grounding facts, closed doors, every measured result |
| [`20260720-143705-bench-harness.md`](20260720-143705-bench-harness.md) | Bench rig runbook (the runnable harness is `../bench/`) |
| `20260720-*-bench-raw-*.txt` | Raw provenance for the ledger's entries — never edited |

## Design history (kept, superseded by the contracts above)

| Doc | Role |
| --- | --- |
| [`20260718-125236-rip-caddy.html`](20260718-125236-rip-caddy.html) | Original design exploration |
| [`20260718-125236-rip-caddy-ownership.html`](20260718-125236-rip-caddy-ownership.html) | Ownership-boundary design notes |
| [`20260718-182420-janus-api-1.0.html`](20260718-182420-janus-api-1.0.html) | `/1.0` API sketch |

Images (`janus-*.png`, `janus-doorway-mark.svg`) are the project logo,
mark, and social card.
