# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: auth ---------------------------------------------------------------
#
# Capability 6: URL-prefix gates for auth-less apps
# (docs/20260728-160734-capability-auth.md). The instruments: the authup
# fixture upstream records every request's Remote-User and Cookie
# (tenant-side truth) and answers hub bridge POSTs 204; /1.0/auth is the
# Janus-side truth. Root-Caddyfile sites: authwall.ripdev.io inherits
# global users + gates (`gate /` + `gate /one/`); authcarol.ripdev.io
# replaces with carol-only `gate /`; every other site carries `auth off`.
# Passwords are the fixture ones beside the blobs. The group runs under
# the heartbeat caddy (TTL 2s).

AUTH_WALL="https://authwall.ripdev.io"
AUTH_CAROL="https://authcarol.ripdev.io"
AUTH_SOCK="$TEST_RUN_DIR/run/auth-up.sock"
AUTH_HITS="$TEST_RUN_DIR/auth-hits"
AUTH_PIDS_FILE="$TEST_RUN_DIR/auth-pids"
AUTH_APP_FILE="$TEST_RUN_DIR/auth-app-id"
AUTH_ALICE_COOKIE="$TEST_RUN_DIR/auth-alice-cookie"

stop_auth_fixtures() {
	if [[ -f "$AUTH_PIDS_FILE" ]]; then
		while read -r pid; do
			if pid_is_owned "$pid" "$TESTKIT"; then
				stop_owned_pid "$pid" "$TESTKIT"
			else
				stop_owned_pid "$pid" "$CADDY_BIN"
			fi
		done <"$AUTH_PIDS_FILE"
	fi
	rm -f "$AUTH_PIDS_FILE" "$AUTH_SOCK"
}

# auth_stat KEY — one top-level value from GET /1.0/auth
auth_stat() {
	capi GET /1.0/auth
	printf '%s' "$REPLY_BODY" | "$TESTKIT" json get "$1"
}

auth_hits_count() {
	if [[ ! -f "$AUTH_HITS" ]]; then
		printf '0'
		return
	fi
	wc -l <"$AUTH_HITS" | tr -d '[:space:]'
}

# auth_req METHOD URL [curl args…] — sets REPLY_CODE / REPLY_HDRS /
# REPLY_BODY. Redirects are never followed; HTTP/2 lowercases header
# names, so header reads below go through the case-insensitive helpers.
auth_req() {
	local method=$1 url=$2
	shift 2
	local hdrfile="$TEST_RUN_DIR/auth-hdrs"
	local out
	out="$(curl -sS --max-time 5 -X "$method" -D "$hdrfile" -w $'\n%{http_code}' "$@" "$url")"
	REPLY_CODE="${out##*$'\n'}"
	REPLY_BODY="${out%$'\n'*}"
	REPLY_HDRS="$(tr -d '\r' <"$hdrfile")"
}

# auth_hdr NAME — first value of a response header, case-insensitive.
# An absent header prints nothing and succeeds: the suite runs pipefail
# and case bodies run set -e, so a bare grep miss inside a command
# substitution would abort the case without a message.
auth_hdr() {
	printf '%s\n' "$REPLY_HDRS" | { grep -i "^$1: " || true; } | head -1 | sed 's/^[^:]*: //'
}

# auth_setcookie NAME — the value a Set-Cookie response header assigns
# (empty when the response set none; same pipefail discipline as above —
# failed logins legitimately set no cookie).
auth_setcookie() {
	printf '%s\n' "$REPLY_HDRS" | { grep -i '^set-cookie: ' || true; } |
		sed -n "s/^[Ss]et-[Cc]ookie: $1=\([^;]*\).*/\1/p" | tail -1
}

auth_csrf_from_body() {
	printf '%s' "$REPLY_BODY" | sed -n 's/.*name="csrf" value="\([^"]*\)".*/\1/p' | head -1
}

# auth_attempt URL USER PASS [TO] — the full GET-form → POST
# flow; leaves the POST's REPLY_* in place and AUTH_SESSION set to the
# minted session cookie (empty on failure).
auth_attempt() {
	local url=$1 user=$2 pass=$3 to=${4:-}
	auth_req GET "$url/auth"
	eq "$REPLY_CODE" "200"
	local csrf
	csrf="$(auth_csrf_from_body)"
	ok "-n \"$csrf\"" "no csrf field in the login form"
	local -a data=(--data-urlencode "csrf=$csrf"
		--data-urlencode "user=$user" --data-urlencode "password=$pass")
	if [[ -n "$to" ]]; then
		data+=(--data-urlencode "to=$to")
	fi
	auth_req POST "$url/auth" -H "Cookie: __Host-janus_csrf=$csrf" "${data[@]}"
	AUTH_SESSION="$(auth_setcookie __Host-janus)"
}

# auth_login URL USER PASS — auth_attempt that must succeed (303 + cookie)
auth_login() {
	auth_attempt "$@"
	eq "$REPLY_CODE" "303"
	ok "-n \"$AUTH_SESSION\"" "successful login minted no session cookie"
}

case_auth_setup() {
	: >"$AUTH_HITS"
	: >"$AUTH_PIDS_FILE"
	capi POST /1.0/apps '{"name":"authapp","hosts":["authwall.ripdev.io","authcarol.ripdev.io"],"bridge":"/rt/bridge"}'
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	ok "-n \"$id\"" "no id in $REPLY_BODY"
	printf '%s' "$id" >"$AUTH_APP_FILE"
	rm -f "$AUTH_SOCK"
	"$TESTKIT" authup --sock "$AUTH_SOCK" --hits "$AUTH_HITS" "$id" \
		>>"$TEST_RUN_DIR/fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$AUTH_PIDS_FILE"
	wait_file "$AUTH_SOCK"
	capi PUT "/1.0/apps/$id/upstreams" "{\"upstreams\":[{\"path\":\"$AUTH_SOCK\"}]}"
	eq "$REPLY_CODE" "200"
	# The Janus-side oracles answer: the GET /1.0 boolean and the wall view.
	capi GET /1.0
	json_has "$REPLY_BODY" '"auth":true'
	capi GET /1.0/auth
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"enabled":true'
	json_has "$REPLY_BODY" '"authwall.ripdev.io"'
	json_has "$REPLY_BODY" '"authcarol.ripdev.io"'
	json_has "$REPLY_BODY" '"sessions"'
	json_has "$REPLY_BODY" '"logins"'
}

case_auth_wall_blocks() {
	local h0
	h0="$(auth_hits_count)"
	# Browser-shaped → 302 to the login form, to carried, no-store.
	auth_req GET "$AUTH_WALL/" -H 'Accept: text/html'
	eq "$REPLY_CODE" "302"
	eq "$(auth_hdr location)" "/auth?to=%2F"
	eq "$(auth_hdr cache-control)" "no-store"
	# Everything else → 401 with no WWW-Authenticate (cookies, not Bearer).
	auth_req GET "$AUTH_WALL/api"
	eq "$REPLY_CODE" "401"
	if printf '%s' "$REPLY_HDRS" | grep -qi '^www-authenticate:'; then
		echo "the wall's 401 must not carry WWW-Authenticate" >&2
		return 1
	fi
	auth_req POST "$AUTH_WALL/submit" --data 'x=1'
	eq "$REPLY_CODE" "401"
	# The upstream never saw a byte.
	eq "$(auth_hits_count)" "$h0"
}

case_auth_login_roundtrip() {
	auth_login "$AUTH_WALL" alice sesame-alice
	eq "$(auth_hdr location)" "/"
	eq "$(auth_hdr cache-control)" "no-store"
	printf '%s' "$AUTH_SESSION" >"$AUTH_ALICE_COOKIE"
	auth_req GET "$AUTH_WALL/" -H "Cookie: __Host-janus=$AUTH_SESSION"
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" 'remote-user:alice'
	# The upstream saw the injected identity and never the wall's cookie.
	local rec
	rec="$(grep '^GET / ' "$AUTH_HITS" | tail -1)"
	json_has "$rec" 'remote-user=alice'
	if printf '%s' "$rec" | grep -qF '__Host-janus'; then
		echo "session cookie rode through to the upstream: $rec" >&2
		return 1
	fi
	ok "$(auth_stat logins) -ge 1" "logins counter unmoved"
	ok "$(auth_stat sessions) -ge 1" "sessions gauge empty"
}

case_auth_to() {
	# The 302's to survives the round trip…
	auth_req GET "$AUTH_WALL/auth?to=%2Freports%3Fq%3D1"
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" 'name="to" value="/reports?q=1"'
	auth_login "$AUTH_WALL" alice sesame-alice "/reports?q=1"
	eq "$(auth_hdr location)" "/reports?q=1"
	# …and hostile targets collapse to /.
	local target
	for target in '//evil.example' '/\evil' 'https://evil.example'; do
		auth_login "$AUTH_WALL" alice sesame-alice "$target"
		eq "$(auth_hdr location)" "/"
	done
}

case_auth_status_signout() {
	local s0
	s0="$(auth_stat signouts)"
	auth_login "$AUTH_WALL" alice sesame-alice
	local session="$AUTH_SESSION"
	# Signed in, GET /auth is the status page — with a re-minted CSRF
	# pair (login cleared the login form's cookie).
	auth_req GET "$AUTH_WALL/auth" -H "Cookie: __Host-janus=$session"
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" 'Signed in as'
	json_has "$REPLY_BODY" 'alice'
	local csrf
	csrf="$(auth_csrf_from_body)"
	ok "-n \"$csrf\"" "status page carries no csrf field"
	eq "$(auth_setcookie __Host-janus_csrf)" "$csrf"
	# Sign-out: POST with neither user nor password → revoked, cleared, 303.
	auth_req POST "$AUTH_WALL/auth" \
		-H "Cookie: __Host-janus=$session; __Host-janus_csrf=$csrf" \
		--data-urlencode "csrf=$csrf"
	eq "$REPLY_CODE" "303"
	eq "$(auth_hdr location)" "/auth"
	# The revoked cookie is dead server-side: the next GET bounces.
	auth_req GET "$AUTH_WALL/" -H "Cookie: __Host-janus=$session" -H 'Accept: text/html'
	eq "$REPLY_CODE" "302"
	ok "$(auth_stat signouts) -gt $s0" "signouts counter unmoved"
}

case_auth_csrf_enforced() {
	# No CSRF pair at all → 403, nothing minted.
	auth_req POST "$AUTH_WALL/auth" \
		--data-urlencode "user=alice" --data-urlencode "password=sesame-alice"
	eq "$REPLY_CODE" "403"
	eq "$(auth_setcookie __Host-janus)" ""
	# Cookie/field mismatch → 403.
	auth_req GET "$AUTH_WALL/auth"
	local csrf other
	csrf="$(auth_csrf_from_body)"
	auth_req GET "$AUTH_WALL/auth"
	other="$(auth_csrf_from_body)"
	auth_req POST "$AUTH_WALL/auth" -H "Cookie: __Host-janus_csrf=$other" \
		--data-urlencode "csrf=$csrf" \
		--data-urlencode "user=alice" --data-urlencode "password=sesame-alice"
	eq "$REPLY_CODE" "403"
	# Sign-out arm enforces it too.
	auth_req POST "$AUTH_WALL/auth" --data-urlencode "x=y"
	eq "$REPLY_CODE" "403"
}

case_auth_spoof_dies() {
	# Spoofed identity without a session: the wall answers, the app never sees it.
	auth_req GET "$AUTH_WALL/spoof" -H 'Remote-User: root'
	eq "$REPLY_CODE" "401"
	# With a session: the client-supplied header dies, the wall's injection wins.
	auth_login "$AUTH_WALL" alice sesame-alice
	auth_req GET "$AUTH_WALL/spoof" -H 'Remote-User: root' \
		-H "Cookie: __Host-janus=$AUTH_SESSION"
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" 'remote-user:alice'
	local rec
	rec="$(grep '^GET /spoof ' "$AUTH_HITS" | tail -1)"
	json_has "$rec" 'remote-user=alice'
	if printf '%s' "$rec" | grep -qF 'root'; then
		echo "spoofed Remote-User reached the upstream: $rec" >&2
		return 1
	fi
}

case_auth_fixation_dead() {
	# A client-proposed token is never honored…
	auth_req GET "$AUTH_WALL/" -H 'Cookie: __Host-janus=attacker-chosen-token' -H 'Accept: text/html'
	eq "$REPLY_CODE" "302"
	# …and login always mints fresh (the pre-set value never comes back).
	auth_req GET "$AUTH_WALL/auth" -H 'Cookie: __Host-janus=attacker-chosen-token'
	local csrf
	csrf="$(auth_csrf_from_body)"
	auth_req POST "$AUTH_WALL/auth" \
		-H "Cookie: __Host-janus=attacker-chosen-token; __Host-janus_csrf=$csrf" \
		--data-urlencode "csrf=$csrf" \
		--data-urlencode "user=alice" --data-urlencode "password=sesame-alice"
	eq "$REPLY_CODE" "303"
	local minted
	minted="$(auth_setcookie __Host-janus)"
	ok "-n \"$minted\"" "no session minted"
	ne "$minted" "attacker-chosen-token"
}

case_auth_wrong_creds() {
	local f0
	f0="$(auth_stat login_failures)"
	# Known user, wrong password.
	auth_attempt "$AUTH_WALL" bob wrong-password
	eq "$REPLY_CODE" "401"
	local body1="$REPLY_BODY"
	# Unknown user.
	auth_attempt "$AUTH_WALL" ghost whatever
	eq "$REPLY_CODE" "401"
	local body2="$REPLY_BODY"
	# One generic message; the bodies are identical once the per-render
	# CSRF token is normalized out — which half was wrong is never disclosed.
	local n1 n2
	n1="$(printf '%s' "$body1" | sed 's/name="csrf" value="[^"]*"/name="csrf" value=X/')"
	n2="$(printf '%s' "$body2" | sed 's/name="csrf" value="[^"]*"/name="csrf" value=X/')"
	eq "$n2" "$n1"
	json_has "$body1" 'Invalid credentials'
	eq "$(auth_stat login_failures)" "$((f0 + 2))"
}

case_auth_throttle() {
	local t0 i
	# Clear the per-IP aggregate left by earlier failure cases (all curl
	# traffic shares 127.0.0.1 as the client key).
	auth_login "$AUTH_WALL" alice sesame-alice
	t0="$(auth_stat throttled)"
	# Ladder: fails 1–3 immediate 401; 4th sets a 10s wait…
	for i in 1 2 3 4; do
		auth_attempt "$AUTH_WALL" eve bad-guess
		eq "$REPLY_CODE" "401"
	done
	# …and the next try during the wait answers 429 (wait does not increment).
	auth_attempt "$AUTH_WALL" eve bad-guess
	eq "$REPLY_CODE" "429"
	ok "-n \"$(auth_hdr retry-after)\"" "429 carries no Retry-After"
	ok "$(auth_stat throttled) -gt $t0" "throttled counter unmoved"
	# Aggregate wait blocks every username from this client key; wait it out,
	# then a successful login clears the aggregate for later cases.
	sleep 11
	auth_login "$AUTH_WALL" alice sesame-alice
}

case_auth_prefix_gates() {
	# Longest prefix: bob may pass gate / but not /one/.
	auth_login "$AUTH_WALL" bob sesame-bob
	auth_req GET "$AUTH_WALL/" -H "Cookie: __Host-janus=$AUTH_SESSION"
	eq "$REPLY_CODE" "200"
	auth_req GET "$AUTH_WALL/one/x" -H "Cookie: __Host-janus=$AUTH_SESSION" -H 'Accept: text/html'
	eq "$REPLY_CODE" "302"
	eq "$(auth_hdr location)" "/one/auth?to=%2Fone%2Fx"
	# alice signs in once at /one/auth and passes both gates that list her.
	auth_req GET "$AUTH_WALL/one/auth"
	eq "$REPLY_CODE" "200"
	local csrf
	csrf="$(auth_csrf_from_body)"
	auth_req POST "$AUTH_WALL/one/auth" -H "Cookie: __Host-janus_csrf=$csrf" \
		--data-urlencode "csrf=$csrf" \
		--data-urlencode "user=alice" --data-urlencode "password=sesame-alice"
	eq "$REPLY_CODE" "303"
	eq "$(auth_hdr location)" "/one/"
	AUTH_SESSION="$(auth_setcookie __Host-janus)"
	ok "-n \"$AUTH_SESSION\"" "prefix-gate login minted no session"
	auth_req GET "$AUTH_WALL/one/x" -H "Cookie: __Host-janus=$AUTH_SESSION"
	eq "$REPLY_CODE" "200"
	auth_req GET "$AUTH_WALL/" -H "Cookie: __Host-janus=$AUTH_SESSION"
	eq "$REPLY_CODE" "200"
	# larry may use /one/ but not gate /.
	auth_req GET "$AUTH_WALL/one/auth"
	csrf="$(auth_csrf_from_body)"
	auth_req POST "$AUTH_WALL/one/auth" -H "Cookie: __Host-janus_csrf=$csrf" \
		--data-urlencode "csrf=$csrf" \
		--data-urlencode "user=larry" --data-urlencode "password=sesame-larry"
	eq "$REPLY_CODE" "303"
	AUTH_SESSION="$(auth_setcookie __Host-janus)"
	auth_req GET "$AUTH_WALL/one/y" -H "Cookie: __Host-janus=$AUTH_SESSION"
	eq "$REPLY_CODE" "200"
	auth_req GET "$AUTH_WALL/" -H "Cookie: __Host-janus=$AUTH_SESSION" -H 'Accept: text/html'
	eq "$REPLY_CODE" "302"
}

case_auth_ping_open() {
	# Liveness is not a secret: /ping answers with no session on the
	# guarded site (site-scoped ping inherited on).
	eq "$(http_body "$AUTH_WALL/ping")" "pong"
	eq "$(http_code "$AUTH_WALL/ping")" "200"
}

case_auth_exact_match_only() {
	# /auth neighbors are ordinary app paths: with a session they proxy
	# to the upstream (which records them), never the login machinery.
	auth_login "$AUTH_WALL" alice sesame-alice
	local p
	for p in /authors /auth/callback /auth.js; do
		auth_req GET "$AUTH_WALL$p" -H "Cookie: __Host-janus=$AUTH_SESSION"
		eq "$REPLY_CODE" "200"
		json_has "$REPLY_BODY" 'remote-user:alice'
		ok "-n \"$(grep "^GET $p " "$AUTH_HITS" | tail -1)\"" "$p never reached the upstream"
	done
	# Without a session they are walled like any path (401, not a form).
	auth_req GET "$AUTH_WALL/authors"
	eq "$REPLY_CODE" "401"
}

case_auth_ws() {
	# A WS upgrade without a session → 401 (never a redirect an upgrade
	# cannot follow), no connection, no bridge.
	local resp code
	resp="$(curl -sS --max-time 5 --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
		-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
		-w $'\n%{http_code}' "$AUTH_WALL/hub")"
	code="${resp##*$'\n'}"
	eq "$code" "401"
	# With a session the upgrade connects, and the tenant's open-bridge
	# snapshot carries the injected Remote-User — never the wall's cookie.
	auth_login "$AUTH_WALL" alice sesame-alice
	local b0
	b0="$(grep -c '^BRIDGE open ' "$AUTH_HITS" 2>/dev/null || true)"
	hub_ws authwall.ripdev.io https://authwall.ripdev.io "__Host-janus=$AUTH_SESSION" id close
	local rec
	rec="$(grep '^BRIDGE open ' "$AUTH_HITS" | tail -1)"
	ok "-n \"$rec\"" "no open bridge recorded: $REPLY_WS"
	ok "$(grep -c '^BRIDGE open ' "$AUTH_HITS") -gt ${b0:-0}" "open bridge count unmoved"
	json_has "$rec" 'remote-user=alice'
	if printf '%s' "$rec" | grep -qF '__Host-janus'; then
		echo "bridge snapshot holds the session cookie: $rec" >&2
		return 1
	fi
}

case_auth_cascade_users() {
	# Site auth { … } replaces global: alice's credentials fail on the
	# carol-only site (same generic 401)…
	auth_attempt "$AUTH_CAROL" alice sesame-alice
	eq "$REPLY_CODE" "401"
	# …and alice's LIVE SESSION from authwall is unauthenticated there —
	# authorization is per request against the site's users + gates.
	auth_login "$AUTH_WALL" alice sesame-alice
	auth_req GET "$AUTH_CAROL/" -H "Cookie: __Host-janus=$AUTH_SESSION" -H 'Accept: text/html'
	eq "$REPLY_CODE" "302"
	# carol opens the carol door; the upstream sees carol.
	auth_login "$AUTH_CAROL" carol sesame-carol
	auth_req GET "$AUTH_CAROL/" -H "Cookie: __Host-janus=$AUTH_SESSION"
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" 'remote-user:carol'
}

case_auth_reload_keeps_sessions() {
	auth_login "$AUTH_WALL" alice sesame-alice
	printf '%s' "$AUTH_SESSION" >"$AUTH_ALICE_COOKIE"
	if ! XDG_DATA_HOME="$TEST_RUN_DIR/caddy-data" \
		reload_caddy --config "$ROOT/Caddyfile" --force >>"$CADDY_LOG" 2>&1; then
		echo "caddy reload failed; see $CADDY_LOG" >&2
		return 1
	fi
	local i
	for i in $(seq 1 50); do
		curl -sS -o /dev/null --max-time 1 http://127.0.0.1:7600/1.0/health 2>/dev/null && break
		sleep 0.1
	done
	# The pooled session store survived: no re-login.
	auth_req GET "$AUTH_WALL/" -H "Cookie: __Host-janus=$(cat "$AUTH_ALICE_COOKIE")"
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" 'remote-user:alice'
}

case_auth_reload_revokes_removed_user() {
	auth_login "$AUTH_WALL" bob sesame-bob
	local bob_session="$AUTH_SESSION" r0
	r0="$(auth_stat reload_revoked)"
	# Reload with bob's users-table line deleted: his session dies at Start.
	local stripped="$TEST_RUN_DIR/auth-stripped-caddyfile"
	sed '/[[:space:]]bob[[:space:]]*a[0-9A-Za-z]\{31\}/d' "$ROOT/Caddyfile" >"$stripped"
	# gate / still lists bob — drop bob from the allow list too.
	sed -i.bak 's/alice bob/alice/' "$stripped"
	rm -f "$stripped.bak"
	if grep -qE '[[:space:]]bob[[:space:]]*a[0-9A-Za-z]{31}' "$stripped"; then
		echo "failed to strip bob from the reload config" >&2
		return 1
	fi
	if ! XDG_DATA_HOME="$TEST_RUN_DIR/caddy-data" \
		reload_caddy --config "$stripped" --adapter caddyfile --force >>"$CADDY_LOG" 2>&1; then
		echo "caddy reload (bob removed) failed; see $CADDY_LOG" >&2
		return 1
	fi
	local i
	for i in $(seq 1 50); do
		curl -sS -o /dev/null --max-time 1 http://127.0.0.1:7600/1.0/health 2>/dev/null && break
		sleep 0.1
	done
	auth_req GET "$AUTH_WALL/" -H "Cookie: __Host-janus=$bob_session" -H 'Accept: text/html'
	eq "$REPLY_CODE" "302"
	ok "$(auth_stat reload_revoked) -gt $r0" "reload_revoked counter unmoved"
	# alice appears in the surviving set: her session lives on.
	auth_req GET "$AUTH_WALL/" -H "Cookie: __Host-janus=$(cat "$AUTH_ALICE_COOKIE")"
	eq "$REPLY_CODE" "200"
	# Restore the real config.
	if ! XDG_DATA_HOME="$TEST_RUN_DIR/caddy-data" \
		reload_caddy --config "$ROOT/Caddyfile" --force >>"$CADDY_LOG" 2>&1; then
		echo "caddy reload (restore) failed; see $CADDY_LOG" >&2
		return 1
	fi
	rm -f "$stripped"
}

case_auth_hot_revoke() {
	# Start from a clean slate, then one known session.
	capi DELETE /1.0/auth/sessions
	eq "$REPLY_CODE" "200"
	auth_login "$AUTH_WALL" alice sesame-alice
	local session="$AUTH_SESSION"
	capi GET /1.0/auth/sessions
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"user":"alice"'
	json_has "$REPLY_BODY" '"host":"authwall.ripdev.io"'
	json_has "$REPLY_BODY" '"gates"'
	json_has "$REPLY_BODY" '"/"'
	json_has "$REPLY_BODY" '"/one/"'
	json_has "$REPLY_BODY" '"age_ms"'
	json_has "$REPLY_BODY" '"idle_ms"'
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	eq "${#id}" "12"
	# Unknown id → 404; the listed id revokes → that cookie is dead.
	capi DELETE /1.0/auth/sessions/000000000000
	eq "$REPLY_CODE" "404"
	capi DELETE "/1.0/auth/sessions/$id"
	eq "$REPLY_CODE" "204"
	auth_req GET "$AUTH_WALL/" -H "Cookie: __Host-janus=$session" -H 'Accept: text/html'
	eq "$REPLY_CODE" "302"
	# Wipe: two logins, one DELETE, both dead.
	auth_login "$AUTH_WALL" alice sesame-alice
	local s1="$AUTH_SESSION"
	auth_login "$AUTH_WALL" bob sesame-bob
	local s2="$AUTH_SESSION"
	capi DELETE /1.0/auth/sessions
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"revoked":2'
	auth_req GET "$AUTH_WALL/" -H "Cookie: __Host-janus=$s1" -H 'Accept: text/html'
	eq "$REPLY_CODE" "302"
	auth_req GET "$AUTH_WALL/" -H "Cookie: __Host-janus=$s2" -H 'Accept: text/html'
	eq "$REPLY_CODE" "302"
}

case_auth_minter_and_dead_wall() {
	# The minter reads stdin when piped (no argv, no echo) and prints one
	# version-a passhash under the fixed constants.
	local blob
	blob="$(printf 'mint-pass\n' | "$CADDY_BIN" passhash)"
	if [[ ! "$blob" =~ ^a[0-9A-Za-z]{31}$ ]]; then
		printf 'minted blob %q is not a 32-char version-a passhash' "$blob" >&2
		return 1
	fi
	# A second, sockets-apart caddy proves the blob verifies end-to-end —
	# and pins the plain-HTTP dead wall (421 + the once-per-site ERROR).
	local dir
	dir="$(mktemp -d "$TEST_RUN_DIR/janus-auth-mint.XXXXXX")"
	cat >"$dir/Caddyfile" <<EOF
{
	admin off
	auto_https disable_redirects
	skip_install_trust
	janus {
		control local http://127.0.0.1:7602/
	}
}

https://mint.ripdev.io:8444 {
	tls $ROOT/certs/ripdev.io.crt $ROOT/certs/ripdev.io.key
	janus {
		auth {
			user mint $blob
			gate / {
				mint
			}
		}
	}
}

http://authdead.ripdev.io:8081 {
	janus {
		auth {
			user mint $blob
			gate / {
				mint
			}
		}
	}
}
EOF
	local log="$TEST_RUN_DIR/auth-mint.log"
	"$CADDY_BIN" run --config "$dir/Caddyfile" >"$log" 2>&1 &
	local pid=$!
	printf '%s\n' "$pid" >>"$AUTH_PIDS_FILE"
	local i ready=""
	for i in $(seq 1 50); do
		if curl -sS -o /dev/null --max-time 1 http://127.0.0.1:7602/1.0/health 2>/dev/null; then
			ready=1
			break
		fi
		sleep 0.1
	done
	if [[ -z "$ready" ]]; then
		echo "minter caddy never became ready:" >&2
		tail -5 "$log" >&2 || true
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
		rm -rf "$dir"
		return 1
	fi
	local rc=0
	{
		# The wall stands…
		auth_req GET "https://mint.ripdev.io:8444/" -H 'Accept: text/html'
		eq "$REPLY_CODE" "302"
		# …and the minted credential opens it: 303, then an authenticated
		# request reaches the (empty) data plane — 404, not the wall.
		auth_login "https://mint.ripdev.io:8444" mint mint-pass
		auth_req GET "https://mint.ripdev.io:8444/" -H "Cookie: __Host-janus=$AUTH_SESSION"
		eq "$REPLY_CODE" "404"
		# The plain-HTTP guarded site is a dead wall: loud 421, named remedy.
		auth_req GET "http://authdead.ripdev.io:8081/anything"
		eq "$REPLY_CODE" "421"
		json_has "$REPLY_BODY" 'auth requires HTTPS'
		json_has "$(cat "$log")" 'site with auth reached over plain HTTP'
	} || rc=1
	kill "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
	rm -rf "$dir"
	return $rc
}

case_auth_restart_wipes() {
	auth_login "$AUTH_WALL" alice sesame-alice
	printf '%s' "$AUTH_SESSION" >"$AUTH_ALICE_COOKIE"
	printf '%s\n' "$(paint "$DIM" "restarting caddy …")" >&2
	stop_caddy
	start_caddy || return 1
	# Memory-only by contract: the old cookie is dead, everyone signs in again.
	auth_req GET "$AUTH_WALL/" -H "Cookie: __Host-janus=$(cat "$AUTH_ALICE_COOKIE")" -H 'Accept: text/html'
	eq "$REPLY_CODE" "302"
	auth_login "$AUTH_WALL" alice sesame-alice
}

case_auth_zero_users_lockout() {
	# Bare auth on with nothing to inherit refuses to load.
	local dir
	dir="$(mktemp -d "$TEST_RUN_DIR/janus-auth-lockout.XXXXXX")"
	cat >"$dir/Caddyfile" <<'EOF'
{
	admin off
	skip_install_trust
	janus {
	}
}

site.example.com {
	janus {
		auth on
	}
}
EOF
	local out
	if out="$("$CADDY_BIN" validate --config "$dir/Caddyfile" --adapter caddyfile 2>&1)"; then
		echo "caddy validate accepted bare auth on with no config to inherit" >&2
		rm -rf "$dir"
		return 1
	fi
	if ! printf '%s' "$out" | grep -qE 'no auth config to inherit|incomplete'; then
		printf 'lockout error is imprecise: %q' "$out" >&2
		rm -rf "$dir"
		return 1
	fi
	# Users without gates also refuse (needs a site handler to provision).
	local blob
	blob="aZoyWD0mfNZH7GZCh3DH9Te1FwAxA0yc"
	cat >"$dir/Caddyfile" <<EOF
{
	admin off
	skip_install_trust
	janus {
		auth {
			user alice $blob
		}
	}
}

site.example.com {
	janus
}
EOF
	if out="$("$CADDY_BIN" validate --config "$dir/Caddyfile" --adapter caddyfile 2>&1)"; then
		echo "caddy validate accepted auth with users but zero gates" >&2
		rm -rf "$dir"
		return 1
	fi
	if ! printf '%s' "$out" | grep -qF 'incomplete'; then
		printf 'zero-gates error is imprecise: %q' "$out" >&2
		rm -rf "$dir"
		return 1
	fi
	rm -rf "$dir"
}

case_auth_parse_rejections() {
	local bad dir blob
	blob="aZoyWD0mfNZH7GZCh3DH9Te1FwAxA0yc"
	dir="$(mktemp -d "$TEST_RUN_DIR/janus-auth-parse.XXXXXX")"
	local -a cases=(
		'auth maybe'
		'auth on off'
		"auth off {
			user alice $blob
			gate / { alice }
		}"
		'auth
		auth off'
		'auth { }'
		'auth { bogus 1 }'
		'auth { user }'
		'auth { user alice }'
		"auth { user alice $blob extra }"
		"auth {
			user alice $blob
			user alice $blob
		}"
		"auth { user al:ice $blob }"
		'auth { user alice plaintext }'
		'auth { user alice x2:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA }'
		'auth { user alice ashort }'
		"auth { user alice $blob== }"
		'auth { ttl }'
		'auth { ttl 0 }'
		'auth { ttl nope }'
		"auth {
			ttl 1h
			ttl 2h
		}"
		'auth { ttl 1h { nested } }'
		"auth {
			user alice $blob
			gate /one/ { bob }
		}"
		"auth {
			user alice $blob
			gate /one/ { alice }
			gate /one/auth/ { alice }
		}"
		"auth {
			user alice $blob
			gate ones { alice }
		}"
	)
	for bad in "${cases[@]}"; do
		cat >"$dir/Caddyfile" <<EOF
{
	janus {
		$bad
	}
}
EOF
		if "$CADDY_BIN" adapt --config "$dir/Caddyfile" >/dev/null 2>&1; then
			echo "caddy adapt accepted illegal global config: $bad" >&2
			rm -rf "$dir"
			return 1
		fi
		cat >"$dir/Caddyfile" <<EOF
site.example.com {
	janus {
		$bad
	}
}
EOF
		if "$CADDY_BIN" adapt --config "$dir/Caddyfile" >/dev/null 2>&1; then
			echo "caddy adapt accepted illegal site config: $bad" >&2
			rm -rf "$dir"
			return 1
		fi
	done
	rm -rf "$dir"
}
