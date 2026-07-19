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
	if date +%s%N >/dev/null 2>&1; then
		date +%s%N
	else
		# macOS fallback: seconds + milliseconds via python
		python3 - <<'PY'
import time
print(int(time.time() * 1_000_000_000))
PY
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
		set +e
		"$@" 2>&1
		rc=$?
		exit $rc
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
	rm -f "$ROOT/run/janus.sock"
	sleep 0.5
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
	rm -f "$ROOT/.test-app-id"
}

trap cleanup EXIT INT TERM

# --- cases: ping ----------------------------------------------------------

case_ping_catchall_foo() {
	eq "$(http_body https://foo.ripdev.io/ping)" $'pong\n'
	eq "$(http_code https://foo.ripdev.io/ping)" "200"
}

case_ping_catchall_bar() {
	eq "$(http_body https://bar.ripdev.io/ping)" $'pong\n'
	eq "$(http_code https://bar.ripdev.io/ping)" "200"
}

case_ping_on_explicit() {
	eq "$(http_body https://on.ripdev.io/ping)" $'pong\n'
	eq "$(http_code https://on.ripdev.io/ping)" "200"
}

case_ping_off_explicit() {
	eq "$(http_code https://off.ripdev.io/ping)" "404"
	ne "$(http_body https://off.ripdev.io/ping)" $'pong\n'
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

report
exit $?
