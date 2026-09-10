# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: browse ----------------------------------------------------------

BROWSE_ROOT="$TEST_RUN_DIR/browse/root"
BROWSE_APP_FILE="$TEST_RUN_DIR/browse/app-id"

case_browse_setup_and_listing() {
	mkdir -p "$BROWSE_ROOT/sub" "$BROWSE_COLD_ROOT"
	printf '# rendered\n' >"$BROWSE_ROOT/page.md"
	printf 'audio' >"$BROWSE_ROOT/audio.mp3"
	printf 'image' >"$BROWSE_ROOT/image.png"
	printf 'hold' >"$BROWSE_ROOT/wait.hold"
	printf 'slow' >"$BROWSE_ROOT/wait.slow"
	printf 'fail' >"$BROWSE_ROOT/wait.fail"
	printf '<h1>index</h1>' >"$BROWSE_ROOT/sub/index.html"
	printf 'cold-root' >"$BROWSE_COLD_ROOT/cold.txt"

	capi POST /1.0/apps \
		"{\"name\":\"browse\",\"hosts\":[\"browseone.ripdev.io\",\"browsetwo.ripdev.io\"],\"lease\":\"process\",\"files\":{\"roots\":[{\"path\":\"$BROWSE_ROOT\",\"browse\":true}]}}"
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	printf '%s' "$id" >"$BROWSE_APP_FILE"

	local body
	body="$(http_body https://browseone.ripdev.io/)"
	json_has "$body" 'page.md'
	json_has "$body" 'image.png'
	json_has "$body" '/_janus/browse/'
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 https://browseone.ripdev.io/sub)" "308"
	eq "$(http_body https://browseone.ripdev.io/sub/)" '<h1>index</h1>'
}

case_browse_raw_and_renderer() {
	eq "$(http_body 'https://browseone.ripdev.io/page.md?raw')" '# rendered'
	eq "$(http_body https://browseone.ripdev.io/page.md)" '# rendered'
	eq "$(http_body https://browsetwo.ripdev.io/page.md)" '# rendered'
	local headers
	headers="$(curl -sS -I --max-time 5 'https://browseone.ripdev.io/audio.mp3?raw' | tr -d '\r')"
	json_has "$headers" 'content-type: audio/mpeg'
	json_has "$headers" 'cache-control: no-cache'
}

case_browse_renderer_bounds() {
	curl -sS --max-time 5 https://browseone.ripdev.io/wait.hold \
		>"$TEST_RUN_DIR/browse/hold.out" &
	local pid=$!
	sleep 0.2
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 \
		https://browsetwo.ripdev.io/wait.hold)" "503"
	wait "$pid"
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 \
		https://browseone.ripdev.io/wait.slow)" "504"
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 \
		https://browseone.ripdev.io/wait.fail)" "502"
}

case_browse_cascade_lease_and_status() {
	capi POST /1.0/apps \
		"{\"name\":\"browseoff\",\"hosts\":[\"browseoff.ripdev.io\"],\"lease\":\"process\",\"files\":{\"roots\":[{\"path\":\"$BROWSE_ROOT\",\"browse\":true}]}}"
	eq "$REPLY_CODE" "201"
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 https://browseoff.ripdev.io/)" "404"

	capi POST /1.0/apps \
		"{\"name\":\"browsereap\",\"hosts\":[\"browsereap.ripdev.io\"],\"files\":{\"roots\":[{\"path\":\"$BROWSE_ROOT\",\"browse\":true}]}}"
	eq "$REPLY_CODE" "201"
	local reap_id
	reap_id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	local i
	for i in $(seq 1 80); do
		capi GET "/1.0/apps/$reap_id"
		[[ "$REPLY_CODE" == "404" ]] && break
		sleep 0.25
	done
	eq "$REPLY_CODE" "404"

	local id
	id="$(cat "$BROWSE_APP_FILE")"
	if ! XDG_DATA_HOME="$TEST_RUN_DIR/caddy-data" \
		reload_caddy --config "$ROOT/Caddyfile" --force >>"$CADDY_LOG" 2>&1; then
		echo "caddy reload failed; see $CADDY_LOG" >&2
		return 1
	fi
	capi GET "/1.0/apps/$id"
	eq "$REPLY_CODE" "200"
	capi GET /1.0/browse
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"enabled":true'
	[[ "$REPLY_BODY" != *"$BROWSE_ROOT"* ]]
	capi DELETE "/1.0/apps/$id"
	eq "$REPLY_CODE" "204"
}

case_browse_cold_restart_and_conflict() {
	[[ -f "$BROWSE_COLD_ROOT/cold.txt" ]] || {
		echo "cold browse fixture disappeared: $BROWSE_COLD_ROOT/cold.txt" >&2
		return 1
	}
	eq "$(http_body https://coldbrowse.ripdev.io/cold.txt)" "cold-root"
	capi GET '/1.0/tls/ask?domain=coldbrowse.ripdev.io'
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"claim":"cold"'
	capi POST /1.0/apps '{"name":"conflict","hosts":["coldbrowse.ripdev.io"]}'
	eq "$REPLY_CODE" "409"
	json_has "$REPLY_BODY" 'cold:coldbrowse.ripdev.io'
	stop_caddy
	start_caddy || return 1
	[[ -f "$BROWSE_COLD_ROOT/cold.txt" ]] || {
		echo "cold browse fixture disappeared across restart: $BROWSE_COLD_ROOT/cold.txt" >&2
		return 1
	}
	eq "$(http_body https://coldbrowse.ripdev.io/cold.txt)" "cold-root"
	capi GET /1.0/apps
	[[ "$REPLY_BODY" != *'coldbrowse.ripdev.io'* ]]
}
