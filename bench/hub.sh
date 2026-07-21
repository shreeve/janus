#!/bin/zsh
# Hub bench sweep: the six Phase 7 measurements from the hub contract's
# "Bench plan" (docs/20260720-162350-hub-design.md), in section order:
#   1) fan-out throughput   — 1 publisher, 1 channel, N subscribers, rate ladder
#   2) delivery latency     — p50/p99 at 10%/50%/90% of the section-1 ceiling
#   3) conn ceiling + idle  — ramp to max_conns: admitted conns/s, RSS/idle conn
#   4) slow-consumer        — one wedged sub in a busy channel: others' p99 flat
#   5) reload under fan-out — doorbell cut + republish mid-traffic: no drops
#   6) text-bridge tax      — client-send throughput, tenant 204 instant vs +5ms
#
# Needs only: Janus caddy running (root Caddyfile), Go (builds bench/hubbench),
# python3, curl. NO rip manager — the tenant is hubbench's own bridge fixture.
#
# Env knobs (all optional):
#   JANUS_DIR      janus checkout        (default: this script's repo root)
#   BENCH_SCRATCH  scratch dir           (default: /tmp/janus-bench-hub)
#   RAW            raw output file       (default: $BENCH_SCRATCH/raw-hub.txt)
#   CONTROL        control plane base    (default: http://127.0.0.1:7600)
#   HUB_HOST       hub site              (default: hubany.ripdev.io — origin any,
#                                         app floor 4096 via the global default)
#   DUR            leg seconds           (default: 15 — the canonical length)
#   HUB_SECTIONS   sections to run       (default: "1 2 3 4 5 6")
#   HUB1_SIZES     subscriber counts     (default: "100 1000 4000")
#   HUB1_RATES_<N> publish-rate ladder per size (defaults below)
#   HUB2_N         latency-section size  (default: 1000)
#   HUB2_CEIL      publish/s ceiling at HUB2_N, read off section 1 (default: 400)
#   HUB3_N         ramp target           (default: 4096 = the app's cap floor)
#   HUB4_N         wedge-section size    (default: 1000)
#   HUB4_RATE      wedge-section rate    (default: HUB2_CEIL/2)
#   HUB6_N         sender count          (default: 50)
set -u

BENCH=${0:A:h}
JANUS_DIR=${JANUS_DIR:-${0:A:h:h}}
BENCH_SCRATCH=${BENCH_SCRATCH:-/tmp/janus-bench-hub}
RAW=${RAW:-$BENCH_SCRATCH/raw-hub.txt}
CONTROL=${CONTROL:-http://127.0.0.1:7600}
HUB_HOST=${HUB_HOST:-hubany.ripdev.io}
DUR=${DUR:-15}
HUB_SECTIONS=${HUB_SECTIONS:-"1 2 3 4 5 6"}
HUB1_SIZES=${HUB1_SIZES:-"100 1000 4000"}
HUB1_RATES_100=${HUB1_RATES_100:-"250 500 1000 2000 4000"}
HUB1_RATES_1000=${HUB1_RATES_1000:-"50 100 200 400 800"}
HUB1_RATES_4000=${HUB1_RATES_4000:-"12 25 50 100 200"}
HUB2_N=${HUB2_N:-1000}
HUB2_CEIL=${HUB2_CEIL:-400}
HUB3_N=${HUB3_N:-4096}
HUB4_N=${HUB4_N:-1000}
HUB4_RATE=${HUB4_RATE:-$((HUB2_CEIL / 2))}
HUB6_N=${HUB6_N:-50}

CHANNEL=/bench
SOCK=$BENCH_SCRATCH/hub-tenant.sock
HB=$BENCH_SCRATCH/hubbench
APP_ID=
TENANT_PID=
typeset -a KIDS

die() { echo "FATAL: $@" >&2; exit 1 }
say() { echo "$@" | tee -a $RAW }
wants() { [[ " $HUB_SECTIONS " == *" $1 "* ]] }

curl -sf --max-time 2 $CONTROL/1.0/health >/dev/null 2>&1 \
  || die "Janus control plane not answering at $CONTROL/1.0/health — start it: cd $JANUS_DIR && ulimit -n 1048575 && ./bin/caddy run"

mkdir -p $BENCH_SCRATCH
echo "building $HB from $BENCH/hubbench"
(cd $BENCH/hubbench && go build -o $HB .) || die "hubbench build failed"

cleanup() {
  local pid
  for pid in $KIDS; do kill $pid 2>/dev/null; done
  [[ -n "$TENANT_PID" ]] && kill $TENANT_PID 2>/dev/null
  if [[ -n "$APP_ID" ]]; then
    curl -s -o /dev/null -X DELETE $CONTROL/1.0/apps/$APP_ID
  fi
  rm -f $SOCK
}
trap cleanup EXIT INT TERM

# --- control-plane helpers ----------------------------------------------------

# The process totals precede the per-app breakdown in GET /1.0/hub, so
# cutting the body at "apps" leaves each counter key exactly once.
hub_totals() { curl -s $CONTROL/1.0/hub | sed 's/"apps":.*//' }

hub_stat() { # KEY — one process-total counter from GET /1.0/hub
  hub_totals | sed -n "s/.*\"$1\":\([0-9-]*\).*/\1/p"
}

hub_counters() { # LABEL — one line with the counters the sections diff
  local body out=$1 k
  body=$(hub_totals)
  for k in conns deliveries publishes frames_in slow_closes \
           bridge_sent bridge_failed bridge_dropped unknown_targets; do
    out+=" $k=$(printf '%s' "$body" | sed -n "s/.*\"$k\":\([0-9-]*\).*/\1/p")"
  done
  echo "$out" | tee -a $RAW
}

wait_conns_zero() {
  local i
  for i in $(seq 1 200); do
    [[ "$(hub_stat conns)" == "0" ]] && return 0
    sleep 0.25
  done
  say "WARN: hub conns never drained to 0 ($(hub_stat conns) left)"
  return 1
}

wait_line() { # FILE SUBSTR TIMEOUT_S
  local i
  for i in $(seq 1 $(($3 * 10))); do
    grep -q "$2" "$1" 2>/dev/null && return 0
    sleep 0.1
  done
  echo "never saw '$2' in $1" >&2
  return 1
}

start_tenant() { # [text-delay]
  local delay=${1:-0s}
  [[ -n "$TENANT_PID" ]] && { kill $TENANT_PID 2>/dev/null; wait $TENANT_PID 2>/dev/null }
  $HB -mode tenant -sock $SOCK -app $APP_ID -control $CONTROL -text-delay $delay \
    >>$BENCH_SCRATCH/tenant.log 2>&1 &
  TENANT_PID=$!
  local i
  for i in $(seq 1 50); do
    [[ -S $SOCK ]] && { sleep 0.2; return 0 }
    sleep 0.1
  done
  die "tenant socket $SOCK never appeared"
}

upgrade_code() { # HOST — curl-shaped upgrade attempt; prints HTTP status
  curl -sS -o /dev/null -w '%{http_code}' --max-time 5 --http1.1 \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
    -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
    "https://$1/hub" 2>/dev/null
}

caddy_rss_mb() {
  local pid
  pid=$(lsof -t -iTCP:7600 -sTCP:LISTEN 2>/dev/null | head -1)
  [[ -n "$pid" ]] || { echo "0"; return 1 }
  ps -o rss= -p $pid | awk '{printf "%.1f", $1/1024}'
}

# subs_pub_leg LABEL N RATE PAD WEDGE — N subscribers, paced publish for $DUR,
# report both sides plus the slow-close delta.
subs_pub_leg() {
  local label=$1 n=$2 rate=$3 pad=$4 wedge=$5
  local out=$BENCH_SCRATCH/subs-$$.out s0
  rm -f $out
  $HB -mode subs -host $HUB_HOST -channel $CHANNEL -n $n -wedge $wedge \
    -dur $((DUR + 6))s >$out 2>>$BENCH_SCRATCH/subs.err &
  local sp=$!
  KIDS+=($sp)
  wait_line $out READY 120 || { say "$label: subs never READY"; kill $sp 2>/dev/null; return 1 }
  s0=$(hub_stat slow_closes)
  say "$label $(grep READY $out)"
  $HB -mode pub -app $APP_ID -control $CONTROL -channel $CHANNEL \
    -rate $rate -pad $pad -dur ${DUR}s 2>>$BENCH_SCRATCH/pub.err \
    | sed "s/^/$label /" | tee -a $RAW
  wait $sp
  say "$label $(grep SUBS $out)"
  say "$label slow_closes_delta=$(( $(hub_stat slow_closes) - s0 ))"
  wait_conns_zero
}

# --- setup ---------------------------------------------------------------------

say "=== hub bench sweep $(date) ==="
say "sections: $HUB_SECTIONS; legs ${DUR}s; host $HUB_HOST; channel $CHANNEL"
say "load: $(uptime)"
say "rig: $(sysctl -n machdep.cpu.brand_string 2>/dev/null || sysctl -n hw.model), $(sysctl -n hw.ncpu) cores, $(($(sysctl -n hw.memsize)/1073741824))GB, $(sw_vers -productVersion)"
say "$(go version), caddy $($JANUS_DIR/bin/caddy version | head -1 | cut -d' ' -f1)"
say "janus commit: $(git -C $JANUS_DIR rev-parse --short HEAD 2>/dev/null || echo unknown)"
say "ulimit -n $(ulimit -n)"
say ""

REPLY=$(curl -s -X POST $CONTROL/1.0/apps -H 'Content-Type: application/json' \
  -d "{\"name\":\"hubbench\",\"hosts\":[\"$HUB_HOST\"],\"bridge_path\":\"/bench/bridge\"}")
APP_ID=$(printf '%s' "$REPLY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[[ -n "$APP_ID" ]] || die "app registration failed: $REPLY"
say "app: $APP_ID (host $HUB_HOST, bridge /bench/bridge)"
start_tenant
curl -s -o /dev/null -X PUT $CONTROL/1.0/apps/$APP_ID/upstreams \
  -H 'Content-Type: application/json' -d "{\"upstreams\":[{\"path\":\"$SOCK\"}]}"
[[ "$(upgrade_code $HUB_HOST)" == "101" ]] || say "WARN: probe upgrade did not answer 101"
wait_conns_zero
say ""

# --- sections -------------------------------------------------------------------

if wants 1; then
  say "== 1) fan-out throughput: 1 publisher, 1 channel, N subscribers, rate ladder =="
  for n in ${=HUB1_SIZES}; do
    rates_var="HUB1_RATES_$n"
    for r in ${=${(P)rates_var}}; do
      subs_pub_leg "1 fanout n:$n rate:$r" $n $r 0 0
    done
  done
  hub_counters "1 counters"
  say "load after 1: $(uptime | sed 's/.*load/load/')"
  say ""
fi

if wants 2; then
  say "== 2) delivery latency at 10/50/90% of the fan-out ceiling (n:$HUB2_N, ceiling $HUB2_CEIL/s) =="
  for r in $((HUB2_CEIL / 10)) $((HUB2_CEIL / 2)) $((HUB2_CEIL * 9 / 10)); do
    subs_pub_leg "2 latency n:$HUB2_N rate:$r" $HUB2_N $r 0 0
  done
  say "load after 2: $(uptime | sed 's/.*load/load/')"
  say ""
fi

if wants 3; then
  say "== 3) connection ceiling + idle cost: ramp to $HUB3_N, RSS per idle conn =="
  rss0=$(caddy_rss_mb)
  say "3 caddy rss before: ${rss0}MB"
  out=$BENCH_SCRATCH/ramp.out
  rm -f $out
  $HB -mode ramp -host $HUB_HOST -channel $CHANNEL -n $HUB3_N -hold 30s >$out 2>>$BENCH_SCRATCH/subs.err &
  rp=$!
  KIDS+=($rp)
  wait_line $out HOLDING 180 || say "3: ramp never reached HOLDING"
  say "3 $(grep RAMP $out | head -1)"
  sleep 5
  rss1=$(caddy_rss_mb)
  nconns=$(hub_stat conns)
  say "3 caddy rss at $nconns idle conns: ${rss1}MB"
  say "3 rss per idle conn: $(awk -v a=$rss0 -v b=$rss1 -v n=$nconns 'BEGIN{printf "%.1fKB", (b-a)*1024/(n>0?n:1)}')"
  say "3 upgrade past the cap: $(upgrade_code $HUB_HOST) (want 503)"
  kill $rp 2>/dev/null
  wait $rp 2>/dev/null
  wait_conns_zero
  say "load after 3: $(uptime | sed 's/.*load/load/')"
  say ""
fi

if wants 4; then
  say "== 4) slow-consumer isolation: one wedged sub in n:$HUB4_N at rate:$HUB4_RATE (pad 1kb) =="
  subs_pub_leg "4 nowedge pair-A" $HUB4_N $HUB4_RATE 1024 0
  subs_pub_leg "4 wedged  pair-A" $HUB4_N $HUB4_RATE 1024 1
  subs_pub_leg "4 wedged  pair-B" $HUB4_N $HUB4_RATE 1024 1
  subs_pub_leg "4 nowedge pair-B" $HUB4_N $HUB4_RATE 1024 0
  say "load after 4: $(uptime | sed 's/.*load/load/')"
  say ""
fi

if wants 5; then
  say "== 5) reload under fan-out: doorbell cut + republish mid-traffic (n:$HUB4_N rate:$HUB4_RATE) =="
  b0f=$(hub_stat bridge_failed); b0d=$(hub_stat bridge_dropped)
  out=$BENCH_SCRATCH/subs-rl.out
  rm -f $out
  $HB -mode subs -host $HUB_HOST -channel $CHANNEL -n $HUB4_N -dur $((DUR + 10))s \
    >$out 2>>$BENCH_SCRATCH/subs.err &
  sp=$!
  KIDS+=($sp)
  wait_line $out READY 120 || die "5: subs never READY"
  say "5 $(grep READY $out)"
  $HB -mode pub -app $APP_ID -control $CONTROL -channel $CHANNEL \
    -rate $HUB4_RATE -dur $((DUR + 5))s 2>>$BENCH_SCRATCH/pub.err \
    | sed 's/^/5 /' | tee -a $RAW &
  pp=$!
  KIDS+=($pp)
  sleep 5
  say "5 doorbell cut (admission cut mid-traffic)"
  curl -s -o /dev/null -X PUT $CONTROL/1.0/apps/$APP_ID/upstreams \
    -H 'Content-Type: application/json' \
    -d "{\"upstreams\":[{\"path\":\"$SOCK\",\"doorbell\":true}]}"
  sleep 3
  say "5 republish (pool back)"
  curl -s -o /dev/null -X PUT $CONTROL/1.0/apps/$APP_ID/upstreams \
    -H 'Content-Type: application/json' -d "{\"upstreams\":[{\"path\":\"$SOCK\"}]}"
  wait $pp
  wait $sp
  say "5 $(grep SUBS $out)"
  say "5 bridge_failed_delta=$(( $(hub_stat bridge_failed) - b0f )) bridge_dropped_delta=$(( $(hub_stat bridge_dropped) - b0d ))"
  wait_conns_zero
  say "load after 5: $(uptime | sed 's/.*load/load/')"
  say ""
fi

if wants 6; then
  say "== 6) text-bridge tax: n:$HUB6_N senders, tenant 204 instant vs +5ms (interleaved) =="
  for leg in "A 0s" "B 5ms" "B 5ms" "A 0s"; do
    tag=${leg%% *}; delay=${leg##* }
    start_tenant $delay
    f0=$(hub_stat frames_in); s0=$(hub_stat bridge_sent); d0=$(hub_stat bridge_dropped)
    $HB -mode send -host $HUB_HOST -n $HUB6_N -dur ${DUR}s 2>>$BENCH_SCRATCH/subs.err \
      | sed "s/^/6 tax $tag delay:$delay /" | tee -a $RAW
    say "6 tax $tag delay:$delay frames_in_delta=$(( $(hub_stat frames_in) - f0 )) bridge_sent_delta=$(( $(hub_stat bridge_sent) - s0 )) bridge_dropped_delta=$(( $(hub_stat bridge_dropped) - d0 ))"
    wait_conns_zero
  done
  start_tenant 0s
  say "load after 6: $(uptime | sed 's/.*load/load/')"
  say ""
fi

say "DONE $(date)"
say "final load: $(uptime | sed 's/.*load/load/')"
