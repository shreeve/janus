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

**No levers remain open as of 2026-07-20: the shipping spree below
closed the list.** Everything is shipped-with-measurement, measured-out
(the lock collapse: throughput-neutral, landed on simplicity — see
Measured results), deferred-for-cause, or fantasy. The next candidates
are the deferred rows (#6 static bypass, #7 GOMAXPROCS split, #8
hand-rolled proxy, #9 kTLS) — all gated on a real tenant or new
evidence.

| # | Lever | Expected win | Cost | Verdict |
| --- | --- | --- | --- | --- |
| 1 | Raise `c` (8–32) for I/O-bound apps, watch off | 2–10x per worker | ~zero (protocol opt-in exists) | **Shipped 2026-07-20** (`-c` flag) — measured 7x clean 200s/s at c:8 on the 5ms handler, capacity-exact: 503s vanish when w×c ≥ conc (see Measured results) |
| 2 | Janus micro-cache + request coalescing (anonymous GETs) | 10–100x on cacheable pages | Medium-high (correctness) | **Shipped 2026-07-20** as the `cache` capability ([spec](20260720-033201-capability-microcache.md)) — measured **~320–380x clean-200 throughput** on the capacity-bound 5ms route (366→118k, 361→137k 200s/s, w:2 c:1 conc:64, interleaved; gate was 10x), worker sees ~1 req/s per key at ttl 1s; ping-class floor measured **1.6–2.5x** (above the ~1.3x prediction: a HIT also deletes the proxy+UDS hop); stampede conc:64 cold = **1** origin request; Cookie-bypass ≈ cache-off within session noise (see Measured results) |
| 3 | Manager prebuilds app once per dirty epoch; workers boot artifact (+`--bytecode`) | Reload/boot 2–4x; RSS drops | Low-medium | **Shipped 2026-07-20** (rip `8333218`) — per-worker RSS ~137–145MB → 33–40MB (~3.7x, ~105MB/worker); reload w:8 ~470ms → ~170ms (~2.7x, no longer scales with w); boot-to-all-ready w:8 ~650ms → ~300ms (~2x). Bytecode half NOT viable on Bun 1.3.14 (ESM bytecode needs `compile:true`; CJS rejects top-level await) — revisit when Bun ships ESM bytecode (see Measured results) |
| 4 | DSL fast path (context allocation, route buckets) | 1.3–2x per worker ping-class | Medium | **Shipped 2026-07-20** (rip repo, 3 measured cuts) — in-process hot loop ~2404 → ~1690 ns/req (~−30% worker CPU per request; cross-session endpoints, per-cut interleaved ratios); route index adds −12–15% at 40 routes, parity at 1 route. Full-stack RPS unchanged (Janus-bound, as predicted) |
| 5 | `ReverseProxy.BufferPool` + proxy-struct reuse + idle conns scaled with `c` | 5–15% of Janus CPU | Trivial (~20 lines) | **Shipped 2026-07-19** — measured +20–37% RPS (see Measured results), far above the estimate |
| 6 | Static file bypass at Janus (registration declares static roots) | Large for asset-heavy tenants; zero for APIs | Medium (protocol extension) | Later (need a real tenant) |
| 7 | GOMAXPROCS split / core pinning (Janus 2–4 procs, workers own the rest) | 5–15%, mostly tail latency | Low | Measure-first |
| 8 | Hand-rolled UDS proxy replacing httputil.ReverseProxy | 20–40% of the Go-side share only | High (streaming/trailers/upgrades correctness) | Later |
| 9 | kTLS TX-only (TLS 1.3, Linux) | 10–30% of TLS CPU on large bodies | High, fragile under Caddy | Later; watch golang/go#44506 |
| 10 | h2c or QUIC to workers | Negative to zero | — | Fantasy |

### 1. Raise `c` — the biggest lever hiding in plain sight

> **Raise `c` when handlers wait; raise `w` when handlers work.**

Concurrency is not parallelism: a worker is one JS thread, so `c`
interleaves I/O waits (it cannot add CPU), while `w` adds processes
across cores (real parallelism). Bun is an event-loop runtime; at
`c:1` a worker sits idle for the full duration of every DB query or
upstream fetch. For I/O-bound apps, `c:8–32` with the same worker
count is a near-free 2–10x (measured: 4x on a 5ms handler, with busy
bounces going to zero), and halves RSS versus scaling `w`. The pool
protocol already defines higher `c` as an opt-in (watch off). Keep
`c:1` for CPU-bound handlers and for watch mode. Capacity = `w × c`.

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

### 5. Trivial Janus proxy tuning (shipped 2026-07-19)

- `ReverseProxy.BufferPool` (sync.Pool of 32KB buffers) — shipped;
  previously every response copy allocated.
- One `ReverseProxy` per socket path, built lazily and reused — shipped;
  per-attempt state (retryability, the attempt's error) moved to a
  context value so the structs carry no per-request state.
- `MaxIdleConnsPerHost` stays 32 — right for c:1, which is the only
  shipped `c`; scale it alongside a future `c` raise.
- TLS session resumption is on by default in Go/Caddy — verify with
  `openssl s_client -reconnect`, expect no work needed.

Measured effect (M5, interleaved A/B, ping-class, HTTPS full stack):
+14–20% at conc=w, +37% at conc:64 (w:2 conc:64 49.6k → 68.2k RPS;
w:8 conc:64 50.9k → 69.9k). See Measured results.

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
5. **`/ready` must carry truth in status codes**: 200 only when ready,
   503 while booting or draining. v3 answered 200 in every state with
   the truth only in the body — a trap for any `res.ok` consumer. The
   v4 worker implements this correctly; keep it that way.

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
  prebuild-once recovers without fork. The memory multiplier is
  **RSS ≈ w × (JSC baseline ~30–50MB + app retained heap)**; the
  honest levers are keeping the compiler out of workers (#3), small
  `w` with higher `c` (#1), and maxRequests/maxSeconds recycling.
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
- **Janus fast-path for `/ping`-class endpoints**: accelerates
  endpoints users don't call; skip unless health-check volume is
  measurably material.

## Measurement discipline

Claims are verified, not asserted (both repos' standing rule). For the
stress phase:

- Bench over TLS through the full stack (client → Janus → UDS →
  worker), ping-class AND a DB-ish 1–5ms handler; `oha` or `wrk` with
  keep-alive; report p50/p99 alongside RPS.
- Sweep `w` (2, 4, 8, 16, 32) at `c:1`, then fix best-`w` and sweep
  `c` on the I/O-bound handler. Record worker RSS and Janus CPU share
  alongside RPS/p50/p99 — memory and attribution regressions hide
  behind flat throughput numbers.
- One change at a time, before/after numbers in the commit that lands
  the change; construction cost counts (e.g. prebuild time added to
  reload latency must be measured, not assumed).
- fd budget: `ulimit -n` 65k+ before high-RPS runs.

## Measured results (2026-07-19)

**Baseline caveat, applying to every section below:** the canonical
cold-machine baseline is pending a reboot — sessions ran on a warm,
multi-day-uptime rig with background load, so absolute numbers drift
(identical-config legs measured ±10–24% apart); interleaved ratios are
the comparisons to trust.

Phase 8 stress run. Machine: Apple M5, 10 cores, 32GB, macOS Darwin 25.
Bun 1.3.14, Go 1.26.5, Caddy v2.11.4, oha 1.14.0. `ulimit -n` 65536 on
Janus, the manager, and the bench shell. Full stack over HTTPS with
keep-alive: oha → Janus (TLS, `*.ripdev.io` certs) → UDS → Bun worker,
ping-class DSL route returning `{"ok":true}`. Watch OFF
(`RIP_ENV=production`), `c:1`, 15s runs, first warmup run discarded.
p50/p99 from oha's latency percentiles; `conc` is client concurrency.

**The 20k RPS target is comfortably exceeded**: every configuration at
conc ≥ 16 measured ≥ 47k RPS end to end, and w:2 needs only conc:2 to
clear 20k when the machine is cool.

### w sweep at c:1 (pre-fix baseline, all-200 runs)

| w | conc=w RPS | p50 | p99 | conc:64 RPS | p50 | p99 |
| --- | --- | --- | --- | --- | --- | --- |
| 2 | 23,948 | 0.07ms | 0.26ms | 71,804 | 0.67ms | 3.06ms |
| 4 | 33,419 | 0.10ms | 0.33ms | 61,025 | 0.78ms | 3.74ms |
| 8 | 48,359 | 0.14ms | 0.48ms | 64,762 | 0.74ms | 3.48ms |
| 16 | 59,773 | 0.21ms | 0.88ms | 64,057 | 0.76ms | 3.36ms |
| 32 | 54,310 | 0.45ms | 2.09ms | 56,801 | 0.88ms | 3.62ms |

The knee at matched concurrency is w:16 (w:32 loses to spawn overhead
and scheduler pressure at 10 cores). Under conc:64 the curve is nearly
flat across w — the bottleneck at high concurrency is Janus-side
per-request cost, not worker count (see attribution), which is why the
proxy tuning below moved the number and more workers did not.

### Attribution: Janus vs direct UDS (w:2 pool, one worker socket)

| Path | conc | RPS | p50 | p99 |
| --- | --- | --- | --- | --- |
| oha → worker UDS directly | 1 | 67,060 | 0.01ms | 0.03ms |
| oha → worker UDS directly | 2 | 105,601 | 0.02ms | 0.04ms |
| oha → Janus (TLS) → UDS | 1 | 16,471 | 0.05ms | 0.19ms |

A worker answers in ~15µs; the same request through Janus takes ~60µs —
Janus (TLS + proxy + routing) is ~75% of per-request latency on this
route, so Janus-side cost dominates and the §5 tunings were justified.

### Busy-503 fix, before/after (interleaved A/B, same thermal state)

5ms-sleep handler (`/io`), w:8, c:1, conc:64, 15s:

| | 200s (15s) | client 503s (15s) | p50 | p99 |
| --- | --- | --- | --- | --- |
| before | 22,609 | 758,206 | 0.84ms | 6.66ms |
| after | 22,949 | 119,002 | 6.40ms | 13.70ms |

Real work is capacity-bound (w × 1/5ms ≈ 1,600/s) and unchanged; the
fix cuts client-visible 503s 6.4x. Each remaining 503 now means all 8
workers were genuinely busy after Janus tried every one — before, it
meant least_conn's single pick happened to be busy. p50/p99 rise
because requests now find capacity instead of failing fast.

### Proxy tuning (§5), before/after (interleaved A/B, ping-class)

| Config | before RPS | after RPS | Δ |
| --- | --- | --- | --- |
| w:2 conc:2 | 13,848 | 15,825 | +14% |
| w:2 conc:64 | 49,630 | 68,174 | +37% |
| w:8 conc:8 | 36,566 | 43,778 | +20% |
| w:8 conc:64 | 50,856 | 69,883 | +37% |

Peak observed on a cool machine after both changes: **98,702 RPS**
(w:2, conc:64, p50 0.49ms, p99 2.78ms, zero non-200s). Sustained
thermal state costs ~30% on this fanless-class silicon; the A/B tables
above are interleaved runs at matched temperature and are the honest
comparison. Run-to-run variance on absolute numbers is large (w:2
conc:2 measured 13.8k–30.6k across the day); ratios were stable.

### Informational: one c:8 sweep on the 5ms handler (w:8, conc:64)

| c | 200s/s | client 503s (15s) | p99 |
| --- | --- | --- | --- |
| 1 | ~1,530 | 119,002 | 13.7ms |
| 8 | 6,083 | 0 | 114ms |

Raising `c` to 8 on the I/O-bound route delivered ~4x real throughput
and eliminated 503s entirely (capacity w×c = 64 = conc), confirming
lever #1's headroom. Run with a temporary local worker edit; the
shipped worker stays c:1 pending the protocol's opt-in knob.

### c-knob sweep (2026-07-20)

Re-run with the shipped `-c` knob (manager CLI `-c/--concurrency`,
refused with watch on), same rig as Phase 8: M5, Bun 1.3.14, Go 1.26.5,
Caddy v2.11.4, oha 1.14.0, `ulimit -n` 65536, HTTPS full stack, 15s
runs, first warmup discarded. Caddy rebuilt at 18af04e (includes the
reload split-brain fix). Manager run from a clean worktree of rip main.
Rig sanity: ping w:2 c:1 conc:64 measured 97,013 RPS (warmup 100,049
discarded) — top of the expected 70–100k band, rig equivalent to the
Phase 8 runs.

Ping-class, conc:64, interleaved A/B pairs (c:1 vs c:8 per w):

| Config | RPS | p50 | p99 | non-200s |
| --- | --- | --- | --- | --- |
| w:2 c:1 (pair A) | 97,013 | 0.49ms | 2.78ms | 0 |
| w:2 c:8 (pair A) | 97,107 | 0.50ms | 2.80ms | 0 |
| w:2 c:1 (pair B) | 98,950 | 0.49ms | 2.75ms | 0 |
| w:2 c:8 (pair B) | 81,451 | 0.54ms | 3.41ms | 0 |
| w:16 c:1 (pair A) | 76,147 | 0.61ms | 3.67ms | 0 |
| w:16 c:8 (pair A) | 87,252 | 0.55ms | 3.08ms | 0 |
| w:16 c:1 (pair B) | 72,088 | 0.63ms | 3.83ms | 0 |
| w:16 c:8 (pair B) | 81,970 | 0.57ms | 3.32ms | 0 |

At w:2 the knob is invisible (ratios 1.00 and 0.82 — the second pair's
c:8 leg ran hottest; noise, not signal). At w:16 c:8 beats c:1 by the
same ratio in both pairs (1.15, 1.14) — with 16 workers at c:1,
least_conn picks land on busy workers often enough that Janus's
bounce-and-retry churn costs ~13%; c:8 absorbs arrivals without the
retry hop.

Client-concurrency escalation on the two best configs (capacity = w×c):

| Config | conc:64 | conc:128 | conc:256 |
| --- | --- | --- | --- |
| w:2 c:8 (cap 16) | 97,107 | 98,834 (p50 0.94ms, p99 5.49ms) | 82,098 (p50 2.24ms, p99 14.36ms) |
| w:16 c:8 (cap 128) | 87,252 | 84,890 (p50 1.08ms, p99 6.49ms) | 89,248 (p50 2.17ms, p99 11.39ms) |

All rows zero non-200s. Higher conc buys latency, not throughput.

`/io` (5ms sleep), w:8, successful-200s/s with bounced 503s separate:

| c | conc | 200s/s | 503s (15s) | p50 | p99 |
| --- | --- | --- | --- | --- | --- |
| 1 | 64 | 1,536 | 132,953 | 5.80ms | 12.50ms |
| 4 | 64 | 6,034 | 87,525 | 5.43ms | 12.27ms |
| 8 | 64 | 10,695 | 0 | 5.91ms | 7.53ms |
| 16 | 64 | 10,601 | 0 | 5.88ms | 8.13ms |
| 32 | 64 | 10,655 | 0 | 5.89ms | 8.54ms |
| 8 | 128 | 11,896 | 64,373 | 7.30ms | 16.38ms |
| 16 | 128 | 22,252 | 0 | 5.62ms | 7.77ms |
| 32 | 128 | 22,219 | 0 | 5.67ms | 7.25ms |

The curve is capacity math: 200s/s ≈ conc/(5ms + overhead) once
w×c ≥ conc, and 503s vanish at exactly that point (c:8 → cap 64 =
conc:64 clean; conc:128 needs c:16). At c:8/conc:64 the shipped knob
measures 10,695/s clean — 7.0x the c:1 baseline in the same session
and well above the 6,083 temp-edit number recorded on 2026-07-19.
Past saturation, extra c is free but buys nothing (c:16 ≈ c:32).

Thermal note: absolutes sagged through the session (w:2 c:8 conc:64
read 97.1k in the first pair and 81.5k twenty minutes later, −16%);
the interleaved ratios stayed stable, so ratios are the comparisons
to trust. A planned cool-machine repeat of the peak config was lost
to a tooling failure at the end of the session; the numbers above are
complete for every swept config.

**Verdict: the 98,702 peak stands.** Best counted run was 98,950 RPS
(w:2 c:1 conc:64) — +0.3%, a statistical tie, and the discarded warmup
read 100,049 — so the machine reproduces the peak but `c` does not move
it: ping-class is Janus-bound (the attribution table's ~75%), and no
w×c×conc combination pushed past ~99k. The lever ranking is unchanged
and sharpened: lever #1 is confirmed as capacity-exact for I/O-bound
work (7x clean at saturation, 503s to zero, and now shipped rather
than a temp edit), it additionally buys ~14% on ping-class at high w
by killing bounce-retry churn, and the path past ~99k remains lever #4
(DSL fast path) for the worker share and lever #2 (micro-cache) for
the only 10x+ story.

### Hot-path lock collapse (2026-07-20)

Raw per-leg data (every run, warmups, one failed leg, and the load
averages that explain the variance):
[20260720-030700-bench-raw-lock-collapse.txt](20260720-030700-bench-raw-lock-collapse.txt).

The data plane's per-request cost included three `dp.mu` acquisitions
(selection, proxy lookup, release) plus a fourth on failure (health
marking).
Shipped change: `acquireUpstream` returns the socket's `upstreamState`
(now carrying the reusable per-socket proxy) under ONE acquisition;
inflight counts and the unhealthy deadline are atomics, so release and
health marking are lock-free. Selection semantics are unchanged
(least_conn, uniform random tie-break — now reservoir sampling, pinned
by a new uniformity test — unhealthy skip, doorbell exclusion). Also
landed in the same change: manual host:port cut in
`normalizeHostHeader` (SplitHostPort allocates an `*AddrError` on every
portless Host), `resolveHost` returns a shallow snapshot instead of
cloning both slices (registry writes replace slices wholesale, so
published backing arrays are immutable), lazy `tried` map (allocated
only on retry), BufferPool stores `*[32<<10]byte` to avoid boxing the
slice header per response copy, and the NopCloser body shield skips
bodyless requests.

Interleaved A/B, same rig (M5, Bun 1.3.14, Go 1.26.5, Caddy v2.11.4,
oha 1.14.0, `ulimit -n` 65536), HTTPS full stack, ping-class, c:1, 15s
runs, warmups discarded. Legs alternated before/after in both orders
within each config so thermal drift cannot favor one binary. All legs
zero non-200s.

| Config | pairs | median before | median after | median ratio | ratio range |
| --- | --- | --- | --- | --- | --- |
| w:2 conc:64 | 8 | 95,798 | 93,906 | 0.96 | 0.74–2.83 |
| w:16 conc:64 | 6 | 82,830 | 87,334 | 1.02 | 0.84–1.30 |
| w:16 conc:128 | 6 | 89,230 | 91,280 | 1.03 | 1.00–1.56 |

**The honest verdict: throughput-neutral within noise.** The
contention rows lean +2–3% at the median and the cleanest block of the
session (the four cooled-down w:16 conc:128 pairs: before 87.8–91.7k,
after 89.8–93.5k) reads +1–3%, but pair-to-pair variance on this rig
this session (background load; two visibly disturbed legs with p99
11–22ms) swamps any claim. w:2 is a statistical tie. The change lands
on simplicity, not speed: one lock acquisition per request instead of
three (four on a failure), two fewer lock-touching methods, the
`proxies` map folded into
`upstreamState`, and strictly less allocation per request — with the
ceiling story unchanged. Best counted legs read 102.3k (before) and
98.0k (after): both inside the established 95–102k cool-band, so the
~99k ceiling did not move, consistent with the attribution table —
`dp.mu` was never the bottleneck at these RPS; TLS + proxy CPU is.
The lever ranking is unchanged.

### Prebuild-once (2026-07-20)

Lever #3 shipped in the rip repo (`8333218`): the manager builds ONE
ESM artifact per boot epoch (`Bun.build` + a `.rip` plugin over the
compiler it already runs on, into the pool's run tmpdir), and workers
— themselves prebuilt to plain JS at startup — boot it loader-free.
Never-stale composes automatically (new epoch = new artifact, built
inside the single-flight boot after the dirty check); a build failure
takes the exact cached-boot-failure path; direct-entry `APP_ENTRY`
workers keep the loader. Bundling freezes each module's `import.meta`
path fields to its source location, so `import.meta.dir`-relative
file serving is byte-identical to unbundled behavior.

Rig: M5, 10 cores, 32GB, Bun 1.3.14, manager + stub Janus control
socket over UDS, 3 interleaved before/after legs (background load —
trust the ratios). Suite: 103/103 package tests (3 new pins:
loader-free artifact boot, `import.meta.dir` preservation, loud build
rejection); root 5425/0.

Per-worker RSS (the compiler heap leaving workers):

| | before | after |
| --- | --- | --- |
| at boot | ~137–145MB | 32.7MB |
| after 1k requests | ~137–145MB | 37.7–40MB |

~3.7x smaller, ~105MB less per worker — ~850MB recovered at w:8.

Reload latency (save → fresh response), per-leg medians:

| Config | before (3 legs) | after (3 legs) |
| --- | --- | --- |
| w:2 | 193 / 289 / 254ms | 156 / 141 / 163ms |
| w:8 | 470 / 536 / 408ms | 150 / 178 / 175ms |

~2.7x at w:8, and reload no longer scales with worker count — every
worker used to recompile the app; now one build serves all `w`.

Boot, spawn → all-ready at w:8 (artifact build included):

| before (3 legs) | after (3 legs) |
| --- | --- |
| 627 / 650 / 671ms | 215 / 350 / 319ms |

~2x faster to all-ready.

**Bytecode verdict: NOT viable on Bun 1.3.14.** ESM bytecode requires
`compile:true` (a standalone executable), and the one bundle format
bytecode accepts (CJS) rejects top-level await — which idiomatic Rip
(module-level dammit) produces routinely. The plain-JS artifact
ships; revisit when Bun supports ESM bytecode, at which point the
artifact is one flag away from kernel-shared read-only pages.

### Micro-cache + coalescing (2026-07-20)

Lever #2 shipped as the `cache` capability
([spec](20260720-033201-capability-microcache.md) — revision 2,
implemented as written; both test layers green: `go test -race` and a
25-case `test.sh` cache group). Raw legs:
[20260720-062700-bench-raw-microcache.txt](20260720-062700-bench-raw-microcache.txt).

Rig: M5, Bun 1.3.14, Go 1.26.5, Caddy v2.11.4, oha 1.14.0, `ulimit -n`
65536, HTTPS full stack, 15s runs, 5s warmups discarded, interleaved
off/on legs. **Not the post-reboot canonical baseline**: 3-day uptime,
background load 3–8 through the session — identical-config legs
drifted ±10–24% (ping-off read 58.4k and 72.3k two minutes apart), so
ratios, and only ratios spanning 10x+, are load-proof here. Tenant:
the real rip manager (main, prebuild included), w:2 c:1,
`RIP_ENV=production`; one app claiming `bench.ripdev.io` (site cache
**on**, ttl 1s) and `api.ripdev.io` (site cache **off**) so off/on legs
hit identical workers through identical TLS.

**1) The 10x gate — capacity-bound route (5ms handler, conc:64):**

| Leg | 200s/s | non-200 (15s) | p50 | p99 | worker req/s |
| --- | --- | --- | --- | --- | --- |
| off pair-A | 366 | 370,461 | 2.43ms | 6.72ms | ~390 |
| on pair-A | 118,265 | 0 | 0.37ms | 3.22ms | **~1** (16 misses/15s) |
| on pair-B | 137,376 | 0 | 0.34ms | 2.53ms | **~1** (15 misses/15s) |
| off pair-B | 361 | 341,063 | 2.57ms | 7.58ms | ~390 |

**Gate passed: ~320–380x on clean-200 throughput** (366→118,265;
361→137,376), against the 10x bar. The off legs are the spec's
arithmetic made flesh: w×c/5ms ≈ 400/s clean, everything else shed as
capacity 503s (`Retry-After`), ~23–25k RPS counting the sheds — the
cache-on legs beat even that raw-RPS number 4.7–5.9x while turning the
error rate to zero. Cache-on rides **above** the old ~99k proxied
ceiling (118–137k) because a HIT deletes the proxy + UDS hop
entirely, not just the worker. Worker-side truth: 15–16 requests per
15s leg — ~1 req/s per key at ttl 1s, regardless of the ~2M client
requests, exactly the stampede-to-trickle contract.

**2) Ping-class floor (honesty row, no gate):**

| Leg | RPS | p50 | p99 |
| --- | --- | --- | --- |
| off pair-A | 58,432 | 0.65ms | 7.30ms |
| on pair-A | 143,981 | 0.32ms | 2.30ms |
| on pair-B | 118,375 | 0.37ms | 3.10ms |
| off pair-B | 72,280 | 0.60ms | 5.11ms |

Measured **1.6–2.5x** — above the predicted ~1.2–1.4x. The
prediction modeled a HIT as "still pays TLS + routing"; it does, but
it also deletes the ReverseProxy machinery and the UDS round trip,
whose Janus-side share the attribution table had folded into "proxy."
Still the floor of the win curve, and still nowhere near the
capacity-bound story.

**3) Coalescing stampede:** three cold-key bursts at conc:64 → worker
requests **1, 1, 1**; all 64 clients 200 with identical bodies; p99
35–38ms (TLS conn setup for 64 fresh oha connections plus one 5ms
origin round trip — not 64× queueing). At conc:256 (past the 64-waiter
cap) the shape depends on arrival hardness, both observed and both
correct: a hard simultaneous burst produced 1 fill + 172
`waiter_overflow` fall-throughs, whose excess over w:2 c:1 capacity
the data plane shed as 246 capacity 503s + `Retry-After` (exactly
no-cache behavior — the cache manufactured none of them); a
staggered-arrival re-run produced `{200: 256}` — 1 miss + 40 coalesced
+ 215 hits, overflow 0.

**4) The zero case:** every request carrying `Cookie: a=1`, off vs on:
27,514 vs 24,843 RPS (clean 200s/s 373 vs 363 — capacity-identical).
The −10% RPS delta is below this session's identical-leg drift
(±10–24%), so: indistinguishable from noise, as required — the bypass
path adds three header-map probes and two atomic adds.

**5) Reload interaction:** watch-mode tenant under conc:32 load,
save mid-run: the **first** post-save distinct response was the new
code (`v:2`; no stale body observed in any post-cut probe), purge
delta 3 (doorbell PUT + publishes), `fenced_stores` delta 2 — two
straddling fills correctly refused to store across the cut.

The lever table's claim is settled precisely as restated: **10–100x+
on capacity-bound routes** (measured ~320–380x on the 5ms route at
conc:64 — the multiplier is the handler-cost-to-ceiling gap, so slower
handlers measure higher), **~1.6–2.5x on ping-class (Janus-bound)
routes**, **zero by design** on Cookie/auth traffic.

### Next-best lever

The ranked list is closed: #1, #2, #3, #4, and #5 are shipped with
measurements above. What remains is deferred-for-cause — #6 (static
bypass) and #8 (hand-rolled proxy) want a real production tenant's
traffic shape, #7 (GOMAXPROCS split) wants a profile showing scheduler
pressure, #9 (kTLS) waits on golang/go#44506. Operationally, the
biggest available wins are now configuration, not code: enable `cache`
on public anonymous routes (10–100x+ where it applies) and raise `c`
on I/O-bound apps (capacity-exact, measured 7x).

## Pointers

- Master protocol: `docs/20260719-002000-pool-protocol.md` (this repo)
- Janus data plane / ring: `dataplane.go`, `ring.go` (this repo)
- DSL hot path: `rip/packages/server/server.rip` (rip repo)
- Spawn pattern reference: `rip/packages/swarm/swarm.rip`
- v3 baseline (measured ~20k RPS at c:1): `rip-lang/packages/server`
