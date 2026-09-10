# Janus stewardship review — 2026-09-10

Review of commit `50b8e36` (v1.14.1), its working configuration, and the
adjacent Rip Sites integration. This is a point-in-time review, not a new
behavior contract. The changes accompanying it correct documentation;
the implementation findings below remain open.

Follow-up: the [resolution record](20260910-120000-stewardship-resolution.md)
tracks the subsequent fixes and verification; the findings below retain
their original review context.

## Assessment

The core is in a good place. Keep the Caddy module, cold admission / hot
registry split, pooled process state, and capability boundaries. A rewrite
or broad framework extraction would discard useful, tested behavior.

The best next work is a focused hardening pass. The newer CLI repeats
control-client and configuration-loading logic; these repetitions already
produce observable differences. The file service also misses a nonblocking
open safeguard that sendfile already has. These deserve attention ahead of
speculative proxy or hub optimizations.

POLS is strong in the registry and ordinary routing: unknown hosts fail,
empty upstreams are distinct from dead registrations, retries distinguish
unaccepted requests from potentially consumed bodies, and failed reloads
preserve committed state. It is less consistent in the operator layer:
the same env file has different semantics across commands, an accepted TTL
change does not become effective on reload, and `serve` can announce a
registration after failing to apply the site needed to reach it.

## Prioritized implementation findings

### 1. Control requests accumulate idle connections — high priority

`cmd/janus/service.go:controlClient` constructs a new `http.Transport` for
every call, with no idle timeout. `cmd/janus/serve.go:controlDo` uses it on
every heartbeat and neither reuses nor closes its idle connections. Closing
the response body returns a connection to that abandoned transport's pool;
it does not close the socket. The control server also has no idle timeout.

**Reproduced:** twelve sequential requests to an isolated Unix control
listener accepted twelve connections, all twelve still open afterward.
This affects both the long-running `janus serve` process and the edge it
talks to. The existing tests do not exercise sustained client lifetime.

**Fix:** resolve a control endpoint once per command, reuse an owned client
and transport, and close idle connections at command shutdown. Give the
transport a finite idle timeout. Share this implementation with
`controlGet`, `probeControl`, `fetchApps`, and `trustURL`. Preserve the
existing identity check before falling back to loopback TCP. Propagate
response-body read errors; `controlDo` currently discards them.

### 2. Files/browse can block indefinitely opening a FIFO — high priority

`browse_serve.go:serveBrowseRootsPrecompressed` calls `root.Open` before
checking the descriptor's type. Directory indexes, the shell path, and
precompressed sidecars use the same open-then-stat pattern
(`files_serve.go:serveAbsoluteFile` and `serveOpenedFileFromRoot`). A FIFO
with no writer blocks inside open, before the regular-file check runs.

**Reproduced:** a request for a FIFO inside an allowed root did not return;
canceling the request did not release it. Opening a writer released the
probe. An existing FIFO in a served tree is required; this is not a claim
that an HTTP client can create one. Repeated requests can nevertheless
retain handlers and descriptors when such an entry exists.

**Fix:** introduce a confined, nonblocking root-relative open and validate
the opened descriptor. Keep `os.Root` confinement; a preliminary `Lstat`
alone leaves a replacement race. `sendfile_open_unix.go` already uses
`O_NONBLOCK`, so reuse that policy without replacing confined opens with
unconfined path opens. Test canonical files, indexes, shells, sidecars, and
replacement races.

### 3. Env-file behavior differs across commands — medium priority

Startup delegates to Caddy's env loader; reload/validate use Janus's
`cmd/janus/scope.go:loadEnvFile`. The latter implements a second grammar
and overwrites existing environment values. The pinned Caddy loader keeps
existing values and supports quoted multiline values.

**Reproduced:** with a value present in both the shell and env file,
Caddy selected the shell value while Janus's loader selected the file
value. Caddy accepted a quoted multiline value that Janus rejected as
`line 2: not KEY=value`.

**Fix:** define one grammar and precedence rule, then route run, adapt,
validate, reload, restart validation, and internal reloads through it.
Preserve the reserved `JANUS_BIND` rule consistently. Include Caddy's
storage-path initialization semantics when consolidating this code.

### 4. Route inventory walkers lose parent host constraints — medium priority

`hub_config.go:collectHubRoutes` and
`auth_config.go:collectAuthRoutes` replace inherited host patterns when a
child supplies its own. Caddy evaluates nested route constraints together.
`browse_state.go:collectBrowseRoutes` already carries host alternatives
and intersections instead.

**Reproduced:** an outer `a.test` route with an inner `*.test` route
produced a hub table that matched `b.test`, and an auth inventory containing
`*.test`. Actual HTTP routing remains constrained by Caddy; this probe
does not demonstrate an authentication bypass. It demonstrates incorrect
metadata used for hub policy resolution and auth reconciliation.

**Fix:** share route traversal and host-constraint composition across hub,
auth, browse, and mDNS. Retain capability-specific consumers. Explicitly
cover matcher alternatives, nested intersections, and the supported limits
of inventories derived from hostnames alone.

### 5. Heartbeat TTL accepts an ineffective reload — medium priority / POLS

`App.Provision` acquires existing pooled state; `newJanusState` captures
the TTL only on first construction. No reload comparison rejects a change.

**Reproduced:** a second provisioned generation requested 30 seconds but
successfully retained a registry TTL of 15 seconds. This behavior is
intentional in the source comment, but was absent from the README.

**Fix:** retain the restart requirement and reject changed effective TTLs
with a precise restart-required error, or deliberately design a safe live
retune. Publish the effective TTL through `/1.0`. `janus serve` currently
uses a fixed five-second heartbeat and cannot adapt to a shorter TTL.
The README now documents the existing behavior.

### 6. Smaller operator inconsistencies worth batching

- `serveCommand` ignores a `readScope` error, and `serveDir` ignores another;
  most other exposure commands reject corrupt scope state. `serveDir` also
  continues after failure to reload a newly written localhost site. Fail
  before registration when required scope or site setup fails, and verify
  usable routing before announcing success.
- Service-config recognition uses raw path-string equality in several
  command wrappers. Equivalent relative paths or symlinks can select
  different env/default/watch behavior. Centralize target identity and
  distinguish an explicitly unmanaged config from an equivalent spelling
  of the installed config.
- The ordinary control JSON decoder accepts repeated keys with last-value
  wins. A probe accepted two `name` fields and selected the second; hub
  frames and scope state reject duplicate keys. Adopt one documented
  duplicate-key policy, preserving each endpoint's null/absence semantics.
- `janus apps` prints `Hosts`, which is empty for site-pattern registrations;
  its reduced record includes the pattern but its table does not display
  it, and omits aliases. Show the claims the operator actually needs.

## Cleanup and optimization boundaries

| Area | Recommendation |
| --- | --- |
| CLI control access | One endpoint/client abstraction with connection ownership, identity checks, errors, and timeouts. Highest-value extraction. |
| CLI config execution | One resolved config/env/scope context; stop invoking sibling Cobra commands as the shared execution layer. |
| Caddy route inventories | One traversal that retains constraints; separate hub/auth/browse/mDNS projections. |
| File opening | One explicit policy for confined regular files and directories, including nonblocking type checks. |
| `service.go` | Split its 1,467 lines into paths/seeding, lifecycle commands, status/CA inspection, and control access after the seams above are defined. |
| `test.sh` | Split its 4,057 lines into sourced capability groups while retaining one foreground owner, cleanup, tally, and capability order. Put all writable fixture paths in a per-run directory. |
| Install paths | `make install` removes the old binary before copying, while the release installer stages and atomically renames. Unify the replacement primitive and Linux capability handling; keep source-build destination policy explicit. |
| JSON helpers | Share bounded decoding primitives, but retain clear endpoint validation. Avoid replacing small field-specific validators with a generic schema framework. |

Do not merge sendfile's response transformation with registered-file
routing merely because both serve bytes. Their authorization and header
ownership differ. Likewise, keep generation-owned streams/renderers
separate from pooled registry/hub/session state. That complexity protects
reload correctness.

The proxy already has a buffer pool, reusable per-socket proxies, atomic
health counters, per-app selection locks, bounded capacity waits, and
published concurrency support. Hub fan-out and the no-subscriber access
path already have measurement history. No new throughput claim or speedup
is established by this review.

If workloads justify more profiling, inspect site-pattern lookup and its
per-request directory check, full registry/status snapshots, and worker
selection as app/pool counts grow. These are candidates, not demonstrated
bottlenecks. Any cache must preserve immediate host-directory revocation
and registry lifecycle behavior.

## Features available but not enabled in this installation

The installed Caddyfile and its imported sites were inspected read-only.
This describes configuration and integration support, not traffic usage.

| Feature | Evidence and useful next step |
| --- | --- |
| Hub | The installed global block has no hub default; local and localhost routes explicitly disable it, and the inspected other sites do not enable it. Rip's manager registers bridges and the worker implements the bridge. Enable bridge mode only on the sites intended to use live watch/realtime features, remembering that an explicit site `off` beats a global `on`. |
| Precompressed files | Installed config has browse, which implicitly enables files, but no `files { precompressed }`. Rip ships `.br` assets and generates `bundle.json.br` (`manager.rip`, `SHIP_FILES` and bundle publication). This is a concrete opportunity to use already-produced artifacts. Verify `Content-Encoding`, `Vary`, validators, and actual bytes before making a performance claim. |
| Edge auth | No enabled auth gate was found. It is available for private tools without application login; whether it is wanted is an operator policy choice. |
| Browse renderers | Browse uses its default theme with no configured extension renderer. Useful for selected document roots if previews are wanted; do not add renderer processes merely to exercise a feature. |
| Access observation | Rip already reads app-scoped `/access?after=…`, and the Rip/Medlabs drop-ins configure `format janus`. The inspected `.local` and `.localhost` sites lack access-log blocks, so observation is not uniform across host families. Decide whether those requests should also be recorded. |

Other integration features are already present in Rip: published worker
concurrency, doorbells, `X-Sendfile`, `Rip-Mark`, process leases for
until-restart browsing, and directory-gated site registrations. They should
not be presented as unused capabilities just because the minimal demo does
not show them. The installed config already uses `permission janus` for
in-process certificate admission and mDNS for LAN discovery.

The current `seedConfig` enables hub and precompressed files, while this
installation predates those defaults. Existing configs are intentionally
preserved. This is configuration migration debt, not evidence that the
binary lacks the features. No installed config was changed by this review.

## Improvements beyond cleanup

1. **An effective-state diagnostic.** Extend status or add `doctor` to show
   configured and effective capabilities by host, effective TTL, running
   binary version, control endpoint, and why a registered app cannot be
   reached. Distinguish desired exposure addresses from observed listeners
   and independently verified firewall state. Current status reports the
   invoked binary's version, not a version read from the running edge.
2. **A config-upgrade preview.** Compare operator-owned config with current
   seed defaults and propose a reviewable patch. This would surface the
   currently missing hub/precompressed defaults without overwriting edits.
3. **Lifecycle tests for the CLI.** Add repeated-heartbeat connection bounds,
   env parity, failed site setup, equivalent config paths, and transactional
   exposure failure cases. The capability acceptance suite is excellent
   evidence for the module; it is not a complete service-command suite.
4. **Earlier CI feedback.** The workflow runs on tags/manual dispatch.
   Add PR/push Go/vet/race checks and run platform-specific CLI tests on
   macOS and Linux. Keep foreground acceptance as a separate integration
   gate. Its external Rip checkout should have explicit tested provenance.
5. **A compact living operator contract.** Add one command/default/scope
   matrix and one reload-survival matrix. Keep dated designs and raw
   measurements intact. The existing TODO for redirect/HSTS work must
   explicitly account for the plain-HTTP LAN trust bootstrap.

## Documentation corrected with this review

- Replaced removed `janus-auth-hash` examples with `passhash` in both
  working and example Caddyfiles.
- Corrected the README's service control socket to `run/janus.sock`.
- Replaced the claim that every other subcommand is stock Caddy with the
  actual Janus commands, wrappers, and unsupported package-management verbs.
- Updated the package overview from two registered modules to four.
- Qualified reload-survival claims with committed policy/host changes.
- Clarified tenant process ownership versus edge services and renderers.
- Documented the restart-only heartbeat TTL behavior.
- Removed an unqualified static-linkage claim; a single executable is
  what the build guarantees.

## Verification

The Go suite, race suite, vet, build, both example-config validations, and
152 capability acceptance checks passed during the review. Execution was
local to macOS; Linux and Windows service behavior was reviewed in source,
not exercised on those operating systems. Six temporary characterization probes reproduced
the FIFO, connection-lifetime, env, host-table, TTL, and duplicate-key
findings; the probe sources were removed from the working tree afterward.
Their local sources and output are retained in
`/tmp/janus-review-evidence/` for this session. No production service,
firewall, or installed configuration was changed.

Recommended order: fix connection lifetime and confined file opens;
unify env/config execution and route inventories; make TTL and `serve`
failures explicit; then adopt the already-supported capabilities desired
in the installed configuration. Profile only after those correctness and
operational issues are resolved.
