# Auth: absolute session lifetime and per-user revocation

Status: **Draft — awaiting owner review.** Amends
[capability 5, auth](20260728-160734-capability-auth.md): a `lifetime`
line in the `auth` block, `DELETE /1.0/auth/users/{name}/sessions`, and
the rule that ending a session also ends the connections it authorized.
Nothing here is implemented.

## Why it exists

Janus `auth` fronts internal tools that trust `Remote-User`: Redmine and a
wiki at this deployment, both of which hold PHI incidentally (a ticket that
quotes a lab result is PHI). The PHI application does its own login; these
tools have no login but Janus's.

The shipped session model has one clock, a sliding idle `ttl` (default
8h). Three gaps follow:

1. **No absolute cap.** A session used at least once every eight hours
   never ends. A cookie lifted from an unlocked workstation on Monday
   still works on Friday. HIPAA's automatic-logoff specification
   (§164.312(a)(2)(iii), addressable) and every session-management
   baseline expect an absolute limit alongside the idle one.
2. **No per-user revocation.** `/1.0/auth` revokes one session by id or
   every session. Offboarding a person, or responding to a reported lost
   laptop, means listing sessions, filtering by `user` by hand, and
   deleting each id, racing any new login.
3. **Revocation does not reach live connections.** A hub WebSocket
   upgraded under a gate, and a WebTransport session admitted with a
   ticket minted from a session, outlive that session. Revoking alice
   leaves her open WebSocket streaming.

A fourth gap is a clock defect: idle expiry measures `now.Sub(lastSeen)`
with Go's monotonic clock, which does not advance while macOS or Linux is
suspended. On a laptop edge that sleeps overnight, the night does not
count toward the idle timeout.

## Scope and cascade

- `lifetime` is a line inside an `auth` block, beside `ttl`.
- Scope: **site-scoped**, as the auth block is.
- Cascades: **yes**, with auth's replace semantics. A site that inherits
  the global auth configuration inherits its `lifetime`; a site with its
  own `auth { … }` block uses that block's `lifetime` or the built-in
  default, never the global block's (no merging, as for `ttl`, `users`,
  and gates).
- The control surface is process-wide, like the rest of `/1.0/auth`:
  observe and revoke, never configure (rule 2).

## Caddyfile grammar

```caddyfile
{
	janus {
		auth {
			users {
				alice aZoyWD0mfNZH7GZCh3DH9Te1FwAxA0yc
				bob   aI9eoj7z2M0oTFWY3ai7uQzblQR2spHF
			}
			ttl 30m
			lifetime 12h
			gate /redmine/ {
				alice bob
			}
			gate /wiki/ {
				alice bob
			}
		}
	}
}
```

| Line | Legal values | Default |
| --- | --- | --- |
| `ttl <duration>` | positive duration (unchanged) | 8h |
| `lifetime <duration>` | 5m to 720h inclusive, and not shorter than the block's effective `ttl` | **12h** |

`lifetime` appears at most once per auth block, takes exactly one
argument, and takes no nested block. A `lifetime` shorter than the
effective `ttl` is a contradiction (the idle timeout could never end a
session) and rejects.

## Session clocks

A session records `issuedAt` and `lastSeen` (unchanged) and ends at the
first of:

- **idle**: `ttl` elapsed since `lastSeen`;
- **lifetime**: `lifetime` elapsed since `issuedAt`, however active the
  session is. Activity never extends it.

Elapsed time is the larger of the monotonic elapsed and the wall-clock
elapsed. Monotonic time does not count suspend; wall time counts it but
can step. Taking the larger means a suspended edge counts the night, and a
wall clock stepped backward cannot extend a session. A wall clock stepped
forward can end sessions early, which is the safe direction. The same rule
applies to idle expiry, which fixes the suspend defect.

Expiry is checked on every lookup (the authority, as today) and by the
minute reaper, which now sweeps by both clocks: the reap bound is the
largest effective `ttl` for idle and the largest effective `lifetime` for
age across enabled sites.

## Reload and restart

Sessions are pooled; a reload keeps them, as today. Both limits are read
from the requesting site's effective configuration at each lookup, so:

- a reload that **lowers** `lifetime` or `ttl` applies at once to existing
  sessions: one older than the new lifetime ends at its next request or
  the next reaper pass;
- a reload that **raises** them extends existing sessions; `issuedAt`
  never resets;
- the existing App Start revocation (users no longer in `users`) is
  unchanged and now also closes their bound connections (below).

A restart wipes every session and rotates the per-boot key, as today.

## Bound connections

A hub WebSocket upgraded under a winning gate, and a WebTransport session
admitted with a ticket, record the store key of the session that
authorized them. When that session ends for any reason (sign-out, revoke
by id, user, or all, reload revocation, idle, or lifetime), Janus closes
every connection bound to it:

- hub: close code **1008**, reason `session ended`;
- WebTransport: session close code **0x4A01**, reason `session ended`.

A connection is not activity: frames on an open WebSocket do not refresh
`lastSeen`, so an idle person's open page is signed out with the session.
The page reconnects, the upgrade answers 401, and the application shows
its signed-out state. Closure is prompt for explicit revocation (it runs
in the revoking call) and within one reaper pass (one minute) for idle and
lifetime expiry discovered by the reaper.

Hub connections and WebTransport sessions on paths outside every gate
carry no session and are untouched.

## What the person sees

When a cookie names a session that has ended, the store keeps a
tombstone: the store key, the reason (`idle`, `lifetime`, or `revoked`),
and an expiry one hour out. Tombstones hold nothing but that, live in a
map bounded at 65,536 rows (oldest evicted first, since a lost tombstone
costs only a message), and die with the process.

The login door, rendering its form for a request whose cookie names a
tombstone, shows one fixed sentence and clears the cookie:

| Reason | Sentence |
| --- | --- |
| `lifetime` | Your session reached its 12-hour limit. Sign in again to continue. |
| `idle` | You were signed out after 30 minutes without activity. |
| `revoked` | Your session was ended. Sign in again to continue. |

The durations are the site's effective values, formatted. No query
parameter carries the reason, so no link can put words on the page. The
signed-in status view adds the lifetime's end beside the existing "since":
`Signed in as alice since 09:02 · until 21:02`.

Non-browser requests are unchanged: 401, `no-store`, no
`WWW-Authenticate`, no reason.

## Control surface

`DELETE /1.0/auth/users/{name}/sessions` revokes every live session for
one user across every host, closes their bound connections, and tombstones
them as `revoked`.

| Case | Response |
| --- | --- |
| Name fails the username grammar (`[a-z0-9._-]{1,64}` after lowercasing) | **400** |
| Name appears in no effective `users` table and has no live session | **404** `unknown user alice` |
| Otherwise | **200** `{"revoked": n}`, `n` ≥ 0 |

The path form is deliberate: `DELETE /1.0/auth/sessions` already means
"revoke everyone", and a query form (`?user=`) is one typo
(`?usr=alice`) away from it. The existing routes keep their meaning; the
wipe route rejects any query string with 400 so the two can never be
confused.

`GET /1.0/auth` gains `"lifetime_expired"` (sessions ended by the
lifetime) and `"connections_closed"` (bound connections closed on a
session's end). `GET /1.0/auth/sessions` rows gain `"lifetime_ms"`, the
time left before the lifetime ends the session. Both are counters and
figures, never configuration (users and gates stay cold).

## Interaction with existing capabilities

- **hub**: gains the session binding and the 1008 close; hub paths outside
  gates unchanged.
- **webtransport**: the ticket records the minting session's store key;
  the relay session inherits it. The ticket's own 60-second single-use
  rule is unchanged.
- **mdns dashboard**: a `gate /` on a front-door site behaves as any gate.
- **access log**: closes are not requests and do not appear; the 401 a
  reconnect earns does.
- **metrics** ([draft](20261010-210100-metrics-and-alerting.md)):
  `janus_auth_sessions_ended_total{cause}` gains `idle` and `lifetime`.

## Migration

Existing auth blocks have no `lifetime`, so the 12h default applies on
upgrade: sessions that are older than 12 hours and still active end at
their next request, and every person signs in at least once per 12 hours
afterward. That is the intended change. An existing block with `ttl`
above 12h now rejects (the lifetime default would be shorter than the
idle timeout); the error names both values and the fix.

The seed (`seedConfig`) carries no auth block and does not change.
`Caddyfile.example` shows `ttl 30m` and `lifetime 12h` with a comment on
automatic logoff; the root `Caddyfile` exercises an explicit `lifetime`
on one site and the default on another.

## Hard errors

Parse:

- `lifetime` without exactly one argument, with a block, or repeated;
- a duration outside 5m–720h;
- `lifetime` shorter than the block's `ttl`.

Provision:

- the effective `lifetime` (explicit or default) shorter than the
  effective `ttl`: `auth on example.com: lifetime 12h is shorter than ttl
  24h; the idle timeout could never end a session (lower ttl or raise
  lifetime)`.

## Acceptance

Go tests:

- Parse and provision: legal forms; each hard error with its message; the
  default; inheritance of the global block's `lifetime` and its absence
  under a site's own block.
- Clocks, with the store's injected clock carrying both readings: lifetime
  ends an active session; idle ends an inactive one; a simulated suspend
  (wall advances, monotonic does not) ends both; a wall step backward
  extends neither; the reaper sweeps by both.
- Reload: lowering `lifetime` ends an over-age session on its next lookup;
  raising it extends; `issuedAt` survives.
- Per-user revoke: 200 with the count across two hosts, 404 for an unknown
  name with no sessions, 200 with 0 for a configured name with none, 400
  for an illegal name; `DELETE /1.0/auth/sessions?user=alice` answers 400
  and revokes nothing.
- Bound connections: revoking by user, by id, by all, signing out, reload
  revocation, and reaper-discovered lifetime expiry each close the bound
  hub connection with 1008 and the bound WebTransport session with
  0x4A01; an unbound connection outside gates stays open.
- Tombstones: the door shows each sentence once for the matching reason,
  clears the cookie, shows nothing for an unknown cookie, and the map
  stays within its bound under a flood.
- Status view and `/1.0/auth` fields.

`./test.sh`, `auth` group: a site with `lifetime 5m` and `ttl 5m` in the
fixture Caddyfile; sign in with curl, revoke by user over the control
socket, and the next gated request answers 302 to the door, whose body
carries the revoked sentence; a hub WebSocket opened under the gate with
the testkit WS driver is closed with 1008 when the user is revoked; the
wipe route with a query answers 400. Lifetime expiry in wall time is
pinned in Go, not by sleeping five minutes in acceptance.

## Measurement

The lookup gains one comparison and one wall-clock read under the lock it
already holds. The existing auth Go benchmarks (gated request, login door)
re-run before and after with `-benchmem`; the five-run raw output lands as
`docs/*-bench-raw-auth-lifetime.txt` with a ledger row. No regression
beyond run-to-run spread is accepted without a note.

## Non-goals

- Re-authentication prompts, step-up, or "remember me".
- Per-gate or per-user lifetimes.
- A CLI verb for revocation (the control socket is the surface; a verb is
  a follow-up).
- Revoking by host, or listing by user (the session list carries `user`).
- Persisting sessions or tombstones across restarts.

## Decisions for the owner

| # | Decision | Recommendation |
| --- | --- | --- |
| 1 | Name | `lifetime` (alternatives: `absolute`, `max_age`) |
| 2 | Default | 12h, range 5m–720h |
| 3 | Idle default | Unchanged at 8h; the example and the PHI guidance say 30m |
| 4 | Route | `DELETE /1.0/auth/users/{name}/sessions` |
| 5 | Close codes | hub 1008, WebTransport 0x4A01 |
| 6 | Connection activity refreshes idle | No |

## Adversarial review

1. **The open WebSocket.** The first draft revoked sessions and stopped.
   A revoked user's realtime page kept streaming until the user closed
   the tab.
2. **Suspend.** On the owner's Mac edge, monotonic time stops in sleep: an
   8h idle session survives a 14h night, and a 12h lifetime would too.
3. **Wall-clock trust.** Measuring by wall time alone lets an NTP step
   backward, or an operator fixing the clock, extend every session.
4. **`?user=` and the wipe-all route.** A query parameter on
   `DELETE /1.0/auth/sessions` that the server ignores when misspelled
   kicks everyone; one it ignores when absent is the same route.
5. **Misspelled user.** `DELETE …/users/alcie/sessions` answering
   `{"revoked":0}` reads as success.
6. **Raising `lifetime` resurrects.** A session already over the old
   limit but not yet looked up would live on after a reload raises it.
7. **`lifetime` < `ttl`.** Silently makes `ttl` meaningless; an operator
   reading `ttl 24h` believes it.
8. **Reason in the redirect.** A `?why=` parameter lets anyone put a
   chosen message on the login page through a link.
9. **Tombstone growth.** A tombstone per ended session is an unbounded map
   under a login flood.
10. **Upgrade surprise.** A 12h default changes behavior on every existing
    auth install, and an existing `ttl` above 12h now fails to start.

## Revisions

- Bound connections and their close codes (1).
- Elapsed time is the larger of monotonic and wall elapsed, for both
  clocks (2, 3).
- Path form for per-user revocation; the wipe route rejects any query
  string (4).
- 404 for a name in no `users` table with no sessions (5).
- Stated: raising a limit extends sessions not yet ended; a session the
  reaper or a lookup already ended stays ended (6).
- `lifetime` shorter than `ttl` is a hard error, at parse when both are
  in the block and at provision when either is a default (7).
- The reason comes from a server-side tombstone, never the URL (8).
- Tombstones bounded at 65,536 and one hour (9).
- Migration section names both effects; decision 2 lets the owner pick a
  longer default (10).
