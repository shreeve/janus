# Capability 9: access log

`access log` publishes one bounded, app-scoped, live NDJSON record for each
registered-app request that reaches Caddy's access-log encoder. The same seam
retains Caddy's durable JSON access log and observes the final post-error-route
status, Caddy recorder size, and application-visible logged response-header
map.

The stream is operational visibility for the process operator. It is not a
tenant authorization boundary, a durable queue, or a request middleware.

## Scope and ownership

- Capability order: **9**, after browse.
- Surface: Caddy access-log encoder plus hot control-plane observation.
- Configuration: Caddy's site `log` directive with `format janus`.
- Cascade: **no**. Logging configuration belongs to each Caddy site.
- Default: inactive until Caddy provisions a Janus log encoder.
- Stream scope: requests attributed to a live registered app.
- Trust boundary: every principal able to connect to a control listener is in
  one operator trust domain; a configured Bearer token adds its existing
  listener check. An app id filters records; it is not an authorization
  secret.

Registered-app scope includes proxy, auth, files, sendfile, browse, and
Hub responses once the request host resolves to a registration. Resolution
for logging happens early enough to attribute an auth denial, without changing
auth or routing decisions.

The stream excludes:

- cold browse roots;
- `/1.0` control requests;
- ACME challenges;
- the mDNS front door;
- `ping` requests that do not resolve to a registration;
- unknown and unclaimed hosts;
- Caddy and Janus operational diagnostics;
- Rip manager and worker diagnostics.

Excluded HTTP traffic remains eligible for ordinary Caddy access logs. Janus
service diagnostics remain on the service logger.

### Publication boundary

The encoder is the sole publication source. Capability 9 publishes exactly
the access entries Caddy actually invokes through `format janus`. Caddy policy
decides whether an entry reaches the encoder before Janus can observe it.
Entries excluded by disabled logging, unmapped or skipped hosts, logger level,
sampling, a filtering or discard core, a discard writer, or runtime
`log_skip` receive no access event and consume no sequence. Such exclusion is
not stream overflow and produces no gap.

The Janus encoder validates only its own `format janus` grammar and encoder
configuration. It neither inspects nor rejects its parent Caddy writer, core,
sampling, level, host mapping, or log enablement.

The repository root and operator-facing example configure every Janus site
with a real file or stderr sink, no sampling, no filtering/discard core, no
discard writer, and host mapping that invokes `format janus` for managed-app
traffic. Under that policy, every managed-app access entry not excluded by
runtime `log_skip` reaches Capability 9.

## Completion-seam architecture

The module id is:

```text
caddy.logging.encoders.janus
```

The encoder wraps a normal Caddy JSON encoder configured with the same common
encoder options. Janus inserts one reserved field into
`caddyhttp.ExtraLogFields`:

```text
key:  "janus.access.facts"
type: zapcore.SkipType
Interface: *accessFacts
```

Zap's JSON encoder ignores `SkipType`, so the sentinel produces no key, value,
separator, or byte in durable output. The Janus encoder locates the sentinel
by exact key, type, and pointer type in the `EncodeEntry` fields. No sentinel
means a non-Janus or pre-handler access entry: the encoder delegates it
unchanged and publishes nothing. A duplicate sentinel, the reserved key with
another zap type, or a `SkipType` sentinel whose interface is not exactly
`*accessFacts` is an invariant failure.

Caddy attaches the request object through `WithLazy`, which reaches an
encoder through `With` and `Clone`, not the final `EncodeEntry` field slice.
The Janus encoder therefore implements the complete `zapcore.Encoder`
contract:

- `Clone` clones both the Janus wrapper state and wrapped JSON encoder;
- every `Add*`, `OpenNamespace`, and `With` effect forwards unchanged to the
  wrapped encoder;
- `AddObject("request", LoggableHTTPRequest)` captures the concrete request
  log object in the wrapper clone and also forwards it;
- clone-local request capture never becomes shared mutable state;
- `EncodeEntry` combines captured request data, final fields, and the facts
  sentinel, then delegates the original entry and fields unchanged.

Immediately on entry to the Janus handler, before attribution or auth, Janus
stores the duration start timestamp and resolves `{http.request.uuid}` through
Caddy's request replacer. That single evaluation materializes Caddy's lazy
UUID, and Janus copies the concrete UUID into bounded request facts. Every
durable and stream surface therefore uses one request id.

The `EncodeEntry` path performs these steps:

1. scan for the reserved facts sentinel;
2. when absent, delegate the original entry and fields unchanged and stop;
3. when duplicate or malformed, record the invariant and still delegate the
   original entry and fields unchanged, publish nothing, and stop;
4. when facts contain no final event-owner tuple, delegate unchanged, publish
   nothing, and stop; this is normal for ping and unresolved routing;
5. read Caddy's completion fields: entry time, `status`, `size`, and
   `resp_headers`;
6. compute duration from the Janus request-start timestamp to encoder
   invocation, including Caddy error routes;
7. bound all fields;
8. lock the current event-owner access state and reserve the next sequence;
9. encode one immutable bounded UTF-8 JSON line including LF;
10. publish a pointer to that line exactly once;
11. delegate the original entry and fields to the wrapped JSON encoder.

Final delegation produces byte-for-byte the JSON that `format json` with the
same encoder options produces for the same entry and fields. Publication
neither removes nor rewrites durable fields. A publication failure never
suppresses the durable access-log entry.

Janus attaches one mutable request-facts object to the Caddy request context.
Owning request paths update only their own facts:

- auth attribution and routing resolution set registration identity,
  tenant-site identity, and `*accessState` together;
- auth, files, browse, sendfile, Hub, proxy, and Janus errors set the response
  class;
- cache handling sets the cache verdict;
- each worker attempt updates attempt count;
- only the final selected worker attempt sets the opaque upstream identity;
- only the final accepted worker response captures `Rip-Mark`, before scrub;
- response-copy paths classify cancellation, upstream abort, and write error.

The facts object contains its Janus start timestamp and a one-shot publication
guard. Overlapping log outputs or encoder instances may observe one request,
but only the first Janus encoder invocation reserves a sequence and publishes.
All configured outputs still receive their durable JSON.

Handlers never infer ordinary final HTTP status, response bytes, MIME type, or
duration. The encoder derives:

- timestamp from the Caddy log entry time;
- ordinary status and recorder size from Caddy's completion fields;
- duration from the Janus start timestamp at encoder invocation;
- MIME type from the `Content-Type` value in Caddy's application-visible
  logged response-header map.

Caddy records status `0` when no handler explicitly writes a response.
The encoder maps `0` to `200` only for a normally completed response. An
aborted or failed response with no committed status keeps `status: null`.

For an ordinary response, Caddy's `size` is the encoded response-body byte
count accepted through its outer recorder and excludes response headers.
Janus serializes it as a canonical decimal string. This value reflects the
identity, gzip, or zstd body path but is not a transport acknowledgment.

Two protocol cases override Caddy's recorder:

- Hub records force `status: 101` and `response_bytes: "0"` because Gorilla
  writes its handshake through a hijacked connection and Caddy's recorder
  counts raw upgraded-connection bytes;
- every HEAD record forces `response_bytes: "0"` because Go reports the
  notional body length to the recorder while writing no body.

Hub duration is request and handshake handling through encoder invocation. It
is not upgraded-connection lifetime.

The logged header map is not a wire-header snapshot. `mime_type` is the final
single `Content-Type` value visible in that map. It may include a post-commit
application mutation and does not include a `net/http` MIME sniff that exists
only on the wire.

Sequence assignment occurs under the registration access-state mutex after
field bounding and before final encoding, because the sequence is part of the
line. This lock order defines completion order for the stream. Publication
performs no network or disk I/O.

## Caddyfile syntax

Every Caddy site that routes requests through a Janus handler enables an
access log and selects the Janus encoder:

```caddyfile
*.ripdev.io {
	tls certs/ripdev.io.crt certs/ripdev.io.key

	log {
		output file var/access.json
		format janus {
			time_format rfc3339_nano
			duration_format seconds
		}
	}

	janus
}
```

The short form is:

```caddyfile
log {
	format janus
}
```

`janus` accepts no argument on its format line. Its optional block accepts
exactly the common JSON encoder subdirectives supported by the pinned Caddy
version:

```text
message_key <key>
level_key <key>
time_key <key>
name_key <key>
caller_key <key>
stacktrace_key <key>
line_ending <text>
time_format <format>
time_local
duration_format <format>
level_format <format>
```

Duplicate or empty leaf values follow Caddy's JSON encoder behavior. An
argument on `format janus`, an unknown subdirective, a nested block under a
leaf, or the wrong number of arguments rejects the Caddy load.

`log { format janus }` is the operator contract for a Janus-served site that
needs access streams. The encoder validates the `janus` format line and block
only. Parent logger policy remains Caddy configuration and may prevent the
encoder from being invoked.

## Registration state

Each successful `POST /1.0/apps` allocates a fresh access state containing:

- a sequence head starting at zero;
- a subscriber set;
- cumulative publication and overflow counters;
- a tombstone bit.

`AppRecord` carries `*accessState`. Registry clones preserve that pointer.
The state belongs to that registration, not its app name or host claims.
PATCH and upstream PUT retain it. Caddy reload retains it through pooled
process state.

A separate `accessBridge` lives in its own `caddy.UsagePool`, independent of
the registry/data-plane pool. Every Janus `App` and every provisioned Janus
encoder acquires one bridge reference and releases exactly that reference from
its matching cleanup path. Provisioning failure and aborted reload release
partial acquisitions. The bridge survives while either an App or encoder can
publish or close streams, and its final destruction asserts zero subscribers
and no attached registration state.

The bridge allocates each `accessState`, and `AppRecord` points to that
bridge-owned state. Encoder cleanup decrements `provisioned_encoders` under
bridge accounting before releasing its UsagePool reference.

The only legal nested lock order is:

```text
appRegistry.mu → accessState.mu → accessBridge.accountingMu
```

No path acquires those locks in reverse. Publication needs only
`accessState.mu`; stream admission begins with the registry lock and follows
the full order when process accounting changes.

DELETE and heartbeat reap atomically remove registry claims and tombstone the
access state through one shared registry removal primitive. While holding the
registry and access-state locks, that primitive:

1. removes the app and every host/site claim;
2. sets the tombstone and snapshots the final sequence head;
3. detaches every subscriber while holding bridge accounting in lock order;
4. records `registration_deleted` or `registration_reaped`;
5. prevents every later publication through that state.

It releases all locks before waking writers, closing channels, or performing
network I/O. DELETE and reap differ only in their recorded close reason.
Tombstoning never creates replacement state.

A new POST, including one with the same name and hosts, receives a new app id,
new access state, and sequence head zero. A request that captures the old
registration before tombstone and completes after tombstone remains in the
durable Caddy log but is outside the stream's publication guarantee.

The sequence is an unsigned 64-bit counter represented on the protocol as a
canonical decimal string. Legal values are `0` through
`18446744073709551615`. The first access event is `"1"`. If a registration at
the maximum head receives another publishable completion, Janus tombstones
its access stream with reason `sequence_exhausted`, emits an operational
error, and requires DELETE plus POST to obtain a fresh stream. Janus never
wraps a sequence.

For each non-skipped completion that wins the one-shot publication guard,
exactly one of these occurs while holding `accessState.mu`:

- tombstone rejects publication without changing the head; or
- Janus increments the head exactly once and that sequence identifies either
  one queued access line or one explicit encoding failure.

After reservation Janus encodes the final line with the assigned sequence. A
forced encoding invariant failure consumes the sequence, increments
`invariant_failures`, queues no line, and becomes observable as a gap when a
later sequence arrives or when `droppedThrough` is advanced for every
subscriber by that failed sequence. No two access lines share a sequence and
no access line appears outside strictly increasing sequence order.

## Resource bounds

The bounds are constants, not Caddyfile settings:

| Resource | Bound |
| --- | ---: |
| Subscribers per registration | 4 |
| Subscribers per process | 64 |
| Pending line pointers per subscriber | 128 |
| Encoded NDJSON line, including LF | 8,192 bytes |
| Heartbeat interval | 15 seconds |
| Ordinary line write and flush | 2 seconds absolute |
| Generation-stop deadline | 2 seconds absolute |

Four subscribers cover the foreground Rip owner plus independent operator
inspection without allowing one app to consume the process allowance. The
64-process cap supports many registered apps while bounding goroutines and
open control responses. A 128-pointer queue absorbs short bursts and bounds
queued line payloads at 64 × 128 × 8,192 = 64 MiB; channel pointers, one
currently writing line per subscriber, goroutine stacks, and HTTP buffers are
additional bounded or measured overhead. Shared pointers reduce ordinary
retention. The
8 KiB line cap contains the 4 KiB escaped path plus the complete fixed schema
without accepting arbitrarily large app-controlled headers. A two-second
write deadline keeps a blocked control peer from delaying reload, while a
15-second heartbeat detects trailing loss without creating high idle traffic.

After sequence reservation Janus encodes exactly one immutable exact-length
`[]byte`, including LF, releases structured source fields, and queues pointers
to that line. The same line pointer is shared across subscribers. A full queue
drops only that subscriber's pointer and advances only that subscriber's
`droppedThrough`; sequence assignment and every other subscriber continue.

Field validation and bounds are exact:

| Field | JSON type | Bound and source policy |
| --- | --- | --- |
| `v` | integer | exactly `1` |
| `type` | string | fixed ASCII enum |
| `sequence` | string | 1–20 canonical ASCII decimal bytes |
| `timestamp` | string | valid UTC RFC3339Nano, at most 35 ASCII bytes |
| `request_id` | string | materialized Caddy UUID, exactly 36 lowercase ASCII bytes |
| `app_id` | string | registration-valid ASCII, at most 70 bytes |
| `app_name` | string | registration-valid ASCII, at most 63 bytes |
| `tenant_site` | string or null | validated DNS-label ASCII, at most 63 bytes |
| `request_host` | string | normalized validated DNS ASCII, at most 253 bytes |
| `client_ip` | string | Caddy trusted-client IP parsed as an IP address, at most 64 ASCII bytes |
| `method` | string | valid HTTP token ASCII, at most 32 bytes; longer values truncate and declare |
| `path` | string | valid ASCII `EscapedPath`, query-free, at most 4,096 bytes; longer values truncate and declare |
| `status` | integer or null | 100–599, or null before commit |
| `duration_seconds` | number | finite, nonnegative |
| `response_bytes` | string | 1–20 canonical ASCII decimal bytes |
| `mime_type` | string or null | one valid UTF-8 logged header value, at most 256 bytes; absent, repeated, invalid, or oversized values omit and declare except ordinary absence |
| `response_class` | string | fixed ASCII enum, at most 32 bytes |
| `selected_upstream` | string or null | fixed `worker-` plus 16 lowercase hex digits |
| `retry_count` | integer | 0 through 4,294,967,295 |
| `outcome` | string | fixed ASCII enum, at most 32 bytes |
| `mark` | string or null | one valid UTF-8 value, at most 256 bytes; ambiguous, invalid, or oversized values omit and declare except ordinary absence |
| `truncated_fields` | string array or absent | sorted unique names from `method`, `path` |
| `omitted_fields` | string array or absent | sorted unique names from `mime_type`, `mark` |

No source field reaches `encoding/json` before its UTF-8 policy runs, so JSON
replacement characters never repair invalid input silently. Truncation
preserves complete UTF-8 code points. Ordinary absence produces a null value
without `omitted_fields`; that array means present input is rejected.

The encoded-line bound is checked after JSON escaping. If the line exceeds
8,192 bytes, Janus shortens `path` at valid escape/input boundaries and names
it in `truncated_fields`. All other variable fields already have bounds whose
simultaneous worst-case escaped representation fits the fixed envelope.
Failure of that invariant after sequence reservation consumes the sequence,
advances each attached subscriber's `droppedThrough`, emits one structured
operational error, and queues no line. Janus never emits an oversized line or
silently replaces, clips, or drops a field.

## Access event schema

Every stream record is one UTF-8 JSON object followed by one LF. There are no
blank lines and no CRLF framing. JSON member order is the order shown in the
examples. Protocol integers whose precision matters across JavaScript are
decimal strings.

An access record has this schema:

```json
{
  "v": 1,
  "type": "access",
  "sequence": "42",
  "timestamp": "2026-08-01T14:16:00.123456789Z",
  "request_id": "7a37ea98-38c6-4e23-bdc6-601590d43f04",
  "app_id": "cart-a1b2c3",
  "app_name": "cart",
  "tenant_site": "alice",
  "request_host": "alice.apps.example",
  "client_ip": "203.0.113.8",
  "method": "GET",
  "path": "/products/blue%20cup",
  "status": 200,
  "duration_seconds": 0.012345678,
  "response_bytes": "1842",
  "mime_type": "text/html; charset=utf-8",
  "response_class": "proxy",
  "selected_upstream": "worker-9f31e205bf82c630",
  "retry_count": 1,
  "outcome": "complete",
  "mark": "checkout-812"
}
```

Field rules:

- `v` is the integer `1`.
- `sequence` is the registration's canonical decimal sequence.
- `timestamp` is UTC RFC3339Nano at encoder invocation.
- `request_id` is Caddy's request UUID, materialized once at request start and
  shared with Caddy's request context.
- `app_id` and `app_name` come from the captured registration.
- `tenant_site` is the resolved `{site}` label, or `null` for an exact-host
  registration.
- `request_host` is the normalized lowercase host without a port.
- `client_ip` is Caddy's trusted client IP after its configured trusted-proxy
  processing, never a Janus parse of forwarding headers.
- `method` is the received HTTP method.
- `path` is the request's escaped path without `?` or query bytes.
- `status` is an integer 100 through 599, or `null` when no final status
  commits.
- `duration_seconds` is a finite nonnegative JSON number computed from the
  Janus start timestamp through encoder invocation, including error routes.
- `response_bytes` is a canonical nonnegative decimal string. Ordinary
  responses use Caddy recorder size; HEAD and Hub override it to `"0"`.
- `mime_type` is the single final `Content-Type` value in Caddy's logged
  application-visible response-header map with surrounding HTTP whitespace
  removed. It is not guaranteed to equal a wire-sniffed MIME type.
- `response_class` is one of `auth`, `browse_asset`, `browse_listing`,
  `browse_render`, `file`, `hub`, `janus`, `proxy`, or `sendfile`.
- `selected_upstream` is `null` when no worker is selected. Otherwise it is
  `worker-` plus the first 16 lowercase hexadecimal digits of
  HMAC-SHA-256(process-random-key, socket-path). It reveals neither a
  filesystem path nor a stable identity across process restarts.
- `retry_count` is the number of selected attempts before the final selected
  attempt. It is zero when the first selection is final or no worker is
  selected. Dial failures and retryable marked 503 responses count.
- `outcome` follows the outcome table below.
- `mark` is the final accepted worker response's `Rip-Mark`, or `null`.
- adjustment arrays are optional and contain only schema field names.

Rejected present input is explicit:

```json
{"mark":null,"mime_type":null,"omitted_fields":["mark","mime_type"]}
```

Ordinary absence has the same null fields and no `omitted_fields`.

Response-class ownership is final-response ownership. A sendfile
transformation is `sendfile`, including its Janus-generated error. A Hub 101
is `hub`. A generic Janus routing or availability response is `janus`.

## Final attempt, retries, and `Rip-Mark`

Each request starts with no selected upstream, zero attempts, and no mark.
Before each worker attempt Janus records the opaque identity and increments
the attempt count. A dial failure or replayable busy/draining 503 may lead to
another attempt.

Facts from a rejected attempt never become final response facts:

- a rejected busy/draining 503 does not set response class, MIME type, or
  mark;
- a failed attempt's mark is ignored;
- the next attempt replaces selected-upstream identity;
- `retry_count` derives from total selected attempts rather than mutable
  response metadata.

For the final accepted worker response, Janus owns `Rip-Mark` across headers
and trailers:

1. before response headers commit, remove every `Rip-Mark` header value and
   remove `Rip-Mark` from every `Trailer` declaration;
2. retain one header value as a candidate only when exactly one field value
   exists;
3. at body EOF, capture and delete every actual `Rip-Mark` trailer value
   before downstream trailer forwarding;
4. accept a mark only when exactly one source supplies exactly one nonempty,
   valid UTF-8 value no larger than 256 bytes;
5. use `mark: null` plus `"mark"` in `omitted_fields` for an empty value,
   repeated values, header-plus-trailer ambiguity, invalid UTF-8, or oversize;
6. use `mark: null` with no omission declaration when no value exists.

No header, trailer declaration, or actual trailer value reaches the client,
cache entry, or durable `resp_headers`. A final non-replayable marked 503 may
contribute its own mark because that response is the client-visible final
attempt. Retried responses never contribute.

## Response outcomes

Exactly one outcome is selected in this precedence order:

| Outcome | Rule |
| --- | --- |
| `upgraded` | Janus accepts a Hub upgrade and overrides status to 101. Duration covers request and handshake handling through encoder invocation; response bytes are `"0"`. |
| `upstream_aborted` | A final worker response starts, then its body read fails while the client context remains live. |
| `client_canceled` | The request context is canceled before normal completion, including a write failure attributable to the disconnected client. |
| `write_error` | A client-bound write or flush fails while the request context remains live and no upstream body-read failure owns the result. |
| `complete` | Handling and any Caddy error route return normally with no condition above. |

`upstream_aborted`, `client_canceled`, and `write_error` retain a committed
status and partial canonical response-byte string when present. They use
`status: null` when no final status commits. Caddy's implicit status zero
becomes 200 only for `complete`.

A custom Caddy error route may replace a handler's proposed error status,
headers, and body. The event records the error route's final status, final
logged-header-map MIME type, and final Caddy recorder size. The Janus duration
includes that route because encoder invocation supplies the endpoint.
Handler-local proposed values never override the completion fields. HEAD and
Hub retain their documented byte overrides.

## Control protocol

### Process status

```text
GET /1.0/access
```

returns 200 JSON:

```json
{
  "provisioned_encoders": 2,
  "registrations": 12,
  "subscribers": 3,
  "caps": {
    "subscribers_per_registration": 4,
    "subscribers_per_process": 64,
    "queue_events": 128,
    "line_bytes": 8192,
    "heartbeat_seconds": 15,
    "write_deadline_seconds": 2
  },
  "counters": {
    "published": 18421,
    "subscriber_overflows": 3,
    "stream_opens": 19,
    "stream_closes": 16,
    "write_timeouts": 0,
    "invariant_failures": 0
  }
}
```

`provisioned_encoders` counts provisioned module instances that hold a bridge
reference, including instances in overlapping or not-yet-committed Caddy
generations. Caddy exposes no encoder Start/Stop seam, so this count is
advisory and makes no active-serving or readiness promise. It never gates
stream admission and does not prove that a parent core invokes any provisioned
encoder; parent policy may filter, sample, skip, disable, or discard every
entry. Counters are pooled, monotonic until process exit, and independently
loaded rather than a transactional snapshot. Status exposes no path, mark,
host, client IP, or upstream identity.

Every status count is a nonnegative JSON integer within uint64. Every cap is
the positive JSON integer shown. The response has exactly the top-level
members and nested members shown above; no per-app objects or optional members
exist.

### App stream

```text
GET /1.0/apps/{id}/access?after=<decimal-string>
```

The raw query is exactly the ASCII bytes `after=` followed by canonical
unsigned decimal: `0`, or a nonzero value with no leading zero, within the
sequence range. Percent encoding, `+`, semicolons, empty values, repeated
keys, and every extra query byte reject. Janus validates RawQuery directly
rather than accepting a decoded equivalent. URL fragments never reach an HTTP
server and do not participate in this protocol.

The request is bodyless only when `ContentLength == 0` and
`len(TransferEncoding) == 0`; Janus rejects every other body shape without
reading it. `Content-Length: 0` is legal. Nonzero, unknown-length, and
chunked-empty bodies reject.

Validation precedence is exact:

1. method;
2. body shape;
3. raw query syntax and decimal range;
4. app existence and tombstone;
5. cursor against current head;
6. per-registration cap;
7. process cap;
8. response-controller support.

The handler requires `r.Method == "GET"` even though Go's GET route pattern
also matches HEAD. Every non-GET, including HEAD, returns 405 with
`Allow: GET`.

Before committing status, Janus walks the response writer's `Unwrap` chain to
prove `http.Flusher` support, creates an `http.ResponseController`, and sets
the deadline selected by the generation rule below; without generation stop
that deadline is two seconds ahead. It does not call Flush as a probe because
that commits status. Missing Flusher support or deadline setup failure
releases any reserved subscriber counts and returns 500 JSON before status
200.

Success returns:

```http
HTTP/1.1 200 OK
Content-Type: application/x-ndjson
Cache-Control: no-store
X-Content-Type-Options: nosniff
```

With the initial deadline already set, Janus commits 200, writes the complete
pre-encoded hello line, and calls `ResponseController.Flush`. Each ordinary
later line resets one absolute deadline to two seconds after selection, then
writes the complete line and flushes. Errors satisfying
`errors.Is(err, os.ErrDeadlineExceeded)` increment `write_timeouts`; every
write or flush error detaches the subscriber exactly once. EOF is
authoritative. A `closed` line is advisory because a disconnect or failed
final write can prevent it.

Once generation stop begins, the deadline selected for every status, hello,
access, gap, heartbeat, closed, and flush operation is:

```text
min(now + 2 seconds, generation stop deadline)
```

The writer reads stop state and chooses this deadline under the same
generation-stream mutex that arbitrates line selection. A stop racing with
selection either supplies the stop-capped deadline to that write or interrupts
the selected write by advancing the connection deadline to the stop deadline;
no ordinary deadline survives past generation stop.

The pre-stream response matrix is:

| Condition | Status | Response |
| --- | ---: | --- |
| Valid live registration and capacity | 200 | NDJSON stream |
| Unknown or tombstoned app id | 404 | JSON API error |
| Request body present | 400 | JSON API error |
| Missing, repeated, escaped, noncanonical, negative, overflowed, or extra query | 400 | JSON API error |
| `after` exceeds current head | 409 | JSON API error naming cursor and head |
| Per-registration or process cap reached | 429 | JSON API error, `Retry-After: 1` |
| Method other than GET | 405 | `Allow: GET` |
| Response deadline or flush unsupported | 500 | JSON API error before 200 |

Once status 200 commits, protocol and write failures close the response; they
never substitute an HTTP error body into the NDJSON stream.

## Stream records

Version 1 objects have exact members:

| Type | Required members | Optional members |
| --- | --- | --- |
| `hello` | `v`, `type`, `app_id`, `app_name`, `after`, `head`, `heartbeat_seconds`, `max_line_bytes` | none |
| `access` | every access field through `mark` shown in the schema | nonempty `truncated_fields`, nonempty `omitted_fields` |
| `gap` | `v`, `type`, `app_id`, `from`, `through`, `head`, `reason` | none |
| `heartbeat` | `v`, `type`, `app_id`, `head`, `timestamp` | none |
| `closed` | `v`, `type`, `app_id`, `head`, `timestamp`, `reason` | none |

Every object uses the member order shown by its example. Duplicate, missing,
unknown, out-of-order, or extra members are malformed. `after`, `head`,
`from`, `through`, `sequence`, and `response_bytes` are canonical uint64
decimal strings. `heartbeat_seconds` is exactly 15 and `max_line_bytes` is
exactly 8192. Gap reason is `no_replay` or `overflow`; close reason is one of
the four values defined below. All timestamps are UTC RFC3339Nano.

The first record is always `hello`:

```json
{"v":1,"type":"hello","app_id":"cart-a1b2c3","app_name":"cart","after":"38","head":"42","heartbeat_seconds":15,"max_line_bytes":8192}
```

Subscriber attachment and the `head` snapshot occur under the access-state
mutex. Events assigned after that snapshot enter the new subscriber's queue.
If `after` is below `head`, `hello` is followed immediately by:

```json
{"v":1,"type":"gap","app_id":"cart-a1b2c3","from":"39","through":"42","head":"42","reason":"no_replay"}
```

There is no replay. `after` reports the last access sequence the client has
accounted for; it never asks Janus to retain or resend an event. The initial
gap makes reconnect loss explicit. After writing that gap, the writer sets
`accounted = hello.head`. If `after == head`, it sets the same value after
hello without a gap.

Normal events follow:

```json
{"v":1,"type":"access","sequence":"43","timestamp":"2026-08-01T14:16:01.100Z","request_id":"10d21165-2701-4c37-b948-c93d891da3b1","app_id":"cart-a1b2c3","app_name":"cart","tenant_site":null,"request_host":"cart.ripdev.io","client_ip":"127.0.0.1","method":"GET","path":"/","status":200,"duration_seconds":0.0021,"response_bytes":"918","mime_type":"text/html; charset=utf-8","response_class":"file","selected_upstream":null,"retry_count":0,"outcome":"complete","mark":null}
```

Each subscriber tracks `accounted` and `droppedThrough`. Publication changes
`droppedThrough` only when enqueue of that sequence fails, or when a
post-reservation encoding failure leaves no line for any subscriber. A
successful enqueue never advances it.

The writer processes queued lines in sequence order. If the next queued
sequence exceeds `accounted + 1`, the missing interval must end no later than
`droppedThrough`; the writer emits exactly:

```json
{"v":1,"type":"gap","app_id":"cart-a1b2c3","from":"44","through":"51","head":"52","reason":"overflow"}
```

and then emits event 52. A queue overflow does not enqueue a gap object.
The writer derives the gap from sequence discontinuity and advances
`accounted` to `through`, then advances it again only after event 52 writes and
flushes successfully. `from` always equals the prior `accounted + 1`,
`through >= from`, `through <= droppedThrough`, and the next access sequence,
when present, is `through + 1`. Violation is an internal invariant failure
that ends the stream.

When heartbeat or close is due, the writer first drains every queued
successful line needed to reach the first actual discontinuity at or below
`droppedThrough`. It reports a trailing overflow gap only when
`droppedThrough > accounted`, and only through `droppedThrough`. It never gaps
or discards a queued successful line merely because registration head is
ahead. It then emits:

```json
{"v":1,"type":"heartbeat","app_id":"cart-a1b2c3","head":"61","timestamp":"2026-08-01T14:16:15Z"}
```

The heartbeat `head` is the current registration head and remains diagnostic;
it may exceed both `accounted` and `droppedThrough` while intact lines remain
queued. This rule reports a final dropped line when no later access event
arrives without inventing loss for a slow but lossless writer. Heartbeat
service cannot starve, but preserving queued lines precedes its gap.

An orderly server-side end first reports any trailing overflow gap and then
attempts:

```json
{"v":1,"type":"closed","app_id":"cart-a1b2c3","head":"61","timestamp":"2026-08-01T14:16:20Z","reason":"generation_stop"}
```

Legal close reasons are `generation_stop`, `registration_deleted`,
`registration_reaped`, and `sequence_exhausted`. Caddy's App Stop callback
does not identify reload versus process shutdown, so Janus never guesses
between them. Client disconnect and write timeout normally end at EOF without
`closed`.

The client advances its cursor after successful consumption of an access
record's `sequence` or a gap's `through`. A heartbeat or closed `head` never
advances it.

## Lifecycle and races

The separate pooled access bridge owns sequence state, process caps, and
counters. App and encoder references keep it alive across Caddy generation
overlap. Encoder provisioning and cleanup adjust `provisioned_encoders`;
neither transition claims that the generation serves traffic. During a
successful reload old and new instances may overlap, but the per-request
publication guard prevents duplicates and pooled sequence state prevents
reset.

Each control-server generation owns the stream handlers it accepts. During
App stop it performs this order:

1. establish one absolute stop deadline no later than two seconds from the
   stop signal;
2. atomically detach all of its subscribers from registration and bridge
   accounting, recording reason `generation_stop`;
3. wake all writers concurrently;
4. use the one stop deadline for every remaining gap, closed, and flush
   attempt;
5. wait until handlers exit or that deadline expires;
6. at deadline, force-close every still-tracked control connection owned by
   that generation without closing the pooled listener;
7. wait for forced handlers to exit;
8. call the control servers' `Shutdown`.

No final-line operation resets or extends the stop deadline. Stream
detachment therefore occurs before network I/O and all 64 blocked streams
finish their handler paths within the same bound. Long-lived streams do not
consume the control server's five-second shutdown budget. Acceptance requires
total stream-handler exit within three seconds and no control-server shutdown
timeout. Each generation tracks accepted control connections through
`ConnState`; forced close affects that generation's connections, not the
replacement generation or pooled listener.

Caddy starts the replacement generation before stopping the serving
generation. A client reconnects to the pooled listener, attaches to the same
registration state, and supplies its last accounted sequence. Events that
complete between old-stream close and new attachment advance the head and
appear as `no_replay` loss. Live stream continuity is deliberately not
preserved across reload.

DELETE and heartbeat reap race with completion as follows:

- publication holding the access-state mutex before tombstone reserves the
  next sequence and targets subscribers attached at that point;
- tombstone holding the registry and access-state mutexes first rejects the
  late publication;
- no publication enters a new registration state;
- subscriber attachment under the registry, access-state, and accounting
  locks either attaches to a live state or receives 404.

Subscriber-cap admission and removal are atomic across per-registration and
process counts. A failed header write removes the subscriber exactly once.
A blocked subscriber cannot hold the publication mutex while performing I/O.

Early host resolution exists only to attribute a response completed by auth.
It does not become a routing snapshot and never changes existing routing or
re-resolution behavior:

1. before auth, facts atomically receive one provisional owner tuple
   `(app_id, app_name, tenant_site, *accessState)`;
2. if auth completes the request, that tuple owns the event;
3. if auth passes, Janus clears the provisional tuple before normal routing;
4. every existing routing resolution runs at its existing seam;
5. each successful routing or re-resolution atomically replaces the complete
   owner tuple with that resolved `AppRecord`;
6. a routing resolution with no record clears the owner tuple;
7. final publication uses only the last complete tuple and its tombstone.

The atomic tuple prevents mixed identity and access-state fields. It preserves
host re-resolution after cache waits, doorbell rings, and every other current
registry refresh. DELETE plus same-host re-registration between auth
attribution and routing can therefore route to the new record; facts switch to
that new record as one operation. A late completion publishes only when the
final owner's access state remains live.

Owner replacement runs under the facts mutex. Re-resolution to the same
`*accessState` refreshes the record/site snapshot and retains attempt facts.
Replacement with a different access-state pointer clears response class,
cache verdict, selected upstream, retry count, mark candidates, and
owner-specific outcome facts before installing the new tuple; subsequent
owning seams repopulate them. No fact from the displaced registration
contaminates the final owner's event.

Provisioning failure, aborted reload, old/new overlap, final request
completion during retirement, and final bridge destruction all obey balanced
UsagePool references. Cleanup cannot destroy a bridge still held by either an
App or encoder.

Final Janus-state destruction uses the established lock order before registry
memory or the App's bridge reference disappears:

1. hold `appRegistry.mu`;
2. for every registration, lock its `accessState.mu`, then bridge accounting;
3. tombstone the state, detach subscribers, remove claims and registry
   entries, and record `generation_stop`;
4. release accounting and access locks, then the registry lock;
5. wake detached writers and close generation control connections without
   registry locks;
6. destroy registry state;
7. release the App's accessBridge UsagePool reference.

An encoder reference may keep the bridge alive after step 7; final bridge
release occurs only after all App and encoder references balance.

## Processing order

```text
request enters Caddy server
→ Caddy creates trusted client IP, outer response recorder, and ExtraLogFields
→ Janus creates request facts with start timestamp
→ Janus materializes Caddy request UUID
→ ping
→ provisional registered-host lookup for auth attribution only
→ auth completes with provisional owner, or passes and clears it
→ path validation
→ ordinary routing resolves at its existing seam and atomically sets owner
→ Hub, files, browse, doorbell, or worker re-resolution may replace it
→ retries; each rejected attempt is discarded from final facts
→ final response transformation and Rip-Mark header/declaration scrub
→ body EOF Rip-Mark trailer capture and scrub
→ Caddy response encoding and error routes
→ Caddy access-log completion
→ Janus encoder extracts clone-captured request and SkipType facts
→ Janus computes duration through encoder invocation
→ bounded event uses the final owner and reserves its next sequence
→ one immutable LF-terminated line is encoded
→ nonblocking line-pointer publication to subscriber queues
→ wrapped JSON encoder writes the durable access-log entry
```

Attribution never bypasses auth or routing. Routing remains authoritative for
both response selection and final event ownership.

## Privacy and security

Control access follows Capability 2 exactly:

- `internal` trusts every principal the operating system permits to connect to
  the Unix socket, plus Bearer when configured; on Unix Janus creates a
  missing parent with mode 0755 and Caddy's unqualified socket address applies
  its current owner-write-only 0200 socket mode;
- `local` trusts every process able to reach the loopback listener, plus
  Bearer when configured; its admitted hosts remain loopback
  `127.0.0.1`, `::1`, or `localhost`;
- `public` requires TLS and Bearer.

The current internal socket ownership/mode and local loopback exposure are
accepted parts of the operator trust boundary and receive explicit acceptance
pins; Janus does not imply peer credentials, per-user isolation, or mandatory
Bearer on those modes. Any principal admitted by the listener may subscribe
to any registered app id. Janus does not implement per-app stream
credentials.

The stream deliberately excludes:

- query strings and fragments;
- request and response headers other than the derived MIME value;
- cookies, authorization values, and user ids;
- request and response bodies;
- filesystem paths;
- raw upstream socket paths;
- worker busy/draining markers.

The request path, app identity, trusted client IP, logged MIME type, response size,
timing, and app-supplied mark remain sensitive operator data. Public control
mode therefore retains its token and TLS requirements.

The durable Caddy JSON entry is unchanged and may contain Caddy's normal
request URI, including query bytes. Operators apply Caddy log filtering and
retention policy to that durable sink. The narrower stream schema does not
redact or redefine the durable log.

`Rip-Mark` is untrusted application text. Bounds and UTF-8 validation apply
before publication, and the value is JSON-escaped. Header values, trailer
declarations, and actual trailer values are removed before client forwarding,
cache storage, and durable response-header logging.

## Hard errors

Janus rejects or reports loudly:

- malformed `format janus` Caddyfile syntax;
- an unknown encoder subdirective;
- malformed, repeated, missing, or out-of-range `after`;
- an extra stream query key;
- a stream request body;
- a cursor ahead of the registration head;
- stream admission beyond either subscriber cap;
- a response writer without deadline and flush controller support;
- a line that cannot satisfy the fixed encoded bound;
- invalid UTF-8 in a field whose policy requires omission or failure;
- a sequence increment beyond uint64;
- an event with an unknown response class, cache verdict, or outcome;
- a duplicate or malformed reserved facts sentinel;
- a duplicate publication attempt that carries conflicting completion facts.

An encoder invariant failure increments `invariant_failures`, logs one
structured service error with the available logger/entry identity and, when a
valid facts pointer supplies them, request id and app id. It still delegates
the durable Caddy JSON and never emits malformed NDJSON. Sentinel absence and
a valid sentinel with no final owner are normal exclusions, not invariant
failures.

## Non-goals

- Durable event retention or replay.
- Delivery acknowledgment.
- Exactly-once delivery across disconnect, overflow, DELETE, reap, reload, or
  process exit.
- Streams for cold roots, control traffic, ACME, or unknown hosts.
- Per-app authorization inside one control listener.
- A browser EventSource protocol or WebSocket access-log transport.
- Query, header, cookie, body, or user-identity logging in the stream.
- A configurable queue, line, subscriber, heartbeat, or deadline limit.
- Preserving live control responses across Caddy reload.
- Replacing, parsing from, or suppressing Caddy's durable access log.
- Inspecting, validating, or rejecting parent Caddy writer, core, sampling,
  level, host mapping, or access-log enablement.
- Publishing or accounting for an entry Caddy excludes before encoder
  invocation through disabled/unmapped/level-filtered/sampled/filtered/
  discarded policy or runtime `log_skip`.
- Claiming that `provisioned_encoders` identifies an active serving
  generation or a parent core that invokes it.
- Claiming that logged MIME is a wire-sniffed header snapshot.
- Measuring a Hub connection's lifetime.
- Janus-side presentation, ANSI color, picture formatting, or stdout
  ownership.

## Acceptance matrix

Go tests pin:

| Area | Required cases |
| --- | --- |
| Encoder parity | absent sentinel on ACME and pre-handler entries delegates unchanged with no publish/invariant; SkipType sentinel contributes zero bytes; duplicate/wrong sentinel is invariant; `janus` and `json` produce identical durable bytes for all common options; Clone, WithLazy, With, AddObject, namespaces, multiple outputs, and publication failure preserve durable JSON |
| Caddy policy boundary | direct invocation for an eligible Janus entry publishes; disabled logging, unmapped/skipped host, excluding level, sampling, filtering/discard core, discard writer, and runtime `log_skip` prevent invocation or publication without consuming sequence or producing gap; none is rejected by Janus encoder config |
| Completion seam | custom error route status/body and Janus duration; implicit 200; logged-map MIME mutations and absent wire-sniff MIME; identity/gzip/zstd recorder bytes; HEAD override; 304; ranges; partial writes |
| Request facts | UUID materializes once; exact SkipType extraction; provisional owner exists only for auth; every routing resolution atomically replaces or clears the complete owner tuple; same-owner refresh preserves and cross-owner replacement clears owner-specific facts |
| Registered scope | exact host, `{site}`, auth denial, delete/re-register between attribution and routing, doorbell re-resolution, unknown host, cold root, ping, control, ACME, and pre-handler errors |
| Response classes | auth, ordinary file, browse asset/listing/renderer, sendfile success/failure, proxy, Hub, and Janus-generated response |
| Retries | dial failure, marked busy/draining retry, all busy, final accepted attempt, non-replayable final 503, and no cross-attempt mark contamination |
| Rip-Mark | absent; one empty/nonempty header; one trailer; repeated; header plus trailer; invalid UTF-8; exact bound; oversized; declaration/header/trailer scrub against real chunked upstream, client, durable log, and stream |
| Outcomes | complete, Hub 101 handshake-duration override, HEAD zero-byte override, client cancellation before/after headers, upstream body abort, and non-cancellation write error |
| Schema bounds | every table boundary and JSON type; invalid UTF-8 without replacement; simultaneous worst-case escaping; exact adjustment arrays; response bytes at 2^53−1, 2^53, and recorder maximum; 8,192-byte final line |
| Sequence | first event; concurrent completion order; values beyond JavaScript safe integer; uint64 maximum; no wrap; forced post-reservation encoding failure followed by later event and heartbeat-only report |
| Queue loss | full queue without enqueue failure emits no gap; slow lossless writer emits no gap; every enqueue-failure position; successful lines around a dropped interval retain order; final dropped line reports at heartbeat and close; no discard through head |
| Stream protocol | exact member order/types for hello/access/gap/heartbeat/closed; immediate hello; one initial no-replay gap then accounted=head; strict gap invariant; trailing gap only through droppedThrough; EOF authority |
| Admission | explicit HEAD rejection; method/body/query/app/cursor/cap/controller precedence; Content-Length 0/nonzero/unknown; chunked empty; escaped/semicolon/empty/repeated/extra query; cursor ahead; both caps; exact headers |
| Write control | unsupported controller before 200; blocked status/hello/access/gap/heartbeat/closed/flush; every post-stop write uses min deadline; write-selection/stop race; deadline force-closes only that generation's tracked connections before Shutdown; timeout classification; exactly-once detachment |
| Registration lifecycle | `*accessState` clone identity; lock-order assertions; PATCH and upstream PUT retain state; DELETE/reap use one primitive; late completion; same-name re-registration receives fresh state |
| Pool lifecycle | App and encoder balanced acquisition; provisioning failure; aborted reload; overlap; retirement completion; final Janus destruction tombstones/detaches all registrations under lock before registry destruction and App bridge release; final bridge destruction has zero attachments |
| Reload | pooled sequence; duplicate-encoder guard; generation_stop close; new attach; explicit reconnect gap; 64 blocked streams under one two-second deadline; total under three seconds; no shutdown timeout |
| Privacy | no query, credentials, headers, body, raw socket, or filesystem path in any stream record |
| Control trust | internal Unix parent 0755/socket 0200 exposure, local loopback exposure on every admitted loopback spelling, optional Bearer on each, required TLS/Bearer on public, and app id as filter rather than credential |
| Status | provisioned count for zero/one/overlap/failed generation/cleanup/final destruction; no readiness or parent-invocation inference even when parent policy discards every entry; exact caps, redaction, counters, and independent snapshot semantics |

The foreground Caddy suite proves:

- root and example Janus Caddyfiles adapt with real, unsampled, unfiltered,
  non-discard sinks and mappings that invoke `format janus` for managed apps;
- a real HTTPS registered request under that policy produces matching durable
  JSON and one streamed access event;
- ACME and a pre-Janus-handler failure produce unchanged durable entries, no
  access sequence, and no invariant failure;
- an error route changes final status and bytes observed by the event;
- identity, gzip, and zstd responses report Caddy recorder body sizes;
- registered files, browse, sendfile, auth, proxy, and Hub 101 produce
  their documented classes;
- a busy worker retry contributes only the final attempt's mark and metadata;
- a blocked stream overflows without delaying unrelated requests;
- DELETE and re-registration create disjoint sequence spaces;
- a Caddy reload closes the old stream, keeps the registration and sequence,
  accepts a reconnect, reports intervening loss, and completes without a
  shutdown timeout.

Hub acceptance uses a real WebSocket handshake and requires `status: 101`,
`response_class: "hub"`, `outcome: "upgraded"`, `response_bytes: "0"`, and a
duration ending at access-encoder invocation rather than connection close.

## Benchmarks and retained evidence

The retained 2026-08-01 Apple M5 run uses Go 1.26.5, five samples per case, the
same request/event corpus, and the same durable JSON sink. Each isolated case
uses a 500 ms benchtime and includes encoder construction, request capture,
completion encoding, bounded access-event construction, publication, and
durable JSON encoding. Medians are:

- Caddy JSON baseline: 590.1 ns/op, 1,425 B/op, 8 allocs/op;
- Janus with no subscribers: 1,874 ns/op, 3,384 B/op, 23 allocs/op;
- one draining subscriber: 2,188 ns/op, 3,384 B/op, 23 allocs/op;
- four draining subscribers on one registration: 2,413 ns/op, 3,384 B/op,
  23 allocs/op;
- one blocked full queue taking the overflow branch: 1,856 ns/op, 3,384 B/op,
  23 allocs/op;
- 64 draining subscribers across 64 registrations, publishing round-robin:
  2,073 ns/op, 3,383 B/op, 23 allocs/op.

The no-subscriber Janus tax over the equivalent JSON construction and durable
encoding is therefore 1,283.9 ns/op, 1,959 B/op, and 15 allocations on this
machine. One draining subscriber adds a median 314 ns/op over no subscribers;
four-subscriber fan-out adds 539 ns/op. The overflow median is 18 ns/op below
the no-subscriber median, so this run resolves no incremental overflow-branch
latency; the branch never performs network or disk I/O.

Separate route-only and route-plus-access pairs exercise the full Janus handler
rather than wrapping a trivial helper in a network benchmark. Both arms install
Caddy request variables and a response recorder. The access arm additionally
attaches request facts once, materializes the Caddy UUID, wraps the response,
allows the real route to populate owner/class/attempt/upgrade facts, and invokes
the Janus completion encoder on those same facts. Median route-only →
route-plus-access times are:

- registered 64 KiB file: 29.750 µs → 35.748 µs (+5.998 µs, +20.2%),
  +5,290 B/op and +51 allocs/op;
- 1 MiB Unix-socket `X-Sendfile`: 187.656 µs → 204.710 µs (+17.054 µs,
  +9.1%), +8,056 B/op and +65 allocs/op;
- transparent gzip Unix-socket proxy response: 74.119 µs → 80.782 µs
  (+6.663 µs, +9.0%), +6,344 B/op and +54 allocs/op;
- proxied zstd representation: 26.110 µs → 31.677 µs (+5.567 µs, +21.3%),
  +5,828 B/op and +54 allocs/op;
- real Janus WebSocket 101 handshake: 84.637 µs → 89.657 µs (+5.020 µs,
  +5.9%), +5,068 B/op and +47 allocs/op.

These paired deltas include all Capability 9 request and completion machinery
exactly once in the route-plus-access arm. They are not transport
acknowledgments and do not measure upgraded-connection lifetime. The synthetic
completion-corpus cases for file, sendfile, gzip, zstd, and WebSocket measure
only the encoder seam and are named separately from the real path pairs.

Raw output, exact commands, toolchain, machine details, and working-tree
provenance are retained in
`docs/20260801-102358-bench-raw-access-log.txt`. The isolated encoder matrix
uses a 500 ms benchtime; the route pairs use a 1 s benchtime. Both use five
samples. Benchmarks measure and explain trade-offs; they are not correctness
gates.

## Rip client interoperability

This section constrains a Rip consumer of the protocol. It does not add
presentation code or stream handling to Janus.

Rip exposes `--access-log=pretty|raw|off`, with deterministic default
`pretty`, and `--access-format <picture>` for pretty mode only. `raw` means
validated byte-preserving NDJSON, not reserialized JSON. `off` opens no
subscription.

### Registration and cursor

A foreground manager owns exactly one subscription for each registration
generation. Its cursor starts at `"0"` for every new app id. Re-registration
atomically:

1. increments the local registration generation;
2. disables callbacks and reconnect from the old generation;
3. aborts the old stream;
4. installs the new app id with cursor `"0"`;
5. connects only the new generation.

A stale read, timer, or callback checks its captured generation before
mutation and cannot reconnect.

For each connection:

- requested `after` equals the current cursor;
- hello `app_id` equals the current app id and hello `after` equals the
  requested cursor;
- hello `head` is canonical and not below `after`;
- when `head > after`, exactly one immediate `no_replay` gap has
  `from = after + 1` and `through = head`;
- when `head == after`, no initial gap exists;
- every access sequence is cursor + 1 unless one valid gap first advances the
  cursor;
- every gap has `from = cursor + 1`, `through >= from`, and canonical values;
- access cursor advancement occurs only after the raw line writes or the
  pretty line renders and writes successfully;
- in raw mode, a gap writes its stderr diagnostic first, then its exact raw
  stdout line, then advances the cursor; in pretty mode it writes the stderr
  diagnostic, then advances;
- heartbeat and closed heads never advance the cursor;
- malformed, duplicate, regressive, skipped, or cross-app records terminate
  that connection and enter reconnect backoff.

The first connection attempt is immediate with backoff base 100ms. After a
connection ends without a terminal output failure, Rip samples full jitter
uniformly from the closed duration interval `[0, base]`, performs that
abortable sleep, then sets `base = min(base × 2, 5s)` before the next failure.
Randomness enters through an injectable sampler seam. A valid hello
immediately resets base to 100ms; TCP success, HTTP 200, or an invalid hello
does not. Sleep, connect, body read, and response handling are abortable by
generation replacement or shutdown. A heartbeat-lease browse command
re-registers after heartbeat 404 and moves its stream through the same
generation cut. A process-lease command that exits after registration has no
foreground owner and rejects access-log flags.

### Framing and raw preservation

One incremental framer handles arbitrary chunks:

1. accumulate bytes through LF;
2. count the complete line including LF;
3. if the count exceeds 8,192 before LF, discard through the next LF,
   diagnose, and terminate the connection;
4. reject an empty line, CRLF, invalid UTF-8, malformed JSON, duplicate JSON
   members, unknown `v`, unknown `type`, missing/extra members, or a member
   with the wrong exact schema type;
5. discard an incomplete trailing record at EOF, diagnose it, and reconnect
   without writing those bytes.

Raw mode writes every accepted complete source line, including its original
LF, byte-for-byte to stdout. Validation never reorders members, normalizes
escapes, or reserializes. It writes no hello/heartbeat/access/gap/closed line
until the complete line passes validation. Hello and heartbeat still appear
on raw stdout because raw mode preserves the complete Janus stream. Protocol
diagnostics are separate stderr lines.

For a raw gap, the diagnostic write to stderr completes first, the exact raw
line writes to stdout second, and cursor mutation occurs third. Any output
write failure in raw mode—diagnostic or record, stderr or stdout, including a
partial stdout write—is terminal: Rip disables reconnect, aborts the stream,
performs shutdown cleanup, and never retries from an ambiguous output
boundary. It exits status 1 unless a signal already owns the exit status.

### Descriptor ownership

The descriptor matrix is normative:

| Producer | `pretty` | `raw` | `off` |
| --- | --- | --- | --- |
| Access stream | formatted access lines on stdout | exact NDJSON stdout | none |
| Manager startup report | stdout | stderr | stdout |
| Manager lifecycle notices | stdout | stderr | stdout |
| Gap/protocol/reconnect diagnostics | stderr | stderr | stderr |
| Worker stdout | inherited stdout | pumped to stderr | inherited stdout |
| Worker stderr | inherited stderr | pumped to stderr | inherited stderr |

Raw mode starts both worker pumps immediately, before registration or access
setup, and drains them independently with destination backpressure. Worker
output beyond pipe capacity cannot deadlock worker startup or shutdown.
Access stdout backpressure controls cursor advancement. Stdout EPIPE is a
terminal output failure under the general raw-write rule: Rip disables
reconnect, aborts access I/O, deregisters, drains workers, and exits status 1.

### Signals

The first SIGINT or SIGTERM:

1. records exit status 130 or 143 respectively;
2. disables every reconnect source;
3. aborts access reads and pending backoff;
4. deregisters;
5. drains and terminates workers;
6. exits with the recorded signal status after orderly cleanup.

A second SIGINT or SIGTERM skips cleanup waits, sends that signal to the
managed process tree, and exits immediately with 128 plus the signal number.
Direct manager execution and `bin/rip` have identical behavior. The wrapper
uses an asynchronously supervised child, forwards first and second signals,
and propagates ordinary child status when no wrapper signal owns termination.

### Picture grammar

The picture is parsed as Unicode scalar values with zero-based UTF-16 offsets
for positioned errors:

```ebnf
picture     = { text | "{{" | "}}" | replacement } ;
replacement = "{" field [ ":" format ] "}" ;
format      = width-format [ scale ] | scale ;
width-format = [ alignment ] width [ middle-overflow ] ;
alignment   = "<" | ">" | "^" ;
middle-overflow = "^" ;
width       = nonzero-digit { digit } ;
scale       = "@" unit ;
unit        = unit-scalar { unit-scalar } ;
field       = identifier-run ;
text        = text-scalar { text-scalar } ;
digit       = "0" … "9" ;
nonzero-digit = "1" … "9" ;
```

Width formatting always precedes scale syntax when both appear. Examples:

```text
{app_name}                 scalar substitution
{app_name:10}              left-aligned exact width
{app_name:<10}             explicit left-aligned exact width
{mime_type:>6}             right-aligned exact width
{path:^40}                 centered exact width with edge overflow
{path:40^}                 exact width with middle overflow
{duration_seconds:@s}      scaled with unit s
{response_bytes:>8@B}      scale, then exact-width formatting
{duration_seconds:20^@Hz}  scale, then middle overflow
{duration_seconds:@Hz}     arbitrary multi-character unit
{response_bytes:@ }        one-space unit
{{ and }}                  literal braces
```

`text-scalar` is a Unicode scalar except `{`, `}`, C0/C1, DEL, bidi controls,
U+2028, and U+2029. `unit-scalar` has the same exclusions and also excludes
braces by construction. An unescaped brace rejects.
`identifier-run` is exactly one run returned by Rip's shared
`identifierRuns`, and `isIdentifierName` must accept the complete run. Those
two lexer seams are the only identifier vocabulary; the picture parser adds
no character class or regex. The accepted identifier is then checked against
the fixed presentation-field whitelist below. `width` is an exact 1 through
1,024 terminal columns. Leading `^` requires width 3 or greater and cannot be
combined with trailing `^`; `^width^` rejects. `unit` is any nonempty sequence
except `{`, `}`, C0/C1, DEL, bidi controls, U+2028, and U+2029.
Ordinary spaces, mixed case, punctuation, and arbitrary safe Unicode are
legal; one ordinary space is a complete unit. There is no unit vocabulary.

For natural display width `N <= S`, omitted/`<` alignment pads right, `>` pads
left, and leading `^` puts `floor((S-N)/2)` spaces on the left and the
remainder on the right. For `N > S`, omitted/`<` retains the head and ends in
one ellipsis, `>` starts with one ellipsis and retains the tail, and leading
`^` puts one ellipsis at each outer edge around the centered interior.
Trailing `^` instead puts ellipsis in the middle with
`L=floor(S/2)`, `R=S-L-1`, and `prefix(L) + … + suffix(R)`. Its preceding
omitted/`<`/`>` alignment affects only short-value padding.

Legal raw scalar fields are:

```text
sequence timestamp request_id app_id app_name tenant_site request_host
client_ip method path status duration_seconds response_bytes mime_type
response_class selected_upstream retry_count outcome mark
```

`truncated_fields` and `omitted_fields` are arrays and are not picture fields.
The renderer also provides:

- `local_time`: event timestamp in the process local zone as
  `YYYY-MM-DD HH:mm:ss.SSS`;
- `local_timezone`: the same instant's numeric offset as `+HHMM` or `-HHMM`;
- `mime_abbrev`: the lowercase MIME subtype before parameters, mapped by the
  exact v3 table:

```text
html→html  css→css  javascript→js  json→json  plain→text  xml→xml
png→png  jpeg→jpg  gif→gif  webp→webp  avif→avif
svg+xml→svg  svg→svg  x-icon→icon  vnd.microsoft.icon→icon
woff→font  woff2→font  ttf→font  otf→font
mpeg→mp3  mp4→mp4  ogg→ogg  webm→webm
pdf→pdf  zip→zip  gzip→gz  wasm→wasm
octet-stream→bin  x-rip→rip
```

The unmapped subtype remains unchanged. Absent MIME renders `-`.

An unknown field rejects at startup. A null scalar renders `-`. String,
integer, number, and canonical-decimal-string fields render their source
value. Scale is legal only for `duration_seconds` and `response_bytes`.
Wrong runtime types make the event malformed and terminate the connection.

### Scaling oracle

Picture scaling is the tested v3 `scale(value, unit, true)` oracle:

1. accept a positive finite JavaScript Number; canonical decimal
   `response_bytes` converts with `Number` for presentation only;
2. start at the blank prefix in `T G M k [blank] m µ n p`;
3. while value `< 0.995`, multiply by 1,000 and move one slot toward pico;
4. while value `>= 999.5`, divide by 1,000 and move one slot toward tera;
5. outside pico-through-tera renders `???` plus one blank prefix column plus
   unit;
6. otherwise round to one decimal; values whose rounded tenth is at least 10
   render a rounded integer, smaller values render one decimal; left-pad the
   significand to three characters;
7. zero renders `  0 ` plus unit;
8. negative, NaN, and either infinity render `??? ` plus unit.

Thus the replacement is three significand columns, one SI-prefix column, then
the supplied unit. Tests use the v3 function as the byte oracle and cover
zero, invalid values, every threshold neighbor around 0.995 and 999.5,
post-rounding promotion, and pico/tera overflow.

### Unicode safety and width

Event strings must contain Unicode scalar values; a lone surrogate makes the
event malformed. Before width calculation the renderer replaces each
dangerous scalar with literal ASCII `\u{XXXX}` using uppercase four-digit hex:

- U+0000–U+001F;
- U+007F–U+009F;
- U+061C, U+200E, U+200F;
- U+2028, U+2029, U+202A–U+202E;
- U+2066–U+2069.

Rendering order is exact:

1. validate schema and Unicode;
2. derive presentation fields;
3. substitute or scale;
4. escape dangerous scalars in field values;
5. segment the unstyled replacement into Unicode grapheme clusters, treating
   each escaped control representation as one indivisible display atom;
6. compute terminal columns with `Bun.stringWidth`, then pad or truncate to
   the requested exact width without splitting an atom;
7. add ANSI outside the picture result.

Combining marks, ZWJ emoji, wide glyphs, escaped controls, and mixed-width
Unicode follow this order. If atom widths leave an unfillable content column,
deterministic spaces make the bounded replacement exactly `S` columns. ANSI
is empty for non-TTY stdout or `NO_COLOR`.

The default is itself a legal picture:

```text
{local_time} {local_timezone} {duration_seconds:@s} │ {status} {mime_abbrev:<4} {response_bytes:@B} │ {method} {path} │ {mark}
```

Pretty mode ignores hello and heartbeat after validation, reports gaps and
protocol failures on stderr, and treats closed or EOF as a reconnect
condition.

### Rip acceptance and removal gate

The Rip integration suite pins:

- strict cursor and generation behavior, stale callbacks, reconnect backoff,
  exact 100ms doubling/full-jitter schedule through the 5s cap, injectable
  randomness, abort at every sleep boundary, valid-hello-only reset, browse
  re-registration, and process-lease flag rejection;
- every framing failure, exact raw complete-line preservation, incomplete EOF,
  stdout purity, stderr-before-raw gap ordering, failure at each diagnostic and
  raw write boundary, no reconnect after partial output, EPIPE, and high-volume
  worker output;
- the complete descriptor matrix;
- direct and wrapper first/second SIGINT and SIGTERM, exit statuses, process
  tree termination, and cleanup;
- every grammar production and error offset, exact-width alignment and
  overflow, combined scale/width formatting,
  shared `identifierRuns`/`isIdentifierName` lexing with no local identifier
  regex, whitelist rejection, arbitrary safe units, one-space and zero-width
  units, all field types, derived fields, and default-picture legality;
- the exact v3 scale oracle and Unicode validation, grapheme segmentation,
  control-escape atomicity, truncation, padding, and width order.

Removal of Rip's worker `logger` middleware, export, and tests requires this
complete Rip foreground integration suite plus Janus response-class
certification. `packages/app/launch.rip` is outside this capability and stays
unchanged.
