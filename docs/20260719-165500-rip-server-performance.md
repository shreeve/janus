# Rip Server + Janus: performance findings and maximization map

What to address, in what order, to maximize performance of the
Janus + Rip Server stack. Distilled from a three-track evaluation
(memory/COW recovery, adversarial pitfalls, throughput levers) run
against the implemented system on 2026-07-19. A reader with no other
context should be able to start from this file.

## The stack under discussion

```text
Client → Janus (Go/Caddy module: TLS, host routing, least_conn,
         passive health, doorbell ring) → unix sockets →
         Bun worker processes (c:1, @rip-lang/server Sinatra DSL)

Manager (Bun) off the data path: spawn/watch/heartbeat, doorbell,
demand-driven reload per docs/20260719-002000-pool-protocol.md
```

Baseline target: ~20k RPS on ping-class handlers (v3's measured
number). Grounding ceiling: a single Bun worker serving hello-world
over a unix socket measures ~200k+ req/s (oven-sh/bun#8044, M1) — so
the stack's limit is not the workers' HTTP layer; it is (a) per-request
DSL work, (b) Janus TLS + proxy cost, and (c) the `c:1` admission
throttle.

## Grounding facts (verified 2026-07; do not re-litigate without new evidence)

- **ALS is not a cost.** Bun inlined AsyncLocalStorage save/restore;
  `run()` overhead measures ~0.04–0.4µs. Ignore it in optimization
  plans.
- **splice(2) does not apply** to the proxy's response path (HTTP
  framing + TLS means bytes transit userspace anyway). Only relevant
  for Upgrade tunnels or a future kTLS world.
- **kTLS is not in Go's stdlib** (golang/go#44506 accepted, backlog).
  Third-party TLS 1.3 TX-only wrappers exist; invasive under Caddy.
- **`bun build --bytecode` is real**: 1.5–4x startup improvement,
  artifact ~8x larger, Bun-version-locked, `--target=bun`; JSC mmaps
  the `.jsc` read-only so the pages are shared across all workers via
  the kernel page cache.
- **HTTP/3 to clients is already served** by Caddy. Nothing to build.
- **fork()/zygote/CRIU are dead ends on Bun/JSC** (see "Closed doors").

## Ranked levers

| # | Lever | Expected win | Cost | Verdict |
| --- | --- | --- | --- | --- |
| 1 | Raise `c` (8–32) for I/O-bound apps, watch off | 2–10x per worker | ~zero (protocol opt-in exists) | **Ship now** |
| 2 | Janus micro-cache + request coalescing (anonymous GETs) | 10–100x on cacheable pages | Medium-high (correctness) | Measure-first, then build as a capability |
| 3 | Manager prebuilds app once per dirty epoch; workers boot artifact (+`--bytecode`) | Reload/boot 2–4x; RSS drops | Low-medium | **Ship now** |
| 4 | DSL fast path (context allocation, route buckets) | 1.3–2x per worker ping-class | Medium | Measure-first (profile, then cut) |
| 5 | `ReverseProxy.BufferPool` + proxy-struct reuse + idle conns scaled with `c` | 5–15% of Janus CPU | Trivial (~20 lines) | **Ship now** |
| 6 | Static file bypass at Janus (registration declares static roots) | Large for asset-heavy tenants; zero for APIs | Medium (protocol extension) | Later (need a real tenant) |
| 7 | GOMAXPROCS split / core pinning (Janus 2–4 procs, workers own the rest) | 5–15%, mostly tail latency | Low | Measure-first |
| 8 | Hand-rolled UDS proxy replacing httputil.ReverseProxy | 20–40% of the Go-side share only | High (streaming/trailers/upgrades correctness) | Later |
| 9 | kTLS TX-only (TLS 1.3, Linux) | 10–30% of TLS CPU on large bodies | High, fragile under Caddy | Later; watch golang/go#44506 |
| 10 | h2c or QUIC to workers | Negative to zero | — | Fantasy |

### 1. Raise `c` — the biggest lever hiding in plain sight

Bun is an event-loop runtime; at `c:1` a worker sits idle for the full
duration of every DB query or upstream fetch. For I/O-bound apps,
`c:8–32` with the same worker count is a near-free 2–10x, and halves
RSS versus scaling `w`. The pool protocol already defines higher `c` as
an opt-in (watch off). Keep `c:1` for CPU-bound handlers and for watch
mode. Capacity = `w × c`.

### 3. Prebuild-once + bytecode — the honest replacement for fork/COW

Today every worker independently imports the entire Rip compiler and
recompiles the whole app: `w×` redundant work on every pool boot, paid
while a client holds on the doorbell (hold cap ~15s). Instead:

- The manager (which already owns the file watch) compiles the app
  once per dirty epoch into a single JS bundle.
- Workers boot the artifact — no compiler in the worker, no Rip
  compilation, just module evaluation + heap build (irreducibly
  per-process).
- Optionally `bun build --bytecode` the bundle: JSC skips parse/AST at
  boot, and the mmapped read-only `.jsc` pages are shared across all
  `w` workers — the closest honest thing to COW available.
- Regenerate on Bun upgrade (version-locked bytecode) — loud check.
- Bun's internal transpiler cache does NOT cover plugin `onLoad`
  output, so this artifact must be Rip's own.

Wins: reload latency (the metric the doorbell exposes to users), the
boot-storm-vs-hold-cap risk largely dissolves, and RSS drops because
the compiler's retained heap (parser tables) leaves all workers.
Zero protocol changes. Composes with scrap-at-publish: a dirty epoch
rebuilds one artifact, then spawns against it.

### 5. Trivial Janus proxy tuning

- Set `ReverseProxy.BufferPool` (sync.Pool of 32KB buffers) — today
  every response copy allocates.
- Stop constructing a `ReverseProxy` struct per proxy attempt — build
  one per socket path or pool them (only the Rewrite closure is
  per-request).
- Scale `MaxIdleConnsPerHost` with `c` (32 is right for c:1; at c:16
  under load it churns connections).
- TLS session resumption is on by default in Go/Caddy — verify with
  `openssl s_client -reconnect`, expect no work needed.

### 4. DSL fast path — profile first, then cut

Predicted first flame-graph hotspot in `packages/server/server.rip`
(rip repo): **`createContext`** — a `new URL`, a `new Headers`, and an
object with ~15 fresh closures allocated per request; then the response
path (`new Response` + Headers mutation). NOT ALS; NOT the route regex
walk at ≤20 routes (~1–2µs).

Fixes in value order once profiling confirms:
1. Lazy context / move closures to a prototype so per-request
   allocation is one small object.
2. Bucket `_routes` by method; static paths in a Map before the regex
   walk (radix tree is overkill below hundreds of routes).
3. Skip `posix.normalize` + merged-params object for routes that don't
   need them.

### 2. Micro-cache + coalescing — the only 10x+ idea

A 1s-TTL response cache at Janus for anonymous GETs, honoring
`Cache-Control`, with single-flight coalescing per cache key, turns a
stampede on a public page into ~1 worker request/second. The danger is
entirely correctness: key on host+path+(Vary), bypass on `Cookie` /
`Authorization`, honor `no-store`/`private`. Build it as a proper
capability (doc, cascade rules, hard errors) when a use case demands
it; do coalescing in the same change (same machinery, and it is what
saves cold-cache stampedes).

## Performance-adjacent correctness (fix before stress testing)

These came from the adversarial track; the first one caps throughput
under load and contradicts the protocol as implemented:

1. **Busy-503 bounces (fixed 2026-07-19).** At `c:1` a worker's NORMAL
   "second request while busy" answer is a 503. Correction to the
   original finding: Janus passive health never counted response 5xx
   toward health (only failed dials and post-dial transport failures),
   so the predicted health-poisoning blackhole could not occur — but
   every busy bounce was forwarded to the client as a raw 503, which
   under a burst is most responses (measured: w:8/conc:64 on a 5ms
   handler = 993,997 client-visible 503s in 15s). Shipped fix: worker
   503s carry `Rip-Worker-Busy: 1` (drain: `Rip-Worker-Draining: 1`);
   Janus excludes marked 503s from health accounting and immediately
   tries the next upstream for replayable requests (no body streamed).
   All-workers-busy still answers 503 + `Retry-After`, silently —
   capacity, not failure. See the pool protocol "Data plane decision
   table" and "Measured results" below.
2. **Boot storm vs the 15s ring hold.** `w` simultaneous cold boots
   contend for cores; a heavy app can push first-readiness past the
   hold cap. Mitigation: prebuild-once (#3) mostly dissolves this;
   staggered spawn (boot one, publish at `readyWhen:1`, boot the rest)
   is the cheap fallback.
3. **Hung handler at `c:1` is lost capacity that reports healthy.**
   In-flight-age watchdog in the worker; self-recycle past a ceiling.
4. **Drain constants must order correctly**: worker in-flight wait ≤
   manager SIGKILL grace, and deliberate kills marked expected so
   crash/restart budgets stay honest.

## Closed doors (do not spend time here)

- **fork()/zygote via FFI**: Bun/JSC runs concurrent GC + JIT threads
  before any JS executes; a forked child inherits permanently locked
  mutexes from dead threads. No quiesce hatch exists. `posix_spawn`
  (what `Bun.spawn` uses) is safe precisely because it discards the
  address space — i.e. no COW.
- **CRIU snapshot/restore**: Linux-only, privileged, restores private
  pages per process (no cross-sibling sharing), breaks on live unix
  sockets. Built for minute-scale GPU cold starts, not 300ms pools.
- **Real COW would not hold anyway**: GC, inline caches, and JIT
  profiling counters dirty shared heap pages within minutes (Ruby's
  `GC.compact` saga). Fork-COW's durable value was load-once, which
  prebuild-once recovers without fork.
- **worker_threads as the default pool**: each Bun Worker is its own
  JSC isolate (shared scaffolding, not heap), and it trades away the
  pool's crash-isolation: one segfault/OOM kills every "worker,"
  SIGTERM-drain becomes cooperative cancellation, no per-worker RSS
  cap. Steelmanned and rejected for the default; conceivable later as
  an opt-in for trusted, memory-tight deployments.
- **SO_REUSEPORT for workers**: macOS semantics are disqualifying
  (verified live 2026-07: sticky last-binder; a paused listener still
  receives SYNs). Skip on Linux too: kernel balancing is
  per-connection at accept time, which degrades under Janus's
  keep-alive pools — per-request least_conn is strictly better.
- **h2c/QUIC to workers**: a unix socket has no head-of-line problem
  to multiplex away and no loss to recover; h2/QUIC add framing and
  crypto CPU on both ends for negative value.
- **Hot-spare warm pools**: a pre-booted generation N+1 before files
  change is the same files (buys nothing); after files change it is
  exactly `reload: eager`, which already exists. No separate mechanism.
- **103 Early Hints**: helps browser paint latency, not server
  throughput.

## Measurement discipline

Claims are verified, not asserted (both repos' standing rule). For the
stress phase:

- Bench over TLS through the full stack (client → Janus → UDS →
  worker), ping-class AND a DB-ish 1–5ms handler; `oha` or `wrk` with
  keep-alive; report p50/p99 alongside RPS.
- Sweep `w` (2, 4, 8, 16, 32) at `c:1`, then fix best-`w` and sweep
  `c` on the I/O-bound handler.
- One change at a time, before/after numbers in the commit that lands
  the change; construction cost counts (e.g. prebuild time added to
  reload latency must be measured, not assumed).
- fd budget: `ulimit -n` 65k+ before high-RPS runs.

## Pointers

- Master protocol: `docs/20260719-002000-pool-protocol.md` (this repo)
- Janus data plane / ring: `dataplane.go`, `ring.go` (this repo)
- DSL hot path: `rip/packages/server/server.rip` (rip repo)
- Spawn pattern reference: `rip/packages/swarm/swarm.rip`
- v3 baseline (measured ~20k RPS at c:1): `rip-lang/packages/server`
