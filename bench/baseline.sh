#!/bin/zsh
# Canonical cold-machine baseline runner: three sections in fixed order
# (A w sweep, B c sweep, C UDS attribution), every
# leg tee'd to $RAW. Discipline and interpretation:
# docs/20260720-143705-bench-harness.md.
#
# Assumes: Janus already running (control at $CONTROL), NO rip
# manager running — this script owns the manager lifecycle.
#
# Env knobs (all optional):
#   JANUS_DIR      janus checkout        (default: this script's repo root)
#   RIP_DIR        rip checkout          (default: $HOME/Data/Code/rip)
#   RIP_BIN        rip CLI               (default: $RIP_DIR/node_modules/.bin/rip)
#   BENCH_SCRATCH  scratch dir           (default: /tmp/janus-bench)
#   RAW            raw output file       (default: $BENCH_SCRATCH/raw.txt)
#   CONTROL        control plane base    (default: http://127.0.0.1:7600)
#   DUR            leg seconds           (default: 15 — the canonical length)
#   BENCH_SECTIONS sections to run       (default: "A B C")
set -u

BENCH=${0:A:h}
JANUS_DIR=${JANUS_DIR:-${0:A:h:h}}
RIP_DIR=${RIP_DIR:-$HOME/Data/Code/rip}
RIP_BIN=${RIP_BIN:-$RIP_DIR/node_modules/.bin/rip}
BENCH_SCRATCH=${BENCH_SCRATCH:-/tmp/janus-bench}
RAW=${RAW:-$BENCH_SCRATCH/raw.txt}
CONTROL=${CONTROL:-http://127.0.0.1:7600}
DUR=${DUR:-15}
BENCH_SECTIONS=${BENCH_SECTIONS:-"A B C"}

die() { echo "FATAL: $@" >&2; exit 1 }
wants() { [[ " $BENCH_SECTIONS " == *" $1 "* ]] }

[[ -d $RIP_DIR ]] || die "rip checkout not found at $RIP_DIR (set RIP_DIR)"
[[ -x $RIP_BIN ]] || die "rip CLI not found at $RIP_BIN (bun install in $RIP_DIR, or set RIP_BIN)"
command -v oha >/dev/null 2>&1 || die "oha not found on PATH (brew install oha)"
[[ -x $JANUS_DIR/bin/janus ]] || die "no Janus binary at $JANUS_DIR/bin/janus — build it: cd $JANUS_DIR && make janus"
curl -sf --max-time 2 $CONTROL/1.0/health >/dev/null 2>&1 \
  || die "Janus control plane not answering at $CONTROL/1.0/health — start it: cd $JANUS_DIR && ulimit -n 1048575 && ./bin/janus run"

# Scratch dir: the manager's Bun.build artifact step resolves imports from
# the app's directory, so @rip-lang/{server,validate} must be resolvable
# there. Created pieces are echoed; existing ones are left alone — except
# a scratch app.rip that diverges from the committed tenant, which is an
# unknown tenant and a hard stop.
[[ -d $BENCH_SCRATCH ]] || { echo "creating scratch dir $BENCH_SCRATCH"; mkdir -p $BENCH_SCRATCH }
mkdir -p $BENCH_SCRATCH/node_modules/@rip-lang
for pkg in server validate; do
  if [[ ! -e $BENCH_SCRATCH/node_modules/@rip-lang/$pkg ]]; then
    echo "creating symlink $BENCH_SCRATCH/node_modules/@rip-lang/$pkg -> $RIP_DIR/packages/$pkg"
    ln -sfn $RIP_DIR/packages/$pkg $BENCH_SCRATCH/node_modules/@rip-lang/$pkg
  fi
done
if [[ ! -e $BENCH_SCRATCH/app.rip ]]; then
  echo "copying tenant $BENCH/app.rip -> $BENCH_SCRATCH/app.rip"
  cp $BENCH/app.rip $BENCH_SCRATCH/app.rip
elif ! cmp -s $BENCH/app.rip $BENCH_SCRATCH/app.rip; then
  die "$BENCH_SCRATCH/app.rip differs from $BENCH/app.rip — refusing to bench an unknown tenant; remove the scratch copy or align the two"
fi

cd $BENCH_SCRATCH

# Direct bun invocation (no rip wrapper) so $MGR is the real manager pid —
# killing the rip-server bin wrapper would orphan a live manager + workers.
BIN=(bun --preload=$RIP_DIR/src/loader.js $RIP_DIR/src/run.js $RIP_DIR/node_modules/.bin/rip-server)
PING=https://bench.ripdev.io/
IO=https://bench.ripdev.io/io
MGR=

say() { echo "$@" | tee -a $RAW }

leg() { # label url conc [secs]
  local secs=${4:-$DUR}
  env -u NO_COLOR oha -z ${secs}s -c $3 --no-tui --output-format json "$2" 2>/dev/null \
    | $RIP_BIN $BENCH/parse.rip "$1" $secs | tee -a $RAW
}

warmup() { # url conc secs
  env -u NO_COLOR oha -z ${3}s -c $2 --no-tui --output-format quiet "$1" >/dev/null 2>&1
}

start_mgr() { # w c
  RIP_ENV=production $BIN[@] app.rip --name bench --host bench.ripdev.io \
    -w $1 -c $2 --control $CONTROL >> $BENCH_SCRATCH/mgr.log 2>&1 &
  MGR=$!
  local i=0
  until curl -sf --max-time 2 $PING >/dev/null 2>&1; do
    sleep 0.25; i=$((i+1))
    if [[ $i -gt 240 ]]; then say "FATAL: manager w:$1 c:$2 never became ready"; exit 1; fi
  done
}

stop_mgr() {
  kill $MGR 2>/dev/null
  wait $MGR 2>/dev/null
  # wait for the host claim to clear (the next register would 409-retry
  # anyway, but keep legs clean)
  sleep 2
}

rss_snap() { # label
  ps -Ao rss,args | grep -E 'rip-srv-.*worker\.js' | grep -v grep \
    | awk -v l="$1" '{s+=$1; n+=1; if($1>mx)mx=$1; if(mn==0||$1<mn)mn=$1} END {printf "%s: workers %d rss min %.1fMB max %.1fMB\n", l, n, mn/1024, mx/1024}' | tee -a $RAW
}

say "=== canonical cold-machine baseline $(date) ==="
say "sections: $BENCH_SECTIONS; legs ${DUR}s"
say "load: $(uptime)"
say "rig: $(sysctl -n machdep.cpu.brand_string 2>/dev/null || sysctl -n hw.model), $(sysctl -n hw.ncpu) cores, $(($(sysctl -n hw.memsize)/1073741824))GB, $(sw_vers -productVersion)"
say "bun $(bun --version), $(go version | cut -d' ' -f3), $($JANUS_DIR/bin/janus version | head -1), oha $(oha --version | cut -d' ' -f2)"
say "ulimit -n $(ulimit -n); ${DUR}s legs, 5s warmups discarded; HTTPS full stack; RIP_ENV=production"
say ""

if wants A; then
  say "== A) w sweep, ping-class, c:1 =="
  for w in 2 4 8 16 32; do
    start_mgr $w 1
    warmup $PING 64 5
    leg "A ping w:$w c:1 conc:$w" $PING $w
    leg "A ping w:$w c:1 conc:64" $PING 64
    rss_snap "A rss w:$w"
    stop_mgr
  done
  say "load after A: $(uptime | sed 's/.*load/load/')"
  say ""
fi

if wants B; then
  say "== B) c sweep on /io (5ms), w:8, conc:64 (+128 at c>=16) =="
  for c in 1 4 8 16; do
    start_mgr 8 $c
    warmup $IO 64 5
    leg "B io w:8 c:$c conc:64" $IO 64
    if [[ $c -ge 16 ]]; then
      leg "B io w:8 c:$c conc:128" $IO 128
    fi
    stop_mgr
  done
  say "load after B: $(uptime | sed 's/.*load/load/')"
  say ""
fi

if wants C; then
  say "== C) attribution: direct worker UDS vs Janus (w:2 c:1) =="
  start_mgr 2 1
  warmup $PING 64 5
  SOCK=$($RIP_BIN $BENCH/sock.rip $CONTROL) || die "no worker socket at the control plane"
  say "worker socket: $SOCK"
  env -u NO_COLOR oha -z 3s -c 1 --no-tui --output-format quiet --unix-socket $SOCK http://localhost/ >/dev/null 2>&1
  leg_uds() { # label conc
    env -u NO_COLOR oha -z ${DUR}s -c $2 --no-tui --output-format json --unix-socket $SOCK http://localhost/ 2>/dev/null \
      | $RIP_BIN $BENCH/parse.rip "$1" $DUR | tee -a $RAW
  }
  leg_uds "C uds direct conc:1" 1
  leg_uds "C uds direct conc:2" 2
  leg "C janus tls conc:1" $PING 1
  stop_mgr
fi

say ""
say "DONE $(date)"
say "final load: $(uptime | sed 's/.*load/load/')"
