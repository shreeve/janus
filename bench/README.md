# bench/ — the performance bench harness

Measures, never gates. Nothing here is part of the test contract
(`go test ./...` and `./test.sh` are); these scripts exist to produce
numbers that are comparable across sessions. The discipline, oha sharp
edges, tenant rationale, and leak-probe interpretation live in
[docs/20260720-143705-bench-harness.md](../docs/20260720-143705-bench-harness.md);
the recorded results live in
[docs/20260719-165500-rip-server-performance.md](../docs/20260719-165500-rip-server-performance.md).
Raw result files land flat under `docs/` as
`docs/YYYYMMDD-HHMMSS-bench-raw-*.txt` — never here.

## Files

| File | Role |
| --- | --- |
| `baseline.sh` | The canonical four-section sweep: A) w sweep, B) c sweep on `/io`, C) cache off/on pairs, D) direct-UDS attribution. Owns the manager lifecycle; every leg tees to `$RAW`. |
| `leakprobe.sh` | RSS slope-vs-plateau probe: hammers one worker direct-UDS in batches, snapshots its RSS after each. Reads a running pool; starts nothing. |
| `app.rip` | The bench tenant: ping-class `/` returns `{ok:true}`; `/io` sleeps 5ms. Claims `bench.ripdev.io` (cache on) and `api.ripdev.io` (cache off). |
| `parse.rip` | oha JSON (stdin) → one summary line: `LABEL: rps N 200s/s N non200 N p50 X.XXms p99 X.XXms`. The format is stable — raw files are diffed across sessions. |
| `delta.rip` | Two `/1.0/cache` snapshots → one counter-delta line. |
| `sock.rip` | Prints the first worker socket path from `/1.0/apps`. |
| `count.rip` | oha JSON (stdin) → total request count. |

## Prerequisites

- A rip checkout with `bun install` run (default `~/Data/Code/rip`;
  override with `RIP_DIR`, or point `RIP_BIN` at a rip CLI directly).
- `oha` on PATH (`brew install oha`).
- Janus caddy running with the root `Caddyfile`:
  `ulimit -n 1048575 && ./bin/caddy run` from the repo root. The
  scripts refuse to start it themselves — they check
  `/1.0/health` and fail with the exact command instead.

## Running

```bash
./bench/baseline.sh                       # full canonical sweep, 15s legs
BENCH_SECTIONS="C D" DUR=3 ./bench/baseline.sh   # subset, short legs
./bench/leakprobe.sh                      # against an already-running pool
```

`baseline.sh` creates its scratch dir (`/tmp/janus-bench` by default,
override with `BENCH_SCRATCH`) and the `@rip-lang/{server,validate}`
symlinks the manager needs, echoing everything it creates. A 15s-leg
`DUR` on a cold, otherwise-idle machine is the canonical setup —
anything else measures ratios, not absolutes.
