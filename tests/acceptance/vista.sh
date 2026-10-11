# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: vista -------------------------------------------------------------
#
# Capability 11: VistA's RPC Broker behind POST /rpc. The testkit's fake em
# (`testkit vistaem`) answers on a Unix socket the way em does behind its
# service manager: the PROXY line first, then one reply per message, with
# RPC-HTTPS §3's measured replies. The route sits under the global auth
# gate / with `auth exempt`. Bodies and replies are binary (NUL bytes), so
# every request and reply travels through files and is compared with cmp.

VISTA_HOST=vista.janus.localhost
VISTA_URL="https://$VISTA_HOST/rpc"
VISTA_DIR="$TEST_RUN_DIR/vista"
VISTA_EM_LOG="$TEST_RUN_DIR/vista-em.log"

# The broker messages and replies of RPC-HTTPS §3, as the fake em speaks them.
vista_msgs() {
	mkdir -p "$VISTA_DIR"
	printf '[XWB]10304\x0aTCPConnect50009127.0.0.1f0001''0f0004cprsf\x04' >"$VISTA_DIR/tcpconnect"
	printf '[XWB]11302\x051.108\x0bXWB IM HERE5\x04' >"$VISTA_DIR/imhere"
	printf '[XWB]11302\x051.108\x0bXUS AV CODE50011 GOOD;CODE f\x04' >"$VISTA_DIR/avgood"
	printf '[XWB]11302\x051.108\x04MERR5\x04' >"$VISTA_DIR/merr"
	printf '[XWB]11302\x051.108\x0bCLOSE-AFTER50003300f\x04' >"$VISTA_DIR/closeafter"
	printf '[XWB]10304\x05#BYE#\x04' >"$VISTA_DIR/bye"
	printf 'GET /rpc HTTP/1.1\r\n' >"$VISTA_DIR/notxwb"
	printf '[XWB]11312\x051.108\x0bXWB IM HERE5\x04' >"$VISTA_DIR/noreply"
	printf '\x00\x00accept\x04' >"$VISTA_DIR/r-accept"
	printf '\x00\x001\x04' >"$VISTA_DIR/r-imhere"
	printf '\x00\x00#BYE#\x04' >"$VISTA_DIR/r-bye"
	printf '\x00\x00\x04' >"$VISTA_DIR/r-empty"
	printf '\x00\x00\x18M  ERROR=CAPI+16^XWBPRS,4 actual parameters for IMHERE^XWBLIB,M58,-M58\r\nLAST REF=^XTV(8989.3,1,"XWB")\x04\x04' >"$VISTA_DIR/r-merr"
}

# vista_post MSG [curl args…] — POST a message file; sets REPLY_CODE and
# leaves the reply in $VISTA_DIR/reply and the headers in $VISTA_DIR/headers.
vista_post() {
	local msg=$1
	shift
	REPLY_CODE="$(curl -sS -o "$VISTA_DIR/reply" -D "$VISTA_DIR/headers" -w '%{http_code}' --max-time 10 \
		-H 'Content-Type: application/x-xwb' --data-binary @"$VISTA_DIR/$msg" "$@" "$VISTA_URL")"
}

# vista_hdr NAME — a response header's value from the last vista_post.
vista_hdr() {
	tr -d '\r' <"$VISTA_DIR/headers" | grep -i "^$1:" | head -1 | sed 's/^[^:]*: *//'
}

# vista_token — the token in the last response's __Host-ov Set-Cookie.
vista_token() {
	tr -d '\r' <"$VISTA_DIR/headers" | grep -i '^set-cookie: __Host-ov=' | head -1 | sed 's/^[^=]*=\([^;]*\);.*/\1/'
}

vista_reply_is() {
	if ! cmp -s "$VISTA_DIR/reply" "$VISTA_DIR/$1"; then
		echo "reply is not $1: $(od -c "$VISTA_DIR/reply" | head -3)" >&2
		return 1
	fi
}

# vista_session — start a session; prints its token.
vista_session() {
	vista_post tcpconnect
	eq "$REPLY_CODE" "200" >&2
	vista_reply_is r-accept
	vista_token
}

case_vista_setup() {
	vista_msgs
	: >"$VISTA_EM_LOG"
	"$TESTKIT" vistaem --sock "$JANUS_TEST_VISTA_SOCK" --log "$VISTA_EM_LOG" >>"$TEST_RUN_DIR/fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$DATA_PIDS_FILE"
	wait_file "$JANUS_TEST_VISTA_SOCK"
	capi GET /1.0/vista
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" "\"$VISTA_HOST/rpc\":{\"target\":\"unix/$JANUS_TEST_VISTA_SOCK\""
}

case_vista_session_round_trip() {
	vista_post tcpconnect
	eq "$REPLY_CODE" "200"
	vista_reply_is r-accept
	eq "$(vista_hdr content-type)" "application/x-xwb"
	local cookie token
	cookie="$(vista_hdr set-cookie)"
	case "$cookie" in
	"__Host-ov="*"; Path=/; Secure; HttpOnly; SameSite=Strict") ;;
	*)
		echo "TCPConnect's Set-Cookie: $cookie" >&2
		return 1
		;;
	esac
	if tr -d '\r' <"$VISTA_DIR/headers" | grep -qi '^ov-session:'; then
		echo "the token rode in a header other than Set-Cookie" >&2
		return 1
	fi
	token="$(vista_token)"
	eq "${#token}" "43"
	grep -q 'proxy=PROXY TCP[46] ' "$VISTA_EM_LOG"
	# Bearer (the CPRS relay) needs no X-OV; a cookie needs it.
	vista_post imhere -H "Authorization: Bearer $token"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-imhere
	vista_post imhere -H "Cookie: __Host-ov=$token"
	eq "$REPLY_CODE" "403"
	vista_post imhere -H "Cookie: __Host-ov=$token" -H 'X-OV: 1' -H "Origin: https://$VISTA_HOST"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-imhere
	vista_post imhere -H "Cookie: __Host-ov=$token" -H 'X-OV: 1' -H 'Origin: https://evil.example'
	eq "$REPLY_CODE" "403"
	printf '%s' "$token" >"$VISTA_DIR/token-a"
}

case_vista_signon_rotates() {
	local old new
	old="$(cat "$VISTA_DIR/token-a")"
	vista_post avgood -H "Authorization: Bearer $old"
	eq "$REPLY_CODE" "200"
	new="$(vista_token)"
	ok "-n \"$new\" && \"$new\" != \"$old\"" "sign-on did not rotate the token"
	vista_post imhere -H "Authorization: Bearer $old"
	eq "$REPLY_CODE" "401"
	eq "$(vista_hdr ov-session-ended)" "rotated"
	vista_post imhere -H "Authorization: Bearer $new"
	eq "$REPLY_CODE" "200"
	printf '%s' "$new" >"$VISTA_DIR/token-a"
	capi GET /1.0/vista
	json_has "$REPLY_BODY" '"signed_on":1'
}

case_vista_m_error_whole() {
	vista_post merr -H "Authorization: Bearer $(cat "$VISTA_DIR/token-a")"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-merr
	# The session goes on after an M error.
	vista_post imhere -H "Authorization: Bearer $(cat "$VISTA_DIR/token-a")"
	eq "$REPLY_CODE" "200"
}

case_vista_refusals() {
	vista_post notxwb
	eq "$REPLY_CODE" "400"
	vista_post noreply
	eq "$REPLY_CODE" "400"
	vista_post imhere
	eq "$REPLY_CODE" "401"
	eq "$(vista_hdr ov-session-ended)" "unknown"
	REPLY_CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$VISTA_URL")"
	eq "$REPLY_CODE" "405"
	REPLY_CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -H 'Content-Type: text/plain' \
		--data-binary @"$VISTA_DIR/imhere" "$VISTA_URL")"
	eq "$REPLY_CODE" "415"
	# Other spellings of the path are not the route: the site's gate answers.
	local p
	for p in /RPC /rpc/ /%72pc; do
		REPLY_CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -X POST "https://$VISTA_HOST$p")"
		eq "$REPLY_CODE" "401"
	done
}

case_vista_bye_and_idle_close() {
	local token
	vista_post bye -H "Authorization: Bearer $(cat "$VISTA_DIR/token-a")"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-bye
	vista_post imhere -H "Authorization: Bearer $(cat "$VISTA_DIR/token-a")"
	eq "$REPLY_CODE" "401"
	eq "$(vista_hdr ov-session-ended)" "bye"
	# em ending the stream while idle (its broker timeout) is seen at once.
	token="$(vista_session)"
	vista_post closeafter -H "Authorization: Bearer $token"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-empty
	sleep 1
	vista_post imhere -H "Authorization: Bearer $token"
	eq "$REPLY_CODE" "401"
	eq "$(vista_hdr ov-session-ended)" "closed"
}

case_vista_unsigned_cap_and_deadline() {
	# max_unsigned 4: a fifth unsigned session is refused until the 10 s
	# sign-on deadline ends the others.
	local i
	for i in 1 2 3 4; do
		vista_session >/dev/null
	done
	vista_post tcpconnect
	eq "$REPLY_CODE" "503"
	ok "-n \"$(vista_hdr retry-after)\"" "503 without Retry-After"
	sleep 11
	vista_post tcpconnect
	eq "$REPLY_CODE" "200"
	capi GET /1.0/vista
	json_has "$REPLY_BODY" '"signon_timeout":4'
}

case_vista_no_token_in_access_log() {
	local t
	for t in $(tr -d '\r' <"$VISTA_DIR/headers" | grep -i '^set-cookie: __Host-ov=' | sed 's/^[^=]*=\([^;]*\);.*/\1/') $(cat "$VISTA_DIR/token-a"); do
		[[ -n "$t" ]] || continue
		if grep -qF "$t" "$JANUS_TEST_ACCESS_LOG"; then
			echo "a session token is in the access log" >&2
			return 1
		fi
	done
	grep -Eq '"(Cookie|Authorization)":\["REDACTED"\]' "$JANUS_TEST_ACCESS_LOG"
}
