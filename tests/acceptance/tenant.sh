# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: tenant ------------------------------------------------------------
#
# Phase 8: the first real tenant. Runs the actual rip/sites manager
# and workers from the sibling rip checkout (../rip) against this Janus:
# registration on /1.0, HTTPS routing to a worker unix socket, hot reload
# through the doorbell (a save is never served stale), heartbeats, and a
# clean SIGTERM shutdown. The group reuses the JANUS_HEARTBEAT_TTL=2s caddy
# (manager heartbeats are shortened to match), so TTL survival is observable
# in seconds.
#
# The sibling checkout and bun are REQUIRED — missing pieces fail the suite.

RIP_ROOT="${RIP_ROOT:-$(cd "$ROOT/.." && pwd)/rip}"
TENANT_DIR_FILE="$TEST_RUN_DIR/tenant-dir"
TENANT_PID_FILE="$TEST_RUN_DIR/tenant-pid"
TENANT_LOG="$TEST_RUN_DIR/tenant-manager.log"
TENANT_HOST="e2e.ripdev.io"

tenant_dir() { cat "$TENANT_DIR_FILE"; }
tenant_pid() { cat "$TENANT_PID_FILE"; }

require_rip() {
	local missing=()
	[[ -f "$RIP_ROOT/src/loader.js" ]] || missing+=("$RIP_ROOT/src/loader.js")
	[[ -f "$RIP_ROOT/packages/sites/manager.rip" ]] || missing+=("$RIP_ROOT/packages/sites/manager.rip")
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
import { get, post, start } from 'rip/sites'

get '/' -> { message: 'hello', version: $version }

post '/echo', ->
  @req.text!

start()
RIP
}

stop_tenant() {
	local pid child children tenant_path tenant_real tmp_real
	if [[ -f "$TENANT_PID_FILE" ]]; then
		pid="$(cat "$TENANT_PID_FILE")"
		if pid_is_owned "$pid" "$RIP_ROOT/packages/sites/manager.rip"; then
			# Capture only this manager's direct workers before asking it to exit.
			# A global pkill can terminate unrelated Rip sites on the same machine.
			children="$(pgrep -P "$pid" 2>/dev/null || true)"
			stop_owned_pid "$pid" "$RIP_ROOT/packages/sites/manager.rip"
			wait "$pid" 2>/dev/null || true
			for child in $children; do
				stop_owned_pid "$child" "$RIP_ROOT/packages/sites/worker.rip"
			done
		else
			stop_owned_pid "$pid" "$RIP_ROOT/packages/sites/manager.rip"
		fi
	fi
	if [[ -f "$TENANT_DIR_FILE" ]]; then
		tenant_path="$(cat "$TENANT_DIR_FILE")"
		if [[ -d "$tenant_path" ]]; then
			tenant_real="$(cd "$tenant_path" && pwd -P)"
			tmp_real="$(cd "$TEST_RUN_DIR" && pwd -P)"
			if [[ "$(dirname "$tenant_real")" == "$tmp_real" &&
				"$(basename "$tenant_real")" == janus-tenant.* ]]; then
				rm -rf "$tenant_real"
			else
				echo "refusing to remove unexpected tenant directory: $tenant_real" >&2
			fi
		fi
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
	dir="$(mktemp -d "${TEST_RUN_DIR}/janus-tenant.XXXXXX")"
	printf '%s' "$dir" >"$TENANT_DIR_FILE"
	write_tenant_app 1
	(cd "$dir" && exec env RIP_HEARTBEAT_MS=500 \
		bun --preload="$RIP_ROOT/src/loader.js" "$RIP_ROOT/packages/sites/manager.rip" \
		"$dir/app.rip" --watch --name e2e --host "$TENANT_HOST" --workers 2 \
		--control http://127.0.0.1:7600 --access-log=off) >"$TENANT_LOG" 2>&1 &
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
