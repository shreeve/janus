# Janus changelog

Janus release tags use `vX.Y.Z`. Entries are ordered by tag date, newest
first. Versions that were prepared but never tagged (1.6.5, 1.7.1) have no
entry; their changes ship in the next tag.

## 1.12.2 — 2026-09-06

- `janus status --json` reports the service's paths for tools that write
  site files or register with the edge: `env`, `state`, `socket` (the
  control socket), and `admin` (Caddy's admin socket).

## 1.12.1 — 2026-09-06

- Service verbs, hardened. `autostart` says what it does: a user's edge
  starts at login (root's at boot) and, on Linux, `autostart` enables
  lingering so it survives logout. The systemd user unit no longer orders
  itself after `default.target`, an ordering cycle that dropped its start
  at login. `start` after a clean stop replaces the job launchd still
  holds, so a rewritten item is what runs. The seed puts Caddy's admin API
  on a socket under the state directory, and `status` identifies its edge
  by that socket rather than by whoever answers on port 7600, so `stop`,
  `reload`, and `restart` can never reach another Caddy on the host.
  `restart` validates the Caddyfile before stopping anything. `stop` when
  nothing runs is a quiet no-op; `reload` says so. A bare `start` with no
  Caddyfile is refused instead of starting an empty Caddy. Stale pidfiles
  (a crash, a reused pid) are recognized and removed. `autostart --config`
  is gone: the service Caddyfile lives in one place, and the sites
  directory beside it is where variation goes. An env file beside the
  Caddyfile reaches the edge at start. The item's PATH carries only
  absolute entries, and relocated XDG roots follow the edge. A root
  install refuses a binary another user can change. `status --json`
  reports `control` and `apps` only when the control plane answered, and
  splits the version into `janus` and `caddy`.

## 1.12.0 — 2026-09-06

- Adds the service verbs: `janus autostart` installs the edge under launchd
  (macOS) or systemd (Linux) as a login item for a user or a system service
  for root, running now, at every boot, and again after a crash; `janus
  autostart off [stop]` removes it. `janus start`, `stop`, and `restart`
  manage that edge, and restart is how an installed upgrade takes effect.
  `janus status` reports running or stopped, the supervisor, the binary and
  version (and whether the binary is newer than the running edge), the
  Caddyfile and log paths, and how many apps are registered on the control
  plane, exiting 3 when stopped. The service Caddyfile lives at
  `~/.config/janus/Caddyfile` (root: `/etc/janus/Caddyfile`) with the
  control socket, pidfile, and rolling process log under
  `~/.local/state/janus` (root: `/var/lib/janus`, `/var/log/janus`);
  `autostart` seeds a runnable one when absent and validates before
  installing, so a bad config never becomes a restart loop. `start`,
  `stop`, `reload`, and `validate` default to that Caddyfile, and `run`
  does when the current directory has none. The seed imports
  `sites/*.caddy` beside the Caddyfile, so a tool that owns a site drops
  one file there and reloads. `janus status --json` prints the same
  status as one object; `JANUS_SERVICE_LABEL` renames the item so a test
  suite can run an edge beside the host's. `autostart` refuses a state
  root that would push the control socket past the unix path limit.

## 1.11.1 — 2026-09-05

- Makes `janus help` and every subcommand's help name `janus` as the
  command and Janus as the process. The binary now owns its command tree:
  a Janus description and examples, Caddy's registered commands with
  their help text made accurate, a manpage command titled Janus, and
  shell completion generated for a root named `janus`. Caddy's own nouns
  stay where they are accurate (Caddyfile, Caddy's native JSON, Caddy
  modules, caddyserver.com links), and the help footer links both the
  Janus repository and Caddy's command-line reference.
- Fixes a timing race in the release smoke test that could kill the
  binary with SIGPIPE under `grep -q` and fail a platform build.

## 1.11.0 — 2026-09-05

- Holds a request whose every worker is busy instead of answering `503` on
  first contact. The hold is bounded (about 2 s per request, and the app's
  existing 64-waiter cap), wakes when a proxied request to the app
  completes, and re-selects against the app's current upstreams, so a
  burst wider than the pool drains through it and a pool swapped mid-hold
  is where the request lands. Past the bound the client still gets `503` +
  `Retry-After`, distinct from the logged all-unhealthy `503`.
- Accepts `concurrency` on upstream entries: the worker's own request cap.
  Selection skips a socket whose in-flight count has reached it and holds
  for capacity without dialing, so a saturated worker is never dialed only
  to refuse. Omitted or `0` keeps the bounce-and-retry behavior; a negative
  value, or a cap on a doorbell entry, is a `400`.
- Signals a freed slot on every concluded attempt, including a worker that
  dies mid-response, and jitters the backstop poll (100 ms, drawn from
  50–150 ms) so a burst parked in the same millisecond does not re-select
  in lockstep.
- Keeps the access log's `retry_count` honest: bounces taken while a
  request is held for capacity are not counted as retries.
- Adds `--uninstall` to the bootstrap installer, which removes only the
  binary it put down and leaves the Caddyfile, service units, and
  certificates in place.
- Colors installer output on a terminal (honoring `NO_COLOR`), and prints
  the green success line even for archives whose embedded installer
  predates it.
- Documents the capacity hold and published concurrency in the pool
  protocol: decision table, flow-control rules, and defaults.

## 1.10.1 — 2026-08-31

- Retires the `caddy` and `caddy-janus` binary names everywhere: the
  Makefile, installer, release smoke test, bench scripts, and capability
  docs build and invoke `janus`, and `janus-auth-hash` is documented as a
  `janus` subcommand.
- Restores `cap_net_bind_service` once, not twice, when the release
  archive's embedded installer already carries the logic.
- Scopes the memory-only claim (Caddy still writes certificate storage and
  logs) and completes the README's files, sendfile, and browse sections.
- Locates the mdns block structurally in the acceptance suite instead of
  by a literal comment.

## 1.10.0 — 2026-08-31

- Pins that inbound `X-Forwarded-*` headers are replaced with Janus's own
  hop, never appended to.
- Removes the remaining references to the retired micro-cache.

## 1.9.1 — 2026-08-31

- Fixes the acceptance contract for the cache-free build.

## 1.9.0 — 2026-08-31

- Removes the response micro-cache and request coalescing capability.
- Adds the `curl | bash` bootstrap installer for macOS and Linux: a user
  install lands in `~/.local/bin` and root keeps `/usr/local/bin`, so a
  system deploy keeps its path; never uses sudo to create a directory
  under `HOME`.
- Preserves `cap_net_bind_service` across upgrades, hints on fresh Linux
  installs, hardens the no-release guard, and retries `curl`.

## 1.8.1 — 2026-08-27

- Fixes an aborted config reload wedging the control unix socket. Janus
  now owns a pooled canonical listener per socket path; each config
  generation holds only a duplicated descriptor, so an aborted generation
  cannot disturb the survivor.
- Adds the acceptance regression: after an aborted reload, later reloads
  still work and both control listeners answer.

## 1.8.0 — 2026-08-27

- Ships a repo-owned main: `cmd/janus` compiles stock Caddy, the Janus
  module, and the Route 53 DNS provider (DNS-01 wildcard issuance) into one
  static `janus` executable, replacing the xcaddy assembly step.
- Adds `janus version`, reporting the Janus and Caddy versions from the
  release stamp with a VCS-stamped fallback.
- Release archives and the installer ship `janus`; docs describe the
  binary name, build flow, and version surface.

## 1.7.2 — 2026-08-26

- Fixes an identical mdns reload re-probing: the advertiser epoch is kept,
  so the names on the air never flap.
- Adds `Caddyfile.minimal` as the starting point and positions
  `Caddyfile.example` as the capability tour.

## 1.7.0 — 2026-08-26

- Hardens the request path: single host resolution per request, per-app
  upstream selection, a reload commit seam, host-bound auth sessions, and
  mdns rollback.
- Control: rejects unsafe listen URLs and aliased binds, closes listeners
  idempotently, and force-closes shutdown stragglers.
- Hub: dispatches outside the membership lock, kicks and closes
  atomically, fences bridge attachment, and closes a socket whose ping
  write fails.
- Access log: validates but never constructs an unobservable event.
- Gates releases behind the acceptance suite, packages the installer with
  each archive, and makes the acceptance harness safe to run.
- Bumps `golang.org/x`, OpenTelemetry, gRPC, and compression dependencies.

## 1.6.8 — 2026-08-26

- Re-cuts 1.6.7 with the release packaging corrected.

## 1.6.7 — 2026-08-26

- Makes the startup unwind portable, and verifies that a failed startup
  stops serving.
- Makes the passhash rejection fixture deterministic.

## 1.6.6 — 2026-08-14

- Auth hardening from a security review: the login form posts to its own
  gate's door (a non-root gate no longer leaks credentials to the
  upstream), session and CSRF cookies are `SameSite=Strict`, and gate
  matching folds case so `/ONE/secret` cannot slip past a `/one/` gate.
- Gives the auth wall light, dark, and system themes with a three-stop
  toggle.
- Adds `release.sh`: dev builds locally, tagged releases publish
  per-platform binaries.
- Documents auth in front of an app that has none of its own.

## 1.6.4 — 2026-08-10

- Allows remapping aliases beneath the `{site}` pattern; only a true
  self-alias is rejected.

## 1.6.3 — 2026-08-08

- Treats launchd file-descriptor listeners as the HTTP port for shared
  mDNS coverage, so the local posture starts under macOS socket activation.

## 1.6.2 — 2026-08-07

- Moves credentials to passhash version `a` with 32-character base62
  blobs, matching Zift so minted blobs verify in either project; drops the
  `g1` naming.

## 1.6.1 — 2026-08-05

- Renames the hub's `bridge_path` field to `bridge` and normalizes slashes
  (`hub`, `/hub`, and `///hub///` all store as `/hub`).

## 1.6.0 — 2026-08-05

- Serves transparent precompressed sidecars for registered files: a
  same-root `.br`, `.zst`, or `.gz` representation when `Accept-Encoding`
  matches, with identity fallback and `Vary: Accept-Encoding`, never
  across roots.

## 1.5.0 — 2026-08-01

- Adds registration-scoped access logs: a JSON-compatible Caddy encoder
  and bounded per-registration NDJSON streams carrying authoritative
  completion facts across proxied, file, sendfile, browse, error, and
  WebSocket responses.
- Hardens startup, cancellation, trailer, and reload behavior found during
  access-path certification.

## 1.4.0 — 2026-08-01

- Adds the browse capability: navigable file roots with bounded directory
  listings, embedded and custom themes, extension renderers, cold and
  managed root lifetimes, process leases, and explicit `never`,
  `revalidate`, and `forever` cache policies. The renderer supervisor
  bounds process-wide concurrency, timeout, output, and descendant cleanup.

## 1.3.0 — 2026-08-01

- Adds the files capability: registered ordered file roots, SPA shells,
  directory-gated site hosts, and trusted `Rip-Site` context.
- Adds the sendfile capability: always-on `X-Sendfile` offload for final
  client-bound upstream responses, with Janus owning validators, ranges,
  framing, compression compatibility, and streaming while stripping the
  instruction across every trust boundary.
- Rewrites auth as URL-prefix gates under a shared users table: one
  host-wide session, per-gate allow lists, longest-prefix match, and a
  ladder throttle.
- Completes the Rip application edge: atomic initial upstreams at
  registration, direct hub admission for manager-owned browser apps, and
  redacted LAN launch status with site-alias advertising.

## 1.2.0 — 2026-07-22

- Adds the auth capability: an edge authentication wall for apps that have
  none of their own, with one reserved `/auth` URL, argon2id credentials
  minted by `janus-auth-hash`, pooled sessions that survive a reload,
  per-request site authorization, strip-then-inject `Remote-User`, a
  runtime `421` on plain-HTTP walls, and the `/1.0/auth` observe-and-revoke
  surface.

## 1.1.0 — 2026-07-22

- Adds the mdns capability: LAN presence for `janus.local` and per-app
  `.local` names over multicast DNS, a read-only status front door that
  shares the `:80` server, and loud logging when an aborted reload leaves
  the advertiser on the wrong configuration.
- Adds the README's positioning section.

## 1.0.0 — 2026-07-21

- First release, with four capabilities: ping, control, cache, and hub.
- Control: the memory-only `/1.0/apps` registry, the pool coordination
  protocol with Rip Server (doorbell ring, heartbeat TTL reaping,
  worker-marked `503`s as flow control), and on-demand TLS minting gated by
  the registry.
- Cache: a generation-fenced micro-cache with request coalescing (removed
  in 1.9.0).
- Hub: edge-terminated WebSocket fan-out with the Bam directive grammar, a
  tenant bridge, and a publish plane.
- Ships the cascade configuration model, the operator `Caddyfile.example`,
  a control TLS knob, the Go testkit behind the acceptance suite, the
  bench harness with its performance ledger, and the realtime counter
  tutorial.
