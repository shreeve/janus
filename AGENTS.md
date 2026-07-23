# AGENTS.md — Operating Rules for Janus

Standing rules for anyone (AI or human) working in this repository.

Janus is a **Caddy module**: cold Caddyfile capabilities + hot `/1.0`
control API. Registry, data plane, and hub state live in pooled process
state (`caddy.UsagePool`) — a config reload never drops a registration
or a WebSocket. Everything is memory-only by contract: a restart
empties the registry and tenants re-register.

**Era: stewardship.** Feature-complete at v1.0.0 — every build-spec
box ticked; six cold capabilities shipped. Ongoing work is fix,
harden, measure. New behavior arrives as a capability through the
proven loop: **design contract → adversarial review → revise →
implement → pin in tests → measure** (mdns, capability 5, is the
first post-v1.0.0 product of that loop; auth, capability 6, is the
second).

## The Rules

1. **Reject loudly; never tolerate silently.** Bad Caddyfile tokens,
   illegal control modes, cascade conflicts, unknown hosts on the data
   plane — fail with precise errors. No silent repair, no
   drift-tolerance.

2. **Cold vs hot stay separate.** Caddyfile = capabilities and
   admission. `/1.0` = registry (apps, upstreams, heartbeats, hub).
   Hot JSON never changes the cold admission gate.

3. **Normal Caddyfile only.** No parallel config language. New behavior
   is a Caddy module / directive with `UnmarshalCaddyfile`, legal
   values, defaults, and hard errors — same grammar as stock Caddy.

4. **Capabilities are the unit of cold work.** Numbered by landing
   order; the story (ping → control → cache → hub → mdns → auth → …) never
   reorders in docs or `test.sh`. A new one starts at "When adding a
   capability" below — contract doc and adversarial review before code.

5. **Cascade is explicit.**
   - **Process-wide** (control; cache's `max_bytes`/`max_app_share`):
     global `janus { }` only; a site-level occurrence is a parse error.
   - **Site-scoped** (ping, cache, hub, auth): global default → site
     override; unmentioned inherits; explicit `off` beats inherited
     `on`; built-in default when unset everywhere.
   Document **Cascades: yes/no** on every capability page.

6. **Present tense only.** Code and docs state current facts. No
   "legacy", "previously", "for compat", or speculative "someday"
   comments.

7. **Tests are the contract.** Two layers, both required:
   - **`go test ./...`** — idiomatic Go tests for developers building
     the module (parsing, cascade helpers, internals).
   - **`./test.sh`** — self-contained high-level acceptance proving
     cold capabilities end-to-end (HTTPS, SNI sites, status/body). Run
     it in the **foreground** — its fixture processes die when the
     parent shell detaches (this cost real debugging time). Add cases
     as capabilities land; do not replace Go tests with it.
   Failures are failures — do not weaken acceptance to pass.

8. **Claims are verified.** Reproduce before changing code. Run both
   test layers before calling work done. Every performance claim lands
   with its measurement in the performance ledger, raw provenance as
   `docs/*-bench-raw-*.txt` (never edited).

9. **Plain git.** Commits carry no AI attribution. Do not commit
   secrets outside the intentional `certs/ripdev.io.*` pair (see
   certs/README.md and `.github/secret_scanning.yml`).

10. **Go, Rip, and shell — nothing else.** Implementation languages
    are Go, Rip, and shell, everywhere in the repo: module code, test
    fixtures, bench rigs, tooling. No python (or any other language),
    not even in a heredoc. Test-support programs live in `./testkit`
    (Go).

## Capabilities (all shipped)

| # | Capability | Standalone meaning |
| --- | --- | --- |
| 1 | **ping** | Primordial. Proves the module, TLS, site admission, and cascade. Needs nothing else. |
| 2 | **control** | Process-wide `/1.0` listeners: `internal` / `local` / `public`, per-line `token:` / `cert:` / `key:`. |
| 3 | **cache** | Site-scoped micro-cache + request coalescing, generation-fenced (cascades: yes). |
| 4 | **hub** | Edge-terminated WebSocket fan-out with the Bam directive grammar; the tenant observes and steers over HTTP (cascades: yes). |
| 5 | **mdns** | LAN presence: `janus.local` + per-app `.local` names over multicast DNS; the read-only status front door with the canonical hand-off (cascades: no — process-wide). |
| 6 | **auth** | Edge authentication wall for auth-less apps: exact `/auth`, `g1:` argon2id credentials, pooled in-memory sessions, `Remote-User` strip-and-inject (cascades: yes). |
| 7+ | next | Future capabilities, each through the loop above. |

`./test.sh` runs groups in this order: ping, control, apps, data,
cache, heartbeat, tls, hub, tenant, mdns, auth.

## Architecture (short)

| Plane | Config | Role |
| --- | --- | --- |
| **data** | Site `janus` [block] | Admit this host into Janus; site-scoped overrides (ping, cache, hub, auth) |
| **control** | Global `janus { control … }` | Where `/1.0` listens: `internal` / `local` / `public` |

Unknown public hosts → **404**. Registry, data plane, and hubs sit in
pooled process state (`caddy.UsagePool`): config reloads reuse them;
only registry DELETE, heartbeat TTL reap, or process exit tears them
down. Memory-only across restarts — tenants re-register.

## Docs

- [`docs/README.md`](docs/README.md) is the index — contracts vs
  measurements vs design history. Start there.
- Timestamp prefix: `YYYYMMDD-HHMMSS-{name}.md` (or `.html`) under
  `docs/` only; each file is an append-only point-in-time contract or
  record.
- Runnable demo tutorials live in `docs/<name>/` subdirectories
  (`index.md` + artifacts, e.g. `docs/counter/`).
- Each capability doc covers order #, what/why, scope, cascade yes/no,
  syntax, examples, hard errors, non-goals.
- Design HTML = history. Build SPEC + capability pages = what we
  implement against.
- Root **`Caddyfile`** is the working cold config; **`Caddyfile.example`**
  is the operator-facing example (validates standalone).

## Local HTTPS

Use committed `certs/ripdev.io.{crt,key}` (`*.ripdev.io` → `127.0.0.1`);
SNI picks the site (`off.ripdev.io`, `on.ripdev.io`, …). No `curl -k`.

## Commands

```bash
export PATH="$(go env GOPATH)/bin:$PATH"

go test ./...          # developer / unit
./test.sh              # acceptance (foreground!) — builds testkit and caddy itself

xcaddy build --with github.com/shreeve/janus=. --output ./bin/caddy
./bin/caddy validate   # Caddyfile.example: add --config … --adapter caddyfile
./bin/caddy run
```

Rebuild after Go changes; `bin/` is gitignored.
Benchmarks: [`bench/README.md`](bench/README.md) — measures, never gates.

## When adding a capability

1. Write the contract doc (`docs/YYYYMMDD-HHMMSS-capability-<name>.md`)
   first; decide process-wide vs site-scoped (cascade rules above).
2. Adversarial review; fold the findings in; revise the contract.
3. Implement parse + provision + behavior; reject illegal input.
4. Exercise it in root `Caddyfile` (prefer two sites when cascade
   matters).
5. Pin every contract row in both test layers; validate.
6. Measure; record results with raw provenance (rule 8).

## When blocked

- Missing decision → present options with a recommendation; ask. Do not
  silently choose.
- Gate diverging inexplicably → stop and report.
- Acceptance criterion seems wrong → propose the change; do not quietly
  test something weaker.

## Pointers

- `HANDOFF.md` — optional untracked session snapshot; never committed.
- [certs/README.md](certs/README.md) — the intentional public
  `*.ripdev.io` TLS material.
