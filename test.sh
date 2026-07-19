#!/usr/bin/env bash
# test.sh — high-level Janus acceptance suite (self-contained).
#
# For operators/users: prove cold capabilities behave end-to-end.
# Developers still use idiomatic `go test ./...` while building.
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

start_caddy() {
	# clear stale listeners from prior runs
	if lsof -nP -iTCP:443 -sTCP:LISTEN >/dev/null 2>&1; then
		lsof -nP -iTCP:443 -sTCP:LISTEN 2>/dev/null | awk 'NR>1 {print $2}' | sort -u | while read -r pid; do
			kill "$pid" 2>/dev/null || true
		done
		sleep 1
	fi
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
		if curl -sS -o /dev/null --max-time 1 https://on.ripdev.io/ping 2>/dev/null; then
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

report
exit $?
