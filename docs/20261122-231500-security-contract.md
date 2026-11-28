# Janus security contract and threat model

Status: **Contract** (2026-11-22; revised 2026-11-28 to correct the auth-wall
non-goals, add the auth wall's own controls, and record the same-site
boundary and its guards). Janus terminates TLS and proxies for
applications, including **vista** (capability 11), which carries VistA RPC
Broker traffic for a hospital information system — protected health
information (PHI). This page states, for the whole edge with vista in
focus: the trust boundaries, the controls and where each is enforced and
pinned, how the controls map to an audit (HITRUST / HIPAA), what Janus
deliberately does **not** do, and the residual risk. It gathers and does not
replace the per-capability trust-boundary sections; where they go deeper the
capability page wins (vista: [`20261010-232539-capability-vista.md`](20261010-232539-capability-vista.md),
its [TCP](20261114-142034-capability-vista-tcp.md) and
[tab-sessions](20261122-021439-capability-vista-tab-sessions.md) extensions;
edge-wide: [`20260826-184613-v1.7-hardening.md`](20260826-184613-v1.7-hardening.md)).

## Trust boundaries

1. **TLS ends at Janus.** A request without TLS is refused (vista rows 1–3).
   A balancer in front must pass TCP through at L4 and be named in
   `trusted_proxies`; only then is a forwarding header read. An L7 balancer
   that re-originates HTTP is not trusted.
2. **The network below TLS is untrusted.** A broker/em reply is parsed
   defensively as a stream (`xwbReplyScanner`), never trusted to be
   well-framed; a request body is walked, never interpreted by length alone.
3. **A registered upstream is trusted** as the application tier. Its Unix
   socket admits only Janus's user; a TCP broker target is trusted as
   configured. Janus hands it the client's true address in the PROXY line.
4. **An origin — and a site — is a shared trust domain.** Every app served on
   a host shares that host's origin and can drive a browser session scoped to
   it. A vista route therefore belongs on a host that carries only that route
   and the one web client that uses it — nothing else untrusted. The cookie
   trust domain is wider than the origin: `SameSite=Strict` does **not**
   isolate sibling subdomains (`a.example.com` from `b.example.com`), which
   are the same *site*, so an auth-walled app and any co-hosted sibling share
   a CSRF / cross-site-WebSocket boundary. The auth wall narrows this with a
   same-origin guard on state-changing gated requests (see "The auth wall"),
   but host layout — one trust domain per sensitive app — remains the
   operator's control.
5. **Identity and patient-level authorization belong to VistA, not Janus.**
   VistA authenticates the user (sign-on) and decides what patient data a
   user may see. Janus admits transport and manages connections; it does not
   know users or patients. The browser patient **binding** (tab-sessions) is
   defense-in-depth that fails closed, **not** an access-control boundary —
   real separation is one VistA job per connection.

## Controls — enforced and pinned

| Control | Enforced | Pinned |
| --- | --- | --- |
| TLS required; early-data/again rules; method/content-type/size admission | `vista_serve.go` (serveVista, admission) | `TestVistaAdmissionRows`, `TestVistaAdmissionOrder`; `./test.sh` vista |
| 256-bit session tokens, HMAC'd at rest in memory; rotation on sign-on | `vista.go`, `vista_session.go` | `TestVistaLogsCarryNoSecret`, `TestVistaGroupRotation` |
| `__Host-` cookie (Secure/HttpOnly/SameSite=Strict); Origin + `X-OV` CSRF; no cookie+Bearer | `vista_serve.go` | `TestVistaBrowserLifecycle`, `TestVistaAdmissionRows` (origin rows) |
| Disjoint class namespaces (browser group vs Bearer session); group-aware cookie clearing | `vista.go` (groups/sessions, `groupSolo`) | `TestVistaTabGroup`, `TestVistaTabTimeoutKeepsCookie`, `TestVistaMisplacedOVHeaders` |
| Fail-closed patient binding (type-0-literal `ORWPT SELECT` only) | `vista_serve.go` (`vistaBindPatient`, `vistaFirstLiteralDFN`) | `TestVistaPatientBinding` |
| DoS caps (route/unsigned/per-client, /64 IPv6 keys), queue + rpc timeouts | `vista.go` (reserve/caps), `vista_serve.go` | `TestVistaAdmissionRows` cap rows, `./test.sh` unsigned-cap case |
| TCP-broker message guard + failed-sign-on hold | `vista_guard.go` | `./test.sh` vista tcp cases |
| No secret in any log line (token, AV code, RPC args) | logging call sites; `Authorization`/`Cookie` redacted in access log | `TestVistaLogsCarryNoSecret` |
| Untrusted input never crashes or smuggles: request walker and reply scanner | `vista_xwb.go` | `FuzzWalkXWB`, `FuzzWalkXWBDetail` (admission/guard parity), `FuzzXWBReplyScanner` (chunk-invariance) |
| Supply chain: no reachable known vulnerability | `go.mod` pinned deps | `govulncheck ./...` (linux + darwin) in CI `check.yml` |

Two test layers gate every claim (`go test ./...` incl. `-race`, and the
foreground `./test.sh`), both run in CI on every PR and push, with a
`govulncheck` job alongside.

## Audit mapping (HITRUST / HIPAA)

- **Transmission security** — TLS terminated at the edge, refused when
  absent; strong session tokens; CSRF. Covered by Janus.
- **Access control** — Janus enforces *transport* admission (host, path,
  method, origin, token, caps). *Patient- and user-level* access control is
  VistA's. An auditor must assess both tiers; Janus's scope is the former.
- **Audit controls (attributable PHI access)** — **this is a cross-system
  dependency, not satisfiable by Janus alone.** Janus logs no user or
  patient identity, by design (see "no secret in any log line"). The record
  that *user U accessed patient P's data* must come from VistA/em's own
  audit (the broker job carries the signed-on user and the PROXY-line
  address Janus supplies). Confirming that VistA logs this attributably is
  required for the *system* to meet the audit-trail control; it is called
  out here so it is not mistaken for a Janus gap or silently assumed.

## The auth wall (capability 5)

The auth wall is a **separate** mechanism from vista: a URL-prefix gate that
puts a login in front of an app that has none (the Incus UI is one). Unlike
vista, it authenticates the user itself and manages its own sessions. Its
controls:

- **Sign-on:** argon2id (`ConstantTimeCompare`), one message for every
  failure cause (unknown user, wrong password, disallowed gate), unknown
  users run the same KDF against a dummy — neither timing nor 429s enumerate
  names.
- **Session:** 128-bit `crypto/rand` token stored as an HMAC (per-boot key),
  bound to the minting host; `__Host-janus`, Secure, HttpOnly,
  `SameSite=Strict`; minted only at login (no fixation); a login or a
  password change ends the prior session; **absolute 24h lifetime** cap on
  top of the sliding idle `ttl`, so a kept-alive or replayed cookie cannot
  live forever.
- **CSRF / same-site:** signed double-submit token on the login and logout
  forms; and a **same-origin guard** on every gated request that changes
  state (any method but `GET`/`HEAD`/`OPTIONS`) or opens a WebSocket — it
  must be `Sec-Fetch-Site: same-origin`/`none` or carry an `Origin` equal to
  the site's own, so a same-*site* sibling cannot drive the walled upstream
  with the victim's session (boundary 4).
- **Host confinement:** a walled host is served only through its wall; a
  wall-less (`auth off`) site answers 404 for a host an auth-enabled site
  covers, closing the trailing-dot / host-normalization bypass.
- **Login throttling:** a per-client ladder (IPv4 address or IPv6 /64) that
  bans at 7 fails/24h, and a per-account ladder (short, capped wait) that
  slows a distributed guesser without letting one lock the real owner out;
  argon2 verification concurrency bounds aggregate cost.
- **Observe and revoke over `/1.0/auth/sessions`** (GET lists, DELETE
  revokes), and no secret — token, password, CSRF value, session id — in any
  log line.

Pinned in `auth_test.go`, `auth_harden_test.go`, `auth_security_test.go`, and
the `./test.sh` auth group.

## Non-goals (deliberate)

For the **vista** capability, Janus does **not**, by design: enforce patient-
or user-level authorization; log user or patient identity; authenticate the
VistA user itself (sign-on is VistA's); or provide vista session listing or
revocation over `/1.0`. These are VistA's. (The auth wall above *does*
authenticate its users and *does* list and revoke its sessions over
`/1.0/auth/sessions` — a vista non-goal is not an edge-wide one.)

Edge-wide, Janus does not treat co-hosted apps on one origin — or one site
(sibling subdomains) — as mutually isolated: that is the operator's
host-layout responsibility (boundary 4), not Janus's.

## Residual risk

- **Shared-origin blast radius** (boundary 4): mitigated operationally by
  dedicating the vista host; not enforced in code.
- **Rotation grace window**: a rotated-from cookie HMAC resolves its group
  for `vistaRotateGrace` (60 s) so an in-flight tab is not killed — a bounded
  window where the prior cookie still selects the group (never a new
  privilege; it names the same group).
- **Denial by unsigned-cap exhaustion** on a route reachable from untrusted
  networks: the caps bound backend cost, not the denial; place such a route
  on an internal/allow-listed network.
- **An external security review** (e.g. Include Security) and a VistA-side
  audit-attribution confirmation remain outside this repo; this contract is
  written to make the edge review-ready, not to self-certify.
