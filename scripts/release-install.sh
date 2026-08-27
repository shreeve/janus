#!/usr/bin/env bash
#
# install.sh — install caddy-janus from this extracted release archive.
#
#   caddy-janus -> /usr/local/bin   (override: BIN=...)
#
# The archive is self-contained and also runs in place. This installer only
# puts the binary on PATH; Caddyfile.minimal and Caddyfile.example remain here
# as references so they never overwrite an operator's live configuration. sudo is used only when
# the destination is not writable.

set -euo pipefail
cd "$(dirname "$0")"

[[ -f caddy-janus && -x caddy-janus ]] || {
  echo "install.sh: caddy-janus is missing or not executable" >&2
  exit 2
}

BIN=${BIN:-/usr/local/bin}
SOURCE="$(pwd -P)/caddy-janus"

as_owner() {
  if [[ -w "$(dirname "$1")" || -w "$1" ]]; then
    "${@:2}"
  else
    sudo "${@:2}"
  fi
}

if [[ -e "$BIN" && ! -d "$BIN" ]]; then
  echo "install.sh: BIN exists but is not a directory: $BIN" >&2
  exit 2
fi
if [[ ! -d "$BIN" ]]; then
  as_owner "$BIN" install -d -m 0755 "$BIN"
fi
DEST_DIR="$(cd "$BIN" && pwd -P)"
DEST="$DEST_DIR/caddy-janus"
if [[ "$SOURCE" == "$DEST" ]]; then
  echo "caddy-janus already runs in place at $DEST"
  exit 0
fi
# Stage the complete replacement beside the destination, so a copy failure
# leaves the old binary untouched and the final rename is an atomic replacement
# on the same filesystem.
need_sudo=false
if [[ -w "$DEST_DIR" ]]; then
  need_sudo=false
else
  need_sudo=true
fi
run_as_owner() {
  if $need_sudo; then
    sudo "$@"
  else
    "$@"
  fi
}
STAGE="$(run_as_owner mktemp "$DEST_DIR/.caddy-janus.install.XXXXXX")"
cleanup_stage() {
  if [[ -n "${STAGE:-}" ]]; then
    run_as_owner rm -f "$STAGE"
  fi
}
trap cleanup_stage EXIT
trap 'exit 1' HUP INT TERM
run_as_owner install -m 0755 "$SOURCE" "$STAGE"
run_as_owner mv -f "$STAGE" "$DEST"
STAGE=""
trap - EXIT HUP INT TERM

echo "installed caddy-janus -> $DEST_DIR  (on your PATH)"
echo "try: caddy-janus version"
