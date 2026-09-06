# Janus changelog

Janus release tags use `vX.Y.Z`. Entries are ordered by tag date, newest
first. Releases before 1.11.0 predate this file; their notes live on the
[releases page](https://github.com/shreeve/janus/releases).

## 1.11.0 — 2026-09-05

- Holds a request whose every worker is busy instead of answering `503` on
  first contact. The hold is bounded (about 2 s per request, and the app's
  existing 64-waiter cap), wakes when a proxied request to the app
  completes, and re-selects against the app's current upstreams, so a
  burst wider than the pool drains through it and a pool swapped mid-hold
  is where the request lands. Past the bound the client still gets `503` +
  `Retry-After`, distinct from the logged all-unhealthy `503`.
- Accepts `concurrency` on upstream entries: the worker's own request cap.
  Selection skips a socket whose in-flight count has reached it and holds
  for capacity without dialing, so a saturated worker is never dialed only
  to refuse. Omitted or `0` keeps the bounce-and-retry behavior; a negative
  value, or a cap on a doorbell entry, is a `400`.
- Signals a freed slot on every concluded attempt, including a worker that
  dies mid-response, and jitters the backstop poll (100 ms, drawn from
  50–150 ms) so a burst parked in the same millisecond does not re-select
  in lockstep.
- Keeps the access log's `retry_count` honest: bounces taken while a
  request is held for capacity are not counted as retries.
- Adds `--uninstall` to the bootstrap installer, which removes only the
  binary it put down and leaves the Caddyfile, service units, and
  certificates in place.
- Colors installer output on a terminal (honoring `NO_COLOR`), and prints
  the green success line even for archives whose embedded installer
  predates it.
- Documents the capacity hold and published concurrency in the pool
  protocol: decision table, flow-control rules, and defaults.
