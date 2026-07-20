# Capability: cache (micro-cache + request coalescing)

> **DESIGN DRAFT — pending adversarial review. Nothing below is
> implemented.** This document is the design contract to attack before
> any code lands. Decisions taken here are argued, not assumed; the
> "Open questions for adversarial review" section lists every position
> the review should try to break.

Cold Caddyfile capability. When enabled for a site, Janus keeps a
short-TTL in-memory copy of anonymous `GET` responses and answers
repeats from memory without touching a worker. Concurrent misses on
the same key coalesce into one origin request (singleflight); the rest
wait for the fill.

| | |
| --- | --- |
| **Order** | **3** (after **ping**, **control**; sits on the Phase 4 data plane) |
| **Surface** | Cold config (Caddyfile); counters on hot `/1.0` |
| **Cascades** | Yes — global default → site override (one process-wide exception: `max_bytes`) |
| **Built-in default** | off (when unset at every level) |
| **Module** | Site handler (data plane), between ping and upstream selection |
| **Status** | **Design draft** — no implementation, no tests |

## Why it exists

Lever #2 of the performance map
([`20260719-165500-rip-server-performance.md`](20260719-165500-rip-server-performance.md)
§2) — the only remaining 10x+ story. The measured stack tops out near
~99k RPS on ping-class routes because every request pays TLS + proxy +
a worker round trip; the attribution table shows Janus-side cost is
already ~75% of per-request latency, so no worker-side change moves
the ceiling. A cache hit deletes the UDS hop and the worker entirely:
the request costs TLS + a memory lookup.

The honest scope of the claim:

- **Where 10–100x applies:** public, anonymous, repeatedly-requested
  pages — a landing page, a product page, a public JSON feed — under
  concurrency. A stampede of N clients per second on one page becomes
  ~1 worker request per TTL (default ~1s), regardless of N. On a 5ms
  handler that is the difference between capacity math
  (`w×c / 5ms`) and the TLS-serve ceiling.
- **Where the win is zero, by design:** APIs and personalized pages.
  Any request carrying `Cookie` or `Authorization` bypasses the cache
  completely; any response carrying `Set-Cookie`, `no-store`, or
  `private` is never stored. Non-GET is never cached. For those
  workloads this capability changes nothing — levers #1 and #4 remain
  their story.

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
| Request has `Cookie` or `Authorization` | bypass — never serve from cache, never store |
| Request has `Range` | bypass |
| Request has a conditional header (`If-None-Match`, `If-Modified-Since`, …) | bypass |
| Fresh entry under the key, variant matches (Vary, below) | **HIT** — serve from memory |
| Fill in flight for this coalescing key | **COALESCE** — wait for the fill |
| Otherwise | **MISS** — become the fill: proxy via the normal data plane, then decide storability |

Bypass means the request proceeds through the standard decision table
([pool protocol](20260719-002000-pool-protocol.md)) exactly as today
— doorbell, marked-503 retry, health accounting, all unchanged.

Request `Cache-Control` is **ignored** for the serve decision. A
client's `no-cache` / `no-store` does not force a worker touch: the
micro-cache exists to protect the origin from request volume, and the
request header is attacker-controlled — honoring it hands every
stampede a one-line opt-out. RFC 9111 §5.2.1 explicitly permits a
shared cache to be configured to ignore request directives. (Position
flagged for review below.)

### Cache key

Primary key: **normalized host + raw path + raw query**.

- Host is the same normalization the data plane already applies to
  `Host` (lowercase, trailing dot stripped, `:port` dropped).
- Path and query are used **as received** — no canonicalization, no
  query-parameter sorting, no decoding. Two byte-different URLs are
  two keys. Canonicalization is a classic cache-poisoning vector
  (normalize differently than the origin parses and you serve one
  resource under another's key); refusing to normalize makes that
  class unreachable at the cost of some duplicate entries. Random
  query strings can force misses — but a miss is exactly today's
  behavior, and key cardinality is bounded by the byte-cap eviction.

Secondary key (variant selection): the values of the request headers
named by the **stored response's `Vary`** — standard HTTP semantics.
An entry remembers its `Vary` header names and the request header
values it was filled under; a lookup matches only when the current
request's values for those names are identical.

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

The allowlist bounds variant cardinality (an unbounded `Vary` is an
unbounded memory key) and doubles as a tenant escape hatch: an app
that computes a response from any request header the cache does not
key on can say `Vary: <that header>` and is thereby guaranteed never
to be cached wrongly — it simply is never cached.

### Never store

A fill's response is stored only when **all** of these hold:

| Rule | Why |
| --- | --- |
| Status is exactly **200** | See below |
| No `Set-Cookie` header | A response that sets a session is per-client by definition; storing it leaks sessions. Non-negotiable |
| Response `Cache-Control` has no `no-store`, `no-cache`, or `private` | `no-store`/`private` are the origin's veto; `no-cache` demands revalidation machinery v1 does not have, so it is treated as a veto too |
| Effective freshness > 0 (TTL rules below) | `max-age=0` means "do not reuse" |
| `Vary` (if present) is within the allowlist | Above |
| Body fits `max_body` after buffering | Below |
| Response has no trailers and is not an Upgrade | Streaming/tunnel semantics don't buffer |
| Not a worker-marked 503, not any 503, not anything but 200 | Marked 503s are flow control (`Rip-Worker-Busy` / `Rip-Worker-Draining`, pool protocol); caching one would convert one worker's momentary "busy" into a TTL-long outage for every client. Subsumed by 200-only, stated explicitly because the failure mode is so bad |

**Why 200 only.** 301/308 caching turns a misconfigured redirect into
a TTL-long trap; 404/negative caching is a real feature with its own
semantics (and Janus's own unknown-host 404 never reaches a worker
anyway, so there is nothing to save); 206 requires Range machinery
this capability bypasses. Every non-200 status is some flavor of
exceptional, and exceptional responses are precisely where a stale
copy does the most damage per byte saved. v1 stores the boring case
only.

`Expires` is ignored (Cache-Control only) — see non-goals.

**Stored bytes are post-scrub bytes.** The data plane's
`ModifyResponse` already deletes `Rip-Mark` (the tenant's internal
correlation id) from every client response; the cache stores the
response **after** that scrub, so a cached response is byte-identical
to what the same client would have received on a miss. No internal
header can leak via the cache, and a HIT re-scrubs nothing.

### Buffering and the size cap

A fill buffers the body up to `max_body` (default 256kb). If the body
exceeds the cap — by `Content-Length` up front, or discovered while
reading a chunked body — the entry is **abandoned**: buffered bytes
flush to the client and the remainder streams through untouched. The
client sees a normal streamed response; the cache stores nothing.
Streaming responses (SSE and friends) therefore self-exclude by size
or by never ending.

### TTL

Freshness for a storable response, in order:

1. Response `Cache-Control: s-maxage=N` (shared-cache directive wins,
   per RFC 9111), else `max-age=N` → freshness = `min(N, ttl_max)`.
2. Neither present → freshness = the site's `ttl` (default **1s**).

Entries past their freshness are dead — evicted lazily on lookup and
by opportunistic sweep. There is no revalidation, no
stale-while-revalidate, no serving stale on error: expired means the
next request is a MISS that fills through the normal data plane.

Hits carry an `Age` header (whole seconds). At 1s TTLs it is almost
always `Age: 0`, but downstream shared caches are entitled to it.

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
  `Set-Cookie`, out-of-allowlist `Vary`).

### Coalescing

**Coalescing key:** the primary key **plus the request's values of
all three allowlisted headers.** The response's `Vary` is unknowable
until the fill returns, so coalescing on the primary key alone could
hand an `Accept-Language: de` waiter a response filled under `en`.
Partitioning by the full potential secondary key over-splits (requests
differing in a header the response turns out not to vary on fill
separately) but can never share a wrong variant. Safe side taken.

Semantics:

- First MISS on a coalescing key becomes the **leader**; its request
  proxies through the normal data plane.
- Later arrivals on the same key **wait** on that fill instead of
  dialing a worker. A waiter's own request is never sent anywhere —
  it is a GET with no body, so there is nothing half-delivered.
- **Fill produced a storable 200** → the entry is stored and every
  waiter is served from it (counted as COALESCED, not HIT).
- **Fill produced anything else** — a non-storable 200
  (`Set-Cookie`, oversize, bad `Vary`), any non-200, a marked 503, a
  transport error → the leader's response goes to the leader alone
  (it may be per-client; sharing it is the session-leak disaster),
  and **every waiter falls through to the normal data plane
  individually**.

Why fall-through and not a shared 503: without this capability, all N
requests would have hit the data plane anyway — fall-through is
exactly no-cache behavior, and the data plane already owns the
failure story (marked-503 retry, health, doorbell, 503 +
`Retry-After`). The cache is an optimization layer; on any path it
cannot prove safe, it must degrade to today, never to something
worse. Manufacturing a 503 for N−1 waiters because one fill hit one
bad socket would be the cache *creating* an outage.

Waiters are **not capped in count** (position flagged for review).
The pool protocol rejects unbounded edge queues — but that rejection
is about unbounded *time* (holding for a whole pool boot). A cache
waiter holds for at most one origin round trip, already bounded by
the proxy's timeouts; and the alternative for that same request is a
full data-plane pass, which holds the same goroutine *plus* a worker
slot. Waiting is strictly cheaper than the fall-through it replaces.
A wait-duration cap equal to the proxy timeout bounds the worst case.

### Interaction with the pool protocol: purge on swap

The pool protocol's invariant: *Janus never admits a new request to a
generation the tenant has declared stale.* Does a cached response
violate it during a dirty window (after the doorbell PUT, before the
new pool publishes)?

Argued strictly: **no.** The invariant governs request admission —
new requests must not be *executed by* old code. A cached response is
a different object: it was legally produced by the then-current
generation before the cut, like an in-flight response that completes
after the doorbell PUT ("finishing a commitment"). Serving it
executes nothing. A cached response predating the cut is not "known
stale" in the invariant's sense — the invariant is about NEW requests
to OLD code, and a HIT is neither.

**Decision anyway: purge on swap.** Every `PUT
/1.0/apps/{id}/upstreams` — doorbell, sockets, or empty — and every
`DELETE` / heartbeat reap **drops all cache entries for that app**,
atomically with the registry write. Reasoning:

- The letter of the invariant permits serving the old copy for up to
  `ttl` after a save; the *spirit* of the reload story ("a save cuts
  admission — the next request is the NEW code, never the old") does
  not. An operator who saves a file and refreshes must see the new
  output; explaining "except for up to 1s, from the cache" is
  drift-tolerance in miniature.
- At ~1s TTLs the purge is nearly free — the entries were about to
  die anyway. The cheap price buys a mental model with no asterisk:
  **after the cut, nothing produced before the cut is served.**
- It composes with coalescing perfectly during the dirty window: the
  purge empties the key, the first request MISSes, the fill rings the
  doorbell and holds per the protocol, and all concurrent arrivals
  coalesce onto that one fill — the cache turns a reload stampede
  into **one** ring and one first-delivery instead of N holders
  against the waiter cap.

Purge needs entries indexed by app id; the key layout carries it.

### Memory bounds and eviction

- One process-wide pool: **`max_bytes`** (default 64mb), counting
  body + headers + key overhead. Global rather than per-app in v1: a
  per-app fairness carve-up needs per-app knobs the registry does not
  have (apps are hot, cache config is cold), and ~1s TTLs mean the
  steady-state footprint is (hot keys × body size), self-limiting in
  practice. Flagged for review.
- Eviction: **LRU by bytes** when a store would exceed `max_bytes`;
  expired entries are evicted lazily on lookup and by opportunistic
  sweep; per-app purge on swap/delete/reap as above.
- An entry that cannot fit even after eviction (pathological
  `max_body` vs `max_bytes` settings) is simply not stored.

### Sharp edges (documented, tenant-facing)

A shared cache shares whatever the origin computes. Two classes need
naming in tenant documentation:

- **Responses derived from connection identity.** Janus sets
  `X-Forwarded-For` on every proxied request; a page that echoes the
  client's IP (or geolocates it) will cache one client's answer for
  everyone for `ttl`. The tenant's remedies are real and mechanical:
  `Cache-Control: private` (never stored) or `Vary: X-Forwarded-For`
  (outside the allowlist → never stored). This is inherent to shared
  caching, not a Janus defect — but the capability doc must say it.
- **Time-sensitive anonymous pages** (a "current server time" JSON, a
  countdown) are up to `ttl` stale by construction. That is the
  product being bought; the veto is per-response `Cache-Control`.

## Cascade

Site-scoped, exactly the **ping** pattern — with one process-wide
exception: `max_bytes` names one shared memory pool and is legal
**only** in the global `janus` block (like `control`, a site-level
occurrence is a parse error).

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
| `debug` | off |
| Vary allowlist | `Accept, Accept-Encoding, Accept-Language` (fixed; not configurable in v1) |

### Hard errors (reject at parse)

- Unknown argument (anything but `on` / `off`)
- `cache off { … }` — a block on an off switch is a contradiction
- Unknown subdirective inside the block
- Duplicate `cache` directive in the same block; duplicate subdirective
- `ttl`, `ttl_max` not a valid positive duration; `ttl_max < ttl`
  (checked at provision, where both effective values are known)
- `max_body`, `max_bytes` not a valid positive size
- `max_bytes` in a **site** `janus` block (process-wide only)
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
    "entries": 0, "stored_bytes": 0,
    "apps": { "shop-x7k2p9": { "hits": 0, "…": 0 } }
  }
  ```

  Counters are the acceptance-test instrument (a HIT must not touch a
  worker — provable as `hits` up, worker request count flat).

- **`X-Janus-Cache: HIT|MISS|COALESCED|BYPASS` response header,
  gated by `debug`:** dev-visible, prod-quiet. Off by default so
  production responses carry no cache chatter; a site flips it on
  with one word while developing. Never emitted on control listeners
  (data plane only).

- Registry reads and cache lookups stay on the data-plane hot path's
  existing discipline: no per-request control-plane calls, no new
  locks on the bypass path.

## Non-goals

- **No ESI**, no partial-page assembly
- **No stale-while-revalidate / stale-if-error** in v1 — expired is
  dead; the fill pays the full round trip
- **No purge API** in v1 — TTL expiry + swap-purge only; a hot
  `DELETE /1.0/cache` surface can come later if a use case demands it
- **No disk** — memory only, gone on restart (like the registry)
- **No shared cache across Janus instances** — one process, one pool
- **No `Expires` parsing** — `Cache-Control` only
- **No request-`Cache-Control` honoring** — see correctness spec
- **No cache on control listeners** — `/1.0` is never cached
- **No configurable Vary allowlist** in v1 — fixed three headers
- **No 304/conditional machinery** — conditionals bypass
- Not a CDN, not a static file server (lever #6 is separate work)

## Acceptance sketch (`test.sh` cache group)

The instrument: a fixture upstream that counts requests it receives
(the tenant-side truth), plus `/1.0/cache` counters (the Janus-side
truth). Every case asserts both sides.

| Case | Assert |
| --- | --- |
| **Hit serves without touching the worker** | Two `GET /page`; worker counter = 1; second response byte-identical + `Age` present; `/1.0/cache` hits = 1 |
| **Cookie bypasses** | `GET` with `Cookie: a=1` twice; worker counter = 2; bypass counter = 2; nothing stored |
| **Authorization bypasses** | Same shape with `Authorization: Bearer x` |
| **POST bypasses** | Two `POST`; worker counter = 2 |
| **`Set-Cookie` never stored** | Upstream sets a cookie; two `GET`s; worker counter = 2 |
| **`no-store` respected** | Upstream answers `Cache-Control: no-store`; two `GET`s; worker counter = 2; same for `private` |
| **Vary respected** | Upstream varies on `Accept-Language`; `de` then `en` then `de` again; worker counter = 2, third request is a HIT with the `de` body |
| **Unbounded Vary never stored** | Upstream answers `Vary: *` (and `Vary: Cookie`); repeats always reach the worker |
| **Non-200 never stored** | Upstream 404 and 500; repeats always reach the worker |
| **Marked 503 never stored** | Busy fixture answers `Rip-Worker-Busy: 1` 503; repeats reach the data plane's normal retry machinery |
| **Coalescing** | Slow fixture (~200ms); N=32 concurrent `GET`s on a cold key; worker counter = **1**; all 32 get 200 with the same body; coalesced counter = 31 |
| **Fill failure falls through** | Slow fixture answers `Set-Cookie`; N concurrent; worker counter = N (leader + fall-throughs), no waiter shares the leader's response |
| **Purge on upstream swap** | Fill the cache; `PUT …/upstreams` with a new socket; immediate `GET` reaches the **new** worker (counter = 1 on the new fixture); purges counter incremented |
| **TTL expiry** | Fill with `ttl 1s`; sleep 1.2s; `GET` reaches the worker again |
| **`max_body` streams uncached** | Body > cap; response intact; repeats reach the worker |
| **`cache off` site** | Site with explicit off; repeats always reach the worker |
| **Cascade** | Global `cache on`; catchall site HITs; `cache off` site misses; per-site `ttl` override observed |
| **Parse rejections** | Each hard-error line above fails `caddy adapt` with a precise error |

Plus `go test ./...` units: key construction, Vary matching,
storability table (one test per row), TTL arithmetic (`s-maxage` >
`max-age` > default, capped), LRU accounting, purge-on-PUT, and
coalescing leader/waiter/fall-through state machine.

## Measurement plan

Per the measurement discipline in the performance doc — interleaved
A/B, ratios over absolutes, run against the **post-reboot canonical
baseline** on the established rig (M5, HTTPS full stack, oha,
keep-alive, 15s runs, warmup discarded, `ulimit -n` 65536):

1. **The 10x claim (steady-state hit rate).** Ping-class cacheable
   page (`Cache-Control` absent, default `ttl 1s`), w:2 c:1, conc:64:
   cache off vs on, interleaved. Expectation: off ≈ the known
   95–102k band; on approaches Janus's TLS-serve ceiling with the
   UDS hop and worker deleted — the honest number is whatever it
   measures, but < 2x means the capability failed its reason to
   exist. Report worker-side request counts alongside RPS (the
   worker should see ~1 req/s per key).
2. **The coalescing stampede.** 5ms handler, cold cache per burst
   (restart or distinct keys), conc:256 first-arrival burst:
   worker request count per burst (expect 1) and client p99 vs the
   no-cache baseline (expect ≈ one origin round trip, not 256×
   queueing).
3. **The zero case (honesty check).** Same rig, every request
   carrying `Cookie`: expect cache-on ≈ cache-off within noise —
   the bypass path must not tax the workloads that can't win.
4. **Reload interaction.** Watch-mode save under load: assert the
   first post-save response is new code (purge observed) and the
   dirty-window stampede produced one ring.

Numbers land in the performance doc's Measured results with the
commit that ships the capability, per rule 8.

## Protocol doc delta (do not apply until implementation lands)

[`20260719-002000-pool-protocol.md`](20260719-002000-pool-protocol.md)
gains two touches when this capability ships — noted here, **not**
edited there while this is a draft:

1. **Data plane decision table** gains one row, second position
   (after "Unknown host → 404", before everything that consults
   upstreams):

   > | Known host, cache HIT (site cache on, no bypass) | Serve from
   > memory; no upstream selected, nothing counted toward health |

2. **Control surface / `PUT …/upstreams`** gains one sentence: any
   upstreams PUT (and `DELETE` / TTL reap) atomically drops the
   app's cache entries — the admission cut also cuts the cache.

The invariant's text needs no change: the purge-on-swap decision makes
the cache strictly stricter than the invariant requires.

## Open questions for adversarial review

Positions taken above that the review should attack hardest:

1. **Ignoring request `Cache-Control`.** RFC-permitted and
   stampede-proof, but it means a developer cannot force a fresh
   fetch with a header — only `debug` + waiting out the TTL, or a
   `Cookie`-carrying request. Acceptable?
2. **No waiter-count cap on coalescing.** Argued as strictly cheaper
   than fall-through; is there a failure mode (leader wedged inside
   the proxy timeout, mass memory held in waiter queues at very high
   key cardinality) that wants a cap + fall-through anyway?
3. **Fill-failure fall-through** re-releases N−1 requests at a
   possibly-sick origin at once. The data plane's 503/health story
   absorbs it, but is a one-shot re-coalesce (promote one waiter to
   new leader) worth its complexity?
4. **`Accept-Encoding` in the allowlist** invites variant explosion
   (arbitrary client encodings each fill separately). Should Janus
   normalize `Accept-Encoding` to a tiny set (identity/gzip/br/zstd)
   for the coalescing and variant key, or is the raw-value position
   (consistent with no-canonicalization) right?
5. **HEAD bypasses** rather than serving headers from a GET-filled
   entry. Rarely-used method, kept out of v1 for simplicity — fine?
6. **200-only** storability. Is there a real tenant case for cached
   301s or negative-cached 404s that justifies their risk in v1?
7. **Global-only `max_bytes`** with LRU means one hot tenant can
   evict every other tenant's entries. Self-limiting at 1s TTLs, but
   is per-app accounting needed before a multi-tenant deployment?
8. **`Age` at 1s granularity** rounds to 0 for almost every hit;
   is whole-second `Age` (vs dropping it, vs milliseconds via a
   private header) the right downstream-cache citizenship?
9. **Purge-on-swap vs let-TTL-expire.** The draft argues the
   invariant does not require the purge but takes it anyway for the
   no-asterisk reload story. If the review disagrees with the
   invariant reading itself — i.e. holds that a cached response *is*
   "new admission to old code" — the purge becomes mandatory rather
   than chosen, and the doc's argument section must be rewritten.
10. **Coalescing key over-partitioning** (three allowlisted headers
    always in the coalescing key) means clients with exotic `Accept`
    strings never coalesce. Cheap correctness or measurable waste?
11. **Cache poisoning review**: the no-canonicalization key, the
    Vary allowlist, and the Cookie/Authorization bypass are the
    defenses. What request-smuggling or unkeyed-input vectors remain
    (e.g. headers the origin reads that Janus neither keys nor
    strips)?

## Related

- Performance map, lever #2: [`20260719-165500-rip-server-performance.md`](20260719-165500-rip-server-performance.md)
- Pool protocol (the data plane this sits in front of): [`20260719-002000-pool-protocol.md`](20260719-002000-pool-protocol.md)
- Cascade rules: [`20260718-191425-janus-build-spec.md`](20260718-191425-janus-build-spec.md)
- Site-scoped capability template: [`20260718-204255-capability-ping.md`](20260718-204255-capability-ping.md)
- Process-wide capability (contrast): [`20260718-203749-capability-control.md`](20260718-203749-capability-control.md)
