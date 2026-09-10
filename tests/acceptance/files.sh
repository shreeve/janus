# Sourced by test.sh; fixture ownership and cleanup remain in the driver.
# --- cases: files --------------------------------------------------------------

FILES_APP_FILE="$TEST_RUN_DIR/files/app-id"
FILES_OFF_APP_FILE="$TEST_RUN_DIR/files/off-app-id"

case_files_setup() {
	mkdir -p "$TEST_RUN_DIR/files/root1/docs" "$TEST_RUN_DIR/files/root2" "$TEST_RUN_DIR/files/forever"
	printf 'first-root' >"$TEST_RUN_DIR/files/root1/ordered.txt"
	printf 'value = 1' >"$TEST_RUN_DIR/files/root1/source.rip"
	printf '{"bundle":"canonical transparent sidecar payload"}' >"$TEST_RUN_DIR/files/root1/bundle.json"
	"$TESTKIT" precompress --input "$TEST_RUN_DIR/files/root1/bundle.json"
	printf '{"plain":"identity only"}' >"$TEST_RUN_DIR/files/root1/plain.json"
	printf '{"priority":"first identity"}' >"$TEST_RUN_DIR/files/root1/priority.json"
	printf '{"orphan":"sidecar only"}' >"$TEST_RUN_DIR/files/root1/orphan.json"
	"$TESTKIT" precompress --input "$TEST_RUN_DIR/files/root1/orphan.json"
	rm -f "$TEST_RUN_DIR/files/root1/orphan.json" "$TEST_RUN_DIR/files/root1/orphan.json.zst" "$TEST_RUN_DIR/files/root1/orphan.json.gz"
	printf '<main>directory index</main>' >"$TEST_RUN_DIR/files/root1/docs/index.html"
	"$TESTKIT" precompress --input "$TEST_RUN_DIR/files/root1/docs/index.html"
	printf 'second-root' >"$TEST_RUN_DIR/files/root2/ordered.txt"
	printf 'second-only' >"$TEST_RUN_DIR/files/root2/second.txt"
	printf 'body{}' >"$TEST_RUN_DIR/files/root2/styles.css"
	printf '{"priority":"second sidecar"}' >"$TEST_RUN_DIR/files/root2/priority.json"
	"$TESTKIT" precompress --input "$TEST_RUN_DIR/files/root2/priority.json"
	printf 'export{}' >"$TEST_RUN_DIR/files/forever/app.js"
	printf '<main>spa-shell</main>' >"$TEST_RUN_DIR/files/shell.html"
	"$TESTKIT" precompress --input "$TEST_RUN_DIR/files/shell.html"
	capi POST /1.0/apps \
		"{\"name\":\"files\",\"hosts\":[\"files.ripdev.io\",\"filesencode.ripdev.io\"],\"files\":{\"roots\":[{\"path\":\"$TEST_RUN_DIR/files/root1\",\"cache\":\"never\",\"browse\":true},{\"path\":\"$TEST_RUN_DIR/files/root2\",\"cache\":\"revalidate\"},{\"path\":\"$TEST_RUN_DIR/files/forever\",\"cache\":\"forever\"}],\"proxy_first\":[\"/api\"],\"shell\":\"$TEST_RUN_DIR/files/shell.html\"}}"
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	printf '%s' "$id" >"$FILES_APP_FILE"
}

case_files_special_files() {
	mkfifo "$TEST_RUN_DIR/files/root1/pipe" "$TEST_RUN_DIR/files/root1/plain.json.br"
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 -H 'Accept: application/json' https://files.ripdev.io/pipe)" "404"
	eq "$(curl -sS --max-time 2 -H 'Accept-Encoding: br' https://files.ripdev.io/plain.json)" '{"plain":"identity only"}'
	rm "$TEST_RUN_DIR/files/root1/pipe" "$TEST_RUN_DIR/files/root1/plain.json.br"
}

case_files_precompressed_brotli() {
	local headers decoded sidecar_size
	headers="$(curl -sS -D - -o "$TEST_RUN_DIR/files/bundle.br.response" --max-time 5 \
		-H 'Accept-Encoding: br' https://files.ripdev.io/bundle.json | tr -d '\r')"
	json_has "$headers" 'content-encoding: br'
	json_has "$headers" 'vary: Accept-Encoding'
	json_has "$headers" 'content-type: application/json'
	json_has "$headers" 'cache-control: no-store'
	json_has "$headers" 'etag: W/"'
	sidecar_size="$(wc -c <"$TEST_RUN_DIR/files/root1/bundle.json.br" | tr -d ' ')"
	json_has "$headers" "content-length: $sidecar_size"
	cmp -s "$TEST_RUN_DIR/files/bundle.br.response" "$TEST_RUN_DIR/files/root1/bundle.json.br"
	decoded="$("$TESTKIT" fetch --ca "$ROOT/certs/ripdev.io.crt" --accept br https://files.ripdev.io/bundle.json)"
	eq "$decoded" '{"bundle":"canonical transparent sidecar payload"}'
}

case_files_precompressed_negotiation() {
	local headers decoded
	headers="$(curl -sS -I --max-time 5 -H 'Accept-Encoding: gzip;q=0.9, br;q=0.4' https://files.ripdev.io/bundle.json | tr -d '\r')"
	json_has "$headers" 'content-encoding: gzip'
	headers="$(curl -sS -I --max-time 5 -H 'Accept-Encoding: br;q=0, *;q=0.8' https://files.ripdev.io/bundle.json | tr -d '\r')"
	json_has "$headers" 'content-encoding: zstd'
	headers="$(curl -sS -I --max-time 5 -H 'Accept-Encoding: br;q=0' https://files.ripdev.io/bundle.json | tr -d '\r')"
	if printf '%s' "$headers" | grep -qi '^content-encoding:'; then
		echo "br;q=0 selected an encoded representation" >&2
		return 1
	fi
	decoded="$("$TESTKIT" fetch --ca "$ROOT/certs/ripdev.io.crt" --accept 'identity;q=1, br;q=0.5' https://files.ripdev.io/bundle.json)"
	eq "$decoded" '{"bundle":"canonical transparent sidecar payload"}'
	headers="$(curl -sS -I --max-time 5 -H 'Accept-Encoding: identity;q=1, br;q=0.5' https://files.ripdev.io/bundle.json | tr -d '\r')"
	if printf '%s' "$headers" | grep -qi '^content-encoding:'; then
		echo "higher-quality identity did not win" >&2
		return 1
	fi
}

case_files_precompressed_fallback_and_root() {
	local headers decoded
	headers="$(curl -sS -I --max-time 5 -H 'Accept-Encoding: br' https://files.ripdev.io/plain.json | tr -d '\r')"
	if printf '%s' "$headers" | grep -qi '^content-encoding:'; then
		echo "missing sidecar did not fall back to identity" >&2
		return 1
	fi
	decoded="$("$TESTKIT" fetch --ca "$ROOT/certs/ripdev.io.crt" --accept br https://files.ripdev.io/priority.json)"
	eq "$decoded" '{"priority":"first identity"}'
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -H 'Accept: application/json' -H 'Accept-Encoding: br' https://files.ripdev.io/orphan.json)" "404"

	rm -f "$TEST_RUN_DIR/files/root1/bundle.json.br"
	headers="$(curl -sS -I --max-time 5 -H 'Accept-Encoding: br' https://files.ripdev.io/bundle.json | tr -d '\r')"
	if printf '%s' "$headers" | grep -qi '^content-encoding:'; then
		echo "removed sidecar did not fall back to identity" >&2
		return 1
	fi
	decoded="$("$TESTKIT" fetch --ca "$ROOT/certs/ripdev.io.crt" --accept br https://files.ripdev.io/bundle.json)"
	eq "$decoded" '{"bundle":"canonical transparent sidecar payload"}'
	"$TESTKIT" precompress --input "$TEST_RUN_DIR/files/root1/bundle.json"
}

case_files_precompressed_conditionals() {
	local br_headers br_etag modified code
	br_headers="$(curl -sS -I --max-time 5 -H 'Accept-Encoding: br' https://files.ripdev.io/bundle.json | tr -d '\r')"
	br_etag="$(printf '%s\n' "$br_headers" | sed -n 's/^etag: //p' | head -1)"
	modified="$(printf '%s\n' "$br_headers" | sed -n 's/^last-modified: //p' | head -1)"
	ok "-n \"$br_etag\"" "Brotli response has no ETag"
	ok "-n \"$modified\"" "Brotli response has no Last-Modified"
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -H 'Accept-Encoding: br' -H "If-None-Match: $br_etag" https://files.ripdev.io/bundle.json)"
	eq "$code" "304"
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -H "If-None-Match: $br_etag" https://files.ripdev.io/bundle.json)"
	eq "$code" "200"
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -H 'Accept-Encoding: gzip' -H "If-None-Match: $br_etag" https://files.ripdev.io/bundle.json)"
	eq "$code" "200"
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -H 'Accept-Encoding: br' -H "If-Modified-Since: $modified" https://files.ripdev.io/bundle.json)"
	eq "$code" "304"
}

case_files_precompressed_head_range_and_encode() {
	local headers sidecar_size decoded
	headers="$(curl -sS -I --max-time 5 -H 'Accept-Encoding: br' https://files.ripdev.io/bundle.json | tr -d '\r')"
	sidecar_size="$(wc -c <"$TEST_RUN_DIR/files/root1/bundle.json.br" | tr -d ' ')"
	json_has "$headers" 'content-encoding: br'
	json_has "$headers" "content-length: $sidecar_size"
	curl -sS -D "$TEST_RUN_DIR/files/range.headers" -o "$TEST_RUN_DIR/files/range.body" --max-time 5 \
		-H 'Accept-Encoding: br' -H 'Range: bytes=1-4' https://files.ripdev.io/bundle.json
	dd if="$TEST_RUN_DIR/files/root1/bundle.json.br" of="$TEST_RUN_DIR/files/range.want" bs=1 skip=1 count=4 2>/dev/null
	cmp -s "$TEST_RUN_DIR/files/range.body" "$TEST_RUN_DIR/files/range.want"
	headers="$(tr -d '\r' <"$TEST_RUN_DIR/files/range.headers")"
	json_has "$headers" '206'
	json_has "$headers" 'content-encoding: br'
	json_has "$headers" "content-range: bytes 1-4/$sidecar_size"

	headers="$(curl -sS -I --max-time 5 -H 'Accept-Encoding: br, gzip' https://filesencode.ripdev.io/bundle.json | tr -d '\r')"
	eq "$(printf '%s\n' "$headers" | grep -ci '^content-encoding: br$')" "1"
	decoded="$("$TESTKIT" fetch --ca "$ROOT/certs/ripdev.io.crt" --accept 'br, gzip' https://filesencode.ripdev.io/bundle.json)"
	eq "$decoded" '{"bundle":"canonical transparent sidecar payload"}'
}

case_files_precompressed_shell_and_index() {
	local decoded
	decoded="$("$TESTKIT" fetch --ca "$ROOT/certs/ripdev.io.crt" --accept br https://files.ripdev.io/docs/)"
	eq "$decoded" '<main>directory index</main>'
	decoded="$("$TESTKIT" fetch --ca "$ROOT/certs/ripdev.io.crt" --accept br --media text/html https://files.ripdev.io/client-route)"
	eq "$decoded" '<main>spa-shell</main>'
}

case_files_order_and_http_semantics() {
	eq "$(http_body https://files.ripdev.io/ordered.txt)" "first-root"
	eq "$(http_body https://files.ripdev.io/second.txt)" "second-only"
	local headers
	headers="$(curl -sS -I --max-time 5 https://files.ripdev.io/ordered.txt)"
	json_has "$headers" 'etag: W/"'
	json_has "$headers" 'content-length: 10'
	eq "$(curl -sS --max-time 5 -H 'Range: bytes=1-4' https://files.ripdev.io/ordered.txt)" "irst"
}

case_files_shell_proxy_first_and_paths() {
	eq "$(curl -sS --max-time 5 -H 'Accept: text/html' https://files.ripdev.io/missing)" '<main>spa-shell</main>'
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -H 'Accept: text/html' https://files.ripdev.io/api)" "503"
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -H 'Accept: image/*' https://files.ripdev.io/missing.png)" "404"
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -X POST https://files.ripdev.io/missing)" "404"
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 'https://files.ripdev.io/a%2fb')" "400"
}

case_files_response_policy() {
	local headers
	headers="$(curl -sS -I --max-time 5 https://files.ripdev.io/source.rip | tr -d '\r')"
	json_has "$headers" 'content-type: text/plain; charset=utf-8'
	json_has "$headers" 'cache-control: no-store'
	headers="$(curl -sS -I --max-time 5 https://files.ripdev.io/styles.css | tr -d '\r')"
	json_has "$headers" 'content-type: text/css; charset=utf-8'
	json_has "$headers" 'cache-control: no-cache'
	headers="$(curl -sS -I --max-time 5 https://files.ripdev.io/app.js | tr -d '\r')"
	json_has "$headers" 'cache-control: public, max-age=31536000, immutable'
}

case_files_strict_hot_fields() {
	capi POST /1.0/apps '{"name":"badfiles","hosts":["badfiles.ripdev.io"],"files":null}'
	eq "$REPLY_CODE" "400"
	capi POST /1.0/apps '{"name":"badsite","site":{"host":"{site}.bad.ripdev.io","dir":"/tmp","unknown":1}}'
	eq "$REPLY_CODE" "400"
	capi POST /1.0/apps '{"name":"both","hosts":["both.ripdev.io"],"site":{"host":"{site}.both.ripdev.io","dir":"/tmp"}}'
	eq "$REPLY_CODE" "400"
}

case_files_cascade_off() {
	capi POST /1.0/apps \
		"{\"name\":\"filesoff\",\"hosts\":[\"filesoff.ripdev.io\"],\"files\":{\"roots\":[{\"path\":\"$TEST_RUN_DIR/files/root1\",\"cache\":\"revalidate\"}],\"shell\":\"$TEST_RUN_DIR/files/shell.html\"}}"
	eq "$REPLY_CODE" "201"
	local id
	id="$(printf '%s' "$REPLY_BODY" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	printf '%s' "$id" >"$FILES_OFF_APP_FILE"
	eq "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 https://filesoff.ripdev.io/ordered.txt)" "503"
}
