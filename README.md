<p align="center">
  <img src="docs/janus-720w-white.png" alt="Janus Logo" width="360">
</p>

<p align="center">
  <strong>Caddy module: long-lived edge server — TLS admission, dynamic host routing, registry-driven upstreams, heartbeats, on-demand TLS asks, edge-terminated WebSocket fan-out, zero-config LAN presence over mDNS, an edge authentication wall, registered static files and directory browsing, X-Sendfile offload, and bounded app-scoped access observation, driven by a JSON control API.</strong>
</p>

---

**Module names:** `janus` (app) · `http.handlers.janus` (HTTP handler) · `caddy.logging.encoders.janus` (access encoder)

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
versions; every other subcommand is stock Caddy.

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

Opt-in LAN presence: `janus.local` (and every registered single-label `.local` host) answers over multicast DNS with no DNS server or client install, and a plain-HTTP front door serves a read-only, self-contained status page — registry, worker health, heartbeat freshness, and hub counters, with socket paths redacted. An optional `canonical` origin turns the page into a hand-off ramp to real HTTPS, with a built-in diagnostic for router DNS-rebinding filters.

```bash
curl -s http://127.0.0.1:7600/1.0/mdns      # advertiser state (names, states, counters)
curl -s -H 'Host: janus.local' http://127.0.0.1:7680/status.json
```

### 5. auth

URL-prefix gates in front of tenant apps that have no login story of their own: define a shared `users` table and one or more `gate <path> { … }` allow lists (credentials minted by `janus janus-auth-hash`). Each gate's login door is exact `{prefix}auth`. One host-wide session — sign in once, sign out once; a request under a gate proceeds only if the session user is on that gate's allow list. Longest prefix wins; paths outside every gate stay open. What passes a gate carries `Remote-User: <name>`; cookies and client `Remote-User` are stripped on every fall-through. Sessions live in memory: unchanged reloads keep eligible sessions, removing a user or host revokes its sessions after commit, and a restart signs everyone out. Admins observe and revoke over `/1.0/auth`.

```bash
./bin/janus janus-auth-hash                 # mint a version-a passhash (password prompted, never argv)
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
go install github.com/shreeve/janus/cmd/janus@v1.10.1
```

Janus also remains a plain Caddy module: builders that assemble their own
Caddy (xcaddy or a custom main) add `github.com/shreeve/janus` like any
other plugin.

Confirm the modules are linked:

```bash
./bin/janus list-modules | grep -E '^janus$|route53'
```

### Prebuilt releases

On macOS and Linux, one command installs the latest release — it picks the
right archive for the platform, verifies its sha256 against the published
checksums, and installs `janus` into `~/.local/bin` (as root:
`/usr/local/bin`, so a system deploy keeps its path; override either with
`BIN=...`; sudo only if the destination is root-owned):

```bash
curl -fsSL https://raw.githubusercontent.com/shreeve/janus/main/install.sh | bash
```

Pin a version with `... | bash -s v1.10.1`.

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
| `auth_cmd.go` | `janus janus-auth-hash` credential minter |
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
