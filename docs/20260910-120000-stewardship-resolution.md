# Stewardship findings — resolution record

Follow-up to the [v1.14.1 review](20260910-100643-stewardship-review.md).
The work uses one branch, `fix/stewardship-hardening`, and keeps each
implementation finding in a separate commit. This is a resolution record;
the [living operator reference](operators/index.md) describes current use.

## Implemented and verified

| Finding | Resolution | Commit |
| --- | --- | --- |
| Documentation inconsistencies | Corrected command ownership, socket paths, module count, passhash examples, reload qualifications, and executable claims | `612009f` |
| Control transport lifetime | Owned/reused client, finite idle timeout, explicit close, identity-preserving fallback, propagated body-read errors | `faa9d84` |
| FIFO request hangs | Confined nonblocking opens followed by descriptor checks for files, indexes, shells, sidecars, and listings | `9a3b0a1` |
| Environment divergence | Caddy grammar/precedence for run, adapt, validate, reload and internal operations; reserved bind checked consistently; storage defaults refreshed | `9e28dbc` |
| Nested route inventory | One traversal retains parent intersections, OR alternatives, and impossible matches for hub/auth/browse/mDNS | `ab1992b` |
| Ineffective TTL reload | Changed effective TTL rejected with restart error; `/1.0` reports actual TTL; serve derives its heartbeat interval | `5cc37ce` |
| Equivalent config paths | Relative paths/symlinks preserve service identity and adapter/env behavior; internal reload uses a shared function without sibling flag mutation | `b2dc639` |
| Duplicate control JSON | Shared strict decoder rejects repeated keys at every depth and preserves field absence/null distinctions | `f718a5c` |
| Incomplete app listing | Site patterns and sorted aliases appear in HOSTS | `2f8ab6e` |
| Installer inconsistency | Source and release installs share atomic replacement; copy/capability failure preserves the old executable | `3247227` |
| Misleading serve success | Corrupt scope and failed site application stop setup; new failed site is rolled back; HTTPS route is checked before announcement; leases continue during readiness | `3fb71de` |
| Oversized service module | Separated path/seeding, lifecycle, status, configuration, and control-client responsibilities | `8002170` |
| Acceptance harness sprawl | Sourced capability groups, one foreground owner, isolated per-run writable state, retained failure diagnostics | `05c6037` |
| Late CI feedback | Shared PR/main/release Go/vet/race checks on macOS and Linux, plus foreground tenant acceptance with a pinned Rip revision | `2e8f216` |
| Operator documentation | Living command/scope and reload-survival matrices; strict JSON and config migration guidance; HTTP trust-bootstrap exception made explicit | Accompanying documentation commit |

The TTL acceptance fixtures previously relied on the ineffective-reload bug.
They now preserve the running TTL. The aborted-reload recovery test changes
and verifies the ping default, while a separate test requires TTL rejection.
No survival assertion or capability group was removed.

## Verification

- `make test`: **156 passed, zero failed/skipped**, including real HTTPS,
  Unix control, hub, auth, file representations, browse, and Rip tenant
  lifecycle checks after the source and harness splits.
- `go test -race ./...` and `go vet ./...`: passed on macOS.
- Both `Caddyfile.minimal` and `Caddyfile.example`: validate successfully.
- Linux amd64 and Windows amd64: command cross-builds passed. Native CI
  supplies the macOS/Linux test execution; a cross-build is not an OS test.
- Workflow syntax/action checks: actionlint v1.7.12 passed.
- Installer regressions verify failed copies and failed capability setup
  leave the old executable usable and remove the incomplete stage.
- Rip integration provenance: clean revision
  `88ab78e45db4e559fe2a827a1c4209f4ba6ac9e2`.

These changes establish correctness and bounded client resources. They do
not claim a throughput or latency speedup; no benchmark ledger was changed.

## Configuration choices and further ideas

The repository's seeds already enable bridge hub, precompressed files, and
local access logging. Older operator-owned configurations are intentionally
not rewritten by a binary update. The operator reference provides a concrete
migration procedure for these existing features. Applying it to the installed
edge is separate from merging this branch.

Auth gates and external browse renderers remain explicit policy choices,
not defects to fix by enabling every feature. The review's richer diagnostic
command and automated config-upgrade preview remain potential new product
work, requiring their own contract and adversarial review. Effective TTL,
accurate host inventories, explicit serve failures, the improved app list,
and the migration reference deliver the useful diagnostics within this
cleanup's existing interfaces. No speculative proxy, hub, or registry
rewrite was justified by the review's evidence.
