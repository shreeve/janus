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
	stop_tenant
	rm -f "$ROOT/.test-app-id" "$ROOT/.test-hb-app-id" "$ROOT/.test-tls-app-id" "$ROOT/.test-fixtures.log"
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
	# registry. The registry itself is memory-only per config generation, so
	# the pre-reload app is gone and the tenant re-registers (heartbeat →
	# 404 → re-register, per the pool protocol).
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

	# Both listeners answer, and both see the same (fresh) registry.
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

	capi POST /1.0/apps '{"name":"reload","hosts":["reload.example.com"]}'
	eq "$REPLY_CODE" "201"
	local new_id
	new_id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	capi GET /1.0/apps
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" "\"$new_id\""
	capi_unix GET /1.0/apps
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" "\"$new_id\""
	# No half-started app lingers behind either listener: the pre-reload
	# registration is gone from BOTH (split-brain would show it on one).
	if printf '%s' "$REPLY_BODY" | grep -qF "$old_id"; then
		printf 'stale registry behind the unix listener: %q' "$REPLY_BODY" >&2
		return 1
	fi
	capi DELETE "/1.0/apps/$new_id"
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
	python3 - "$sock" "$name" "$hitfile" >>"$ROOT/.test-fixtures.log" 2>&1 <<'PY' &
import http.server, socketserver, sys

sock, name, hitfile = sys.argv[1], sys.argv[2], sys.argv[3]

class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def _send(self, code, body):
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        self._send(200, f"upstream:{name}\n".encode())
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        data = self.rfile.read(n)
        with open(hitfile, "ab") as f:
            f.write(data + b"\n")
        self._send(200, b"received:" + data + b"\n")
    def log_message(self, *args): pass
    def address_string(self): return "unix"

class S(socketserver.ThreadingUnixStreamServer):
    daemon_threads = True

S(sock, H).serve_forever()
PY
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
	python3 - "$sock" "$appid" "$newsock" "$ringfile" >>"$ROOT/.test-fixtures.log" 2>&1 <<'PY' &
import http.server, socketserver, json, sys, urllib.request

sock, appid, newsock, ringfile = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def do_GET(self):
        if self.path != "/ring":
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        with open(ringfile, "ab") as f:
            f.write(b"ring\n")
        body = json.dumps({"upstreams": [{"path": newsock}]}).encode()
        req = urllib.request.Request(
            f"http://127.0.0.1:7600/1.0/apps/{appid}/upstreams",
            data=body, method="PUT",
            headers={"Content-Type": "application/json"})
        resp = urllib.request.urlopen(req, timeout=5)
        assert resp.status == 200, f"PUT upstreams -> {resp.status}"
        self.send_response(204)
        self.end_headers()
    def log_message(self, *args): pass
    def address_string(self): return "unix"

class S(socketserver.ThreadingUnixStreamServer):
    daemon_threads = True

S(sock, H).serve_forever()
PY
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
	printf '%s' "$REPLY_BODY" | python3 -c '
import json, sys
apps = json.load(sys.stdin)
for a in apps:
    if "'"$TENANT_HOST"'" in a["hosts"]:
        print(json.dumps(a["upstreams"]))
        break
'
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
		if tenant_upstreams | grep -qF '"doorbell": true'; then
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

group "tenant"
test "real manager registers on /1.0" case_tenant_register
test "GET / routes through Janus to a worker" case_tenant_get_json
test "POST /echo body arrives intact" case_tenant_echo_body
test "save → ring → response is the NEW code" case_tenant_reload_fresh_code
test "heartbeats keep the app alive past the TTL" case_tenant_heartbeats_keep_alive
test "SIGTERM → clean deregistration" case_tenant_sigterm_deregisters

report
exit $?
