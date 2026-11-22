# Capability 11 extension: vista tab sessions (per-cookie connections)

Status: **Contract** (adversarially reviewed 2026-11-22; findings folded,
see the end). Extends [capability vista](20261010-232539-capability-vista.md)
and its [TCP extension](20261114-142034-capability-vista-tcp.md). Owner
approved the direction 2026-11-22. Wire contract the clients build to:
`side-session-notes/.../tab-connections-contract.md`; design:
`tab-sessions-design.md`. This page is the Janus-side contract.

## Why

VistA's broker is per-connection stateful: one job, one signed-on user, one
open patient. A browser user with several chart tabs needs independent
connections — one per tab, each on its own patient — without signing on in
every tab. Today vista maps one token to one session, and a `TCPConnect`
with a live token **replaces** it: a second tab would evict the first.

This extension makes a **browser** `__Host-ov` cookie name a *group* of
connections. The cookie is the one credential (set once, rotated once);
each connection is selected within the group by an `OV-Session` number the
tab keeps. Non-browser (`Authorization: Bearer`) clients are unchanged:
one token, one connection, no `OV-Session`, no patient binding — CPRS owns
one broker connection and changes patient in place on it.

## Class, by namespace

A connection's **class** is where its token resolves, not a per-request
header test:

- **Browser:** the token arrives as `Cookie: __Host-ov` and resolves in the
  route's **groups** namespace (cookie → group → `OV-Session` → connection).
- **Non-browser:** the token arrives as `Authorization: Bearer` and resolves
  in the route's **sessions** namespace (token-HMAC → connection), exactly
  as today.

The two namespaces are **disjoint**: a browser group connection is never
present in the `sessions` token map, and a Bearer connection is never in a
group. So the cookie alone cannot reach a browser connection (it needs
`OV-Session` too), an `OV-Session` alone reaches nothing, and a Bearer
presentation cannot reach a browser connection or vice versa. This
namespace split — not a header check — is what keeps the classes apart.

The vista browser rule (admission row 4) is unchanged: a request whose token
is a cookie, or that carries `Origin`, must carry `X-OV: 1`. A request may
not carry both a cookie and a `Bearer` token (400, as today).

## The group (browser)

Per route, beside the existing `token-HMAC → session` **sessions** map
(now Bearer connections only), Janus keeps a **groups** map and the live
group state, all guarded by the **route mutex** (`rs.mu`) — no new lock, so
join, rotate, end-all, lookup, and forget are each atomic against each
other and preserve the existing `s.mu → rs.mu` order.

- `groups: cookie-HMAC → *vistaGroup`. A `vistaGroup` holds its live
  connections keyed by `OV-Session`, a signed-on count, and the set of
  cookie-HMACs that currently resolve to it (its live token plus any it was
  rotated from, within a grace window). Each connection carries a back-link
  to its group and its `OV-Session`.
- `OV-Session`: 128 bits from `crypto/rand`, 22 base64url characters
  without padding, minted per connection, never reused. A selector under
  the cookie, not a credential alone.
- A non-browser token names one connection directly, as today.

The group adds no new cap: `max_sessions`, `max_unsigned`, `per_client`,
and `per_client_unsigned` bound the route's connections across all cookies,
counted per connection.

## Sessions, revised

Replaces the vista page's **Sessions** for a browser `TCPConnect` (`Origin`
present, token a cookie or none):

- **No cookie, or a cookie resolving no live group** (unknown or an expired
  alias): a new cookie token and a new group; the connection joins it. The
  `accept` reply carries `Set-Cookie: __Host-ov` (the new token) and
  `OV-Session: NUMBER`.
- **A cookie resolving a live group:** the connection **joins** that group —
  no other connection ends, no `Set-Cookie`. The `accept` reply carries
  `OV-Session: NUMBER` only.
- A `reject` reply carries neither `Set-Cookie` nor `OV-Session`.
- A browser `TCPConnect` **never** ends another connection: joining is the
  point. A `TCPConnect` carrying an `OV-Session` request header is **400**
  (Janus assigns the number).

**Non-browser `TCPConnect`** is unchanged: it starts one connection, and a
Bearer token naming a live one ends that one (`replaced`).

## Later requests, revised

Replaces admission rows 11–12 for a browser request (a non-`TCPConnect`
whose token is a cookie):

| Condition | Answer |
| --- | --- |
| No `OV-Session`, or not 22 base64url characters | 400 |
| No cookie | 401 `OV-Session-Ended: unknown` |
| The cookie resolves no live group (unknown or an expired alias) | 401 `unknown`; the cookie is cleared |
| `OV-Session` names no live connection in the group | 401, that connection's tombstone cause or `unknown`; the cookie is **not** cleared (other tabs live) |
| Otherwise | the request runs on that connection |

A non-browser later request (Bearer) is unchanged: the token names its one
connection; an `OV-Session`, `OV-Patient`, or `OV-End` header on a
non-browser request is **400**.

**Clearing the cookie.** `Set-Cookie: __Host-ov=; Max-Age=0` goes out only
when the cookie resolves no live group at all: an `unknown` 401, the last
connection's `#BYE#`, or end-all. A 401 for one dead `OV-Session` while the
group lives sends no cookie. A request on a **rotated** cookie that is still
a live alias resolves its group normally and is never a 401 (see Rotation) —
so ordinary concurrent multi-tab use never clears a live group's cookie.

## Rotation, revised

Replaces the vista page's token rotation for a browser connection. The
check, re-key, and count all happen in **one atomic section under `rs.mu`**,
so two tabs signing on at once cannot both rotate:

- A browser connection that signs on (`XUS AV CODE`, or `XUS CVC` on a
  *changing* connection) rotates the group's cookie **only when the group's
  signed-on count is 0 before this one**. Janus mints a new cookie token,
  adds its HMAC as the group's live token, keeps the previous cookie-HMAC(s)
  as **live aliases** for a grace window (`vistaRotateGrace`, 60 s), and the
  signing-on reply carries the new `Set-Cookie`. Every connection stays in
  the one group; each keeps its `OV-Session`.
- A sign-on when the group already has a signed-on connection (a second tab
  signing on by Kernel's one-time handle) rotates nothing and sends no
  `Set-Cookie`.
- A request by an aliased (recently-rotated) cookie resolves the group and
  runs; the alias is dropped when its grace window passes or the group ends.
  A request by an alias never clears the cookie.
- A connection's `OV-Session` never changes.

A non-browser connection rotates its own token at sign-on, as today
(`replaced`/`rotated` on its one token).

Rotation stays judged inside the sign-on reply, headers held until the first
data line is decided, so the new cookie rides those headers.

## Patient binding (browser only) — advisory, not a boundary

A browser connection may bind to one patient DFN so a **cooperative**
multi-tab client keeps each tab on its own patient and a reload or a
stray RPC cannot silently move it. It is **defense-in-depth and alignment,
not a security boundary**: the base vista trust boundary already holds that
a co-hosted or hostile same-origin page can drive a session, and such a page
can read another patient by putting the DFN in a data RPC's own parameters
or by omitting `OV-Patient`. Binding does not stop that; the clients must
not treat it as isolation between patients. A non-browser connection never
binds (CPRS changes patient in place).

- When a connection's first `ORWPT SELECT` leaves the queue and its first
  parameter is a **type-0 literal** matching `^[1-9][0-9]{0,15}$`, the
  connection binds to that DFN. The request is still sent.
- Once bound, Janus **refuses before sending**, with a 200 broker security
  reply `This connection is open for another patient.`:
  - a later `ORWPT SELECT` whose first parameter is a type-0 literal of a
    different DFN;
  - any request carrying `OV-Patient: DFN` whose DFN differs from the bound
    one.
- `OV-Patient` on an **unbound** connection is allowed.
- A malformed `OV-Patient` (not `^[1-9][0-9]{0,15}$`): 400.
- An `ORWPT SELECT` whose first parameter is **not** a type-0 literal of
  that shape (a reference, a list, absent, malformed) binds nothing and is
  sent unchanged. **Verification owed by the clients/Em:** that em and CPRS
  only ever change the open patient via a type-0-literal `ORWPT SELECT` —
  if another form can, binding is bypassable even for a cooperative client,
  and the guard must widen or be dropped.
- Binding never ends a connection; it is per connection, not per group.

Janus reads the DFN from the walked message (chunk 5's first parameter); it
stores only the per-connection bound value, never logs the DFN, and exposes
only a bound count on `/1.0/vista`.

## Ending one, and end-all

- **`#BYE#` with `OV-Session`** (browser) ends that one connection. The
  cookie is cleared only when it was the group's last live connection.
- **`#BYE#` with `OV-End: all` and no `OV-Session`** (browser) ends every
  connection in the group (snapshot and join are mutually exclusive under
  `rs.mu`, so a concurrent join is either included or starts a fresh group,
  never orphaned): each finishes a running request, em logs out, later
  requests get 401 `bye`. The reply is 200 with `\x00\x00#BYE#\x04`, a
  header `OV-Ended: N`, and the cookie cleared. With no cookie or an unknown
  one: 200, `OV-Ended: 0`, no broker contacted.
- `OV-End` present and not exactly `all`, `all` with an `OV-Session`,
  `OV-End` on a non-`#BYE#` request, or any `OV-End` on a non-browser
  request: **400**.
- A non-browser `#BYE#` is unchanged.

## New request/response headers

| Header | Direction | Meaning |
| --- | --- | --- |
| `OV-Session: NUMBER` | response (accept) / request (browser, later) | the connection's number; selects the connection |
| `OV-Patient: DFN` | request (browser) | the patient a patient RPC names; checked against the binding |
| `OV-End: all` | request (browser `#BYE#`) | end every connection in the group |
| `OV-Ended: N` | response (end-all) | how many connections ended |

Off every request the site forwards anywhere but its vista routes, Janus
strips `__Host-ov` (as today) and `OV-Session`, `OV-Patient`, and `OV-End`,
so no upstream, file, or hub sees them. `X-OV` is left (not sensitive).

## Admission order, revised

The vista page's table, with browser rows split. For a non-`TCPConnect`
browser request, after row 7 (token form) and before the session lookup:

- `OV-Session` present but malformed → 400.
- `OV-Patient` present but malformed → 400.
- `OV-End` present and not exactly `all`, or `all` with an `OV-Session` → 400.

Then end-all (if `OV-End: all`), else the group + `OV-Session` lookup above.
A browser `TCPConnect` carrying an `OV-Session`, or a non-browser request
carrying any OV-* header, is 400. The browser rule (row 4) and the walker
(row 6) are unchanged.

## Tombstones

A dead browser connection leaves a tombstone keyed by (cookie-HMAC,
`OV-Session`) in the route's existing tombstone map, under the same count
cap (`max_sessions × 4`) and 10-minute TTL and the same oldest-first
eviction — so a tab-churning group cannot grow memory without bound. A
rotated cookie-HMAC is a live alias, not a tombstone, until its grace window
passes; then it resolves `unknown`.

## Hard errors (parse/Start)

No new Caddyfile grammar: the limits are unchanged and tab sessions reuse
them. A Bearer-only route (no `origin`) simply never engages groups,
`OV-Session`, or binding.

## Counters (`/1.0/vista`)

Each route row gains `connections` (live), `groups` (live cookie groups),
and `bound` (connections bound to a patient). Refusal rows gain
`ov_session` (missing/malformed, 400), `connection` (a live group, no such
`OV-Session`, 401), `patient` (a cross-patient 200 security reply), and
`ov_header` (a malformed or misplaced OV-* header, 400). The existing
`unknown` row keeps the dead-group and no-cookie 401s.

## Non-goals (this extension)

- A group that outlives all its connections: an empty group is forgotten,
  its live token and aliases dropped.
- Moving a connection between groups, or a shared connection across tabs.
- Server push of a patient change to other tabs (the client coordinates;
  §7 item 00's sign-on-off fallback is the owner's, unbuilt).
- Binding on non-browser connections; a per-RPC patient catalog; binding as
  a boundary against a hostile same-origin page (it is advisory).
- A per-group cap distinct from the route and per-client caps.

## Tests (both layers)

- **go test:** no-cookie `TCPConnect` mints cookie + `OV-Session`; a second
  with that cookie joins (new `OV-Session`, no `Set-Cookie`, first still
  live); a later request selects by cookie + `OV-Session`; missing/short
  `OV-Session` → 400; a dead `OV-Session` in a live group → 401 without
  clearing; a truly unknown cookie → 401 and clears. Rotation: the first
  sign-on rotates and both connections stay in one group, each `OV-Session`
  unchanged; **a request on the pre-rotation cookie within the grace window
  still resolves the group and does not clear it** (the blocker-1
  regression); the second sign-on rotates nothing; the old cookie resolves
  `unknown` after the grace window. Binding: first type-0 `ORWPT SELECT`
  binds; a different DFN on `ORWPT SELECT` or `OV-Patient` → the 200
  security reply, unsent; `OV-Patient` on an unbound connection passes;
  malformed `OV-Patient` → 400; a non-type-0 first param binds nothing; a
  Bearer connection binds to nothing and changes patient freely. End-all
  ends every connection, `OV-Ended: N`, clears; an unknown cookie →
  `OV-Ended: 0`. Misplaced OV-* headers → 400. Caps count per connection
  across a group. OV-* stripped off-route. Tombstones bounded. `-race`
  across join/rotate/end-all/lookup under concurrent requests on one group.
- **./test.sh** vista group: against the fake em, two browser connections
  under one cookie each sign on and open a different patient; the
  cross-patient guard answers the security reply; end-all ends both.

## Acceptance

The vista page's acceptance, plus two chart tabs under one cookie signing on
independently, each bound to its own patient, a cross-patient request
refused, sign-out ending both — proven against the fake em in `./test.sh`
and, as a separate gate, end-to-end against em on pup with the Rip chart
(shreeve/rip#492, shreeve/openvista#3). The VistA-side handle path and the
type-0-`ORWPT SELECT` verification (above) are the clients' and Em's gate.

## Adversarial review (2026-11-22) and what this revision did

| Finding | Severity | Revision |
| --- | --- | --- |
| Shared-cookie rotation + dead-group-401-clears-cookie kills the whole group on ordinary concurrent multi-tab use | blocker | A rotated cookie-HMAC stays a **live alias** for a 60 s grace window; a stale in-flight request resolves the group and never clears; only a truly unknown cookie clears |
| Binding read a type-0/1 literal; a type-1 reference, list, or malformed `ORWPT SELECT` changes the patient while Janus thinks unbound | blocker | Bind only on a **type-0 literal**; the type-0-only verification is owed by the clients/Em, stated as the guard's precondition |
| Binding sold as a boundary but a hostile same-origin page bypasses it | should-fix | Re-scoped to **advisory/defense-in-depth for a cooperative client**, explicitly not a boundary |
| Rotate-at-first-sign-on TOCTOU on the signed-on count | should-fix | Check + re-key + count in one atomic `rs.mu` section |
| end-all races a join, orphaning a connection | should-fix | Join and end-all mutually exclusive under `rs.mu` |
| No lock order for a new group lock | should-fix | No new lock: group state under `rs.mu`, order unchanged (`s.mu → rs.mu`) |
| Per-`OV-Session` tombstones unbounded | should-fix | Tombstoned by (cookie-HMAC, `OV-Session`) in the existing bounded tomb map |
| "fast path: each connection's own token" let a cookie reach a connection without `OV-Session` | should-fix | Deleted: groups and sessions maps are **disjoint**; class follows the namespace |
| Class re-derived per request, not recorded; row 4 admits cookie+X-OV without Origin | should-fix | Class **is** the namespace the token resolves in; the lookup enforces it |
| Reject-loudly gaps (misplaced OV-* headers) | should-fix | Each specified as 400 |
| Off-route stripping covered only the cookie | should-fix | OV-Session/OV-Patient/OV-End stripped off-route too |
| DFN shape 16 digits vs DUZ 15 | nit | Kept; a DFN IEN is not a DUZ, bound stated |
