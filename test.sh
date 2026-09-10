#!/usr/bin/env bash
# test.sh — high-level Janus acceptance suite (self-contained).
#
# For operators/users: prove cold capabilities behave end-to-end.
# Developers still use idiomatic `go test ./...` while building.
#
# Groups run in capability order: ping (1), control (2), …, access (9) last.
#
#   ./test.sh
#   NO_COLOR=1 ./test.sh
#
set -uo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

TMP_BASE="${TMPDIR:-/tmp}"
# A short path keeps every fixture's Unix socket below the platform limit.
TEST_RUN_DIR="$(mktemp -d /tmp/janus-test.XXXXXX)"
export XDG_CONFIG_HOME="$TEST_RUN_DIR/config"
export XDG_STATE_HOME="$TEST_RUN_DIR/state"
export JANUS_TEST_CONTROL="$TEST_RUN_DIR/run/janus.sock"
export JANUS_TEST_ACCESS_LOG="$TEST_RUN_DIR/access.log"
mkdir -p "$TEST_RUN_DIR/run"
CADDY_BIN="${CADDY_BIN:-$ROOT/bin/janus}"
CADDY_LOG="${CADDY_LOG:-$TEST_RUN_DIR/caddy.log}"
CADDY_PID=""
CADDY_PID_FILE="$TEST_RUN_DIR/caddy.pid"

# testkit: the suite's Go support binary (fixture servers, WS driver,
# JSON/string utilities). Built fresh at suite start from ./testkit.
TESTKIT="$TEST_RUN_DIR/testkit"
BROWSE_COLD_ROOT="$(mktemp -d "${TEST_RUN_DIR}/janus-browse-cold.XXXXXX")"
export JANUS_BROWSE_COLD_ROOT="$BROWSE_COLD_ROOT"

# --- colors ---------------------------------------------------------------

RESET=$'\033[0m'
BOLD=$'\033[1m'
DIM=$'\033[2m'
GREEN=$'\033[32m'
RED=$'\033[31m'
YELLOW=$'\033[33m'

use_color() {
	if [[ -n "${NO_COLOR:-}" ]]; then
		return 1
	fi
	if [[ -n "${FORCE_COLOR:-}" && "${FORCE_COLOR}" != "0" ]]; then
		return 0
	fi
	[[ -t 1 ]]
}

paint() {
	local code=$1 text=$2
	if use_color; then
		printf '%s%s%s' "$code" "$text" "$RESET"
	else
		printf '%s' "$text"
	fi
}

# --- tally ----------------------------------------------------------------

PASS=0
FAIL=0
SKIP=0
SUITE_START_NS=0

now_ns() {
	local ns
	ns="$(date +%s%N 2>/dev/null)"
	if [[ "$ns" =~ ^[0-9]+$ ]]; then
		printf '%s\n' "$ns"
	elif [[ -x "$TESTKIT" ]]; then
		"$TESTKIT" now-ns
	else
		# bash 5 fallback: EPOCHREALTIME is "seconds.microseconds"
		printf '%s%s000\n' "${EPOCHREALTIME%.*}" "${EPOCHREALTIME#*.}"
	fi
}

fmt_ms() {
	local ns=$1
	local ms=$((ns / 1000000))
	if ((ms < 1000)); then
		printf '%dms' "$ms"
	else
		awk -v m="$ms" 'BEGIN { printf "%.2fs", m/1000 }'
	fi
}

# --- assertions (throw = nonzero return) ----------------------------------

# eq GOT WANT — string equality
eq() {
	local got=$1 want=$2
	if [[ "$got" != "$want" ]]; then
		printf 'expected %q, got %q' "$want" "$got" >&2
		return 1
	fi
}

# ok COND [MSG] — COND is a shell command/expression string evaluated with [[ ]]
ok() {
	local cond=$1
	local msg=${2:-assertion failed}
	if ! eval "[[ $cond ]]"; then
		printf '%s' "$msg" >&2
		return 1
	fi
}

# ne GOT UNWANTED — string inequality
ne() {
	local got=$1 unwanted=$2
	if [[ "$got" == "$unwanted" ]]; then
		printf 'expected value other than %q' "$unwanted" >&2
		return 1
	fi
}

# --- HTTP helpers ---------------------------------------------------------

http_code() {
	curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$1"
}

http_body() {
	curl -sS --max-time 5 "$1"
}

# --- runner ---------------------------------------------------------------

group() {
	printf '\n%s\n' "$(paint "$BOLD" "== $1 ==")"
}

# test "name" — body is remaining args as a command, or a function name
test() {
	local name=$1
	shift
	local start end elapsed rc err
	start=$(now_ns)
	err="$(
		set -e # every assertion in the case body gates; first failure wins
		"$@" 2>&1
	)"
	rc=$?
	end=$(now_ns)
	elapsed=$((end - start))
	local timing
	timing="$(paint "$DIM" "($(fmt_ms "$elapsed"))")"

	if ((rc == 0)); then
		PASS=$((PASS + 1))
		printf '  %s %s %s\n' "$(paint "$GREEN" "✓")" "$name" "$timing"
	else
		FAIL=$((FAIL + 1))
		printf '  %s %s %s\n' "$(paint "$RED" "✗")" "$name" "$timing"
		if [[ -n "$err" ]]; then
			printf '      %s\n' "$(paint "$RED" "$err")"
		fi
	fi
}

skip() {
	local name=$1
	local reason=${2:-}
	SKIP=$((SKIP + 1))
	if [[ -n "$reason" ]]; then
		printf '  %s %s %s\n' "$(paint "$YELLOW" "!")" "$name" "$(paint "$DIM" "($reason)")"
	else
		printf '  %s %s\n' "$(paint "$YELLOW" "!")" "$name"
	fi
}

report() {
	local total=$((PASS + FAIL + SKIP))
	local end elapsed
	end=$(now_ns)
	elapsed=$((end - SUITE_START_NS))
	local passed failed skipped
	passed="$(paint "$GREEN" "${PASS} passed")"
	if ((FAIL > 0)); then
		failed="$(paint "$RED" "${FAIL} failed")"
	else
		failed="$(paint "$DIM" "${FAIL} failed")"
	fi
	skipped="$(paint "$YELLOW" "${SKIP} skipped")"
	printf '\n%s: %s, %s, %s  %s\n\n' \
		"$(paint "$BOLD" "${total} tests")" \
		"$passed" "$failed" "$skipped" \
		"$(paint "$DIM" "($(fmt_ms "$elapsed"))")"
	((FAIL == 0))
}

# --- lifecycle ------------------------------------------------------------

need_certs() {
	[[ -f certs/ripdev.io.crt && -f certs/ripdev.io.key ]]
}

need_caddy() {
	[[ -x "$CADDY_BIN" ]]
}

build_caddy() {
	mkdir -p bin
	go build -trimpath -o "$CADDY_BIN" ./cmd/janus
}

build_testkit() {
	go build -o "$TESTKIT" ./testkit
}

pid_is_owned() {
	local pid=$1 marker=$2 command
	[[ "$pid" =~ ^[0-9]+$ && -n "$marker" ]] || return 1
	command="$(ps -ww -p "$pid" -o command= 2>/dev/null)" || return 1
	[[ "$command" == *"$marker"* ]]
}

stop_owned_pid() {
	local pid=$1 marker=$2
	if pid_is_owned "$pid" "$marker"; then
		kill "$pid" 2>/dev/null || true
	elif [[ "$pid" =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null; then
		echo "refusing to stop pid $pid: command does not match $marker" >&2
	fi
}

require_ports_free() {
	local port listeners busy=0
	for port in "$@"; do
		listeners="$(lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | awk 'NR > 1 {print}' || true)"
		if [[ -n "$listeners" ]]; then
			echo "test port $port is already in use:" >&2
			printf '%s\n' "$listeners" >&2
			busy=1
		fi
	done
	if ((busy)); then
		echo "stop the owning service before running the Janus acceptance suite" >&2
		return 1
	fi
}

start_caddy() {
	printf '%s' "${JANUS_HEARTBEAT_TTL:-15s}" >"$TEST_RUN_DIR/heartbeat-ttl"
	# Fixed ports must be exclusive. Killing an unknown listener is unsafe, and
	# supervised services can immediately restart with SO_REUSEPORT and split
	# acceptance traffic between two Caddy processes.
	require_ports_free 443 2019 7600 8443 7680 7681 7602 8444 8081 || return 1
	rm -f "$TEST_RUN_DIR/run/janus.sock"
	sleep 0.5
	# Pin caddy storage so the internal CA root lands at a known path
	# (used by the tls group to verify on-demand minted chains).
	# JANUS_ABORT_TOKEN exists so the abort-recovery case's bad candidate
	# survives provisioning (token resolves) and fails only at Start.
	XDG_DATA_HOME="$TEST_RUN_DIR/caddy-data" \
	JANUS_ABORT_TOKEN="acceptance-abort-token" \
		"$CADDY_BIN" run --config "$ROOT/Caddyfile" >"$CADDY_LOG" 2>&1 &
	CADDY_PID=$!
	printf '%s\n' "$CADDY_PID" >"$CADDY_PID_FILE"
	local i
	for i in $(seq 1 50); do
		if ! kill -0 "$CADDY_PID" 2>/dev/null; then
			echo "caddy exited early; see $CADDY_LOG" >&2
			tail -20 "$CADDY_LOG" >&2 || true
			CADDY_PID=""
			rm -f "$CADDY_PID_FILE"
			return 1
		fi
		if curl -sS -o /dev/null --max-time 1 https://on.ripdev.io/ping 2>/dev/null \
			&& curl -sS -o /dev/null --max-time 1 http://127.0.0.1:7600/1.0/health 2>/dev/null; then
			return 0
		fi
		sleep 0.1
	done
	echo "caddy did not become ready; see $CADDY_LOG" >&2
	tail -20 "$CADDY_LOG" >&2 || true
	return 1
}

stop_caddy() {
	local pid="${CADDY_PID:-}"
	if [[ -s "$CADDY_PID_FILE" ]]; then
		pid="$(<"$CADDY_PID_FILE")"
	fi
	if pid_is_owned "$pid" "$CADDY_BIN"; then
		stop_owned_pid "$pid" "$CADDY_BIN"
		wait "$pid" 2>/dev/null || true
		local i
		for i in $(seq 1 100); do
			kill -0 "$pid" 2>/dev/null || break
			sleep 0.05
			done
	elif [[ -n "$pid" ]]; then
		stop_owned_pid "$pid" "$CADDY_BIN"
	fi
	rm -f "$CADDY_PID_FILE"
	CADDY_PID=""
}

cleanup() {
 local status=$?
 stop_caddy
 stop_data_fixtures
 stop_hub_fixtures
 stop_tenant
 stop_mdns_canon
 stop_auth_fixtures
 if ((status != 0 || FAIL != 0)) || [[ "${KEEP_TEST_STATE:-0}" == 1 ]]; then
  printf '\nfixture state retained at %s\n' "$TEST_RUN_DIR"
 else
  rm -rf "$TEST_RUN_DIR"
 fi
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

source "$ROOT/tests/acceptance/ping.sh"
source "$ROOT/tests/acceptance/control.sh"
source "$ROOT/tests/acceptance/reload.sh"
source "$ROOT/tests/acceptance/apps.sh"
source "$ROOT/tests/acceptance/data.sh"
source "$ROOT/tests/acceptance/heartbeat.sh"
source "$ROOT/tests/acceptance/tls.sh"
source "$ROOT/tests/acceptance/hub.sh"
source "$ROOT/tests/acceptance/tenant.sh"
source "$ROOT/tests/acceptance/mdns.sh"
source "$ROOT/tests/acceptance/auth.sh"
source "$ROOT/tests/acceptance/files.sh"
source "$ROOT/tests/acceptance/sendfile.sh"
source "$ROOT/tests/acceptance/browse.sh"
source "$ROOT/tests/acceptance/access.sh"

# --- main -----------------------------------------------------------------

SUITE_START_NS=$(now_ns)

printf '%s\n' "$(paint "$BOLD" "Janus acceptance")"

if ! need_certs; then
	echo "missing certs/ripdev.io.{crt,key}" >&2
	exit 1
fi

if ! need_caddy; then
	printf '%s\n' "$(paint "$DIM" "building $CADDY_BIN …")"
	build_caddy || exit 1
fi

printf '%s\n' "$(paint "$DIM" "building testkit …")"
build_testkit || exit 1

printf '%s\n' "$(paint "$DIM" "starting caddy …")"
start_caddy || exit 1

group "ping"
test "catchall foo.ripdev.io → pong" case_ping_catchall_foo
test "catchall bar.ripdev.io → pong" case_ping_catchall_bar
test "on.ripdev.io explicit on → pong" case_ping_on_explicit
test "off.ripdev.io explicit off → 404" case_ping_off_explicit
test "TLS verify trusted (no -k)" case_ping_tls_trusted

group "control"
test "local GET /1.0 → janus meta" case_control_local_root
test "config commands share environment precedence and reserved bind" case_config_environment
test "local GET /1.0/health → ok" case_control_local_health
test "unix GET /1.0 → janus meta" case_control_unix_root
test "unix GET /1.0/health → ok" case_control_unix_health
test "unknown /1.0 paths → 404, wrong method → 405" case_control_unknown_paths_404
test "control JSON rejects repeated fields at every depth" case_control_duplicate_json
test "reload → both listeners serve one live registry" case_reload_no_split_brain
test "changed TTL reload is rejected and reports the running value" case_reload_ttl_rejected
test "aborted reload → later reloads still work" case_reload_abort_recovery

group "apps"
test "register shop → 201 shop-xxxxxx" case_apps_register
test "register invalid bodies → 400" case_apps_register_bad
test "host already claimed → 409 names host+holder" case_apps_host_conflict
test "list apps → contains shop" case_apps_list
test "get app → name + hosts" case_apps_get
test "get unknown id → 404" case_apps_get_unknown
test "unix socket sees same registry" case_apps_unix_sees_registry
test "put upstreams → 200 stored" case_apps_put_upstreams
test "put empty upstreams → 200 (not routable)" case_apps_put_upstreams_empty
test "put mixed doorbell list → 400" case_apps_put_upstreams_mixed_doorbell
test "POST optional initial upstreams; PUT field remains strict" case_apps_initial_upstreams
test "delete app → 204, then 404" case_apps_delete
test "register app to survive restart" case_apps_register_survivor

printf '%s\n' "$(paint "$DIM" "restarting caddy …")"
stop_caddy
start_caddy || exit 1
test "restart → registry empty" case_apps_empty_after_restart

group "data"
test "register app + real unix upstream" case_data_register_with_upstream
test "GET routes to upstream over unix" case_data_proxy_get
test "POST body arrives at upstream" case_data_proxy_post_body
test "unknown host → 404" case_data_unknown_host
test "PUT upstreams [] → 503 + Retry-After" case_data_empty_upstreams_503
test "doorbell ring → body delivered once, no redirect" case_data_doorbell_ring
test "after ring: steady state on new upstream" case_data_after_ring_steady_state
test "registered host still answers /ping" case_data_ping_still_answers

group "heartbeat"
printf '%s\n' "$(paint "$DIM" "restarting caddy with JANUS_HEARTBEAT_TTL=2s …")"
stop_caddy
export JANUS_HEARTBEAT_TTL=2s
start_caddy || exit 1
unset JANUS_HEARTBEAT_TTL
test "register + upstream → traffic works" case_hb_register_traffic
test "POST heartbeat → 204" case_hb_beat_204
test "heartbeat unknown id → 404" case_hb_beat_unknown_404
test "silence past TTL → reaped: 404 + gone from list" case_hb_ttl_reaps
test "re-register + re-PUT → traffic recovers" case_hb_reregister_recovers
test "fresh beats + empty upstreams → 503, stays registered" case_hb_alive_not_routable

group "tls"
test "ask without domain → 400" case_tls_ask_missing_domain
test "ask unknown domain → 404" case_tls_ask_unknown_domain
test "register host → ask 200 (case-insensitive)" case_tls_register_allows
test "allowed host completes handshake (cert minted)" case_tls_allowed_host_minted
test "unregistered name → handshake denied" case_tls_denied_host_no_handshake
test "delete app → ask 404" case_tls_delete_denies
test "silence past TTL → ask 404" case_tls_reap_denies
test "alive but not routable → ask stays 200" case_tls_alive_not_routable_allowed

group "hub"
test "register apps + bridge tenant, /1.0/hub answers" case_hub_setup
test "direct mode works with empty upstreams and zero bridge calls" case_hub_direct_no_bridge
test "open handshake: bridge headers, tenant enrolls, snapshot agrees" case_hub_open_full_path
test "open rejected by tenant → status+body forwarded, no conn" case_hub_open_rejected_by_tenant
test "open with tenant down → 503 + Retry-After" case_hub_open_tenant_down
test "no bridge → 503" case_hub_no_bridge
test "bare name excludes sender's connection; ! includes + strips" case_hub_exclusion_rules
test "publish ignores ! spelling (no originating conn)" case_hub_publish_ignores_exclusion
test "? answers ! at the edge; frame still bridged" case_hub_pong_at_edge
test "< stamped as [connection-id] on client deliveries" case_hub_provenance_stamped
test "client-supplied < → close 1008, close bridged" case_hub_spoof_rejected
test "> reserved / exact ! / client * / binary all reject" case_hub_reserved_sigils_reject
test "join/leave bookkeeping via snapshot; empty channel gone" case_hub_join_leave_snapshot
test "client membership is sender-only; publish can enroll" case_hub_sender_only_membership
test "bare channel name in + → close 1008" case_hub_channel_grammar_rejects
test "hierarchy is naming-only: /h misses /h/sub" case_hub_naming_only_hierarchy
test "per-app isolation: same channel name, separate hubs" case_hub_per_app_isolation
test "text bridge observes frames in order, verbatim" case_hub_text_bridge_observation
test "bridge-response directives run in sender's context" case_hub_bridge_response_directives
test "bridge garbage counted, sender unaffected" case_hub_bridge_garbage
test "atomic rejection: nothing from a rejected frame applies" case_hub_atomic_rejection
test "text-bridge failure invisible to clients" case_hub_text_failure_invisible
test "reload invisibility: fan-out mid-window, held bridge completes" case_hub_reload_invisibility
test "DELETE → sockets close 1001, snapshot 404s" case_hub_teardown_on_delete
test "slow consumer → close 1013, counted, close bridged" case_hub_slow_consumer
test "oversize frame → close 1009" case_hub_oversize_frame
test "max_conns floor 10 across hosts; reservation rejects the 11th" case_hub_cap_floor_and_reservation
test "open/teardown race: tombstone wins, no zombie" case_hub_open_teardown_race
test "origin same rejects before any bridge; any admits" case_hub_origin_policy
test "hub claims upgrades only; hub-off site never intercepts" case_hub_interception_scope
test "publish plane: 400 positioned / 404 / 409" case_hub_publish_plane_errors
test "publish * kick: frame, close 1000, close bridge" case_hub_publish_kick
test "janus reload: socket, membership, fan-out survive" case_hub_caddy_reload_persistence
test "handshake snapshot over 32KiB → 431" case_hub_bridge_snapshot_cap
test "snapshot exposes opaque handles, never raw ids" case_hub_snapshot_opacity
test "parse rejections: every hub hard error fails janus adapt" case_hub_parse_rejections

group "tenant"
test "real manager registers on /1.0" case_tenant_register
test "GET / routes through Janus to a worker" case_tenant_get_json
test "POST /echo body arrives intact" case_tenant_echo_body
test "save → ring → response is the NEW code" case_tenant_reload_fresh_code
test "heartbeats keep the app alive past the TTL" case_tenant_heartbeats_keep_alive
test "SIGTERM → clean deregistration" case_tenant_sigterm_deregisters

group "mdns"
test "advertiser state: enabled, named, settled" case_mdns_state
test "front door serves page + status.json, no-store" case_mdns_front_door
test "Host allowlist: name + IP served, others 421" case_mdns_host_allowlist
test "read-only: wrong method 405, unknown path 404" case_mdns_read_only
test "registration advertises the .local host only" case_mdns_register_advertises
test "site alias advertises and withdraws; pattern never advertises" case_mdns_site_alias_advertises_and_withdraws
test "multi-label .local: skipped gauge up, never advertised, down on DELETE" case_mdns_multilabel_gauge
test "status page truth: hosts, upstream health, heartbeat age" case_mdns_status_truth
test "redaction: socket path and bridge bytes absent" case_mdns_redaction
test "PATCH reconciles exactly the diff" case_mdns_patch_reconciles
test "DELETE withdraws, counter moves" case_mdns_delete_withdraws
test "TTL reap withdraws" case_mdns_reap_withdraws
test "reload no-flap: set identical, counters unmoved" case_mdns_reload_no_flap
test "reload teardown: enabled:false, port closed, restore returns" case_mdns_reload_teardown
test "canonical present: /1.0/mdns + status.json + probe script" case_mdns_canonical_present
test "parse rejections: every mdns hard error fails janus adapt" case_mdns_parse_rejections

group "auth"
test "register app + recording upstream, /1.0/auth answers" case_auth_setup
test "wall blocks: browser 302 + to, API 401, upstream untouched" case_auth_wall_blocks
test "login round trip: 303 + cookie; upstream sees Remote-User, never the cookie" case_auth_login_roundtrip
test "to survives login; hostile targets collapse to /" case_auth_to
test "status page + sign-out: re-minted CSRF, revoked server-side" case_auth_status_signout
test "CSRF enforced on both POST arms" case_auth_csrf_enforced
test "spoofed Remote-User dies at the edge" case_auth_spoof_dies
test "fixation dead: pre-set cookie never honored, never re-minted" case_auth_fixation_dead
test "wrong password and unknown user → one generic 401" case_auth_wrong_creds
test "throttle ladder: 5th try during wait → 429; pairs isolated" case_auth_throttle
test "prefix gates: longest prefix, allow lists, one host session" case_auth_prefix_gates
test "/ping answers unauthenticated on the guarded site" case_auth_ping_open
test "/auth exact match only: neighbors proxy" case_auth_exact_match_only
test "WS upgrade: 401 without a session; bridge carries Remote-User with one" case_auth_ws
test "cascade: site users replace global; sessions authorized per request" case_auth_cascade_users
test "reload keeps sessions (pooled store)" case_auth_reload_keeps_sessions
test "reload revokes the removed user, keeps the rest" case_auth_reload_revokes_removed_user
test "hot surface: list, revoke one, wipe all" case_auth_hot_revoke
test "passhash mints; minted cred opens a wall; plain HTTP → 421" case_auth_minter_and_dead_wall
test "restart wipes every session" case_auth_restart_wipes
test "zero-users lockout fails janus validate" case_auth_zero_users_lockout
test "parse rejections: every auth hard error fails janus adapt" case_auth_parse_rejections
stop_auth_fixtures

group "files"
test "register exact-host app with ordered roots" case_files_setup
test "ordered roots + ETag + HEAD + range" case_files_order_and_http_semantics
test "SPA shell, proxy_first, strict request path" case_files_shell_proxy_first_and_paths
test "independent MIME and cache policies" case_files_response_policy
test "precompressed Brotli is transparent with canonical headers" case_files_precompressed_brotli
test "precompressed q-values, wildcard, q=0, and identity" case_files_precompressed_negotiation
test "precompressed fallback, removal, canonical-first, same-root" case_files_precompressed_fallback_and_root
test "precompressed representation-specific conditionals" case_files_precompressed_conditionals
test "precompressed HEAD, encoded range, and no double encode" case_files_precompressed_head_range_and_encode
test "precompressed SPA shell and directory index" case_files_precompressed_shell_and_index
test "FIFO canonical files and sidecars never block" case_files_special_files
test "strict site/files JSON fields reject" case_files_strict_hot_fields
test "cascade: site files off beats global on" case_files_cascade_off

group "sendfile"
test "register ordinary upstream; no sendfile configuration" case_sendfile_setup
test "GET, HEAD, and byte range stream the selected file" case_sendfile_get_head_and_range
test "application metadata wins; conditional request returns 304" case_sendfile_metadata_and_conditional
test "missing target returns 502 without leaking instruction" case_sendfile_failure_strips_instruction

group "browse"
test "hot browsable root lists, previews, redirects, and indexes" case_browse_setup_and_listing
test "raw delivery and one renderer work across hosts" case_browse_raw_and_renderer
test "renderer saturation, timeout, and failure statuses" case_browse_renderer_bounds
test "site off, heartbeat reap, process reload, and redacted status" case_browse_cascade_lease_and_status
test "cold root survives restart and conflicts with hot claims" case_browse_cold_restart_and_conflict

group "access"
test "register process app and report exact access status" case_access_setup_and_status
test "stream one event and retain durable JSON" case_access_stream_and_durable_json
test "strict method and cursor syntax reject" case_access_protocol_rejections

report
exit $?
