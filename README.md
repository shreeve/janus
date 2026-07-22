<p align="center">
  <img src="docs/janus-720w-white.png" alt="Janus Logo" width="360">
</p>

<p align="center">
  <strong>Caddy module: long-lived edge server — TLS admission, dynamic host routing, registry-driven upstreams, heartbeats, on-demand TLS asks, a generation-fenced micro-cache with request coalescing, edge-terminated WebSocket fan-out, and zero-config LAN presence over mDNS, driven by a JSON control API.</strong>
</p>

---

**Module names:** `janus` (app) · `http.handlers.janus` (HTTP handler)

Janus is a Caddy module. Caddy provides listeners, HTTP/1–3, TLS, and ACME. Janus provides the inward face: a memory-resident registry and engines driven by the `/1.0` JSON API. Cold Caddyfile config sets capabilities (such as **control** reachability) and which sites admit traffic into Janus; hot `/1.0` calls decide how admitted hosts map to upstreams, health, certificate allowlisting, and realtime fan-out.

```caddyfile
{
	janus {
		ping
		control local
		cache
		hub
		mdns
	}
}

app.example.com {
	janus
}
```

Registry, data plane, and hub state live in pooled process state: a Caddy config reload never drops a registration or a WebSocket. Everything is memory-only by contract — a restart empties the registry and tenants re-register. See [`Caddyfile.example`](Caddyfile.example) for the full operator-facing configuration and [`docs/`](docs/) for the contracts.

This repository is a Go module. Caddy is a dependency, not a git submodule. A Janus-enabled binary is produced with [xcaddy](https://github.com/caddyserver/xcaddy), which compiles stock Caddy together with this module into one static `caddy` binary.

**License:** Apache License 2.0 (same family as Caddy’s source).

## What Janus is — and is not

Every capability in Janus has a famous neighbor; the mix has none. The
novel contract is admission itself: **the app announces itself to its
own edge.** A tenant POSTs its name and hosts to `/1.0/apps`,
heartbeats, and publishes worker unix sockets — and with that one
registration it has TLS and ACME, HTTP/1–3, host routing, health-aware
least-conn balancing, an app-steerable micro-cache, edge-terminated
WebSocket fan-out, and LAN presence, with zero per-app edge
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
| **Varnish** | Cache policy as a product: VCL compiled to native code, grace mode, ESI, bans, now native TLS | A deliberately small micro-cache — 1s default TTL, request coalescing, generation-fenced purge on every upstream swap, hard bypass on `Cookie`/`Authorization` — honest speed for dynamic anonymous GETs, not a policy engine |
| **Pushpin** | Protocol range for realtime: HTTP streaming, long-polling, SSE, SockJS, WebSocket-over-HTTP against a stateless backend | The same architectural instinct — connections held at the proxy, tenant on plain HTTP — plus registry integration (an app's hub lives and dies with its registration) and a validated per-frame directive grammar executed at the edge |
| **Caddy** | Everything it already is: listeners, ACME, HTTP/1–3, the Caddyfile, the admin API, the module ecosystem — all of it remains available beside Janus in the same process | A second axis of dynamism: Caddy's admin API pushes operator config; the Janus registry pulls state from running apps, and a registration never touches the config |

Traefik answers "what is my orchestrator running?"; Janus answers
"what is announcing itself to me right now?" — the second question
needs no infrastructure underneath the app. Varnish is the right tool
when cache policy is the point; the Janus cache exists to make a
stampede on a dynamic page cost the worker ~1 request per second, and
it measures ~410x on capacity-bound routes and 1.6–1.8x on trivial
ones (the [performance ledger](docs/20260719-165500-rip-server-performance.md)
holds every number with raw provenance — sustained hub fan-out is
~0.4M deliveries/s, roughly independent of room size, with zero socket
drops across a config reload). Pushpin proved the edge-held-connection
pattern at Fastly scale; the hub is that pattern folded into the
registry. And Caddy is not a competitor at all: Janus is a Caddy
module, and every stock directive works unchanged next to it.

**Janus is not:**

- **a reverse-proxy configuration language.** There is no parallel
  grammar — capabilities are normal Caddyfile directives with legal
  values, defaults, and hard errors, same as stock Caddy.
- **a persistent store.** Memory-only by contract: a restart empties
  the registry and tenants re-register. Nothing is written to disk.
- **a container orchestrator.** Janus never starts, stops, or
  supervises a process. Tenants run themselves; Janus routes to what
  is alive.
- **a CDN or cache appliance.** The micro-cache shields workers from
  request volume; it does not do cache hierarchies, edge networks, or
  operator-authored policy.
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

- **Go** — current stable release ([go.dev/dl](https://go.dev/dl/))
- **xcaddy** — builds Caddy with modules
- A `Caddyfile` that loads Janus (repo root)

### Install Go (macOS, Homebrew)

```bash
brew update
brew install go          # or: brew upgrade go
go version               # confirm current stable
```

### Install Go (official tarball)

Follow [go.dev/doc/install](https://go.dev/doc/install). On macOS Apple Silicon, that is typically the `darwin-arm64` archive or `.pkg` from [go.dev/dl](https://go.dev/dl/). Ensure `$(go env GOPATH)/bin` is on your `PATH` so tools installed with `go install` are available.

### Install xcaddy

```bash
go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
xcaddy version
```

## Capability order

Cold capabilities land in order. Each step stands alone before the next is added.

| # | Capability | What it does | Doc |
| --- | --- | --- | --- |
| 1 | **ping** | Proves module load, TLS, site admission, cascade | [`capability-ping`](docs/20260718-204255-capability-ping.md) |
| 2 | **control** | Opens the `/1.0` listeners (internal/local/public) | [`capability-control`](docs/20260718-203749-capability-control.md) |
| 3 | **cache** | Site-scoped micro-cache + request coalescing on the data plane | [`capability-microcache`](docs/20260720-033201-capability-microcache.md) |
| 4 | **hub** | Per-app WebSocket fan-out terminated at the edge; tenants observe and steer over HTTP | [`capability-hub`](docs/20260720-162350-hub-design.md) |
| 5 | **mdns** | LAN presence: `janus.local` + per-app `.local` names over multicast DNS, and the read-only status front door | [`capability-mdns`](docs/20260722-034619-capability-mdns.md) |

```bash
export PATH="$(go env GOPATH)/bin:$PATH"

go mod tidy
mkdir -p bin
xcaddy build \
  --with github.com/shreeve/janus=. \
  --output ./bin/caddy

go test ./...
./test.sh   # 10 groups, 127 cases, in capability order: ping, control, apps, data, cache, heartbeat, tls, hub, tenant, mdns
```

### 1. ping (data plane)

Trusted wildcard cert in [`certs/`](certs/); DNS → `127.0.0.1`; SNI picks the site. No control plane required.

```bash
./bin/caddy run
```

```bash
curl -s https://foo.ripdev.io/ping          # catchall → pong
curl -s https://on.ripdev.io/ping           # explicit on → pong
curl -s -o /dev/null -w '%{http_code}\n' https://off.ripdev.io/ping
# → 404
```

On some systems binding :443 needs elevated privileges (`sudo ./bin/caddy run …`). On current macOS it often works without sudo.

### 2. control (`/1.0`)

Same process. Loopback HTTP and a unix socket serve the control API.

```bash
curl -s http://127.0.0.1:7600/1.0
curl -s http://127.0.0.1:7600/1.0/health
curl -s --unix-socket run/janus.sock http://janus/1.0
```

### 3. cache

Anonymous GETs on registered hosts answer from memory for one TTL; concurrent misses coalesce into one worker request; every upstream swap purges.

```bash
curl -s http://127.0.0.1:7600/1.0/cache     # hit/miss/coalesce counters
```

### 4. hub

WebSocket upgrades on hub-enabled sites terminate at Janus; JSON directive frames fan out per app at the edge, so app reloads never drop a socket. The tenant registers a `bridge_path` to observe frames and steer, and publishes through the control plane.

```bash
curl -s http://127.0.0.1:7600/1.0/hub       # fan-out / bridge counters
curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"@":["/lobby"],"news":{"v":1}}' \
  http://127.0.0.1:7600/1.0/apps/$APP_ID/hub/publish
```

### 5. mdns

Opt-in LAN presence: `janus.local` (and every registered single-label `.local` host) answers over multicast DNS with no DNS server or client install, and a plain-HTTP front door serves a read-only, self-contained status page — registry, worker health, heartbeat freshness, cache and hub counters, socket paths redacted. An optional `canonical` origin turns the page into a hand-off ramp to real HTTPS, with a built-in diagnostic for router DNS-rebinding filters.

```bash
curl -s http://127.0.0.1:7600/1.0/mdns      # advertiser state (names, states, counters)
curl -s -H 'Host: janus.local' http://127.0.0.1:7680/status.json
```

## Build and run

From this repository (local module replacement is automatic when you run `xcaddy` inside the module):

```bash
# Develop: build a temporary caddy+janus and run it
xcaddy run

# Produce a binary (see ping-only proof above)
xcaddy build \
  --with github.com/shreeve/janus=. \
  --output ./bin/caddy
./bin/caddy run
```

From anywhere, against a published module version:

```bash
xcaddy build \
  --with github.com/shreeve/janus@main \
  --output ./caddy
```

Pin Caddy and Janus versions for reproducible builds (replace versions as appropriate):

```bash
xcaddy build v2.11.4 \
  --with github.com/shreeve/janus@v1.0.0 \
  --output ./caddy
```

Confirm the module is linked:

```bash
./bin/caddy list-modules | grep janus
```

## JSON config

The Caddyfile adapts to this JSON shape (all capability keys optional; unset keys cascade global → site → built-in default):

```json
{
  "apps": {
    "janus": {
      "control": [{ "mode": "local" }],
      "ping": true,
      "cache": { "enabled": true, "ttl": "1s" },
      "hub": { "enabled": true, "path": "/hub", "max_conns": 4096 },
      "mdns": { "name": "janus.local" },
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
| `apps.go` | Hot apps registry (CRUD, upstreams, bridge_path, heartbeats, TTL sweep) |
| `dataplane.go` | Host → worker-socket proxying (least-conn, health, marked 503s) |
| `ring.go` | Doorbell ring: single-flight wake-up for dirty apps |
| `cache.go` | Micro-cache store: shards, doorkeeper, LRU, purge, counters |
| `cache_serve.go` | Cache request path: decision table, coalescing, the fill |
| `cache_config.go` | `cache` directive: parse, cascade, provision |
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
| `testkit/` | Go test-support program: fixtures + WS driver for `test.sh` |
| `bench/` | Committed bench harness (baseline, leak probe, hub arm) |
| `Caddyfile` | Working cold config (multi-site cascade demos) |
| `Caddyfile.example` | Operator-facing, production-shaped example config (validates standalone) |
| `test.sh` | High-level acceptance suite (self-contained; not a substitute for `go test`) |
| `docs/` | Contracts, capability pages, measurements (`YYYYMMDD-HHMMSS-` prefixed; see [`docs/README.md`](docs/README.md)) |

## Design notes

See [`docs/`](docs/) for the control-plane sketch and related material. The `/1.0` API follows an Incus-inspired style (envelopes, resource paths) while remaining Janus’s own protocol; writes carry no fencing fields — the tenant serializes its own writes (see the [pool protocol](docs/20260719-002000-pool-protocol.md)).

## Name

In Roman myth, **Janus** is the god of doorways and thresholds — beginnings, passages, and the space between inside and outside. He is shown with two faces: one looking out, one looking in. That is the shape of this module. One face serves the public world over TLS; the other coordinates private upstreams, registry, and control-plane state so that serving is possible. The passage between them is the product.
