#!/bin/zsh
# Leak probe: hammer ONE worker over its UDS in successive batches;
# snapshot its RSS after each. The verdict is slope vs plateau against
# CUMULATIVE requests: a leak grows with request count; GC steady-state
# plateaus. Interpretation: docs/20260720-143705-bench-harness.md.
#
# Assumes: Janus caddy AND a manager already running (this script only
# reads the pool — it never starts or stops anything).
#
# Env knobs (all optional):
#   RIP_DIR   rip checkout       (default: $HOME/Data/Code/rip)
#   RIP_BIN   rip CLI            (default: $RIP_DIR/node_modules/.bin/rip)
#   CONTROL   control plane base (default: http://127.0.0.1:7600)
#   DUR       batch seconds      (default: 15 — the canonical length)
#   BATCHES   batch count        (default: 8)
set -u

BENCH=${0:A:h}
RIP_DIR=${RIP_DIR:-$HOME/Data/Code/rip}
RIP_BIN=${RIP_BIN:-$RIP_DIR/node_modules/.bin/rip}
CONTROL=${CONTROL:-http://127.0.0.1:7600}
DUR=${DUR:-15}
BATCHES=${BATCHES:-8}

die() { echo "FATAL: $@" >&2; exit 1 }

[[ -d $RIP_DIR ]] || die "rip checkout not found at $RIP_DIR (set RIP_DIR)"
[[ -x $RIP_BIN ]] || die "rip CLI not found at $RIP_BIN (bun install in $RIP_DIR, or set RIP_BIN)"
command -v oha >/dev/null 2>&1 || die "oha not found on PATH (brew install oha)"
curl -sf --max-time 2 $CONTROL/1.0/health >/dev/null 2>&1 \
  || die "Janus control plane not answering at $CONTROL/1.0/health — start caddy first"

SOCK=$($RIP_BIN $BENCH/sock.rip $CONTROL) || die "no worker socket at the control plane (is a manager running?)"
WPID=$(lsof -t "$SOCK" 2>/dev/null | head -1)
[[ -n $WPID ]] || die "no process listening on $SOCK"
echo "socket $SOCK worker pid $WPID"

snap() { ps -o rss= -p $WPID | awk -v l="$1" '{printf "%s: rss %.1fMB\n", l, $1/1024}' }

snap "at publish, 0 req"
total=0
for i in $(seq 1 $BATCHES); do
  n=$(env -u NO_COLOR oha -z ${DUR}s -c 2 --no-tui --output-format json --unix-socket "$SOCK" http://localhost/ 2>/dev/null \
    | $RIP_BIN $BENCH/count.rip)
  total=$((total + n))
  snap "cumulative $(printf "%'d" $total) req"
done
echo PROBE-DONE
