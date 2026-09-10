# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: apps -----------------------------------------------------------

APP_ID_FILE="$TEST_RUN_DIR/app-id"

# capi METHOD PATH [JSON] — control API over local TCP; sets REPLY_CODE / REPLY_BODY
capi() {
	local method=$1 path=$2 data=${3:-} resp
	if [[ -n "$data" ]]; then
		resp="$(curl -sS --max-time 5 -X "$method" -H 'Content-Type: application/json' \
			--data "$data" -w $'\n%{http_code}' "http://127.0.0.1:7600$path")"
	else
		resp="$(curl -sS --max-time 5 -X "$method" -w $'\n%{http_code}' "http://127.0.0.1:7600$path")"
	fi
	REPLY_CODE="${resp##*$'\n'}"
	REPLY_BODY="${resp%$'\n'*}"
}

# capi_unix METHOD PATH [JSON] — same, over the internal unix socket
capi_unix() {
	local method=$1 path=$2 data=${3:-} resp
	if [[ -n "$data" ]]; then
		resp="$(curl -sS --max-time 5 --unix-socket "$TEST_RUN_DIR/run/janus.sock" -X "$method" \
			-H 'Content-Type: application/json' --data "$data" -w $'\n%{http_code}' "http://janus$path")"
	else
		resp="$(curl -sS --max-time 5 --unix-socket "$TEST_RUN_DIR/run/janus.sock" -X "$method" \
			-w $'\n%{http_code}' "http://janus$path")"
	fi
	REPLY_CODE="${resp##*$'\n'}"
	REPLY_BODY="${resp%$'\n'*}"
}

app_id() {
	cat "$APP_ID_FILE"
}

case_apps_register() {
	capi POST /1.0/apps '{"name":"shop","hosts":["shop.example.com"]}'
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	if [[ ! "$id" =~ ^shop-[a-z0-9]{6}$ ]]; then
		printf 'id %q does not match shop-xxxxxx in %q' "$id" "$REPLY_BODY" >&2
		return 1
	fi
	printf '%s' "$id" >"$APP_ID_FILE"
}

case_apps_register_bad() {
	capi POST /1.0/apps '{"hosts":["a.example.com"]}'
	eq "$REPLY_CODE" "400"
	capi POST /1.0/apps '{"name":"shop2","hosts":[]}'
	eq "$REPLY_CODE" "400"
	capi POST /1.0/apps '{"name":"shop2","hosts":["not a host"]}'
	eq "$REPLY_CODE" "400"
	capi POST /1.0/apps 'not json'
	eq "$REPLY_CODE" "400"
}

case_apps_host_conflict() {
	capi POST /1.0/apps '{"name":"rival","hosts":["shop.example.com"]}'
	eq "$REPLY_CODE" "409"
	json_has "$REPLY_BODY" 'shop.example.com'
	json_has "$REPLY_BODY" "$(app_id)"
}

case_apps_list() {
	capi GET /1.0/apps
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" "\"$(app_id)\""
}

case_apps_get() {
	capi GET "/1.0/apps/$(app_id)"
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" '"name":"shop"'
	json_has "$REPLY_BODY" '"shop.example.com"'
}

case_apps_get_unknown() {
	capi GET /1.0/apps/shop-zzzzzz
	eq "$REPLY_CODE" "404"
}

case_apps_unix_sees_registry() {
	capi_unix GET /1.0/apps
	eq "$REPLY_CODE" "200"
	json_has "$REPLY_BODY" "\"$(app_id)\""
}

case_apps_put_upstreams() {
	capi PUT "/1.0/apps/$(app_id)/upstreams" \
		'{"upstreams":[{"path":"/run/w1.sock"},{"path":"/run/w2.sock"}]}'
	eq "$REPLY_CODE" "200"
	capi GET "/1.0/apps/$(app_id)"
	json_has "$REPLY_BODY" '"/run/w1.sock"'
	json_has "$REPLY_BODY" '"/run/w2.sock"'
}

case_apps_put_upstreams_empty() {
	capi PUT "/1.0/apps/$(app_id)/upstreams" '{"upstreams":[]}'
	eq "$REPLY_CODE" "200"
	capi GET "/1.0/apps/$(app_id)"
	json_has "$REPLY_BODY" '"upstreams":[]'
}

case_apps_put_upstreams_mixed_doorbell() {
	capi PUT "/1.0/apps/$(app_id)/upstreams" \
		'{"upstreams":[{"path":"/run/bell.sock","doorbell":true},{"path":"/run/w1.sock"}]}'
	eq "$REPLY_CODE" "400"
	json_has "$REPLY_BODY" 'doorbell'
}

case_apps_initial_upstreams() {
	capi POST /1.0/apps '{"name":"initialempty","hosts":["initialempty.example.com"],"upstreams":[]}'
	eq "$REPLY_CODE" "201"
	local empty_id
	empty_id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	capi GET "/1.0/apps/$empty_id"
	json_has "$REPLY_BODY" '"upstreams":[]'
	capi DELETE "/1.0/apps/$empty_id"
	eq "$REPLY_CODE" "204"

	capi POST /1.0/apps '{"name":"initialworker","hosts":["initialworker.example.com"],"upstreams":[{"path":"/run/initial.sock"}]}'
	eq "$REPLY_CODE" "201"
	local worker_id
	worker_id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	capi GET "/1.0/apps/$worker_id"
	json_has "$REPLY_BODY" '"/run/initial.sock"'
	capi PUT "/1.0/apps/$worker_id/upstreams" '{}'
	eq "$REPLY_CODE" "400"
	capi DELETE "/1.0/apps/$worker_id"
	eq "$REPLY_CODE" "204"

	capi POST /1.0/apps \
		'{"name":"initialbad","hosts":["initialbad.example.com"],"upstreams":[{"path":"/run/bell.sock","doorbell":true},{"path":"/run/w.sock"}]}'
	eq "$REPLY_CODE" "400"
}

case_apps_delete() {
	capi DELETE "/1.0/apps/$(app_id)"
	eq "$REPLY_CODE" "204"
	capi GET "/1.0/apps/$(app_id)"
	eq "$REPLY_CODE" "404"
}

case_apps_register_survivor() {
	# Register an app that exists when Janus restarts; it must not survive.
	capi POST /1.0/apps '{"name":"ghost","hosts":["ghost.example.com"]}'
	eq "$REPLY_CODE" "201"
}

case_apps_empty_after_restart() {
	capi GET /1.0/apps
	eq "$REPLY_CODE" "200"
	eq "$(printf '%s' "$REPLY_BODY" | tr -d '[:space:]')" "[]"
}
