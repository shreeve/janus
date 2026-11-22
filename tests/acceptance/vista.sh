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
	# A bare TCPConnect is the non-browser (CPRS relay) class: accept, a
	# __Host-ov cookie, no OV-Session, and the token speaks as a Bearer.
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
		echo "a non-browser TCPConnect set an OV-Session header" >&2
		return 1
	fi
	token="$(vista_token)"
	eq "${#token}" "43"
	grep -q 'proxy=PROXY TCP[46] ' "$VISTA_EM_LOG"
	vista_post imhere -H "Authorization: Bearer $token"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-imhere
	printf '%s' "$token" >"$VISTA_DIR/token-a"

	# A TCPConnect with an Origin + X-OV is the browser class: it joins a
	# group, answers an OV-Session, and its tab speaks cookie + OV-Session.
	local bcookie bov
	vista_post tcpconnect -H 'X-OV: 1' -H "Origin: https://$VISTA_HOST"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-accept
	bcookie="$(vista_token)"
	bov="$(vista_hdr ov-session)"
	ok "-n \"$bov\"" "a browser TCPConnect answered no OV-Session"
	# A cookie needs X-OV (CSRF); the right Origin passes, a foreign one is refused.
	vista_post imhere -H "Cookie: __Host-ov=$bcookie" -H "OV-Session: $bov"
	eq "$REPLY_CODE" "403"
	vista_post imhere -H "Cookie: __Host-ov=$bcookie" -H "OV-Session: $bov" -H 'X-OV: 1' -H "Origin: https://$VISTA_HOST"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-imhere
	vista_post imhere -H "Cookie: __Host-ov=$bcookie" -H "OV-Session: $bov" -H 'X-OV: 1' -H 'Origin: https://evil.example'
	eq "$REPLY_CODE" "403"
	# The last tab's #BYE# clears the shared cookie and frees the connection.
	vista_post bye -H "Cookie: __Host-ov=$bcookie" -H "OV-Session: $bov" -H 'X-OV: 1' -H "Origin: https://$VISTA_HOST"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-bye
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
	# A later request naming no session: a well-formed OV-Session with no
	# cookie resolves nothing (401 unknown); with no OV-Session it is a 400.
	vista_post imhere -H 'OV-Session: AAAAAAAAAAAAAAAAAAAAAA'
	eq "$REPLY_CODE" "401"
	eq "$(vista_hdr ov-session-ended)" "unknown"
	vista_post imhere
	eq "$REPLY_CODE" "400"
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

# --- cases: vista to a stock broker over TCP ----------------------------------
#
# The same capability to a stock RPC Broker (`testkit vistaem --tcp`): no
# PROXY line (the fake logs bad-first for any byte before TCPConnect), and
# Janus's message guard and failed sign-on limit (max_failed_signons 2) in
# front of it. A refused message is answered 200 with a fixed security
# reply and never reaches the broker.

VISTA_TCP_HOST=vista-tcp.janus.localhost
VISTA_TCP_URL="https://$VISTA_TCP_HOST/rpc"
VISTA_TCP_LOG="$TEST_RUN_DIR/vista-broker.log"

vista_tcp_msgs() {
	printf '[XWB]11302\x051.108\x0dORWU USERINFO5\x04' >"$VISTA_DIR/userinfo"
	printf '[XWB]11302\x051.108\x0bXUS AV CODE50010 BAD;CODE f\x04' >"$VISTA_DIR/avbad"
	printf '[XWB]11302\x051.108\x08ZZ GUARD52002$J001vf\x04' >"$VISTA_DIR/guardlist"
	printf '[XWB]10304\x0aTCPConnect50009127.0.0.1f0001''0f0005cp^rsf\x04' >"$VISTA_DIR/tcpconnect-caret"
	printf '\x0fInvalid request\x00\x04' >"$VISTA_DIR/r-invalid"
	printf '\x14Unauthorized request\x00\x04' >"$VISTA_DIR/r-unauthorized"
	printf '\x29Too many failed sign-ons; try again later\x00\x04' >"$VISTA_DIR/r-held"
	printf '\x00\x00100^MANAGER,SITE^0^0^0^0^0^300^20^1^1^5^FOIA.DOMAIN.EXT^0^180\x04' >"$VISTA_DIR/r-userinfo"
	printf '\x00\x00100\r\n0\r\n0\r\n\r\n0\r\n0\r\n\r\nGood evening MANAGER,SITE\r\n\x04' >"$VISTA_DIR/r-avgood"
}

# vista_tcp_post MSG [curl args…] — vista_post to the tcp route.
vista_tcp_post() {
	local VISTA_URL="$VISTA_TCP_URL"
	vista_post "$@"
}

vista_tcp_session() {
	vista_tcp_post tcpconnect
	eq "$REPLY_CODE" "200" >&2
	vista_reply_is r-accept
	vista_token
}

# vista_broker_saw RPC — whether the fake broker received an RPC so named.
vista_broker_saw() {
	grep -qF "rpc=\"$1\"" "$VISTA_TCP_LOG"
}

case_vista_tcp_setup() {
	vista_tcp_msgs
	: >"$VISTA_TCP_LOG"
	"$TESTKIT" vistaem --tcp "127.0.0.1:$JANUS_TEST_VISTA_TCP_PORT" --log "$VISTA_TCP_LOG" >>"$TEST_RUN_DIR/fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$DATA_PIDS_FILE"
	local i
	for i in $(seq 1 50); do
		grep -q . "$VISTA_TCP_LOG" 2>/dev/null && break
		(exec 3<>"/dev/tcp/127.0.0.1/$JANUS_TEST_VISTA_TCP_PORT") 2>/dev/null && break
		sleep 0.1
	done
	capi GET /1.0/vista
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" "\"$VISTA_TCP_HOST/rpc\":{\"target\":\"tcp/127.0.0.1:$JANUS_TEST_VISTA_TCP_PORT\",\"network\":\"tcp\""
	json_has "$REPLY_BODY" "\"$VISTA_HOST/rpc\":{\"target\":\"unix/$JANUS_TEST_VISTA_SOCK\",\"network\":\"unix\""
	json_has "$REPLY_BODY" '"failed_signon_holds":0'
}

case_vista_tcp_session() {
	local token new
	token="$(vista_tcp_session)"
	eq "${#token}" "43"
	grep -Eq '^conn [0-9]+ tcp$' "$VISTA_TCP_LOG"
	if grep -q 'bad-first' "$VISTA_TCP_LOG"; then
		echo "a byte preceded TCPConnect at the broker" >&2
		return 1
	fi
	# Before sign-on only the sign-on RPCs run.
	vista_tcp_post userinfo -H "Authorization: Bearer $token"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-unauthorized
	if vista_broker_saw "ORWU USERINFO"; then
		echo "a refused RPC reached the broker" >&2
		return 1
	fi
	vista_tcp_post avgood -H "Authorization: Bearer $token"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-avgood
	new="$(vista_token)"
	ok "-n \"$new\" && \"$new\" != \"$token\"" "sign-on did not rotate the token"
	vista_tcp_post userinfo -H "Authorization: Bearer $new"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-userinfo
	vista_tcp_post bye -H "Authorization: Bearer $new"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-bye
	vista_tcp_post imhere -H "Authorization: Bearer $new"
	eq "$REPLY_CODE" "401"
	eq "$(vista_hdr ov-session-ended)" "bye"
}

case_vista_tcp_guard() {
	local token
	token="$(vista_tcp_session)"
	# A list subscript that is not literals: refused at admission, never sent.
	vista_tcp_post guardlist -H "Authorization: Bearer $token"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-invalid
	if vista_broker_saw "ZZ GUARD"; then
		echo "a refused message reached the broker" >&2
		return 1
	fi
	# The session goes on.
	vista_tcp_post imhere -H "Authorization: Bearer $token"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-imhere
	# TCPConnect's shape is judged too: a ^ in its name.
	vista_tcp_post tcpconnect-caret
	eq "$REPLY_CODE" "200"
	vista_reply_is r-invalid
	capi GET /1.0/vista
	json_has "$REPLY_BODY" '"list_subscript":1'
	json_has "$REPLY_BODY" '"command_params":1'
	json_has "$REPLY_BODY" '"signon_rpc":1'
}

case_vista_tcp_failed_signons() {
	# max_failed_signons 2: the third XUS AV CODE from this client is held,
	# even with the right code, and never reaches the broker.
	local token before
	token="$(vista_tcp_session)"
	vista_tcp_post avbad -H "Authorization: Bearer $token"
	eq "$REPLY_CODE" "200"
	vista_tcp_post avbad -H "Authorization: Bearer $token"
	eq "$REPLY_CODE" "200"
	before="$(grep -c 'rpc="XUS AV CODE"' "$VISTA_TCP_LOG")"
	vista_tcp_post avgood -H "Authorization: Bearer $token"
	eq "$REPLY_CODE" "200"
	vista_reply_is r-held
	eq "$(grep -c 'rpc="XUS AV CODE"' "$VISTA_TCP_LOG")" "$before"
	capi GET /1.0/vista
	json_has "$REPLY_BODY" '"failed_signons":2'
	json_has "$REPLY_BODY" '"failed_signon_holds":1'
}
