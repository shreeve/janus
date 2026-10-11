# Metrics and alerting: Prometheus exposition and a not-running signal

Status: **Draft — awaiting owner review.** A new read-only `/1.0` surface
(`GET /1.0/metrics`), Janus collectors in Caddy's metrics registry, a
start-refusal record, and `janus status --check`. Nothing here is
implemented.

## Why it exists

Janus counts a great deal and tells no one. `/1.0/hub`, `/1.0/auth`,
`/1.0/access`, `/1.0/webtransport`, and `/1.0/mdns` answer JSON counters to
whoever asks, and nothing asks. There is no Prometheus exposition and no
alert path, so a capacity 503 storm, a slow-consumer close wave, or a login
spray is visible only to an operator already looking.

Worse, metrics cannot report an edge that is not running. An installed
edge refused to start for five days because its pf anchor was missing (on
the owner's machine, a macOS update on 2026-10-05 rewrote `/etc/pf.conf`
without the janus lines). The edge correctly refused to serve unscoped,
the supervisor restarted it into the same refusal over and over, and
nobody knew. A HIPAA-covered
service needs both halves: numbers while it runs, and a signal that does
not depend on it running.

## Scope

- Surface: process-wide. No Caddyfile directive: exposition is always on
  and read-only, and rule 3 forbids a decorative toggle. Cascades: **no**.
- Exposition rides two existing doors:
  1. **Caddy's registry.** Each configuration generation registers one
     Janus collector in `ctx.GetMetricsRegistry()`, the per-config
     registry Caddy's admin `/metrics` route and stock `metrics` handler
     already serve. An operator who uses Caddy's metrics gets Janus's
     families beside `caddy_http_*` and `caddy_config_last_reload_*` with
     no new configuration.
  2. **`GET /1.0/metrics`** on every control listener, with that
     listener's existing Bearer posture, serving the same registry through
     `promhttp` (text format, OpenMetrics when the scraper asks for it).
     This is the remote scrape path: the service edge's admin API is a
     unix socket on purpose, and a `public` control listener is already
     TLS and token-gated.
- Not-running signal: a start-refusal record the binary writes, and
  `janus status --check` reading it (below).
- Dependency: `github.com/prometheus/client_golang`, already in the module
  graph through Caddy; it moves from indirect to direct.

## Collector model

The collector reads at scrape time; it adds nothing to the proxied request
path. Every number below already exists as an atomic, a registry snapshot,
or a hub snapshot, except those marked **new**, and every new counter sits
on a path that is already a failure, a ring, a reap, or a control call:
never on the 200-path of a proxied request.

- Registry figures come from the same snapshot `GET /1.0/apps` builds
  (registry read lock, no filesystem work).
- Per-upstream `inflight` and health are the `upstreamState` atomics,
  summed per app at scrape.
- Pooled counters (auth, hub, access, webtransport, mdns) survive reloads,
  as the JSON surfaces already promise. Per-app counters live with the
  registration and restart from zero when the app re-registers, which
  Prometheus reads as a counter reset.
- One collector per generation, registered in that generation's registry,
  reading pooled state: a reload never double-registers and never
  double-counts.

## Metric families

Prefix `janus_`. Types: C counter, G gauge.

### Process

| Name | Type | Labels | Source |
| --- | --- | --- | --- |
| `janus_build_info` | G (=1) | `version`, `caddy` | `version.go` |
| `janus_start_time_seconds` | G | — | process start |
| `janus_exposure_info` | G (=1) | `mode` (`localhost`\|`lan`\|`wan`) | `SetExposureScope` |
| `janus_firewall_verified` | G (0/1) | — | macOS verifier status ([draft](20261010-210400-macos-pf-verifier.md)); absent elsewhere |
| `janus_control_unauthorized_total` | C **new** | `listener` (`internal`\|`local`\|`public`) | `bearerAuth` 401 |

Reload success is Caddy's `caddy_config_last_reload_successful`; Janus does
not duplicate it.

### Registry and data plane

| Name | Type | Labels | Source |
| --- | --- | --- | --- |
| `janus_apps` | G | — | registry size |
| `janus_app_upstreams` | G | `app` | registered upstreams |
| `janus_app_upstreams_healthy` | G | `app` | `upstreamHealth` |
| `janus_app_inflight` | G | `app` | sum of `upstreamState.inflight` |
| `janus_app_heartbeat_age_seconds` | G | `app` | now − `heartbeatAt` |
| `janus_app_rings_total` | C **new** | `app`, `outcome` (`woke`\|`boot_error`\|`failed`\|`overflow`\|`client_gone`) | `awaitRing` result |
| `janus_app_capacity_holds_total` | C **new** | `app`, `outcome` (`admitted`\|`expired`\|`cap`) | `awaitCapacity` result |
| `janus_app_unavailable_total` | C **new** | `app`, `reason` (`empty`\|`ring_cap`\|`ring_waiters`\|`ring_failed`\|`ring_boot_error`\|`unhealthy`\|`busy`) | every data-plane 503 (`unavailable`, `allWorkersBusy`, boot-error forward) |
| `janus_app_upstream_failures_total` | C **new** | `app`, `kind` (`dial`\|`mid_response`) | `markUnhealthy` callers |
| `janus_apps_reaped_total` | C **new** | — | `sweepExpired` |
| `janus_apps_retired_total` | C **new** | `reason` (`unconfined`) | [confine draft](20261010-210000-files-sendfile-confine.md) |
| `janus_sendfile_rejected_total` | C **new** | `reason` (`malformed`\|`status`\|`open`\|`not_regular`\|`validator`\|`unconfined`) | `applySendfile` failures |
| `janus_tls_ask_total` | C **new** | `verdict` (`allow`\|`deny`) | `certificateAllowed` |

### Hub

| Name | Type | Labels | Source |
| --- | --- | --- | --- |
| `janus_hub_connections` | G | `app` | hub snapshot |
| `janus_hub_frames_in_total` | C | `app` | `hubCounters.framesIn` |
| `janus_hub_deliveries_total` | C | `app` | `deliveries` |
| `janus_hub_publishes_total` | C | `app` | `publishes` |
| `janus_hub_rejected_frames_total` | C | `app` | `rejectedFrames` |
| `janus_hub_closes_total` | C | `app`, `reason` (`slow`\|`ping`) | `slowCloses`, `pingCloses` |
| `janus_hub_bridge_total` | C | `app`, `result` (`sent`\|`failed`\|`dropped`\|`garbage`) | `bridge*` |

### Auth

Process totals only: auth counters are kept process-wide, and a site label
would be a host.

| Name | Type | Labels | Source |
| --- | --- | --- | --- |
| `janus_auth_sessions` | G | — | `sessionCount` |
| `janus_auth_logins_total` | C | `result` (`ok`\|`failed`) | `logins`, `loginFailures` |
| `janus_auth_throttled_total` | C | — | `throttled` |
| `janus_auth_banned_total` | C | — | `banned` |
| `janus_auth_sessions_ended_total` | C | `cause` (`signout`\|`revoked`\|`reload`\|`idle`\|`absolute`) | `signouts`, `revoked`, `reloadRevoked`, `expired`, and the [absolute-lifetime draft](20261010-210300-auth-session-lifetime.md) |

### WebTransport

Process totals only: a route's identity is `host/path`.

| Name | Type | Labels | Source |
| --- | --- | --- | --- |
| `janus_webtransport_sessions` | G | — | `sessionN` |
| `janus_webtransport_accepted_total` | C | — | sum of route `accepted` |
| `janus_webtransport_refused_total` | C | `reason` (`host`\|`path`\|`fronting`\|`method`\|`origin`\|`cap`\|`upstream`\|`settings`\|`ticket`) | process and route `refused*` |
| `janus_webtransport_datagrams_total` | C | `direction` (`in`\|`out`) | route sums |
| `janus_webtransport_bytes_total` | C | `direction` | route sums |
| `janus_webtransport_dropped_total` | C | `reason` (`queue`\|`oversize_in`\|`oversize_out`) | route sums |
| `janus_webtransport_upstream_refused_total` | C | — | route sums |
| `janus_webtransport_panics_total` | C | — | route sums |

### Access log and mdns

| Name | Type | Labels | Source |
| --- | --- | --- | --- |
| `janus_access_published_total` | C | — | `published` |
| `janus_access_dropped_total` | C | — | `subscriberOverflow` |
| `janus_access_streams` | G | — | opens − closes |
| `janus_access_write_timeouts_total` | C | — | `writeTimeouts` |
| `janus_mdns_announces_total` | C | — | `announces` |
| `janus_mdns_withdraws_total` | C | — | `withdraws` |

## Label policy and cardinality

Legal label names are `app`, `version`, `caddy`, `mode`, `listener`, and
the closed enums above (`outcome`, `reason`, `result`, `cause`, `kind`,
`direction`, `verdict`), whose values are constants in the code. A host,
path, user, session id, ticket, remote address, socket path, or file path
is never a label value. Usernames and hosts are PHI-adjacent at a covered
entity (a tenant label like `{site}.medlabs.health` names a customer), and
every one of them is attacker-driven cardinality.

`app` is the registered app **name** (the `[a-z0-9-]{1,63}` grammar), not
the id: the id changes on every re-registration and would churn series.
Two live registrations sharing a name sum into one series.

The per-app families emit at most **128** distinct `app` values, the first
128 names in sorted order; the remainder sum into `app="_other"`, and
`janus_metrics_apps_truncated` (G) reports how many were folded. A control
client that registers ten thousand apps costs the scraper 128 series per
family, not ten thousand.

## The not-running signal

Metrics answer while the edge runs. Three things cover the rest.

### 1. The start-refusal record

The binary writes `state/health.json` (mode 0644, atomic rename) whenever
it refuses to serve: a pre-start refusal (scope unusable, firewall missing
or unverified, `https_port` refusal, webtransport anchor mismatch), a
Caddy load or Start error, or an exposure-watch exit.

```json
{
  "state": "refused",
  "reason": "firewall: /etc/pf.conf does not load the janus anchor (run: sudo janus firewall)",
  "since": "2026-10-05T09:12:44Z",
  "last": "2026-10-10T08:01:03Z",
  "attempts": 43117,
  "version": "1.19.0"
}
```

`since` holds while consecutive refusals share a reason; `attempts`
counts them. A Start that completes rewrites the file as
`{"state":"serving","since":…}`. The file states one fact, the latest
attempt, and is never a log.

### 2. `janus status --check`

A flag on the existing verb, with monitoring-plugin exit codes (the
convention Nagios, Icinga, Sensu, and most agents read), distinct from
plain `status`'s LSB codes:

| Exit | Meaning | When |
| --- | --- | --- |
| 0 | OK | running, answering `/1.0/health`, `health.json` says serving, firewall verified or not needed |
| 1 | WARNING | running, but the installed binary is newer than the running one, or the firewall is `unverified` because this invocation lacks root and no verifier status exists |
| 2 | CRITICAL | not running; running but not answering; `health.json` says refused; firewall missing or verifier status stale or failing |
| 3 | UNKNOWN | the check itself could not run (state directory unreadable) |

Output is one summary line first, then one line per check:

```text
CRITICAL: janus refusing to start since 2026-10-05T09:12:44Z (43117 attempts): firewall: /etc/pf.conf does not load the janus anchor (run: sudo janus firewall)
```

`--check --json` prints the same verdicts as an object. Plain `janus
status` keeps its LSB codes (3 not running, 1 not answering) for the
scripts that read them today.

### 3. A dead-man's switch, documented

A monitor that only runs on the edge's machine cannot report the machine
being off, asleep, or offline. The robust pattern inverts it: a periodic
job runs `janus status --check` and, on exit 0, sends a heartbeat to an
external service the operator already uses (a Prometheus Pushgateway, a
healthchecks-style URL, an uptime monitor); the external side alerts when
heartbeats stop. Janus ships no transport and stores no URL. The operator
tutorial carries two recipes: a systemd user timer plus service, and a
launchd agent with `StartInterval`, each one command on top of `--check`.

For operators who already scrape, the Prometheus side is two rules,
documented beside the recipes: `up{job="janus"} == 0 for 2m` and
`absent(janus_build_info) for 2m`.

### Why not a supervisor failure hook

The Linux unit sets `StartLimitIntervalSec=0` and `Restart=on-failure`, so
a refusing edge never enters `failed`, and `OnFailure=` never fires. That
setting is deliberate: a `lan` edge whose address moved must keep retrying.
launchd has no failure hook at all. A hook inside the binary would be a
second config language (rule 3) and would not run when the binary cannot
start. The record plus `--check` plus an external switch covers every case
a hook would, including the five-day one.

## Interaction with existing capabilities

- The JSON surfaces stay as they are; metrics are a second rendering of
  the same counters, never a replacement.
- `control`: `/1.0/metrics` is one more read-only route on every listener,
  with the same token posture. The `internal` socket's permissions already
  bound who reads it.
- `access log`: unchanged. Per-request counts by status come from Caddy's
  own `metrics` (per server, opt-in, with its own cost) or the access log,
  not from Janus.
- `wan` exposure: nothing new listens; `/1.0/metrics` is reachable exactly
  where `/1.0` is.

## Reload and restart

A reload registers the new generation's collector in the new context's
registry; the old generation's registry is discarded with its context.
Pooled counters carry across; the scrape after a reload continues every
pooled series without a reset. A restart resets every counter, and
`janus_start_time_seconds` lets rules tell a restart from a fault.
`health.json` survives restarts by design: it describes the latest start.

## Migration

None required. Existing Caddyfiles gain the families with no edit. The
seed gains nothing; its comment on the control line mentions
`/1.0/metrics`. The operator tutorial gains the scrape example (a
`public` control listener with a token file, or the admin socket through a
local agent) and the two dead-man's-switch recipes.

## Hard errors

None at parse (no directive). `/1.0/metrics` answers 405 to methods other
than GET and HEAD, and 401 without the listener's token. A collector that
fails to register in Caddy's registry (a name collision with another
module) is a provision error naming the family.

## Acceptance

Go tests:

- A golden list of every family name, type, and label set; a test fails
  on any family added without a row here.
- Label policy: every emitted label name is in the legal set, and no
  series value matches a registered host, a gate prefix, a configured
  username, or a socket path across a fixture with all of them present.
- Cardinality: 200 registered apps emit 128 `app` values plus `_other`,
  and `janus_metrics_apps_truncated` = 72.
- Counters monotonic across a stage-and-commit reload of the pooled state;
  per-app series vanish after `DELETE` and after a reap.
- Each new counter increments on its path: every data-plane 503 reason,
  every ring outcome, capacity hold outcomes, reap, sendfile rejection
  reasons, TLS ask verdicts, a 401 on a token listener.
- `/1.0/metrics`: content type, OpenMetrics negotiation, 401 without the
  token, 405 on POST.
- Caddy's admin `/metrics` (driven through the admin handler in-process)
  includes `janus_build_info`.
- `cmd/janus`: `health.json` written on each refusal class with `since`
  stable across same-reason attempts and reset on a new reason, rewritten
  to serving after Start; `status --check` exit code and summary line for
  every row of the table, with fakes for the supervisor, socket, and
  firewall.

`./test.sh`, `control` group: `curl --unix-socket … /1.0/metrics` contains
`janus_build_info` and `janus_apps 0`; after the group's registration,
`janus_app_upstreams{app="…"}`; a request to an app with no upstreams
increments `janus_app_unavailable_total{reason="empty"}`; the admin
socket's `/metrics` carries the same `janus_` families. `status --check`
needs a service edge and stays in the Go layer.

## Measurement

The claim is "no regression on the proxied hot path", proved, not
asserted:

1. **Allocation.** The existing data-plane Go benchmarks run with
   `-benchmem` before and after; allocations per request on the 200-path
   are identical.
2. **Throughput.** `bench/baseline.sh` section A (ping-class, w:2
   conc:64, the canonical ~105k RPS configuration) on a freshly booted
   machine, before and after binaries interleaved as in the lock-collapse
   A/B, eight pairs, with a scraper hitting `/1.0/metrics` once per second
   throughout the after legs. Pass: median ratio within the canonical
   baseline's ±3% drift band and p99 within the same band.
3. **Scrape cost.** Wall time and allocations of one scrape at 1, 128, and
   1,000 registered apps with 8 upstreams each, and the registry read-lock
   hold time during it.

Raw provenance as `docs/*-bench-raw-metrics.txt`; results in the ledger.

## Non-goals

- Alert transport (email, SMS, chat); storing a monitoring URL.
- Per-host, per-path, or per-user series; histograms of request latency
  (Caddy's `metrics` has them per server).
- OpenTelemetry push; a metrics directive.
- Replacing the JSON counters.

## Decisions for the owner

| # | Decision | Recommendation |
| --- | --- | --- |
| 1 | Exposition doors | Both: Caddy's registry and `GET /1.0/metrics` |
| 2 | Per-app label cap | 128, overflow into `_other` |
| 3 | Not-running signal | `health.json` + `status --check` + documented dead-man's switch; no shipped timer |
| 4 | Should `janus autostart` install the periodic check job | No in this contract; revisit after the recipes are used |
| 5 | A scrape-only door for a containerized Prometheus | Open. The owner's intended deployment runs Prometheus in an Incus container on the edge host, its UI behind a Janus auth gate (registered through an Incus proxy device that exposes the container's port as a host unix socket). Such a scraper must reach Janus metrics without control-plane rights: `GET /1.0/metrics` on a control listener grants the whole `/1.0` surface to whoever holds that socket or token, and Caddy's admin `/metrics` sits beside an API that rewrites config. Decide whether to add a metrics-only listener (a unix socket an Incus proxy device can expose, serving `/metrics` and nothing else) before choosing door 1 |

## Adversarial review

1. **Cardinality bomb.** The first draft labeled per-app families by app
   id. A tenant that restarts every minute mints a new id each time; a
   control client can mint thousands. Unbounded series either way.
2. **Host labels sneak in.** WebTransport routes and auth sites are
   natural keys and both are hosts; the first draft labeled them.
3. **The scrape as a lock.** A scrape that walks the registry under its
   write lock, or that touches the filesystem (the files directory gate),
   stalls proxied requests behind it.
4. **Counting on the hot path.** The obvious per-app `requests_total`
   is an atomic add on every proxied request on a shared cache line: the
   regression the ledger warns about.
5. **Exit-code collision.** Plain `status` exits 3 for "not running"
   (LSB); monitoring plugins read 3 as UNKNOWN. A monitor wired to plain
   `status` reports a dead edge as unknown and nobody is paged.
6. **The five-day case still silent.** Metrics alone do not fire when the
   process is not running; a check that runs on the same machine does not
   fire when the machine is off.
7. **`OnFailure=` looks like the answer** and never fires under
   `StartLimitIntervalSec=0`.
8. **Admin API exposure.** "Scrape Caddy's `/metrics`" invites operators
   to put the admin API on TCP, which is configuration write access.
9. **`health.json` as a log.** A refusal loop at ten-second intervals
   writing a history fills the disk in exactly the outage nobody watches.
10. **Double registration on reload.** Registering pooled collectors in a
    process-global registry panics or double-counts on the second
    generation.

## Revisions

- `app` label is the name; 128-value cap with `_other` and a truncation
  gauge (1).
- WebTransport and auth families are process totals; the label policy
  forbids hosts, paths, users, ids, and addresses, and a test enforces it
  (2).
- The collector reads the same snapshot `GET /1.0/apps` builds, under the
  read lock, and never touches the filesystem (3).
- No per-request counter; request counts are left to Caddy's opt-in
  `metrics` and the access log; the measurement plan pins allocations and
  throughput (4).
- `--check` is a flag with monitoring-plugin codes; plain `status` keeps
  LSB codes, and the table says which is which (5).
- The not-running signal is a record, a check, and an external
  dead-man's switch; metrics are not claimed to cover it (6).
- The supervisor-hook section says why not (7).
- `/1.0/metrics` on the token-gated control listeners is the remote door;
  the tutorial's scrape example never exposes the admin API (8).
- `health.json` holds one record with `since` and `attempts` (9).
- One collector per generation in that generation's registry (10).
