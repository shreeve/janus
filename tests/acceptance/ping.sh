# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: ping ----------------------------------------------------------

# $( ) strips trailing newlines, so bodies compare without the final \n.
case_ping_catchall_foo() {
	eq "$(http_body https://foo.janus.localhost/ping)" "pong"
	eq "$(http_code https://foo.janus.localhost/ping)" "200"
}

case_ping_catchall_bar() {
	eq "$(http_body https://bar.janus.localhost/ping)" "pong"
	eq "$(http_code https://bar.janus.localhost/ping)" "200"
}

case_ping_on_explicit() {
	eq "$(http_body https://on.janus.localhost/ping)" "pong"
	eq "$(http_code https://on.janus.localhost/ping)" "200"
}

case_ping_off_explicit() {
	eq "$(http_code https://off.janus.localhost/ping)" "404"
	ne "$(http_body https://off.janus.localhost/ping)" "pong"
}

case_ping_tls_trusted() {
	# curl verify result 0 = chain trusted (no -k)
	local v
	v="$(curl -sS -o /dev/null -w '%{ssl_verify_result}' --max-time 5 https://on.janus.localhost/ping)"
	eq "$v" "0"
}
