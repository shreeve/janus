# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: data ------------------------------------------------------------

# Case bodies run inside the runner's $( ) subshell, so fixture bookkeeping
# must go through files (like APP_ID_FILE) — array appends in a subshell
# never reach the parent, and the cleaner would leak every fixture server.
DATA_APP_FILE="$TEST_RUN_DIR/data-app-id"
DATA_PIDS_FILE="$TEST_RUN_DIR/data-pids"
DATA_SOCKS_FILE="$TEST_RUN_DIR/data-socks"
DATA_HITFILES_FILE="$TEST_RUN_DIR/data-files"

data_app_id() {
	cat "$DATA_APP_FILE"
}

# start_data_upstream SOCK NAME HITFILE — HTTP/1.1 echo server on a unix socket.
# GET / → "upstream:NAME"; POST → append body to HITFILE, echo "received:BODY".
start_data_upstream() {
	local sock=$1 name=$2 hitfile=$3
	rm -f "$sock"
	: >"$hitfile"
	printf '%s\n' "$sock" >>"$DATA_SOCKS_FILE"
	printf '%s\n' "$hitfile" >>"$DATA_HITFILES_FILE"
	# detach stdout/stderr: the runner captures case output via $( ) and
	# would otherwise wait for this background server to exit
	"$TESTKIT" upstream --sock "$sock" --name "$name" --hits "$hitfile" \
		>>"$TEST_RUN_DIR/fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$DATA_PIDS_FILE"
	local i
	for i in $(seq 1 50); do
		[[ -S "$sock" ]] && return 0
		sleep 0.1
	done
	echo "upstream socket $sock never appeared" >&2
	return 1
}

# start_data_doorbell SOCK APPID NEWSOCK RINGFILE — on GET /ring, PUT NEWSOCK
# as the app's real upstream via /1.0 (awaits the 200), then answer 204.
start_data_doorbell() {
	local sock=$1 appid=$2 newsock=$3 ringfile=$4
	rm -f "$sock"
	: >"$ringfile"
	printf '%s\n' "$sock" >>"$DATA_SOCKS_FILE"
	printf '%s\n' "$ringfile" >>"$DATA_HITFILES_FILE"
	"$TESTKIT" doorbell --sock "$sock" --app "$appid" --newsock "$newsock" --ring "$ringfile" \
		>>"$TEST_RUN_DIR/fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$DATA_PIDS_FILE"
	local i
	for i in $(seq 1 50); do
		[[ -S "$sock" ]] && return 0
		sleep 0.1
	done
	echo "doorbell socket $sock never appeared" >&2
	return 1
}

stop_data_fixtures() {
	local pid f path
	if [[ -f "$DATA_PIDS_FILE" ]]; then
		while read -r pid; do
			stop_owned_pid "$pid" "$TESTKIT"
		done <"$DATA_PIDS_FILE"
	fi
	for f in "$DATA_SOCKS_FILE" "$DATA_HITFILES_FILE"; do
		if [[ -f "$f" ]]; then
			while read -r path; do
				rm -f "$path"
			done <"$f"
		fi
	done
	rm -f "$DATA_PIDS_FILE" "$DATA_SOCKS_FILE" "$DATA_HITFILES_FILE" "$DATA_APP_FILE"
}

case_data_register_with_upstream() {
	start_data_upstream "$TEST_RUN_DIR/run/u1.sock" u1 "$TEST_RUN_DIR/u1.hits" || return 1
	capi POST /1.0/apps '{"name":"web","hosts":["app.ripdev.io"]}'
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	ok "-n \"$id\"" "no id in $REPLY_BODY"
	printf '%s' "$id" >"$DATA_APP_FILE"
	capi PUT "/1.0/apps/$id/upstreams" "{\"upstreams\":[{\"path\":\"$TEST_RUN_DIR/run/u1.sock\"}]}"
	eq "$REPLY_CODE" "200"
}

case_data_proxy_get() {
	eq "$(http_code https://app.ripdev.io/)" "200"
	eq "$(http_body https://app.ripdev.io/)" "upstream:u1"
}

case_data_proxy_post_body() {
	local body
	body="$(curl -sS --max-time 5 -X POST --data 'alpha' https://app.ripdev.io/submit)"
	eq "$body" "received:alpha"
	eq "$(wc -l <"$TEST_RUN_DIR/u1.hits" | tr -d ' ')" "1"
}

case_data_unknown_host() {
	eq "$(http_code https://nowhere.ripdev.io/)" "404"
}

case_data_empty_upstreams_503() {
	capi PUT "/1.0/apps/$(data_app_id)/upstreams" '{"upstreams":[]}'
	eq "$REPLY_CODE" "200"
	eq "$(http_code https://app.ripdev.io/)" "503"
	local ra
	ra="$(curl -sS -o /dev/null -D - --max-time 5 https://app.ripdev.io/ |
		tr -d '\r' | awk -F': ' 'tolower($1)=="retry-after" {print $2}')"
	eq "$ra" "1"
}

case_data_doorbell_ring() {
	start_data_upstream "$TEST_RUN_DIR/run/u2.sock" u2 "$TEST_RUN_DIR/u2.hits" || return 1
	start_data_doorbell "$TEST_RUN_DIR/run/bell.sock" "$(data_app_id)" "$TEST_RUN_DIR/run/u2.sock" "$TEST_RUN_DIR/bell.rings" || return 1
	capi PUT "/1.0/apps/$(data_app_id)/upstreams" \
		"{\"upstreams\":[{\"path\":\"$TEST_RUN_DIR/run/bell.sock\",\"doorbell\":true}]}"
	eq "$REPLY_CODE" "200"

	# Client POST with a body while only the doorbell is published:
	# the ring swaps in u2 and the body arrives there intact, exactly once,
	# with no visible redirect.
	local resp code body
	resp="$(curl -sS --max-time 20 -X POST --data 'door-payload' \
		-w $'\n%{http_code} %{num_redirects}' https://app.ripdev.io/submit)"
	code="${resp##*$'\n'}"
	body="${resp%$'\n'*}"
	eq "$code" "200 0"
	eq "$body" $'received:door-payload\n'
	eq "$(wc -l <"$TEST_RUN_DIR/u2.hits" | tr -d ' ')" "1"
	eq "$(cat "$TEST_RUN_DIR/u2.hits")" "door-payload"
	eq "$(wc -l <"$TEST_RUN_DIR/bell.rings" | tr -d ' ')" "1"
	eq "$(wc -l <"$TEST_RUN_DIR/u1.hits" | tr -d ' ')" "1" # old upstream got nothing new
}

case_data_after_ring_steady_state() {
	# The doorbell is retired; traffic flows to u2 without ringing again.
	eq "$(http_body https://app.ripdev.io/)" "upstream:u2"
	eq "$(wc -l <"$TEST_RUN_DIR/bell.rings" | tr -d ' ')" "1"
}

case_data_ping_still_answers() {
	# Site-scoped ping (global on) answers ahead of routing for this host.
	eq "$(http_body https://app.ripdev.io/ping)" "pong"
}
