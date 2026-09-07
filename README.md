<p align="center">
  <img src="docs/janus-720w-white.png" alt="Janus Logo" width="360">
</p>

<p align="center">
  <strong>Caddy module: long-lived edge server — TLS admission, dynamic host routing, registry-driven upstreams, heartbeats, on-demand TLS asks, edge-terminated WebSocket fan-out, zero-config LAN presence over mDNS, an edge authentication wall, registered static files and directory browsing, X-Sendfile offload, and bounded app-scoped access observation, driven by a JSON control API.</strong>
</p>

---

**Module names:** `janus` (app) · `http.handlers.janus` (HTTP handler) · `tls.permission.janus` (on-demand TLS permission) · `caddy.logging.encoders.janus` (access encoder)

Janus is a Caddy module. Caddy provides listeners, HTTP/1–3, TLS, and ACME. Janus provides the inward face: a memory-resident registry and engines driven by the `/1.0` JSON API. Cold Caddyfile config sets capabilities (such as **control** reachability) and which sites admit traffic into Janus; hot `/1.0` calls decide how admitted hosts map to upstreams, health, certificate allowlisting, and realtime fan-out.

```caddyfile
{
	janus {
		ping
		control local
		hub
		mdns
		files {
			precompressed
		}
	}
}

app.example.com {
	log {
		output file /var/log/janus/access.json
		format janus
	}
	janus
}
```

Registry, data plane, and hub state live in pooled process state: a Caddy config reload never drops a registration or a WebSocket. Janus control state is memory-only — a restart empties the registry and tenants re-register. See [`Caddyfile.minimal`](Caddyfile.minimal) for the operator-facing starting point, [`Caddyfile.example`](Caddyfile.example) for the full capability walkthrough, and [`docs/`](docs/) for the contracts.

This repository is a Go module. Caddy is a dependency, not a git submodule. The `janus` binary is built from [`cmd/janus`](cmd/janus/main.go), which compiles stock Caddy, this module, and the Route 53 DNS provider into one static executable; the module also loads into any custom Caddy build like any other plugin.

**License:** Apache License 2.0 (same family as Caddy’s source).

## What Janus is — and is not

Every capability in Janus has a famous neighbor; the mix has none. The
novel contract is admission itself: **the app announces itself to its
own edge.** A tenant POSTs its name and hosts to `/1.0/apps`,
heartbeats, and publishes worker unix sockets — and with that one
registration it has TLS and ACME, HTTP/1–3, host routing, health-aware
least-conn balancing, edge-terminated WebSocket fan-out, and LAN
presence, with zero per-app edge
configuration. That is the router contract of a PaaS — the shape of
Fly's proxy or Heroku's router — in one self-hosted binary, with the
running app as the source of truth and heartbeat reaping as the
garbage collector: an app that stops heartbeating simply ceases to
exist at the edge. The nearest historical relative is Phusion
Passenger, the app-aware web server — but Passenger manages processes
for its supported languages and learns about apps from the web
server's own config; Janus speaks a JSON API and learns about apps
from the apps.

Each neighbor is better at being itself. The honest comparison:

| Neighbor | What it does better | What Janus does instead |
| --- | --- | --- |
| **Traefik** | Provider ecosystem — routing derived from Docker labels, Kubernetes Ingress/CRD/Gateway API, Consul, Nomad, ECS — plus a deep middleware catalog and a community that dwarfs this module | Apps register themselves over plain HTTP; no container runtime, orchestrator, or label convention required — a bare process on a unix socket is a first-class tenant |
| **Pushpin** | Protocol range for realtime: HTTP streaming, long-polling, SSE, SockJS, WebSocket-over-HTTP against a stateless backend | The same architectural instinct — connections held at the proxy, tenant on plain HTTP — plus registry integration (an app's hub lives and dies with its registration) and a validated per-frame directive grammar executed at the edge |
| **Caddy** | Everything it already is: listeners, ACME, HTTP/1–3, the Caddyfile, the admin API, the module ecosystem — all of it remains available beside Janus in the same process | A second axis of dynamism: Caddy's admin API pushes operator config; the Janus registry pulls state from running apps, and a registration never touches the config |

Traefik answers "what is my orchestrator running?"; Janus answers
"what is announcing itself to me right now?" — the second question
needs no infrastructure underneath the app. The
[performance ledger](docs/20260719-165500-rip-server-performance.md)
holds every number with raw provenance — sustained hub fan-out is
~0.4M deliveries/s, roughly independent of room size, with zero socket
drops across a config reload. Pushpin proved the edge-held-connection
pattern at Fastly scale; the hub is that pattern folded into the
registry. And Caddy is not a competitor at all: Janus is a Caddy
module, and every stock directive works unchanged next to it.

**Janus is not:**

- **a reverse-proxy configuration language.** There is no parallel
  grammar — capabilities are normal Caddyfile directives with legal
  values, defaults, and hard errors, same as stock Caddy.
- **a persistent store.** Janus does not persist registry, session, or
  hub state; tenants re-register after a restart. Caddy still writes
  configured certificate storage and log outputs.
- **a container orchestrator.** Janus never starts, stops, or
  supervises a process. Tenants run themselves; Janus routes to what
  is alive.
- **a service mesh.** One edge, inward-facing unix sockets — no
  sidecars, no inter-service mTLS fabric, no traffic policy between
  tenants.

The same binary spans the whole distance: `janus.local` answering a
phone on a bare LAN with no DNS and no client install, and a
production edge with ACME certificates and HTTP/3 — the difference is
only Caddyfile. And with rip-server as the tenant, the same app file
that runs standalone on a laptop registers, heartbeats, and pools
behind Janus in production, unchanged.

## Requirements

- A supported prebuilt platform, or the current stable **Go** release
  ([go.dev/dl](https://go.dev/dl/)) for source builds
- A Caddyfile that loads Janus; start with [`Caddyfile.minimal`](Caddyfile.minimal)

### Install Go (macOS, Homebrew)

```bash
brew update
brew install go          # or: brew upgrade go
go version               # confirm current stable
```

### Install Go (official tarball)

Follow [go.dev/doc/install](https://go.dev/doc/install). On macOS Apple Silicon, that is typically the `darwin-arm64` archive or `.pkg` from [go.dev/dl](https://go.dev/dl/). Ensure `$(go env GOPATH)/bin` is on your `PATH` so tools installed with `go install` are available.

## Capability order

Cold capabilities land in order. Each step stands alone before the next is added.

| # | Capability | What it does | Doc |
| --- | --- | --- | --- |
| 1 | **ping** | Proves module load, TLS, site admission, cascade | [`capability-ping`](docs/20260718-204255-capability-ping.md) |
| 2 | **control** | Opens the `/1.0` listeners (internal/local/public) | [`capability-control`](docs/20260718-203749-capability-control.md) |
| 3 | **hub** | Per-app WebSocket fan-out terminated at the edge; tenants observe and steer over HTTP | [`capability-hub`](docs/20260720-162350-hub-design.md) |
| 4 | **mdns** | LAN presence: `janus.local` + per-app `.local` names over multicast DNS, and the read-only status front door | [`capability-mdns`](docs/20260722-034619-capability-mdns.md) |
| 5 | **auth** | URL-prefix gates for auth-less apps: shared users, per-gate allow lists, host-wide session, `Remote-User` strip-and-inject | [`capability-auth`](docs/20260728-160734-capability-auth.md) |
| 6 | **files** | Registered ordered roots, transparent precompressed sidecars, SPA shells, and directory-gated site hosts | [`capability-files`](docs/20260730-202700-capability-files.md), [`precompressed extension`](docs/20260805-020944-capability-files-precompressed.md) |
| 7 | **sendfile** | Always-on final upstream `X-Sendfile` transformation with validators, ranges, and streaming | [`capability-sendfile`](docs/20260801-020600-capability-sendfile.md) |
| 8 | **browse** | Navigable hot and cold roots, content-addressed themes, bounded extension renderers, and process leases | [`capability-browse`](docs/20260801-042700-capability-browse.md) |
| 9 | **access log** | JSON-compatible durable access log plus bounded app-scoped NDJSON streams on `/1.0` | [`capability-access-log`](docs/20260801-081600-capability-access-log.md) |

```bash
make janus   # go build ./cmd/janus -> bin/janus

go test ./...
./test.sh    # capability-ordered acceptance groups, ending with access
```

`cmd/janus` is the binary's main package: stock Caddy, the Janus module,
and the Route 53 DNS provider (DNS-01 wildcard certificates), compiled as
one static executable. `go.mod` pins all dependencies, including explicit
security-sensitive overrides.
`janus version`, `-v`, `-V`, and `--version` all report the Janus and Caddy
versions; the service verbs (`autostart`, `start`, `stop`, `restart`,
`status`) are Janus's; every other subcommand is stock Caddy.

### 1. ping (data plane)

Trusted wildcard cert in [`certs/`](certs/); DNS → `127.0.0.1`; SNI picks the site. No control plane required.

```bash
./bin/janus run
```

```bash
curl -s https://foo.ripdev.io/ping          # catchall → pong
curl -s https://on.ripdev.io/ping           # explicit on → pong
curl -s -o /dev/null -w '%{http_code}\n' https://off.ripdev.io/ping
# → 404
```

On some systems binding :443 needs elevated privileges (`sudo ./bin/janus run …`). On current macOS it often works without sudo.

### 2. control (`/1.0`)

Same process. Loopback HTTP and a unix socket serve the control API.

```bash
curl -s http://127.0.0.1:7600/1.0
curl -s http://127.0.0.1:7600/1.0/health
curl -s --unix-socket run/janus.sock http://janus/1.0
```

### 3. hub

WebSocket upgrades on hub-enabled sites terminate at Janus; JSON directive frames fan out per app at the edge, so app reloads never drop a socket. The tenant registers a `bridge` to observe frames and steer, and publishes through the control plane.

```bash
curl -s http://127.0.0.1:7600/1.0/hub       # fan-out / bridge counters
curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"@":["/lobby"],"news":{"v":1}}' \
  http://127.0.0.1:7600/1.0/apps/$APP_ID/hub/publish
```

### 4. mdns

Opt-in LAN presence: `janus.local` (and every registered single-label `.local` host) answers over multicast DNS with no DNS server or client install, and a plain-HTTP front door serves a read-only, self-contained status page — registry, worker health, heartbeat freshness, and hub counters, with socket paths redacted — and the trust page, `http://janus.local/trust`: the one-time setup that has a phone or another machine trust the edge's own CA, so every `.local` name it serves gets a real lock. The page detects the device, hands iOS a configuration profile (`/trust/ca.mobileconfig`) and everything else the root certificate (`/trust/ca.crt`), and watches for the moment trust lands (`/trust/check` over HTTPS) to move the device on. The front door's own name also answers over HTTPS. An optional `canonical` origin turns the page into a hand-off ramp to real HTTPS, with a built-in diagnostic for router DNS-rebinding filters. The announced name must answer on the HTTP port; where a `janus` site covers it, that site serves the front door, and where a site of your own covers it, your site owns the name.

```bash
curl -s --unix-socket ~/.local/state/janus/run/control.sock http://janus/1.0/mdns          # advertiser state
curl -s --unix-socket ~/.local/state/janus/run/control.sock http://janus/1.0/mdns/status   # the front door's snapshot
curl -s -H 'Host: janus.local' http://127.0.0.1/status.json
curl -s -H 'Host: janus.local' http://127.0.0.1/trust/ca.crt                                # the CA, as a phone fetches it
```

### 5. auth

URL-prefix gates in front of tenant apps that have no login story of their own: define a shared `users` table and one or more `gate <path> { … }` allow lists (credentials minted by `janus passhash`). Each gate's login door is exact `{prefix}auth`. One host-wide session — sign in once, sign out once; a request under a gate proceeds only if the session user is on that gate's allow list. Longest prefix wins; paths outside every gate stay open. What passes a gate carries `Remote-User: <name>`; cookies and client `Remote-User` are stripped on every fall-through. Sessions live in memory: unchanged reloads keep eligible sessions, removing a user or host revokes its sessions after commit, and a restart signs everyone out. Admins observe and revoke over `/1.0/auth`.

```bash
./bin/janus passhash                        # mint a version-a passhash (password prompted, never argv)
curl -s http://127.0.0.1:7600/1.0/auth      # wall counters + session count
curl -s http://127.0.0.1:7600/1.0/auth/sessions
```

### 6. files

Apps register ordered static roots. Janus serves files and same-root
precompressed sidecars, can fall back to an SPA shell, and can admit a
host only when its requested site directory exists.

### 7. sendfile

An upstream can return `X-Sendfile` after authorizing a download. Janus
validates the path and serves the file with range and conditional-request
support; this response transformation is always available and has no
configuration toggle.

### 8. browse

Hot registered roots and cold configured roots can expose navigable
directory listings with embedded or custom themes, bounded renderers,
and process leases.

### 9. access log

Each Janus site opts into Caddy access logging with `format janus`. Durable output is byte-equivalent to Caddy's JSON encoder with the same options. Registered-app requests also publish bounded NDJSON to operator streams; Caddy policy remains authoritative, so entries excluded before encoder invocation publish nothing.

```bash
curl -s http://127.0.0.1:7600/1.0/access
curl -sN "http://127.0.0.1:7600/1.0/apps/$APP_ID/access?after=0"
```

## Build and run

From this repository:

```bash
make janus        # go build ./cmd/janus -> bin/janus
./bin/janus run
```

From anywhere, against a published version:

```bash
go install github.com/shreeve/janus/cmd/janus@v1.13.0
```

Janus also remains a plain Caddy module: builders that assemble their own
Caddy (xcaddy or a custom main) add `github.com/shreeve/janus` like any
other plugin.

`janus help` lists every command. Most are Caddy's, under the `janus`
name: `janus trust`, `janus adapt`, `janus fmt`, and the rest behave as
Caddy's reference documents them, and there is no separate `caddy` command
on a Janus host. The service verbs below are Janus's own, and `run`,
`start`, `stop`, `reload`, and `validate` default to the service Caddyfile.

Confirm the modules are linked:

```bash
./bin/janus list-modules | grep -E '^janus$|route53'
```

### Running as a service

One Janus per host is the model, and the binary manages it with the same
verbs as [Harbor](https://github.com/shreeve/duckdb-harbor):

| Verb | What it does |
| --- | --- |
| `janus autostart` | Installs the edge as a service and starts it: running now, at every login (as root: every boot), and again after a crash. |
| `janus autostart off [stop]` | Removes the service; a running edge is left alone unless `stop` is given. |
| `janus start` / `janus stop` | Start or stop that edge. A stop is clean, so it stays stopped until the next login or boot. Stopping a stopped edge is a quiet no-op. |
| `janus restart` | Stop and start again. This is how an installed upgrade takes effect. The Caddyfile is validated first, so a broken one leaves the edge as it is. |
| `janus reload` | Apply an edited Caddyfile in place, keeping every connection. |
| `janus status [--json]` | Running or not, under what, which binary and version, whether the binary is newer than the running edge, how many apps are registered, whether this machine trusts the edge's CA and where a phone trusts it, and the exposure mode: scope, bind, firewall, and each front-door address with how its reach is enforced. Exit 3 when stopped. |
| `janus serve [dir] [--name N]` | Open a directory through the running edge with the browse capability, at `https://<name>.localhost/` here and `https://<name>.local/` on the LAN in `lan` mode (mdns). One registration, heartbeats while it runs, removed on Ctrl-C; nothing new listens. |
| `janus apps [--json]` | What is registered with the running edge: each app's hosts, what serves them (workers, a files root, a site directory), and its lease. Exit 3 when no edge answers. |
| `janus logs [-n N] [-f]` | The edge's log, live: the last lines, then each line as it arrives, across roll-overs, until Ctrl-C. Piped, it prints the last lines and exits (`-f` follows anyway). `--supervisor` reads the supervisor's capture, where a start that failed before the log opened left its reason; that is also what prints when the edge has no log yet. |
| `janus validate` | Check the service Caddyfile without touching the running edge. |
| `janus trust [--export FILE]` / `janus untrust` | Install (or remove) the edge's own CA in this machine's trust stores, through the service edge's admin socket. `--export` writes the root certificate (PEM) instead, for another machine's trust store; phones take it from `http://janus.local/trust`. |
| `janus mode [localhost\|lan\|wan]` | Show the exposure mode, or set it (below) and put the edge on it; the same mode again re-resolves it after the lan address or default route moved. |
| `janus firewall` | Re-apply the host firewall rule the mode needs (root); after a macOS update rewrites `/etc/pf.conf`. |

Run it as yourself on a machine you log in to. Run it as root (`sudo janus
autostart`, with `janus` installed where root finds it: the installer puts
it in `/usr/local/bin` when run as root) for a server: ports 80 and 443
with no setcap, up before anyone logs in, unaffected by logouts. One or
the other on a host, never both, since two edges would contend for the
same ports.

The seeded Caddyfile serves nothing of its own except the LAN's local
names, `*.local` (announced by mdns), with the edge's own CA, which `janus
trust` installs here and `http://janus.local/trust` installs on a phone,
one certificate per registered name at its first handshake. This machine's
own names, `*.localhost`, are a drop-in janus writes into the sites
directory (`sites/localhost.caddy`) and leaves to you afterwards; being a
drop-in, it survives a Caddyfile rendered by another tool, so `janus serve`
works on any host. Both sites carry the seed's defaults for an app: the
hub (bridge mode, same-origin pages), precompressed files, and an access
log in the janus format beside the process log. Apps register hosts under
both, and everything else is a drop-in `*.caddy` file of your own.

The supervisor is launchd on macOS and systemd on Linux. As a user the
service is a login item (`~/Library/LaunchAgents/janus.edge.plist`, or a
systemd user unit, for which `autostart` also enables lingering so the
edge survives logout); as root it is a system service
(`/Library/LaunchDaemons`, or `/etc/systemd/system/janus.service`). It runs
`janus run --config <Caddyfile>` in the foreground, restarts it after a
crash, and never after a clean exit.

The service's files:

| | User | Root |
| --- | --- | --- |
| Caddyfile | `~/.config/janus/Caddyfile` | `/etc/janus/Caddyfile` |
| Drop-in sites | `~/.config/janus/sites/*.caddy` | `/etc/janus/sites/*.caddy` |
| Environment (optional) | `~/.config/janus/env` | `/etc/janus/env` |
| Control and admin sockets, pidfile | `~/.local/state/janus/run/` | `/var/lib/janus/run/` |
| Process log | `~/.local/state/janus/log/janus.log` | `/var/log/janus/janus.log` |

`$XDG_CONFIG_HOME` and `$XDG_STATE_HOME` move the user's Caddyfile and
state. `autostart` seeds the Caddyfile when there is none: the control
plane on its unix socket, Caddy's admin API on
a socket only this edge's user can reach, `ping` on every site, the
process log on a rolling file, and an `import` of the drop-in sites
directory. It validates the Caddyfile before installing anything, so a bad
config never becomes a restart loop, and refuses a state directory deep
enough to push the socket paths past the unix limit. A tool that owns a
site writes one `*.caddy` file into the sites directory and runs `janus
reload`; the Caddyfile itself stays the operator's. `KEY=value` lines in
the env file reach the edge at start (the Route 53 provider's `AWS_*`
credentials, say, or anything a `{env.*}` placeholder reads).

`start`, `stop`, `reload`, and `validate` default to the service Caddyfile
when it exists; `run` does too when the current directory has no Caddyfile
of its own. `stop`, `reload`, and `restart` reach the edge through its
admin socket, so they can never touch another Caddy on the host.

Upgrading is the installer followed by a restart:

```bash
curl -fsSL https://raw.githubusercontent.com/shreeve/janus/main/install.sh | bash && janus restart
```

`janus status` says when the installed binary is newer than the edge that
is running. On Linux a user's edge needs `cap_net_bind_service` on the
binary to bind ports 80 and 443; `autostart` says so when it is missing,
and the installer restores it across upgrades.

`JANUS_SERVICE_LABEL` renames the launchd item or systemd unit (default
`janus.edge` and `janus.service`). With `XDG_CONFIG_HOME`,
`XDG_STATE_HOME`, and `XDG_DATA_HOME` pointed elsewhere it lets a test
suite run an edge beside the host's real one, with its own Caddy storage;
that edge needs its own ports in its own Caddyfile.

Rip's `rip sites` registers apps with this edge and drops its site files
into the sites directory; the edge itself is Janus's to run.

### Exposure modes

The edge listens where its exposure mode says, on ports 80 and 443, IPv4
and IPv6 as separate sockets:

| Mode | Listens on | Reachable by |
| --- | --- | --- |
| `localhost` | `127.0.0.1` and `::1` | this machine only — the default |
| `lan` | localhost plus one interface's private IPv4 address | this machine and its local network; a private address is unreachable from the internet by addressing alone |
| `wan` | every interface | anyone the network lets through |

`janus autostart --scope lan` installs with a mode, `janus mode <scope>`
changes it later, and `janus status` reports it. `lan` uses the interface
the default route leaves through, the one this host reaches its network
by; `--interface <name>` pins another (`--interface auto` returns to the
default route), and when the interface has several private addresses
`--v4` names the one to use — the address is never guessed among
several. `lan` is IPv4 only: a public address is `wan`, and IPv6 (whose
interface addresses are globally routable) stays on the loopback and on
`wan`. Link-local addresses and tunnel, bridge, and VM interfaces are
never candidates.

The mode is stored in `scope.json` under the state directory, and the
service Caddyfile binds through `default_bind {$JANUS_BIND}`, which `run`,
`reload`, `validate`, and `adapt` fill in from the stored mode; the mode
is the one source of that value. The running edge checks its own sockets
against the mode every few seconds and stops, loudly, rather than serve
wider than the mode allows. HTTP/3 stays off in the seed: it would open UDP
listeners beside the scoped TCP ones.

In `wan` the local name families are off: a request whose Host is
`<name>.local`, `<name>.localhost`, `localhost`, `via.rip`, or
`<name>.via.rip` (public DNS pins the via.rip names to 127.0.0.1) is
refused (421) by every janus site, the front door included, no
certificate is minted for such a name, mdns announces nothing, and
`janus serve` says why it has nothing to open. Those names belong to a
machine you sit at and a network you are on; a public address has
neither, and neither the seed's local sites nor a via.rip drop-in need
editing to be safe there. The mode governs the service
edge; a Caddyfile run some other way is localhost by default and keeps
its local names.

What enforces a mode differs by platform, and `status` says which:

- **macOS** cannot bind an exact low port without root, so the socket is
  always the wildcard and a pf anchor (`/etc/pf.anchors/janus`, loaded
  from `/etc/pf.conf` ahead of Apple's anchors) scopes it to the
  loopbacks, plus the on-link subnet for `lan`. The rules are stateless,
  so narrowing the mode cuts the connections the wider mode admitted, and
  the block covers every destination, so an address the host gains later
  is covered too; the cost is that in `localhost` and `lan` the host also
  stops forwarding ports 80 and 443 for others (Internet Sharing, VMs on
  shared networking). pf is enabled by reference token, now and at boot by a small
  LaunchDaemon, since macOS loads pf rules at boot but leaves pf off.
  Loading the anchor is the one step that needs root, and it is the only
  thing root does: `janus mode` and `autostart` write their
  files as you and run `sudo janus firewall` when a terminal can prompt
  (without one they refuse, exit 4, before changing anything); `sudo
  janus firewall` and `sudo janus status` act on your edge when root has
  none of its own. The edge refuses to serve `localhost` or `lan` until
  the anchor is in place, so a wildcard socket is never open unscoped.
- **Linux** binds the exact addresses (the low ports need
  `cap_net_bind_service` for a user's edge) and needs no firewall in any
  mode, since `lan`'s address is private. The system unit never stops
  retrying: a lan edge whose DHCP address moved comes back the moment
  `janus mode lan` stores the new one.
- **Windows**: exposure modes are not supported yet; the verbs refuse.

`janus status` never claims more than it verified. On macOS without root
the firewall shows as `missing` when the anchor files are not in place and
otherwise `unverified (run: sudo janus status)`; with root it verifies
that pf is enabled and holds exactly the anchor's rules. Each address says
what enforces its reach: the loopbacks are `host-enforced (loopback)`, a
private address `host-enforced (RFC1918)` by addressing alone, and `wan`
is `network-gated (wildcard)`.

### Prebuilt releases

On macOS and Linux, one command installs the latest release — it picks the
right archive for the platform, verifies its sha256 against the published
checksums, and installs `janus` into `~/.local/bin` (as root:
`/usr/local/bin`, so a system deploy keeps its path; override either with
`BIN=...`; sudo only if the destination is root-owned):

```bash
curl -fsSL https://raw.githubusercontent.com/shreeve/janus/main/install.sh | bash
```

Then `janus autostart` makes it the host's edge
([Running as a service](#running-as-a-service)).

Pin a version with `... | bash -s v1.13.0`. Uninstall with
`... | bash -s -- --uninstall` — the binary goes; your Caddyfile, service
units, and certificates stay.

The tagged release workflow publishes five self-contained archives:

| Platform | Archive |
| --- | --- |
| macOS Apple Silicon | `janus-<tag>-osx-arm64.tar.gz` |
| Linux x86-64 | `janus-<tag>-linux-amd64.tar.gz` |
| Linux ARM64 | `janus-<tag>-linux-arm64.tar.gz` |
| Windows x86-64 | `janus-<tag>-windows-amd64.zip` |
| Windows ARM64 | `janus-<tag>-windows-arm64.zip` |

Download the matching archive from the
[releases page](https://github.com/shreeve/janus/releases) and extract it.
On macOS and Linux, run `./install.sh` to install `janus` into
`~/.local/bin` (as root: `/usr/local/bin`), or choose another destination
with `BIN="$HOME/bin" ./install.sh`. The extracted binary also runs in place.
On Windows, run `janus.exe` directly. Each archive also contains
`Caddyfile.minimal`, `Caddyfile.example`, the README, and the license; the installer deliberately
leaves configuration in the archive rather than overwriting a live Caddyfile.
The release's `janus-<tag>-checksums.txt` verifies every archive.
(Debian packages the unrelated WebRTC gateway janus-gateway as `janus`;
on a host running both, install this binary under a different `BIN`.)

What changed in each release is in [CHANGELOG.md](CHANGELOG.md).

Release builds run on native GitHub runners and compile from the pushed tag,
so `janus version` on a downloaded binary reports the exact Janus and Caddy
versions, and `janus build-info` records the tagged module version. Pushing
a `v*` tag runs the release workflow automatically.

For a local development build:

```bash
make janus              # working tree -> bin/janus
make unit               # fast Go test suite
make test               # build + unit + acceptance suite
make install            # build + install -> /usr/local/bin/janus
# make install BIN="$HOME/bin"
```

## JSON config

The Caddyfile adapts to the following partial JSON shape. Site-scoped
capabilities cascade global → site → built-in default. `control` and `mdns`
are process-wide; access logging belongs to each HTTP site's `log` config;
sendfile is always on and has no config key.

```json
{
  "apps": {
    "janus": {
      "control": [{ "mode": "local" }],
      "ping": true,
      "hub": { "enabled": true, "path": "/hub", "max_conns": 4096 },
      "mdns": { "name": "janus.local" },
      "auth": { "enabled": true, "replace": true, "users": [{ "name": "alice", "credential": "a…" }], "gates": [{ "prefix": "/", "allow": ["alice"] }], "ttl": "8h" },
      "heartbeat_ttl": "15s"
    },
    "http": {
      "servers": {
        "srv0": {
          "listen": [":443"],
          "routes": [{
            "match": [{ "host": ["app.example.com"] }],
            "handle": [{ "handler": "janus" }]
          }]
        }
      }
    }
  }
}
```

## Layout

| Path | Role |
| --- | --- |
| `app.go` | Process-wide `janus` app (control, global defaults, pooled state) |
| `handler.go` | Site `http.handlers.janus` (admission + site overrides) |
| `caddyfile.go` | Caddyfile wiring: global `janus` block + site directive parsing, directive order |
| `doc.go` | Package overview (the `go doc` face of the module) |
| `state.go` | Pooled process state (registry, data plane, hubs survive reloads) |
| `cascade.go` | Cascade helpers shared by every site-scoped capability |
| `control.go` | Control listener config (`control internal/local/public`, `token:…`) |
| `control_api.go` | Control listeners + `/1.0` mux (meta, health, tls/ask) |
| `control_hub.go` | Hub control surface (publish, snapshot, counters) |
| `apps.go` | Hot apps registry (CRUD, upstreams, bridge, heartbeats, TTL sweep) |
| `dataplane.go` | Host → worker-socket proxying (least-conn, health, marked 503s) |
| `ring.go` | Doorbell ring: single-flight wake-up for dirty apps |
| `hub.go` | Hub state and executor (membership, delivery, counters) |
| `hub_frame.go` | Hub wire grammar (sigils, events, whole-frame validation) |
| `hub_conn.go` | Hub connection lifecycle (writer, backpressure, close paths) |
| `hub_ws.go` | Hub WebSocket edge (admission, upgrade, reader) |
| `hub_bridge.go` | Hub tenant bridge (per-connection FIFO, open/text/close POSTs) |
| `hub_config.go` | `hub` directive: parse, cascade, site table, floors |
| `mdns.go` | mDNS advertiser (pooled, reconcile goroutine) + status front door |
| `mdns_config.go` | `mdns` directive: parse, provision, validation |
| `mdns.html` | Embedded status page (self-contained; zero external resources) |
| `control_mdns.go` | mDNS control surface (`GET /1.0/mdns`) |
| `auth.go` | Auth wall: gates, pooled sessions, throttle ladder, CSRF, login doors |
| `auth_config.go` | `auth` directive: users, gates, parse, cascade, passhash codec, site table |
| `auth_cmd.go` | `janus passhash` credential minter |
| `auth.html` | Embedded login/status page (self-contained; zero external resources) |
| `control_auth.go` | Auth control surface (`GET /1.0/auth`, session list + revocation) |
| `access.go` | Pooled access bridge, registration sequence state, bounded event schema |
| `access_encoder.go` | `caddy.logging.encoders.janus`, wrapping durable JSON |
| `access_stream.go` | Access status and app-scoped NDJSON control streams |
| `access_writer.go` | Response outcome observation with optional interfaces preserved |
| `testkit/` | Go test-support program: fixtures + WS driver for `test.sh` |
| `bench/` | Committed bench harness (baseline, leak probe, hub arm) |
| `Caddyfile` | Working cold config (multi-site cascade demos) |
| `Caddyfile.minimal` | Operator-facing starting point: one app site, one browsable root (validates standalone) |
| `Caddyfile.example` | Production-shaped walkthrough of every capability and knob (validates standalone) |
| `test.sh` | High-level acceptance suite (self-contained; not a substitute for `go test`) |
| `docs/` | Contracts, capability pages, measurements (`YYYYMMDD-HHMMSS-` prefixed; see [`docs/README.md`](docs/README.md)) |

## Design notes

See [`docs/`](docs/) for the control-plane sketch and related material. The `/1.0` API follows an Incus-inspired style (envelopes, resource paths) while remaining Janus’s own protocol; writes carry no fencing fields — the tenant serializes its own writes (see the [pool protocol](docs/20260719-002000-pool-protocol.md)).

## Name

In Roman myth, **Janus** is the god of doorways and thresholds — beginnings, passages, and the space between inside and outside. He is shown with two faces: one looking out, one looking in. That is the shape of this module. One face serves the public world over TLS; the other coordinates private upstreams, registry, and control-plane state so that serving is possible. The passage between them is the product.
