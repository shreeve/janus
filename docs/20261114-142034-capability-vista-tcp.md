# Capability 11 extension: vista to a stock RPC Broker over TCP

Status: **Contract**, revised after an adversarial review (below), with
the owner's rulings of 2026-11-14 (each as recommended). Extends
[Capability 11: vista](20261010-232539-capability-vista.md); everything
there holds unless a row below says otherwise.

A `vista` route reaches one kind of backend today: em, one process per
stream, behind a Unix socket, reading a PROXY line first. This extension
lets a route reach **an unmodified VistA RPC Broker** (VistA's own
`XWBTCPM` listener, on YottaDB, GT.M or IRIS) over TCP. The client side
does not change: the same `POST /rpc`, the same token, the same replies.
A site picks its backend by the route's target alone.

```
 client ── HTTPS POST /rpc ──▶ janus ──┬── unix//run/openvista/rpc.sock ──▶ em (PROXY line, then XWB)
        ◀── 200, the reply ──          └── tcp/10.10.218.1:9432 ─────────▶ a stock broker (XWB only)
```

The point: OpenVista's web chart and CPRS's relay run unchanged against
any VistA, not only OpenVista's. VEHU on pup is the proving ground: the
`vehu` container (YottaDB, 10.10.218.233:9430), `vehu-ydb` (YottaDB on the
host, 10.10.218.1:9432) and `vehu-gtm` (GT.M on the host, :9431).

## Scope

- Capability order unchanged: **11**. Cascades: **no** (as the route).
- Surface: the route's target, and two options that exist only for TCP
  targets (failed sign-ons, below). No `protocol` switch: the target's
  network says which backend it is (rule 3).
- Default: none. A route still declares its target.

## Grammar

```caddyfile
vista.example.org {
	janus {
		vista /rpc tcp/10.10.218.1:9432 {
			max_failed_signons 5          # TCP only: failed sign-ons per client key in the window, default 5, range 1–100
			failed_signon_window 15m      # TCP only: the window and the hold after it, default 15m, range 1m–24h
			signon_timeout 170s           # TCP: default 170s, range 10s–179s (the broker's own is 180s)
			# every other option of the route, unchanged
		}
	}
}
```

The target is a Caddy network address with network `unix` (today's) or
`tcp`: an **IP address literal** (IPv4, or IPv6 in brackets, no zone) and
one port. The literal's family decides the dial; a name is refused (the
target is trusted as a registered upstream is, so no resolver may choose
it, and a name with several addresses would make one route reach several
brokers).

## What changes for a TCP target

| # | Topic | Unix target (em) | TCP target (stock broker) |
| --- | --- | --- | --- |
| 1 | Dial | `unix`, 3 s | `tcp`, 3 s, TCP keepalive every 15 s; a read EOF, a half-close included, is `closed` |
| 2 | First bytes | the PROXY line | **none**: the `TCPConnect` body is the first byte |
| 3 | Kernel's client address (sign-on log, `$$GETPEER`, `IO("IP")`, IP lockout, handles) | the PROXY line's client | **Janus's address**, for every session |
| 4 | Message guard | em's (RPC-HTTPS §5) | **Janus's** (below) |
| 5 | Failed sign-ons | Kernel's, per client address | Janus's per client key (below), as Kernel cannot tell clients apart |
| 6 | `signon_timeout` | default 180 s, range 10 s–10 m | default 170 s, range 10 s–179 s, so Janus ends a session before the broker's own 180 s pre-sign-on read sends `TIME-OUT` unasked |
| 7 | The broker ending a session | em exits; the stream closes | after sign-on its activity timeout (`$$BAT^XUPARAM`) sends `#BYE#` unasked and exits: an idle session's stream yields `#BYE#` then EOF, reported `closed` |
| 8 | `\x04` in data | em refuses such a result | the broker sends it; the session ends by `protocol`, at the reply's end if the rest is already buffered, else on the next read (the stock XWBRW writes 255-byte pieces); either way no request gets another's reply |
| 9 | The broker's own refusals | immediate | each waits about 2 s for the line to go quiet (`CLRBUF`, `XWBPRS.m:187-190`) |
| 10 | Everything else | | unchanged: walker, admission, sessions, sign-on, token rotation, caps, timeouts, `#BYE#`, ending causes |

**Rows 1–2, measured.** All three pup brokers close the connection at
once when a PROXY line arrives first (the container's also prints `Job
ended`), and with none answer exactly as em does: `TCPConnect` →
`\0\0accept\x04`, then `XUS SIGNON SETUP` and `XUS INTRO MSG` normally,
nothing unsolicited in 20 s idle (probe of 2026-11-14, no credentials).

## The message guard

A stock broker parses a message as VA wrote it: it builds a list
parameter's subscripts as M code and evaluates a reference parameter
(`XWBPRS.m:155-167`), before it checks the RPC or the user, and it parses
`TCPConnect`'s parameters the same way on every connection's first
message (`XWBTCPM.m:126-135`). Worse, when that parsing raises an M error
partway through a message, its trap answers and returns to the read loop
**without reading the rest of the message** (`XWBTCPM.m:198-214`), so the
rest is read as a new message. em closes all of this in VistA (OpenVista's
`ZEMXWB` edits). For a TCP target Janus closes it before a message leaves
Janus, in two places.

**At admission (row 6), the shape rules, for every message, commands
included:**

1. **A list subscript is literals** (types `2` and `3`): numbers or quoted
   strings (`""` a quote inside one) separated by commas, as `LIT^ZEMXWB`
   reads them: a number is an optional `-`, then digits with an optional
   fraction, or a fraction alone. An empty subscript ends its entry with no
   value, anywhere in a list, as the edited parser skips it.
2. **Bounded literals**, so that parsing cannot raise an M error on any of
   the three engines: a number has at most 15 digits; a quoted string at
   most 200 characters; one entry's subscript at most 240 bytes and 8
   levels; a value at most 32,000 bytes. (Limits chosen under YottaDB's,
   GT.M's and IRIS's key and string maxima, and each measured on the pup
   brokers: at its bound the broker answers exactly one reply, nothing
   after.)
3. **No framing bytes in any field**: no short string, long string,
   subscript or value holds `\x04` or the bytes `[XWB]`.
4. **`TCPConnect` carries exactly three type-`0` literals** (address, port,
   name), none holding `^` (the broker joins them with `^`,
   `XWBTCPM.m:131`), the port all digits. `#BYE#` carries no chunk 5.

**When the request leaves the queue (row 14), the session rules,**
judged under the session's turn so a request queued behind a sign-on is
judged on the state after it:

5. **Before sign-on only the sign-on RPCs run**: the RPC name is byte-exact
   one of `XUS SIGNON SETUP`, `XUS INTRO MSG`, `XUS AV CODE`, `XUS AV
   HELP`, `XUS CVC`, `XUS SET SHARED`, `XWB GET BROKER INFO`, `XWB IM
   HERE`, `XWB IS RPC AVAILABLE` (`PRE^ZEMXWB`). `XUS SET SHARED` sets
   broker state with no check in the stock parser (`XWBPRS.m:11`), as on
   em.
6. **A reference parameter** (type `1`) is refused before sign-on; after
   it, its value is a local variable name (`%` or a letter, then letters
   and digits), optionally with subscripts by rules 1–3 (`REF^ZEMXWB`).
7. **No second sign-on**: once a session is signed on, `XUS SIGNON SETUP`,
   `XUS AV CODE`, `XUS CVC` and `XUS ESSO VALIDATE` are refused. Janus's
   signed-on fact is one-way, and em's checks read the broker's DUZ each
   message; refusing a second sign-on keeps the two the same, since no
   client signs on twice in one session.

**A message that breaks a rule is never sent.** Janus answers it `200`
with a broker reply carrying a fixed security message and no data:
`LEN`, the message, `\0`, `\x04`, the message one of:

| Rule | Message |
| --- | --- |
| 1, 2, 3, 4 | `Invalid request` |
| 5, 6, 7 | `Unauthorized request` |
| failed sign-ons | `Too many failed sign-ons; try again later` |

No client byte is echoed, so the reply is byte-safe. The stream is
untouched and the session continues. Sign-on judging never runs on a
synthetic reply. A refused request counts as refused, never as sent.

The guard runs for TCP targets only; a Unix target's backend guards
itself, and Janus does not judge twice.

## Failed sign-ons

Kernel locks out an address after failed sign-ons where IP locking is on
(site parameter 405; `XUSTZIP`, `XUS3`), and through a TCP route every
user is Janus's one address, so one person's failures would lock out all.
Janus counts failures itself, per client key: an `XUS AV CODE` reply that
signs nothing on and is not an expired verify code (Sign-on). After
`max_failed_signons` in `failed_signon_window`, that key's `XUS AV CODE`
is refused (the fixed message above) until the window passes. A site
should still list Janus's address as a terminal server in Kernel (site
parameter 405.2), the way it lists a Citrix farm, so Kernel's own
threshold suits many users behind one address; Janus's limit keeps one
client from reaching it.

## Trust boundary, what a TCP target changes

- **Kernel never sees the client's address** (row 3). Stock XWB has no
  field VistA trusts for it; `TCPConnect`'s claimed address is replaced by
  the peer (`XWBTCPM.m:133-139`). Janus's session start line logs the TCP
  dial's local port beside the session id and client, so a site joins
  Janus's record to the broker job's exactly.
- **Janus to the broker is plain TCP.** The codes cross it as the client
  sent them (wrapped, which hides nothing). Run Janus on the broker's
  host, or on a segment only the two share. At Start a TCP target whose
  address is neither loopback nor private (RFC 1918, RFC 4193) logs a warn
  line naming the route.
- **The guard closes the parser and the pre-sign-on paths only.** After
  sign-on, an RPC's own behavior is VistA's, gated by its context, as on
  em.
- **Framing** (row 8): with no XWBRW edit, a result holding `\x04` ends
  the session; the user signs on again.

## Hard errors

Parse time, besides today's:

- A target whose network is not `unix` or `tcp`.
- A TCP target whose host is not an IP literal; an IPv6 literal with a
  zone; the unspecified address (`0.0.0.0`, `::`); a multicast address; a
  port missing, `0`, above 65535, or a range.
- `max_failed_signons` or `failed_signon_window` on a Unix route, or out of
  range; `signon_timeout` of 180 s or more on a TCP route.

Start time: unchanged, plus the warn line above. The target is not dialed
at Start.

## Control and observability

`/1.0/vista` shows the target as written and its `network` (`unix` or
`tcp`). Counters for TCP routes: `guard_refused` by rule
(`command_params`, `list_subscript`, `literal_bound`, `framing_byte`,
`signon_rpc`, `reference`, `signon_rerun`), `failed_signons`, and
`failed_signon_holds`. Warn lines use the base contract's per-route,
per-row 10 s limit and never carry a value. The session start line names
the network and, for TCP, the dial's local port.

## Non-goals

- Passing the client's address to a stock broker.
- TLS from Janus to the broker.
- DNS names, several brokers per route, failover.
- VA production sign-on (PIV through `XUS ESSO VALIDATE`): the sign-on
  fact recognizes `XUS AV CODE` and `XUS CVC` only.
- Making a stock broker's RPCs safe after sign-on.

## Decisions, ruled by the owner on 2026-11-14 as recommended

| # | Decision | Recommendation |
| --- | --- | --- |
| 1 | Target `tcp/IP:PORT`, IP literals only | yes |
| 2 | Janus enforces the seven guard rules (em's three, plus literal bounds, framing bytes, `TCPConnect`'s shape, no second sign-on) for TCP targets | yes; without them a TCP route exposes VA's parser to anyone who reaches Janus |
| 3 | Refusals answered as a broker security reply (200) with fixed text | yes |
| 4 | Janus's failed sign-on limit for TCP routes, 5 per client key in 15 min | yes |
| 5 | `signon_timeout` for TCP routes defaults to 170 s, below the broker's 180 s | yes |
| 6 | A warn line, not a refusal, for a TCP target off loopback and private ranges | yes |
| 7 | First release proven on pup's three VEHU brokers only, read-only RPCs and the owner's sign-on | yes |
| 8 | Refusal text: `Invalid request`, `Unauthorized request`, and the failed sign-on hold's (`Too many failed sign-ons; try again later`); the rule broken is in the per-rule counters | ruled by the owner |

## Adversarial review

An independent review (2026-11-14) attacked the first draft: 2 critical,
5 high, 7 medium, 5 low. Every finding was adopted:

| Finding | Revision |
| --- | --- |
| Critical: an M error partway through parsing leaves the rest of a message to be read as a new one; rule 1 alone cannot prevent it | Rules 2 (bounded literals, measured on the brokers) and 3 (no framing bytes) |
| Critical: `TCPConnect`'s parameters reach the same parser, unguarded | The shape rules apply to every message; rule 4 |
| High: a one-way signed-on fact is looser than em's per-message DUZ | Rule 7, no second sign-on |
| High: judging the session rules at admission sees the state before a queued sign-on | Rules 5–7 judged when the request leaves the queue |
| High: echoing the RPC name could overflow the 255-byte security message | Fixed messages, no client byte |
| High: IP lockout through one address is a denial lever | Janus's failed sign-on limit |
| High: DNS names allow rebinding and several brokers per route | IP literals only; `tcp4`/`tcp6` dropped |
| Medium: rule 1 stricter than `LIT` on empty subscripts | Empty subscripts as the edited parser takes them |
| Medium: the broker's own refusals cost about 2 s | Row 9; measured |
| Medium: the broker's 180 s `TIME-OUT` and idle `#BYE#` come unasked | Rows 6–7 |
| Medium: a split early `\x04` may be caught a read late | Row 8 states it |
| Medium: the plain hop carries sign-on codes | The warn line; Trust boundary |
| Medium: thin observability | Named counters, the base rate limit, `network` |
| Medium: the PROXY line's guarantee quietly stops | Row 3; the dial's local port logged |
| Low: `XUS SET SHARED` sets state unchecked | Stated in rule 5 |
| Low: post-sign-on RPC behavior out of scope, unstated | Trust boundary and Non-goals |
| Low: missing target hard errors | Hard errors |
| Low: keepalive and half-close unstated | Row 1 |
| Low: acceptance gaps | Acceptance |

## Acceptance

Go tests, besides today's (the fake em stays for Unix targets):

- A **fake stock broker** on a TCP listener: fails the test if any byte
  precedes the `TCPConnect`; answers `\0\0accept\x04` and scripted replies;
  can close, stall, split replies (after an early `\x04` too), send `#BYE#`
  or `TIME-OUT` unasked, and records every byte it received.
- Grammar: every accepted and refused target form and option.
- Guard: every rule with accepted and refused cases (subscripts
  `"TEXT",1,0`, `1`, `-1.5`, `.5`, `""""` accepted; `$J`, `X`, `1E3`,
  `"a"_"b"`, `@X`, a 16-digit number, a 201-character string refused;
  `\x04` and `[XWB]` in each field kind; `TCPConnect` shapes; references
  and each RPC before and after sign-on; a second sign-on; a request queued
  behind `XUS AV CODE`); a refused message never reaches the fake broker,
  its reply is the fixed bytes, and the session continues.
- Failed sign-ons: the limit, the window, per key, and the hold's end.
- Timeouts: `signon_timeout` below 180 s; the broker's `#BYE#` while idle is
  `closed`.

`./test.sh`: a vista route to a TCP fake broker beside the Unix one.

End to end on each pup broker, recorded in the PR: each literal bound and
framing rule at its limit answers exactly one reply; through Janus a
`TCPConnect`, `XUS SIGNON SETUP`, a refused pre-sign-on RPC, the owner's
sign-on in the chart, `XWB CREATE CONTEXT`, `ORWU USERINFO`, a patient's
read-only RPCs, `#BYE#`.

## Measurement

The TCP dial, the guard's cost per request, and the broker's own refusal
delay, beside the Unix path, five runs, raw provenance as
`docs/*-bench-raw-vista-tcp.txt` (rule 8).
