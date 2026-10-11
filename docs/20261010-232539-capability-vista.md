# Capability 11: vista

Status: **Contract**, rulings by the owner on 2026-10-11. Built to OpenVista's
[RPC-HTTPS contract](../../openvista/docs/RPC-HTTPS.md) (§1 client and
Janus, §2 Janus and em, §3 the reply rule), with the limits Em and the
owner settled on 2026-10-10, Em's review against the M side, and an
adversarial review (below, with what it changed).

`vista` holds one stream per client session to a Unix socket that a service
manager answers with one process per connection. Each HTTPS request
carries one message to that stream, and its one reply back. The client
speaks plain HTTPS (`POST`, HTTP/1.1 or 2); the backend speaks a strictly
request-and-reply byte protocol that keeps per-connection state. Janus
reads a request only to check that it is one well-formed message and a
reply only to find its end, plus the one sign-on fact named below.

The protocol is XWB, VistA's RPC Broker; the capability is named for
VistA because nothing else speaks it. OpenVista's clients (CPRS through
its workstation relay, and Rip's `rip/vista` package in a browser) reach
VistA on em, each session one em process running `XWBP^%ZISTCPS`.

```
 client ── HTTPS POST /rpc ──▶ janus ── Unix socket, one stream per session ──▶ em: XWBP^%ZISTCPS
        ◀── 200, the reply ──        PROXY line once, then each request body     (one process per stream)
```

## Scope and ownership

- Capability order: **11**, after webtransport.
- Surface: routes in exact-host site blocks; no global block.
- Cascade: **no**. A route belongs to the site that declares it; `vista` in
  the global `janus` block is a parse error.
- Default: **off**. Nothing exists until a site declares a route.
- Exposure: no new listener. Routes are served by the site's HTTPS servers
  and obey the exposure mode with them.
- Dependencies: none beyond the standard library.

**Trust boundary.**

- The backend authenticates the user (VistA's sign-on). Janus admits a
  request by host, path, method, content type, origin, size, and session
  token; caps sessions so that clients who never sign on cannot pile up
  backend processes; and hands the backend the client's true address in
  the PROXY line. The target socket is trusted as a registered upstream
  is; its mode admits only Janus's user.
- **Every app served on a vista host shares its origin** and can drive the
  sessions of a browser that holds the cookie (a page's script can send
  `X-OV: 1` to its own origin). Serve a route on a host that carries only
  the vista route and the web client that uses it, and nothing untrusted.
- **TLS ends at Janus.** A request that arrives without TLS (an HTTP site,
  or an L7 balancer that terminated TLS in front) is refused. A balancer
  in front must pass TCP through (L4, optionally with the PROXY protocol
  to Caddy's listener) and be named in `trusted_proxies`.
- A route reachable from untrusted networks can have sign-on denied to
  everyone by a client who fills the unsigned caps; the caps bound the
  cost to the backend, not the denial. Serve a route on an internal or
  allow-listed network where that matters.
- The backend keeps a reply's framing honest: OpenVista's edit to XWBRW
  checks a result before sending any byte, and answers data holding `\x04`
  (or a 4-character type-6 record, whose length byte is `\x04`) with an RPC
  error and no data (RPC-HTTPS §5). Janus's leftover-bytes rule (below)
  stays as the backstop.

## Site Caddyfile grammar

```caddyfile
vista.example.org {
	janus {
		vista /rpc unix//run/openvista/rpc.sock {
			max_sessions 256           # sessions on this route, default 256, range 1–65535
			max_unsigned 32            # sessions not yet signed on, default 32, range 1–max_sessions
			per_client 64              # sessions from one client key, default 64, range 1–max_sessions
			per_client_unsigned 8      # not yet signed on, from one client key, default 8, range 1–max_unsigned
			per_client_exempt 10.20.0.0/24   # client keys the per-client caps skip (terminal servers); default none
			signon_timeout 180s        # end a session not signed on by then, default 180s, range 10s–10m
			rpc_timeout 10m            # one request, from leaving the queue to its reply's end, default 10m, range 10s–1h
			queue 8                    # requests waiting behind the running one, default 8, range 1–64
			max_request 1MiB           # request body, default 1 MiB, range 1 KiB–16 MiB
			max_reply 64MiB            # reply, default 64 MiB, range 1 MiB–1 GiB
			origin same                # browsers: default same; or one or more origins
			auth exempt                # required when a site auth gate covers the path; see Auth gates
		}
	}
}
```

`vista <path> <target> [{ … }]` declares one route. The path is the
endpoint clients post to (`/rpc` in RPC-HTTPS); it is absolute,
exact, unique within the site, and canonical (no empty, `.`, or `..`
segment, no trailing `/`, no percent escape). The target is a Caddy
network address with network `unix` (`unix//run/openvista/rpc.sock`), an
absolute, clean path, no port. Each option appears at most once and an
empty block takes every default.

A **client key** is the client address (below) as a /32 for IPv4 and a
/64 for IPv6, so one host rotating addresses inside its prefix is one key.
`per_client_exempt` takes CIDRs; a key inside one skips `per_client` and
`per_client_unsigned` (never the route caps). A terminal server is the use:
dozens of CPRS users behind one address (RPC-HTTPS §6).

`origin` reuses the hub's token grammar, refusing `any`. `same` means the
request's scheme, host, and port: `https://` plus the request Host. Named
entries are full origins (`https://web.example.org`). `Origin: null` is
never allowed.

`auth exempt` is the route's stance under a covering site gate (Auth
gates). It is legal only when such a gate exists.

## Requests and responses

**`POST <path>`**, `Content-Type: application/x-xwb`. The body is exactly
one XWB message (RPC-HTTPS §3). The response, `200` with
`Content-Type: application/x-xwb` and `Cache-Control: no-store`, is the
backend's reply, byte for byte. A request body is read whole (within
`max_request`, and within 30 s) and walked before it joins a queue;
nothing is streamed to the backend.

### The token

- 32 bytes from `crypto/rand`, sent as 43 base64url characters without
  padding. Janus keeps only HMAC-SHA-256 of it under a per-process key
  (the auth store's pattern), bound to its route: a token presented on
  another route is unknown.
- Janus sends it only in `Set-Cookie: __Host-ov=TOKEN; Secure; HttpOnly;
  SameSite=Strict; Path=/`, on two replies: the `TCPConnect` reply that
  starts the session, and the reply that signs it on (**rotation**: a new
  token, the old one tombstoned as `rotated`).
- A client presents it as `Cookie: __Host-ov=TOKEN` (a browser) or
  `Authorization: Bearer TOKEN` (the CPRS relay and other non-browser
  clients; the scheme is matched case-insensitively). Caddy's access log
  redacts both headers by default. Janus reads the token nowhere else.
- 400: two `__Host-ov` cookies, a cookie and a Bearer token together, an
  `Authorization` header of any other scheme, or a token that is not 43
  base64url characters.
- Every response to a request whose token came as a cookie and that ends
  or refuses its session (401, `#BYE#`, a session-ending 502 or 504)
  clears it: `Set-Cookie: __Host-ov=; Secure; HttpOnly; SameSite=Strict;
  Path=/; Max-Age=0`.
- **On the rest of the site**, `__Host-ov` is stripped from every request
  forwarded anywhere else (an upstream, the hub bridge, files), and from
  every `Set-Cookie` in every non-vista response, as the auth wall does for
  its own cookie. Only the vista handler ever sets it.

### Sessions

- **`TCPConnect` always starts a new session**, whatever token it carries.
  If the token names a live session on this route, that session ends
  (`replaced`). Admission (rows 9–10) applies, then Janus dials the
  target, writes the PROXY line and the body, and reads the reply:
  - `accept` (`\0\0accept\x04`): the session starts; the reply carries
    the token.
  - `reject` (`\0\0reject\x04`: sign-ons inhibited or the job limit hit;
    em then ends the stream): 200 with that reply, no token, no cookie;
    the stream closes and the slot is released.
  - anything else: 502, the stream closes, the slot is released, one warn
    line.
- **Later requests** carry the token. A session runs one request at a
  time; a request that arrives while one runs waits in the session's queue
  (FIFO, depth `queue`); one more is 503.
- **A waiting request** whose client goes away leaves the queue and is
  never sent. A waiting request that `rpc_timeout` overtakes in the queue
  is 504 and was never sent; the session continues.
- **A running request** is written whole once it leaves the queue,
  whatever its client does; a write that fails or does not finish within
  30 s ends the session (`closed`). If the client goes away during the
  reply, the reply is still read to its end and discarded (`draining`), so
  the stream stays aligned; `rpc_timeout` and `max_reply` still bound it,
  and sign-on detection still runs on it.
- **The response** goes out as the reply is read: headers with the first
  reply byte, then the body, flushed per read. For `XUS AV CODE` and `XUS
  CVC` alone, the headers wait until the reply's first data line is
  judged (Sign-on), at most the two short strings and 64 bytes of data,
  since a rotated token must ride in those headers. Each write to the client
  has a 30 s deadline; a client that stops reading has its response
  aborted and the reply drains.

### Reading a reply (RPC-HTTPS §3)

A short string, a short string, then data to `\x04`, then one more `\x04`
when the data's first byte is `\x18`. Janus reads by this rule and never
by waiting for the stream to pause.

When a reply ends, any byte already buffered from the stream ends the
session (`protocol`), checked without waiting: data that held `\x04`
ended its reply early, and the rest must never become the next request's
reply. Bytes that arrive while no request runs also end the session
(`protocol`); em sends nothing unsolicited (verified by Em).

### Ending

| Cause (`OV-Session-Ended`) | When |
| --- | --- |
| `bye` | the request was the `#BYE#` command and its reply (`\0\0#BYE#\x04`) completed; Janus closes the stream. Requests queued behind it get 401 `bye` |
| `closed` | the stream closed or errored (the backend's idle timeout, its process ended, a write failed) |
| `signon-timeout` | not signed on within `signon_timeout`; when it falls during a request, the session ends as that request's reply ends |
| `rpc-timeout` | a running request passed `rpc_timeout` |
| `reply-too-large` | a reply passed `max_reply` |
| `protocol` | bytes left over at a reply's end, or bytes while idle |
| `replaced` | a `TCPConnect` presented this session's token |
| `rotated` | the token was replaced at sign-on (the session continues under the new token) |
| `route-removed` | a committed reload removed the route; a running request is aborted |
| `unknown` | Janus never minted this token on this route, or its tombstone has gone (a restart forgets everything) |

One reader goroutine per session owns every read of the stream, so an
idle stream's close is noticed at once and the session's entry dropped
then. An ended or rotated token is a tombstone for 10 minutes (at most
`max_sessions` × 4 per route, oldest dropped first), so its next request
gets `401` with `OV-Session-Ended` naming the cause.

## Sign-on

The one fact Janus reads from the protocol. A session is **signed on** by
either:

1. **`XUS AV CODE`.** The request holds exactly one chunk 2, whose name
   is byte-exact `XUS AV CODE`. The reply begins `\0\0` (no security or
   error message), its data does not begin with `\x18`, and its first line
   matches `^[1-9][0-9]{0,14}\r\n`: VistA's DUZ, above 0. The same RPC
   carries access and verify codes, handles, and CCOW sign-ons.
2. **`XUS CVC` after an expired verify code.** When an `XUS AV CODE`
   reply's first line is `0\r\n` and its third line is `1\r\n` (Kernel's
   VALIDAV: the verify code has expired), the session is marked
   *changing*, and its sign-on deadline restarts. A later `XUS CVC` reply
   that begins `\0\0` with first line exactly `0\r\n` signs on a session
   marked *changing*; on any other session it signs nothing on. (`XUS CVC`
   returns an array, so its first line always ends `\r\n`; verified by Em.)

Janus buffers at most the first 64 bytes of such a reply's data to judge
it, so a line split across reads judges the same. Signed-on is one-way.
Janus never logs, stores, or exposes the RPC name, the line, or a DUZ.

A session not signed on within `signon_timeout` (180 s by default:
VistA's own pre-sign-on limit, so a user typing at CPRS's sign-on screen
meets no limit stock VistA lacks) is ended and its stream closed, so em
exits. With `max_unsigned` and `per_client_unsigned` this bounds what
clients that never sign on can hold.

## The stream

On a new session Janus dials the target (3 s limit) and writes, first and
once, a PROXY protocol version 1 line:

```
PROXY TCP4|TCP6 <client> <server> <client-port> <server-port>\r\n
```

- `<server>` and `<server-port>`: the HTTPS connection's local address.
- `<client>` and `<client-port>`: the TCP peer of the HTTPS connection;
  when Caddy's `trusted_proxies` trusts that peer, Caddy's client IP
  (`client_ip`) and port 0 (em reads only the address; verified by Em).
  Never a header from an untrusted peer.
- Zone identifiers are removed. When the two addresses differ in family,
  the IPv4 one is written IPv4-mapped and the line is `TCP6`.
- The backend makes the client the session's address (Kernel's sign-on
  log, failed-sign-on lockout, and handles key on it); Janus's client keys
  derive from the same address.

Then the `TCPConnect` body and every later body, unchanged. Requests are
walked before they are queued, so what reaches the stream is always one
whole message.

## Admission

Every request whose path is a route's path byte for byte
(`r.URL.EscapedPath()` equals it) is judged in this order, first match
wins. Anything else (`/RPC`, `/rpc/`, `/rpc/.`, `/%72pc`) is not the route
and goes wherever the site sends it, behind whatever gate covers it.

| # | Condition | Answer |
| --- | --- | --- |
| 1 | Method is not `POST` | 405, `Allow: POST` |
| 2 | No TLS (`r.TLS == nil`) | 421 |
| 3 | TLS early data (`!r.TLS.HandshakeComplete`), or `Early-Data: 1` from a trusted proxy | 425 |
| 4 | `Origin` present and not allowed (including `null`); or `Origin` present, or the token a cookie, without `X-OV: 1` | 403 |
| 5 | `Content-Type` is not `application/x-xwb` | 415 |
| 6 | The body passes `max_request` or 30 s, or is not exactly one message by the walker (below) | 400; the body is read only to the limit |
| 7 | The token malformed or doubled (The token) | 400 |
| 8 | A `TCPConnect`: a route cap, or this client key's cap, is full | 503, `Retry-After: 5`, the cap named in the log |
| 9 | A `TCPConnect`: the target cannot be dialed | 502; the slot is released |
| 10 | A `TCPConnect` | a new session (Sessions) |
| 11 | No token | 401 `unknown` |
| 12 | A token naming no live session | 401 with its tombstone's cause, or `unknown` |
| 13 | The session's queue is full | 503, `Retry-After: 1` |
| 14 | Otherwise | the request joins the queue, then runs |

Row 4 is the browser rule. A browser sends `Origin` on every `POST`, and
the cookie on its own; `X-OV: 1` (and `application/x-xwb`) are what no
page elsewhere can attach without a CORS preflight, which the route never
answers. A non-browser client presenting `Authorization: Bearer` sends no
`Origin` and needs no `X-OV`. Cap reservations (row 8) are atomic.

**The walker** (row 6), as VistA's parser walks a message, never searching
for its end:

- `[XWB]`, then four digits: version `1`; type `1` (an RPC) or `0` (a
  command); width `0`–`9` (`0` means 3); and `0` (a message that wants a
  reply).
- Chunks in ascending order, each at most once: `1` (two short strings),
  `2` (two short strings: version and name), `3` (nothing), `4` (one
  short string, the command), `5` (the parameters, RPC-HTTPS §3, to the
  end byte). Type `1` requires chunk 2 and has no chunk 4; type `0`
  requires chunk 4 and has no chunk 2.
- A command is `TCPConnect` or `#BYE#`; any other gets no reply from the
  broker and is refused.
- Every declared length is checked against the bytes remaining before it
  is taken, and never allocated from.
- The message ends with exactly one `\x04`, and nothing follows it.

A bare `#BYE#` without the `[XWB]` header is refused: the CPRS relay sends
the `[XWB]` form.

Running a request:

| Outcome | Answer |
| --- | --- |
| The reply ends by the framing rule | 200 and the reply |
| `rpc_timeout` passes before the first reply byte | 504; the session ends |
| The stream closes, or a write fails, before the first reply byte | 502; the session ends |
| After the first reply byte: `rpc_timeout`, `max_reply`, the stream closing, or `route-removed` | the response is aborted (HTTP/2 `RST_STREAM`; HTTP/1.1 connection closed), since its 200 is out; the session ends |

Janus never retries a request: an RPC may have changed data.

## Auth gates

VistA's sign-on is the authentication, and the CPRS relay cannot sign on
to Janus. Where a site `auth` gate covers a route's path:

- **`auth exempt`** in the route serves the route outside the gate: the
  vista handler runs before the wall for exactly the route's path (Admission:
  byte-for-byte), and every other path stays behind it.
- **A gate whose prefix is exactly the route's path** (`gate /rpc { … }`)
  puts the route behind the wall: a web-only site behind Janus's login.
- **Neither** is a Start error: "vista /rpc on vista.example.org lies under
  gate /; add `auth exempt` to the route, or `gate /rpc { … }`". Janus
  never decides it silently.

Identity headers (`Remote-User`, `Rip-Site`, every spelling) are stripped
on every Janus site before admission; `vista` injects none.

## Lifecycle across reloads

Sessions live in pooled process state (`janusState.vista`), so a reload
keeps them. Routes follow stage and commit (`stageReconciliation`,
`commitReconciliation`): nothing staged reaches requests until it commits.

- **Identical reload.** Nothing is touched.
- **Limits change.** New limits apply to new admissions and requests at
  commit; a running request keeps its timeout; sessions over a lowered cap
  are not ended.
- **Target or protocol changes.** Existing sessions keep their streams;
  new sessions use the new target.
- **A route removed.** At commit its sessions end (`route-removed`), a
  running request is aborted, the streams close, and em logs each user
  out. One warn line per route names the count.
- **Aborted reload.** Nothing staged went live.
- **Restart.** Every stream closes with the process, every em exits, every
  token is unknown.

## Hard errors

Parse time:

- `vista` in the global `janus` block.
- A route without a path and target; a path not absolute, not canonical,
  or repeated within the site.
- A target whose network is not `unix`, whose path is not absolute and
  clean, or that carries a port.
- An unknown or repeated option; a value outside its range; a `per_client*` above its route cap; `max_unsigned`
  above `max_sessions`; a `per_client_exempt` entry that is not a CIDR.
- `origin any`, an empty `origin`, `null`, or an entry that is not an
  `https://` origin or `same`.

Start time:

- A route on a wildcard or catch-all site, or on a site whose listeners
  include one without TLS or a Unix socket.
- A route whose path is the site's hub path, a webtransport route's path,
  `/ping`, an auth gate's login door, or under the browse asset prefix.
- A route under a covering gate with neither `auth exempt` nor an exact
  gate; `auth exempt` with no covering gate.
- A route on a server with `log_credentials` (its tokens would be logged).

The target is not dialed at Start; a backend that is down is a 502 per
`TCPConnect`, not a refused configuration.

## Control and observability

`GET /1.0/vista` on every control listener answers `{"routes":{}}` with no
routes, otherwise:

```json
{
  "routes": {
    "vista.example.org/rpc": {
      "target": "unix//run/openvista/rpc.sock",
      "sessions": 3, "unsigned": 1, "running": 1, "queued": 0, "draining": 0,
      "oldest_running_ms": 840,
      "max_sessions": 256, "max_unsigned": 32,
      "started": 41, "signed_on": 37, "rejected": 0,
      "requests": 18402, "bytes_in": 2209811, "bytes_out": 90112044,
      "client_gone": 2, "aborted": 0, "queue_timeout": 0,
      "ended": {"bye": 30, "closed": 6, "signon_timeout": 1, "rpc_timeout": 0, "reply_too_large": 0, "protocol": 0, "replaced": 0, "route_removed": 0},
      "refused": {"method": 0, "no_tls": 0, "early_data": 0, "origin": 2, "content_type": 0, "message": 1, "token": 0, "unknown": 4,
                  "cap_route": 0, "cap_unsigned": 0, "cap_client": 0, "cap_client_unsigned": 0, "dial": 0, "queue": 0}
    }
  }
}
```

Counters are monotonic for the process and survive reloads. No token,
session id, client address, or user is on this surface.

Logs never carry a body, token, RPC name, or reply line:

- Each session's start and end at info: route, a session id (16 random hex
  characters, unrelated to the token and unchanged by rotation), the
  client address, the end's cause, duration, requests, and bytes each way.
- Each request at debug: route, session id, duration, outcome, bytes.
- Refusals: rows 1–9 and 13 at warn, rate-limited to one line per route and
  row per 10 s with a count of those suppressed; rows 11–12 (routine after
  an idle end) at debug.

The access log (capability 9) records `/rpc` requests like any other, with
`Cookie`, `Authorization`, and `Set-Cookie` reading `REDACTED`. Janus adds
the session id to each `/rpc` entry's extra fields, so an audit can join a
request to its session's start and end lines. An aborted response logs as
status 200 with `vista_aborted: true` in those fields. A route has no
registry owner, so `/rpc` requests never appear in any app's live NDJSON
stream.

## Non-goals (v1)

- The broker's other protocols (M2M, the callback broker `{XWB}`); TCP targets; a target per request or registered
  hot through `/1.0`.
- Retries, replay, resuming a session on another stream.
- Push to the client: a web client uses the hub (RPC-HTTPS §1).
- Session listing, inspection, or revocation over `/1.0`.
- Mutual TLS for relays.
- HTTP/3: the seed keeps it off; where an operator enables it, rows 2–3
  hold.
- BSE "visitor" sign-on (`XUS SIGNON SETUP` with a remote token): such a
  session meets `signon_timeout`. OpenVista does not use it.
- A per-client limit on the rate of `TCPConnect`s beyond the caps.

## Decisions

Settled with Em on 2026-10-10, for the owner's review:

| # | Decision | Ruling |
| --- | --- | --- |
| 1 | Name | `vista` (the owner, 2026-10-11; first drafted as `rpc`), with no `protocol` option: XWB is VistA's only framing, and a one-value option is the decorative toggle rule 3 forbids |
| 2 | Sign-on | Strict `XUS AV CODE` DUZ, plus `XUS CVC` after an expired code; `signon_timeout` 180 s |
| 3 | PROXY client | TCP peer, or Caddy's client IP when `trusted_proxies` trusts the peer; port 0 then |
| 4 | Limits | `rpc_timeout` 10 m from leaving the queue, `queue` 8, `max_request` 1 MiB, `max_reply` 64 MiB streamed |
| 5 | Token transport | `Set-Cookie` only; requests by `Cookie` or `Authorization: Bearer`, both redacted |
| 6 | Idle | No Janus timer; a stream's close is noticed at once |
| 7 | `TCPConnect` `reject` | 200 with the reply, no token, slot released |

Ruled by the owner on 2026-10-11, each as recommended:

| # | Decision | Ruling |
| --- | --- | --- |
| 8 | Token rotation at sign-on (closes session fixation; RPC-HTTPS §1 and the relay take a cookie from the signing-on reply) | Yes |
| 9 | A covering gate requires `auth exempt` or an exact gate (Start error otherwise) | Yes: rule 1, never silent |
| 10 | Per-client defaults 64 / 8, keyed /32 and /64, with `per_client_exempt` | As drafted |
| 11 | Refuse a route on a `log_credentials` server | Yes |
| 12 | A route removed by reload ends its sessions | Yes, loudly |
| 13 | TLS must end at Janus (no L7 balancer in front) | Yes |

**Amendments to RPC-HTTPS** this contract asked for, all made by Em (openvista `f316baa`):

1. The token travels only in `Set-Cookie` (`Path=/`, required by
   `__Host-`), presented as `Cookie` or `Authorization: Bearer`.
2. `TCPConnect` `reject`: 200, no token.
3. `XUS CVC` after an expired verify code signs on; `signon_timeout` 180 s.
4. Rotation: a second `Set-Cookie` on the signing-on reply (decision 8);
   the relay switches its Bearer token to it.
5. §1's 502 and 504 apply before a reply's first byte; after it the
   response is aborted.
6. §1's status table gains 405, 415, 421, 425, and 400 for a malformed
   token or an unknown command.
7. §5: XWBRW refuses `\x04` in reply data before sending any byte; an M
   error's reply is exempt.
8. §3: chunk 4 is followed by chunk 5 (`TCPConnect`'s address, port, and
   name) or by the end byte.

## Adversarial review

An independent review (2026-10-10) attacked the first draft. Its findings,
by severity, and what this revision did:

| Finding | Revision |
| --- | --- |
| Critical: a stale cookie after any end but `bye` 401s forever; a `TCPConnect` with a live token was undefined | `TCPConnect` always starts a new session (`replaced`); every session-ending answer to a cookie clears it |
| Critical: session fixation, by a planted `__Host-ov` from a co-hosted app or script | Rotation at sign-on; `Set-Cookie: __Host-ov` stripped from every non-vista response |
| High: the gate exemption could match spellings the route does not (`/RPC`, `/rpc/`) | The route is byte-exact on the escaped path; everything else is gated as usual |
| High: a partial write cancelled with its client would desync the stream | Bodies are read and walked whole before queueing and written whole regardless of the client |
| High: `\x04` in data delivers a reply to the wrong request | Leftover bytes at a reply's end end the session; XWBRW is asked to refuse such data |
| High: the walker accepted any end byte, reordered or repeated chunks, and allocated declared lengths | The walker's rules above |
| High: sign-on detection could be fooled (`00`, `1E3`, security messages, repeated chunk 2) | The strict rule above |
| High: unsigned caps as a denial lever; per-client keys wrong for IPv6 and terminal servers | /64 keys, higher defaults, `per_client_exempt`, and the denial stated in Trust boundary |
| High: co-hosted apps share the origin | Stated in Trust boundary |
| Medium: `#BYE#` detected from reply bytes alone | `bye` only when the request was the command |
| Medium: timeout gaps (queue time, body read, slow client, sign-on mid-request, route removal) | Each specified |
| Medium: PROXY family mismatch, zones, Unix listeners, L7 balancers | Mapped addresses, zones removed, Unix and non-TLS listeners refused, TLS at Janus |
| Medium: a process-wide cap named but never defined | Removed; caps are per route |
| Medium: draft and RPC-HTTPS disagreed | The amendments list |
| Medium: an info line silently overriding a gate violates rule 1 | `auth exempt` or an exact gate, else a Start error |
| Medium: thin observability | Gauges, counters, rate-limited warns, the session id in the access log |
| Low: no-TLS answer, `Early-Data`, `Origin: null`, origin scope; token binding and format; missing hard errors | Each adopted |

Not adopted: a 300 s `signon_timeout` (Em's 180 s matches VistA's own
pre-sign-on limit, the evidence for the default).

## Housekeeping this capability lands with

AGENTS.md's capability table gains row 11 and its `./test.sh` order line
gains `vista`; `docs/README.md` gains the index row; the README's capability
list and HANDOFF's table gain the row; `Caddyfile` gains a route on
`vista.janus.localhost` for the acceptance suite; `Caddyfile.example` gains a
commented example; `Caddyfile.minimal` is unchanged.

## Acceptance

Go tests (`go test ./...`) use a **fake em**: a helper that accepts on a
Unix socket, requires the PROXY line first, walks each message, answers
with a scripted reply, records what it received, and can split a reply
across writes, stall, close mid-reply, or send unsolicited bytes. Its
fixtures are RPC-HTTPS §3's measured replies, byte for byte (`accept`,
`reject`, `XWB IM HERE`, `XWB GET BROKER INFO`, `XUS SIGNON SETUP`, `XUS
AV CODE` with DUZ `100`, with `0`, and with the expired flag, `XUS CVC`
success, a security refusal, an M error with its second `\x04`, `#BYE#`).

- Parse: every legal form; every parse-time hard error with its message.
- Start: every Start-time hard error, including the gate stance.
- The walker: RPC-HTTPS §3's examples; a value holding `\x04`; list
  continuation; 9-digit widths; every refused form above; a declared
  length past the body (no allocation). A fuzz target asserts that an
  accepted body walks to exactly its last byte.
- The reply reader: each fixture; an M error's second `\x04`; replies split
  at every byte; at and over `max_reply`; leftover bytes at a reply's end.
- Admission: every row, in order; the route's byte-exact path match.
- The token: Set-Cookie only on `TCPConnect` and sign-on; Cookie and Bearer;
  every 400 form; route binding; clearing on every session-ending answer;
  `__Host-ov` stripped from forwarded requests and non-vista responses.
- Sessions: FIFO under concurrent requests on one HTTP/2 connection and
  across connections; the queue cap; a client gone while queued (never
  sent), while writing (written whole), and during the reply (drained, the
  next request aligned); a queued request overtaken by `rpc_timeout`;
  `#BYE#` by command only; idle close noticed with no request; unsolicited
  bytes; `replaced`; `reject`; every `OV-Session-Ended` cause; tombstones.
- Sign-on: DUZ > 0; DUZ 0; every fooling form above; split lines; the
  expired flag, then `XUS CVC`; `XUS CVC` with no expired flag; deadline
  restart; `signon_timeout` mid-request; the unsigned caps; /64 keys;
  `per_client_exempt`; rotation.
- PROXY line: TCP4, TCP6, mixed families, zones; the TCP peer; Caddy's
  client IP behind a trusted proxy; an untrusted `X-Forwarded-For` ignored.
- Timeouts: before and after the first byte; a slow-reading client.
- Reload: limits change; target change keeps sessions; removal ends them.
- No token, body, RPC name, or reply line in any log line: an observer
  test over every path above.

`./test.sh` group `vista` (after webtransport), with the testkit's fake em:

- `TCPConnect` → 200, `accept`, a `__Host-ov` cookie and no other token
  header; `XWB IM HERE` with the cookie and `X-OV: 1` → 200; with Bearer
  and no `X-OV` → 200.
- A scripted sign-on rotates the cookie; the old token → 401 `rotated`.
- An M-error reply arrives whole with its second `\x04`.
- A cross-origin `POST`, and a cookie without `X-OV`, → 403.
- A body not `[XWB]`, and one with a fourth digit `1`, → 400 at once.
- No token → 401; after `#BYE#` → 401 `bye` and a cleared cookie.
- The fake em closing an idle session → 401 `closed`.
- The unsigned cap → 503; `signon_timeout` set to 10 s frees it.
- `/RPC` under the suite's `gate /` gets the gate's answer.
- The access log holds no token: a grep for each minted token finds none.

End to end against real em (Em provides the environment): the CPRS relay
signs on through Janus to VistA, as `test/relay`'s mini-Janus does today.

## Measurement

Per AGENTS.md rule 8, with raw provenance as `docs/*-bench-raw-vista.txt`:
`XWB IM HERE` round trips per second and their latency on one session and
on 64 concurrent sessions against the fake em; the admission path's cost;
memory per idle session; and a 64 MiB reply's throughput and Janus's peak
memory while streaming it.
