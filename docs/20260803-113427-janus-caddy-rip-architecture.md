# Caddy + Janus + Rip Server — implementation map

This map separates the three owners in the running system:

- **Caddy** owns listeners, HTTP/1–3, TLS/ACME, site routing, the
  Caddyfile, and the access-log pipeline.
- **Janus** runs inside that Caddy process. Its process-wide `janus` app
  owns the hot `/1.0` API and shared memory; its
  `http.handlers.janus` site handler owns admitted request decisions.
- **Rip Server** is a tenant. Its manager registers the app and file
  policy, heartbeats, publishes Unix worker sockets, observes access
  events, and supervises disposable workers. Janus never starts or
  supervises Rip processes.

The companion rendered overview is
[`20260803-113427-janus-caddy-rip-architecture.svg`](20260803-113427-janus-caddy-rip-architecture.svg)
or
[`20260803-113427-janus-caddy-rip-architecture.png`](20260803-113427-janus-caddy-rip-architecture.png).

```mermaid
flowchart LR
  classDef actor fill:#13243a,stroke:#66d9ef,color:#eef7ff,stroke-width:2px
  classDef caddy fill:#18304a,stroke:#79b8ff,color:#eef7ff,stroke-width:2px
  classDef janus fill:#152c35,stroke:#58d6b0,color:#eef7ff,stroke-width:2px
  classDef rip fill:#2c223a,stroke:#c79cff,color:#eef7ff,stroke-width:2px
  classDef state fill:#242b3a,stroke:#f3be63,color:#fff8e7,stroke-width:2px

  Browser[Browsers / API clients]:::actor
  LAN[LAN clients]:::actor
  Operator[Operator + Caddyfile]:::actor

  subgraph Process[One Caddy + Janus process]
    direction TB
    Caddy[Caddy edge<br/>listeners · HTTP/1–3 · TLS/ACME<br/>site routing · access logs]:::caddy

    subgraph Handler[Janus site handler — admitted data plane]
      direction LR
      Ping[1 Ping]:::janus --> Auth[6 Auth wall]:::janus --> Resolve[Validate path<br/>resolve host]:::janus
      Resolve --> Hub[4 Hub<br/>WSS terminates here]:::janus
      Resolve --> Files[7 Files + 9 Browse]:::janus
      Resolve --> Cache[3 Cache + coalescing]:::janus
      Hub -->|bridge mode only| Proxy[Least-conn Unix proxy<br/>8 X-Sendfile transform]:::janus
      Files --> Proxy
      Cache --> Proxy
    end

    Control[2 Control API `/1.0`<br/>internal Unix · local HTTP · public HTTPS<br/>apps · TLS ask · stats · publish · access]:::janus
    State[(Hot in-memory state<br/>registry + generations · data plane · hubs<br/>mDNS · auth sessions · browse leases<br/>access bridge)]:::state

    Caddy -->|admitted site route| Ping
    Caddy -->|on-demand TLS ask| Control
    Control --> State
    State -->|host / upstream snapshot| Resolve
  end

  subgraph Rip[One Rip Server tenant]
    direction TB
    Manager[Rip manager<br/>register · heartbeat · publish sockets<br/>watch/generate · drain · access monitor]:::rip
    Doorbell[Doorbell Unix socket<br/>GET /ring]:::rip
    Workers[Rip worker pool<br/>disposable API processes<br/>one Unix socket each]:::rip
    Roots[(Registered App / static roots)]:::rip
    Manager --> Doorbell
    Manager --> Workers
    Manager --> Roots
  end

  Browser -->|HTTPS / WSS| Caddy
  LAN -->|mDNS + HTTP status| State
  Operator -->|cold capabilities + admission| Caddy
  Manager -.->|hot JSON: POST app; heartbeat; PUT upstreams; DELETE; hub publish; access stream| Control
  Proxy -->|HTTP over Unix socket| Workers
  Hub -->|bridge POST when bridge_path is registered| Workers
  Workers -->|response; cache policy; X-Sendfile| Proxy
  Roots -->|Janus reads registered paths| Files
  Resolve -->|GET /ring while request body stays unread| Doorbell
  Doorbell -.->|boot fresh workers; PUT sockets before 204| Control
```

## Request path

For a normal admitted site, `Handler.ServeHTTP` answers enabled
`/ping` first, applies the auth wall, validates the request path, resolves
the host from the hot registry, and then chooses a terminal branch:

1. A WebSocket upgrade on the configured hub path terminates at Janus.
2. A registered static or browse root may answer directly.
3. An eligible anonymous `GET` may hit or fill the micro-cache.
4. Everything else proxies to a healthy least-connection Rip worker over
   a Unix socket. A final worker `X-Sendfile` response authorizes Janus to
   serve the file with its own validators, ranges, and streaming.

Unknown registry hosts answer `404`. An app with no usable upstream
answers `503`; Janus does not invent a tenant route from cold config.

## Rip control and reload path

The Rip manager sends an atomic `POST /1.0/apps` containing its name,
host or patterned site claim, optional registered-file policy, and
initial upstream list. It keeps that claim alive with a heartbeat every
5 seconds; Janus reaps a heartbeat lease after its configured TTL (15
seconds by default). Worker changes publish a complete replacement list
with `PUT /1.0/apps/{id}/upstreams`.

For a validated API change, Rip publishes its doorbell socket and drains
the old workers. The first demand makes Janus send a separate bodyless
`GET /ring` while leaving the client request body unread. Rip boots fresh
workers and awaits the upstream `PUT`; only then does the doorbell answer
`204`. Janus re-resolves the registry and delivers the original request
once to a fresh worker.

Hub WebSockets stay in Janus, above the disposable worker pool. A
registered `bridge_path` lets Janus POST socket lifecycle and frame events
to the worker data plane, and the worker response carries fan-out
directives. The Rip manager uses the trusted hub publish endpoint for
server-originated dings, but its current registration body does not emit
`bridge_path`; another hot API client may add it with `PATCH`.

## Lifetime boundary

Registry, data-plane, hub, mDNS, auth, browse, and access state use Caddy
usage pools, so overlapping Caddy config generations bind to the same
live state. A successful Caddy reload does not drop registrations or Hub
WebSockets. The registry is intentionally memory-only across process
restarts, so Rip Server re-registers.

## Code anchors

- Caddy module registration and cold grammar: [`caddyfile.go`](../caddyfile.go)
- Process-wide app and usage-pool lifecycle: [`app.go`](../app.go),
  [`state.go`](../state.go)
- Site request decision order: [`handler.go`](../handler.go)
- `/1.0` routes: [`control_api.go`](../control_api.go)
- Registration and heartbeat lifecycle: [`apps.go`](../apps.go)
- Unix proxy, health, least-connection routing, and sendfile hook:
  [`dataplane.go`](../dataplane.go)
- Doorbell implementation: [`ring.go`](../ring.go)
- Hub bridge and edge WebSocket termination: [`hub_bridge.go`](../hub_bridge.go),
  [`hub_ws.go`](../hub_ws.go)
- Rip manager implementation: `../rip/packages/server/manager.rip` in the
  sibling Rip repository
