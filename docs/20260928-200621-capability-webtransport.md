# Capability 10: webtransport

`webtransport` terminates WebTransport (HTTP/3 over QUIC, RFC 9220 with
the WebTransport-over-HTTP/3 draft) on a UDP listener the edge owns and
relays each datagram, opaque and byte-exact, between a browser session and
one UDP address fixed in the Caddyfile. A web page cannot send UDP; a
WebAssembly client that speaks a UDP protocol reaches its server through
this relay, with its own end-to-end encryption intact. Janus never reads,
frames, retries, or reorders a datagram; it moves one WebTransport datagram
to one UDP datagram and back.

The first user is a remote-desktop host that speaks its own protocol on
`udp/127.0.0.1:41151`. Nothing here is specific to it.

## Scope and ownership

- Capability order: **10**, after access log.
- Surface: one process-wide UDP listener, configured in the global `janus`
  block; relay routes, configured in exact-host site blocks.
- Cascade: **no**. The listener is process-wide like `control` and `mdns`;
  a site-level `webtransport { }` block is a parse error. A route belongs to
  the site that declares it; the global block declares no routes.
- Default: **off**. Nothing binds until the global block is present.
- Trust boundary: admission is Host, path, Origin, the caps, and, on a
  site whose effective `auth` is on, a ticket minted for a signed-in user.
  No cookie reaches the relay (browsers send none with a WebTransport
  CONNECT); the ticket is how the auth gate's decision carries across to
  the second connection. Authentication of the application's own protocol
  stays end to end; the relay target is trusted the way a registered
  upstream is. Origin is a
  browser-side control against cross-site use, not authentication: a
  non-browser client sets any Origin it likes.
- Exposure: the relay's UDP sockets sit inside the exposure contract on the
  same terms as the TCP front door (below).
- Dependency: `github.com/quic-go/webtransport-go` v0.10.0, whose `go.mod`
  requires quic-go v0.59.0; minimal version selection keeps Caddy 2.11.4's
  v0.59.1. Later webtransport-go releases require quic-go v0.60 and up and
  wait for a Caddy that ships one. It brings `dunglas/httpsfv` with it.

## The relay model

```
browser ── QUIC/UDP :443 ──▶ janus relay ── connected UDP socket ──▶ udp/127.0.0.1:41151
        ◀────────────────────            ◀──────────────────────────
   WebTransport datagram  ⇄  one goroutine pair per session  ⇄  UDP datagram
```

One WebTransport session is one connected UDP socket toward the route's
target, opened before the session is accepted and closed the moment the
session ends. The socket's source port is fixed for the session's life and
never shared; a connected socket delivers only datagrams from the target
address (Linux and macOS both filter by the connected peer and surface an
ICMP unreachable as `ECONNREFUSED`), so a stray sender cannot inject into a
session. Datagram boundaries are preserved in both directions: one in, one
out, never coalesced, split, padded, duplicated, retried, or reordered by
the relay.

Streams are not relayed in v1. An incoming WebTransport stream is reset
with the `unsupported` stream code and counted. Datagrams are unreliable
by nature; the relay adds no reliability and removes none.

## Global Caddyfile grammar

```caddyfile
{
	janus {
		webtransport {
			max_sessions 256     # process-wide ceiling, default 256, range 1–65535
			max_datagram 1200    # bytes relayed per datagram either way, default 1200, range 64–1200
			idle_timeout 60s     # QUIC max idle timeout, default 60s, minimum 30s
		}
	}
}
```

Presence enables the listener; an empty block takes every default. Each
option appears at most once. The block takes no arguments.

There is no `listen` option in v1. The relay's UDP port is Caddy's HTTPS
port (`https_port`, 443 unless changed), and its addresses are the listen
hosts of the HTTP servers that serve on that port. A browser therefore
dials the site's own URL, `https://pup.local/lyte`, with no port, and under
the service edge the UDP sockets bind exactly where `default_bind
{$JANUS_BIND}` puts the TLS sockets: the exposure plan, with no second
source of truth.

The addresses come from each server's `listen` strings (as the mdns
collision check reads them, since Caddy fills a server's parsed addresses
only in its own Start): a `tcp4/` or `tcp6/` prefix becomes `udp4/` or
`udp6/`, a bare host keeps its family, `unix/` and `fd/` listeners are
skipped, a port range is refused, and a hostname in place of an IP literal
is refused (the relay binds addresses, never names).

## Site Caddyfile grammar

```caddyfile
pup.local {
	janus {
		webtransport /lyte udp/127.0.0.1:41151 {
			origin same          # default; or one or more hostnames
			max_sessions 4       # per route, default 4, range 1–65535
		}
		browse { root /srv/lyte/www }
	}
}
```

`webtransport <path> <target> [{ … }]` declares one route. The path is
absolute, exact (no prefix match), and unique within the site. The target
is `udp/<addrport>` parsed with `netip.ParseAddrPort` semantics: an IPv4
literal or a bracketed IPv6 literal (`udp/[::1]:41151`) with a nonzero
port. A hostname is refused because the target is a fixed operator
decision, not a resolution that can drift. An unspecified, multicast,
broadcast, or zone-scoped address is refused, and so is a target equal to
one of the relay's own listen addresses (a self-loop). A site may declare
several routes with distinct paths; two routes may name the same target.

The site's `janus` block must sit under exact host matchers only: the
certificate the relay presents comes from Caddy's cache, and an exact host
is what guarantees Caddy manages one. Wildcard and catch-all sites refuse
the directive.

`origin` reuses the hub's token grammar and check through a wrapper that
refuses `any` first. A relay toward a fixed UDP target that any web page
could drive is an open relay; `same` (the Origin's hostname equals the
request Host, compared lowercase and hostname-only, scheme and port
ignored as the hub does) is the default, and named hosts extend it. A
missing Origin is refused under every policy.

A route on a site whose effective `auth` is on (declared in the site or
cascaded from the global block) requires a ticket. The gate cannot see a
CONNECT, which carries no cookie, so the gated descriptor mints a ticket
for the signed-in user and the relay admits only a CONNECT that presents
it (below). Nothing is configured for this; it follows from the site being
gated. A route on an ungated site needs no ticket.

```caddyfile
lyte.trusthealth.com {
	tls { dns route53 }
	janus {
		auth {
			user steve <passhash>
			gate / { steve }
		}
		browse { root /srv/lyte/www }
		webtransport /lyte udp/127.0.0.1:41151
	}
}
```

One `gate /` covers the page, the descriptor at `/lyte`, and through the
ticket the relay session. On a public name with a publicly trusted
certificate the local-CA question below does not arise.

## Discovery (mDNS)

A relay a browser cannot resolve is useless, so with mdns enabled the
advertiser announces each route's host on the LAN the same way it announces
registered app hosts, and in lan mode that answer is A-only (the selected
IPv4), which matters for WebTransport: a browser's QUIC dial does not fall
back from a AAAA the edge never bound. The announcement is independent of
the mdns `apps` knob (a relay host is not an app host), deduped against a
registered app host of the same name (announced once), and never displaces
the configured front-door name. A single-label `<label>.local` host is
advertisable; a multi-label `.local` name is not (as for app hosts). This
is why a relay should use a Janus-owned name (e.g. `lyte.local`) rather
than a name another mDNS responder already owns (a machine's own
`<host>.local` published by Avahi/Bonjour): two responders for one name
force a rename. When the relay's host is served over a publicly resolvable
name instead, ordinary DNS carries it and mdns is not involved.

## Exposure

The relay's UDP sockets are front-door sockets and obey the mode. The
exposure contract ([exposure-modes](20260907-000352-exposure-modes.md)) is
amended by this capability where it states that the front door is TCP
only and that the pf anchor passes TCP alone; the README paragraph that
says HTTP/3 stays off because UDP listeners are outside the mode, and the
seed's comment above `protocols h1 h2`, change to say that the relay is
the one UDP listener the mode covers and that HTTP/3 stays off because it
would bind the relay's port.

- `PlanListeners` is unchanged; the relay binds UDP at the plan's HTTPS
  addresses, which it learns from the HTTP servers' listen hosts.
- The port is 443. `edgePorts`, the pf anchor, and the Windows rules are
  fixed at 80 and 443; a relay on another `https_port` would be outside
  every one of them. Under the service edge (a stored mode exists) the
  relay refuses to start when `https_port` is not 443, naming the port. A
  hand-run Caddyfile with no stored mode has no exposure contract to
  widen and may use any HTTPS port.
- `verifyExposure` accepts a UDP socket on 443 when its address is in the
  plan's HTTPS set (fewer is fine, as for TCP) and continues to reject UDP
  on port 80 and UDP on 443 at an address outside the plan. On macOS, where
  the TCP bind is wildcard and pf is the scope truth, TCP is not judged by
  address today and UDP is judged the same way: by port only, 443 allowed
  and 80 refused. UDP on other ports is not a front-door concern and stays
  ignored. The violation text names the relay so an operator reading it
  knows which capability opened the socket. The existing test row "UDP on
  a front-door port is a violation anywhere" is replaced by these rows,
  not weakened around.
- The pf anchor passes `proto { tcp udp }` on 443 from the mode's sources
  and blocks the rest; port 80 stays TCP only. The pf allow is stateless
  and a UDP source is spoofable; the QUIC handshake with source-address
  validation (below) is the gate, the anchor only narrows who can knock.
- The anchor is written by `janus mode` and `janus firewall` as root and
  never by the running edge. An edge upgraded under an anchor written
  before this capability would have UDP 443 open to any source by pf's
  default while the anchor still blocks only TCP. So on macOS, outside
  `wan`, the relay refuses to start unless `/etc/pf.anchors/janus`
  (world-readable) equals `PfAnchor(scope, onlink)` for the stored mode,
  and the error says to re-run `janus mode <scope>` (as root, `janus
  firewall`). `janus status` already reports the anchor as missing on a
  text mismatch.
- Windows gets a second inbound rule, UDP 443 under the same profiles as
  the TCP rule, with its own display name; the rule check and removal
  paths know both.
- `janus status` gains a protocol column in its listener rows and prints
  the UDP 443 rows when the adapted service config carries the global
  block.

In `wan` the local name families are refused (421) on the relay listener
exactly as on the TCP front door.

## Transport

The relay owns its QUIC transport rather than calling the library's
`Serve`, which would wrap the socket in a single-use transport with no
source-address validation:

- One `quic.Transport` per bound socket, with `VerifySourceAddress`
  returning true above 1000 new handshakes per second per socket (the
  rate Caddy's own HTTP/3 listener uses), so a spoofed 1200-byte Initial
  gets a Retry, never the up-to-threefold handshake reply toward a
  victim.
- `ListenEarly` with the pooled TLS config and one `quic.Config`:
  `EnableDatagrams` and `EnableStreamResetPartialDelivery` (both required
  by `ServeQUICConn`), `MaxIdleTimeout` = `idle_timeout`, `KeepAlivePeriod`
  = `idle_timeout / 4` (quic-go clamps it to half the idle timeout and
  chooses the keep-alive it sends), `Allow0RTT` false, the default initial
  packet size of 1280 bytes, path MTU discovery on.
- Each accepted connection goes to `webtransport.Server.ServeQUICConn`
  over an `http3.Server` whose handler is the relay's admission handler,
  prepared with `ConfigureHTTP3Server`, `IdleTimeout` left at zero (the
  QUIC idle timeout governs), and `CheckOrigin` set to a function that
  returns true, because Janus judges Origin itself before calling
  `Upgrade` so the 403 and its counter are Janus's.

## TLS and trust

Certificates come from Caddy. Each config generation provisions a
`caddytls.ConnectionPolicies{{ALPN: ["h3"]}}`, builds its TLS config from
the tls app, applies `http3.ConfigureTLSConfig`, and publishes it through
an atomic pointer that the pooled transport's fixed `tls.Config` consults
per handshake (`GetConfigForClient`; quic-go wraps that hook itself and
forces TLS 1.3). ACME, `tls internal`, loaded pairs, and on-demand
issuance under `permission janus` all work unchanged, because the site is
an exact host Caddy already manages. The `acme-tls/1` ALPN Caddy appends
is harmless on this listener.

**Browser trust over QUIC (decided by test).** Chrome verifies the server
certificate twice: for the page over TCP, where it exempts a locally-trusted
root from Certificate Transparency (the page loads with a lock), and for the
WebTransport QUIC handshake, where in practice it still requires CT — which a
local CA cannot provide. So plain CA trust is **not** enough for WebTransport
with an internal CA (proven 2026-09-29 in Chrome 152: the page loads, the
dial fails `QUIC_TLS_CERTIFICATE_UNKNOWN` / `CERTIFICATE_VERIFY_FAILED`). The
browser-proven path for a non-public cert is `serverCertificateHashes`, which
pins the leaf's SHA-256 and skips CA/CT entirely.

**Certificate hashes in the descriptor.** The relay's on-demand internal leaf
is ECDSA P-256 with a short validity, which already satisfies the
`serverCertificateHashes` constraints (P-256, validity ≤14 days). So the
descriptor publishes the current leaf's SHA-256 (standard base64 of the DER)
in `certificate_hashes` whenever the leaf qualifies, and a page dials with
`serverCertificateHashes` set from it. When the served leaf is long-lived (a
public, CT-compliant cert, e.g. via Route 53 DNS-01), `certificate_hashes` is
empty and the browser validates by CA as usual. The leaf rotates, so the
descriptor is `no-store` and the page GETs it before each dial; a dial that
races a rotation retries with the fresh hash. No separately-minted pinning
cert is needed — the edge's own short-lived leaf is the pinned cert.

**Descriptor.** `GET <path>` on the site over TCP (HTTP/1.1 or HTTP/2, served
by the site handler) answers `application/json`, `Cache-Control: no-store`:

```json
{"url":"https://pup.local/lyte","max_datagram":1200,"certificate_hashes":[{"algorithm":"sha-256","value":"<base64 DER SHA-256>"}]}
```

`certificate_hashes` carries the current pinnable leaf hash (empty only when
the served leaf is a long-lived public cert). `HEAD` is allowed; other methods
answer 405. On a gated site the descriptor sits behind the auth wall and its
`url` also carries a single-use ticket (below).

## Admission

The relay listener serves Janus's own HTTP/3 handler, not Caddy's routes:
no Caddy middleware, site `log`, auth wall, or access log sees these
requests. Every request on the listener is judged in this order, first
match wins:

| # | Condition | Answer |
| --- | --- | --- |
| 1 | Method is not `CONNECT` with `:protocol webtransport` | 405 (a GET over HTTP/3 is not the descriptor; that is served over TCP) |
| 2 | Host is not a site with routes | 404, logged with the host |
| 3 | The TLS server name the certificate was chosen for differs from Host | 421, logged (Caddy sites do not enforce this; a non-browser client can front one name with another's certificate) |
| 4 | `wan` mode and Host is a local family name | 421 |
| 5 | Path is not a route of that site | 404 |
| 6 | Origin missing or not allowed by the route's policy | 403, logged with Origin and route |
| 6a | The site is gated and the ticket is missing, malformed, expired, already used, or for another route | 403, logged with the reason and never the ticket; the path is logged without its query everywhere |
| 7 | Route or process session count at its cap | 503 with `Retry-After: 1`, logged. The slot is reserved atomically here, so two racing CONNECTs cannot both pass the cap; a later refusal releases it |
| 8 | Connected UDP socket toward the target cannot be created | 503 with `Retry-After: 5`, logged with the error |
| 9 | `Upgrade` fails: client SETTINGS lack datagram support, or do not arrive within the library's 5 s reordering timeout | 400 written by Janus; the socket and slot are released |
| 10 | Otherwise | `Upgrade` has written 200; the session begins |

The socket is created before `Upgrade` because `Upgrade` writes and
flushes the 200 the moment the client's SETTINGS arrive; there is no hook
between them. Every refusal is counted per route and logged at warn with
the reason, so a refused client is visible to the operator and never only
to itself.

## Datagram semantics

**Size.** `max_datagram` bounds the payload relayed in either direction,
default and ceiling 1200 bytes. Relay → browser, a 1200-byte payload fits
quic-go's first-flight budget before any path MTU discovery: the initial
packet size of 1280 bytes is the UDP payload, `SendDatagram` accepts up to
1243 bytes of frame at that size (1280 less the short header, a 20-byte
connection-id allowance, and the AEAD tag), and the frame for a 1200-byte
payload is 1204 bytes (type, two-byte length, the HTTP/3 quarter-stream-id,
payload), leaving a margin of at least 39 bytes. The ceiling is 1200 and
not higher because a frame between about 1240 and 1243 bytes passes
`SendDatagram` and can still be discarded silently by the packer when it
does not fit the packet being built. Browser → relay, the size a browser
sends is its own packetization, not the relay's to guarantee; the browser
proof records it.

**Browser → target.** Each received WebTransport datagram is written to
the session's UDP socket at once. A payload above `max_datagram` is
dropped and counted (`dropped_oversize_in`); the first drop per session is
logged. The relay keeps no queue of its own in this direction. Two queues
it cannot see exist below it: quic-go holds 128 received datagrams per
connection and http3 32 per stream, both dropping the oldest silently when
the reader lags. So the session's receive loop is one `ReceiveDatagram`
loop whose only blocking call is the UDP write, which does not wait, and
the contract does not claim that every loss on this path is counted.

**Target → browser.** One reader goroutine per session reads the UDP
socket into a bounded ring of 256 datagrams with a 50 ms age limit (the
bounds the first user asked for) and one sender goroutine calls
`SendDatagram`. When the ring is full or an entry is older than 50 ms, the
oldest is dropped and counted (`dropped_queue`). A `DatagramTooLargeError`
or a payload above `max_datagram` is dropped and counted
(`dropped_oversize_out`); the first per session is logged. `SendDatagram`
is called from the session's sender only, never from a shared goroutine,
because it blocks once quic-go's queue holds 32 frames.

**Latency.** Nothing batches, delays, or aggregates; arrival dispersion
reaches the target as it came. The UDP socket's receive buffer is raised
to 1 MiB, best effort: the first user's 6.25 MB/s ceiling over the ring's
50 ms window is about 320 KB, and 1 MiB covers a scheduling gap three
times that. The sustained rate the relay carries per session without ring
drops is a measurement result (below), not a claim made here.

**Target unreachable.** `ECONNREFUSED` or an ICMP unreachable on the
connected socket is counted (`upstream_refused`) and the session stays
open; the target may be restarting. The idle timeout bounds the wait.

**Idle and keep-alive.** The QUIC connection's max idle timeout is
`idle_timeout`, with quic-go's keep-alive at a quarter of it, so a quiet
but healthy session lives, and a dead peer closes after `idle_timeout`. A
relay-level idle rule stricter than QUIC's is a non-goal.

## Session lifecycle

A session ends when the browser closes it, the QUIC connection times out,
the route is removed by a committed reload, the process stops, or one of
the session's goroutines panics. On every end the UDP socket is closed
first, then the counters are folded into the route, then one info line is
logged with route, duration, datagrams and bytes each way, and drops, and
last the session is closed from its own goroutine (`CloseWithError`
blocks until the session context ends, so no shared goroutine calls it).

Session close codes, sent as the WebTransport close-session capsule:

| Code | Meaning |
| --- | --- |
| 0 | normal, relay-initiated close at shutdown or route removal |
| 1 | internal error: a panic in this session's goroutines, recovered and logged with the stack; other sessions are unaffected |
| 2 | UDP socket failed after admission (a read or write error that is not a refusal) |

Stream error codes, a separate namespace mapped onto HTTP/3 stream codes:

| Code | Meaning |
| --- | --- |
| 3 | `unsupported`: an incoming stream, reset and counted (`streams_reset`) |

## Lifecycle across reloads

The relay is pooled process state beside the mdns advertiser: `wtRelay`
in `janusState`, bound with a plain `net.ListenConfig{}` (no
`SO_REUSEPORT`, so a second binder fails loudly instead of splitting
packets), and torn down in `Destruct`.

The relay follows the stage-and-commit model the hub and auth tables use
(`stageReconciliation`, `commitReconciliation`), not mdns's apply-and-roll-
back: nothing a generation stages reaches the wire until it commits.

- **Start.** A generation with the global block stages its route table
  and TLS config and binds any address not yet bound. A bind failure is a
  hard Start error and the sockets bound for this generation alone are
  closed; nothing live changes.
- **Commit.** The first generation, with no active predecessor, commits at
  its own Start. A later generation commits in `commitReconciliation`,
  called from the old generation's `Stop`: the staged routes replace the
  live table atomically, sessions on routes no longer present are closed
  (code 0), sockets whose address left the plan are closed and their
  sessions die with them, and the TLS pointer swaps.
- **Identical reload.** An unchanged configuration is a no-op: no rebind,
  no session touched, no log line.
- **Aborted reload.** A generation retired while an older one survives
  (the identity check on the generation, as mdns does it) discards its
  staged table and closes the sockets bound for it alone. There is nothing
  to restore, because nothing of it went live; one warn line says so.
- **Disable.** A generation without the global block closes every session
  and socket at commit. A restart empties nothing that matters: the relay
  holds no registry.

**Collision.** Start refuses the capability when any listener on the
relay's port lists `h3` in its protocols, judged per listener
(`ListenProtocols`, falling back to the server's `Protocols`, whose Caddy
default includes `h3`), naming the server and the fix, `servers {
protocols h1 h2 }`. A bind that still fails (`EADDRINUSE`) is a hard Start
error naming the address.

## Hard errors

Parse time:

- `webtransport` in a site block with a `{ }` block and no path and target;
  a global block with arguments.
- A global block with an unknown option, a repeated option, a value
  outside its range, or `idle_timeout` under 30 s.
- A site route whose path is not absolute or is repeated within the site,
  or whose target is not `udp/<addrport>` with an IP literal, or names an
  unspecified, multicast, broadcast, or zone-scoped address.
- `origin any`, an empty `origin`, or a non-hostname token.

Start time (these need the HTTP app, which is not provisioned when the
janus app provisions, so they are Start errors like every other
HTTP-dependent gate in the module):

- A site route on a wildcard or catch-all site.
- A site route while the global block is absent: "webtransport routes need
  the process-wide listener; add `webtransport { }` to the global janus
  block".
- A listener on the relay's port with `h3` enabled.
- No HTTP listener on the relay's port at all; a listener whose host is a
  hostname or whose port is a range.
- A target equal to one of the relay's listen addresses.
- Under the service edge: `https_port` other than 443; on macOS outside
  `wan`, a pf anchor that does not match the stored mode.
- A UDP bind failure at any planned address.

## Control and observability

`GET /1.0/webtransport` (and `/1.0/webtransport/`) on every control
listener answers `{"enabled":false}` when the global block is absent,
otherwise:

```json
{
  "enabled": true,
  "listen": ["10.0.0.249:443", "127.0.0.1:443"],
  "sessions": 1,
  "max_sessions": 256,
  "max_datagram": 1200,
  "idle_timeout": "60s",
  "routes": {
    "pup.local/lyte": {
      "target": "udp/127.0.0.1:41151",
      "origin": ["same"],
      "sessions": 1,
      "max_sessions": 4,
      "accepted": 12,
      "refused_origin": 0,
      "refused_cap": 0,
      "refused_upstream": 0,
      "refused_settings": 0,
      "refused_ticket": 0,
      "datagrams_in": 40213,
      "datagrams_out": 812044,
      "bytes_in": 3117802,
      "bytes_out": 935474688,
      "dropped_queue": 0,
      "dropped_oversize_in": 0,
      "dropped_oversize_out": 0,
      "upstream_refused": 0,
      "streams_reset": 0,
      "panics": 0
    }
  },
  "refused_host": 0,
  "refused_path": 0,
  "refused_fronting": 0,
  "refused_method": 0,
  "retries_sent": 0
}
```

Counters are monotonic for the process and survive reloads with the pooled
state; a route removed and re-added starts over. No remote address is
published: the surface is counters, not a session directory.

The session open and close lines carry the ticket's user on a gated
site, so the operator sees who connected, which the relay cannot know
otherwise. Logs: one info line per session open and close, one warn per refusal with
its reason, one warn per session for the first oversize drop in each
direction, and the aborted-reload line. The access log (capability 9)
does not see relay sessions; this surface is the observability for them.

## Processing order

The site handler answers the descriptor `GET <path>` after the front-door
return and before `validatedRequestPath` and the cold-browse branch, keyed
on the handler's own route table and independent of registry resolution,
so a cold-only site with a route and no browse root serves its descriptor
and a static root never shadows it. The `wan` local-name refusal at the
top of the handler already precedes it. The relay listener's own admission
order is the table above.

## Non-goals (v1)

- Streams (bidirectional or unidirectional) beyond reset-and-count.
- A client-chosen target, a hostname target, or a hot-registered route
  through `/1.0`. Cold config fixes the target (rule 2).
- Per-session or per-route packet-rate limits, and per-remote-IP
  new-session rate limiting beyond QUIC's source-address validation.
  Relayed clients reach the target from the relay's address and share its
  handshake budget; the session cap is the v1 bound.
- Serving pages over the relay's HTTP/3 listener, or Alt-Svc advertising.
- A configurable listen address or port; `origin any`.
- Counting the losses inside quic-go's and http3's own receive queues.

## Decisions

Decided by the owner on 2026-09-28, each as recommended:

| # | Decision | Ruling |
| --- | --- | --- |
| 1 | Grammar | No `listen` option; port and addresses inherited from the HTTPS servers |
| 2 | Exposure | (a): UDP 443 joins the plan, the pf anchor, and the Windows rules |
| 3 | `https_port` other than 443 under the service edge | Refuse |
| 4 | macOS with a pf anchor written before this capability | Refuse to start until `janus mode` rewrites it |
| 5 | Oversize datagram | Drop and count, log once per session per direction |
| 6 | Target refuses while restarting | Keep the session; the idle timeout bounds it |
| 7 | `max_pps` | Non-goal |
| 8 | Routes on auth-gated sites | The gated descriptor mints a 60 s single-use ticket the CONNECT presents in its URL |
| 9 | Local-CA trust over QUIC | Decided by test: Chrome requires CT on the WebTransport QUIC handshake, so a local CA needs `serverCertificateHashes` (the descriptor pins the short-lived P-256 leaf); plain CA trust suffices only for a public, CT-compliant cert |

## Housekeeping this capability lands with

AGENTS.md's capability table gains row 10 and its `./test.sh` order line
gains `webtransport`; `docs/README.md` gains the index row; the README's
capability list, the exposure-modes contract, the seed comment, and the
HANDOFF snapshot change as named above; `Caddyfile`, `Caddyfile.example`,
and `Caddyfile.minimal` still validate. The mdns capability page notes that
relay-route hosts join the advertised set.

## Acceptance

Go tests (`go test ./...`), none of which touch the network beyond
loopback:

- Parse: every legal form of both blocks; every parse-time hard error
  above with its message.
- Start gates, against a fixture HTTP app: every Start-time hard error;
  h3 collision detected per listener through `ListenProtocols` and the
  server default; the address set derived from `listen` strings with the
  `tcp4/`, `tcp6/`, `unix/`, hostname, and port-range cases.
- Exposure (`cmd/janus`): `verifyExposure` accepts UDP on 443 at planned
  addresses in every mode on Linux, rejects it at an unplanned address and
  on port 80, judges by port only on darwin, and still rejects UDP
  elsewhere on the front door; `PfAnchor` passes UDP on 443 and not on 80;
  the Windows rules are two; the `https_port` and anchor-mismatch refusals.
- Relay, with `webtransport.Dialer` (whose `Dial` returns the HTTP
  response, so statuses and `Retry-After` are assertable) and a UDP echo
  fixture on `127.0.0.1:0`, the server's `quic.Config` with path MTU
  discovery disabled so the first-flight budget is what is measured:
  1152-byte and 1200-byte payloads relayed byte-exact relay → browser and
  browser → relay; boundaries preserved across 1000 mixed-size datagrams;
  an oversize payload dropped and counted in each direction; a missing
  Origin (the Go client sends none) and a foreign Origin refused 403 and
  counted; SNI/Host fronting refused 421; the route cap and process cap
  refused 503 with `Retry-After`, with two concurrent dials at the cap
  admitting exactly one; one connected socket per session with a stable
  source port, reported by the echo fixture, and closed at session end; a
  refused target keeps the session and counts; a panic injected through a
  package-level fault hook closes that session with code 1 and leaves
  another session relaying; an incoming stream is reset with code 3 and
  counted; the ring, tested as a unit with an injected clock, drops the
  oldest past 256 or 50 ms and counts, and end to end a full ring drops
  the oldest.
- Lifecycle, driving `configure`, `commit`, and the retirement path on
  the pooled object as the mdns tests do: identical reload leaves sessions
  and sockets untouched; a route removal closes its sessions at commit and
  not before; an aborted generation discards its staging and closes only
  its own sockets; disabling closes everything; `Destruct` closes the
  sockets.
- Control: `/1.0/webtransport` shape in both states.
- Descriptor: `GET` and `HEAD` answer the JSON with `no-store`; other
  methods 405; a cold-only site with no browse root serves it; on a gated
  site an unauthenticated GET gets the wall's answer and a signed-in GET
  gets a `url` with a ticket, while an ungated site's `url` has no query.
- Ticket, on a gated route: a CONNECT without one, with a forged one, an
  expired one, one for another route, and one presented twice is refused
  403 and counted (`refused_ticket`); a valid one admits and the session
  log line names the user; no log line contains the ticket or the query.

`./test.sh`, group `webtransport` after `access`: the root `Caddyfile`
gains `servers { protocols h1 h2 }`, the global block, and a route site;
`require_ports_free` also checks UDP 443; the testkit gains `udp-echo` and
`wt` subcommands (Go: dial with the suite's `tls internal` root as
`RootCAs`, an explicit `Origin`, N datagrams each way, a JSON result) and
the group asserts the echo, the descriptor, the origin refusal, and the
`/1.0/webtransport` counters. curl cannot drive WebTransport.

Browser proof (recorded 2026-09-29, Chrome 152 on macOS 27 → Janus on pup,
lan mode, internal CA): with the descriptor publishing the leaf hash and the
viewer dialing `serverCertificateHashes`, the session went Live end to end —
Noise handshake complete, the browser rendered the host's desktop, and audio
and input round-tripped. Relay counters for the route: a live session with
datagrams both ways (12.8 MB host→browser, ~4,900 browser→host), zero
oversize/queue drops, zero upstream refusals, zero panics.

The finding this milestone establishes: **Chrome enforces Certificate
Transparency on the WebTransport QUIC handshake even when the server's root
is a locally-trusted CA** — the page over TCP is CT-exempt for a local root
and loads with a lock, but the WebTransport dial fails
`QUIC_TLS_CERTIFICATE_UNKNOWN` / `CERTIFICATE_VERIFY_FAILED`. Pinning the leaf
hash with `serverCertificateHashes` exempts the dial from CA/CT, which is why
the internal-CA path works only through `certificate_hashes`. A publicly
trusted (CT-compliant) cert needs no hash. A Go/quic-go client does not
enforce CT and so does not reveal this — the proof must be a browser.

## Measurement

On the reference path (Linux host, Wi-Fi client) and on loopback, with raw
provenance as `docs/*-bench-raw-webtransport.txt`: round-trip p50 and p99
for 1152-byte datagrams; the sustained target → browser rate at 1152 bytes
the relay carries with zero ring drops, against the first user's need of
about 5,400 datagrams per second; sessions per second admitted at the
cap; and the relay's CPU per 10,000 datagrams. No number is claimed in the
README without its row in the ledger.
