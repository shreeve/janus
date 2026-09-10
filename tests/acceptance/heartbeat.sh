# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: heartbeat --------------------------------------------------------
#
# The group restarts caddy with JANUS_HEARTBEAT_TTL=2s (sweep every ~666ms)
# so TTL expiry is observable in seconds. Recovery from expiry is
# RE-REGISTRATION: the reap has the same effect as DELETE, so the tenant sees
# heartbeat → 404, re-registers, and re-PUTs its upstreams.

HB_APP_FILE="$TEST_RUN_DIR/hb-app-id"

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
	capi PUT "/1.0/apps/$id/upstreams" "{\"upstreams\":[{\"path\":\"$TEST_RUN_DIR/run/hb.sock\"}]}"
	eq "$REPLY_CODE" "200"
}

case_hb_register_traffic() {
	start_data_upstream "$TEST_RUN_DIR/run/hb.sock" hb "$TEST_RUN_DIR/hb.hits" || return 1
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
