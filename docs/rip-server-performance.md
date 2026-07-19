# Rip Server + Janus: performance findings and maximization map

Read this to know where the performance is, where it isn't, and what to
do next. Findings come from a three-way adversarial evaluation
(2026-07-19) of the shipped stack: Janus Phases 1–6 (ping, control,
apps registry, data plane + doorbell ring, heartbeat/TTL, TLS ask) and
`@rip-lang/server` (DSL + manager/worker pool). The architecture
contract is `docs/20260719-002000-pool-protocol.md` — nothing below
changes protocol semantics except the one bug called out.

## The stack, one line

`Client → Janus (Go/Caddy: TLS, host routing, least_conn) → unix
sockets → Bun worker processes (c:1, Sinatra-style Rip DSL)`, with a
manager process off the data path (spawn/watch/heartbeat/doorbell).

## Grounding facts (verified against 2025–2026 sources)

- **Workers are not the ceiling.** Bun.serve over a unix socket does
  ~227k req/s hello-world on an M1 (oven-sh/bun#8044); ~160k req/s TCP
  on Linux per official docs. A single worker's HTTP layer is ~10× the
  whole-stack 20k RPS target. The budget goes to: (a) DSL per-request
  work, (b) Janus TLS + proxy CPU, (c) the `c:1` admission throttle.
- **AsyncLocalStorage is NOT a cost.** ~0.04–0.4µs per `run()` in Bun
  release builds; context save/restore was inlined (bun#24324,
  #26897). Do not spend effort there.
- **splice(2) mostly doesn't apply.** Go splices only raw
  UnixConn→TCPConn; our response path is HTTP-framed and the client
  side is TLS (bytes transit userspace for encryption anyway).
- **kTLS is not in Go's stdlib** (golang/go#44506 accepted, backlog).
  Third-party TLS-1.3-TX wrappers exist; invasive under Caddy. Watch,
  don't build.
- **HTTP/3 to clients is already done** — Caddy serves H3 by default.
- **`bun build --bytecode` is real**: 1.5–4× startup, artifact ~2–8×
  larger, Bun-version-pinned, `--target=bun`.

## The COW question (settled)

Real fork()-style copy-on-write is **unrecoverable and unnecessary**:

- fork() in Bun/JSC is suicide: concurrent GC (Riptide) + JIT threads
  mean a forked child inherits permanently-locked mutexes; no quiesce
  hatch exists. Zygote variants die on the same rock. CRIU is
  Linux-only, privileged, breaks on live sockets — fantasy here.
- fork-COW's real value in Unicorn was *load-the-app-once*; the
  shared-warm-heap half decays in minutes as GC/JIT dirty pages.
- worker_threads share scaffolding, not heap (each Bun Worker is its
  own JSC isolate) and forfeit the pool's crash isolation, per-worker
  RSS caps, and SIGTERM drain. Rejected as the default; c:1-only
  steelman noted for completeness.

**RSS ≈ w × (JSC baseline ~30–50MB + app retained heap).** The honest
levers: keep the Rip compiler out of workers (below), keep `w` small
and raise `c` for I/O-bound apps, recycle via maxRequests/maxSeconds.

## Do these, in order

### 1. Prebuild-once (the convergent winner; ship next)

Today every worker loads the entire Rip compiler and recompiles the
whole app — w× redundant CPU on every pool boot, which is exactly the
latency a doorbell-held client feels (hold cap ~15s; first-request
latency = one boot).

Change: **the manager compiles the app once per dirty epoch into a
single prebuilt JS bundle; workers boot the artifact.** Then run
`bun build --bytecode` on the bundle: JSC mmaps the `.jsc` read-only,
so all w workers share those pages via the kernel page cache — the
honest version of COW.

Wins: reload latency 2–4×; the boot-storm-vs-hold-cap risk largely
dissolves; RSS *drops* (compiler heap leaves every worker); zero
protocol changes (composes with scrap-at-publish: one artifact per
epoch). Caveats: bytecode is Bun-version-pinned (manager regenerates
on upgrade — loud check); Bun's internal transpiler cache does NOT
cover plugin onLoad output, so this artifact is the only cache.
What it cannot remove: Bun boot (~30–80ms) and module *evaluation*
(top-level app code, per process, unavoidable).

### 2. Fix the busy-503 health bug (protocol-level, cheap, urgent)

Janus passive health marks an upstream unhealthy on any 5xx — but at
`c:1` a worker's NORMAL "second request while busy" answer is a 503,
and a draining worker answers 503 too. A modest burst can mark every
healthy worker unhealthy and blackhole the app.

Fix: worker busy/draining responses carry a marker header (e.g.
`Rip-Worker-Busy: 1` / `Rip-Worker-Draining: 1`); Janus excludes
marked 503s from health accounting and treats them as "retry next
upstream now." Only dial failures and unmarked 5xx count. Update the
pool protocol doc + `dataplane.go` + the worker runtime together.

### 3. Raise `c` for I/O-bound apps (near-free 2–10×)

`c:1` idles the event loop for the whole duration of every DB/upstream
wait. The protocol already defines `c>1` as an opt-in (watch off).
`c:8–32` with the same `w` multiplies capacity on I/O-bound handlers
and *halves* RSS versus scaling `w`. Keep `c:1` for CPU-bound.
Remember Janus `MaxIdleConnsPerHost` must scale with `c`.

### 4. Trivial Janus proxy tuning (~20 lines, 5–15% of Go CPU)

- Set `ReverseProxy.BufferPool` (sync.Pool of 32KB buffers — today
  every response copy allocates).
- Stop constructing a `ReverseProxy` struct per attempt in
  `proxyOnce` — build one per socket path or pool them (only the
  Rewrite closure is per-request).
- Verify TLS session resumption is active (`openssl s_client
  -reconnect`) — expected already on via Caddy defaults.

### 5. DSL fast path (measure first, then cut; 1.3–2× per worker)

Predicted first flame-graph hotspot in `packages/server/server.rip` is
**`createContext`**: per request it allocates a `new URL`, a
`new Headers`, and an object with ~15 fresh closures. Second: the
response path (`new Response` + Headers mutation). NOT hotspots: ALS
(measured negligible), route regex walk (≤20 routes ≈ 1–2µs).

Cuts, in value order: (a) lazy context / move closures to a prototype
so per-request allocation is one small object; (b) bucket `_routes` by
method + static-path Map before the regex walk (radix is overkill
below hundreds of routes); (c) skip `posix.normalize` + merged-params
for routes that don't need them. Per repo rule: profile first, land
with the measurement.

### 6. Janus micro-cache + request coalescing (the only 10–100× idea)

A short-TTL (~1s) response cache for anonymous GETs honoring
`Cache-Control`, with singleflight coalescing per cache key, turns a
public-page stampede into ~1 worker request/second. Entirely a
correctness project: key host+path+Vary, bypass on
Cookie/Authorization, honor no-store/private. Build as a proper Janus
capability (doc, cascade rules, hard errors). Do coalescing in the
same change — same machinery, and it is what saves cold-cache
stampedes.

## Boot/reload-path notes (affect perceived performance)

- **Boot storm**: reload spawns w processes compiling simultaneously;
  on heavy apps this can push first-readiness past the 15s ring hold.
  Prebuild-once (#1) is the fix; staggered spawn (boot one, publish at
  `readyWhen:1`, then the rest) is the cheap interim.
- **Hung handler at c:1** is silently lost capacity that reports
  healthy (event loop idle → `/ready` still answers ok). Worker needs
  an in-flight-age watchdog that self-recycles past a ceiling.
- **`/ready` must carry truth in status codes**: 200 only when ready;
  503 while booting/draining (v3 returned 200 for every state with the
  truth only in the body — a trap for any `res.ok` consumer).
- Reload-to-ready ≈ one boot is by design (never serve known-stale);
  `reload: eager` is the existing warm-pool mechanism. A separate
  hot-spare adds nothing: pre-booting before files change buys
  nothing, and after a change it IS eager mode.

## Evaluated and rejected (don't relitigate without new facts)

| Idea | Verdict |
| --- | --- |
| fork()/zygote via FFI | Suicide on JSC (locked GC/JIT mutexes in child) |
| CRIU checkpoint/restore | Linux-only, privileged, no cross-sibling sharing on restore; built for minute-scale boots, ours is ~300ms |
| worker_threads pool | Isolate-per-thread shares little; forfeits crash isolation/RSS caps/kill — the pool's reason to exist |
| Hand-rolled UDS proxy | 20–40% of the Go-side share only; re-owns trailers/upgrades/1xx; Janus budget is mostly TLS + kernel |
| h2c / QUIC to workers | Negative value: UDS + HTTP/1.1 keep-alive has no head-of-line problem; h2/QUIC add framing + crypto CPU |
| kTLS | Not in Go stdlib; fragile under Caddy; 10–30% of TLS CPU on LARGE responses only. Watch golang/go#44506 |
| SO_REUSEPORT for workers (any OS) | macOS: broken semantics (proven live 2026-07: sticky last-binder, paused listener still gets SYNs). Linux: balances per-connection at accept; with keep-alive pools that degrades vs per-request least_conn |
| 103 Early Hints | Browser paint latency, not server throughput |
| Janus fast-path for /ping-class | Accelerates endpoints users don't call |

## Measurement discipline

Claims are verified, not asserted (repo rule 7). The Phase 8 benchmark
must land BEFORE optimization: baseline TLS + keep-alive load (oha/wrk)
against ping-class and small-JSON handlers at w ∈ {2, 8, 16, 32},
c ∈ {1, 8}, recording RPS, p50/p99, worker RSS, Janus CPU share. Every
change above lands with its before/after on the same rig. Construction
cost counts: prebuild adds manager-side compile time per epoch —
measure that too.
