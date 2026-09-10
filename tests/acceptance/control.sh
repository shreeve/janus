# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: control -------------------------------------------------------

json_has() {
	local body=$1 needle=$2
	if [[ "$body" != *"$needle"* ]]; then
		printf 'missing %q in %q' "$needle" "$body" >&2
		return 1
	fi
}

case_config_environment() {
	local dir="$TEST_RUN_DIR/env" out
	mkdir -p "$dir"
	printf 'http://example.test:8098 {\n respond "{$JANUS_TEST_ENV}"\n}\n' >"$dir/Caddyfile"
	printf 'JANUS_TEST_ENV="file\nvalue"\n' >"$dir/env"
	out="$(JANUS_TEST_ENV=shell "$CADDY_BIN" adapt --config "$dir/Caddyfile" --envfile "$dir/env" 2>/dev/null)"
	[[ "$out" == *'"body":"shell"'* ]] || return 1
	printf 'JANUS_BIND=0.0.0.0\n' >>"$dir/env"
	for verb in run adapt validate reload; do
		if "$CADDY_BIN" "$verb" --config "$dir/Caddyfile" --envfile "$dir/env" >"$dir/rejected" 2>&1; then
			echo "$verb accepted reserved JANUS_BIND" >&2
			return 1
		fi
		[[ "$(cat "$dir/rejected")" == *'janus mode'* ]] || return 1
	done
}

case_control_duplicate_json() {
	capi POST /1.0/apps '{"name":"first","name":"second","hosts":["duplicate.ripdev.io"]}'
	eq "$REPLY_CODE" "400"
	json_has "$REPLY_BODY" 'appears twice'
	capi POST /1.0/apps '{"name":"nested","hosts":["duplicate.ripdev.io"],"files":{"roots":[{"path":"/one","path":"/two"}]}}'
	eq "$REPLY_CODE" "400"
	json_has "$REPLY_BODY" 'appears twice'
}

case_control_local_root() {
	local body
	body="$(http_body http://127.0.0.1:7600/1.0)"
	eq "$(http_code http://127.0.0.1:7600/1.0)" "200"
	json_has "$body" '"api_version":"1.0"'
	json_has "$body" '"type":"janus"'
}

case_control_local_health() {
	local body
	body="$(http_body http://127.0.0.1:7600/1.0/health)"
	eq "$(http_code http://127.0.0.1:7600/1.0/health)" "200"
	json_has "$body" '"status":"ok"'
}

case_control_unix_root() {
	ok "-S \"$TEST_RUN_DIR/run/janus.sock\"" "missing unix socket"
	local body code
	body="$(curl -sS --max-time 5 --unix-socket "$TEST_RUN_DIR/run/janus.sock" http://janus/1.0)"
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 --unix-socket "$TEST_RUN_DIR/run/janus.sock" http://janus/1.0)"
	eq "$code" "200"
	json_has "$body" '"type":"janus"'
}

case_control_unix_health() {
	local body code
	body="$(curl -sS --max-time 5 --unix-socket "$TEST_RUN_DIR/run/janus.sock" http://janus/1.0/health)"
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 --unix-socket "$TEST_RUN_DIR/run/janus.sock" http://janus/1.0/health)"
	eq "$code" "200"
	json_has "$body" '"status":"ok"'
}

case_control_unknown_paths_404() {
	# Typos and wrong-method calls must never look alive.
	eq "$(http_code http://127.0.0.1:7600/1.0/bogus)" "404"
	local code
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -X GET \
		http://127.0.0.1:7600/1.0/apps/nope-zzzzzz/heartbeat)"
	eq "$code" "405"
}
