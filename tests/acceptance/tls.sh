# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
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

TLS_APP_FILE="$TEST_RUN_DIR/tls-app-id"
CADDY_LOCAL_ROOT="$TEST_RUN_DIR/caddy-data/caddy/pki/authorities/local/root.crt"

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
		openssl x509 -noout -issuer -text 2>/dev/null)"
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
