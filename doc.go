// Package janus is a Caddy module that fronts disposable worker pools:
// cold Caddyfile capabilities on the data plane, a hot /1.0 control API
// on the control plane, and nothing durable in between.
//
// Janus registers two Caddy modules: the app "janus" (process-wide
// control listeners and capability defaults, configured in the global
// options block) and the HTTP handler "http.handlers.janus" (per-site
// admission and capability overrides). Cold config admits capabilities;
// the hot registry wires tenants: apps register their hosts, publish
// their worker unix sockets, and heartbeat on /1.0, while Janus routes
// admitted requests host→upstream with doorbell-driven reloads that are
// invisible to clients.
//
// Capabilities land in order: ping (1) proves the chassis, control (2)
// serves /1.0, hub (3) terminates WebSockets at the edge and fans JSON
// directive frames out per app while the tenant observes and steers
// over plain HTTP, mdns (4) advertises janus.local plus registered
// .local app hosts over multicast DNS and serves the read-only status
// front door, auth (5) is URL-prefix gates for auth-less apps:
// shared users, per-gate allow lists, one host-wide session, and
// Remote-User strip-and-inject on fall-through, and files (6) serves
// registered ordered roots and SPA shells with directory-gated site host
// patterns and trusted Rip-Site context. Sendfile (7) is an always-on
// reverse-proxy response protocol: a final application X-Sendfile
// instruction selects any regular file Janus can open, and Janus applies
// validators, ranges, and streaming at the edge. Browse
// (8) turns selected hot and cold roots into navigable spaces with a
// content-addressed theme and bounded extension renderers. Access log
// (9) wraps Caddy's JSON encoder without changing durable bytes and
// publishes bounded app-scoped NDJSON through the control plane.
//
// The registry, data plane, and hub state live in pooled process state
// (caddy.UsagePool), so a Caddy config reload never drops a registration
// or a hub socket; only registry DELETE, heartbeat TTL reap, or process
// exit tears them down. Everything is memory-only by contract — a
// restart empties the registry and tenants re-register.
//
// The authoritative contracts live under docs/: the phased build spec,
// one page per capability, the Janus↔tenant pool protocol, and the
// performance ledger with raw bench provenance.
package janus
