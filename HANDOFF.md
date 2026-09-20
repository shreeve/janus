# HANDOFF.md — Getting Up to Speed in This Repository

Read this first, then the documents it points to. It says what Janus
is, how work moves through this repository, what a newcomer trips on,
and where the current state of each area is recorded. It states
present facts; `git log` holds the history. State snapshot taken
2026-09-20 at tag `v1.18.0`; anything dated below is verified as of then.

## Reading order

1. [AGENTS.md](AGENTS.md) — the operating rules. Ten numbered rules,
   the capability table, the cascade model, and "When adding a
   capability". Every rule is enforced by a gate or a reviewer. Rule 10
   (Go, Rip, and shell only — no python, not even in a heredoc) and
   rule 9 (plain git, no AI attribution) are the two most often
   violated by a newcomer.
2. [README.md](README.md) — what Janus is and is not, the capability
   order, build and run, the service verbs, exposure modes, prebuilt
   releases, JSON config, and the source layout table.
3. [docs/README.md](docs/README.md) — the index that separates
   contracts (what the code implements against) from measurements,
   tutorials, design history, and reviews. Every `docs/` file has a
   row there.
4. [docs/20260719-002000-pool-protocol.md](docs/20260719-002000-pool-protocol.md)
   — the Janus↔tenant protocol: registration, doorbell, ring,
   heartbeats, never-stale. Read before touching `apps.go`,
   `dataplane.go`, or `ring.go`.
5. The capability page for the area in hand (table below), then
   [docs/20260907-000352-exposure-modes.md](docs/20260907-000352-exposure-modes.md)
   for anything that touches where the edge listens, and
   [docs/operators/index.md](docs/operators/index.md) for command
   defaults, reload survival, and the migration procedure an older
   installed Caddyfile needs.
6. [CHANGELOG.md](CHANGELOG.md) newest-first, and
   [TODO.md](TODO.md) for open product work.

## What Janus is, in one paragraph

Janus is a Caddy module plus the `janus` binary that embeds it (stock
Caddy, the module, and the Route 53 DNS provider in one executable).
Cold Caddyfile directives decide which hosts are admitted and which
capabilities each site has; a hot `/1.0` control API over a unix socket
(or a token-gated TCP listener) lets a running application register
its name, hosts, and worker unix sockets, heartbeat, and publish into
its own WebSocket hub. Registry, data plane, and hub state live in
pooled process state that survives `janus reload`; nothing is persisted,
so a restart empties the registry and tenants re-register. Rip Sites
(`packages/sites` in the sibling `rip` repository) is the tenant that
drives most decisions; MedLabs is the application behind them.

## The repository in one screen

```
*.go                the module, one flat package `janus` (see README "Layout" for the
                    file-by-file table): app.go (process-wide app), handler.go (site
                    handler), caddyfile.go (directive grammar), state.go (pooled state),
                    apps.go + dataplane.go + ring.go (registry and proxying), hub_*.go,
                    mdns*.go + mdns_trust.go (LAN presence, status front door, /trust),
                    auth*.go, files_*.go, sendfile*.go, browse_*.go, access*.go,
                    exposure.go (wan refusals), tls_permission.go (on-demand TLS gate),
                    control*.go (/1.0 listeners and surfaces)
*.html              embedded pages: mdns.html (status dashboard), trust.html,
                    trust_safari.html, auth.html — self-contained, no external resources
cmd/janus/          the binary: main.go (verb table), service_*.go (autostart/start/stop/
                    restart/status/logs per platform), exposure*.go + scope.go + firewall*.go
                    + listeners*.go (modes, pf anchor, socket watch), serve.go, apps.go,
                    config.go, control_client.go
internal/strictjson repeated-key-rejecting JSON decoder used by every control request
testkit/            Go test-support program (fixtures, precompressed sidecars, WS driver)
                    that test.sh drives
tests/acceptance/   the sourced case groups test.sh runs, one file per capability group
test.sh             the acceptance driver: one foreground owner, capability order, tally
bench/              the measurement harness (measures, never gates); raw results go to docs/
docs/               timestamped contracts, measurements, reviews; docs/counter/ and
                    docs/operators/ are the two living tutorials
certs/              the intentional public *.ripdev.io wildcard pair (127.0.0.1) for tests
Caddyfile           working multi-site cascade config; Caddyfile.minimal (operator start),
                    Caddyfile.example (every knob) both validate standalone
install.sh          curl | bash installer for the prebuilt archives; scripts/ packages them
scripts/            package-release.sh (archives), release-install.sh (the archive installer and
                    make install), bundle-macos.sh (Janus.app from a built binary)
.github/workflows/  check.yml (Go on ubuntu-24.04 + macos-15, then foreground acceptance on
                    macos-15) and release.yml (checks, then five archives, then the Release)
```

Capabilities, in landing order (the order never changes in docs or
`test.sh`):

| # | Capability | Scope | Contract |
| --- | --- | --- | --- |
| 1 | ping | site (cascades) | [capability-ping](docs/20260718-204255-capability-ping.md) — also defines the cascade rules |
| 2 | control | process-wide | [capability-control](docs/20260718-203749-capability-control.md) |
| 3 | hub | site (cascades) | [hub-design](docs/20260720-162350-hub-design.md) |
| 4 | mdns | process-wide | [capability-mdns](docs/20260722-034619-capability-mdns.md) — `janus.local`, app `.local` names, the dashboard front door, `/trust` |
| 5 | auth | site (cascades) | [capability-auth](docs/20260728-160734-capability-auth.md) |
| 6 | files | site (cascades) | [capability-files](docs/20260730-202700-capability-files.md), [precompressed](docs/20260805-020944-capability-files-precompressed.md) |
| 7 | sendfile | always on, no config | [capability-sendfile](docs/20260801-020600-capability-sendfile.md) |
| 8 | browse | site (cascades) | [capability-browse](docs/20260801-042700-capability-browse.md) |
| 9 | access log | per-site `log` | [capability-access-log](docs/20260801-081600-capability-access-log.md) |

Exposure modes (`localhost` | `lan` | `wan`) are not a tenth capability;
they are the binary's job (where the service edge listens, and the pf
anchor or exact binds that hold it there) plus one module rule: in
`wan`, every janus site refuses `<name>.local`, `<name>.localhost`,
`localhost`, `via.rip`, and `<name>.via.rip` with 421, mints no
certificate for them, and mdns announces nothing.

## How work moves

- **Edit loop.** `go test ./...` is seconds and covers parsing,
  cascade, and internals. `make test` builds `bin/janus`, runs the unit
  gate, then `./test.sh` — the acceptance suite, which needs the
  sibling `../rip` checkout (or `RIP_ROOT`), free fixed ports, and a
  **foreground** shell: its fixture processes die when the parent
  detaches, and this has cost real debugging time. `test.sh` groups run
  ping, control, apps, data, heartbeat, tls, hub, tenant, mdns, auth,
  files, sendfile, browse, access; the cases are the sourced files in
  `tests/acceptance/`.
- **Before tagging, run `go test -race ./...` locally.** The release
  workflow runs the full check (unit, vet, race on two platforms, then
  acceptance on macos-15, which needs port 443 and runs under sudo).
  A gate that fails on the tag blocks the release; fix, then move the
  tag — a tag with no published release may be deleted and recreated.
- **Landing.** Branch from main, open a pull request with `gh`, let
  `Checks` run (the Go matrix and the foreground acceptance), then
  merge as a TRUE MERGE commit and delete the branch locally and on
  origin. Only `main` remains between rounds. Commit messages are one
  imperative sentence (an `area:` prefix is common, not required) with
  no AI attribution of any kind.
- **Releasing.** Add the version's entry to `CHANGELOG.md` (dated),
  bump the two README pins and the `install.sh` pin, merge, then push
  an annotated `vX.Y.Z` tag on the merge commit. `release.yml` builds
  osx-arm64, linux-amd64, linux-arm64, windows-amd64, windows-arm64,
  smoke-tests each archive, and publishes the GitHub Release with
  checksums. `install.sh` installs the latest release into
  `~/.local/bin` (`/usr/local/bin` as root).
- **The acceptance suite pins a Rip revision.** `check.yml` checks out
  `shreeve/rip` at a fixed SHA as the real tenant. Moving that pin is a
  deliberate act: run the whole suite against the new revision and
  record it in the same change.
- **Claims are verified.** Reproduce a defect before changing code.
  Every performance number lands in the ledger
  ([rip-server-performance](docs/20260719-165500-rip-server-performance.md))
  with its raw provenance as `docs/*-bench-raw-*.txt`, never edited.
- **Timeless wording.** Code, comments, README, capability pages, and
  the operator tutorial state present facts: no "previously", "now",
  "legacy", no transition narration. Timestamped design and
  measurement records keep their point-in-time context; that is the
  one exception, and `docs/README.md` labels it.
- **New behavior is a capability.** Contract doc first, adversarial
  review, revise, implement, pin in both test layers, measure. Fix,
  harden, and measure need no contract; a new directive or a new
  `/1.0` surface does.

## What a newcomer trips on

- **Cold and hot never mix.** The Caddyfile is the admission gate and
  the capability switchboard. `/1.0` is the registry. No JSON write
  changes what a site admits, and no directive registers an app.
- **Cascade is explicit and asymmetric.** Process-wide directives
  (`control`, `mdns`) are legal only in the global `janus { }` block; a
  site-level occurrence is a parse error. Site-scoped ones (`ping`,
  `hub`, `auth`, `files`, `browse`) inherit the global default,
  explicit `off` beats inherited `on`. Every capability page states
  "Cascades: yes/no".
- **Reject loudly.** Unknown tokens, illegal modes, repeated JSON keys
  (`internal/strictjson`), unknown hosts on the data plane (404), and
  local names in `wan` (421) all fail with a precise message. Never
  add tolerance; never repair silently.
- **Pooled state survives reloads, not restarts.** `caddy.UsagePool`
  holds the registry, data plane, hubs, mdns advertiser, access
  bridge, and auth sessions across `janus reload`. A restart empties
  everything; tenants heartbeat and re-register on their own.
- **The seed is written once.** `janus autostart` writes the service
  Caddyfile only when none exists. An installed edge keeps its
  Caddyfile through binary upgrades, so an older config can lack what
  the current seed enables (bridge hub, precompressed files, `format
  janus` access logs, `grace_period 10s`). The operator tutorial has
  the merge procedure. A symptom of a stale seed: a Rip manager
  logging `Janus change publish failed (409)` forever, because hub is
  not enabled for any of its sites.
- **Local names belong to a machine you sit at.** `*.localhost`,
  `*.local`, and `via.rip` / `*.via.rip` (public DNS pins them to
  127.0.0.1) are served in `localhost` and `lan`, refused in `wan`. The
  trust page (`http://janus.local/trust`, `/trust/ca.crt`,
  `/trust/ca.mobileconfig`) rides the mdns front door over plain HTTP
  on purpose: a phone cannot validate the local CA before installing
  it. TODO.md's HTTPS-only item must preserve that bootstrap.
- **The front door answers for Janus's own names only.** `janus.local`
  (or the configured, canonical, or conflict-renamed name) gets the
  dashboard; an app's `.local` host on the shared plain-HTTP port gets
  a 308 to HTTPS. Site auth gates (`gate /`) run before the dashboard.
- **macOS Local Network privacy gates the agent.** Since macOS 15 a user
  launchd agent needs its own Local Network grant; daemons, root, and
  Terminal children are exempt. A denied edge announces `janus.local`
  and never hears a query, from anyone, with nothing logged: the kernel
  drops inbound multicast for that process. `dns-sd -B _http._tcp` then
  shows `janus` on interface 1 (loopback) only. Fix: System Settings →
  Privacy & Security → Local Network, turn on the janus row, `janus
  restart`. The row is named by the code-signing identifier: `install.sh`,
  the archives, and `make install` sign every binary as
  `com.github.shreeve.janus` (an unnamed Go binary is `a.out`). On
  macOS they install `~/Applications/Janus.app` and link the command
  into it, so the row reads "Janus" with the logo and `janus autostart`
  registers the bundle's executable. Never
  re-sign an installed binary under another name, and never `cp` over
  it in place; replace it with a new file renamed into place. The
  policy lives in `/Library/Preferences/com.apple.networkextension.plist`
  (world-readable, `plutil -p`), not in TCC. `janus run` from a shell
  cannot reproduce it. Full account and one-pass setup:
  [docs/20260919-225500-macos-local-network-privacy.md](docs/20260919-225500-macos-local-network-privacy.md).
- **macOS binds the wildcard.** Low ports without root force the
  wildcard socket, so a pf anchor (`/etc/pf.anchors/janus`) is what
  scopes `localhost` and `lan`; `sudo janus firewall` is the single
  root step, and the edge refuses to serve those modes until the anchor
  is in place. Linux binds exact addresses with `cap_net_bind_service`
  and needs no firewall. Windows builds but has no exposure modes.
- **The exposure watch on macOS.** `/dev/fd` is listed by name, not
  stat'ed (stat fails there); the watch compares the edge's own sockets
  to the stored mode every few seconds and stops the edge, loudly,
  rather than serve wider than allowed.
- **Reading the running edge.** `janus status --json`, `janus apps
  --json`, `janus logs -f`, and `curl --unix-socket <control.sock>
  http://janus/1.0` are the diagnostic set. State lives under the state
  directory (`scope.json`, `run/janus.sock`, `run/admin.sock`,
  `log/janus.log`); `status --json` prints every path.
- **Embedded HTML means rebuild.** `mdns.html`, `trust*.html`, and
  `auth.html` are `go:embed`; a probe edge shows an edit only after
  `make janus` and a restart.
- **Tests that touch the advertiser wait for quiet.** mDNS withdrawals
  and re-announcements complete asynchronously; acceptance cases
  baseline with `mdns_wait_quiet` before asserting no-flap, and Go
  tests join their watch goroutines. Race-detector failures here have
  broken a release gate before.

## Environment

- Go (current stable; `go.mod` says 1.25.1, CI uses
  `go-version-file`). `make janus` builds `bin/janus` (gitignored) and
  asserts the module is linked. Caddy is pinned at 2.11.4 with explicit
  security overrides in `go.mod`.
- Acceptance needs `bun` and the sibling `../rip` checkout
  (`Data/Code/rip`) as the real tenant, `oha` only for `bench/`.
- Sibling repositories referenced from here: `rip` (Rip Sites is the
  tenant; its `packages/sites/rip.caddy` is the drop-in an app ships
  into `~/.config/janus/sites/`), `duckdb-harbor` (whose service verbs
  Janus's mirror), `medlabs` (the production tenant).
- Installed edges: this machine (macOS 27, `Janus.app`, `lan`) and the
  `live` host (Linux, user systemd unit `janus.service`, `wan`); live's
  seed is current (hub bridge on). An edge's own Caddyfile, sites drop-ins,
  and CA live under `~/.config/janus/` and `~/.local/state/janus/`.

## State of the tree

Verified 2026-09-20:

- **Release.** `v1.18.0` (2026-09-20) is the latest tag and sits on
  main's head. `go test ./...` passes on this machine (module,
  `cmd/janus`, `internal/strictjson`). No open pull requests.
- **Pins.** README's `go install …@v1.18.0` and `bash -s v1.18.0` lines and
  `install.sh`'s header example name the latest tag; a release branch
  bumps all three with the changelog entry. `install.sh` itself
  resolves the latest release at run time.
- **Recent work (1.14.1 → 1.18.0).** A stewardship round
  ([review](docs/20260910-100643-stewardship-review.md),
  [resolution](docs/20260910-120000-stewardship-resolution.md)) fixed
  the reproduced defects across the service commands, control
  requests, files, and status, split `cmd/janus/service.go` into
  paths/lifecycle/status modules, split acceptance into
  `tests/acceptance/`, and put the Go matrix and acceptance on pull
  requests. Then: mdns advertises only the selected LAN address and
  interface (1.15.1); the shared front door 308-redirects other hosts
  to HTTPS (1.15.2); site auth gates run before the dashboard, and
  auth-enabled shared HTTP sites redirect dashboard requests to HTTPS
  (1.16.0); `janus restart` always finishes, with a bounded stop and
  SIGKILL, and the seed carries `grace_period 10s` (1.17.0); macOS
  installs `Janus.app` signed as `com.github.shreeve.janus`, after a
  launchd agent filed as `a.out` was silently denied inbound multicast
  by Local Network privacy
  ([record](docs/20260919-225500-macos-local-network-privacy.md),
  [contract](docs/20260920-001500-macos-app-bundle.md)) (1.18.0).
- **Exposure modes** (1.13.0–1.14.1) are the front-door contract in
  [exposure-modes](docs/20260907-000352-exposure-modes.md); the wan
  refusals of local name families are in `exposure.go` and pinned in
  `exposure_test.go`, `mdns_exposure_test.go`, and
  `tls_permission_test.go`.
- **Open product work.** `TODO.md` holds one item: HTTPS-only public
  transport with HSTS (no `includeSubDomains`, no `preload`),
  preserving plain-HTTP LAN discovery and `/trust`. The stewardship
  resolution names two ideas that would need their own contracts: a
  richer diagnostic command and an automated config-upgrade preview
  for older seeds. Neither is started.
- **Known platform gaps.** Windows: builds and runs, exposure verbs
  refuse. Linux `lan` after a DHCP move needs `janus mode lan` again to
  store the new address (the unit keeps retrying until then).

## When blocked

From AGENTS.md: a missing decision gets options with a recommendation,
never a silent choice; a gate diverging inexplicably is a stop and
report; an acceptance criterion that seems wrong gets a proposed
change, never a quietly weaker test. Product decisions are the
owner's; the verdict "land" means merge as a true merge and delete the
branch on both sides.
