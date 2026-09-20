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

`run`, `adapt`, `validate`, `reload`, and config-selected `stop` share Caddy envfile syntax. The
optional environment file beside the installed config is the default;
explicit `--envfile` paths replace it. Existing process values win, including
empty values. With multiple files the first loaded value wins. Quoted
multiline values work. `JANUS_BIND` is rejected in envfiles because the stored
scope owns it. Changing a configuration placeholder on reload does not
replace the running process environment; that requires a restart.

An explicit `stop --address` uses that admin endpoint directly, even when
the installed configuration or scope file is unusable. Restart also stops
a foreground edge identified through its control listener and waits for it
to release the listener before starting a replacement.

`janus mode lan` advertises only the selected LAN interface's selected private
IPv4 address. It never advertises the interface's IPv6 addresses, since LAN
exposure accepts IPv6 only on loopback. Localhost and WAN modes advertise no
LAN names. A mode/interface/address change replaces the mDNS records on the
advertiser's next reconciliation pass. An explicit `mdns interface` list must
include the selected LAN interface.

`status --json` includes `dashboard_url` and `trust_url` when available.
Janus resolves these from its configured front door, actual listener port,
exposure, and conflict-resolved name. The dashboard can use a canonical
HTTPS handoff; peer CA onboarding uses the local HTTP name. A loopback-only
front door does not produce a peer trust link. CA status reads the service's
storage environment and prefers the running admin API's certificate.

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

`GET /1.0` reports `app_count` and `heartbeat_ttl`. `serve` heartbeats at one third of it,
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

## macOS Local Network privacy

macOS 15 and later subject a user launchd agent to Local Network
privacy. Daemons, processes running as root, and tools run from Terminal
(with their children) are exempt; the user service that `janus autostart`
installs is not.

Symptom: `/1.0/mdns` reports `janus.local` announced, TCP on ports 80 and
443 works by address, yet no device resolves the name, this machine
included. The kernel drops inbound multicast for the denied process
before it reaches the socket, so the edge log is silent and no drop
counter moves. `dns-sd -B _http._tcp` shows the `janus` instance on
interface 1 (loopback) only; a healthy edge shows it on the LAN interface
too.

Check: the policy is world-readable.

```bash
plutil -p /Library/Preferences/com.apple.networkextension.plist | grep -n -A20 '"Path" =>' | grep -E 'janus|DenyMulticast|MulticastPreferenceSet'
```

A rule with `DenyMulticast => true` for the executable's path is the
denial. `tccutil` does not manage this list, and `sudo rm` of the policy
file is refused.

Fix: System Settings → Privacy & Security → Local Network, turn on the
row for the janus executable, then `janus restart`. The restart is
required: a process keeps the decision it received when it started, so
an edge that was running when the row was turned on stays deaf until it
is restarted. An allowed process that still hears nothing after a reboot
is a known macOS defect; turning the row off and on, then restarting the
edge, clears it.

Replace the binary by writing a new file and renaming it over the old
one, as `install.sh` and `make install` do. Overwriting the executable in
place (`cp` onto the existing file, `codesign -f` on it) keeps the inode
whose signature macOS has already cached: every later launch of it is
killed outright (exit 137), and an edge already started from it has no
identity macOS can match, so no Local Network rule is ever created for
it.

On macOS the installers lay down `~/Applications/Janus.app` for a user and
make `$BIN/janus` a symlink into it; `janus autostart` registers the
bundle's executable, so the row reads "Janus" with the logo and the
permission prompt names it. Root keeps the bare binary, exempt as a daemon.
A Mac upgraded from a bare install keeps the old row for the bare binary,
inert; `janus restart` re-registers the item onto the bundle, the "Janus"
row appears at that start, turn it on once and restart again. Contract:
[macos-app-bundle](../20260920-001500-macos-app-bundle.md).

Identity: the row is named by the executable's code-signing identifier
and keyed with its path and build UUID. `install.sh`, the release
archives, and `make install` (`scripts/release-install.sh`) sign every
janus binary as `com.github.shreeve.janus`, on the staged copy before it
is renamed into place, so the name and its grant survive upgrades. A
binary built some other way is `a.out` until it is signed the same way:

```bash
codesign -s - -f -i com.github.shreeve.janus bin/janus
```

Do not rename an installed binary: each name creates another rule for
the same build UUID, macOS may then resolve the process to none of them,
and the edge stays deaf under every name until a freshly built binary
replaces it. Rows for deleted executables cannot be removed from the
list; they are inert. The investigation record and the one-pass setup for a
new Mac: [20260919-225500-macos-local-network-privacy.md](../20260919-225500-macos-local-network-privacy.md).
