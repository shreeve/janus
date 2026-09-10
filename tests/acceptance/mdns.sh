# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: mdns ---------------------------------------------------------------
#
# Capability 5: LAN presence (docs/20260722-034619-capability-mdns.md
# "Acceptance sketch"). Multicast itself is not CI-assertable, so no case
# sends or receives a multicast packet: the group asserts the ADVERTISER'S
# STATE (via /1.0/mdns — counters, the pinned state enum, the skipped
# gauge) and the FRONT DOOR'S behavior (direct HTTP to the listener).
# The root Caddyfile pins `listen :7680` — DEDICATED mode — so the group
# runs unprivileged; the shared-mode decider (default, front door inside
# the :80 HTTP server) needs a privileged bind and is pinned in the
# Go-test layer instead.
# The group runs under the heartbeat caddy (TTL 2s): cases that span time
# heartbeat their apps; the reap case deliberately does not.

MDNS_URL="http://127.0.0.1:7680"
MDNS_APP_FILE="$TEST_RUN_DIR/mdns-app-id"
MDNS_CANON_PID_FILE="$TEST_RUN_DIR/mdns-canon-pid"

mdns_app_id() { cat "$MDNS_APP_FILE"; }

# mdns_stat KEY — one top-level value from GET /1.0/mdns
mdns_stat() {
	capi GET /1.0/mdns
	printf '%s' "$REPLY_BODY" | "$TESTKIT" json get "$1"
}

# mdns_hb — keep the group's app alive across polls (TTL is 2s)
mdns_hb() {
	if [[ -f "$MDNS_APP_FILE" ]]; then
		capi POST "/1.0/apps/$(mdns_app_id)/heartbeat" || true
	fi
}

# mdns_wait_advertised NAME — poll until NAME appears in the advertised
# array. Matched by the name+state field adjacency of an advertised
# entry, never the top-level "name" field (which always matches the
# configured name, advertised or not); "failed" does not count.
mdns_wait_advertised() {
	local name=$1 i
	for i in $(seq 1 100); do
		mdns_hb
		capi GET /1.0/mdns
		if printf '%s' "$REPLY_BODY" |
			grep -qE "\"name\":\"$name\",\"state\":\"(probing|announced|renamed)\""; then
			return 0
		fi
		sleep 0.1
	done
	echo "$name never advertised: $REPLY_BODY" >&2
	return 1
}

# mdns_wait_gone NAME — poll until NAME leaves the advertised array
mdns_wait_gone() {
	local name=$1 i
	for i in $(seq 1 100); do
		mdns_hb
		capi GET /1.0/mdns
		if ! printf '%s' "$REPLY_BODY" | grep -qE "\"name\":\"$name\",\"state\":\""; then
			return 0
		fi
		sleep 0.1
	done
	echo "$name still advertised: $REPLY_BODY" >&2
	return 1
}

# mdns_wait_quiet — poll until the announce and withdraw counters hold
# still for a second. An entry leaves /1.0/mdns the moment its withdrawal
# is decided; the counter moves only after the responder's goodbye
# returns, which can take seconds. A baseline read in that window is one
# short and reads as a flap.
mdns_wait_quiet() {
	local i prev cur
	prev="$(mdns_stat announces)/$(mdns_stat withdraws)"
	for i in $(seq 1 30); do
		sleep 1
		mdns_hb
		cur="$(mdns_stat announces)/$(mdns_stat withdraws)"
		if [[ "$cur" == "$prev" ]]; then
			return 0
		fi
		prev=$cur
	done
	echo "mdns counters never held still: $cur" >&2
	return 1
}

# mdns_wait_settled — poll until no entry is still probing or awaiting
# an Add retry (announces / withdraws quiesce so counter deltas are
# attributable)
mdns_wait_settled() {
	local i
	for i in $(seq 1 100); do
		mdns_hb
		capi GET /1.0/mdns
		if ! printf '%s' "$REPLY_BODY" | grep -qE '"state":"(probing|failed)"'; then
			return 0
		fi
		sleep 0.2
	done
	echo "advertiser never settled: $REPLY_BODY" >&2
	return 1
}

stop_mdns_canon() {
	if [[ -f "$MDNS_CANON_PID_FILE" ]]; then
		stop_owned_pid "$(cat "$MDNS_CANON_PID_FILE")" "$CADDY_BIN"
		rm -f "$MDNS_CANON_PID_FILE"
	fi
}

case_mdns_state() {
	# GET /1.0 carries the mdns boolean beside ping (presence-shaped).
	capi GET /1.0
	json_has "$REPLY_BODY" '"mdns":true'
	capi GET /1.0/mdns
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"enabled":true'
	json_has "$REPLY_BODY" '"name":"janus.local"'
	json_has "$REPLY_BODY" '"mode":"dedicated"' # listen set — the root Caddyfile's unprivileged fixture
	json_has "$REPLY_BODY" '"front_door":":7680"'
	json_has "$REPLY_BODY" '"announces"'
	json_has "$REPLY_BODY" '"withdraws"'
	# The configured name settles: announced, or renamed on a LAN that
	# already claims janus.local (both are settled states; probing is not).
	mdns_wait_advertised janus.local
	mdns_wait_settled
	local dashboard name
	dashboard="$(mdns_stat dashboard_url)"
	case "$dashboard" in
		'http://127.0.0.1:7680/'|'http://[::1]:7680/') ;;
		*) printf 'unexpected dashboard URL: %s' "$dashboard" >&2; return 1 ;;
	esac
	name="$(mdns_stat effective_name)"
	eq "$(mdns_stat trust_url)" "http://${name}:7680/trust"
	capi GET /1.0/mdns
	if ! printf '%s' "$REPLY_BODY" | grep -qE '"name":"janus.local","state":"(announced|renamed)"'; then
		printf 'janus.local never settled: %q' "$REPLY_BODY" >&2
		return 1
	fi
}

case_mdns_front_door() {
	local hdrs body
	hdrs="$(curl -sS -D - -o /dev/null --max-time 5 "$MDNS_URL/" | tr -d '\r')"
	json_has "$hdrs" 'HTTP/1.1 200'
	json_has "$hdrs" 'no-store'
	body="$(http_body "$MDNS_URL/")"
	json_has "$body" 'Janus'
	json_has "$body" '/status.json'
	hdrs="$(curl -sS -D - -o /dev/null --max-time 5 "$MDNS_URL/status.json" | tr -d '\r')"
	json_has "$hdrs" 'HTTP/1.1 200'
	json_has "$hdrs" 'no-store'
	body="$(http_body "$MDNS_URL/status.json")"
	json_has "$body" '"name":"janus.local"'
	json_has "$body" '"effective_name"'
	json_has "$body" '"advertised"'
	json_has "$body" '"apps"'
	json_has "$body" '"hub"'
}

case_mdns_host_allowlist() {
	# IP-literal Hosts and the advertised name are served; anything else
	# (a DNS-rebinding attacker's own domain) answers 421.
	eq "$(http_code "$MDNS_URL/")" "200" # Host: 127.0.0.1:7680 — IP literal
	local code
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -H 'Host: janus.local' "$MDNS_URL/")"
	eq "$code" "200"
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -H 'Host: evil.example.com' "$MDNS_URL/")"
	eq "$code" "421"
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -H 'Host: evil.example.com' "$MDNS_URL/status.json")"
	eq "$code" "421"
}

case_mdns_read_only() {
	local code m
	for m in POST PUT DELETE; do
		code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -X "$m" "$MDNS_URL/status.json")"
		eq "$code" "405"
	done
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -X POST "$MDNS_URL/")"
	eq "$code" "405"
	eq "$(http_code "$MDNS_URL/anything-else")" "404"
	eq "$(http_code "$MDNS_URL/status")" "404"
}

case_mdns_register_advertises() {
	capi POST /1.0/apps '{"name":"mdnsshop","hosts":["mdnsshop.local","mdnsshop.ripdev.io"]}'
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	ok "-n \"$id\"" "no id in $REPLY_BODY"
	printf '%s' "$id" >"$MDNS_APP_FILE"
	# The .local host advertises (carrying the app id); the public
	# hostname is never multicast material.
	mdns_wait_advertised mdnsshop.local
	capi GET /1.0/mdns
	json_has "$REPLY_BODY" "\"app\":\"$id\""
	if printf '%s' "$REPLY_BODY" | grep -qF 'mdnsshop.ripdev.io'; then
		printf 'non-.local host advertised: %q' "$REPLY_BODY" >&2
		return 1
	fi
}

case_mdns_site_alias_advertises_and_withdraws() {
	local dir="$TEST_RUN_DIR/mdns-sites-alias"
	mkdir -p "$dir/tenant"
	capi POST /1.0/apps \
		"{\"name\":\"mdnssite\",\"site\":{\"host\":\"{site}.mdnssite.test\",\"dir\":\"$dir\",\"aliases\":{\"mdnssite.local\":\"tenant\",\"mdnssite.ripdev.io\":\"tenant\"}}}"
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	mdns_wait_advertised mdnssite.local
	local i settled=""
	for i in $(seq 1 100); do
		capi POST "/1.0/apps/$id/heartbeat"
		capi GET /1.0/mdns
		if printf '%s' "$REPLY_BODY" |
			grep -qE '"name":"mdnssite.local","state":"(announced|renamed)"'; then
			settled=1
			break
		fi
		sleep 0.1
	done
	ok "-n \"$settled\"" "mdnssite.local never settled: $REPLY_BODY"
	capi GET /1.0/mdns
	if printf '%s' "$REPLY_BODY" | grep -qF '{site}.mdnssite.test'; then
		printf 'site pattern advertised: %q' "$REPLY_BODY" >&2
		return 1
	fi
	local status
	status="$(http_body "$MDNS_URL/status.json")"
	json_has "$status" '"site_pattern":"{site}.mdnssite.test"'
	json_has "$status" '"host":"mdnssite.local"'
	if [[ "$status" == *'"host":"mdnssite.local","url":"https://mdnssite.local/","mdns":"announced"'* ]]; then
		:
	else
		json_has "$status" '"host":"mdnssite.local","mdns":"renamed"'
	fi
	capi DELETE "/1.0/apps/$id"
	eq "$REPLY_CODE" "204"
	mdns_wait_gone mdnssite.local
}

case_mdns_multilabel_gauge() {
	mdns_hb
	local s0 id2 i
	s0="$(mdns_stat skipped_hosts)"
	capi POST /1.0/apps '{"name":"mdnsmulti","hosts":["a.b.local"]}'
	eq "$REPLY_CODE" "201"
	id2="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	# The gauge counts it; the name never advertises.
	local up=""
	for i in $(seq 1 50); do
		mdns_hb
		capi POST "/1.0/apps/$id2/heartbeat"
		if [[ "$(mdns_stat skipped_hosts)" == "$((s0 + 1))" ]]; then
			up=1
			break
		fi
		sleep 0.1
	done
	ok "-n \"$up\"" "skipped_hosts gauge never rose from $s0"
	capi GET /1.0/mdns
	if printf '%s' "$REPLY_BODY" | grep -qF '"name":"a.b.local"'; then
		printf 'multi-label host advertised: %q' "$REPLY_BODY" >&2
		return 1
	fi
	# Gauge, not counter: DELETE reconciles it back down.
	capi DELETE "/1.0/apps/$id2"
	eq "$REPLY_CODE" "204"
	local down=""
	for i in $(seq 1 50); do
		mdns_hb
		if [[ "$(mdns_stat skipped_hosts)" == "$s0" ]]; then
			down=1
			break
		fi
		sleep 0.1
	done
	ok "-n \"$down\"" "skipped_hosts gauge never fell back to $s0"
}

case_mdns_status_truth() {
	mdns_hb
	capi PUT "/1.0/apps/$(mdns_app_id)/upstreams" \
		'{"upstreams":[{"path":"/tmp/janus-mdns-SENTINEL-xyz.sock"}]}'
	eq "$REPLY_CODE" "200"
	capi PATCH "/1.0/apps/$(mdns_app_id)" '{"bridge":"/rt/SECRETBRIDGE-xyz"}'
	eq "$REPLY_CODE" "200"
	mdns_hb
	local body age
	body="$(http_body "$MDNS_URL/status.json")"
	json_has "$body" '"mdnsshop.local"'
	json_has "$body" '"mdnsshop.ripdev.io"'
	json_has "$body" '"shape":"api-only"'
	json_has "$body" '"admission":"workers"'
	json_has "$body" '"total":1'
	json_has "$body" '"healthy":1'
	age="$(printf '%s' "$body" | sed -n 's/.*"heartbeat_age_ms":\([0-9]*\).*/\1/p')"
	ok "-n \"$age\"" "no heartbeat_age_ms in $body"
}

case_mdns_redaction() {
	mdns_hb
	local body page
	body="$(http_body "$MDNS_URL/status.json")"
	page="$(http_body "$MDNS_URL/")"
	local secret
	for secret in 'SENTINEL-xyz' 'SECRETBRIDGE-xyz' 'bridge'; do
		if printf '%s%s' "$body" "$page" | grep -qF "$secret"; then
			printf 'front door leaked %q' "$secret" >&2
			return 1
		fi
	done
}

case_mdns_patch_reconciles() {
	mdns_hb
	capi PATCH "/1.0/apps/$(mdns_app_id)" '{"hosts":["mdnsstore.local"]}'
	eq "$REPLY_CODE" "200"
	# Exactly the diff: the new name arrives, the old name withdraws.
	mdns_wait_advertised mdnsstore.local
	mdns_wait_gone mdnsshop.local
}

case_mdns_delete_withdraws() {
	mdns_hb
	mdns_wait_settled
	local w0
	w0="$(mdns_stat withdraws)"
	capi DELETE "/1.0/apps/$(mdns_app_id)"
	eq "$REPLY_CODE" "204"
	rm -f "$MDNS_APP_FILE"
	mdns_wait_gone mdnsstore.local
	# The counter moves once the goodbye lands — the library sends it per
	# interface with real inter-packet delays, so poll rather than read.
	local i
	for i in $(seq 1 100); do
		[[ "$(mdns_stat withdraws)" -gt "$w0" ]] && return 0
		sleep 0.1
	done
	echo "withdraws counter never moved past $w0" >&2
	return 1
}

case_mdns_reap_withdraws() {
	# Register, heartbeat until the name is advertised (goodbyes from the
	# previous case can hold the reconcile goroutine for several seconds,
	# and the TTL here is only 2s), then go silent: the TTL reap
	# withdraws the name the same way DELETE does.
	local dir="$TEST_RUN_DIR/mdns-sites-reap"
	mkdir -p "$dir/tenant"
	capi POST /1.0/apps \
		"{\"name\":\"mdnsreap\",\"site\":{\"host\":\"{site}.mdnsreap.test\",\"dir\":\"$dir\",\"aliases\":{\"mdnsreap.local\":\"tenant\"}}}"
	eq "$REPLY_CODE" "201"
	printf '%s' "$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')" >"$MDNS_APP_FILE"
	mdns_wait_advertised mdnsreap.local # mdns_hb keeps it alive while waiting
	rm -f "$MDNS_APP_FILE"              # heartbeats stop; the reap clock runs
	sleep 3.5
	mdns_wait_gone mdnsreap.local
}

case_mdns_reload_no_flap() {
	mdns_wait_settled
	mdns_wait_quiet
	local a0 w0 adv0 adv1
	a0="$(mdns_stat announces)"
	w0="$(mdns_stat withdraws)"
	capi GET /1.0/mdns
	adv0="$(printf '%s' "$REPLY_BODY" | grep -o '"name":"[^"]*"' | sort)"
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
	# No LAN flap: the advertised set is identical and the responder
	# never moved — zero announces, zero withdraws across the reload.
	capi GET /1.0/mdns
	adv1="$(printf '%s' "$REPLY_BODY" | grep -o '"name":"[^"]*"' | sort)"
	eq "$adv1" "$adv0"
	eq "$(mdns_stat announces)" "$a0"
	eq "$(mdns_stat withdraws)" "$w0"
	# The front door still serves (pooled listener reused, never rebound).
	eq "$(http_code "$MDNS_URL/status.json")" "200"
}

case_mdns_reload_teardown() {
	# Reload with mdns removed: real teardown — the advertiser withdraws
	# and the front-door port closes; /1.0/mdns answers enabled:false.
	local stripped="$TEST_RUN_DIR/mdns-stripped-caddyfile"
	# Locate the block structurally — opener to its closing brace — so the
	# test survives comment and formatting changes inside the Caddyfile.
	# (The mdns block nests no braces; the first bare } closes it.)
	local start end
	start="$(grep -nE '^[[:space:]]*mdns \{' "$ROOT/Caddyfile" | cut -d: -f1 | head -n1)"
	ok "-n \"$start\"" "mdns block not found in the root Caddyfile"
	end="$(awk -v s="$start" 'NR > s && /^[[:space:]]*\}[[:space:]]*$/ { print NR; exit }' "$ROOT/Caddyfile")"
	ok "-n \"$end\"" "mdns block closing brace not found"
	sed "${start},${end}d" "$ROOT/Caddyfile" >"$stripped"
	if grep -qE '^[[:space:]]*mdns' "$stripped"; then
		echo "failed to strip the mdns block from the reload config" >&2
		return 1
	fi
	if ! XDG_DATA_HOME="$TEST_RUN_DIR/caddy-data" \
		reload_caddy --config "$stripped" --adapter caddyfile --force >>"$CADDY_LOG" 2>&1; then
		echo "caddy reload (mdns removed) failed; see $CADDY_LOG" >&2
		return 1
	fi
	local i ok_state=""
	for i in $(seq 1 50); do
		capi GET /1.0/mdns
		if [[ "$REPLY_CODE" == "200" ]] && printf '%s' "$REPLY_BODY" | grep -qF '"enabled":false'; then
			ok_state=1
			break
		fi
		sleep 0.1
	done
	ok "-n \"$ok_state\"" "/1.0/mdns never reported enabled:false: $REPLY_BODY"
	# The GET /1.0 boolean tracks presence in the live config.
	capi GET /1.0
	json_has "$REPLY_BODY" '"mdns":false'
	local closed=""
	for i in $(seq 1 50); do
		if ! curl -sS -o /dev/null --max-time 1 "$MDNS_URL/" 2>/dev/null; then
			closed=1
			break
		fi
		sleep 0.1
	done
	ok "-n \"$closed\"" "front-door port never closed after teardown"
	# Reload the real config back: the capability returns (a fresh probe
	# is expected — removal was a real teardown, not a pool release).
	if ! XDG_DATA_HOME="$TEST_RUN_DIR/caddy-data" \
		reload_caddy --config "$ROOT/Caddyfile" --force >>"$CADDY_LOG" 2>&1; then
		echo "caddy reload (restore) failed; see $CADDY_LOG" >&2
		return 1
	fi
	mdns_wait_advertised janus.local
	rm -f "$stripped"
}

case_mdns_canonical_present() {
	# A second, sockets-apart caddy proves the canonical rows: /1.0/mdns
	# and /status.json carry the origin; the page ships the probe script.
	local dir
	dir="$(mktemp -d "$TEST_RUN_DIR/janus-mdns-canon.XXXXXX")"
	cat >"$dir/Caddyfile" <<'EOF'
{
	admin off
	janus {
		control local http://127.0.0.1:7602/
		mdns {
			name januscanon.local
			canonical https://janus.lan.example.com
			listen :7681
		}
	}
}
EOF
	"$CADDY_BIN" run --config "$dir/Caddyfile" >"$TEST_RUN_DIR/mdns-canon.log" 2>&1 &
	printf '%s' "$!" >"$MDNS_CANON_PID_FILE"
	local i ready=""
	for i in $(seq 1 50); do
		if curl -sS -o /dev/null --max-time 1 http://127.0.0.1:7602/1.0/health 2>/dev/null; then
			ready=1
			break
		fi
		sleep 0.1
	done
	if [[ -z "$ready" ]]; then
		echo "canonical caddy never became ready:" >&2
		tail -5 "$TEST_RUN_DIR/mdns-canon.log" >&2 || true
		stop_mdns_canon
		rm -rf "$dir"
		return 1
	fi
	local state status page rc=0
	state="$(curl -sS --max-time 5 http://127.0.0.1:7602/1.0/mdns)"
	status="$(curl -sS --max-time 5 http://127.0.0.1:7681/status.json)"
	page="$(curl -sS --max-time 5 http://127.0.0.1:7681/)"
	json_has "$state" '"canonical":"https://janus.lan.example.com"' || rc=1
	json_has "$state" '"name":"januscanon.local"' || rc=1
	json_has "$status" '"canonical":"https://janus.lan.example.com"' || rc=1
	json_has "$page" 'no-cors' || rc=1
	json_has "$page" 'location.replace' || rc=1
	stop_mdns_canon
	rm -rf "$dir"
	return $rc
}

case_mdns_parse_rejections() {
	local bad dir
	dir="$(mktemp -d "$TEST_RUN_DIR/janus-mdns-parse.XXXXXX")"
	local -a cases=(
		'mdns on'
		'mdns off'
		'mdns { bogus 1 }'
		'mdns { name }'
		'mdns { name janus }'
		'mdns { name a.b.local }'
		'mdns { name janus.lan }'
		'mdns { name janus.local. }'
		'mdns { canonical http://x.example.com }'
		'mdns { canonical https://x.example.com/path }'
		'mdns { canonical https://10.0.0.1 }'
		'mdns { canonical https://x.local }'
		'mdns { interface }'
		'mdns { apps maybe }'
		'mdns { apps }'
		'mdns { listen 80 }'
		'mdns { listen :0 }'
		'mdns { name janus.local
			name x.local }'
		'mdns { name janus.local { nested } }'
		'mdns
		mdns'
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
	done
	# Site-level mdns is a parse error (process-wide only).
	cat >"$dir/Caddyfile" <<EOF
site.example.com {
	janus {
		mdns
	}
}
EOF
	if "$CADDY_BIN" adapt --config "$dir/Caddyfile" >/dev/null 2>&1; then
		echo "caddy adapt accepted site-level mdns" >&2
		rm -rf "$dir"
		return 1
	fi
	rm -rf "$dir"
}
