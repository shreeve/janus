# Capability: cache (micro-cache + request coalescing)

> **Revision 2, adversarial review (2 reviewers) folded in —
> IMPLEMENTED as specified.** Two independent adversarial reviews
> (HTTP-caching correctness; systems / DoS / concurrency) attacked
> revision 1. Both found the same purge race independently and
> converged on the same fix (generation-fenced stores); their
> CRITICALs, HIGHs, and MEDIUMs are folded in below, and the former
> "Open questions for adversarial review" section is now a settled/open
> ledger. Where the two reviews contradicted each other, the resolution
> is recorded in place. The implementation lives in `cache.go`
> (sharded store), `cache_serve.go` (decision table, coalescing, fill),
> and `cache_config.go` (directive, cascade); tests in `cache_test.go`
> and the `cache` group of `test.sh`.

Cold Caddyfile capability. When enabled for a site, Janus keeps a
short-TTL in-memory copy of anonymous `GET` responses and answers
repeats from memory without touching a worker. Concurrent misses on
the same key coalesce into one origin request (singleflight); the rest
wait for the fill.

| | |
| --- | --- |
| **Order** | **3** (after **ping**, **control**; sits on the Phase 4 data plane) |
| **Surface** | Cold config (Caddyfile); counters on hot `/1.0` |
| **Cascades** | Yes — global default → site override (process-wide exceptions: `max_bytes`, `max_app_share`) |
| **Built-in default** | off (when unset at every level) |
| **Module** | Site handler (data plane), between ping and upstream selection |
| **Status** | **Shipped** — implemented per revision 2; both test layers green; measured (see Measured results) |

## Why it exists

Lever #2 of the performance map
([`20260719-165500-rip-server-performance.md`](20260719-165500-rip-server-performance.md)
§2). The honest claim, per the review's arithmetic:

- **Where 10–100x applies: capacity-bound routes** — public,
  anonymous, repeatedly-requested pages whose handlers cost real time
  (a DB-backed landing page, a rendered product page, a public JSON
  feed with a 5ms+ handler). Capacity math: a 5ms handler at w:2 c:1
  serves ~400 req/s; cached, the same page rides the ~90k+ TLS-serve
  ceiling — a genuine ~200x on the measured configuration. A stampede
  of N clients per second on one page becomes ~1 worker request per
  TTL (default ~1s), regardless of N.
- **Where the win is ~1.3x, by arithmetic: ping-class routes.** The
  attribution table in the performance doc shows Janus-side cost (TLS
  + routing + proxy) is already ~75% of per-request latency, and the
  measured ~99k ceiling is Janus-CPU-bound. A cache hit deletes the
  worker (~15µs, mostly on other cores) and the proxy's Janus-side
  share, but still pays TLS + routing + a cache lookup. Expected
  ping-class win: **~1.3–1.7x** — real, but not the headline. The
  headline number must never be measured on a ping-class route (see
  Measurement plan).
- **Where the win is zero, by design:** APIs and personalized pages.
  Any request carrying `Cookie`, `Authorization`, or
  `Proxy-Authorization` bypasses the cache completely; any response
  carrying `Set-Cookie`, `no-store`, or `private` is never stored.
  Non-GET is never cached. For those workloads this capability
  changes nothing — levers #1 and #4 remain their story.

Coalescing ships in the same capability because it is the same
machinery (the fill path) and it is what saves the *cold* case: N
concurrent first requests on an empty key become 1 origin request and
N−1 waiters, instead of N worker hits. Without coalescing, a 1s TTL
still admits a full stampede once per second.

## Correctness spec

This is the heart of the capability. The danger is entirely
correctness: a wrong cache serves user A's page to user B, or
yesterday's code's output after a reload. Every rule below exists to
make those impossible, and every rule is enforced — a request or
response the rules cannot prove safe **bypasses the cache**; there is
no "probably fine" path. Bypass is the safe default because bypass is
exactly today's behavior.

### The decision, per request

Checked in order, after ping and after host resolution (the cache
never sees unknown hosts — those are already 404, worker untouched):

| Condition | Action |
| --- | --- |
| Site's effective `cache` is off | bypass |
| Method is not `GET` | bypass |
| Request has `Cookie`, `Authorization`, or `Proxy-Authorization` | bypass — never serve from cache, never store |
| Request has `Range` | bypass |
| Request has a conditional header (`If-None-Match`, `If-Modified-Since`, …) | bypass |
| Key (host+path+query) exceeds 8 KB | bypass — a minted key can never be megabyte-scale |
| Key carries a live do-not-coalesce mark (below) | bypass — no coalescing, no buffering, for one `ttl` |
| Fresh entry under the key, entry's app id matches the resolved record, variant matches (Vary, below) | **HIT** — serve from memory |
| Fresh entry whose stored app id ≠ the resolved record's id | treat as **MISS and evict** — a re-claimed host never serves the previous tenant's bytes |
| Fill in flight for this coalescing key, flight generation current, waiter slot free | **COALESCE** — wait for the fill |
| Otherwise | **MISS** — become the fill: proxy via the normal data plane, then decide storability |

Bypass means the request proceeds through the standard decision table
([pool protocol](20260719-002000-pool-protocol.md)) exactly as today
— doorbell, marked-503 retry, health accounting, all unchanged.

Request `Cache-Control` is **ignored** for the serve decision, and so
is its HTTP/1.0 twin `Pragma: no-cache`. A client's `no-cache` /
`no-store` does not force a worker touch: the micro-cache exists to
protect the origin from request volume, and the request header is
attacker-controlled — honoring it hands every stampede a one-line
opt-out. RFC 9111 §5.2.1 explicitly permits a shared cache to be
configured to ignore request directives. The developer force-refresh
recipe is the bypass table itself: `curl -H 'Cookie: x=1'` (or any
`Authorization` header) bypasses the cache today and forever; at a 1s
default TTL, waiting is also a real answer. (Settled — ledger Q1.)

### Cache key

Primary key: **normalized host + raw path + raw query**, and every
stored entry additionally carries the **app id** it was filled for.

- Host is the same normalization the data plane already applies to
  `Host` (lowercase, trailing dot stripped, `:port` dropped).
- Path and query are the **request-target bytes as received on the
  wire** (`RequestURI`) — never a decoded or re-encoded form. This is
  a load-bearing sentence, not a restatement: Go's `r.URL.Path` is
  *decoded* (`/a%2Fb` and `/a/b` collapse to one string), and
  `RawPath` is empty when the forms agree. Keying on decoded bytes
  merges resources many routers treat as distinct — fill `/a%2fb`,
  serve it under `/a/b` — which is precisely the poisoning class the
  no-canonicalization rule exists to make unreachable. A unit test
  pins `/a%2Fb` and `/a/b` as two keys.
- No canonicalization, no query-parameter sorting, no decoding. Two
  byte-different URLs are two keys. Random query strings can force
  misses — but a miss is exactly today's behavior, key cardinality is
  bounded by the byte-cap eviction, and the admission doorkeeper
  (below) keeps one-hit-wonder keys out of the store entirely.
- The app id in the entry is what makes key identity safe against
  host re-claim: host normalization drops `:port`, and a host
  deleted then re-registered under a new app id within `ttl_max`
  must never serve the previous tenant's bytes. A HIT whose entry
  app id differs from the resolved record's id is a MISS-and-evict
  (decision table above). The purge index wants entries by app id
  anyway; this is the same bytes doing double duty.

Secondary key (variant selection): the values of the request headers
named by the **stored response's `Vary`** — standard HTTP semantics.
An entry remembers its `Vary` header names and the request header
values it was filled under; a lookup matches only when the current
request's values for those names are identical.

**A primary key holds a *set* of variants**, each separately
LRU-accounted, all dropped together on purge. (An implementation that
stores one variant per key would make `de`/`en` traffic thrash — the
acceptance sketch's Vary case requires concurrent variants, and now
the normative text does too.)

**One value-extraction function, used by both keys.** The variant key
and the coalescing key extract request-header values with the
identical function: multiple header lines are joined with `", "` in
arrival order; an **absent** header is its own value, distinct from
an empty one (over-splits, never shares). This shared function is the
precondition for the coalescing-safety argument (ledger Q10).

### Vary: bounded allowlist

`Vary` is honored only within a fixed allowlist:

```text
Accept, Accept-Encoding, Accept-Language
```

- Response `Vary` naming only allowlisted headers → storable, variant
  keyed as above.
- Response `Vary` naming **anything else** — `Vary: *`, `Vary:
  Cookie`, `Vary: User-Agent`, `Vary: X-Forwarded-For`, … → **never
  stored**; the response streams to its client and the cache forgets
  it.

**Vary parsing is fail-closed, mechanically:** header-name comparison
is case-insensitive (HTTP/2 delivers lowercase — a byte-compare
against `Accept-Encoding` would silently disable the feature on every
h2 response); values are OWS-trimmed and comma-split; multiple `Vary`
lines are combined; any member not recognized after that → never
store.

The allowlist bounds variant cardinality (an unbounded `Vary` is an
unbounded memory key) and doubles as a tenant escape hatch: an app
that computes a response from any request header the cache does not
key on can say `Vary: <that header>` and is thereby guaranteed never
to be cached wrongly — it simply is never cached.

Allowlisted header values are used **raw** — no normalization, no
`Accept-Encoding` bucketing. The two reviews split here: the systems
review proposed bucketing `Accept-Encoding` to the supported-decoder
power set to stop variant minting; the correctness review argued any
normalization reintroduces the canonicalization surface for zero
correctness gain. Resolution: **raw values stand.** The wrong-bytes
risk lives entirely in the *undeclared*-variance case, which the
`Content-Encoding` never-store row (below) closes; and the
variant-minting attack the bucketing targeted is already killed by
the admission doorkeeper — the systems review's own fairness fix —
since minted variants are one-hit wonders that never enter the store.
The residual cost (exotic `Accept-*` strings never coalesce) degrades
to exactly today's behavior. (Ledger Q4.)

### Never store

A fill's response is stored only when **all** of these hold:

| Rule | Why |
| --- | --- |
| Status is exactly **200** | See below |
| No `Set-Cookie` header | A response that sets a session is per-client by definition; storing it leaks sessions. Non-negotiable |
| Response `Cache-Control` has no `no-store`, `no-cache`, or `private` | `no-store`/`private` are the origin's veto; `no-cache` demands revalidation machinery v1 does not have, so it is treated as a veto too |
| Response `Cache-Control` parses | An unparseable `Cache-Control` is a veto the cache failed to read — never store. The "cannot prove safe" principle, made a table row so the parser's fail direction is contractual |
| No `Expires` header, at all | `Expires: <anything>` with no `Cache-Control` would otherwise get the default TTL — the cache overriding an explicit origin veto because it declined to parse the header carrying it. Presence is the veto; no date parsing. Tenants that want caching plus `Expires` use `Cache-Control`, which wins per RFC anyway |
| No `Age` header | An `Age`-bearing response has already consumed freshness this cache would re-grant; there is no upstream shared cache in this topology, so the header is exotic — never store |
| No `Content-Encoding` (other than `identity`) unless `Vary` includes `Accept-Encoding` | The undecodable-bytes hazard: origin (or compression middleware) answers `Content-Encoding: gzip` with **no `Vary`** — one of the most common real-world misconfigs. Stored, that entry HITs for a client that never negotiated gzip, which receives bytes it cannot decode. Semantic variance is operator-accepted risk; **encoding variance is never acceptable** because the failure is not wrong content, it is garbage |
| `Access-Control-Allow-Origin`, if present, is exactly `*` | Buggy CORS middleware echoes the request `Origin` into `ACAO` without `Vary: Origin`; cached, one origin's `ACAO` breaks every other origin for `ttl` — intermittent, load-dependent CORS denial. Static `*` is origin-independent by construction; anything else is echoing or conditional |
| Effective freshness > 0 (TTL rules below) | `max-age=0` means "do not reuse" |
| `Vary` (if present) is within the allowlist | Above |
| Body fits `max_body` after buffering | Below |
| Body read completed without error **and** byte count equals any declared `Content-Length` | A fill whose upstream dies mid-body, or whose bytes disagree with its `Content-Length`, must not poison the key with a confident truncated entry for a full TTL. Abandon: leader gets whatever streamed, nothing stored |
| Response has no trailers and is not an Upgrade | Streaming/tunnel semantics don't buffer |
| Doorkeeper: key seen ≥ 2 times within the current window | Admission filter, below — one-hit-wonder keys (flood keys and long-tail URLs alike) never enter the LRU |
| Fill's generation snapshot equals the app's current generation | The purge fence, below — a fill that straddled a swap never stores pre-cut bytes into the post-cut cache |
| Not a worker-marked 503, not any 503, not anything but 200 | Marked 503s are flow control (`Rip-Worker-Busy` / `Rip-Worker-Draining`, pool protocol); caching one would convert one worker's momentary "busy" into a TTL-long outage for every client. Subsumed by 200-only, stated explicitly because the failure mode is so bad |

**Why 200 only.** 301/308 caching turns a misconfigured redirect into
a TTL-long trap — and worse, a cached redirect plus any open-redirect
bug is a classic poisoning *amplifier*: one crafted request turns a
reflected redirect into a site-wide TTL-long redirect to the
attacker. Negative-caching 404s wouldn't even buy DoS protection: a
miss-flood attacker uses random paths, which are distinct keys, so a
negative cache never hits (and Janus's own unknown-host 404 never
reaches a worker anyway). 206 requires Range machinery this
capability bypasses. Every non-200 status is some flavor of
exceptional, and exceptional responses are precisely where a stale
copy does the most damage per byte saved. v1 stores the boring case
only.

**Stored bytes are post-scrub bytes.** The data plane's
`ModifyResponse` already deletes `Rip-Mark` (the tenant's internal
correlation id) from every client response; the cache stores the
response **after** that scrub, so a cached response is byte-identical
to what the same client would have received on a miss. No internal
header can leak via the cache, and a HIT re-scrubs nothing.

**Stored headers exclude hop-by-hop headers** (`Transfer-Encoding`,
`Connection`, `Keep-Alive`, `TE`, `Trailer`, `Upgrade`); a HIT serves
the buffered length as `Content-Length`. Interim (1xx) responses —
a 103 forwarded during a fill — go to the leader's connection only;
they are never stored, and HIT/COALESCED responses carry none.

**Ordering relative to Caddy's `encode` handler:** the cache lives
*inside* the janus site handler; a Caddyfile `encode` directive wraps
*outside* it. Entries are therefore captured pre-`encode` (as the
origin produced them, post-scrub), and every response — HIT, MISS, or
BYPASS — passes through `encode` identically on the way out. The
never-store row above governs encodings applied by the *origin*; this
paragraph governs encodings applied by *Caddy*. Both are now stated
so no implementer discovers the ordering by accident.

### Buffering and the size cap

A fill buffers the body up to `max_body` (default 256kb). If the body
exceeds the cap — by `Content-Length` up front, or discovered while
reading a chunked body — the entry is **abandoned**: buffered bytes
flush to the client and the remainder streams through untouched. The
client sees a normal streamed response; the cache stores nothing.
Streaming responses (SSE and friends) therefore self-exclude by size
or by never ending.

Before store, the body buffer is trimmed to its exact length — a
growing buffer for a 200 KB body can retain a 512 KB backing array,
and the byte accounting must charge what is actually retained.

**Header-time early release:** when non-storability is already
visible in the response headers (status ≠ 200, `Set-Cookie`,
out-of-allowlist `Vary`, `Content-Length > max_body`, any never-store
row decidable without the body), the fill releases its waiters
immediately (fall-through, below) instead of holding them for a body
that will never be shared.

### TTL

Freshness for a storable response, in order:

1. Response `Cache-Control: s-maxage=N` (shared-cache directive wins,
   per RFC 9111), else `max-age=N` → freshness = `min(N, ttl_max)`.
2. Neither present → freshness = the site's `ttl` (default **1s**).

**The TTL anchor is fill-start:** an entry's age is measured from the
moment the fill's upstream request was sent — not from store time.
Freshness expiry and the `Age` header both derive from that anchor.
(Anchoring at fill-end would let a `ttl 1s` entry behind a 900ms
handler serve content up to ~1.9s old with a wrong `Age`; RFC 9111's
corrected-age calculation anchors conservatively at request time.)

Entries past their freshness are dead — evicted lazily on lookup and
by opportunistic sweep. There is no revalidation, no
stale-while-revalidate, no serving stale on error: expired means the
next request is a MISS that fills through the normal data plane.

Hits carry an `Age` header (whole seconds, from the fill-start
anchor). At 1s TTLs it is almost always `Age: 0`, but downstream
shared caches are entitled to it.

**Decision: opt-in per site, then default-TTL within.** Two designs
were on the table:

- **Opt-out** (cache everywhere, honor only explicit response
  `Cache-Control`) — rejected. Silently inserting a response cache in
  front of every tenant is exactly the "silent repair / drift
  tolerance" posture the repo rules forbid; and since today's tenants
  (Rip apps) set no `Cache-Control` at all, honoring only explicit
  headers would also make the capability a no-op — all cost, no win.
- **Opt-in** (`cache on` per site or globally, default off like
  **ping**) — taken. The operator who enables it is declaring "this
  site's anonymous GETs may be shared for `ttl`" — and from that
  declaration, caching responses that carry no `Cache-Control` at the
  default `ttl` is the entire point of a micro-cache. A tenant
  response can still veto per-response (`no-store`, `private`,
  `Set-Cookie`, out-of-allowlist `Vary`). The operator-accepted risk
  covers wrong-*content* (semantic variance); it never covers
  wrong-*encoding* (the `Content-Encoding` row) or credential
  leakage (the bypass rows) — that line is drawn explicitly.

### Coalescing

**Coalescing key:** the primary key **plus the request's values of
all three allowlisted headers** (extracted with the shared function
above). The response's `Vary` is unknowable until the fill returns,
so coalescing on the primary key alone could hand an
`Accept-Language: de` waiter a response filled under `en`.
Partitioning by the full potential secondary key over-splits (requests
differing in a header the response turns out not to vary on fill
separately) but can never share a wrong variant. Safe side taken —
and verified: the coalescing key is finer than or equal to every
legal storage variant key, since a storable response may `Vary` only
on a subset of those three headers; every waiter is byte-identical to
the leader on all three, so whatever subset the response names, the
waiter's values match the fill's by construction. (Ledger Q10.)

Semantics:

- First MISS on a coalescing key becomes the **leader**; its request
  proxies through the normal data plane. The leader snapshots the
  app's **generation** (purge section) before host/upstream
  resolution; the flight carries that snapshot.
- Later arrivals on the same key **wait** on that fill instead of
  dialing a worker — *if* the flight's generation is still current
  and a waiter slot is free. A waiter's own request is never sent
  anywhere — it is a GET with no body, so there is nothing
  half-delivered.
- **Waiter count is capped per coalescing key** (same shape and
  magnitude as the ring's `defaultWaiterCap`, **64**). Overflow
  **falls through to the data plane** — not a 503: fall-through
  preserves "never worse than today," because the data plane sheds at
  capacity anyway.
- **Waiter wait is capped by a wall clock** (ring-timeout order,
  **~15s**). Expiry → that waiter falls through individually. This
  deadline must be defined here because there is nothing to inherit:
  the shipped data plane has **no proxy response timeout** —
  `dataplane.go` sets a 3s dial timeout and an idle timeout only; no
  `ResponseHeaderTimeout`, no request deadline, and the site server
  runs `ReadTimeout 0` by design. A leader behind a wedged or
  trickling handler would otherwise hold every waiter forever.
  (Revision 1 claimed waiters were "already bounded by the proxy's
  timeouts" — that premise was false against the codebase and is
  struck; the leader itself holds exactly as long as any of today's
  slow requests, no better and no worse.)
- **A waiter whose client disconnects is abandoned**; the fill
  continues for the rest — same rule as the ring's holder-disconnect.
- **Fill produced a storable 200** → every waiter is served from the
  fill's response (counted as COALESCED, not HIT), and the entry is
  stored if the doorkeeper and the generation fence permit. Waiter
  service does not depend on the store: a doorkeeper refusal is an
  admission decision, not a safety one, and a generation-fenced fill
  has no waiters left to serve (the purge detached them). Sharing is
  gated by *storability* — the proof the response is not per-client
  — never by whether the bytes happened to be retained.
- **Fill produced anything else** — a non-storable 200
  (`Set-Cookie`, oversize, bad `Vary`), any non-200, a marked 503, a
  transport error → the leader's response goes to the leader alone
  (it may be per-client; sharing it is the session-leak disaster),
  and **every waiter falls through to the normal data plane
  individually**. With the waiter cap, that release pulse is bounded
  at ~64 — the same magnitude the ring cap already admits.
- **Do-not-coalesce mark:** after a fill returns non-storable for a
  key, the key is marked BYPASS — no coalescing, no buffering — for
  one `ttl`. Without it, a hot page that always sets `Set-Cookie`
  (or always exceeds `max_body`) enters a permanent cycle: gather
  waiters for one fill, release the herd, repeat — the cache
  converting smooth traffic into synchronized pulses forever on
  exactly the routes it cannot help. The mark is a few bytes per key
  and is **not** negative response caching: no body is stored, no
  200-only violation — it remembers "this key can't win" for one TTL.

Why fall-through and not a shared 503: without this capability, all N
requests would have hit the data plane anyway — fall-through is
exactly no-cache behavior, and the data plane already owns the
failure story (marked-503 retry, health, doorbell, 503 +
`Retry-After`). The cache is an optimization layer; on any path it
cannot prove safe, it must degrade to today, never to something
worse. Manufacturing a 503 for N−1 waiters because one fill hit one
bad socket would be the cache *creating* an outage.

Why the caps are mandatory and not garnish: the systems review's
capacity-shedding inversion. Without the cache, a burst of N on a
slow route hits `w×c` capacity and the marked-503 machinery sheds it
in microseconds. With uncapped coalescing, requests that today would
be *rejected fast* are instead *held as goroutines* — TLS conn +
request state each, at attacker-chosen concurrency, for the handler's
full (unbounded, see above) duration. And during a dirty window, only
the leader counts against the ring's 64-per-app holder cap; an
uncapped waiter crowd behind it would be the pool protocol's
explicitly rejected "unbounded queue amplification at the edge"
reintroduced one layer up. The correctness review argued a count cap
was unnecessary (a waiter is cheaper than its fall-through); the
systems review's inversion argument shows the comparison is against
fast-shed 503s, not fall-through work — the count cap stands.
(Conflict resolved; ledger Q2.)

One-shot re-coalesce on fill failure (promote a waiter to new leader)
is **rejected**: it serializes the retry — if the second fill also
fails (likely, same sick origin), remaining waiters have waited two
timeouts, then three. Fall-through releases everyone into machinery
that already owns partial failure in one bounded step. (Ledger Q3.)

### Interaction with the pool protocol: purge on swap, generation-fenced

The pool protocol's invariant: *Janus never admits a new request to a
generation the tenant has declared stale.* The letter-of-the-invariant
reading for a plain HIT survives review: a cached response was legally
produced by the then-current generation before the cut, like an
in-flight response that completes after the doorbell PUT ("finishing a
commitment"); serving it executes nothing.

**But the purge is mandatory, not chosen — and it must be fenced.**
Both reviews independently found the same race, so it is stated with
its orderings ("PUT" = any upstreams PUT + purge):

- **O1 — stores stale:** fill starts pre-cut → resolves OLD sockets →
  PUT lands, purge empties the app's keys → fill completes against an
  old worker (drain grace is 2–5s per the protocol) → **the store
  lands in the post-purge cache**. Old-generation bytes now serve for
  up to `ttl` after the cut — exactly the asterisk the purge was
  bought to remove.
- **O4 — serves stale without storing:** fill starts pre-cut; PUT
  lands; **waiters keep joining the coalescing key after the purge**;
  every waiter — including post-cut arrivals — receives the pre-cut
  response. A post-cut waiter handed old-generation output *is*
  materially "new admission to old code" from the client's
  perspective; the "finishing a commitment" carve-out covers the
  leader, never a request that arrived after the cut. The operator
  who saves and refreshes coalesces onto a pre-cut fill and sees old
  output — the precise scenario the purge exists to kill.
- **O5 — host re-claim:** app deleted/reaped, host re-registered as a
  new app id within `ttl_max`. The purge fired against the old app,
  but any O1-style resurrection — or the same race on DELETE — lets
  the new app serve the **previous tenant's** bytes.

An unfenced purge is therefore theater. The mechanism:

- **Per-app atomic generation counter.** Every purge event — any
  `PUT /1.0/apps/{id}/upstreams` (doorbell, sockets, or empty),
  `DELETE`, heartbeat reap, **and host claim via `POST /1.0/apps`** —
  increments the app's generation in the same critical section as the
  registry write, and drops all cache entries for that app.
- **Fills snapshot the generation before host/upstream resolution**
  (before `resolveHost` — snapshotting later loses the race to a
  registry read that predates the swap).
- **Store is rejected** unless the fill's snapshot equals the app's
  current generation. One integer compare. (Closes O1.)
- **The purge detaches the app's in-flight coalescing flights:**
  current waiters are released to **fall through to the data plane
  individually** — where they correctly find the doorbell and
  coalesce onto *one ring* — and, belt-and-suspenders, a new arrival
  never joins a flight whose generation is no longer current (it
  falls through or starts a fresh flight). (Closes O4.)
- **Stored entries carry the app id; a HIT validates it** against the
  resolved record and treats a mismatch as MISS-and-evict. (Closes O5
  unconditionally.)

Spurious rejections are safe by construction: a fill that legally
completed against the *new* pool but lost its store to a straggler
`readyWhen: 1` worker's full-list PUT costs one extra miss, nothing
more. The side effect is documented so nobody "optimizes" it away
later: with staggered publishes, a w:16 boot is 16 purges and
generation bumps — the cache stays cold through warmup, by design.
Any future scheme that diffs socket lists to skip "redundant" purges
must re-derive the entire race analysis above first.

What the purge buys beyond the race fix, stated as properties rather
than accidents:

- **The no-asterisk reload story is now true under load:** after the
  cut, nothing produced before the cut is served — the fence is what
  makes the sentence hold, not the purge alone.
- **Demand masking is structurally prevented:** a 100%-hit app cannot
  defer its own reload — the doorbell PUT empties the keys, the first
  post-save request MISSes and rings.
- It composes with coalescing during the dirty window: the purge
  empties the key and detaches flights, the first request MISSes, the
  fill rings the doorbell and holds per the protocol, and concurrent
  arrivals coalesce onto that one fill (waiter cap applies) — one
  ring and one first-delivery instead of N holders.

What the purge does **not** cover, said out loud: **worker death
without a PUT.** All workers crash → passive health marks them on
dial failure → no registry write, no purge → **a HIT can serve an app
whose every worker is dead, for up to `ttl`** (bounded by `ttl_max`),
and HITs count nothing toward health, so monitoring sees green until
the first MISS sees "all upstreams unhealthy." At 1s default TTLs
this is accepted; it is a behavior change to the health story and
tenant documentation must state it.

Purge needs entries indexed by app id; the key layout carries it (and
the HIT-validation rule reuses it).

### Memory bounds, admission, and eviction

- One process-wide pool: **`max_bytes`** (default 64mb). Accounting
  charges body (post-trim) + headers + key + a fixed per-entry
  overhead constant (~512 B — map buckets, list element, entry
  struct, stored Vary values). **`max_bytes` is an accounting bound,
  not an RSS promise:** Go's GC (GOGC=100) roughly doubles live heap,
  and churn is also an allocation attack — operators should expect
  RSS excursions of ~1.5–2x accounted bytes under small-entry churn,
  and size `max_bytes` accordingly. Stated so the knob is honest.
- **Admission doorkeeper, required in v1:** a store is admitted only
  for a key seen at least **twice within the current window** (small
  counting-Bloom / TinyLFU doorkeeper, reset periodically; every
  lookup bumps it). One-hit-wonder keys — which is what random-query
  floods, random-variant minting, *and* legitimate long-tail URLs all
  are — never enter the LRU. This single mechanism is what makes the
  "key cardinality is bounded by eviction" argument true under
  attack: without it, `GET /?r=<rand>` at a few thousand fills/s
  rotates the entire 64 MB pool in seconds and zeroes every tenant's
  hit rate while the counters read "just cold." Not configurable in
  v1.
- **Per-app share cap:** one cold global knob, **`max_app_share`**
  (percent of `max_bytes`, default **50**). A store that would push
  an app past its share evicts within that app first. Per-app byte
  accounting already exists for the purge index and the `/1.0/cache`
  per-app breakdown, so the cap is one comparison at store time. Full
  per-app carve-up knobs stay out of v1 — but a hot tenant silently
  zeroing every other tenant's hit rate is not "self-limiting," it is
  unmonitored. (Ledger Q7.)
- Eviction: **LRU by bytes** within each shard (below) when a store
  would exceed the shard budget; expired entries are evicted lazily
  on lookup and by opportunistic sweep; per-app purge on
  swap/delete/reap/claim as above.
- An entry that cannot fit even after eviction (pathological
  `max_body` vs `max_bytes` settings) is simply not stored.

### Concurrency structure (normative)

The data plane just collapsed its hot path to **one lock acquisition
per request** ([performance doc](20260719-165500-rip-server-performance.md),
"Hot-path lock collapse") — the cache must not hand that back. The
naive reading of "LRU by bytes" is a global mutex guarding a map plus
a linked list, taken *and written* (the LRU splice) on every HIT, on
the very path meant to run at several hundred k/s. An `RWMutex` does
not save it: an LRU touch is a write. So the structure is specified,
not left to the implementer's guess:

- The cache is **sharded** (16–64 shards by key hash). Each shard has
  its own mutex, its own byte budget (`max_bytes / N` — the slight
  fairness skew is accepted), and its own per-app index for purge.
- **The HIT path takes at most one shard lock — never a global
  lock.** Recency is an atomic last-access timestamp with sampled
  eviction (Redis-style), so a HIT does no list splice at all.
- **Counters are per-shard padded atomics**, aggregated at read time.
  Global atomics bumped at 10⁵–10⁶/s are a cache-line contention
  point; per-app counters live in the shards with the entries, not in
  a mutex-guarded global map.
- **Purge iterates shards; it never quiesces the world.** The
  generation bump is a single atomic in the registry's critical
  section; the entry drop walks shard-by-shard.
- **`GET /1.0/cache` is a lock-free-ish snapshot:** it sums shard
  atomics; values are monotonic but not mutually atomic (documented
  as such). The endpoint never blocks stores or hits — a tight scrape
  loop must not be able to degrade the data plane.
- The bypass path stays as revision 1 promised: no new locks at all.

### Sharp edges (documented, tenant-facing)

A shared cache shares whatever the origin computes. These classes
need naming in tenant documentation:

- **Responses derived from connection identity.** Janus sets
  `X-Forwarded-For` on every proxied request; a page that echoes the
  client's IP (or geolocates it) will cache one client's answer for
  everyone for `ttl`. The tenant's remedies are real and mechanical:
  `Cache-Control: private` (never stored) or `Vary: X-Forwarded-For`
  (outside the allowlist → never stored). This is inherent to shared
  caching, not a Janus defect — but the capability doc must say it.
- **Auth carried anywhere other than `Cookie` / `Authorization` /
  `Proxy-Authorization` is invisible to the cache.** Custom auth
  headers (`X-API-Key`, …) cannot be bypassed mechanically — the
  cache cannot enumerate headers it has never heard of. The operator
  enabling `cache` on a site that authenticates via custom headers
  has misconfigured (the syntax example below puts `cache off` on
  `api.ripdev.io` for exactly this reason); the tenant's veto is
  `Cache-Control: private`. Query-param tokens (`?token=…`) are the
  *safe* case, stated so nobody "fixes" it: the token is part of the
  raw-query key, so sharing requires possessing the same token.
- **UA-sniffing and client-hint variance** (`Sec-CH-UA`, `Save-Data`,
  `DPR`): correct apps declare `Vary` (→ never stored, safe); buggy
  ones are the accepted wrong-content class above.
- **Time-sensitive anonymous pages** (a "current server time" JSON, a
  countdown) are up to `ttl` stale by construction. That is the
  product being bought; the veto is per-response `Cache-Control`.
- **A HIT can serve a dead app** for up to `ttl` (crash without a
  PUT, above).

## Cascade

Site-scoped, exactly the **ping** pattern — with two process-wide
exceptions: `max_bytes` and `max_app_share` name one shared memory
pool and are legal **only** in the global `janus` block (like
`control`, a site-level occurrence is a parse error).

| Global | Site | Effective |
| --- | --- | --- |
| (unset) | (unset) | **off** |
| `cache` / `cache on` | (unset) | **on** (global tuning) |
| `cache` | `cache off` | **off** |
| `cache off` / (unset) | `cache on` | **on** |
| `cache { ttl 1s }` | `cache { ttl 5s }` | on, ttl **5s** for this site |

Tuning subdirectives (`ttl`, `ttl_max`, `max_body`, `debug`) cascade
per key: a site block overrides only the keys it names; unmentioned
keys inherit the global values; built-in defaults apply when unset at
every level.

## Syntax

Normal Caddyfile grammar — a directive with optional `on|off` and an
optional block, same shape as stock Caddy:

```caddyfile
{
	janus {
		control local
		ping
		cache {                 # global default: on, tuned
			ttl 1s              # freshness when the response has no max-age
			ttl_max 10s         # cap on response-declared max-age/s-maxage
			max_body 256kb      # largest storable body; larger streams uncached
			max_bytes 64mb      # process-wide pool (GLOBAL ONLY)
			max_app_share 50    # % of max_bytes one app may hold (GLOBAL ONLY)
		}
	}
}

(ripdev_tls) {
	tls certs/ripdev.io.crt certs/ripdev.io.key
}

*.ripdev.io {                   # catchall: inherits cache on
	import ripdev_tls
	janus
}

api.ripdev.io {
	import ripdev_tls
	janus {
		cache off               # explicit off beats inherited on
	}
}

pages.ripdev.io {
	import ripdev_tls
	janus {
		cache {
			ttl 5s              # override one key; the rest inherit
			debug               # emit X-Janus-Cache on this site
		}
	}
}
```

### Legal lines

```text
cache
cache on
cache off
cache {
	ttl <duration>          # > 0
	ttl_max <duration>      # ≥ ttl
	max_body <size>         # > 0
	max_bytes <size>        # > 0; global janus block only
	max_app_share <percent> # integer 1–100; global janus block only
	debug                   # X-Janus-Cache response header on
}
cache on { … }              # same as cache { … }
```

### Defaults

| Knob | Default |
| --- | --- |
| capability | off (unset at every level) |
| `ttl` | 1s |
| `ttl_max` | 10s |
| `max_body` | 256kb |
| `max_bytes` | 64mb |
| `max_app_share` | 50 (%) |
| `debug` | off |
| Vary allowlist | `Accept, Accept-Encoding, Accept-Language` (fixed; not configurable in v1) |
| Waiter cap / waiter deadline | 64 per coalescing key / ~15s (fixed in v1) |
| Admission doorkeeper | on, 2-hit (fixed in v1) |
| Key length cap | 8 KB → bypass (fixed in v1) |

### Hard errors (reject at parse)

- Unknown argument (anything but `on` / `off`)
- `cache off { … }` — a block on an off switch is a contradiction
- Unknown subdirective inside the block
- Duplicate `cache` directive in the same block; duplicate subdirective
- `ttl`, `ttl_max` not a valid positive duration; `ttl_max < ttl`
  (checked at provision, where both effective values are known)
- `max_body`, `max_bytes` not a valid positive size
- `max_bytes` or `max_app_share` in a **site** `janus` block
  (process-wide only)
- `max_app_share` not an integer in 1–100
- Nested block under any subdirective
- `debug` with any argument

## Observability

- **Counters, always on, hot `/1.0`:** `GET {base}/1.0/cache` on
  every control listener (same Bearer behavior as every `/1.0`
  route) returns process totals and per-app breakdown:

  ```json
  {
    "hits": 0, "misses": 0, "coalesced": 0, "bypass": 0,
    "stores": 0, "purges": 0, "evictions": 0,
    "fenced_stores": 0, "admission_rejects": 0,
    "waiter_overflow": 0, "waiter_expired": 0,
    "entries": 0, "stored_bytes": 0,
    "apps": { "shop-x7k2p9": { "hits": 0, "…": 0 } }
  }
  ```

  `fenced_stores` counts stores rejected by the generation fence;
  `admission_rejects` counts doorkeeper refusals; `waiter_overflow` /
  `waiter_expired` count cap and deadline fall-throughs. Counters are
  the acceptance-test instrument (a HIT must not touch a worker —
  provable as `hits` up, worker request count flat). Implementation:
  per-shard padded atomics summed at read; the endpoint never blocks
  stores or hits; the snapshot is monotonic but not mutually atomic.

- **`X-Janus-Cache: HIT|MISS|COALESCED|BYPASS` response header,
  gated by `debug`:** dev-visible, prod-quiet. **Stamped at serve
  time, never stored** — otherwise a HIT replays the stored `MISS`
  marker. Off by default so production responses carry no cache
  chatter; a site flips it on with one word while developing. Never
  emitted on control listeners (data plane only).

- Registry reads and cache lookups stay on the data-plane hot path's
  existing discipline: no per-request control-plane calls, no new
  locks on the bypass path, at most one shard lock on the HIT path
  (concurrency section above).

## Non-goals

- **No ESI**, no partial-page assembly
- **No stale-while-revalidate / stale-if-error** in v1 — expired is
  dead; the fill pays the full round trip
- **No purge API** in v1 — TTL expiry + swap-purge only; a hot
  `DELETE /1.0/cache` surface can come later if a use case demands it
- **No disk** — memory only, gone on restart (like the registry)
- **No shared cache across Janus instances** — one process, one pool
- **No `Expires` parsing** — its *presence* vetoes storage (never-store
  table); its date is never parsed
- **No request-`Cache-Control` (or `Pragma`) honoring** — see
  correctness spec
- **No cache on control listeners** — `/1.0` is never cached
- **No configurable Vary allowlist** in v1 — fixed three headers
- **No `Accept-Encoding` normalization/bucketing** — raw values;
  see the allowlist section for the review conflict and resolution
- **No 304/conditional machinery** — conditionals bypass
- **No fill deadline on the leader** in v1 — the leader is a normal
  data-plane request and holds exactly as long as today's slow
  requests; the waiter deadline is what bounds the crowd
- Not a CDN, not a static file server (lever #6 is separate work)

## Acceptance sketch (`test.sh` cache group)

The instrument: a fixture upstream that counts requests it receives
(the tenant-side truth), plus `/1.0/cache` counters (the Janus-side
truth). Every case asserts both sides. Note the doorkeeper: a key's
*first* storable fill is not admitted (seen once), the *second* is —
so hit tests use three requests, not two.

| Case | Assert |
| --- | --- |
| **Hit serves without touching the worker** | Three `GET /page`; worker counter = 2 (doorkeeper admits on the second fill); third response byte-identical + `Age` present; `/1.0/cache` hits = 1, admission_rejects = 1 |
| **Cookie bypasses** | `GET` with `Cookie: a=1` repeatedly; worker counter = N; bypass counter = N; nothing stored |
| **Authorization / Proxy-Authorization bypass** | Same shape with `Authorization: Bearer x`, then `Proxy-Authorization: Basic x` |
| **POST bypasses** | Two `POST`; worker counter = 2 |
| **`Set-Cookie` never stored** | Upstream sets a cookie; repeated `GET`s all reach the worker |
| **`no-store` respected** | Upstream answers `Cache-Control: no-store`; repeats reach the worker; same for `private`; same for an unparseable `Cache-Control` |
| **`Expires` presence vetoes** | Upstream answers `Expires: <future>` and no `Cache-Control`; repeats reach the worker |
| **`Content-Encoding` without matching Vary never stored** | Upstream answers `Content-Encoding: gzip`, no `Vary`; repeats reach the worker. With `Vary: Accept-Encoding` added, stores per variant |
| **`ACAO` echo never stored** | Upstream echoes request `Origin` into `Access-Control-Allow-Origin`; repeats reach the worker. With `ACAO: *`, stores |
| **Vary respected** | Upstream varies on `Accept-Language`; `de`×2 then `en`×2 then `de` again; the fifth request is a HIT with the `de` body; both variants coexist under one primary key |
| **Unbounded Vary never stored** | Upstream answers `Vary: *` (and `Vary: Cookie`); repeats always reach the worker |
| **Non-200 never stored** | Upstream 404 and 500; repeats always reach the worker |
| **Marked 503 never stored** | Busy fixture answers `Rip-Worker-Busy: 1` 503; repeats reach the data plane's normal retry machinery |
| **Truncated fill never stored** | Fixture declares `Content-Length: 1000`, sends 500 bytes, closes; repeats reach the worker |
| **Key is wire bytes** | `go test` unit: `/a%2Fb` and `/a/b` are two keys |
| **Coalescing** | Slow fixture (~200ms); N=32 concurrent `GET`s on a cold key; worker counter = **1**; all 32 get 200 with the same body; coalesced counter = 31 |
| **Waiter cap overflow falls through** | N=100 concurrent on one cold key; ≥ N−65 requests reach the worker as fall-throughs (waiter_overflow counted); nobody gets a manufactured 503 |
| **Fill failure falls through** | Slow fixture answers `Set-Cookie`; N concurrent; worker counter = N (leader + fall-throughs), no waiter shares the leader's response; key carries a do-not-coalesce mark for one `ttl` (next burst bypasses without buffering) |
| **Purge on upstream swap** | Fill the cache; `PUT …/upstreams` with a new socket; immediate `GET` reaches the **new** worker (counter = 1 on the new fixture); purges counter incremented |
| **Purge race (fill straddles the PUT)** | Slow fixture (~500ms) on the old socket; start a fill; mid-fill, `PUT` new upstreams; fill completes → `fenced_stores` = 1, nothing stored; released waiters fall through and reach the **new** worker; next `GET` misses and fills from the new pool |
| **Host re-claim never serves the old tenant** | Fill app A's cache; `DELETE` A; `POST /1.0/apps` re-claims the host as app B; immediate `GET` reaches B's worker, never A's cached body |
| **TTL expiry** | Fill with `ttl 1s`; sleep 1.2s; `GET` reaches the worker again |
| **`max_body` streams uncached** | Body > cap; response intact; repeats reach the worker |
| **`cache off` site** | Site with explicit off; repeats always reach the worker |
| **Cascade** | Global `cache on`; catchall site HITs; `cache off` site misses; per-site `ttl` override observed |
| **Parse rejections** | Each hard-error line above fails `caddy adapt` with a precise error |

Plus `go test ./...` units: key construction (wire-bytes, key-length
cap, app id), Vary parsing (case-insensitivity, OWS, multi-line,
fail-closed), the shared value-extraction function (multi-line join,
absent-vs-empty), storability table (one test per row, including the
new rows), TTL arithmetic (`s-maxage` > `max-age` > default, capped,
fill-start anchor), LRU + share-cap accounting, doorkeeper admission,
generation fencing (store-reject, waiter-detach, join-reject), and
the coalescing leader/waiter/cap/deadline/fall-through state machine.

## Measurement plan

Per the measurement discipline in the performance doc — interleaved
A/B, ratios over absolutes, run against the **post-reboot canonical
baseline** on the established rig (M5, HTTPS full stack, oha,
keep-alive, 15s runs, warmup discarded, `ulimit -n` 65536).

Revision 1's plan ran the flagship number on a ping-class route with
a "< 2x = failed" gate — a plan its own arithmetic would flunk, since
ping-class cache-off already sits at the ~99k Janus-bound ceiling and
a hit still pays TLS + routing (~1.3x expected). The roles are now
swapped: the headline runs where the arithmetic says the win lives.

1. **The 10–100x claim (capacity-bound route).** A ~5ms handler
   (sleep or DB-shaped fixture), `Cache-Control` absent, default
   `ttl 1s`, w:2 c:1, conc:64: cache off vs on, interleaved.
   Expectation: off ≈ `w×c / 5ms` ≈ 400 req/s; on rides toward the
   TLS-serve ceiling. **< 10x on this route means the capability
   failed its reason to exist.** Report worker-side request counts
   alongside RPS (the worker should see ~1 req/s per key while the
   clients see thousands).
2. **The ping-class floor (honesty check, no gate).** Ping-class
   cacheable page, same rig, w:2 c:1, conc:64, interleaved.
   Expected **~1.2–1.4x** — the hit deletes the worker and the UDS
   hop but keeps TLS + routing, and the ceiling is Janus-CPU-bound.
   Reported as the floor of the win curve, not a pass/fail.
3. **The coalescing stampede.** 5ms handler, cold cache per burst
   (restart or distinct keys). Two shapes: conc:64 (≤ waiter cap) —
   worker request count per burst = **1**, client p99 ≈ one origin
   round trip, not 64× queueing; and conc:256 (> cap) — expect 1
   fill + ~(conc − cap) fall-throughs on the data plane
   (`waiter_overflow` counted), no manufactured 503s below capacity.
4. **The zero case (honesty check).** Same rig, every request
   carrying `Cookie`: expect cache-on ≈ cache-off within noise —
   the bypass path must not tax the workloads that can't win.
5. **Reload interaction.** Watch-mode save under load: assert the
   first post-save response is new code (purge + fence observed:
   `fenced_stores` ≥ 0 and no stale body), and the dirty-window
   stampede produced one ring.

Numbers land in the performance doc's Measured results with the
commit that ships the capability, per rule 8. The performance doc's
lever framing ("the only remaining 10x+ story") survives — restated
precisely as **10–100x on capacity-bound routes; ~1.3x on ping-class
(Janus-bound) routes**.

## Protocol doc delta (applied)

[`20260719-002000-pool-protocol.md`](20260719-002000-pool-protocol.md)
carries three touches from this capability:

1. **Data plane decision table** gains one row, second position
   (after "Unknown host → 404", before everything that consults
   upstreams):

   > | Known host, cache HIT (site cache on, no bypass, app id and
   > variant match) | Serve from memory; no upstream selected,
   > nothing counted toward health |

2. **Control surface / `PUT …/upstreams`** gains one sentence: any
   upstreams PUT (and `DELETE`, TTL reap, and host claim via
   `POST /apps`) atomically drops the app's cache entries **and bumps
   the app's cache generation** — the admission cut also cuts the
   cache, and in-flight fills that straddle the cut can neither store
   nor retain waiters.

3. **The registry record** gains one field: the per-app atomic
   generation counter (bumped in the same critical section as the
   registry write; read by fills before host resolution).

The invariant's text needs no change: for the plain HIT the
letter-reading stands (serving a stored response admits nothing to
any generation), and the generation fence makes the cache strictly
stricter than the invariant requires everywhere else.

## Open-question ledger (post-review)

Revision 1 posed eleven questions. Two independent reviews settled
them as follows — one line of settling argument each; anything
genuinely unsettled is flagged for the implementer.

1. **Ignoring request `Cache-Control`** — **SETTLED: keep.** Every
   proposed middle (honor `no-cache` unless under load) makes cache
   behavior load-dependent — untestable drift-tolerance; the
   developer force-refresh already exists (`Cookie`-carrying request
   bypasses) and is now documented, with `Pragma` explicitly covered.
2. **Waiter cap** — **SETTLED: cap count (64/key) and duration
   (~15s), overflow/expiry fall through.** The revision-1 time-bound
   argument cited proxy timeouts that do not exist in `dataplane.go`;
   uncapped waiters convert the data plane's fast 503 shedding into
   unbounded held goroutines and bypass the ring's holder cap during
   dirty windows. (The reviews split on the count cap — correctness
   called it unnecessary, systems mandatory; resolved to **cap**,
   since the capacity-shedding inversion argument stands even where
   the trickle argument is fixed by the deadline alone.)
3. **Fill-failure re-coalesce** — **SETTLED: reject; keep
   fall-through; add the one-`ttl` do-not-coalesce mark.** Leader
   promotion serializes retries against a sick origin; the mark
   breaks the permanent waiter-pulse cycle on never-storable hot
   routes.
4. **`Accept-Encoding` normalization** — **SETTLED: raw values
   stand.** (The reviews contradicted each other; resolution in the
   Vary section: the `Content-Encoding` never-store row closes the
   wrong-bytes case, the doorkeeper closes the variant-mint case, and
   bucketing would reintroduce a canonicalization surface for zero
   remaining gain.)
5. **HEAD bypasses** — **SETTLED: keep.** Serving HEAD from a
   GET-filled entry buys a pile of RFC 9110 §9.3.2 header edge cases
   for a method with no stampede story; HEAD can never fill or poison
   a GET key (non-GET never stores).
6. **200-only** — **SETTLED: keep, argument strengthened.** A cached
   redirect plus any open-redirect bug is a poisoning amplifier;
   negative-cached 404s never hit under a random-path flood, so they
   buy nothing.
7. **Global-only `max_bytes` / LRU fairness** — **SETTLED for v1:
   doorkeeper admission filter + `max_app_share` cap + key-length
   cap** (the "self-limiting in practice" claim did not survive the
   churn arithmetic). *Flagged for the implementer/product:* whether
   full per-app byte quotas (per-app knobs, not one global share cap)
   are required before a genuinely multi-tenant production
   deployment remains a product call — the reviews sharpened it but
   did not settle it.
8. **Whole-second `Age`** — **SETTLED: keep.** `Age` is defined in
   seconds (RFC 9111 §5.1); `Age: 0` is a correct statement to
   downstream caches — contingent on the fill-start anchor now in
   the TTL section.
9. **Purge-on-swap vs let-TTL-expire, and the invariant reading** —
   **SETTLED: purge is mandatory and generation-fenced.** Both
   reviews found the straddling-fill race independently; a post-cut
   waiter receiving pre-cut output is materially new-admission-to-
   old-code, and the doc's no-asterisk reload promise is false
   without the fence. The letter-of-the-invariant reading survives
   for the plain HIT; the argument section was rewritten accordingly.
10. **Coalescing over-partitioning** — **SETTLED: keep.** Verified:
    the coalescing key is finer than or equal to every legal storage
    variant key, so no waiter can receive a wrong variant; the
    precondition (one shared value-extraction function) is now
    normative; the waste case degrades to today's behavior.
11. **Remaining poisoning / unkeyed-input vectors** — **SETTLED by
    enumeration.** Found and fixed: `Origin`/ACAO echo, `Proxy-
    Authorization`, decoded-path keying, truncated fills,
    `Content-Encoding` variance; documented as sharp edges:
    `X-Forwarded-For`, custom auth headers, client hints. With those
    landed, neither review could construct a further vector: the
    wire-bytes key closes normalization mismatch, out-of-allowlist
    `Vary` fails closed, and post-scrub storage closes internal-
    header leakage.

**New items opened by the reviews, resolved in this revision:** the
concurrency structure (sharded, normative), memory honesty
(`max_bytes` as accounting bound, RSS multiplier stated), the
crash-without-PUT health window (documented as a sharp edge), boot
purge churn (documented as intentional), and the `encode`-handler
ordering (specified). Nothing else is known-open.

## Measured results (2026-07-20)

Full tables and rig notes live in the performance doc's Measured
results ("Micro-cache + coalescing", the house ledger:
[`20260719-165500-rip-server-performance.md`](20260719-165500-rip-server-performance.md));
raw legs in
[`20260720-062700-bench-raw-microcache.txt`](20260720-062700-bench-raw-microcache.txt).
The headline rows, against this doc's measurement plan:

1. **10x gate — passed at ~320–380x** (clean 200s/s on the 5ms
   handler, w:2 c:1 conc:64, interleaved off/on: 366 → 118,265 and
   361 → 137,376). Worker-side: 15–16 requests per 15s leg — ~1 req/s
   per key at ttl 1s, per the plan. Cache-on runs above the old ~99k
   proxied ceiling: a HIT deletes the proxy + UDS hop, not just the
   worker.
2. **Ping-class floor: 1.6–2.5x** (predicted ~1.2–1.4x; the HIT also
   deletes the proxy machinery the prediction had charged to "TLS +
   routing"). No gate; reported as the floor of the win curve.
3. **Stampede:** conc:64 cold key → **1** origin request, three
   bursts out of three, all clients 200. conc:256 (past the cap):
   hard-simultaneous arrivals → 1 fill + 172 overflow fall-throughs,
   excess shed by the data plane as capacity 503s (none manufactured
   by the cache); staggered arrivals → `{200: 256}`.
4. **Zero case:** Cookie-carrying traffic off vs on = 27.5k vs 24.8k
   RPS, inside the session's ±10–24% identical-leg drift — the bypass
   tax is unmeasurable on this rig.
5. **Reload under load:** first post-save response is the new code;
   `fenced_stores` caught 2 straddling fills; no stale body observed.

## Related

- Performance map, lever #2: [`20260719-165500-rip-server-performance.md`](20260719-165500-rip-server-performance.md)
- Pool protocol (the data plane this sits in front of): [`20260719-002000-pool-protocol.md`](20260719-002000-pool-protocol.md)
- Cascade rules: [`20260718-191425-janus-build-spec.md`](20260718-191425-janus-build-spec.md)
- Site-scoped capability template: [`20260718-204255-capability-ping.md`](20260718-204255-capability-ping.md)
- Process-wide capability (contrast): [`20260718-203749-capability-control.md`](20260718-203749-capability-control.md)
