# Janus operator contract

This is a living reference for the shipped commands. The capability contracts
in [the documentation index](../README.md) define their detailed behavior.

## Command defaults and scope

| Command | Config and environment | What changes |
| --- | --- | --- |
| `run` | Current-directory Caddyfile; otherwise installed service config | Runs the edge in the foreground. The service config enables exposure preflight and ongoing verification. |
| `start` | Installed config unless explicitly overridden | Starts the installed supervisor item, or a bare background process. Supervisor launch options belong in its installed configuration. |
| `stop` | Installed config unless explicitly overridden | Stops the selected edge. An already stopped service is a quiet no-op. |
| `restart` | Installed service config | Validates first, then restarts. Registrations must be recreated by tenants. |
| `adapt` | Caddy's current-directory/default selection or explicit config | Prints native JSON; does not apply it. |
| `validate` | Installed config when present, or explicit config | Adapts and provisions a candidate; does not replace the running edge. |
| `reload` | Installed config when present, or explicit config/address | Applies a candidate through the admin API. A failed candidate leaves the committed generation serving. |
| `autostart` | Installed config, seeded only when absent | Installs/enables the supervisor item after validation. Existing config is preserved. |
| `mode`, `interface` | Stored `scope.json` | Changes the exposure plan, reconciles its firewall, then applies it to the edge. |
| `status` | Selected service paths and stored scope | Reports service state; its version is the invoked binary's version. Listener rows describe the exposure plan. Firewall state is labeled unverified unless it was checked. |
| `apps` | Selected edge's control socket, then identified loopback fallback | Shows exact hosts, site patterns, aliases, workers/files, and leases. `--json` preserves the API response. |
| `serve` | Selected edge's scope and browse capability | Registers a temporary directory, maintains its heartbeat lease, and deletes it on exit. Scope, site reload, and HTTPS route failures are errors before a success announcement. |

Relative paths, cleaned paths, and symlinks naming the installed Caddyfile
receive the same service identity, adapter, environment, and exposure
handling. An explicit different config remains independently managed.

`run`, `adapt`, `validate`, and `reload` share Caddy envfile syntax. The
optional environment file beside the installed config is the default;
explicit `--envfile` paths replace it. Existing process values win, including
empty values. With multiple files the first loaded value wins. Quoted
multiline values work. `JANUS_BIND` is rejected in envfiles because the stored
scope owns it. Changing a configuration placeholder on reload does not
replace the running process environment; that requires a restart.

## Reload survival

| State | Successful compatible reload | Restart |
| --- | --- | --- |
| Apps, upstream registrations, heartbeat/process leases | Preserved; normal deletion and expiry still apply | Empty; tenants re-register |
| Eligible hub WebSockets and memberships | Preserved; committed host/policy changes can close affected connections | Closed |
| Auth sessions | Preserved where the committed host/user policy still authorizes them | Cleared |
| Heartbeat TTL | Must equal the running effective value; otherwise reload is rejected | New effective value is captured |
| Cold browse roots and reservations | Candidate is validated, then committed | Rebuilt from config |
| Renderers and live access streams | Generation-owned resources follow their lifecycle contract | Stopped |
| Durable access logs | Remain on disk under Caddy's logging policy | Remain on disk |

`GET /1.0` reports `heartbeat_ttl`. `serve` heartbeats at one third of it,
including while checking route readiness; older edges without the field use
the historical 5-second interval.

Hub, auth, browse, and mDNS use one host-constraint traversal. Parent and
child constraints intersect; matcher sets remain alternatives. These are
host inventories, not a complete simulation of path, expression, header, or
other request matchers. Cold browse reservations still require unambiguous
exact DNS hosts. An impossible route never becomes a catch-all deeper down.

Control bodies are bounded to 1 MiB, contain one JSON value, reject unknown
struct fields, and reject repeated object keys at every depth (including
keys spelled with JSON escapes). Field-specific absent/null behavior remains
in each endpoint contract. Hub frames retain their own bounded parser and
duplicate-key rules; scope files use the shared strict decoder.

## Updating an older installation

Current seeds include bridge-mode hub, precompressed files, browse, and
Janus access logs. Seeding deliberately leaves existing configs alone.
Compare an older installation with these settings before enabling them:

```caddyfile
{
    janus {
        hub {
            mode bridge
            origin same
        }
        files {
            precompressed
        }
    }
}
```

Merge these into the existing global block. A site with `hub off` continues
to override the global default; remove that override only for sites whose
applications implement the bridge. Give each local site an access-log block
using the same path and `format janus` as the other observed sites. Validate,
reload, then verify bridge admission, compressed representation headers and
bytes, and app-scoped access observation. These are capability enablement
choices, not claims of measured throughput improvements.

Auth gates and external browse renderers require explicit policy choices;
their absence is not a defect. Do not create users, gates, or renderer
processes solely to make a feature checklist complete. A richer diagnostic
command and automatic config-upgrade preview remain separate product designs;
this cleanup makes the existing commands consistent and provides this
reviewable migration procedure.

Plain HTTP LAN discovery and `/trust` are intentional bootstrap paths:
a device cannot necessarily validate the local CA before installing it.
Future public redirect/HSTS changes must preserve that bootstrap and the
exposure-mode contract.
