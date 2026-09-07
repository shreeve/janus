# Exposure Modes — `localhost | lan | wan`

**Status: design contract (not yet implemented).** This is the agreed model for
how Janus binds the edge to ports 80 and 443 across macOS, Linux, and Windows,
in three exposure modes, without running Caddy as root at runtime. It is the
output of a design + adversarial-review pass; implementation follows the build
order at the end.

## The one idea

An exposure mode is **a list of exact addresses to listen on.** Everything is
one pipeline, with a single small platform-specific step in the middle:

```
mode (localhost | lan | wan)
   → plan the exact address list  (portable)
   → acquire listeners for that list  ← the ONLY platform-specific step
   → verify what actually bound matches the plan  (portable)
   → report the real addresses; on any mismatch, refuse to serve  (portable)
```

Only listener *acquisition* differs per OS. Address planning, verification,
status, and the fail-closed policy are shared.

## Two axes, cleanly split

Janus and rip own different things, and today they are tangled. Separate them:

| Axis | Controls | Owner |
| --- | --- | --- |
| **Exposure mode** | which *addresses* the front door listens on | **Janus** (listeners) |
| **Routing posture** | Bonjour/mDNS, `rip.local`/`via.rip`, certs, on-demand TLS | **rip** (rendered sites) |

rip stops declaring bind addresses entirely. Janus injects the active mode's
listeners underneath rip's routes on every load **and** reload, so a reload can
never silently widen the exposure back to a wildcard.

## The three modes

Every mode uses ports 80 **and** 443. Every mode binds **distinct IPv4 and IPv6
sockets** with `IPV6_V6ONLY=1` — never a single dual-stack `*:443` that lets a
v6 listener silently answer v4.

| Mode | Binds | Reachable by |
| --- | --- | --- |
| **localhost** | `127.0.0.1` + `::1` | only this machine — absolute, both families, host-enforced |
| **lan** | localhost + the selected interface's unicast address(es) **+ a host firewall rule scoped to the current on-link subnet** | this machine + the local network; the internet is blocked *by the host*, including over IPv6 |
| **wan** | `0.0.0.0` + `::` (distinct sockets) | anyone the network permits — requires explicit confirmation |

`localhost` is the **default value** — the mode you get when none is named. It is
the one mode that can make an absolute promise (identical on v4/v6, independent
of NAT, router, or firewall), so it carries a literal name with zero wiggle room.
`lan` and `wan` name **binding intent**; their real reachability is a separate
fact that `status` discloses per address (see Status).

### Exact address inventory

**localhost** — 4 sockets:
```
tcp4 127.0.0.1:80     tcp4 127.0.0.1:443
tcp6 [::1]:80         tcp6 [::1]:443        (v6only)
```

**lan** — localhost + selected interface (8 sockets), plus the on-link firewall rule:
```
tcp4 127.0.0.1:80     tcp4 127.0.0.1:443
tcp6 [::1]:80         tcp6 [::1]:443        (v6only)
tcp4 <LAN-v4>:80      tcp4 <LAN-v4>:443
tcp6 [<LAN-v6>]:80    tcp6 [<LAN-v6>]:443   (v6only, global unicast / ULA)
```
Exclude link-local (`fe80::/10`), temporary/privacy v6 addresses, and
VPN/VM/Docker/bridge/AWDL interfaces. Require an explicit `--interface`.

**wan** — 4 wildcard sockets:
```
tcp4 0.0.0.0:80       tcp4 0.0.0.0:443
tcp6 [::]:80          tcp6 [::]:443         (v6only)
```

### Why `lan` needs a firewall rule

Over IPv4 the LAN address is RFC1918 (`10.x`, `192.168.x`) — unroutable from the
internet, so `lan` is host-enforced-private by addressing. Over IPv6 there is no
NAT: the interface's LAN address is a **global, routable** address, so binding it
is, by itself, internet-facing — protected only by the router. To make `lan`
honestly mean "my local network, never the internet" on both families, `lan` mode
installs a **host firewall rule that allows inbound :80/:443 only from the host's
current on-link prefixes** (its `/24` and its `/64`), plus link-local and
loopback, and drops everything else. A LAN peer's source address is inside the
prefix; an internet peer's never is. TCP requires a completed handshake, so an
off-link source cannot spoof its way in. The allowed prefix is re-derived when
the interface address changes (see DHCP drift).

## Per-platform mechanism

Do **not** unify on one binding mechanism. Unify the contract; use each
platform's native minimum.

### macOS — launchd socket activation

macOS is the only platform where an unprivileged process may bind a wildcard low
port but **not** an exact one (`127.0.0.1:443` → `EACCES`). So a privileged binder
is unavoidable:

1. A `root:wheel` LaunchDaemon declares the mode's exact sockets in its `Sockets`
   dict and sets `UserName`/`GroupName` to the developer.
2. launchd binds those sockets under privilege, then spawns Janus as the
   developer uid — the Go runtime never holds privilege.
3. Janus retrieves the named sockets via `launch_activate_socket`, validates each
   descriptor (`getsockname`, `SO_ACCEPTCONN`, `IPV6_V6ONLY`), and hands the fd
   **numbers** to Caddy as `fd/N` listeners in in-process-generated JSON.

A root LaunchDaemon (not a per-user LaunchAgent) is required: it also survives an
unattended reboot with no console login. The **plist** is the privilege boundary
(`root:wheel`, not writable by the runtime user); the runtime binary running as
the user is not.

Caddy gotchas that must be handled or the mode leaks:
- **Auto-HTTPS redirect self-binds a wildcard `:80`** (its computed
  `0.0.0.0:80` never matches an `fd/N`). Supply an explicit plain-HTTP server on
  the `fd/80` descriptors and set `auto_https disable_redirects`.
- **ACME:** prefer DNS-01 so no HTTP-01 wildcard `:80` is ever needed; if
  HTTP-01 is used, serve it through the explicit `fd/80` server.
- **fd lifetime:** Caddy dups the descriptor on adoption and keeps the original
  in a process-global map — Janus must **never close** an activated descriptor,
  and must reuse the **identical `fd/N`** strings across reloads so Caddy's
  listener pool keeps the sockets hot.

### Linux — direct bind + capability

Linux gates low ports by `CAP_NET_BIND_SERVICE` regardless of address, so the
capability alone lets an unprivileged process bind the exact addresses directly.
Socket activation would be pure overhead — do not use it.

- Ship a **system unit** running as an unprivileged `User=` with
  `AmbientCapabilities=CAP_NET_BIND_SERVICE` (and `CapabilityBoundingSet`,
  `NoNewPrivileges=yes`). It starts at boot with no login and survives binary
  upgrades (the capability lives in the unit, not the file).
- Bind `tcp4/` and `tcp6/` as distinct sockets (do not rely on
  `bindv6only=0`); `tcp6/` implies `IPV6_V6ONLY=1`.
- The `lan` firewall rule is nftables (an on-link source set on the input
  chain). Reserve the linger + `setcap` user-unit path as the desktop fallback.

### Windows — direct bind + firewall + preflight

Windows has no low-port restriction at all, so the port itself needs no privilege
in any mode. There is also no fd-passing primitive, so no socket activation.

- Run as a **Windows Service (SCM)** under the virtual account
  `NT SERVICE\janus` (least privilege, never `LocalSystem`), `Automatic` start.
- **Preflight** before bind: check `http.sys` URL registrations
  (`netsh http show servicestate`) and excluded port ranges
  (`netsh interface ipv4 show excludedportrange`). If 80/443 is reserved, abort
  with a **named culprit**, not an opaque `EADDRINUSE`.
- **Firewall per mode:** `localhost` needs no inbound rule (loopback is exempt);
  `lan` gets an inbound allow rule scoped to `Private`/`Domain` with
  `RemoteAddress LocalSubnet` (auto-tracks the on-link subnet); `wan` gets an
  allow rule across all profiles incl. `Public`, gated behind confirmation.

## The rip seam

rip renders **routing only** — host matchers, `redir`, `reverse_proxy`, `janus`
app blocks, TLS/on-demand policy, `import sites/*.caddy`, and the global
`admin`/`log`/`control` lines. It declares **no `default_bind` and no per-site
`bind`**. Delete those directives and retire the `{$RIP_EDGE_BIND}` /
`{$RIP_EDGE_HTTP_BIND}` placeholders.

Janus composes on every `run` and `reload`:
1. Adapt rip's bind-free routing Caddyfile → Caddy JSON (servers come out on the
   bare ports the site schemes imply: `:80`, `:443`).
2. Rewrite each `apps.http.servers.*.listen` by port role — `:80` → the HTTP
   listener set, `:443` → the HTTPS set — to the active mode's real listeners
   (`fd/N` on macOS; exact `addr:port` on Linux/Windows).

Keying off the assigned port (80 vs 443) is stable and version-independent, and
lives inside Janus's config-load path, so `reload` applies it identically.

**New handshake for a mode change** (was: one rip re-render + reload):
- Changing *exposure* → `janus mode lan --interface en0` (Janus re-acquires
  listeners; the privileged path). Loopback listeners persist; LAN listeners are
  added. Fail closed on any error.
- Changing *routing/Bonjour/trust* → rip re-renders + `janus reload` (Janus
  re-injects the current mode's listeners onto the new routes).

Guardrail: Janus rejects a rip-rendered config that still contains
`bind`/`default_bind` on a front-door server.

## Vocabulary, verbs, and status

Adopt `localhost | lan | wan` everywhere; rename rip's inverted
`default`/`local`/`public` to match (rip's `local` meant LAN; rip's `default`
meant loopback — keep the old words as deprecated aliases for one release).

Fold onto Janus's existing verbs rather than adding `install`/`uninstall`:
- `janus autostart [--scope localhost|lan|wan] [--interface en0]` — default
  `localhost`. `wan` is confirmation-gated (`--confirm-wan` non-interactively).
- `janus mode <localhost|lan|wan> [--interface en0]` — change mode on a live
  edge (the privileged re-acquire path rip calls).
- `janus repair` — re-resolve the LAN address + firewall prefix after drift.
- `start|stop|restart|reload|validate|status` — unchanged in spirit; `reload`
  re-injects the current mode's listeners. `--scope`/`--interface` are refused on
  `start`/`reload` (they belong to the installed item).

`status` reports the **real per-family effective addresses** (from `getsockname`
/ the bound listeners), never `*:443`, and labels each address **host-enforced**
vs **network-gated** so the mode name never lies:

```
scope    lan
run as   steve (uid 501)
interface en0
HTTPS    127.0.0.1:443     host-enforced (loopback)
         [::1]:443         host-enforced (loopback)
         10.0.0.211:443    host-enforced (RFC1918 + firewall)
         [2601:…]:443      host-enforced (firewall)        # global v6, on-link-scoped
HTTP/3   disabled
scope    healthy
```

Without the `lan` firewall rule, the global-v6 line would read
`network-gated (router only)` — the honesty the mode word cannot carry.

## State

Single source of truth for the mode = Janus, in its existing state dir
(`~/.local/state/janus/scope.json`, root: `/var/lib/janus/scope.json`): the mode,
interface name + selected address, run-as user, `enable_http3`, and a provision
hash (macOS: hash of the plist `Sockets` dict, to detect state↔plist skew).
Written only by the privileged verbs (`autostart`, `mode`, `repair`), owned by
root when installed as root. Janus reads it to plan + acquire listeners; rip reads
the mode back from `status --json` and never re-derives it. The rendered
Caddyfile carries no mode and is fully derived.

## LAN DHCP drift — one policy, three mechanisms

1. **Install:** resolve interface → enumerate unicast addrs → exclude loopback,
   unspecified, multicast, link-local, temporary → store `{interface, v4, v6}`.
2. **Startup:** verify each stored LAN address is still current on the interface.
3. **Drift → fail closed:** bring up loopback (never drifts), **refuse** LAN,
   mark unhealthy, print `janus repair --scope lan --interface <if>`. Never bind
   wildcard, never silently re-pick.
4. **Repair:** re-resolve → rewrite state → re-provision (macOS: regen plist +
   `bootout`/`bootstrap`; Linux/Windows: rebind) → update the firewall prefix →
   reload.

On macOS a stale `SockNodeName` makes launchd fail to bind — that *is*
fail-closed; Janus surfaces the same `repair` message rather than a raw error.

## Fail-closed invariants

Run the full set at startup **and after every reload**. Any failure ⇒ stop
serving (revert to last-good or exit); never continue.

| Condition | Required behavior |
| --- | --- |
| Effective or real UID == 0 at serve time | Abort |
| A descriptor's `getsockname` ∉ AllowedSet(mode) | Close it, abort |
| A localhost/lan descriptor bound to `0.0.0.0`/`::` | Abort — wildcard where none allowed |
| A v6 socket without `IPV6_V6ONLY=1` | Abort — v4-mapped leak risk |
| A required socket missing, or any surplus socket | Abort |
| Any listening fd not in the activation manifest | Abort — Caddy self-bound (ACME/redirect) |
| Any UDP `:80`/`:443` socket while H3 is disabled | Abort; never advertise h3 |
| A TCP descriptor not in LISTEN (`SO_ACCEPTCONN`) | Abort |
| Stored LAN address no longer on the interface | Unhealthy + `repair`; never wildcard |
| Target port already held by a foreign PID | Refuse; never `SO_REUSEPORT`-share |
| Mode/address cannot be proven | Abort — never assume `wan` |

`AllowedSet`: localhost = `{127.0.0.1, ::1}`; lan = localhost ∪ `{LAN-v4, LAN-v6}`;
wan = `{0.0.0.0, ::}`. Normalize v4-mapped forms before comparison.

## Acceptance tests (connection-based)

The authoritative test is an **actual connection attempt** through each allowed
and forbidden path — not `lsof`. Verify from four vantages: a **loopback** client
(this host), a **LAN peer** (same subnet, both families), an **internet peer over
IPv4**, and an **internet peer over IPv6** (a different global prefix). Cell =
PASS (accepted/served) or BLOCK (refused/filtered).

| mode → | loopback | LAN peer | internet v4 | internet v6 |
| --- | --- | --- | --- | --- |
| **localhost** | PASS | BLOCK | BLOCK | BLOCK |
| **lan** | PASS | PASS | BLOCK | BLOCK (host firewall) |
| **wan** | PASS | PASS | PASS\* | PASS\* |

\* subject to the network (NAT/router firewall). Verify host-intent from the
loopback + LAN vantages; a WAN PASS from the internet requires a deliberate
network change and must never be produced by weakening the host. The LAN-peer
result is the authoritative signal that the host answers a given address — an
internet-v6 BLOCK alone does not prove host scope (it may be the router).

## Build order

**localhost first, on all three platforms** — the simplest mode proves the whole
machine: open the ports, drop privilege, verify, report real addresses, survive a
reload.

- macOS: 4 loopback `fd` sockets from launchd → explicit HTTP + HTTPS `fd`
  servers, redirects suppressed, h3 off, descriptors never closed, `fd/N` stable
  across reloads.
- Linux: system unit `User=` + `AmbientCapabilities`, direct bind of
  `127.0.0.1`/`::1`.
- Windows: the SCM service under `NT SERVICE\janus` (replacing the current stub),
  direct bind, no firewall rule needed for localhost.
- Shared: `ExposureScope → PlanListeners → ServerBuilder`, the fail-closed
  invariants, and `status` real-address reporting.

Then **lan** (interface resolution, the on-link firewall rule, drift + repair),
then **wan** (the wildcard sockets + explicit confirmation, ACME flows).
