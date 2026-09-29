# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: webtransport ----------------------------------------------------

WT_ECHO_PORT=7690
WT_HOST=wt.ripdev.io
WT_URL="https://$WT_HOST/relay"

case_webtransport_setup() {
	"$TESTKIT" udp-echo --listen "127.0.0.1:$WT_ECHO_PORT" >>"$TEST_RUN_DIR/fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$DATA_PIDS_FILE"
	local i
	for i in $(seq 1 50); do
		grep -q "addr=127.0.0.1:$WT_ECHO_PORT" "$TEST_RUN_DIR/fixtures.log" 2>/dev/null && break
		sleep 0.1
	done
	grep -q "addr=127.0.0.1:$WT_ECHO_PORT" "$TEST_RUN_DIR/fixtures.log"
	capi GET /1.0/webtransport
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"enabled":true'
	json_has "$REPLY_BODY" "\"$WT_HOST/relay\":{\"target\":\"udp/127.0.0.1:$WT_ECHO_PORT\""
	json_has "$REPLY_BODY" '"listen":["[::]:443"]'
}

case_webtransport_descriptor() {
	local headers body
	headers="$TEST_RUN_DIR/wt-headers"
	body="$(curl -sS -D "$headers" --max-time 5 "$WT_URL")"
	eq "$body" "{\"url\":\"$WT_URL\",\"max_datagram\":1200,\"certificate_hashes\":[]}"
	tr -d '\r' <"$headers" | grep -qi '^cache-control: no-store'
	tr -d '\r' <"$headers" | grep -qi '^content-type: application/json'
	eq "$(curl -sS -o /dev/null -w '%{http_code}' -I --max-time 5 "$WT_URL")" "200"
	eq "$(curl -sS -o /dev/null -w '%{http_code}' -X POST --max-time 5 "$WT_URL")" "405"
}

case_webtransport_echo() {
	local report
	report="$("$TESTKIT" wt --url "$WT_URL" --origin "https://$WT_HOST" --n 200 --size 1152)"
	json_has "$report" '"status":200'
	json_has "$report" '"sent":200,"received":200,"mismatched":0,"bytes":230400'
	report="$("$TESTKIT" wt --url "$WT_URL" --origin "https://$WT_HOST" --n 20 --size 1200)"
	json_has "$report" '"sent":20,"received":20,"mismatched":0'
	capi GET /1.0/webtransport
	json_has "$REPLY_BODY" '"accepted":2'
	json_has "$REPLY_BODY" '"datagrams_in":220,"datagrams_out":220,"bytes_in":254400,"bytes_out":254400'
	json_has "$REPLY_BODY" '"dropped_queue":0,"dropped_oversize_in":0,"dropped_oversize_out":0'
}

case_webtransport_refusals() {
	local report
	report="$("$TESTKIT" wt --url "$WT_URL" --n 1 || true)"
	json_has "$report" '"status":403'
	report="$("$TESTKIT" wt --url "$WT_URL" --origin "https://evil.ripdev.io" --n 1 || true)"
	json_has "$report" '"status":403'
	report="$("$TESTKIT" wt --url "https://$WT_HOST/nope" --origin "https://$WT_HOST" --n 1 || true)"
	json_has "$report" '"status":404'
	capi GET /1.0/webtransport
	json_has "$REPLY_BODY" '"refused_origin":2'
	# A known host with an unknown path is a path miss, not a host miss.
	json_has "$REPLY_BODY" '"refused_path":1'
}

case_webtransport_ticket() {
	local url="https://wtgate.ripdev.io" report ticketurl
	# Anonymous: the auth wall answers, not the descriptor.
	auth_req GET "$url/relay"
	ok "\"$REPLY_CODE\" == 401 || \"$REPLY_CODE\" == 302" "anonymous descriptor answered $REPLY_CODE"
	auth_login "$url" alice sesame-alice
	auth_req GET "$url/relay" -H "Cookie: __Host-janus=$AUTH_SESSION"
	eq "$REPLY_CODE" "200"
	ticketurl="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')"
	case "$ticketurl" in
	"$url/relay?t="*) ;;
	*)
		echo "no ticket in the gated descriptor: $REPLY_BODY"
		return 1
		;;
	esac
	# Without the ticket the relay refuses; with it, it admits; replayed, it refuses.
	report="$("$TESTKIT" wt --url "$url/relay" --origin "$url" --n 1 || true)"
	json_has "$report" '"status":403'
	report="$("$TESTKIT" wt --url "$ticketurl" --origin "$url" --n 50 --size 1152)"
	json_has "$report" '"status":200'
	json_has "$report" '"sent":50,"received":50,"mismatched":0'
	report="$("$TESTKIT" wt --url "$ticketurl" --origin "$url" --n 1 || true)"
	json_has "$report" '"status":403'
	capi GET /1.0/webtransport
	json_has "$REPLY_BODY" '"wtgate.ripdev.io/relay":{"target":"udp/127.0.0.1:7690","origin":["same"],'
	json_has "$REPLY_BODY" '"gated":true,"accepted":1'
	json_has "$REPLY_BODY" '"refused_ticket":2'
}
