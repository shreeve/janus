# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: reload -----------------------------------------------------------

reload_caddy() {
	JANUS_HEARTBEAT_TTL="$(cat "$TEST_RUN_DIR/heartbeat-ttl")" "$CADDY_BIN" reload "$@"
}

case_reload_ttl_rejected() {
	capi GET /1.0
	json_has "$REPLY_BODY" '"heartbeat_ttl":"15s"'
	if JANUS_HEARTBEAT_TTL=30s "$CADDY_BIN" reload --config "$ROOT/Caddyfile" --force >"$TEST_RUN_DIR/ttl-reload.log" 2>&1; then
		echo "reload accepted a changed heartbeat TTL" >&2
		return 1
	fi
	json_has "$(cat "$TEST_RUN_DIR/ttl-reload.log")" 'requires a restart'
	capi GET /1.0
	json_has "$REPLY_BODY" '"heartbeat_ttl":"15s"'
	eq "$(http_body https://on.ripdev.io/ping)" "pong"
}

case_reload_no_split_brain() {
	# A config reload swaps in a new Janus app while the old one still holds
	# its sockets: listener pooling lets the new app bind, the reload
	# succeeds, and afterward BOTH control listeners serve the same live
	# registry. The registry lives in pooled process state
	# (docs/20260720-162350-hub-design.md "Caddy config reload"): the
	# pre-reload registration SURVIVES the reload — only DELETE, TTL reap,
	# or a process restart removes it. Split-brain would show a fresh,
	# empty registry behind one listener.
	capi POST /1.0/apps '{"name":"reload","hosts":["reload.ripdev.io"]}'
	eq "$REPLY_CODE" "201"
	local old_id
	old_id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	ok "-n \"$old_id\"" "no id in $REPLY_BODY"

	if ! XDG_DATA_HOME="$TEST_RUN_DIR/caddy-data" \
		reload_caddy --config "$ROOT/Caddyfile" --force >>"$CADDY_LOG" 2>&1; then
		echo "caddy reload failed; see $CADDY_LOG" >&2
		return 1
	fi

	# Both listeners answer, and both see the same live registry.
	local i ready=""
	for i in $(seq 1 50); do
		if curl -sS -o /dev/null --max-time 1 http://127.0.0.1:7600/1.0/health 2>/dev/null &&
			curl -sS -o /dev/null --max-time 1 --unix-socket "$TEST_RUN_DIR/run/janus.sock" http://janus/1.0/health 2>/dev/null; then
			ready=1
			break
		fi
		sleep 0.1
	done
	ok "-n \"$ready\"" "control listeners never answered after reload"
	ok "-S \"$TEST_RUN_DIR/run/janus.sock\"" "internal control socket vanished after reload"
	# Every probe opens a new UDS connection. Repeated success pins Caddy's
	# listener reference count: retiring the old generation must not close or
	# unlink the new generation's socket handle.
	for i in $(seq 1 10); do
		capi_unix GET /1.0/health
		eq "$REPLY_CODE" "200"
	done

	# The pre-reload registration survived the reload on BOTH listeners.
	capi GET /1.0/apps
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" "\"$old_id\""
	capi_unix GET /1.0/apps
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" "\"$old_id\""
	# The active HTTPS handler is bound to that same registry. With no
	# upstreams the registered host is unavailable (503), never unknown
	# (404); this catches a data-plane/control-plane split after reload.
	eq "$(http_code https://reload.ripdev.io/)" "503"
	# Its host is still claimed: a rival registration conflicts.
	capi POST /1.0/apps '{"name":"rival","hosts":["reload.ripdev.io"]}'
	eq "$REPLY_CODE" "409"
	capi DELETE "/1.0/apps/$old_id"
	eq "$REPLY_CODE" "204"
}

case_reload_abort_recovery() {
	# An ABORTED reload must not poison later reloads. Caddy's unix reuse
	# map repoints at the newest generation's control listener; when that
	# generation aborts (janus bound its sockets, then a later control
	# entry failed inside Start), the map kept a CLOSED listener and every
	# genuinely-different reload afterward failed with "use of closed
	# network connection" until the process restarted.
	local bad_cfg="$TEST_RUN_DIR/abort-bad.caddyfile"
	local good_cfg="$TEST_RUN_DIR/abort-good.caddyfile"
	# Bad candidate: a public control entry whose TLS material is missing
	# fails inside Start, AFTER the unix + local listeners already bound.
	awk '{print} $1 == "control" && $2 == "local" {print "\t\tcontrol public https://127.0.0.1:7601 token:JANUS_ABORT_TOKEN cert:/nonexistent/janus-abort.crt key:/nonexistent/janus-abort.key"}' \
		"$ROOT/Caddyfile" >"$bad_cfg"
	if XDG_DATA_HOME="$TEST_RUN_DIR/caddy-data" \
		reload_caddy --config "$bad_cfg" --force >>"$CADDY_LOG" 2>&1; then
		echo "bad config reload unexpectedly succeeded" >&2
		return 1
	fi
	# The surviving generation still serves and its socket file is intact.
	ok "-S \"$TEST_RUN_DIR/run/janus.sock\"" "control socket vanished after aborted reload"
	capi_unix GET /1.0/health
	eq "$REPLY_CODE" "200"
	# A GENUINELY different config must now load. --force bypasses Caddy's
	# identical-bytes short-circuit, while changing the global ping default
	# proves that a different config actually took effect without a new listener.
	sed 's/ping # 1)/ping off # 1)/' "$ROOT/Caddyfile" >"$good_cfg"
	if ! XDG_DATA_HOME="$TEST_RUN_DIR/caddy-data" \
		reload_caddy --config "$good_cfg" --force >>"$CADDY_LOG" 2>&1; then
		echo "good reload after aborted reload failed; see $CADDY_LOG" >&2
		return 1
	fi
	local i ready=""
	for i in $(seq 1 50); do
		if curl -sS -o /dev/null --max-time 1 http://127.0.0.1:7600/1.0/health 2>/dev/null &&
			curl -sS -o /dev/null --max-time 1 --unix-socket "$TEST_RUN_DIR/run/janus.sock" http://janus/1.0/health 2>/dev/null; then
			ready=1
			break
		fi
		sleep 0.1
	done
	ok "-n \"$ready\"" "control listeners never answered after post-abort reload"
	ok "-S \"$TEST_RUN_DIR/run/janus.sock\"" "control socket vanished after post-abort reload"
	eq "$(http_code https://foo.ripdev.io/ping)" "404"
	# Restore the canonical config for the rest of the suite.
	if ! XDG_DATA_HOME="$TEST_RUN_DIR/caddy-data" \
		reload_caddy --config "$ROOT/Caddyfile" --force >>"$CADDY_LOG" 2>&1; then
		echo "restoring canonical config failed; see $CADDY_LOG" >&2
		return 1
	fi
}
