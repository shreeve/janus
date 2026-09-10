# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: sendfile --------------------------------------------------------

SENDFILE_APP_FILE="$TEST_RUN_DIR/sendfile-app-id"
SENDFILE_SOCK="$TEST_RUN_DIR/run/sendfile.sock"
SENDFILE_PATH="$TEST_RUN_DIR/sendfile/report.txt"

case_sendfile_setup() {
	mkdir -p "$TEST_RUN_DIR/sendfile"
	printf '0123456789-sendfile\n' >"$SENDFILE_PATH"
	rm -f "$SENDFILE_SOCK"
	printf '%s\n' "$SENDFILE_SOCK" >>"$DATA_SOCKS_FILE"
	"$TESTKIT" sendfile --sock "$SENDFILE_SOCK" --path "$SENDFILE_PATH" \
		>>"$TEST_RUN_DIR/fixtures.log" 2>&1 &
	printf '%s\n' "$!" >>"$DATA_PIDS_FILE"
	local i
	for i in $(seq 1 50); do
		[[ -S "$SENDFILE_SOCK" ]] && break
		sleep 0.1
	done
	[[ -S "$SENDFILE_SOCK" ]] || return 1

	capi POST /1.0/apps '{"name":"sendfile","hosts":["sendfile.ripdev.io"]}'
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	ok "-n \"$id\"" "no id in $REPLY_BODY"
	printf '%s' "$id" >"$SENDFILE_APP_FILE"
	capi PUT "/1.0/apps/$id/upstreams" \
		"{\"upstreams\":[{\"path\":\"$SENDFILE_SOCK\"}]}"
	eq "$REPLY_CODE" "200"
}

case_sendfile_get_head_and_range() {
	local headers body
	headers="$TEST_RUN_DIR/sendfile/headers"
	body="$(curl -sS -D "$headers" --max-time 5 https://sendfile.ripdev.io/file)"
	eq "$body" "0123456789-sendfile"
	! tr -d '\r' <"$headers" | awk -F': ' 'tolower($1)=="x-sendfile" {found=1} END {exit !found}'

	eq "$(curl -sS -I -o /dev/null -w '%{http_code} %{size_download}' --max-time 5 \
		https://sendfile.ripdev.io/file)" "200 0"
	eq "$(curl -sS -H 'Range: bytes=2-5' --max-time 5 \
		https://sendfile.ripdev.io/file)" "2345"
	eq "$(curl -sS -o /dev/null -w '%{http_code}' -H 'Range: bytes=2-5' --max-time 5 \
		https://sendfile.ripdev.io/file)" "206"
}

case_sendfile_metadata_and_conditional() {
	local headers
	headers="$TEST_RUN_DIR/sendfile/custom-headers"
	curl -sS -o /dev/null -D "$headers" --max-time 5 https://sendfile.ripdev.io/custom
	local clean
	clean="$(tr -d '\r' <"$headers")"
	printf '%s' "$clean" | awk -F': ' 'tolower($1)=="content-type" && $2=="application/x-janus-acceptance" {found=1} END {exit !found}'
	printf '%s' "$clean" | awk -F': ' 'tolower($1)=="content-disposition" && $2=="attachment; filename=\"edge.data\"" {found=1} END {exit !found}'
	printf '%s' "$clean" | awk -F': ' 'tolower($1)=="etag" && $2=="\"fixture-etag\"" {found=1} END {exit !found}'
	printf '%s' "$clean" | awk -F': ' 'tolower($1)=="cache-control" && $2=="private, no-store" {found=1} END {exit !found}'
	eq "$(curl -sS -o /dev/null -w '%{http_code}' -H 'If-None-Match: "fixture-etag"' \
		--max-time 5 https://sendfile.ripdev.io/custom)" "304"
}

case_sendfile_failure_strips_instruction() {
	local headers code
	headers="$TEST_RUN_DIR/sendfile/missing-headers"
	code="$(curl -sS -o /dev/null -D "$headers" -w '%{http_code}' --max-time 5 \
		https://sendfile.ripdev.io/missing)"
	eq "$code" "502"
	local clean
	clean="$(tr -d '\r' <"$headers")"
	! printf '%s' "$clean" | awk -F': ' 'tolower($1)=="x-sendfile" {found=1} END {exit !found}'
	printf '%s' "$clean" | awk -F': ' 'tolower($1)=="cache-control" && $2=="no-store" {found=1} END {exit !found}'
}
