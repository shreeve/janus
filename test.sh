#!/usr/bin/env bash
# test.sh — high-level Janus acceptance suite (self-contained).
#
# For operators/users: prove cold capabilities behave end-to-end.
# Developers still use idiomatic `go test ./...` while building.
#
# Groups run in capability order: ping (1), then control (2), then …
#
#   ./test.sh
#   NO_COLOR=1 ./test.sh
#
set -uo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

CADDY_BIN="${CADDY_BIN:-$ROOT/bin/caddy}"
CADDY_LOG="${CADDY_LOG:-$ROOT/.test-caddy.log}"
CADDY_PID=""

# testkit: the suite's Go support binary (fixture servers, WS driver,
# JSON/string utilities). Built fresh at suite start from ./testkit.
TESTKIT="$ROOT/.test-testkit"

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
	command -v xcaddy >/dev/null 2>&1 || {
		echo "xcaddy not found; install: go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest" >&2
		return 1
	}
	mkdir -p bin
	xcaddy build --with github.com/shreeve/janus=. --output "$CADDY_BIN"
}

build_testkit() {
	go build -o "$TESTKIT" ./testkit
}

kill_listeners() {
	local port=$1
	if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
		lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | awk 'NR>1 {print $2}' | sort -u | while read -r pid; do
			kill "$pid" 2>/dev/null || true
		done
	fi
}

start_caddy() {
	# clear stale listeners from prior runs
	kill_listeners 443
	kill_listeners 7600
	kill_listeners 8443
	rm -f "$ROOT/run/janus.sock"
	sleep 0.5
	# Pin caddy storage so the internal CA root lands at a known path
	# (used by the tls group to verify on-demand minted chains).
	XDG_DATA_HOME="$ROOT/.test-caddy-data" \
		"$CADDY_BIN" run --config "$ROOT/Caddyfile" >"$CADDY_LOG" 2>&1 &
	CADDY_PID=$!
	local i
	for i in $(seq 1 50); do
		if ! kill -0 "$CADDY_PID" 2>/dev/null; then
			echo "caddy exited early; see $CADDY_LOG" >&2
			tail -20 "$CADDY_LOG" >&2 || true
			CADDY_PID=""
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
	if [[ -n "${CADDY_PID:-}" ]] && kill -0 "$CADDY_PID" 2>/dev/null; then
		kill "$CADDY_PID" 2>/dev/null || true
		wait "$CADDY_PID" 2>/dev/null || true
	fi
	CADDY_PID=""
}

cleanup() {
	stop_caddy
	stop_data_fixtures
	stop_hub_fixtures
	stop_tenant
	rm -f "$ROOT/.test-app-id" "$ROOT/.test-hb-app-id" "$ROOT/.test-tls-app-id" \
		"$ROOT/.test-cache-app-id" "$ROOT/.test-fixtures.log" \
		"$ROOT"/.test-cache-burst-* "$ROOT"/.test-cache-fail-* \
		"$ROOT/.test-cache-cap-codes" "$ROOT/.test-cache-race"
}

trap cleanup EXIT INT TERM

# --- cases: ping ----------------------------------------------------------

# $( ) strips trailing newlines, so bodies compare without the final \n.
case_ping_catchall_foo() {
	eq "$(http_body https://foo.ripdev.io/ping)" "pong"
	eq "$(http_code https://foo.ripdev.io/ping)" "200"
}

case_ping_catchall_bar() {
	eq "$(http_body https://bar.ripdev.io/ping)" "pong"
	eq "$(http_code https://bar.ripdev.io/ping)" "200"
}

case_ping_on_explicit() {
	eq "$(http_body https://on.ripdev.io/ping)" "pong"
	eq "$(http_code https://on.ripdev.io/ping)" "200"
}

case_ping_off_explicit() {
	eq "$(http_code https://off.ripdev.io/ping)" "404"
	ne "$(http_body https://off.ripdev.io/ping)" "pong"
}

case_ping_tls_trusted() {
	# curl verify result 0 = chain trusted (no -k)
	local v
	v="$(curl -sS -o /dev/null -w '%{ssl_verify_result}' --max-time 5 https://on.ripdev.io/ping)"
	eq "$v" "0"
}

# --- cases: control -------------------------------------------------------

json_has() {
	local body=$1 needle=$2
	if ! printf '%s' "$body" | grep -qF "$needle"; then
		printf 'missing %q in %q' "$needle" "$body" >&2
		return 1
	fi
}

case_control_local_root() {
	local body
	body="$(http_body http://127.0.0.1:7600/1.0)"
	eq "$(http_code http://127.0.0.1:7600/1.0)" "200"
	json_has "$body" '"api_version":"1.0"'
	json_has "$body" '"type":"janus"'
}

case_control_local_health() {
	local body
	body="$(http_body http://127.0.0.1:7600/1.0/health)"
	eq "$(http_code http://127.0.0.1:7600/1.0/health)" "200"
	json_has "$body" '"status":"ok"'
}

case_control_unix_root() {
	ok "-S \"$ROOT/run/janus.sock\"" "missing unix socket"
	local body code
	body="$(curl -sS --max-time 5 --unix-socket "$ROOT/run/janus.sock" http://janus/1.0)"
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 --unix-socket "$ROOT/run/janus.sock" http://janus/1.0)"
	eq "$code" "200"
	json_has "$body" '"type":"janus"'
}

case_control_unix_health() {
	local body code
	body="$(curl -sS --max-time 5 --unix-socket "$ROOT/run/janus.sock" http://janus/1.0/health)"
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 --unix-socket "$ROOT/run/janus.sock" http://janus/1.0/health)"
	eq "$code" "200"
	json_has "$body" '"status":"ok"'
}

case_control_unknown_paths_404() {
	# Typos and wrong-method calls must never look alive.
	eq "$(http_code http://127.0.0.1:7600/1.0/bogus)" "404"
	local code
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -X GET \
		http://127.0.0.1:7600/1.0/apps/nope-zzzzzz/heartbeat)"
	eq "$code" "405"
}

# --- cases: reload -----------------------------------------------------------

case_reload_no_split_brain() {
	# A config reload swaps in a new Janus app while the old one still holds
	# its sockets: listener pooling lets the new app bind, the reload
	# succeeds, and afterward BOTH control listeners serve the same live
	# registry. The registry lives in pooled process state
	# (docs/20260720-162350-hub-design.md "Caddy config reload"): the
	# pre-reload registration SURVIVES the reload — only DELETE, TTL reap,
	# or a process restart removes it. Split-brain would show a fresh,
	# empty registry behind one listener.
	capi POST /1.0/apps '{"name":"reload","hosts":["reload.example.com"]}'
	eq "$REPLY_CODE" "201"
	local old_id
	old_id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	ok "-n \"$old_id\"" "no id in $REPLY_BODY"

	if ! XDG_DATA_HOME="$ROOT/.test-caddy-data" \
		"$CADDY_BIN" reload --config "$ROOT/Caddyfile" --force >>"$CADDY_LOG" 2>&1; then
		echo "caddy reload failed; see $CADDY_LOG" >&2
		return 1
	fi

	# Both listeners answer, and both see the same live registry.
	local i ready=""
	for i in $(seq 1 50); do
		if curl -sS -o /dev/null --max-time 1 http://127.0.0.1:7600/1.0/health 2>/dev/null &&
			curl -sS -o /dev/null --max-time 1 --unix-socket "$ROOT/run/janus.sock" http://janus/1.0/health 2>/dev/null; then
			ready=1
			break
		fi
		sleep 0.1
	done
	ok "-n \"$ready\"" "control listeners never answered after reload"

	# The pre-reload registration survived the reload on BOTH listeners.
	capi GET /1.0/apps
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" "\"$old_id\""
	capi_unix GET /1.0/apps
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" "\"$old_id\""
	# Its host is still claimed: a rival registration conflicts.
	capi POST /1.0/apps '{"name":"rival","hosts":["reload.example.com"]}'
	eq "$REPLY_CODE" "409"
	capi DELETE "/1.0/apps/$old_id"
	eq "$REPLY_CODE" "204"
}

# --- cases: apps -----------------------------------------------------------

APP_ID_FILE="$ROOT/.test-app-id"

# capi METHOD PATH [JSON] — control API over local TCP; sets REPLY_CODE / REPLY_BODY
capi() {
	local method=$1 path=$2 data=${3:-} resp
	if [[ -n "$data" ]]; then
		resp="$(curl -sS --max-time 5 -X "$method" -H 'Content-Type: application/json' \
			--data "$data" -w $'\n%{http_code}' "http://127.0.0.1:7600$path")"
	else
		resp="$(curl -sS --max-time 5 -X "$method" -w $'\n%{http_code}' "http://127.0.0.1:7600$path")"
	fi
	REPLY_CODE="${resp##*$'\n'}"
	REPLY_BODY="${resp%$'\n'*}"
}

# capi_unix METHOD PATH [JSON] — same, over the internal unix socket
capi_unix() {
	local method=$1 path=$2 data=${3:-} resp
	if [[ -n "$data" ]]; then
		resp="$(curl -sS --max-time 5 --unix-socket "$ROOT/run/janus.sock" -X "$method" \
			-H 'Content-Type: application/json' --data "$data" -w $'\n%{http_code}' "http://janus$path")"
	else
		resp="$(curl -sS --max-time 5 --unix-socket "$ROOT/run/janus.sock" -X "$method" \
			-w $'\n%{http_code}' "http://janus$path")"
	fi
	REPLY_CODE="${resp##*$'\n'}"
	REPLY_BODY="${resp%$'\n'*}"
}

app_id() {
	cat "$APP_ID_FILE"
}

case_apps_register() {
	capi POST /1.0/apps '{"name":"shop","hosts":["shop.example.com"]}'
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	if [[ ! "$id" =~ ^shop-[a-z0-9]{6}$ ]]; then
		printf 'id %q does not match shop-xxxxxx in %q' "$id" "$REPLY_BODY" >&2
		return 1
	fi
	printf '%s' "$id" >"$APP_ID_FILE"
}

case_apps_register_bad() {
	capi POST /1.0/apps '{"hosts":["a.example.com"]}'
	eq "$REPLY_CODE" "400"
	capi POST /1.0/apps '{"name":"shop2","hosts":[]}'
	eq "$REPLY_CODE" "400"
	capi POST /1.0/apps '{"name":"shop2","hosts":["not a host"]}'
	eq "$REPLY_CODE" "400"
	capi POST /1.0/apps 'not json'
	eq "$REPLY_CODE" "400"
}

case_apps_host_conflict() {
	capi POST /1.0/apps '{"name":"rival","hosts":["shop.example.com"]}'
	eq "$REPLY_CODE" "409"
	json_has "$REPLY_BODY" 'shop.example.com'
	json_has "$REPLY_BODY" "$(app_id)"
}

case_apps_list() {
	capi GET /1.0/apps
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" "\"$(app_id)\""
}

case_apps_get() {
	capi GET "/1.0/apps/$(app_id)"
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"name":"shop"'
	json_has "$REPLY_BODY" '"shop.example.com"'
}

case_apps_get_unknown() {
	capi GET /1.0/apps/shop-zzzzzz
	eq "$REPLY_CODE" "404"
}

case_apps_unix_sees_registry() {
	capi_unix GET /1.0/apps
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" "\"$(app_id)\""
}

case_apps_put_upstreams() {
	capi PUT "/1.0/apps/$(app_id)/upstreams" \
		'{"upstreams":[{"path":"/run/w1.sock"},{"path":"/run/w2.sock"}]}'
	eq "$REPLY_CODE" "200"
	capi GET "/1.0/apps/$(app_id)"
	json_has "$REPLY_BODY" '"/run/w1.sock"'
	json_has "$REPLY_BODY" '"/run/w2.sock"'
}

case_apps_put_upstreams_empty() {
	capi PUT "/1.0/apps/$(app_id)/upstreams" '{"upstreams":[]}'
	eq "$REPLY_CODE" "200"
	capi GET "/1.0/apps/$(app_id)"
	json_has "$REPLY_BODY" '"upstreams":[]'
}

case_apps_put_upstreams_mixed_doorbell() {
	capi PUT "/1.0/apps/$(app_id)/upstreams" \
		'{"upstreams":[{"path":"/run/bell.sock","doorbell":true},{"path":"/run/w1.sock"}]}'
	eq "$REPLY_CODE" "400"
	json_has "$REPLY_BODY" 'doorbell'
}

case_apps_delete() {
	capi DELETE "/1.0/apps/$(app_id)"
	eq "$REPLY_CODE" "204"
	capi GET "/1.0/apps/$(app_id)"
	eq "$REPLY_CODE" "404"
}

case_apps_register_survivor() {
	# Register an app that exists when Janus restarts; it must not survive.
	capi POST /1.0/apps '{"name":"ghost","hosts":["ghost.example.com"]}'
	eq "$REPLY_CODE" "201"
}

case_apps_empty_after_restart() {
	capi GET /1.0/apps
	eq "$REPLY_CODE" "200"
	eq "$(printf '%s' "$REPLY_BODY" | tr -d '[:space:]')" "[]"
}

# --- cases: data ------------------------------------------------------------

# Case bodies run inside the runner's $( ) subshell, so fixture bookkeeping
# must go through files (like APP_ID_FILE) — array appends in a subshell
# never reach the parent, and the cleaner would leak every fixture server.
DATA_APP_FILE="$ROOT/.test-data-app-id"
DATA_PIDS_FILE="$ROOT/.test-data-pids"
DATA_SOCKS_FILE="$ROOT/.test-data-socks"
DATA_HITFILES_FILE="$ROOT/.test-data-files"

data_app_id() {
	cat "$DATA_APP_FILE"
}

# start_data_upstream SOCK NAME HITFILE — HTTP/1.1 echo server on a unix socket.
# GET / → "upstream:NAME"; POST → append body to HITFILE, echo "received:BODY".
start_data_upstream() {
	local sock=$1 name=$2 hitfile=$3
	rm -f "$sock"
	: >"$hitfile"
	printf '%s\n' "$sock" >>"$DATA_SOCKS_FILE"
	printf '%s\n' "$hitfile" >>"$DATA_HITFILES_FILE"
	# detach stdout/stderr: the runner captures case output via $( ) and
	# would otherwise wait for this background server to exit
	"$TESTKIT" upstream --sock "$sock" --name "$name" --hits "$hitfile" \
		>>"$ROOT/.test-fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$DATA_PIDS_FILE"
	local i
	for i in $(seq 1 50); do
		[[ -S "$sock" ]] && return 0
		sleep 0.1
	done
	echo "upstream socket $sock never appeared" >&2
	return 1
}

# start_data_doorbell SOCK APPID NEWSOCK RINGFILE — on GET /ring, PUT NEWSOCK
# as the app's real upstream via /1.0 (awaits the 200), then answer 204.
start_data_doorbell() {
	local sock=$1 appid=$2 newsock=$3 ringfile=$4
	rm -f "$sock"
	: >"$ringfile"
	printf '%s\n' "$sock" >>"$DATA_SOCKS_FILE"
	printf '%s\n' "$ringfile" >>"$DATA_HITFILES_FILE"
	"$TESTKIT" doorbell --sock "$sock" --app "$appid" --newsock "$newsock" --ring "$ringfile" \
		>>"$ROOT/.test-fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$DATA_PIDS_FILE"
	local i
	for i in $(seq 1 50); do
		[[ -S "$sock" ]] && return 0
		sleep 0.1
	done
	echo "doorbell socket $sock never appeared" >&2
	return 1
}

stop_data_fixtures() {
	local pid f path
	if [[ -f "$DATA_PIDS_FILE" ]]; then
		while read -r pid; do
			kill "$pid" 2>/dev/null || true
		done <"$DATA_PIDS_FILE"
	fi
	for f in "$DATA_SOCKS_FILE" "$DATA_HITFILES_FILE"; do
		if [[ -f "$f" ]]; then
			while read -r path; do
				rm -f "$path"
			done <"$f"
		fi
	done
	rm -f "$DATA_PIDS_FILE" "$DATA_SOCKS_FILE" "$DATA_HITFILES_FILE" "$DATA_APP_FILE"
}

case_data_register_with_upstream() {
	start_data_upstream "$ROOT/run/u1.sock" u1 "$ROOT/.test-u1.hits" || return 1
	capi POST /1.0/apps '{"name":"web","hosts":["app.ripdev.io"]}'
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	ok "-n \"$id\"" "no id in $REPLY_BODY"
	printf '%s' "$id" >"$DATA_APP_FILE"
	capi PUT "/1.0/apps/$id/upstreams" "{\"upstreams\":[{\"path\":\"$ROOT/run/u1.sock\"}]}"
	eq "$REPLY_CODE" "200"
}

case_data_proxy_get() {
	eq "$(http_code https://app.ripdev.io/)" "200"
	eq "$(http_body https://app.ripdev.io/)" "upstream:u1"
}

case_data_proxy_post_body() {
	local body
	body="$(curl -sS --max-time 5 -X POST --data 'alpha' https://app.ripdev.io/submit)"
	eq "$body" "received:alpha"
	eq "$(wc -l <"$ROOT/.test-u1.hits" | tr -d ' ')" "1"
}

case_data_unknown_host() {
	eq "$(http_code https://nowhere.ripdev.io/)" "404"
}

case_data_empty_upstreams_503() {
	capi PUT "/1.0/apps/$(data_app_id)/upstreams" '{"upstreams":[]}'
	eq "$REPLY_CODE" "200"
	eq "$(http_code https://app.ripdev.io/)" "503"
	local ra
	ra="$(curl -sS -o /dev/null -D - --max-time 5 https://app.ripdev.io/ |
		tr -d '\r' | awk -F': ' 'tolower($1)=="retry-after" {print $2}')"
	eq "$ra" "1"
}

case_data_doorbell_ring() {
	start_data_upstream "$ROOT/run/u2.sock" u2 "$ROOT/.test-u2.hits" || return 1
	start_data_doorbell "$ROOT/run/bell.sock" "$(data_app_id)" "$ROOT/run/u2.sock" "$ROOT/.test-bell.rings" || return 1
	capi PUT "/1.0/apps/$(data_app_id)/upstreams" \
		"{\"upstreams\":[{\"path\":\"$ROOT/run/bell.sock\",\"doorbell\":true}]}"
	eq "$REPLY_CODE" "200"

	# Client POST with a body while only the doorbell is published:
	# the ring swaps in u2 and the body arrives there intact, exactly once,
	# with no visible redirect.
	local resp code body
	resp="$(curl -sS --max-time 20 -X POST --data 'door-payload' \
		-w $'\n%{http_code} %{num_redirects}' https://app.ripdev.io/submit)"
	code="${resp##*$'\n'}"
	body="${resp%$'\n'*}"
	eq "$code" "200 0"
	eq "$body" $'received:door-payload\n'
	eq "$(wc -l <"$ROOT/.test-u2.hits" | tr -d ' ')" "1"
	eq "$(cat "$ROOT/.test-u2.hits")" "door-payload"
	eq "$(wc -l <"$ROOT/.test-bell.rings" | tr -d ' ')" "1"
	eq "$(wc -l <"$ROOT/.test-u1.hits" | tr -d ' ')" "1" # old upstream got nothing new
}

case_data_after_ring_steady_state() {
	# The doorbell is retired; traffic flows to u2 without ringing again.
	eq "$(http_body https://app.ripdev.io/)" "upstream:u2"
	eq "$(wc -l <"$ROOT/.test-bell.rings" | tr -d ' ')" "1"
}

case_data_ping_still_answers() {
	# Site-scoped ping (global on) answers ahead of routing for this host.
	eq "$(http_body https://app.ripdev.io/ping)" "pong"
}

# --- cases: cache -------------------------------------------------------------
#
# Capability 3: micro-cache + request coalescing
# (docs/20260720-033201-capability-microcache.md "Acceptance sketch").
# The instrument: a fixture upstream that records every request it receives
# (tenant-side truth) plus /1.0/cache counters (Janus-side truth); every
# case asserts both sides. Counter asserts use deltas — earlier groups also
# move the process-wide counters. The doorkeeper admits a key on its second
# sighting, so hit tests use three requests, not two.
#
# Hosts (root Caddyfile): cachetest.ripdev.io inherits global cache on
# (ttl 1s, no debug); pages.ripdev.io overrides ttl 5s + debug;
# api.ripdev.io is cache off.

CACHE_APP_FILE="$ROOT/.test-cache-app-id"
CACHE_HITS1="$ROOT/.test-cache1.hits"
CACHE_HITS2="$ROOT/.test-cache2.hits"
CACHE_SOCK1="$ROOT/run/cache1.sock"
CACHE_SOCK2="$ROOT/run/cache2.sock"

cache_app_id() { cat "$CACHE_APP_FILE"; }

# cache_stat KEY — one process-total counter from GET /1.0/cache
cache_stat() {
	capi GET /1.0/cache
	printf '%s' "$REPLY_BODY" | "$TESTKIT" json get "$1"
}

# path_hits HITFILE PATH — how many requests for exactly PATH the fixture
# received (exact line match: /slow must not count /slower).
path_hits() {
	local file=$1 path=$2
	if [[ ! -f "$file" ]]; then
		echo 0
		return
	fi
	grep -cxF -- "$path" "$file" || true
}

cache_hb() {
	capi POST "/1.0/apps/$(cache_app_id)/heartbeat"
	eq "$REPLY_CODE" "204"
}

# start_cache_upstream SOCK HITFILE — one fixture server, per-path behavior.
start_cache_upstream() {
	local sock=$1 hitfile=$2
	rm -f "$sock"
	: >"$hitfile"
	printf '%s\n' "$sock" >>"$DATA_SOCKS_FILE"
	printf '%s\n' "$hitfile" >>"$DATA_HITFILES_FILE"
	"$TESTKIT" cacheup --sock "$sock" --hits "$hitfile" \
		>>"$ROOT/.test-fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$DATA_PIDS_FILE"
	local i
	for i in $(seq 1 50); do
		[[ -S "$sock" ]] && return 0
		sleep 0.1
	done
	echo "cache upstream socket $sock never appeared" >&2
	return 1
}

case_cache_register() {
	start_cache_upstream "$CACHE_SOCK1" "$CACHE_HITS1" || return 1
	start_cache_upstream "$CACHE_SOCK2" "$CACHE_HITS2" || return 1
	capi POST /1.0/apps \
		'{"name":"cachet","hosts":["cachetest.ripdev.io","pages.ripdev.io","api.ripdev.io"]}'
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	ok "-n \"$id\"" "no id in $REPLY_BODY"
	printf '%s' "$id" >"$CACHE_APP_FILE"
	capi PUT "/1.0/apps/$id/upstreams" "{\"upstreams\":[{\"path\":\"$CACHE_SOCK1\"}]}"
	eq "$REPLY_CODE" "200"
	capi GET /1.0/cache
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"hits"'
	json_has "$REPLY_BODY" '"fenced_stores"'
}

case_cache_hit_serves_without_worker() {
	cache_hb
	local h0 a0 b1 b3 v
	h0="$(cache_stat hits)"
	a0="$(cache_stat admission_rejects)"
	b1="$(curl -sS --max-time 10 -D "$ROOT/.test-cache-h1" https://pages.ripdev.io/page)"
	curl -sS --max-time 10 -o /dev/null https://pages.ripdev.io/page
	b3="$(curl -sS --max-time 10 -D "$ROOT/.test-cache-h3" https://pages.ripdev.io/page)"
	eq "$(path_hits "$CACHE_HITS1" /page)" "2" # doorkeeper admits on the second fill
	eq "$b3" "$b1"
	# The hit carries Age and the debug verdict (pages has debug on).
	v="$(tr -d '\r' <"$ROOT/.test-cache-h3" | awk -F': ' 'tolower($1)=="x-janus-cache" {print $2}')"
	eq "$v" "HIT"
	v="$(tr -d '\r' <"$ROOT/.test-cache-h1" | awk -F': ' 'tolower($1)=="x-janus-cache" {print $2}')"
	eq "$v" "MISS"
	if ! tr -d '\r' <"$ROOT/.test-cache-h3" | grep -qi '^age: '; then
		echo "hit response missing Age header" >&2
		return 1
	fi
	eq "$(($(cache_stat hits) - h0))" "1"
	eq "$(($(cache_stat admission_rejects) - a0))" "1"
	rm -f "$ROOT/.test-cache-h1" "$ROOT/.test-cache-h3"
}

case_cache_cookie_bypass() {
	cache_hb
	local by0 s0 i v
	by0="$(cache_stat bypass)"
	s0="$(cache_stat stores)"
	for i in 1 2 3; do
		v="$(curl -sS --max-time 10 -o /dev/null -D - -H 'Cookie: a=1' https://pages.ripdev.io/cookiepage |
			tr -d '\r' | awk -F': ' 'tolower($1)=="x-janus-cache" {print $2}')"
		eq "$v" "BYPASS"
	done
	eq "$(path_hits "$CACHE_HITS1" /cookiepage)" "3"
	eq "$(($(cache_stat bypass) - by0))" "3"
	eq "$(($(cache_stat stores) - s0))" "0"
}

case_cache_auth_bypass() {
	cache_hb
	curl -sS --max-time 10 -o /dev/null -H 'Authorization: Bearer x' https://cachetest.ripdev.io/authpage
	curl -sS --max-time 10 -o /dev/null -H 'Authorization: Bearer x' https://cachetest.ripdev.io/authpage
	curl -sS --max-time 10 -o /dev/null -H 'Proxy-Authorization: Basic x' https://cachetest.ripdev.io/authpage
	eq "$(path_hits "$CACHE_HITS1" /authpage)" "3"
}

case_cache_post_bypass() {
	cache_hb
	curl -sS --max-time 10 -o /dev/null -X POST --data a https://cachetest.ripdev.io/postpage
	curl -sS --max-time 10 -o /dev/null -X POST --data b https://cachetest.ripdev.io/postpage
	eq "$(path_hits "$CACHE_HITS1" /postpage)" "2"
}

case_cache_setcookie_never_stored() {
	cache_hb
	local s0 i
	s0="$(cache_stat stores)"
	for i in 1 2 3; do
		curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/setcookie
	done
	eq "$(path_hits "$CACHE_HITS1" /setcookie)" "3"
	eq "$(($(cache_stat stores) - s0))" "0"
}

case_cache_origin_vetoes_respected() {
	cache_hb
	local s0 p
	s0="$(cache_stat stores)"
	for p in /nostore /private /badcc /expires; do
		curl -sS --max-time 10 -o /dev/null "https://cachetest.ripdev.io$p"
		curl -sS --max-time 10 -o /dev/null "https://cachetest.ripdev.io$p"
		eq "$(path_hits "$CACHE_HITS1" "$p")" "2"
	done
	eq "$(($(cache_stat stores) - s0))" "0"
}

case_cache_content_encoding() {
	cache_hb
	local s0
	s0="$(cache_stat stores)"
	# gzip without Vary: never stored — every repeat reaches the worker.
	curl -sS --max-time 10 -o /dev/null -H 'Accept-Encoding: gzip' https://cachetest.ripdev.io/ce
	curl -sS --max-time 10 -o /dev/null -H 'Accept-Encoding: gzip' https://cachetest.ripdev.io/ce
	eq "$(path_hits "$CACHE_HITS1" /ce)" "2"
	eq "$(($(cache_stat stores) - s0))" "0"
	# With Vary: Accept-Encoding it stores per variant: third is a HIT.
	curl -sS --max-time 10 -o /dev/null -H 'Accept-Encoding: gzip' https://cachetest.ripdev.io/cevary
	curl -sS --max-time 10 -o /dev/null -H 'Accept-Encoding: gzip' https://cachetest.ripdev.io/cevary
	curl -sS --max-time 10 -o /dev/null -H 'Accept-Encoding: gzip' https://cachetest.ripdev.io/cevary
	eq "$(path_hits "$CACHE_HITS1" /cevary)" "2"
	eq "$(($(cache_stat stores) - s0))" "1"
}

case_cache_acao() {
	cache_hb
	local s0
	s0="$(cache_stat stores)"
	# Echoed ACAO: never stored.
	curl -sS --max-time 10 -o /dev/null -H 'Origin: https://a.test' https://cachetest.ripdev.io/acao-echo
	curl -sS --max-time 10 -o /dev/null -H 'Origin: https://b.test' https://cachetest.ripdev.io/acao-echo
	eq "$(path_hits "$CACHE_HITS1" /acao-echo)" "2"
	eq "$(($(cache_stat stores) - s0))" "0"
	# Static * stores.
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/acao-star
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/acao-star
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/acao-star
	eq "$(path_hits "$CACHE_HITS1" /acao-star)" "2"
	eq "$(($(cache_stat stores) - s0))" "1"
}

case_cache_vary_respected() {
	cache_hb
	local body
	curl -sS --max-time 10 -o /dev/null -H 'Accept-Language: de' https://cachetest.ripdev.io/vary-lang
	curl -sS --max-time 10 -o /dev/null -H 'Accept-Language: de' https://cachetest.ripdev.io/vary-lang
	curl -sS --max-time 10 -o /dev/null -H 'Accept-Language: en' https://cachetest.ripdev.io/vary-lang
	curl -sS --max-time 10 -o /dev/null -H 'Accept-Language: en' https://cachetest.ripdev.io/vary-lang
	# The fifth request is a HIT with the de body; both variants coexist.
	body="$(curl -sS --max-time 10 -H 'Accept-Language: de' https://cachetest.ripdev.io/vary-lang)"
	eq "$body" "lang:de"
	eq "$(path_hits "$CACHE_HITS1" /vary-lang)" "4"
}

case_cache_unbounded_vary_never_stored() {
	cache_hb
	local s0
	s0="$(cache_stat stores)"
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/vary-star
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/vary-star
	eq "$(path_hits "$CACHE_HITS1" /vary-star)" "2"
	eq "$(($(cache_stat stores) - s0))" "0"
}

case_cache_non200_never_stored() {
	cache_hb
	local p
	for p in /404 /500; do
		curl -sS --max-time 10 -o /dev/null "https://cachetest.ripdev.io$p"
		curl -sS --max-time 10 -o /dev/null "https://cachetest.ripdev.io$p"
		eq "$(path_hits "$CACHE_HITS1" "$p")" "2"
	done
}

case_cache_marked_503_never_stored() {
	cache_hb
	local code
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 https://cachetest.ripdev.io/busy)"
	eq "$code" "503"
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 https://cachetest.ripdev.io/busy)"
	eq "$code" "503"
	# Both requests reached the data plane's retry machinery (the fixture
	# is the only upstream, so each request bounces off it once).
	eq "$(path_hits "$CACHE_HITS1" /busy)" "2"
}

case_cache_truncated_fill_never_stored() {
	cache_hb
	local s0
	s0="$(cache_stat stores)"
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/truncate 2>/dev/null || true
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/truncate 2>/dev/null || true
	eq "$(path_hits "$CACHE_HITS1" /truncate)" "2"
	eq "$(($(cache_stat stores) - s0))" "0"
}

case_cache_max_body_streams_uncached() {
	cache_hb
	local s0 n i
	s0="$(cache_stat stores)"
	for i in 1 2 3; do
		n="$(curl -sS --max-time 10 https://cachetest.ripdev.io/big | wc -c | tr -d ' ')"
		eq "$n" "307200" # response intact
	done
	eq "$(path_hits "$CACHE_HITS1" /big)" "3"
	eq "$(($(cache_stat stores) - s0))" "0"
}

case_cache_coalescing_stampede() {
	cache_hb
	local h0 c0 i
	h0="$(cache_stat hits)"
	c0="$(cache_stat coalesced)"
	rm -f "$ROOT"/.test-cache-burst-*
	for i in $(seq 1 32); do
		curl -sS --max-time 15 -o "$ROOT/.test-cache-burst-$i" https://cachetest.ripdev.io/slow &
	done
	wait
	# N concurrent cold misses → exactly 1 origin request.
	eq "$(path_hits "$CACHE_HITS1" /slow)" "1"
	for i in $(seq 1 32); do
		eq "$(cat "$ROOT/.test-cache-burst-$i")" "slow-body"
	done
	# Every non-leader either coalesced onto the fill or hit the stored
	# entry (late arrival) — 31 either way, zero at the worker.
	eq "$((($(cache_stat hits) - h0) + ($(cache_stat coalesced) - c0)))" "31"
	rm -f "$ROOT"/.test-cache-burst-*
}

case_cache_waiter_cap_overflow() {
	cache_hb
	local o0 i codes
	o0="$(cache_stat waiter_overflow)"
	rm -f "$ROOT/.test-cache-cap-codes"
	for i in $(seq 1 80); do
		curl -sS --max-time 20 -o /dev/null -w '%{http_code}\n' \
			https://cachetest.ripdev.io/slower >>"$ROOT/.test-cache-cap-codes" &
	done
	wait
	# Overflow (past 64 waiters) falls through to the worker — nobody
	# gets a manufactured 503.
	codes="$(sort -u "$ROOT/.test-cache-cap-codes" | tr -d ' ')"
	eq "$codes" "200"
	eq "$(wc -l <"$ROOT/.test-cache-cap-codes" | tr -d ' ')" "80"
	ok "$(($(cache_stat waiter_overflow) - o0)) -ge 1" "no waiter_overflow counted"
	ok "$(path_hits "$CACHE_HITS1" /slower) -ge $((80 - 65))" \
		"fall-throughs never reached the worker: $(path_hits "$CACHE_HITS1" /slower)"
	rm -f "$ROOT/.test-cache-cap-codes"
}

case_cache_fill_failure_falls_through() {
	cache_hb
	local i v
	rm -f "$ROOT"/.test-cache-fail-*
	for i in $(seq 1 8); do
		curl -sS --max-time 15 -o "$ROOT/.test-cache-fail-$i" \
			https://pages.ripdev.io/slowcookie &
	done
	wait
	# Set-Cookie fill: the leader's response is never shared — every
	# waiter fell through and reached the worker itself.
	eq "$(path_hits "$CACHE_HITS1" /slowcookie)" "8"
	for i in $(seq 1 8); do
		eq "$(cat "$ROOT/.test-cache-fail-$i")" "personal"
	done
	# The key carries a do-not-coalesce mark for one ttl: the next burst
	# bypasses without buffering (pages has debug on to prove it).
	v="$(curl -sS --max-time 10 -o /dev/null -D - https://pages.ripdev.io/slowcookie |
		tr -d '\r' | awk -F': ' 'tolower($1)=="x-janus-cache" {print $2}')"
	eq "$v" "BYPASS"
	rm -f "$ROOT"/.test-cache-fail-*
}

case_cache_purge_on_upstream_swap() {
	cache_hb
	local p0 body
	p0="$(cache_stat purges)"
	# Fill the cache on the current (cache1) upstream.
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/swap
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/swap
	eq "$(path_hits "$CACHE_HITS1" /swap)" "2"
	# Swap to the second fixture: the purge empties the app's keys.
	capi PUT "/1.0/apps/$(cache_app_id)/upstreams" "{\"upstreams\":[{\"path\":\"$CACHE_SOCK2\"}]}"
	eq "$REPLY_CODE" "200"
	ok "$(($(cache_stat purges) - p0)) -ge 1" "purges counter not incremented"
	# The immediate next request reaches the NEW worker.
	body="$(curl -sS --max-time 10 https://cachetest.ripdev.io/swap)"
	eq "$body" "plain:/swap"
	eq "$(path_hits "$CACHE_HITS2" /swap)" "1"
}

case_cache_purge_race_fill_straddles_put() {
	cache_hb
	local f0 body
	f0="$(cache_stat fenced_stores)"
	# One priming sighting on a fresh key: the doorkeeper now admits the
	# NEXT fill (nothing stored yet — a stored prime would turn the race
	# request into a plain HIT), so only the fence can reject its store.
	curl -sS --max-time 10 -o /dev/null 'https://cachetest.ripdev.io/slow?race=1'
	# Start the slow fill (~500ms), then land a PUT mid-fill.
	curl -sS --max-time 15 -o "$ROOT/.test-cache-race" 'https://cachetest.ripdev.io/slow?race=1' &
	local fill_pid=$!
	sleep 0.2
	capi PUT "/1.0/apps/$(cache_app_id)/upstreams" "{\"upstreams\":[{\"path\":\"$CACHE_SOCK1\"}]}"
	eq "$REPLY_CODE" "200"
	wait "$fill_pid"
	eq "$(cat "$ROOT/.test-cache-race")" "slow-body" # the leader still gets its bytes
	eq "$(($(cache_stat fenced_stores) - f0))" "1"   # …but the store is fenced
	# The next GET misses and fills from the new pool (cache1).
	curl -sS --max-time 10 -o /dev/null 'https://cachetest.ripdev.io/slow?race=1'
	eq "$(path_hits "$CACHE_HITS1" '/slow?race=1')" "1"
	rm -f "$ROOT/.test-cache-race"
}

case_cache_host_reclaim() {
	cache_hb
	local body id2
	# Fill app A's cache.
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/reclaim
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/reclaim
	eq "$(path_hits "$CACHE_HITS1" /reclaim)" "2"
	# DELETE A; re-claim the hosts as app B on the second fixture.
	capi DELETE "/1.0/apps/$(cache_app_id)"
	eq "$REPLY_CODE" "204"
	capi POST /1.0/apps \
		'{"name":"cacheb","hosts":["cachetest.ripdev.io","pages.ripdev.io","api.ripdev.io"]}'
	eq "$REPLY_CODE" "201"
	id2="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	printf '%s' "$id2" >"$CACHE_APP_FILE"
	capi PUT "/1.0/apps/$id2/upstreams" "{\"upstreams\":[{\"path\":\"$CACHE_SOCK2\"}]}"
	eq "$REPLY_CODE" "200"
	# The immediate GET reaches B's worker — never A's cached body.
	body="$(curl -sS --max-time 10 https://cachetest.ripdev.io/reclaim)"
	eq "$body" "plain:/reclaim"
	eq "$(path_hits "$CACHE_HITS2" /reclaim)" "1"
}

case_cache_ttl_expiry() {
	cache_hb
	# cachetest inherits ttl 1s: fill + hit, expire, refill.
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/ttl
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/ttl
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/ttl # HIT
	eq "$(path_hits "$CACHE_HITS2" /ttl)" "2"
	sleep 1.2
	cache_hb
	curl -sS --max-time 10 -o /dev/null https://cachetest.ripdev.io/ttl
	eq "$(path_hits "$CACHE_HITS2" /ttl)" "3"
}

case_cache_off_site() {
	cache_hb
	local i
	for i in 1 2 3; do
		curl -sS --max-time 10 -o /dev/null https://api.ripdev.io/offpage
	done
	# Explicit off beats inherited on: every request reaches the worker.
	eq "$(path_hits "$CACHE_HITS2" /offpage)" "3"
}

case_cache_cascade_ttl_override() {
	cache_hb
	# pages overrides ttl to 5s: a 1.2s-old entry (dead on the 1s
	# catchall, per the expiry case above) still HITs here.
	curl -sS --max-time 10 -o /dev/null https://pages.ripdev.io/cascade
	curl -sS --max-time 10 -o /dev/null https://pages.ripdev.io/cascade
	eq "$(path_hits "$CACHE_HITS2" /cascade)" "2"
	sleep 1.2
	cache_hb
	local v
	v="$(curl -sS --max-time 10 -o /dev/null -D - https://pages.ripdev.io/cascade |
		tr -d '\r' | awk -F': ' 'tolower($1)=="x-janus-cache" {print $2}')"
	eq "$v" "HIT"
	eq "$(path_hits "$CACHE_HITS2" /cascade)" "2"
}

case_cache_parse_rejections() {
	local bad dir rc
	dir="$(mktemp -d /tmp/janus-cache-parse.XXXXXX)"
	local -a cases=(
		'cache maybe'
		'cache off { ttl 1s }'
		'cache { bogus 1 }'
		'cache { ttl }'
		'cache { ttl zero }'
		'cache { max_body 0 }'
		'cache { max_app_share 200 }'
		'cache { debug loudly }'
		'cache { ttl 1s
			ttl 2s }'
	)
	for bad in "${cases[@]}"; do
		cat >"$dir/Caddyfile" <<EOF
{
	janus {
		$bad
	}
}
EOF
		if "$CADDY_BIN" adapt --config "$dir/Caddyfile" >/dev/null 2>&1; then
			echo "caddy adapt accepted illegal global config: $bad" >&2
			rm -rf "$dir"
			return 1
		fi
	done
	# Site-level rejections: process-wide keys and blocks on off.
	local -a site_cases=(
		'cache { max_bytes 1mb }'
		'cache { max_app_share 10 }'
		'cache off { debug }'
	)
	for bad in "${site_cases[@]}"; do
		cat >"$dir/Caddyfile" <<EOF
site.example.com {
	janus {
		$bad
	}
}
EOF
		if "$CADDY_BIN" adapt --config "$dir/Caddyfile" >/dev/null 2>&1; then
			echo "caddy adapt accepted illegal site config: $bad" >&2
			rm -rf "$dir"
			return 1
		fi
	done
	rm -rf "$dir"
}

# --- cases: heartbeat --------------------------------------------------------
#
# The group restarts caddy with JANUS_HEARTBEAT_TTL=2s (sweep every ~666ms)
# so TTL expiry is observable in seconds. Recovery from expiry is
# RE-REGISTRATION: the reap has the same effect as DELETE, so the tenant sees
# heartbeat → 404, re-registers, and re-PUTs its upstreams.

HB_APP_FILE="$ROOT/.test-hb-app-id"

hb_app_id() {
	cat "$HB_APP_FILE"
}

# hb_register — register pulse.ripdev.io and publish the shared upstream;
# used for both the initial registration and the post-reap re-registration.
hb_register() {
	capi POST /1.0/apps '{"name":"pulse","hosts":["pulse.ripdev.io"]}'
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	ok "-n \"$id\"" "no id in $REPLY_BODY"
	printf '%s' "$id" >"$HB_APP_FILE"
	capi PUT "/1.0/apps/$id/upstreams" "{\"upstreams\":[{\"path\":\"$ROOT/run/hb.sock\"}]}"
	eq "$REPLY_CODE" "200"
}

case_hb_register_traffic() {
	start_data_upstream "$ROOT/run/hb.sock" hb "$ROOT/.test-hb.hits" || return 1
	hb_register
	eq "$(http_code https://pulse.ripdev.io/)" "200"
	eq "$(http_body https://pulse.ripdev.io/)" "upstream:hb"
}

case_hb_beat_204() {
	capi POST "/1.0/apps/$(hb_app_id)/heartbeat"
	eq "$REPLY_CODE" "204"
}

case_hb_beat_unknown_404() {
	capi POST /1.0/apps/pulse-zzzzzz/heartbeat
	eq "$REPLY_CODE" "404"
}

case_hb_ttl_reaps() {
	# Stop heartbeating; wait past TTL (2s) + a sweep interval.
	sleep 3.5
	eq "$(http_code https://pulse.ripdev.io/)" "404"
	capi GET "/1.0/apps/$(hb_app_id)"
	eq "$REPLY_CODE" "404"
	capi GET /1.0/apps
	eq "$REPLY_CODE" "200"
	if printf '%s' "$REPLY_BODY" | grep -qF "$(hb_app_id)"; then
		printf 'reaped app still listed: %q' "$REPLY_BODY" >&2
		return 1
	fi
}

case_hb_reregister_recovers() {
	hb_register
	eq "$(http_code https://pulse.ripdev.io/)" "200"
	eq "$(http_body https://pulse.ripdev.io/)" "upstream:hb"
}

case_hb_alive_not_routable() {
	# Heartbeat ≠ readiness: empty upstreams + fresh heartbeats across more
	# than one TTL keeps the app registered — 503, never 404.
	capi PUT "/1.0/apps/$(hb_app_id)/upstreams" '{"upstreams":[]}'
	eq "$REPLY_CODE" "200"
	local i
	for i in $(seq 1 6); do
		capi POST "/1.0/apps/$(hb_app_id)/heartbeat"
		eq "$REPLY_CODE" "204"
		sleep 0.5
	done
	eq "$(http_code https://pulse.ripdev.io/)" "503"
	capi GET "/1.0/apps/$(hb_app_id)"
	eq "$REPLY_CODE" "200"
}

# --- cases: tls ---------------------------------------------------------------
#
# Phase 6: on-demand TLS gated by the registry. The group reuses the
# JANUS_HEARTBEAT_TTL=2s caddy from the heartbeat group, so lifecycle
# transitions (delete, reap) are observable in seconds; cases that span
# time keep their app alive with explicit heartbeats.
#
# Handshake cases use *.janus.test names (resolved to 127.0.0.1 with
# --resolve): Caddy serves cold-loaded certs (the *.ripdev.io wildcard)
# from its cache before consulting on-demand, so only a name the cold
# config does NOT cover exercises the ask → mint path.

TLS_APP_FILE="$ROOT/.test-tls-app-id"
CADDY_LOCAL_ROOT="$ROOT/.test-caddy-data/caddy/pki/authorities/local/root.crt"

tls_app_id() {
	cat "$TLS_APP_FILE"
}

# tls_register NAME HOST — register and stash the id in TLS_APP_FILE.
tls_register() {
	local name=$1 host=$2
	capi POST /1.0/apps "{\"name\":\"$name\",\"hosts\":[\"$host\"]}"
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	ok "-n \"$id\"" "no id in $REPLY_BODY"
	printf '%s' "$id" >"$TLS_APP_FILE"
}

case_tls_ask_missing_domain() {
	capi GET /1.0/tls/ask
	eq "$REPLY_CODE" "400"
	capi GET '/1.0/tls/ask?domain='
	eq "$REPLY_CODE" "400"
}

case_tls_ask_unknown_domain() {
	capi GET '/1.0/tls/ask?domain=stranger.janus.test'
	eq "$REPLY_CODE" "404"
}

case_tls_register_allows() {
	tls_register odt odt.janus.test
	capi GET '/1.0/tls/ask?domain=odt.janus.test'
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"domain":"odt.janus.test"'
	json_has "$REPLY_BODY" "$(tls_app_id)"
	# hostnames are case-insensitive; the ask normalizes
	capi GET '/1.0/tls/ask?domain=ODT.Janus.TEST'
	eq "$REPLY_CODE" "200"
}

case_tls_allowed_host_minted() {
	# The acceptance proof: the registered name completes a real TLS
	# handshake on the on-demand site — Caddy asked /1.0/tls/ask, got
	# 200, and minted a leaf from its internal CA for exactly this name.
	capi POST "/1.0/apps/$(tls_app_id)/heartbeat"
	eq "$REPLY_CODE" "204"
	ok "-f \"$CADDY_LOCAL_ROOT\"" "missing internal CA root $CADDY_LOCAL_ROOT"
	local body verify cert
	body="$(curl -sS --max-time 10 --cacert "$CADDY_LOCAL_ROOT" \
		--resolve odt.janus.test:8443:127.0.0.1 https://odt.janus.test:8443/ping)"
	eq "$body" "pong"
	verify="$(curl -sS -o /dev/null -w '%{ssl_verify_result}' --max-time 10 \
		--cacert "$CADDY_LOCAL_ROOT" \
		--resolve odt.janus.test:8443:127.0.0.1 https://odt.janus.test:8443/ping)"
	eq "$verify" "0"
	# The served leaf is the on-demand mint: internal CA issuer, exact SAN.
	cert="$(echo | openssl s_client -connect 127.0.0.1:8443 \
		-servername odt.janus.test 2>/dev/null |
		openssl x509 -noout -issuer -ext subjectAltName 2>/dev/null)"
	if ! printf '%s' "$cert" | grep -q 'Caddy Local Authority'; then
		printf 'leaf not minted by the internal CA: %s' "$cert" >&2
		return 1
	fi
	if ! printf '%s' "$cert" | grep -q 'DNS:odt.janus.test'; then
		printf 'leaf SAN missing odt.janus.test: %s' "$cert" >&2
		return 1
	fi
}

case_tls_denied_host_no_handshake() {
	# Never-registered name: the ask answers 404, Caddy mints nothing,
	# and the handshake fails outright.
	if curl -sS -o /dev/null --max-time 10 --cacert "$CADDY_LOCAL_ROOT" \
		--resolve denied.janus.test:8443:127.0.0.1 \
		https://denied.janus.test:8443/ping 2>/dev/null; then
		echo "TLS handshake unexpectedly succeeded for unregistered name" >&2
		return 1
	fi
}

case_tls_delete_denies() {
	tls_register del del.janus.test
	capi GET '/1.0/tls/ask?domain=del.janus.test'
	eq "$REPLY_CODE" "200"
	capi DELETE "/1.0/apps/$(tls_app_id)"
	eq "$REPLY_CODE" "204"
	capi GET '/1.0/tls/ask?domain=del.janus.test'
	eq "$REPLY_CODE" "404"
}

case_tls_reap_denies() {
	tls_register reap reap.janus.test
	capi GET '/1.0/tls/ask?domain=reap.janus.test'
	eq "$REPLY_CODE" "200"
	# Stop heartbeating; wait past TTL (2s) + a sweep interval.
	sleep 3.5
	capi GET '/1.0/tls/ask?domain=reap.janus.test'
	eq "$REPLY_CODE" "404"
}

case_tls_alive_not_routable_allowed() {
	# Heartbeat ≠ readiness: empty upstreams + fresh heartbeats across
	# more than one TTL keeps the cert allowance — reload never breaks TLS.
	tls_register alive alive.janus.test
	capi PUT "/1.0/apps/$(tls_app_id)/upstreams" '{"upstreams":[]}'
	eq "$REPLY_CODE" "200"
	local i
	for i in $(seq 1 6); do
		capi POST "/1.0/apps/$(tls_app_id)/heartbeat"
		eq "$REPLY_CODE" "204"
		sleep 0.5
	done
	capi GET '/1.0/tls/ask?domain=alive.janus.test'
	eq "$REPLY_CODE" "200"
}

# --- cases: hub ---------------------------------------------------------------
#
# Capability 4: per-app WebSocket fan-out
# (docs/20260720-162350-hub-design.md "Acceptance sketch"). The instrument:
# a fixture tenant with a bridge endpoint that records every POST it
# receives (headers, frame type, body) and answers from a scriptable
# playbook, plus /1.0/hub counters and the membership snapshot. The
# testkit ws driver runs sockets. The group runs under the heartbeat
# caddy (TTL 2s), so the fixture heartbeats its apps every 500ms.
#
# Hosts (root Caddyfile): hub1/hub2/hubdel/hubrace.ripdev.io inherit
# global hub on (origin same — the driver sends a browser-shaped Origin);
# hubany.ripdev.io is origin any; hubten/hubtwenty cap max_conns 10/20;
# api.ripdev.io is hub off.

HUB_SOCK="$ROOT/run/hub-tenant.sock"
HUB_BRIDGE_LOG="$ROOT/.test-hub-bridge.jsonl"
HUB_PLAYBOOK="$ROOT/.test-hub-playbook"
HUB_PIDS_FILE="$ROOT/.test-hub-pids"
HUB_APP_FILE="$ROOT/.test-hub-app-id"       # hubapp: hub1, hubany, api
HUB_ISO_FILE="$ROOT/.test-hub-iso-id"       # hubiso: hub2 (per-app isolation)
HUB_CAP_FILE="$ROOT/.test-hub-cap-id"       # hubcap: hubten + hubtwenty (floor 10)

hub_app_id() { cat "$HUB_APP_FILE"; }
hub_iso_id() { cat "$HUB_ISO_FILE"; }
hub_cap_id() { cat "$HUB_CAP_FILE"; }

stop_hub_fixtures() {
	if [[ -f "$HUB_PIDS_FILE" ]]; then
		while read -r pid; do
			kill "$pid" 2>/dev/null || true
		done <"$HUB_PIDS_FILE"
	fi
	rm -f "$HUB_PIDS_FILE" "$HUB_BRIDGE_LOG" "$HUB_PLAYBOOK" \
		"$HUB_APP_FILE" "$HUB_ISO_FILE" "$HUB_CAP_FILE" "$HUB_SOCK" \
		"$ROOT"/.test-hub-flag-* "$ROOT"/.test-hub-out-* "$ROOT"/.test-hub-cap-codes
}

# hub_playbook JSON — set the fixture's scripted answers ('' resets).
# Shape: {"open":{"status":200,"body":"...","delay_ms":0},"text":{...},"close":{...}}
hub_playbook() {
	if [[ -z "$1" ]]; then
		rm -f "$HUB_PLAYBOOK"
	else
		printf '%s' "$1" >"$HUB_PLAYBOOK"
	fi
}

# hub_stat KEY — one process-total counter from GET /1.0/hub
hub_stat() {
	capi GET /1.0/hub
	printf '%s' "$REPLY_BODY" | "$TESTKIT" json get "$1"
}

# hub_snapshot APPID — GET /1.0/apps/{id}/hub into REPLY_BODY/REPLY_CODE
hub_snapshot() {
	capi GET "/1.0/apps/$1/hub"
}

# hub_publish APPID JSON — POST the publish plane
hub_publish() {
	capi POST "/1.0/apps/$1/hub/publish" "$2"
}

# hub_bridge_count KIND — how many bridge POSTs of one frame kind landed
hub_bridge_count() {
	local kind=$1
	if [[ ! -f "$HUB_BRIDGE_LOG" ]]; then
		echo 0
		return
	fi
	grep -c "\"kind\": \"$kind\"" "$HUB_BRIDGE_LOG" || true
}

# hub_ws HOST ORIGIN COOKIE CMDS… — run the WS driver in the foreground;
# output (RECV/CLOSE/ID lines) lands in REPLY_WS. ORIGIN/COOKIE '-' = none.
hub_ws() {
	local host=$1 origin=$2 cookie=$3
	shift 3
	REPLY_WS="$("$TESTKIT" ws "$host" "$origin" "$cookie" "$@" 2>&1)"
}

# hub_ws_bg OUTFILE HOST ORIGIN COOKIE CMDS… — same, backgrounded; the
# driver's pid lands in HUB_WS_PID for a targeted wait.
hub_ws_bg() {
	local out=$1 host=$2 origin=$3 cookie=$4
	shift 4
	"$TESTKIT" ws "$host" "$origin" "$cookie" "$@" >"$out" 2>&1 &
	HUB_WS_PID=$!
	printf '%s\n' "$HUB_WS_PID" >>"$HUB_PIDS_FILE"
}

wait_file() {
	local f=$1 i
	for i in $(seq 1 100); do
		[[ -e "$f" ]] && return 0
		sleep 0.1
	done
	echo "file $f never appeared" >&2
	return 1
}

# hub_upgrade_code HOST ORIGIN [HDR…] — a curl-shaped upgrade attempt;
# prints the HTTP status (101 = upgraded; anything else = refused).
# HTTP/1.1 forced: h2 strips Connection/Upgrade, unshaping the request.
hub_upgrade_code() {
	local host=$1 origin=$2
	shift 2
	local -a args=(-sS -o /dev/null -w '%{http_code}' --max-time 5 --http1.1
		-H 'Connection: Upgrade' -H 'Upgrade: websocket'
		-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==')
	if [[ "$origin" != "-" ]]; then
		args+=(-H "Origin: $origin")
	fi
	local h
	for h in "$@"; do
		args+=(-H "$h")
	done
	curl "${args[@]}" "https://$host/hub" 2>/dev/null
}

# start_hub_tenant SOCK APPID… — the recording, scriptable bridge tenant.
# Serves POST {bridge_path} (records + playbook), answers any other request
# with plain:<path>, and heartbeats every app id given (500ms; TTL is 2s).
start_hub_tenant() {
	local sock=$1
	shift
	rm -f "$sock"
	: >"$HUB_BRIDGE_LOG"
	"$TESTKIT" hubtenant --sock "$sock" --hits "$HUB_BRIDGE_LOG" --playbook "$HUB_PLAYBOOK" "$@" \
		>>"$ROOT/.test-fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$HUB_PIDS_FILE"
	local i
	for i in $(seq 1 50); do
		[[ -S "$sock" ]] && return 0
		sleep 0.1
	done
	echo "hub tenant socket $sock never appeared" >&2
	return 1
}

# start_hub_wedge HOST IDFILE — a raw client that completes the WebSocket
# handshake and then never reads: the slow-consumer instrument. Its
# connection id is read from the fixture's open-bridge record by the case.
start_hub_wedge() {
	local host=$1
	"$TESTKIT" wedge --host "$host" >>"$ROOT/.test-fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$HUB_PIDS_FILE"
}

case_hub_setup() {
	: >"$HUB_PIDS_FILE"
	hub_playbook ''

	# Register the three apps FIRST (ids feed the fixture's heartbeater).
	capi POST /1.0/apps '{"name":"hubapp","hosts":["hub1.ripdev.io","hubany.ripdev.io","hubdel.ripdev.io","hubrace.ripdev.io","api.ripdev.io"],"bridge_path":"/rt/bridge"}'
	eq "$REPLY_CODE" "201"
	printf '%s' "$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')" >"$HUB_APP_FILE"
	capi POST /1.0/apps '{"name":"hubiso","hosts":["hub2.ripdev.io"],"bridge_path":"/rt/bridge"}'
	eq "$REPLY_CODE" "201"
	printf '%s' "$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')" >"$HUB_ISO_FILE"
	capi POST /1.0/apps '{"name":"hubcap","hosts":["hubten.ripdev.io","hubtwenty.ripdev.io"],"bridge_path":"/rt/bridge"}'
	eq "$REPLY_CODE" "201"
	printf '%s' "$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')" >"$HUB_CAP_FILE"

	start_hub_tenant "$HUB_SOCK" "$(hub_app_id)" "$(hub_iso_id)" "$(hub_cap_id)" || return 1
	local id
	for id in "$(hub_app_id)" "$(hub_iso_id)" "$(hub_cap_id)"; do
		capi PUT "/1.0/apps/$id/upstreams" "{\"upstreams\":[{\"path\":\"$HUB_SOCK\"}]}"
		eq "$REPLY_CODE" "200"
	done

	# bridge_path surfaces on GET; /1.0/hub counters answer.
	capi GET "/1.0/apps/$(hub_app_id)"
	json_has "$REPLY_BODY" '"bridge_path":"/rt/bridge"'
	capi GET /1.0/hub
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"bridge_garbage"'
}

case_hub_open_full_path() {
	# Open bridge: headers + admission; tenant enrolls the connection; the
	# snapshot proves membership.
	hub_playbook '{"open":{"status":200,"body":"{\"+\":[\"/room\"]}"}}'
	local b0
	b0="$(hub_bridge_count open)"
	hub_ws hubany.ripdev.io - "sid=42" id pause=200 close
	hub_playbook ''
	ok "$(hub_bridge_count open) -gt $b0" "no open bridge recorded"
	local rec
	rec="$(grep '"kind": "open"' "$HUB_BRIDGE_LOG" | tail -1)"
	json_has "$rec" '"path": "/rt/bridge"'
	json_has "$rec" "\"app\": \"$(hub_app_id)\""
	json_has "$rec" '"cookie": "sid=42"'
	json_has "$rec" '"has_sec_ws_key": false'
	json_has "$rec" '"has_connection": false'
	local wsid
	wsid="$(printf '%s' "$REPLY_WS" | sed -n 's/^ID //p')"
	ok "-n \"$wsid\"" "driver printed no id: $REPLY_WS"
	json_has "$rec" "\"client\": \"$wsid\""
}

case_hub_open_rejected_by_tenant() {
	hub_playbook '{"open":{"status":403,"body":"denied by tenant"}}'
	local resp code c0
	c0="$(hub_stat conns)"
	resp="$(curl -sS --max-time 5 --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
		-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
		-w $'\n%{http_code}' "https://hubany.ripdev.io/hub")"
	hub_playbook ''
	code="${resp##*$'\n'}"
	eq "$code" "403"
	json_has "$resp" 'denied by tenant'
	eq "$(hub_stat conns)" "$c0"
}

case_hub_open_tenant_down() {
	capi PUT "/1.0/apps/$(hub_app_id)/upstreams" '{"upstreams":[]}'
	eq "$REPLY_CODE" "200"
	local hdrs
	hdrs="$(curl -sSI --max-time 5 --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
		-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
		-X GET "https://hubany.ripdev.io/hub" 2>/dev/null | tr -d '\r')"
	capi PUT "/1.0/apps/$(hub_app_id)/upstreams" "{\"upstreams\":[{\"path\":\"$HUB_SOCK\"}]}"
	json_has "$hdrs" '503'
	json_has "$hdrs" 'Retry-After'
}

case_hub_no_bridge_path() {
	capi PATCH "/1.0/apps/$(hub_app_id)" '{"bridge_path":null}'
	eq "$REPLY_CODE" "200"
	eq "$(hub_upgrade_code hubany.ripdev.io -)" "503"
	capi PATCH "/1.0/apps/$(hub_app_id)" '{"bridge_path":"/rt/bridge"}'
	eq "$REPLY_CODE" "200"
}

case_hub_exclusion_rules() {
	# ! rules 1+2: bare name excludes the ORIGINATING CONNECTION only (the
	# same user's other tab still receives); ! includes the sender and the
	# suffix is stripped on delivery.
	rm -f "$ROOT"/.test-hub-flag-* "$ROOT"/.test-hub-out-*
	hub_ws_bg "$ROOT/.test-hub-out-a2" hubany.ripdev.io - "user=A" \
		'send={"+":["/room"]}' touch="$ROOT/.test-hub-flag-a2" expect=chat expect=fin close
	hub_ws_bg "$ROOT/.test-hub-out-b" hubany.ripdev.io - "user=B" \
		'send={"+":["/room"]}' touch="$ROOT/.test-hub-flag-b" expect=chat expect=fin close
	wait_file "$ROOT/.test-hub-flag-a2"
	wait_file "$ROOT/.test-hub-flag-b"
	hub_ws hubany.ripdev.io - "user=A" \
		'send={"+":["/room"]}' \
		'send={"@":["/room"],"chat":{"m":1}}' \
		noframe=300 \
		'send={"@":["/room"],"fin!":{}}' \
		expect=fin close
	json_has "$REPLY_WS" 'DONE'
	# The sender received fin (suffix included it) with the ! stripped.
	json_has "$REPLY_WS" '"fin":'
	if printf '%s' "$REPLY_WS" | grep -qF '"fin!"'; then
		echo "suffix leaked to a recipient: $REPLY_WS" >&2
		return 1
	fi
	wait
	json_has "$(cat "$ROOT/.test-hub-out-a2")" 'DONE'
	json_has "$(cat "$ROOT/.test-hub-out-b")" 'DONE'
}

case_hub_publish_ignores_exclusion() {
	# ! rule 3: no originating connection on the publish plane.
	rm -f "$ROOT"/.test-hub-flag-*
	hub_ws_bg "$ROOT/.test-hub-out-pub" hubany.ripdev.io - - \
		'send={"+":["/pubroom"]}' touch="$ROOT/.test-hub-flag-pub" expect=news expect=news close
	wait_file "$ROOT/.test-hub-flag-pub"
	hub_publish "$(hub_app_id)" '{"@":["/pubroom"],"news":{"n":1}}'
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"deliveries":1'
	hub_publish "$(hub_app_id)" '{"@":["/pubroom"],"news!":{"n":2}}'
	json_has "$REPLY_BODY" '"deliveries":1'
	wait
	json_has "$(cat "$ROOT/.test-hub-out-pub")" 'DONE'
}

case_hub_pong_at_edge() {
	# ! rule 4: exact ? answers {"!": value} from the edge, value verbatim;
	# the frame still reaches the text bridge (observation), and no worker
	# answers the pong (the fixture only records).
	local t0
	t0="$(hub_bridge_count text)"
	hub_ws hubany.ripdev.io - - 'send={"?":"t-12345"}' 'expect={"!":"t-12345"}' close
	json_has "$REPLY_WS" 'RECV {"!":"t-12345"}'
	waitfor_bridge_texts $((t0 + 1))
	local rec
	rec="$(grep '"kind": "text"' "$HUB_BRIDGE_LOG" | tail -1)"
	json_has "$rec" '{\"?\":\"t-12345\"}'
}

# waitfor_bridge_texts N — texts land asynchronously; poll for the count.
waitfor_bridge_texts() {
	local want=$1 i
	for i in $(seq 1 50); do
		if [[ "$(hub_bridge_count text)" -ge "$want" ]]; then
			return 0
		fi
		sleep 0.1
	done
	echo "text bridge count never reached $want" >&2
	return 1
}

case_hub_provenance_stamped() {
	rm -f "$ROOT"/.test-hub-flag-*
	hub_ws_bg "$ROOT/.test-hub-out-prov" hubany.ripdev.io - - \
		'send={"+":["/prov"]}' touch="$ROOT/.test-hub-flag-prov" expect=whisper close
	wait_file "$ROOT/.test-hub-flag-prov"
	hub_ws hubany.ripdev.io - - id 'send={"@":["/prov"],"whisper":{"w":1}}' close
	local aid
	aid="$(printf '%s' "$REPLY_WS" | sed -n 's/^ID //p')"
	ok "-n \"$aid\"" "no sender id: $REPLY_WS"
	wait
	# The recipient sees "<":[<sender-connection-id>], stamped by the edge.
	json_has "$(cat "$ROOT/.test-hub-out-prov")" "\"<\":[\"$aid\"]"
}

case_hub_spoof_rejected() {
	local r0
	r0="$(hub_stat rejected_frames)"
	hub_ws hubany.ripdev.io - - 'send={"<":["fake"],"chat":{}}' 'expectclose=1008,stamped by janus'
	json_has "$REPLY_WS" 'CLOSE 1008'
	ok "$(hub_stat rejected_frames) -gt $r0" "rejected_frames not counted"
	# The close is reported to the tenant with the 1008 code (async).
	local i
	for i in $(seq 1 50); do
		if grep '"kind": "close"' "$HUB_BRIDGE_LOG" 2>/dev/null | grep -qF '\"code\":1008'; then
			return 0
		fi
		sleep 0.1
	done
	echo "close bridge with 1008 never recorded" >&2
	return 1
}

case_hub_reserved_sigils_reject() {
	hub_ws hubany.ripdev.io - - 'send={">":["x"],"chat":{}}' 'expectclose=1008,reserved'
	json_has "$REPLY_WS" 'CLOSE 1008'
	hub_ws hubany.ripdev.io - - 'send={"!":"t"}' 'expectclose=1008,janus-to-client'
	json_has "$REPLY_WS" 'CLOSE 1008'
	hub_ws hubany.ripdev.io - - 'send={"*":"bye"}' 'expectclose=1008,delivery-direction'
	json_has "$REPLY_WS" 'CLOSE 1008'
	hub_ws hubany.ripdev.io - - binary 'expectclose=1003,binary'
	json_has "$REPLY_WS" 'CLOSE 1003'
}

case_hub_join_leave_snapshot() {
	# Join/leave bookkeeping through the membership snapshot (the tenant's
	# oracle): default target is the sender (no @ on the joins), the
	# emptied channel disappears.
	rm -f "$ROOT"/.test-hub-flag-*
	hub_ws_bg "$ROOT/.test-hub-out-jl" hubany.ripdev.io - - \
		'send={"+":["/jl/a","/jl/b"]}' touch="$ROOT/.test-hub-flag-jl1" \
		waitfile="$ROOT/.test-hub-flag-go1" 'send={"-":["/jl/b"]}' \
		touch="$ROOT/.test-hub-flag-jl2" waitfile="$ROOT/.test-hub-flag-go2" close
	wait_file "$ROOT/.test-hub-flag-jl1"
	sleep 0.2
	hub_snapshot "$(hub_app_id)"
	json_has "$REPLY_BODY" '"/jl/a":1'
	json_has "$REPLY_BODY" '"/jl/b":1'
	touch "$ROOT/.test-hub-flag-go1"
	wait_file "$ROOT/.test-hub-flag-jl2"
	sleep 0.2
	hub_snapshot "$(hub_app_id)"
	json_has "$REPLY_BODY" '"/jl/a":1'
	if printf '%s' "$REPLY_BODY" | grep -qF '"/jl/b"'; then
		echo "emptied channel still in snapshot: $REPLY_BODY" >&2
		return 1
	fi
	touch "$ROOT/.test-hub-flag-go2"
	wait
}

case_hub_sender_only_membership() {
	# A client @-ing another connection with + closes 1008 and nothing
	# joins; the trusted publish plane CAN enroll it.
	rm -f "$ROOT"/.test-hub-flag-*
	hub_ws_bg "$ROOT/.test-hub-out-victim" hubany.ripdev.io - - \
		id touch="$ROOT/.test-hub-flag-victim" \
		waitfile="$ROOT/.test-hub-flag-victim-go" expect=enrolled close
	wait_file "$ROOT/.test-hub-flag-victim"
	local vid
	for i in $(seq 1 50); do
		vid="$(sed -n 's/^ID //p' "$ROOT/.test-hub-out-victim" 2>/dev/null)"
		[[ -n "$vid" ]] && break
		sleep 0.1
	done
	ok "-n \"$vid\"" "victim id never appeared"
	hub_ws hubany.ripdev.io - - "send={\"@\":[\"$vid\"],\"+\":[\"/vip\"]}" \
		'expectclose=1008,only the sending connection'
	json_has "$REPLY_WS" 'CLOSE 1008'
	hub_snapshot "$(hub_app_id)"
	if printf '%s' "$REPLY_BODY" | grep -qF '"/vip"'; then
		echo "rejected mutation applied: $REPLY_BODY" >&2
		return 1
	fi
	hub_publish "$(hub_app_id)" "{\"@\":[\"$vid\"],\"+\":[\"/vip\"]}"
	eq "$REPLY_CODE" "200"
	hub_snapshot "$(hub_app_id)"
	json_has "$REPLY_BODY" '"/vip":1'
	touch "$ROOT/.test-hub-flag-victim-go"
	hub_publish "$(hub_app_id)" "{\"@\":[\"$vid\"],\"enrolled\":1}"
	wait
	json_has "$(cat "$ROOT/.test-hub-out-victim")" 'DONE'
}

case_hub_channel_grammar_rejects() {
	hub_ws hubany.ripdev.io - - 'send={"+":["room"]}' 'expectclose=1008,want /-prefix'
	json_has "$REPLY_WS" 'CLOSE 1008'
}

case_hub_naming_only_hierarchy() {
	rm -f "$ROOT"/.test-hub-flag-*
	hub_ws_bg "$ROOT/.test-hub-out-hier" hubany.ripdev.io - - \
		'send={"+":["/h/sub"]}' touch="$ROOT/.test-hub-flag-hier" \
		waitfile="$ROOT/.test-hub-flag-hier-go" close
	wait_file "$ROOT/.test-hub-flag-hier"
	hub_publish "$(hub_app_id)" '{"@":["/h"],"chat":{}}'
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"deliveries":0'
	json_has "$REPLY_BODY" '"unknown_targets":1'
	touch "$ROOT/.test-hub-flag-hier-go"
	wait
}

case_hub_per_app_isolation() {
	# Same channel name in two apps: publish into ISO reaches only ISO.
	rm -f "$ROOT"/.test-hub-flag-*
	hub_ws_bg "$ROOT/.test-hub-out-iso1" hub1.ripdev.io https://hub1.ripdev.io - \
		'send={"+":["/shared"]}' touch="$ROOT/.test-hub-flag-iso1" noframe=800 close
	hub_ws_bg "$ROOT/.test-hub-out-iso2" hub2.ripdev.io https://hub2.ripdev.io - \
		'send={"+":["/shared"]}' touch="$ROOT/.test-hub-flag-iso2" expect=only close
	wait_file "$ROOT/.test-hub-flag-iso1"
	wait_file "$ROOT/.test-hub-flag-iso2"
	hub_publish "$(hub_iso_id)" '{"@":["/shared"],"only":{"iso":1}}'
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"deliveries":1'
	wait
	json_has "$(cat "$ROOT/.test-hub-out-iso1")" 'DONE'
	json_has "$(cat "$ROOT/.test-hub-out-iso2")" 'DONE'
}

case_hub_text_bridge_observation() {
	# The driver holds its socket open (waitfile) until the texts land:
	# a local close discards queued bridge texts by design (at-most-once).
	local t0
	t0="$(hub_bridge_count text)"
	rm -f "$ROOT/.test-hub-flag-obs-go"
	hub_ws_bg "$ROOT/.test-hub-out-obs" hubany.ripdev.io - - \
		'send={"obs1!":{"a": 1}}' expect=obs1 \
		'send={"obs2!":{"b" :2}}' expect=obs2 \
		'send={"obs3!":{"c":3}}' expect=obs3 \
		waitfile="$ROOT/.test-hub-flag-obs-go" close
	waitfor_bridge_texts $((t0 + 3))
	touch "$ROOT/.test-hub-flag-obs-go"
	wait
	json_has "$(cat "$ROOT/.test-hub-out-obs")" 'DONE'
	# In order, bodies byte-identical to the wire frames (spacing kept).
	local texts
	texts="$(grep '"kind": "text"' "$HUB_BRIDGE_LOG" | tail -3)"
	printf '%s' "$texts" | head -1 | grep -qF '{\"obs1!\":{\"a\": 1}}' || {
		echo "first text not verbatim: $(printf '%s' "$texts" | head -1)" >&2
		return 1
	}
	printf '%s' "$texts" | sed -n 2p | grep -qF '{\"obs2!\":{\"b\" :2}}' || {
		echo "second text not verbatim" >&2
		return 1
	}
}

case_hub_bridge_response_directives() {
	# The tenant answers a text with directives: they execute in the
	# SENDER's context — a bare name excludes the sender, other members
	# of /room receive.
	rm -f "$ROOT"/.test-hub-flag-*
	hub_playbook '{"text":{"status":200,"body":"{\"@\":[\"/brt\"],\"note\":{\"from\":\"tenant\"}}"}}'
	hub_ws_bg "$ROOT/.test-hub-out-brt" hubany.ripdev.io - - \
		'send={"+":["/brt"]}' touch="$ROOT/.test-hub-flag-brt" expect=note close
	wait_file "$ROOT/.test-hub-flag-brt"
	hub_ws hubany.ripdev.io - - 'send={"+":["/brt"]}' 'send={"trigger!":1}' expect=trigger noframe=700 close
	hub_playbook ''
	json_has "$REPLY_WS" 'DONE'
	wait
	json_has "$(cat "$ROOT/.test-hub-out-brt")" '"note":{"from":"tenant"}'
}

case_hub_bridge_garbage() {
	local g0
	g0="$(hub_stat bridge_garbage)"
	hub_playbook '{"text":{"status":200,"body":"this is not json"}}'
	hub_ws hubany.ripdev.io - - 'send={"garb!":1}' expect=garb 'send={"?":"alive"}' 'expect={"!":"alive"}' close
	hub_playbook ''
	json_has "$REPLY_WS" 'DONE'
	local i
	for i in $(seq 1 50); do
		[[ "$(hub_stat bridge_garbage)" -gt "$g0" ]] && return 0
		sleep 0.1
	done
	echo "bridge_garbage never counted" >&2
	return 1
}

case_hub_atomic_rejection() {
	# A frame with a bare/suffixed collision rejects WHOLE: the join in
	# its first object never applies.
	hub_ws hubany.ripdev.io - - \
		'send=[{"+":["/atomic"]},{"@":["/atomic"],"chat":{},"chat!":{}}]' \
		'expectclose=1008,appears as both'
	json_has "$REPLY_WS" 'CLOSE 1008'
	hub_snapshot "$(hub_app_id)"
	if printf '%s' "$REPLY_BODY" | grep -qF '"/atomic"'; then
		echo "rejected frame applied its join: $REPLY_BODY" >&2
		return 1
	fi
}

case_hub_text_failure_invisible() {
	# Tenant 500s texts: edge fan-out unaffected, sockets stay open,
	# bridge_failed counts.
	local f0
	f0="$(hub_stat bridge_failed)"
	hub_playbook '{"text":{"status":500,"body":"boom"}}'
	rm -f "$ROOT"/.test-hub-flag-*
	hub_ws_bg "$ROOT/.test-hub-out-tf" hubany.ripdev.io - - \
		'send={"+":["/tf"]}' touch="$ROOT/.test-hub-flag-tf" expect=still close
	wait_file "$ROOT/.test-hub-flag-tf"
	hub_ws hubany.ripdev.io - - 'send={"@":["/tf"],"still":{"up":1}}' 'send={"?":"ok"}' 'expect={"!":"ok"}' close
	hub_playbook ''
	json_has "$REPLY_WS" 'DONE'
	wait
	json_has "$(cat "$ROOT/.test-hub-out-tf")" 'DONE'
	local i
	for i in $(seq 1 50); do
		[[ "$(hub_stat bridge_failed)" -gt "$f0" ]] && return 0
		sleep 0.1
	done
	echo "bridge_failed never counted" >&2
	return 1
}

case_hub_reload_invisibility() {
	# Doorbell PUT (admission cut) + dirty window: the socket stays open,
	# membership stays, fan-out works mid-window; the held text bridge
	# completes once the pool publishes.
	rm -f "$ROOT"/.test-hub-flag-* "$ROOT/.test-hub-ring"
	hub_ws_bg "$ROOT/.test-hub-out-rl" hubany.ripdev.io - - \
		'send={"+":["/rl"]}' touch="$ROOT/.test-hub-flag-rl" \
		expect=midwindow 'send={"heldtext!":1}' expect=heldtext \
		'send={"?":"post-reload"}' 'expect={"!":"post-reload"}' \
		waitfile="$ROOT/.test-hub-flag-rl-go" close
	local driver_pid=$HUB_WS_PID
	wait_file "$ROOT/.test-hub-flag-rl"
	local t0
	t0="$(hub_bridge_count text)"
	# Admission cut: the doorbell (rings back to the real fixture sock).
	start_data_doorbell "$ROOT/run/hub-bell.sock" "$(hub_app_id)" "$HUB_SOCK" "$ROOT/.test-hub-ring" || return 1
	capi PUT "/1.0/apps/$(hub_app_id)/upstreams" \
		"{\"upstreams\":[{\"path\":\"$ROOT/run/hub-bell.sock\",\"doorbell\":true}]}"
	eq "$REPLY_CODE" "200"
	# Mid-window: fan-out rides above the worker plane.
	hub_publish "$(hub_app_id)" '{"@":["/rl"],"midwindow":{}}'
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"deliveries":1'
	# The client's next frame executes at the edge immediately (heldtext!
	# loops back) while its text bridge rings the doorbell, which restores
	# the pool; the held POST then completes against the fresh worker.
	waitfor_bridge_texts $((t0 + 1))
	ok "-s \"$ROOT/.test-hub-ring\"" "text bridge never rang the doorbell"
	touch "$ROOT/.test-hub-flag-rl-go"
	# Wait for the driver only: the doorbell fixture serves forever.
	wait "$driver_pid"
	json_has "$(cat "$ROOT/.test-hub-out-rl")" 'DONE'
}

case_hub_teardown_on_delete() {
	# DELETE tears the hub down: sockets close 1001 "app deregistered";
	# the snapshot 404s once the registration is gone.
	capi POST /1.0/apps '{"name":"hubdel","hosts":["hubdel2.ripdev.io"],"bridge_path":"/rt/bridge"}'
	eq "$REPLY_CODE" "201"
	local del_id
	del_id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	capi PUT "/1.0/apps/$del_id/upstreams" "{\"upstreams\":[{\"path\":\"$HUB_SOCK\"}]}"
	rm -f "$ROOT"/.test-hub-flag-*
	hub_ws_bg "$ROOT/.test-hub-out-del" hubdel2.ripdev.io https://hubdel2.ripdev.io - \
		'send={"+":["/dying"]}' touch="$ROOT/.test-hub-flag-del" \
		'expectclose=1001,app deregistered'
	wait_file "$ROOT/.test-hub-flag-del"
	sleep 0.2
	capi DELETE "/1.0/apps/$del_id"
	eq "$REPLY_CODE" "204"
	wait
	json_has "$(cat "$ROOT/.test-hub-out-del")" 'CLOSE 1001'
	hub_snapshot "$del_id"
	eq "$REPLY_CODE" "404"
}

case_hub_slow_consumer() {
	# A recipient that never reads: the outbound queue caps trip and the
	# connection closes 1013 — the sender (publish) is unaffected.
	local s0 o0
	s0="$(hub_stat slow_closes)"
	o0="$(hub_bridge_count open)"
	start_hub_wedge hubany.ripdev.io
	local i wid=""
	for i in $(seq 1 50); do
		if [[ "$(hub_bridge_count open)" -gt "$o0" ]]; then
			wid="$(grep '"kind": "open"' "$HUB_BRIDGE_LOG" | tail -1 |
				"$TESTKIT" json get client)"
			break
		fi
		sleep 0.1
	done
	ok "-n \"$wid\"" "wedge connection never opened"
	# Flood: 30 × ~100KB. The kernel socket buffers absorb the first few;
	# the writer then wedges and the 1MiB outbound queue overflows well
	# before the write deadline.
	local blob
	blob="$("$TESTKIT" repeat x 100000)"
	for i in $(seq 1 30); do
		hub_publish "$(hub_app_id)" "{\"@\":[\"$wid\"],\"flood\":\"$blob\"}"
		eq "$REPLY_CODE" "200"
	done
	for i in $(seq 1 100); do
		[[ "$(hub_stat slow_closes)" -gt "$s0" ]] && break
		sleep 0.1
	done
	ok "$(hub_stat slow_closes) -gt $s0" "slow consumer never closed"
	# The close bridge reports 1013 for that connection. The Close frame
	# rides a 10s write deadline into a socket the wedge never drains, so
	# the bridge record can trail the counter by that full deadline.
	for i in $(seq 1 150); do
		if grep '"kind": "close"' "$HUB_BRIDGE_LOG" | grep -qF "\"client\": \"$wid\""; then
			break
		fi
		sleep 0.1
	done
	grep '"kind": "close"' "$HUB_BRIDGE_LOG" | grep -F "\"client\": \"$wid\"" | tail -1 | grep -qF '1013' || {
		echo "close bridge for the wedged conn lacks 1013" >&2
		return 1
	}
}

case_hub_oversize_frame() {
	hub_ws hubany.ripdev.io - - sendbig=70000 'expectclose=1009'
	json_has "$REPLY_WS" 'CLOSE 1009'
}

case_hub_cap_floor_and_reservation() {
	# One app spans hosts capped 10 and 20: the effective floor is 10 —
	# enforced with slot reservation while open bridges are in flight,
	# even arriving through the 20-capped host.
	hub_playbook '{"open":{"status":204,"delay_ms":2000}}'
	rm -f "$ROOT/.test-hub-cap-codes"
	local i
	for i in $(seq 1 10); do
		curl -sS -o /dev/null -w '%{http_code}\n' --max-time 4 --http1.1 \
			-H 'Connection: Upgrade' -H 'Upgrade: websocket' \
			-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
			"https://hubtwenty.ripdev.io/hub" >>"$ROOT/.test-hub-cap-codes" 2>/dev/null &
	done
	sleep 1
	# All ten slots reserved: the 11th rejects 503 immediately.
	eq "$(hub_upgrade_code hubtwenty.ripdev.io -)" "503"
	wait
	hub_playbook ''
	# The ten held handshakes completed (101) — reservation ≠ rejection.
	eq "$(sort -u "$ROOT/.test-hub-cap-codes" | tr -d '[:space:]')" "101"
	# Their curl drivers die on max-time; cleanup releases every slot.
	for i in $(seq 1 100); do
		hub_snapshot "$(hub_cap_id)"
		if printf '%s' "$REPLY_BODY" | grep -qF '"conns":0'; then
			return 0
		fi
		sleep 0.1
	done
	echo "cap-app conns never drained: $REPLY_BODY" >&2
	return 1
}

case_hub_open_teardown_race() {
	# DELETE while an open bridge is pending: the returning 2xx sees the
	# tombstone — no 101, no zombie connection.
	capi POST /1.0/apps '{"name":"hubrace","hosts":["hubrace2.ripdev.io"],"bridge_path":"/rt/bridge"}'
	eq "$REPLY_CODE" "201"
	local race_id
	race_id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	capi PUT "/1.0/apps/$race_id/upstreams" "{\"upstreams\":[{\"path\":\"$HUB_SOCK\"}]}"
	hub_playbook '{"open":{"status":204,"delay_ms":1500}}'
	local code_file="$ROOT/.test-hub-flag-racecode"
	rm -f "$code_file"
	(hub_upgrade_code hubrace2.ripdev.io https://hubrace2.ripdev.io >"$code_file") &
	sleep 0.4
	capi DELETE "/1.0/apps/$race_id"
	eq "$REPLY_CODE" "204"
	wait
	hub_playbook ''
	eq "$(cat "$code_file")" "503"
	capi GET /1.0/hub
	if printf '%s' "$REPLY_BODY" | grep -qF "\"$race_id\":{\"conns\":1"; then
		echo "zombie connection after teardown race" >&2
		return 1
	fi
}

case_hub_origin_policy() {
	# origin same (hub1 inherits the global default): no Origin and
	# wrong-Origin fail 403 BEFORE any bridge; matching Origin admits.
	local o0
	o0="$(hub_bridge_count open)"
	eq "$(hub_upgrade_code hub1.ripdev.io -)" "403"
	eq "$(hub_upgrade_code hub1.ripdev.io https://evil.example.com)" "403"
	eq "$(hub_bridge_count open)" "$o0"
	eq "$(hub_upgrade_code hub1.ripdev.io https://hub1.ripdev.io)" "101"
	# origin any admits an Origin-less client (proven throughout by the
	# driver, pinned here).
	eq "$(hub_upgrade_code hubany.ripdev.io -)" "101"
}

case_hub_interception_scope() {
	# The hub claims upgrades only: a plain GET to the hub path flows
	# through the data plane to the tenant; an upgrade on a hub-off site
	# (api.ripdev.io) is never intercepted.
	eq "$(http_body https://hubany.ripdev.io/hub)" "plain:/hub"
	local body
	body="$(curl -sS --max-time 5 --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
		-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
		"https://api.ripdev.io/hub")"
	eq "$body" "plain:/hub"
}

case_hub_publish_plane_errors() {
	# Absent @ → 400 positioned; empty @ → 400; unknown app → 404;
	# hub-less app → 409.
	hub_publish "$(hub_app_id)" '{"chat":{}}'
	eq "$REPLY_CODE" "400"
	json_has "$REPLY_BODY" 'item 0'
	json_has "$REPLY_BODY" 'required on the publish plane'
	hub_publish "$(hub_app_id)" '{"@":[],"chat":{}}'
	eq "$REPLY_CODE" "400"
	hub_publish "nope-zzzzzz" '{"@":["/x"],"chat":{}}'
	eq "$REPLY_CODE" "404"
	capi POST /1.0/apps '{"name":"hubless","hosts":["hubless.example.com"]}'
	eq "$REPLY_CODE" "201"
	local hubless_id
	hubless_id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	hub_publish "$hubless_id" '{"@":["/x"],"chat":{}}'
	eq "$REPLY_CODE" "409"
	json_has "$REPLY_BODY" 'not enabled for any site'
	capi DELETE "/1.0/apps/$hubless_id"
}

case_hub_publish_kick() {
	rm -f "$ROOT"/.test-hub-flag-*
	hub_ws_bg "$ROOT/.test-hub-out-kick" hubany.ripdev.io - - \
		id touch="$ROOT/.test-hub-flag-kick" 'expect={"*":"kicked"}' 'expectclose=1000,kicked'
	wait_file "$ROOT/.test-hub-flag-kick"
	local kid i
	for i in $(seq 1 50); do
		kid="$(sed -n 's/^ID //p' "$ROOT/.test-hub-out-kick" 2>/dev/null)"
		[[ -n "$kid" ]] && break
		sleep 0.1
	done
	ok "-n \"$kid\"" "kick target id never appeared"
	local c0
	c0="$(hub_bridge_count close)"
	hub_publish "$(hub_app_id)" "{\"@\":[\"$kid\"],\"*\":\"kicked\"}"
	eq "$REPLY_CODE" "200"
	wait
	json_has "$(cat "$ROOT/.test-hub-out-kick")" 'CLOSE 1000 kicked'
	waitfor_bridge_close $((c0 + 1))
}

waitfor_bridge_close() {
	local want=$1 i
	for i in $(seq 1 50); do
		[[ "$(hub_bridge_count close)" -ge "$want" ]] && return 0
		sleep 0.1
	done
	echo "close bridge count never reached $want" >&2
	return 1
}

case_hub_caddy_reload_persistence() {
	# A cold config reload: the socket, its id, its membership, and
	# fan-out all survive through the pooled registry/hub state.
	rm -f "$ROOT"/.test-hub-flag-*
	hub_ws_bg "$ROOT/.test-hub-out-crl" hubany.ripdev.io - - \
		'send={"+":["/keep"]}' touch="$ROOT/.test-hub-flag-crl" \
		expect=survived 'send={"?":"post"}' 'expect={"!":"post"}' close
	wait_file "$ROOT/.test-hub-flag-crl"
	if ! XDG_DATA_HOME="$ROOT/.test-caddy-data" \
		"$CADDY_BIN" reload --config "$ROOT/Caddyfile" --force >>"$CADDY_LOG" 2>&1; then
		echo "caddy reload failed; see $CADDY_LOG" >&2
		return 1
	fi
	local i
	for i in $(seq 1 50); do
		curl -sS -o /dev/null --max-time 1 http://127.0.0.1:7600/1.0/health 2>/dev/null && break
		sleep 0.1
	done
	# Membership survived the reload.
	hub_snapshot "$(hub_app_id)"
	json_has "$REPLY_BODY" '"/keep":1'
	# Fan-out still works on the same socket.
	hub_publish "$(hub_app_id)" '{"@":["/keep"],"survived":{}}'
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"deliveries":1'
	wait
	json_has "$(cat "$ROOT/.test-hub-out-crl")" 'DONE'
}

case_hub_bridge_snapshot_cap() {
	# >32 KiB of filtered handshake headers → 431, never truncated.
	local big
	big="c=$("$TESTKIT" repeat x 33000)"
	eq "$(hub_upgrade_code hubany.ripdev.io - "Cookie: $big")" "431"
}

case_hub_snapshot_opacity() {
	# Snapshot handles are opaque (conn-N), never raw connection ids.
	rm -f "$ROOT"/.test-hub-flag-*
	hub_ws_bg "$ROOT/.test-hub-out-op" hubany.ripdev.io - - \
		id touch="$ROOT/.test-hub-flag-op" waitfile="$ROOT/.test-hub-flag-op-go" close
	wait_file "$ROOT/.test-hub-flag-op"
	local oid i
	for i in $(seq 1 50); do
		oid="$(sed -n 's/^ID //p' "$ROOT/.test-hub-out-op" 2>/dev/null)"
		[[ -n "$oid" ]] && break
		sleep 0.1
	done
	ok "-n \"$oid\"" "opacity conn id never appeared"
	hub_snapshot "$(hub_app_id)"
	if printf '%s' "$REPLY_BODY" | grep -qF "$oid"; then
		echo "snapshot leaked a raw connection id: $REPLY_BODY" >&2
		return 1
	fi
	json_has "$REPLY_BODY" '"conn-'
	touch "$ROOT/.test-hub-flag-op-go"
	wait
}

case_hub_parse_rejections() {
	local bad dir
	dir="$(mktemp -d /tmp/janus-hub-parse.XXXXXX)"
	local -a cases=(
		'hub maybe'
		'hub off { path /x }'
		'hub { bogus 1 }'
		'hub { path }'
		'hub { path relative }'
		'hub { path "/x?y" }'
		'hub { max_conns 0 }'
		'hub { max_conns many }'
		'hub { max_channels 0 }'
		'hub { max_frame 512b }'
		'hub { origin }'
		'hub { origin any same }'
		'hub { origin "not a host" }'
		'hub { path /x
			path /y }'
	)
	for bad in "${cases[@]}"; do
		cat >"$dir/Caddyfile" <<EOF
{
	janus {
		$bad
	}
}
EOF
		if "$CADDY_BIN" adapt --config "$dir/Caddyfile" >/dev/null 2>&1; then
			echo "caddy adapt accepted illegal hub config: $bad" >&2
			rm -rf "$dir"
			return 1
		fi
	done
	rm -rf "$dir"
}

# --- cases: tenant ------------------------------------------------------------
#
# Phase 8: the first real tenant. Runs the actual @rip-lang/server manager
# and workers from the sibling rip checkout (../rip) against this Janus:
# registration on /1.0, HTTPS routing to a worker unix socket, hot reload
# through the doorbell (a save is never served stale), heartbeats, and a
# clean SIGTERM shutdown. The group reuses the JANUS_HEARTBEAT_TTL=2s caddy
# (manager heartbeats are shortened to match), so TTL survival is observable
# in seconds.
#
# The sibling checkout and bun are REQUIRED — missing pieces fail the suite.

RIP_ROOT="$(cd "$ROOT/.." && pwd)/rip"
TENANT_DIR_FILE="$ROOT/.test-tenant-dir"
TENANT_PID_FILE="$ROOT/.test-tenant-pid"
TENANT_LOG="$ROOT/.test-tenant-manager.log"
TENANT_HOST="e2e.ripdev.io"

tenant_dir() { cat "$TENANT_DIR_FILE"; }
tenant_pid() { cat "$TENANT_PID_FILE"; }

require_rip() {
	local missing=()
	[[ -f "$RIP_ROOT/src/loader.js" ]] || missing+=("$RIP_ROOT/src/loader.js")
	[[ -f "$RIP_ROOT/packages/server/server.rip" ]] || missing+=("$RIP_ROOT/packages/server/server.rip")
	command -v bun >/dev/null 2>&1 || missing+=("bun on PATH (https://bun.sh)")
	if ((${#missing[@]} > 0)); then
		echo "tenant group requires the sibling rip checkout and bun; missing:" >&2
		printf '  %s\n' "${missing[@]}" >&2
		echo "clone the rip repo next to this one (../rip) — do not skip this group" >&2
		return 1
	fi
}

write_tenant_app() {
	local version=$1
	cat >"$(tenant_dir)/app.rip" <<RIP
import { get, post, start } from '@rip-lang/server'

get '/' -> { message: 'hello', version: $version }

post '/echo', ->
  @req.text!

start()
RIP
}

stop_tenant() {
	local pid
	if [[ -f "$TENANT_PID_FILE" ]]; then
		pid="$(cat "$TENANT_PID_FILE")"
		kill "$pid" 2>/dev/null || true
	fi
	pkill -f 'server/worker\.rip' 2>/dev/null || true
	if [[ -f "$TENANT_DIR_FILE" ]]; then
		rm -rf "$(cat "$TENANT_DIR_FILE")"
	fi
	rm -f "$TENANT_DIR_FILE" "$TENANT_PID_FILE" "$TENANT_LOG"
}

# tenant_upstreams — the app's current upstream list (compact JSON)
tenant_upstreams() {
	capi GET /1.0/apps
	printf '%s' "$REPLY_BODY" | "$TESTKIT" upstreams "$TENANT_HOST"
}

case_tenant_register() {
	require_rip || return 1
	local dir
	dir="$(mktemp -d /tmp/janus-tenant.XXXXXX)"
	printf '%s' "$dir" >"$TENANT_DIR_FILE"
	write_tenant_app 1
	# Resolve @rip-lang/server to the sibling checkout — never a published
	# copy from bun's global cache.
	mkdir -p "$dir/node_modules/@rip-lang"
	ln -sfn "$RIP_ROOT/packages/server" "$dir/node_modules/@rip-lang/server"
	(cd "$dir" && exec env RIP_HEARTBEAT_MS=500 \
		bun --preload="$RIP_ROOT/src/loader.js" "$RIP_ROOT/packages/server/server.rip" \
		--name e2e --host "$TENANT_HOST" --workers 2 \
		--control http://127.0.0.1:7600) >"$TENANT_LOG" 2>&1 &
	local pid=$!
	printf '%s' "$pid" >"$TENANT_PID_FILE"
	local i
	for i in $(seq 1 100); do
		if ! kill -0 "$pid" 2>/dev/null; then
			echo "manager exited early:" >&2
			tail -10 "$TENANT_LOG" >&2 || true
			return 1
		fi
		capi GET /1.0/apps
		if printf '%s' "$REPLY_BODY" | grep -qF "\"$TENANT_HOST\""; then
			json_has "$REPLY_BODY" '"name":"e2e"'
			return
		fi
		sleep 0.1
	done
	echo "app never appeared in GET /1.0/apps; manager log:" >&2
	tail -10 "$TENANT_LOG" >&2 || true
	return 1
}

case_tenant_get_json() {
	# First hit may ring the doorbell (watch mode boots on demand).
	local body
	body="$(curl -sS --max-time 20 "https://$TENANT_HOST/")"
	json_has "$body" '"message":"hello"'
	json_has "$body" '"version":1'
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "https://$TENANT_HOST/")" "200"
}

case_tenant_echo_body() {
	local body
	body="$(curl -sS --max-time 5 -X POST -H 'Content-Type: application/octet-stream' \
		--data 'tenant-payload-123' "https://$TENANT_HOST/echo")"
	eq "$body" "tenant-payload-123"
}

case_tenant_reload_fresh_code() {
	write_tenant_app 2
	# The save settles (~150ms) and cuts admission: the doorbell becomes the
	# only upstream. Wait for the cut so the next request provably rings.
	local i cut=""
	for i in $(seq 1 50); do
		if tenant_upstreams | grep -qF '"doorbell":true'; then
			cut=1
			break
		fi
		sleep 0.1
	done
	ok "-n \"$cut\"" "doorbell never published after save; upstreams: $(tenant_upstreams)"
	# The ring boots a fresh pool from the latest files: the response is the
	# NEW code — never the old.
	local body
	body="$(curl -sS --max-time 20 "https://$TENANT_HOST/")"
	json_has "$body" '"version":2'
}

case_tenant_heartbeats_keep_alive() {
	# TTL here is 2s; the manager beats every 500ms. Quiet traffic across
	# multiple TTLs must keep the app registered and routable.
	sleep 3.5
	local body
	body="$(curl -sS --max-time 5 "https://$TENANT_HOST/")"
	json_has "$body" '"version":2'
	capi GET /1.0/apps
	json_has "$REPLY_BODY" "\"$TENANT_HOST\""
}

case_tenant_sigterm_deregisters() {
	kill -TERM "$(tenant_pid)" 2>/dev/null
	# Shutdown: PUT upstreams [] → drain → kill workers → DELETE app.
	local i gone=""
	for i in $(seq 1 100); do
		capi GET /1.0/apps
		if ! printf '%s' "$REPLY_BODY" | grep -qF "\"$TENANT_HOST\""; then
			gone=1
			break
		fi
		sleep 0.1
	done
	ok "-n \"$gone\"" "app still registered 10s after SIGTERM"
	eq "$(http_code "https://$TENANT_HOST/")" "404"
}

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
test "local GET /1.0/health → ok" case_control_local_health
test "unix GET /1.0 → janus meta" case_control_unix_root
test "unix GET /1.0/health → ok" case_control_unix_health
test "unknown /1.0 paths → 404, wrong method → 405" case_control_unknown_paths_404
test "reload → both listeners serve one live registry" case_reload_no_split_brain

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

group "cache"
test "register app + fixture upstreams, /1.0/cache answers" case_cache_register
test "hit serves without touching the worker (Age, debug header)" case_cache_hit_serves_without_worker
test "Cookie bypasses (never serve, never store)" case_cache_cookie_bypass
test "Authorization / Proxy-Authorization bypass" case_cache_auth_bypass
test "POST bypasses" case_cache_post_bypass
test "Set-Cookie never stored" case_cache_setcookie_never_stored
test "no-store / private / unparseable CC / Expires vetoes" case_cache_origin_vetoes_respected
test "Content-Encoding without Vary never stored; with Vary stores" case_cache_content_encoding
test "ACAO echo never stored; static * stores" case_cache_acao
test "Vary respected — variants coexist under one key" case_cache_vary_respected
test "unbounded Vary never stored" case_cache_unbounded_vary_never_stored
test "non-200 never stored" case_cache_non200_never_stored
test "marked 503 never stored" case_cache_marked_503_never_stored
test "truncated fill never stored" case_cache_truncated_fill_never_stored
test "max_body exceeded streams uncached, intact" case_cache_max_body_streams_uncached
test "coalescing: 32 concurrent cold misses → 1 origin request" case_cache_coalescing_stampede
test "waiter cap overflow falls through (no manufactured 503)" case_cache_waiter_cap_overflow
test "fill failure falls through + do-not-coalesce mark" case_cache_fill_failure_falls_through
test "purge on upstream swap → next GET reaches the new worker" case_cache_purge_on_upstream_swap
test "purge race: straddling fill fenced, nothing stored" case_cache_purge_race_fill_straddles_put
test "host re-claim never serves the old tenant" case_cache_host_reclaim
test "TTL expiry → worker again" case_cache_ttl_expiry
test "cache off site: repeats always reach the worker" case_cache_off_site
test "cascade: per-site ttl override observed" case_cache_cascade_ttl_override
test "parse rejections: every hard error fails caddy adapt" case_cache_parse_rejections

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
test "open handshake: bridge headers, tenant enrolls, snapshot agrees" case_hub_open_full_path
test "open rejected by tenant → status+body forwarded, no conn" case_hub_open_rejected_by_tenant
test "open with tenant down → 503 + Retry-After" case_hub_open_tenant_down
test "no bridge_path → 503" case_hub_no_bridge_path
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
test "caddy reload: socket, membership, fan-out survive" case_hub_caddy_reload_persistence
test "handshake snapshot over 32KiB → 431" case_hub_bridge_snapshot_cap
test "snapshot exposes opaque handles, never raw ids" case_hub_snapshot_opacity
test "parse rejections: every hub hard error fails caddy adapt" case_hub_parse_rejections

group "tenant"
test "real manager registers on /1.0" case_tenant_register
test "GET / routes through Janus to a worker" case_tenant_get_json
test "POST /echo body arrives intact" case_tenant_echo_body
test "save → ring → response is the NEW code" case_tenant_reload_fresh_code
test "heartbeats keep the app alive past the TTL" case_tenant_heartbeats_keep_alive
test "SIGTERM → clean deregistration" case_tenant_sigterm_deregisters

report
exit $?
