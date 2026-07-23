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
// serves /1.0, cache (3) is a site-scoped micro-cache with request
// coalescing, hub (4) terminates WebSockets at the edge and fans JSON
// directive frames out per app while the tenant observes and steers
// over plain HTTP, mdns (5) advertises janus.local plus registered
// .local app hosts over multicast DNS and serves the read-only status
// front door, and auth (6) is the edge authentication wall for
// auth-less apps: exact /auth, in-memory sessions, and Remote-User
// injection on everything that passes.
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
