# AGENTS.md — Operating Rules for Janus

Standing rules for anyone (AI or human) working in this repository.
Janus is a **Caddy module**: cold Caddyfile capabilities + hot `/1.0` control API.

Permanent docs:

- [README.md](README.md) — build, run, orientation
- [docs/20260718-191425-janus-build-spec.md](docs/20260718-191425-janus-build-spec.md) — phased build contract
- `docs/YYYYMMDD-HHMMSS-capability-*.md` — per-capability cold-config docs
- [certs/README.md](certs/README.md) — intentional public `*.ripdev.io` TLS material

## The Rules

1. **Reject loudly; never tolerate silently.** Bad Caddyfile tokens, illegal
   control modes, cascade conflicts, unknown hosts on the data plane — fail
   with precise errors. No silent repair, no drift-tolerance.

2. **Cold vs hot stay separate.** Caddyfile = capabilities and admission.
   `/1.0` = registry (apps, upstreams, heartbeats, hub). Hot JSON never
   changes the cold admission gate.

3. **Normal Caddyfile only.** No parallel config language. New behavior is a
   Caddy module / directive with `UnmarshalCaddyfile`, legal values, defaults,
   and hard errors — same grammar as stock Caddy.

4. **Capabilities are the unit of cold work.** Adding behavior means:
   implement → document → wire into root `Caddyfile` → test → validate.
   Each capability doc covers order #, what/why, scope, cascade yes/no,
   syntax, examples, hard errors, non-goals. Keep the story ordered:
   **ping** (1, primordial) → **control** (2) → **cache** (3) →
   whatever is next.

5. **Cascade is explicit.**
   - **Process-wide** (e.g. **control**): global `janus { }` only; never in a site block.
   - **Site-scoped** (e.g. **ping**): global default → site override; unmentioned
     inherits; explicit `off` beats inherited `on`; built-in default when unset
     everywhere.
   Document **Cascades: yes/no** on every capability page.

6. **Present tense only.** Code and docs state current facts. No “legacy”,
   “previously”, “for compat”, or speculative “someday” comments.

7. **Tests are the contract.** Two layers, both required:
   - **`go test ./...`** — idiomatic Go tests for developers building the module
     (parsing, cascade helpers, internals).
   - **`./test.sh`** — self-contained high-level acceptance for proving cold
     capabilities end-to-end (HTTPS, SNI sites, status/body). Add cases there
     as capabilities land; do not replace Go tests with it.
   Failures are failures — do not weaken acceptance to pass.

8. **Claims are verified.** Reproduce before changing code. Run
   `go test ./...` and `./test.sh` before calling a capability done.

9. **Plain git.** Commits carry no AI attribution. Do not commit secrets
   outside the intentional `certs/ripdev.io.*` pair (see certs/README.md and
   `.github/secret_scanning.yml`).

10. **Go, Rip, and shell — nothing else.** Implementation languages are Go,
    Rip, and shell, everywhere in the repo: module code, test fixtures,
    bench rigs, tooling. No python (or any other language), not even in a
    heredoc. Test-support programs live in `./testkit` (Go).

## Capability order

Cold capabilities are numbered by landing order. Do not reorder the story in docs or `test.sh`.

| # | Capability | Standalone meaning |
| --- | --- | --- |
| 1 | **ping** | Primordial. Proves the module, TLS, site admission, and cascade. Needs nothing else. |
| 2 | **control** | Process-wide `/1.0` listeners. Assumes the ping chassis already works. |
| 3 | **cache** | Site-scoped micro-cache + request coalescing (cascades: yes). Sits on the Phase 4 data plane. |
| 4 | **hub** | Per-app WebSocket fan-out (cascades: yes). Sits on the Phase 4 data plane and the Phase 3 registry. |
| 5+ | next | Hot work on `/1.0` (apps, upstreams, …) and any later cold capabilities. |

`./test.sh` runs groups in this order: ping, control, apps, data, cache, heartbeat, tls, hub, tenant.

## Architecture (short)

| Plane | Config | Role |
| --- | --- | --- |
| **data** | Site `janus` [block] | Admit this host into Janus; site-scoped overrides (**ping**, …) |
| **control** | Global `janus { control … }` | Where `/1.0` listens: `internal` / `local` / `public` |

Unknown public hosts → **404**. Registry is memory-only; tenants re-register after restart.

Hub (capability 4, Bam protocol ancestry) terminates WebSockets at the edge and
bridges socket events to the tenant over HTTP.

## Docs

- Timestamp prefix: `YYYYMMDD-HHMMSS-{name}.md` (or `.html`) under `docs/` only.
- Design HTML = history. Build SPEC + capability pages = what we implement against.
- [`docs/README.md`](docs/README.md) indexes what is authoritative vs history vs provenance.
- Root **`Caddyfile`** is the working cold config; **`Caddyfile.example`** is the operator-facing example (validates standalone).

## Local HTTPS

Use committed `certs/ripdev.io.{crt,key}` (`*.ripdev.io` → `127.0.0.1`).
Multi-site SNI demos (`off.ripdev.io`, `on.ripdev.io`, …). No `curl -k`.

## Commands

```bash
export PATH="$(go env GOPATH)/bin:$PATH"

go test ./...          # developer / unit
./test.sh              # high-level acceptance (builds/starts caddy as needed)

xcaddy build --with github.com/shreeve/janus=. --output ./bin/caddy
./bin/caddy validate
./bin/caddy run
```

Rebuild after Go changes. `bin/` is gitignored.

## When adding a capability

1. Decide process-wide vs site-scoped (cascade rules above).
2. Implement parse + provision + behavior; reject illegal input.
3. Write `docs/YYYYMMDD-HHMMSS-capability-<name>.md`.
4. Exercise it in root `Caddyfile` (prefer two sites when cascade matters).
5. Tests + validate + curls; tick the build SPEC if a phase completes.

## When blocked

- Missing decision → present options with a recommendation; ask. Do not silently choose.
- Gate diverging inexplicably → stop and report.
- Acceptance criterion seems wrong → propose the change; do not quietly test something weaker.
