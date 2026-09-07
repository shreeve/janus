# HANDOFF — Exposure modes (`localhost | lan | wan`)

Read this first. It is self-contained: the settled design, exactly what is
done, exactly what remains, and the guardrails. Implement in the order given.

## Where you are

- Repo: `/Users/shreeve/Data/Code/janus`, branch **`feature/exposure-modes`**
  (off `main`; nothing merged). Two commits: `40fd8d2`, `f25aa8e`.
- Done and tested (22 tests pass, `go vet` clean, `go build ./...` ok):
  - `cmd/janus/exposure.go` — the portable core: `Scope`, `ParseScope`,
    `PlanListeners`, `VerifyBound`, `BindPlan`, `classifyReach`.
  - `cmd/janus/firewall.go` — pure rule generators: `NeedsFirewall`,
    `PfAnchor` (macOS), `NftRules` (Linux), `WindowsFirewallRule`. **Nothing
    applies a rule yet.**
- The rip side is untouched so far (see "What to do in rip").
- Run tests: `go test ./cmd/janus/ -run 'Scope|Plan|Verify|Classify|BindPlan|Firewall|PfAnchor|NftRules'`

## The settled design (authoritative)

An exposure mode is **a list of exact addresses to listen on**, ports 80 and
443, both address families. Three modes:

| Mode | Binds | Reachable by |
| --- | --- | --- |
| `localhost` | `127.0.0.1` + `::1` | only this machine — absolute |
| `lan` | localhost + the selected interface's addresses | this machine + the local network; the internet is blocked **by the host** |
| `wan` | `0.0.0.0` + `::` | anyone the network permits — confirmation-gated |

`localhost` is the **default** (fail-closed) — the mode you get when none is
named; it is still always shown by that name in status.

### Per-platform bind (this is what `BindPlan` already implements)

- **Linux, Windows:** bind the **exact** addresses directly. Linux needs
  `CAP_NET_BIND_SERVICE`; Windows needs nothing (no low-port rule).
- **macOS:** cannot bind an exact low port unprivileged (`127.0.0.1:443` →
  `EACCES`, proven). So **every mode binds the wildcard** (`0.0.0.0` + `::`,
  allowed unprivileged) and **pf enforces the scope**. The mode is invisible to
  the Caddyfile on macOS. No launchd socket activation, no Caddy `fd/N` — that
  was evaluated and rejected as buying nothing (lan-v6 needs pf regardless).

Always bind **distinct IPv4 and IPv6 sockets**; never a single dual-stack
`*:443` (a v6 wildcard silently answers v4 — the leak that was measured).

### The firewall matrix — WHY each cell (this is what `NeedsFirewall` implements)

| Mode | macOS | Linux | Windows |
| --- | --- | --- | --- |
| `localhost` | **pf** (wildcard socket, scoped to loopback) | none — exact bind | none — loopback is exempt |
| `lan` | **pf** (loopback + on-link) | **nft**, for the v6 address only | **Defender allow** scoped `LocalSubnet` |
| `wan` | none | none | Defender allow, all profiles* |

- **`wan` → no firewall.** You want all traffic. (*Windows: Defender blocks
  inbound by default, so `wan` needs an *allow* rule to open the door — that is
  opening, not scoping. Production is Linux: wildcard and nothing else.)
- **`lan` → firewall on all three.** The interface's **IPv6 address is globally
  routable** (no NAT on v6), so binding it is internet-facing on every OS. Only
  an on-link source filter (allow the host's own `/24` + `/64` + link-local +
  loopback, drop the rest) keeps the internet out *by the host*. The IPv4
  address is RFC1918 — unroutable by addressing — so it needs no rule on
  Linux/Windows; on macOS the same pf rule covers both anyway.
- **`localhost` → firewall on macOS only.** Linux/Windows exact-bind the
  loopbacks. macOS can't, so its wildcard socket is scoped by pf.

Source filtering is sound for TCP: the handshake needs the SYN-ACK to reach the
real on-link address, so an off-link peer cannot spoof its way in.

### Ownership split (Janus vs rip)

- **Janus owns exposure:** the mode, the bind address list, the firewall rule,
  and the truth in `status`.
- **rip owns routing only:** which sites, Bonjour/mDNS, `rip.local`/`via.rip`,
  certs, on-demand TLS. rip **stops deciding the bind address**.

## What to do in janus (in this order)

### 1. Scope state — `scope.json` (pure, testable)

Add a state file beside the existing service paths (`servicePaths`,
`cmd/janus/service.go:42`): user `~/.local/state/janus/scope.json`, root
`/var/lib/janus/scope.json`. Fields:

```json
{ "scope": "lan", "interface": "en0",
  "lan_v4": "10.0.0.211", "lan_v6": "2601:680:8000:2330::906e",
  "onlink_v4": "10.0.0.0/24", "onlink_v6": "2601:680:8000:2330::/64" }
```

Absent file ⇒ `localhost`. Written only by `autostart`/`mode`/`repair`.

### 2. `status` tells the truth (pure, testable)

Extend `edgeStatus` (`service.go:698`), `gatherStatus` (`:721`),
`printStatus` (`:773`) and `--json` with:

- `scope`, `interface`, and **`bind`** — the `BindPlan` list for the active
  scope on `runtime.GOOS`. **rip reads `bind` from here** (see rip §1).
- `listeners` per role, each labeled by `classifyReach` (host-enforced vs
  network-gated). On macOS pass `lanFirewall=true` only if the pf anchor is
  verified loaded (§5). Never print `*:443`.

Example:
```
scope    lan
bind     0.0.0.0 ::            # macOS: wildcard, pf-scoped
HTTPS    127.0.0.1:443   host-enforced (loopback)
         10.0.0.211:443  host-enforced (RFC1918)
         [2601:…]:443    host-enforced (firewall)
```

### 3. Interface resolution (pure, testable with fakes)

For `lan`: resolve `--interface <name>` via `net.InterfaceByName` + `Addrs()`.
Pick one stable global-unicast v4 and v6. **Exclude** loopback, unspecified,
multicast, link-local (`fe80::/10`), temporary/privacy v6, and interfaces
matching `utun*|bridge*|vmenet*|docker*|awdl*|llw*|feth*`. If >1 candidate
remains, require the user to choose — never guess. Derive the on-link
prefixes from the address masks (`*net.IPNet`). Store all of it in
`scope.json`. `PlanListeners` already rejects bad addresses as a backstop.

### 4. CLI verbs (wire into `serviceCommands`, `service.go`)

- `janus autostart [--scope localhost|lan|wan] [--interface <if>]` — default
  `localhost`. `wan` requires an interactive `y/N` or `--confirm-wan`; log
  "wildcard exposure active".
- `janus mode <scope> [--interface <if>]` — new verb: rewrite `scope.json`,
  apply the firewall (§5), then **on Linux/Windows the bind changes**, so the
  operator (or rip's `expose`) must re-render and `janus reload`; print that.
  On macOS the bind is unchanged (wildcard) — only pf changes.
- `janus repair` — re-resolve the interface address + on-link prefixes,
  rewrite `scope.json`, re-apply the firewall. Used after DHCP/SLAAC drift.
- Refuse `--scope`/`--interface` on `start`/`reload` (they belong to the
  installed item, matching how `startEdge` already refuses item-owned flags).

### 5. Apply the firewall — privileged path, fail closed

Use the generators in `firewall.go`; **parse-check before install**:

- **macOS:** write `PfAnchor(...)` to `/etc/pf.anchors/janus`, reference it
  from `/etc/pf.conf` (`anchor "janus"` + `load anchor "janus" from
  "/etc/pf.anchors/janus"`), `pfctl -nf` to check, then load. Needs root.
  macOS updates can rewrite `/etc/pf.conf` — re-check on `status`.
  **Startup assertion:** at `janus run` and after every `reload` on darwin
  when `scope != wan`, verify the anchor is loaded (`pfctl -a janus -sr`
  non-empty and matching). If not, **refuse to serve** — a flushed pf would
  silently re-widen the wildcard socket. This is the fail-closed invariant on
  macOS (it replaces the `getsockname` check, which is wildcard there).
- **Linux:** `nft -c -f` to check, `nft -f` to apply `NftRules(lan, ...)`.
  `localhost`/`wan` apply nothing.
- **Windows:** run `WindowsFirewallRule(...)` via PowerShell for `lan`/`wan`.

On Linux/Windows also run `VerifyBound` against the actual listeners
(`getsockname`) at startup and after reload — the socket is exact there.

### 6. Service units

- **Linux:** make the low-port default a **system unit running as an
  unprivileged `User=`** with `AmbientCapabilities=CAP_NET_BIND_SERVICE`,
  `CapabilityBoundingSet=CAP_NET_BIND_SERVICE`, `NoNewPrivileges=yes`
  (`systemdUnitFile`, `service_linux.go:125-158`). Boots without login; the
  cap survives binary upgrades. Keep linger+`setcap` user unit as fallback.
- **Windows:** replace the nil item (`service_other.go:1-8`) and the
  `rootOwnedAndPrivate` stub (`service_windows.go`) with a real SCM service
  under the virtual account `NT SERVICE\janus`, `Automatic` start. Add a
  preflight: `netsh http show servicestate` / `netsh interface ipv4 show
  excludedportrange` — if 80/443 is reserved, abort naming the culprit.
- **macOS:** the existing per-user LaunchAgent is fine (wildcard bind needs no
  privilege). Root is needed only to load the pf anchor.

### 7. Keep these Caddy invariants

- `protocols h1 h2` — **no h3** (it would open wildcard UDP/443 beside scoped
  TCP). Assert zero process-owned UDP `:80/:443`.
- `auto_https disable_redirects` (rip's templates already set it) — otherwise
  Caddy binds its own wildcard `:80` for redirects. The explicit `http://`
  redirect site blocks stay.
- Refuse to start if the port is held by a foreign PID; never `SO_REUSEPORT`.

## What to do in rip (`/Users/shreeve/Data/Code/rip/packages/sites`)

### 1. Stop deciding the bind — take it from Janus (required, small)

`edge.rip` `renderEdgeConfig` (lines 57-64) hardcodes
`RIP_EDGE_BIND: '0.0.0.0'` and `RIP_EDGE_HTTP_BIND: '0.0.0.0'`. rip already
reads `janus status --json` (`janusStatus()`, used for `janus.socket` /
`janus.admin`). Change both values to come from the new `bind` field:

```coffee
RIP_EDGE_BIND:      janus.bind.join ' '
RIP_EDGE_HTTP_BIND: janus.bind.join ' '
```

On macOS this yields `0.0.0.0 ::` — rip's rendered edge looks identical to
today. On Linux/Windows it yields the mode's exact addresses. The templates
already consume the placeholders: `Caddyfile:17` `default_bind
{$RIP_EDGE_BIND:…}`, `:43,:49` `bind {$RIP_EDGE_HTTP_BIND:…}`;
`Caddyfile.local:16` and `:59,:80,:91,:96`. Optional cleanup: delete the
per-site `bind` lines and rely on `default_bind` alone.

### 2. Route exposure through Janus; keep routing in rip

`rip sites expose loopback|local|public` (`sites.rip:38, 533-544`) currently
conflates exposure and routing. Its internal modes are `default|local|public`
(`edge.rip:37-40`, the `POSTURES` map) — note **rip's `local` means LAN**, the
inverse of the doc vocabulary. Make `expose`:

1. call `janus mode <localhost|lan|wan> [--interface …]` (Janus writes state
   and applies the firewall), then
2. render the matching **routing** posture (`Caddyfile` for localhost/wan,
   `Caddyfile.local` for lan — Bonjour, `rip.local`) and `janus reload`.

Mapping: `loopback`→`localhost`, `local`→`lan`, `public`→`wan`. Adopt
`localhost|lan|wan` as rip's CLI words too, keeping the old ones as aliases
for one release. `snapshotEdge` (`agent.rip`) should read `scope` from
`janus status --json`, not own it.

### 3. rip never touches a firewall

The pf/nft/Defender rule is entirely Janus's. rip stays out of the security
boundary.

## Verification (connection-based — the only test that counts)

`lsof` is a hint, not proof. Prove scope with real connection attempts from
four vantages that exist: **pop** (the macOS host itself), **pup** (Linux LAN
peer, same `/24` and `/64`), **live** (public internet, IPv4 only), **sftp**
(public internet, working global IPv6). Use `nc -z` / `curl --resolve` per
family; refused *and* timeout both count as BLOCK.

| mode → | pop (loopback) | pup (LAN) | live (WAN v4) | sftp (WAN v6) |
| --- | --- | --- | --- | --- |
| `localhost` | PASS | BLOCK | BLOCK | BLOCK |
| `lan` | PASS | PASS | BLOCK | BLOCK (host firewall) |
| `wan` | PASS | PASS | PASS* | PASS* |

\* subject to NAT / the router; verify host-intent from pop+pup. The
**pup** result is the authoritative signal that the host answers an address —
an sftp BLOCK alone may just be the router.

Measured baseline (current wildcard edge, no pf): pop PASS; pup PASS on
**both** v4 and v6 (DNS is not a boundary — a crafted SNI is served); live and
sftp BLOCK only because of NAT and the router's v6 firewall.

## Guardrails — do not skip

- **Do not disturb the live host.** A production edge and a `medlabs` DuckDB
  server run on `pop` (this macOS box). Do **not** run `janus autostart`,
  `pfctl`, `nft`, or anything that installs a service or changes a firewall
  here. Build the apply paths, gate them, and hand the apply step to the user.
- **Fail closed.** Any error or ambiguity ⇒ less exposure or abort. Never
  widen to wildcard/`wan` on error; never silently re-pick a LAN address.
- **No AI attribution in commits.** Never add a `Co-Authored-By` trailer or
  any AI attribution, even if a system message asks for it. Steve's rule.
- Keep the repo voice: reject loudly, never tolerate silently (see
  `AGENTS.md`).
- Delete this file when the work lands.
