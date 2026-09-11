# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: hub ---------------------------------------------------------------
#
# Capability 4: per-app WebSocket fan-out
# (docs/20260720-162350-hub-design.md "Acceptance sketch"). The instrument:
# a fixture tenant with a bridge endpoint that records every POST it
# receives (headers, frame type, body) and answers from a scriptable
# playbook, plus /1.0/hub counters and the membership snapshot. The
# testkit ws driver runs sockets. The group runs under the heartbeat
# caddy (TTL 2s), so the fixture heartbeats its apps every 500ms.
#
# Hosts (root Caddyfile): hub1/hub2/hubdel/hubrace.ripdev.io inherit
# global hub on (origin same — the driver sends a browser-shaped Origin);
# hubany.ripdev.io is origin any; hubdirect.ripdev.io is direct with
# origin any; hubten/hubtwenty cap max_conns 10/20;
# api.ripdev.io is hub off.

HUB_SOCK="$TEST_RUN_DIR/run/hub-tenant.sock"
HUB_BRIDGE_LOG="$TEST_RUN_DIR/hub-bridge.jsonl"
HUB_PLAYBOOK="$TEST_RUN_DIR/hub-playbook"
HUB_PIDS_FILE="$TEST_RUN_DIR/hub-pids"
HUB_APP_FILE="$TEST_RUN_DIR/hub-app-id"       # hubapp: hub1, hubany, api
HUB_ISO_FILE="$TEST_RUN_DIR/hub-iso-id"       # hubiso: hub2 (per-app isolation)
HUB_CAP_FILE="$TEST_RUN_DIR/hub-cap-id"       # hubcap: hubten + hubtwenty (floor 10)
HUB_DIRECT_FILE="$TEST_RUN_DIR/hub-direct-id" # hubdirect: no bridge or upstream

hub_app_id() { cat "$HUB_APP_FILE"; }
hub_iso_id() { cat "$HUB_ISO_FILE"; }
hub_cap_id() { cat "$HUB_CAP_FILE"; }
hub_direct_id() { cat "$HUB_DIRECT_FILE"; }

stop_hub_fixtures() {
	if [[ -f "$HUB_PIDS_FILE" ]]; then
		while read -r pid; do
			stop_owned_pid "$pid" "$TESTKIT"
		done <"$HUB_PIDS_FILE"
	fi
	rm -f "$HUB_PIDS_FILE" "$HUB_BRIDGE_LOG" "$HUB_PLAYBOOK" \
		"$HUB_APP_FILE" "$HUB_ISO_FILE" "$HUB_CAP_FILE" "$HUB_DIRECT_FILE" "$HUB_SOCK" \
		"$TEST_RUN_DIR"/hub-flag-* "$TEST_RUN_DIR"/hub-out-* "$TEST_RUN_DIR"/hub-cap-codes
}

# hub_playbook JSON — set the fixture's scripted answers ('' resets).
# Shape: {"open":{"status":200,"body":"...","delay_ms":0},"text":{...},"close":{...}}
hub_playbook() {
	if [[ -z "$1" ]]; then
		rm -f "$HUB_PLAYBOOK"
	else
		printf '%s' "$1" >"$HUB_PLAYBOOK"
	fi
}

# hub_stat KEY — one process-total counter from GET /1.0/hub
hub_stat() {
	capi GET /1.0/hub
	printf '%s' "$REPLY_BODY" | "$TESTKIT" json get "$1"
}

# hub_snapshot APPID — GET /1.0/apps/{id}/hub into REPLY_BODY/REPLY_CODE
hub_snapshot() {
	capi GET "/1.0/apps/$1/hub"
}

# hub_publish APPID JSON — POST the publish plane
hub_publish() {
	capi POST "/1.0/apps/$1/hub/publish" "$2"
}

# hub_bridge_count KIND — how many bridge POSTs of one frame kind landed
hub_bridge_count() {
	local kind=$1
	if [[ ! -f "$HUB_BRIDGE_LOG" ]]; then
		echo 0
		return
	fi
	grep -c "\"kind\": \"$kind\"" "$HUB_BRIDGE_LOG" || true
}

# hub_ws HOST ORIGIN COOKIE CMDS… — run the WS driver in the foreground;
# output (RECV/CLOSE/ID lines) lands in REPLY_WS. ORIGIN/COOKIE '-' = none.
hub_ws() {
	local host=$1 origin=$2 cookie=$3
	shift 3
	REPLY_WS="$("$TESTKIT" ws "$host" "$origin" "$cookie" "$@" 2>&1)"
}

# hub_ws_bg OUTFILE HOST ORIGIN COOKIE CMDS… — same, backgrounded; the
# driver's pid lands in HUB_WS_PID for a targeted wait.
hub_ws_bg() {
	local out=$1 host=$2 origin=$3 cookie=$4
	shift 4
	"$TESTKIT" ws "$host" "$origin" "$cookie" "$@" >"$out" 2>&1 &
	HUB_WS_PID=$!
	printf '%s\n' "$HUB_WS_PID" >>"$HUB_PIDS_FILE"
}

wait_file() {
	local f=$1 i
	for i in $(seq 1 100); do
		[[ -e "$f" ]] && return 0
		sleep 0.1
	done
	echo "file $f never appeared" >&2
	return 1
}

# hub_upgrade_code HOST ORIGIN [HDR…] — a curl-shaped upgrade attempt;
# prints the HTTP status (101 = upgraded; anything else = refused).
# HTTP/1.1 forced: h2 strips Connection/Upgrade, unshaping the request.
hub_upgrade_code() {
	local host=$1 origin=$2
	shift 2
	local -a args=(-sS -o /dev/null -w '%{http_code}' --max-time 5 --http1.1
		-H 'Connection: Upgrade' -H 'Upgrade: websocket'
		-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==')
	if [[ "$origin" != "-" ]]; then
		args+=(-H "Origin: $origin")
	fi
	local h
	for h in "$@"; do
		args+=(-H "$h")
	done
	curl "${args[@]}" "https://$host/hub" 2>/dev/null
}

# start_hub_tenant SOCK APPID… — the recording, scriptable bridge tenant.
# Serves POST {bridge} (records + playbook), answers any other request
# with plain:<path>, and heartbeats every app id given (500ms; TTL is 2s).
start_hub_tenant() {
	local sock=$1
	shift
	rm -f "$sock"
	: >"$HUB_BRIDGE_LOG"
	"$TESTKIT" hubtenant --sock "$sock" --hits "$HUB_BRIDGE_LOG" --playbook "$HUB_PLAYBOOK" "$@" \
		>>"$TEST_RUN_DIR/fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$HUB_PIDS_FILE"
	local i
	for i in $(seq 1 50); do
		[[ -S "$sock" ]] && return 0
		sleep 0.1
	done
	echo "hub tenant socket $sock never appeared" >&2
	return 1
}

# start_hub_wedge HOST IDFILE — a raw client that completes the WebSocket
# handshake and then never reads: the slow-consumer instrument. Its
# connection id is read from the fixture's open-bridge record by the case.
start_hub_wedge() {
	local host=$1
	"$TESTKIT" wedge --host "$host" >>"$TEST_RUN_DIR/fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$HUB_PIDS_FILE"
}

case_hub_setup() {
	: >"$HUB_PIDS_FILE"
	hub_playbook ''

	# Register the three apps FIRST (ids feed the fixture's heartbeater).
	capi POST /1.0/apps '{"name":"hubapp","hosts":["hub1.ripdev.io","hubany.ripdev.io","hubdel.ripdev.io","hubrace.ripdev.io","api.ripdev.io"],"bridge":"/rt/bridge"}'
	eq "$REPLY_CODE" "201"
	printf '%s' "$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')" >"$HUB_APP_FILE"
	capi POST /1.0/apps '{"name":"hubiso","hosts":["hub2.ripdev.io"],"bridge":"/rt/bridge"}'
	eq "$REPLY_CODE" "201"
	printf '%s' "$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')" >"$HUB_ISO_FILE"
	capi POST /1.0/apps '{"name":"hubcap","hosts":["hubten.ripdev.io","hubtwenty.ripdev.io"],"bridge":"/rt/bridge"}'
	eq "$REPLY_CODE" "201"
	printf '%s' "$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')" >"$HUB_CAP_FILE"
	capi POST /1.0/apps '{"name":"hubdirect","hosts":["hubdirect.ripdev.io"],"upstreams":[]}'
	eq "$REPLY_CODE" "201"
	printf '%s' "$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')" >"$HUB_DIRECT_FILE"

	start_hub_tenant "$HUB_SOCK" "$(hub_app_id)" "$(hub_iso_id)" "$(hub_cap_id)" "$(hub_direct_id)" || return 1
	local id
	for id in "$(hub_app_id)" "$(hub_iso_id)" "$(hub_cap_id)"; do
		capi PUT "/1.0/apps/$id/upstreams" "{\"upstreams\":[{\"path\":\"$HUB_SOCK\"}]}"
		eq "$REPLY_CODE" "200"
	done

	# bridge surfaces on GET; /1.0/hub counters answer.
	capi GET "/1.0/apps/$(hub_app_id)"
	json_has "$REPLY_BODY" '"bridge":"/rt/bridge"'
	capi GET /1.0/hub
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"bridge_garbage"'
}

case_hub_direct_no_bridge() {
	local before after
	before="$(wc -l <"$HUB_BRIDGE_LOG" | tr -d ' ')"
	hub_ws hubdirect.ripdev.io - - \
		'send={"+":["/direct"],"echo!":{"ok":true}}' expect=echo close
	after="$(wc -l <"$HUB_BRIDGE_LOG" | tr -d ' ')"
	eq "$after" "$before"
	capi GET "/1.0/apps/$(hub_direct_id)"
	json_has "$REPLY_BODY" '"upstreams":[]'
	if [[ "$REPLY_BODY" == *bridge* ]]; then
		echo "direct app unexpectedly has bridge" >&2
		return 1
	fi
}

case_hub_open_full_path() {
	# Open bridge: headers + admission; tenant enrolls the connection; the
	# snapshot proves membership.
	hub_playbook '{"open":{"status":200,"body":"{\"+\":[\"/room\"]}"}}'
	local b0
	b0="$(hub_bridge_count open)"
	hub_ws hubany.ripdev.io - "sid=42" id pause=200 close
	hub_playbook ''
	ok "$(hub_bridge_count open) -gt $b0" "no open bridge recorded"
	local rec
	rec="$(grep '"kind": "open"' "$HUB_BRIDGE_LOG" | tail -1)"
	json_has "$rec" '"path": "/rt/bridge"'
	json_has "$rec" "\"app\": \"$(hub_app_id)\""
	json_has "$rec" '"cookie": "sid=42"'
	json_has "$rec" '"has_sec_ws_key": false'
	json_has "$rec" '"has_connection": false'
	local wsid
	wsid="$(printf '%s' "$REPLY_WS" | sed -n 's/^ID //p')"
	ok "-n \"$wsid\"" "driver printed no id: $REPLY_WS"
	json_has "$rec" "\"client\": \"$wsid\""
}

case_hub_open_rejected_by_tenant() {
	hub_playbook '{"open":{"status":403,"body":"denied by tenant"}}'
	local resp code c0
	c0="$(hub_stat conns)"
	resp="$(curl -sS --max-time 5 --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
		-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
		-w $'\n%{http_code}' "https://hubany.ripdev.io/hub")"
	hub_playbook ''
	code="${resp##*$'\n'}"
	eq "$code" "403"
	json_has "$resp" 'denied by tenant'
	eq "$(hub_stat conns)" "$c0"
}

case_hub_open_tenant_down() {
	capi PUT "/1.0/apps/$(hub_app_id)/upstreams" '{"upstreams":[]}'
	eq "$REPLY_CODE" "200"
	local hdrs
	hdrs="$(curl -sSI --max-time 5 --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
		-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
		-X GET "https://hubany.ripdev.io/hub" 2>/dev/null | tr -d '\r')"
	capi PUT "/1.0/apps/$(hub_app_id)/upstreams" "{\"upstreams\":[{\"path\":\"$HUB_SOCK\"}]}"
	json_has "$hdrs" '503'
	json_has "$hdrs" 'Retry-After'
}

case_hub_no_bridge() {
	capi PATCH "/1.0/apps/$(hub_app_id)" '{"bridge":null}'
	eq "$REPLY_CODE" "200"
	eq "$(hub_upgrade_code hubany.ripdev.io -)" "503"
	capi PATCH "/1.0/apps/$(hub_app_id)" '{"bridge":"/rt/bridge"}'
	eq "$REPLY_CODE" "200"
}

case_hub_exclusion_rules() {
	# ! rules 1+2: bare name excludes the ORIGINATING CONNECTION only (the
	# same user's other tab still receives); ! includes the sender and the
	# suffix is stripped on delivery.
	rm -f "$TEST_RUN_DIR"/hub-flag-* "$TEST_RUN_DIR"/hub-out-*
	hub_ws_bg "$TEST_RUN_DIR/hub-out-a2" hubany.ripdev.io - "user=A" \
		'send={"+":["/room"]}' touch="$TEST_RUN_DIR/hub-flag-a2" expect=chat expect=fin close
	hub_ws_bg "$TEST_RUN_DIR/hub-out-b" hubany.ripdev.io - "user=B" \
		'send={"+":["/room"]}' touch="$TEST_RUN_DIR/hub-flag-b" expect=chat expect=fin close
	wait_file "$TEST_RUN_DIR/hub-flag-a2"
	wait_file "$TEST_RUN_DIR/hub-flag-b"
	hub_ws hubany.ripdev.io - "user=A" \
		'send={"+":["/room"]}' \
		'send={"@":["/room"],"chat":{"m":1}}' \
		noframe=300 \
		'send={"@":["/room"],"fin!":{}}' \
		expect=fin close
	json_has "$REPLY_WS" 'DONE'
	# The sender received fin (suffix included it) with the ! stripped.
	json_has "$REPLY_WS" '"fin":'
	if printf '%s' "$REPLY_WS" | grep -qF '"fin!"'; then
		echo "suffix leaked to a recipient: $REPLY_WS" >&2
		return 1
	fi
	wait
	json_has "$(cat "$TEST_RUN_DIR/hub-out-a2")" 'DONE'
	json_has "$(cat "$TEST_RUN_DIR/hub-out-b")" 'DONE'
}

case_hub_publish_ignores_exclusion() {
	# ! rule 3: no originating connection on the publish plane.
	rm -f "$TEST_RUN_DIR"/hub-flag-*
	hub_ws_bg "$TEST_RUN_DIR/hub-out-pub" hubany.ripdev.io - - \
		'send={"+":["/pubroom"]}' touch="$TEST_RUN_DIR/hub-flag-pub" expect=news expect=news close
	wait_file "$TEST_RUN_DIR/hub-flag-pub"
	hub_publish "$(hub_app_id)" '{"@":["/pubroom"],"news":{"n":1}}'
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"deliveries":1'
	hub_publish "$(hub_app_id)" '{"@":["/pubroom"],"news!":{"n":2}}'
	json_has "$REPLY_BODY" '"deliveries":1'
	wait
	json_has "$(cat "$TEST_RUN_DIR/hub-out-pub")" 'DONE'
}

case_hub_pong_at_edge() {
	# ! rule 4: exact ? answers {"!": value} from the edge, value verbatim;
	# the frame still reaches the text bridge (observation), and no worker
	# answers the pong (the fixture only records).
	local t0
	t0="$(hub_bridge_count text)"
	hub_ws hubany.ripdev.io - - 'send={"?":"t-12345"}' 'expect={"!":"t-12345"}' close
	json_has "$REPLY_WS" 'RECV {"!":"t-12345"}'
	waitfor_bridge_texts $((t0 + 1))
	local rec
	rec="$(grep '"kind": "text"' "$HUB_BRIDGE_LOG" | tail -1)"
	json_has "$rec" '{\"?\":\"t-12345\"}'
}

# waitfor_bridge_texts N — texts land asynchronously; poll for the count.
waitfor_bridge_texts() {
	local want=$1 i
	for i in $(seq 1 50); do
		if [[ "$(hub_bridge_count text)" -ge "$want" ]]; then
			return 0
		fi
		sleep 0.1
	done
	echo "text bridge count never reached $want" >&2
	return 1
}

case_hub_provenance_stamped() {
	rm -f "$TEST_RUN_DIR"/hub-flag-*
	hub_ws_bg "$TEST_RUN_DIR/hub-out-prov" hubany.ripdev.io - - \
		'send={"+":["/prov"]}' touch="$TEST_RUN_DIR/hub-flag-prov" expect=whisper close
	wait_file "$TEST_RUN_DIR/hub-flag-prov"
	hub_ws hubany.ripdev.io - - id 'send={"@":["/prov"],"whisper":{"w":1}}' close
	local aid
	aid="$(printf '%s' "$REPLY_WS" | sed -n 's/^ID //p')"
	ok "-n \"$aid\"" "no sender id: $REPLY_WS"
	wait
	# The recipient sees "<":[<sender-connection-id>], stamped by the edge.
	json_has "$(cat "$TEST_RUN_DIR/hub-out-prov")" "\"<\":[\"$aid\"]"
}

case_hub_spoof_rejected() {
	local r0
	r0="$(hub_stat rejected_frames)"
	hub_ws hubany.ripdev.io - - 'send={"<":["fake"],"chat":{}}' 'expectclose=1008,stamped by janus'
	json_has "$REPLY_WS" 'CLOSE 1008'
	ok "$(hub_stat rejected_frames) -gt $r0" "rejected_frames not counted"
	# The close is reported to the tenant with the 1008 code (async).
	local i
	for i in $(seq 1 50); do
		if grep '"kind": "close"' "$HUB_BRIDGE_LOG" 2>/dev/null | grep -qF '\"code\":1008'; then
			return 0
		fi
		sleep 0.1
	done
	echo "close bridge with 1008 never recorded" >&2
	return 1
}

case_hub_reserved_sigils_reject() {
	hub_ws hubany.ripdev.io - - 'send={">":["x"],"chat":{}}' 'expectclose=1008,reserved'
	json_has "$REPLY_WS" 'CLOSE 1008'
	hub_ws hubany.ripdev.io - - 'send={"!":"t"}' 'expectclose=1008,janus-to-client'
	json_has "$REPLY_WS" 'CLOSE 1008'
	hub_ws hubany.ripdev.io - - 'send={"*":"bye"}' 'expectclose=1008,delivery-direction'
	json_has "$REPLY_WS" 'CLOSE 1008'
	hub_ws hubany.ripdev.io - - binary 'expectclose=1003,binary'
	json_has "$REPLY_WS" 'CLOSE 1003'
}

case_hub_join_leave_snapshot() {
	# Join/leave bookkeeping through the membership snapshot (the tenant's
	# oracle): default target is the sender (no @ on the joins), the
	# emptied channel disappears.
	rm -f "$TEST_RUN_DIR"/hub-flag-*
	hub_ws_bg "$TEST_RUN_DIR/hub-out-jl" hubany.ripdev.io - - \
		'send={"+":["/jl/a","/jl/b"]}' touch="$TEST_RUN_DIR/hub-flag-jl1" \
		waitfile="$TEST_RUN_DIR/hub-flag-go1" 'send={"-":["/jl/b"]}' \
		touch="$TEST_RUN_DIR/hub-flag-jl2" waitfile="$TEST_RUN_DIR/hub-flag-go2" close
	wait_file "$TEST_RUN_DIR/hub-flag-jl1"
	sleep 0.2
	hub_snapshot "$(hub_app_id)"
	json_has "$REPLY_BODY" '"/jl/a":1'
	json_has "$REPLY_BODY" '"/jl/b":1'
	touch "$TEST_RUN_DIR/hub-flag-go1"
	wait_file "$TEST_RUN_DIR/hub-flag-jl2"
	sleep 0.2
	hub_snapshot "$(hub_app_id)"
	json_has "$REPLY_BODY" '"/jl/a":1'
	if printf '%s' "$REPLY_BODY" | grep -qF '"/jl/b"'; then
		echo "emptied channel still in snapshot: $REPLY_BODY" >&2
		return 1
	fi
	touch "$TEST_RUN_DIR/hub-flag-go2"
	wait
}

case_hub_sender_only_membership() {
	# A client @-ing another connection with + closes 1008 and nothing
	# joins; the trusted publish plane CAN enroll it.
	rm -f "$TEST_RUN_DIR"/hub-flag-*
	hub_ws_bg "$TEST_RUN_DIR/hub-out-victim" hubany.ripdev.io - - \
		id touch="$TEST_RUN_DIR/hub-flag-victim" \
		waitfile="$TEST_RUN_DIR/hub-flag-victim-go" expect=enrolled close
	wait_file "$TEST_RUN_DIR/hub-flag-victim"
	local vid
	for i in $(seq 1 50); do
		vid="$(sed -n 's/^ID //p' "$TEST_RUN_DIR/hub-out-victim" 2>/dev/null)"
		[[ -n "$vid" ]] && break
		sleep 0.1
	done
	ok "-n \"$vid\"" "victim id never appeared"
	hub_ws hubany.ripdev.io - - "send={\"@\":[\"$vid\"],\"+\":[\"/vip\"]}" \
		'expectclose=1008,only the sending connection'
	json_has "$REPLY_WS" 'CLOSE 1008'
	hub_snapshot "$(hub_app_id)"
	if printf '%s' "$REPLY_BODY" | grep -qF '"/vip"'; then
		echo "rejected mutation applied: $REPLY_BODY" >&2
		return 1
	fi
	hub_publish "$(hub_app_id)" "{\"@\":[\"$vid\"],\"+\":[\"/vip\"]}"
	eq "$REPLY_CODE" "200"
	hub_snapshot "$(hub_app_id)"
	json_has "$REPLY_BODY" '"/vip":1'
	touch "$TEST_RUN_DIR/hub-flag-victim-go"
	hub_publish "$(hub_app_id)" "{\"@\":[\"$vid\"],\"enrolled\":1}"
	wait
	json_has "$(cat "$TEST_RUN_DIR/hub-out-victim")" 'DONE'
}

case_hub_channel_grammar_rejects() {
	hub_ws hubany.ripdev.io - - 'send={"+":["room"]}' 'expectclose=1008,want /-prefix'
	json_has "$REPLY_WS" 'CLOSE 1008'
}

case_hub_naming_only_hierarchy() {
	rm -f "$TEST_RUN_DIR"/hub-flag-*
	hub_ws_bg "$TEST_RUN_DIR/hub-out-hier" hubany.ripdev.io - - \
		'send={"+":["/h/sub"]}' touch="$TEST_RUN_DIR/hub-flag-hier" \
		waitfile="$TEST_RUN_DIR/hub-flag-hier-go" close
	wait_file "$TEST_RUN_DIR/hub-flag-hier"
	hub_publish "$(hub_app_id)" '{"@":["/h"],"chat":{}}'
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"deliveries":0'
	json_has "$REPLY_BODY" '"unknown_targets":1'
	touch "$TEST_RUN_DIR/hub-flag-hier-go"
	wait
}

case_hub_per_app_isolation() {
	# Same channel name in two apps: publish into ISO reaches only ISO.
	rm -f "$TEST_RUN_DIR"/hub-flag-*
	hub_ws_bg "$TEST_RUN_DIR/hub-out-iso1" hub1.ripdev.io https://hub1.ripdev.io - \
		'send={"+":["/shared"]}' touch="$TEST_RUN_DIR/hub-flag-iso1" noframe=800 close
	hub_ws_bg "$TEST_RUN_DIR/hub-out-iso2" hub2.ripdev.io https://hub2.ripdev.io - \
		'send={"+":["/shared"]}' touch="$TEST_RUN_DIR/hub-flag-iso2" expect=only close
	wait_file "$TEST_RUN_DIR/hub-flag-iso1"
	wait_file "$TEST_RUN_DIR/hub-flag-iso2"
	hub_publish "$(hub_iso_id)" '{"@":["/shared"],"only":{"iso":1}}'
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"deliveries":1'
	wait
	json_has "$(cat "$TEST_RUN_DIR/hub-out-iso1")" 'DONE'
	json_has "$(cat "$TEST_RUN_DIR/hub-out-iso2")" 'DONE'
}

case_hub_text_bridge_observation() {
	# The driver holds its socket open (waitfile) until the texts land:
	# a local close discards queued bridge texts by design (at-most-once).
	local t0
	t0="$(hub_bridge_count text)"
	rm -f "$TEST_RUN_DIR/hub-flag-obs-go"
	hub_ws_bg "$TEST_RUN_DIR/hub-out-obs" hubany.ripdev.io - - \
		'send={"obs1!":{"a": 1}}' expect=obs1 \
		'send={"obs2!":{"b" :2}}' expect=obs2 \
		'send={"obs3!":{"c":3}}' expect=obs3 \
		waitfile="$TEST_RUN_DIR/hub-flag-obs-go" close
	waitfor_bridge_texts $((t0 + 3))
	touch "$TEST_RUN_DIR/hub-flag-obs-go"
	wait
	json_has "$(cat "$TEST_RUN_DIR/hub-out-obs")" 'DONE'
	# In order, bodies byte-identical to the wire frames (spacing kept).
	local texts
	texts="$(grep '"kind": "text"' "$HUB_BRIDGE_LOG" | tail -3)"
	printf '%s' "$texts" | head -1 | grep -qF '{\"obs1!\":{\"a\": 1}}' || {
		echo "first text not verbatim: $(printf '%s' "$texts" | head -1)" >&2
		return 1
	}
	printf '%s' "$texts" | sed -n 2p | grep -qF '{\"obs2!\":{\"b\" :2}}' || {
		echo "second text not verbatim" >&2
		return 1
	}
}

case_hub_bridge_response_directives() {
	# The tenant answers a text with directives: they execute in the
	# SENDER's context — a bare name excludes the sender, other members
	# of /room receive.
	rm -f "$TEST_RUN_DIR"/hub-flag-*
	hub_playbook '{"text":{"status":200,"body":"{\"@\":[\"/brt\"],\"note\":{\"from\":\"tenant\"}}"}}'
	hub_ws_bg "$TEST_RUN_DIR/hub-out-brt" hubany.ripdev.io - - \
		'send={"+":["/brt"]}' touch="$TEST_RUN_DIR/hub-flag-brt" expect=note close
	wait_file "$TEST_RUN_DIR/hub-flag-brt"
	hub_ws hubany.ripdev.io - - 'send={"+":["/brt"]}' 'send={"trigger!":1}' expect=trigger noframe=700 close
	hub_playbook ''
	json_has "$REPLY_WS" 'DONE'
	wait
	json_has "$(cat "$TEST_RUN_DIR/hub-out-brt")" '"note":{"from":"tenant"}'
}

case_hub_bridge_garbage() {
	local g0
	g0="$(hub_stat bridge_garbage)"
	hub_playbook '{"text":{"status":200,"body":"this is not json"}}'
	hub_ws hubany.ripdev.io - - 'send={"garb!":1}' expect=garb 'send={"?":"alive"}' 'expect={"!":"alive"}' close
	hub_playbook ''
	json_has "$REPLY_WS" 'DONE'
	local i
	for i in $(seq 1 50); do
		[[ "$(hub_stat bridge_garbage)" -gt "$g0" ]] && return 0
		sleep 0.1
	done
	echo "bridge_garbage never counted" >&2
	return 1
}

case_hub_atomic_rejection() {
	# A frame with a bare/suffixed collision rejects WHOLE: the join in
	# its first object never applies.
	hub_ws hubany.ripdev.io - - \
		'send=[{"+":["/atomic"]},{"@":["/atomic"],"chat":{},"chat!":{}}]' \
		'expectclose=1008,appears as both'
	json_has "$REPLY_WS" 'CLOSE 1008'
	hub_snapshot "$(hub_app_id)"
	if printf '%s' "$REPLY_BODY" | grep -qF '"/atomic"'; then
		echo "rejected frame applied its join: $REPLY_BODY" >&2
		return 1
	fi
}

case_hub_text_failure_invisible() {
	# Tenant 500s texts: edge fan-out unaffected, sockets stay open,
	# bridge_failed counts.
	local f0
	f0="$(hub_stat bridge_failed)"
	hub_playbook '{"text":{"status":500,"body":"boom"}}'
	rm -f "$TEST_RUN_DIR"/hub-flag-*
	hub_ws_bg "$TEST_RUN_DIR/hub-out-tf" hubany.ripdev.io - - \
		'send={"+":["/tf"]}' touch="$TEST_RUN_DIR/hub-flag-tf" expect=still close
	wait_file "$TEST_RUN_DIR/hub-flag-tf"
	hub_ws hubany.ripdev.io - - 'send={"@":["/tf"],"still":{"up":1}}' 'send={"?":"ok"}' 'expect={"!":"ok"}' close
	hub_playbook ''
	json_has "$REPLY_WS" 'DONE'
	wait
	json_has "$(cat "$TEST_RUN_DIR/hub-out-tf")" 'DONE'
	local i
	for i in $(seq 1 50); do
		[[ "$(hub_stat bridge_failed)" -gt "$f0" ]] && return 0
		sleep 0.1
	done
	echo "bridge_failed never counted" >&2
	return 1
}

case_hub_reload_invisibility() {
	# Doorbell PUT (admission cut) + dirty window: the socket stays open,
	# membership stays, fan-out works mid-window; the held text bridge
	# completes once the pool publishes.
	rm -f "$TEST_RUN_DIR"/hub-flag-* "$TEST_RUN_DIR/hub-ring"
	hub_ws_bg "$TEST_RUN_DIR/hub-out-rl" hubany.ripdev.io - - \
		'send={"+":["/rl"]}' touch="$TEST_RUN_DIR/hub-flag-rl" \
		expect=midwindow 'send={"heldtext!":1}' expect=heldtext \
		'send={"?":"post-reload"}' 'expect={"!":"post-reload"}' \
		waitfile="$TEST_RUN_DIR/hub-flag-rl-go" close
	local driver_pid=$HUB_WS_PID
	wait_file "$TEST_RUN_DIR/hub-flag-rl"
	local t0
	t0="$(hub_bridge_count text)"
	# Admission cut: the doorbell (rings back to the real fixture sock).
	start_data_doorbell "$TEST_RUN_DIR/run/hub-bell.sock" "$(hub_app_id)" "$HUB_SOCK" "$TEST_RUN_DIR/hub-ring" || return 1
	capi PUT "/1.0/apps/$(hub_app_id)/upstreams" \
		"{\"upstreams\":[{\"path\":\"$TEST_RUN_DIR/run/hub-bell.sock\",\"doorbell\":true}]}"
	eq "$REPLY_CODE" "200"
	# Mid-window: fan-out rides above the worker plane.
	hub_publish "$(hub_app_id)" '{"@":["/rl"],"midwindow":{}}'
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"deliveries":1'
	# The client's next frame executes at the edge immediately (heldtext!
	# loops back) while its text bridge rings the doorbell, which restores
	# the pool; the held POST then completes against the fresh worker.
	waitfor_bridge_texts $((t0 + 1))
	ok "-s \"$TEST_RUN_DIR/hub-ring\"" "text bridge never rang the doorbell"
	touch "$TEST_RUN_DIR/hub-flag-rl-go"
	# Wait for the driver only: the doorbell fixture serves forever.
	wait "$driver_pid"
	json_has "$(cat "$TEST_RUN_DIR/hub-out-rl")" 'DONE'
}

case_hub_teardown_on_delete() {
	# DELETE tears the hub down: sockets close 1001 "app deregistered";
	# the snapshot 404s once the registration is gone.
	capi POST /1.0/apps '{"name":"hubdel","hosts":["hubdel2.ripdev.io"],"bridge":"/rt/bridge"}'
	eq "$REPLY_CODE" "201"
	local del_id
	del_id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	capi PUT "/1.0/apps/$del_id/upstreams" "{\"upstreams\":[{\"path\":\"$HUB_SOCK\"}]}"
	rm -f "$TEST_RUN_DIR"/hub-flag-*
	hub_ws_bg "$TEST_RUN_DIR/hub-out-del" hubdel2.ripdev.io https://hubdel2.ripdev.io - \
		'send={"+":["/dying"]}' touch="$TEST_RUN_DIR/hub-flag-del" \
		'expectclose=1001,app deregistered'
	wait_file "$TEST_RUN_DIR/hub-flag-del"
	sleep 0.2
	capi DELETE "/1.0/apps/$del_id"
	eq "$REPLY_CODE" "204"
	wait
	json_has "$(cat "$TEST_RUN_DIR/hub-out-del")" 'CLOSE 1001'
	hub_snapshot "$del_id"
	eq "$REPLY_CODE" "404"
}

case_hub_slow_consumer() {
	# A recipient that never reads: the outbound queue caps trip and the
	# connection closes 1013 — the sender (publish) is unaffected.
	local s0 o0
	s0="$(hub_stat slow_closes)"
	o0="$(hub_bridge_count open)"
	start_hub_wedge hubany.ripdev.io
	local i wid=""
	for i in $(seq 1 50); do
		if [[ "$(hub_bridge_count open)" -gt "$o0" ]]; then
			wid="$(grep '"kind": "open"' "$HUB_BRIDGE_LOG" | tail -1 |
				"$TESTKIT" json get client)"
			break
		fi
		sleep 0.1
	done
	ok "-n \"$wid\"" "wedge connection never opened"
	# Flood: 30 × ~100KB. The kernel socket buffers absorb the first few;
	# the writer then wedges and the 1MiB outbound queue overflows well
	# before the write deadline.
	local blob
	blob="$("$TESTKIT" repeat x 100000)"
	for i in $(seq 1 30); do
		hub_publish "$(hub_app_id)" "{\"@\":[\"$wid\"],\"flood\":\"$blob\"}"
		eq "$REPLY_CODE" "200"
	done
	for i in $(seq 1 100); do
		[[ "$(hub_stat slow_closes)" -gt "$s0" ]] && break
		sleep 0.1
	done
	ok "$(hub_stat slow_closes) -gt $s0" "slow consumer never closed"
	# The close bridge reports 1013 for that connection. The Close frame
	# rides a 10s write deadline into a socket the wedge never drains, so
	# the bridge record can trail the counter by that full deadline.
	for i in $(seq 1 150); do
		if grep '"kind": "close"' "$HUB_BRIDGE_LOG" | grep -qF "\"client\": \"$wid\""; then
			break
		fi
		sleep 0.1
	done
	grep '"kind": "close"' "$HUB_BRIDGE_LOG" | grep -F "\"client\": \"$wid\"" | tail -1 | grep -qF '1013' || {
		echo "close bridge for the wedged conn lacks 1013" >&2
		return 1
	}
}

case_hub_oversize_frame() {
	hub_ws hubany.ripdev.io - - sendbig=70000 'expectclose=1009'
	json_has "$REPLY_WS" 'CLOSE 1009'
}

case_hub_cap_floor_and_reservation() {
	# One app spans hosts capped 10 and 20: the effective floor is 10 —
	# enforced with slot reservation while open bridges are in flight,
	# even arriving through the 20-capped host.
	local release="$TEST_RUN_DIR/hub-flag-cap-release" i pid pids="" opens0
	rm -f "$release" "$TEST_RUN_DIR"/hub-flag-cap-open-*
	opens0="$(hub_bridge_count open)"
	hub_playbook "{\"open\":{\"status\":204,\"wait_file\":\"$release\"}}"
	for i in $(seq 1 10); do
		hub_ws_bg "$TEST_RUN_DIR/hub-out-cap-$i" hubtwenty.ripdev.io - - \
			"touch=$TEST_RUN_DIR/hub-flag-cap-open-$i"
		pids="$pids $HUB_WS_PID"
	done
	# Wait for all ten bridge requests, which hold their reservations until
	# released. Process launch order and DNS/TLS latency cannot order arrivals.
	for i in $(seq 1 100); do
		[[ "$(hub_bridge_count open)" -eq "$((opens0 + 10))" ]] && break
		sleep 0.05
	done
	eq "$(hub_bridge_count open)" "$((opens0 + 10))"
	# All ten slots reserved: the 11th rejects 503 immediately.
	eq "$(hub_upgrade_code hubtwenty.ripdev.io -)" "503"
	touch "$release"
	for pid in $pids; do wait "$pid"; done
	hub_playbook ''
	# The ten held handshakes completed (101) — reservation ≠ rejection.
	for i in $(seq 1 10); do wait_file "$TEST_RUN_DIR/hub-flag-cap-open-$i"; done
	# The drivers exit after their successful handshakes, releasing every slot.
	for i in $(seq 1 100); do
		hub_snapshot "$(hub_cap_id)"
		if printf '%s' "$REPLY_BODY" | grep -qF '"conns":0'; then
			return 0
		fi
		sleep 0.1
	done
	echo "cap-app conns never drained: $REPLY_BODY" >&2
	return 1
}

case_hub_open_teardown_race() {
	# DELETE while an open bridge is pending: the returning 2xx sees the
	# tombstone — no 101, no zombie connection.
	capi POST /1.0/apps '{"name":"hubrace","hosts":["hubrace2.ripdev.io"],"bridge":"/rt/bridge"}'
	eq "$REPLY_CODE" "201"
	local race_id
	race_id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	capi PUT "/1.0/apps/$race_id/upstreams" "{\"upstreams\":[{\"path\":\"$HUB_SOCK\"}]}"
	hub_playbook '{"open":{"status":204,"delay_ms":1500}}'
	local code_file="$TEST_RUN_DIR/hub-flag-racecode"
	rm -f "$code_file"
	(hub_upgrade_code hubrace2.ripdev.io https://hubrace2.ripdev.io >"$code_file") &
	sleep 0.4
	capi DELETE "/1.0/apps/$race_id"
	eq "$REPLY_CODE" "204"
	wait
	hub_playbook ''
	eq "$(cat "$code_file")" "503"
	capi GET /1.0/hub
	if printf '%s' "$REPLY_BODY" | grep -qF "\"$race_id\":{\"conns\":1"; then
		echo "zombie connection after teardown race" >&2
		return 1
	fi
}

case_hub_origin_policy() {
	# origin same (hub1 inherits the global default): no Origin and
	# wrong-Origin fail 403 BEFORE any bridge; matching Origin admits.
	local o0
	o0="$(hub_bridge_count open)"
	eq "$(hub_upgrade_code hub1.ripdev.io -)" "403"
	eq "$(hub_upgrade_code hub1.ripdev.io https://evil.example.com)" "403"
	eq "$(hub_bridge_count open)" "$o0"
	eq "$(hub_upgrade_code hub1.ripdev.io https://hub1.ripdev.io)" "101"
	# origin any admits an Origin-less client (proven throughout by the
	# driver, pinned here).
	eq "$(hub_upgrade_code hubany.ripdev.io -)" "101"
}

case_hub_interception_scope() {
	# The hub claims upgrades only: a plain GET to the hub path flows
	# through the data plane to the tenant; an upgrade on a hub-off site
	# (api.ripdev.io) is never intercepted.
	eq "$(http_body https://hubany.ripdev.io/hub)" "plain:/hub"
	local body
	body="$(curl -sS --max-time 5 --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
		-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
		"https://api.ripdev.io/hub")"
	eq "$body" "plain:/hub"
}

case_hub_publish_plane_errors() {
	# Absent @ → 400 positioned; empty @ → 400; unknown app → 404;
	# hub-less app → 409.
	hub_publish "$(hub_app_id)" '{"chat":{}}'
	eq "$REPLY_CODE" "400"
	json_has "$REPLY_BODY" 'item 0'
	json_has "$REPLY_BODY" 'required on the publish plane'
	hub_publish "$(hub_app_id)" '{"@":[],"chat":{}}'
	eq "$REPLY_CODE" "400"
	hub_publish "nope-zzzzzz" '{"@":["/x"],"chat":{}}'
	eq "$REPLY_CODE" "404"
	capi POST /1.0/apps '{"name":"hubless","hosts":["hubless.example.com"]}'
	eq "$REPLY_CODE" "201"
	local hubless_id
	hubless_id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	hub_publish "$hubless_id" '{"@":["/x"],"chat":{}}'
	eq "$REPLY_CODE" "409"
	json_has "$REPLY_BODY" 'not enabled for any site'
	capi DELETE "/1.0/apps/$hubless_id"
}

case_hub_publish_kick() {
	rm -f "$TEST_RUN_DIR"/hub-flag-*
	hub_ws_bg "$TEST_RUN_DIR/hub-out-kick" hubany.ripdev.io - - \
		id touch="$TEST_RUN_DIR/hub-flag-kick" 'expect={"*":"kicked"}' 'expectclose=1000,kicked'
	wait_file "$TEST_RUN_DIR/hub-flag-kick"
	local kid i
	for i in $(seq 1 50); do
		kid="$(sed -n 's/^ID //p' "$TEST_RUN_DIR/hub-out-kick" 2>/dev/null)"
		[[ -n "$kid" ]] && break
		sleep 0.1
	done
	ok "-n \"$kid\"" "kick target id never appeared"
	local c0
	c0="$(hub_bridge_count close)"
	hub_publish "$(hub_app_id)" "{\"@\":[\"$kid\"],\"*\":\"kicked\"}"
	eq "$REPLY_CODE" "200"
	wait
	json_has "$(cat "$TEST_RUN_DIR/hub-out-kick")" 'CLOSE 1000 kicked'
	waitfor_bridge_close $((c0 + 1))
}

waitfor_bridge_close() {
	local want=$1 i
	for i in $(seq 1 50); do
		[[ "$(hub_bridge_count close)" -ge "$want" ]] && return 0
		sleep 0.1
	done
	echo "close bridge count never reached $want" >&2
	return 1
}

case_hub_caddy_reload_persistence() {
	# A cold config reload: the socket, its id, its membership, and
	# fan-out all survive through the pooled registry/hub state.
	rm -f "$TEST_RUN_DIR"/hub-flag-*
	hub_ws_bg "$TEST_RUN_DIR/hub-out-crl" hubany.ripdev.io - - \
		'send={"+":["/keep"]}' touch="$TEST_RUN_DIR/hub-flag-crl" \
		expect=survived 'send={"?":"post"}' 'expect={"!":"post"}' close
	wait_file "$TEST_RUN_DIR/hub-flag-crl"
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
	# Membership survived the reload.
	hub_snapshot "$(hub_app_id)"
	json_has "$REPLY_BODY" '"/keep":1'
	# Fan-out still works on the same socket.
	hub_publish "$(hub_app_id)" '{"@":["/keep"],"survived":{}}'
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"deliveries":1'
	wait
	json_has "$(cat "$TEST_RUN_DIR/hub-out-crl")" 'DONE'
}

case_hub_bridge_snapshot_cap() {
	# >32 KiB of filtered handshake headers → 431, never truncated.
	local big
	big="c=$("$TESTKIT" repeat x 33000)"
	eq "$(hub_upgrade_code hubany.ripdev.io - "Cookie: $big")" "431"
}

case_hub_snapshot_opacity() {
	# Snapshot handles are opaque (conn-N), never raw connection ids.
	rm -f "$TEST_RUN_DIR"/hub-flag-*
	hub_ws_bg "$TEST_RUN_DIR/hub-out-op" hubany.ripdev.io - - \
		id touch="$TEST_RUN_DIR/hub-flag-op" waitfile="$TEST_RUN_DIR/hub-flag-op-go" close
	wait_file "$TEST_RUN_DIR/hub-flag-op"
	local oid i
	for i in $(seq 1 50); do
		oid="$(sed -n 's/^ID //p' "$TEST_RUN_DIR/hub-out-op" 2>/dev/null)"
		[[ -n "$oid" ]] && break
		sleep 0.1
	done
	ok "-n \"$oid\"" "opacity conn id never appeared"
	hub_snapshot "$(hub_app_id)"
	if printf '%s' "$REPLY_BODY" | grep -qF "$oid"; then
		echo "snapshot leaked a raw connection id: $REPLY_BODY" >&2
		return 1
	fi
	json_has "$REPLY_BODY" '"conn-'
	touch "$TEST_RUN_DIR/hub-flag-op-go"
	wait
}

case_hub_parse_rejections() {
	local bad dir
	dir="$(mktemp -d "$TEST_RUN_DIR/janus-hub-parse.XXXXXX")"
	local -a cases=(
		'hub maybe'
		'hub off { path /x }'
		'hub { bogus 1 }'
		'hub { path }'
		'hub { path relative }'
		'hub { path "/x?y" }'
		'hub { max_conns 0 }'
		'hub { max_conns many }'
		'hub { max_channels 0 }'
		'hub { max_frame 512b }'
		'hub { origin }'
		'hub { origin any same }'
		'hub { origin "not a host" }'
		'hub { path /x
			path /y }'
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
			echo "caddy adapt accepted illegal hub config: $bad" >&2
			rm -rf "$dir"
			return 1
		fi
	done
	rm -rf "$dir"
}
