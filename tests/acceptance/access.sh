# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- capability 9: access log --------------------------------------------

ACCESS_APP_FILE="$TEST_RUN_DIR/access-app-id"
ACCESS_STREAM_FILE="$TEST_RUN_DIR/access-stream.ndjson"

case_access_setup_and_status() {
	rm -f "$ACCESS_STREAM_FILE"
	capi POST /1.0/apps \
		"{\"name\":\"access\",\"hosts\":[\"access.ripdev.io\"],\"lease\":\"process\",\"files\":{\"roots\":[{\"path\":\"$BROWSE_ROOT\",\"browse\":true}]}}"
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	ok "-n \"$id\"" "no id in $REPLY_BODY"
	printf '%s' "$id" >"$ACCESS_APP_FILE"
	capi GET /1.0/access
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"line_bytes":8192'
	json_has "$REPLY_BODY" '"heartbeat_seconds":15'
	json_has "$REPLY_BODY" '"provisioned_encoders":'
}

case_access_stream_and_durable_json() {
	local id pid i
	id="$(cat "$ACCESS_APP_FILE")"
	curl -sSN --max-time 5 \
		"http://127.0.0.1:7600/1.0/apps/$id/access?after=0" \
		>"$ACCESS_STREAM_FILE" &
	pid=$!
	for i in $(seq 1 50); do
		[[ -s "$ACCESS_STREAM_FILE" ]] && break
		sleep 0.05
	done
	json_has "$(cat "$ACCESS_STREAM_FILE")" '"type":"hello"'
	eq "$(http_body 'https://access.ripdev.io/page.md?raw')" '# rendered'
	for i in $(seq 1 50); do
		if [[ "$(cat "$ACCESS_STREAM_FILE")" == *'"type":"access"'* ]]; then
			break
		fi
		sleep 0.05
	done
	kill "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
	local stream
	stream="$(cat "$ACCESS_STREAM_FILE")"
	json_has "$stream" '"sequence":"1"'
	json_has "$stream" '"request_host":"access.ripdev.io"'
	json_has "$stream" '"response_class":"file"'
	json_has "$(cat "$TEST_RUN_DIR/access.log")" '"request":{"remote_ip":'
}

case_access_protocol_rejections() {
	local id
	id="$(cat "$ACCESS_APP_FILE")"
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 \
		"http://127.0.0.1:7600/1.0/apps/$id/access?after=01")" "400"
	eq "$(curl -sS -I -o /dev/null -w '%{http_code}' --max-time 5 \
		"http://127.0.0.1:7600/1.0/apps/$id/access?after=0")" "405"
	eq "$(curl -sS -X GET -o /dev/null -w '%{http_code}' --data-binary x --max-time 5 \
		"http://127.0.0.1:7600/1.0/apps/$id/access?after=0")" "400"
}
