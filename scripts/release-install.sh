#!/usr/bin/env bash
#
# install.sh — install janus from this extracted release archive.
#
#   janus -> ~/.local/bin, or /usr/local/bin when run as root
#   (override: BIN=...)
#
# The archive is self-contained and also runs in place. This installer only
# puts the binary on PATH; Caddyfile.minimal and Caddyfile.example remain here
# as references so they never overwrite an operator's live configuration. sudo is used only when
# the destination is not writable.

set -euo pipefail
cd "$(dirname "$0")"

# Color only when stdout is a terminal, and never against NO_COLOR.
Color_Off='' Red='' Green='' Dim='' Bold_Green='' Bold_White=''
if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  Color_Off='\033[0m'
  Red='\033[0;31m' Green='\033[0;32m' Dim='\033[0;2m'
  Bold_Green='\033[1;32m' Bold_White='\033[1m'
fi

info() { printf "${Dim}%s${Color_Off}\n" "$*"; }
fail() { printf "${Red}error${Color_Off}: %s\n" "$*" >&2; exit 2; }
tildify() { case "$1" in "$HOME"/*) printf '~%s' "${1#"$HOME"}" ;; *) printf '%s' "$1" ;; esac; }

[[ -f janus && -x janus ]] || fail "janus is missing or not executable"

# System-wide for root (a deploy's systemd unit and setcap keep their path),
# user-owned for everyone else — the XDG home for user executables.
if [[ "$(id -u)" == 0 ]]; then BIN=${BIN:-/usr/local/bin}
else BIN=${BIN:-$HOME/.local/bin}; fi
SOURCE="$(pwd -P)/janus"

as_owner() {
  if [[ -w "$(dirname "$1")" || -w "$1" ]]; then
    "${@:2}"
  else
    sudo "${@:2}"
  fi
}

if [[ -e "$BIN" && ! -d "$BIN" ]]; then
  fail "BIN exists but is not a directory: $BIN"
fi
if [[ ! -d "$BIN" ]]; then
  # Plain first: a missing parent (a fresh ~/.local) reads as unwritable,
  # and sudo must never create a user's own home directories as root.
  install -d -m 0755 "$BIN" 2>/dev/null || as_owner "$BIN" install -d -m 0755 "$BIN"
fi
DEST_DIR="$(cd "$BIN" && pwd -P)"
DEST="$DEST_DIR/janus"
if [[ "$SOURCE" == "$DEST" ]]; then
  printf "${Green}janus already runs in place at ${Bold_Green}%s${Color_Off}\n" "$(tildify "$DEST")"
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
# Linux: the replacement is a fresh inode, which silently drops any
# file capability (cap_net_bind_service) the old binary carried —
# capture it now so it can be restored after the swap.
HAD_CAPS=
if [[ "$(uname -s)" == Linux ]] && command -v getcap >/dev/null; then
  HAD_CAPS="$(getcap "$DEST" 2>/dev/null || true)"
fi

STAGE="$(run_as_owner mktemp "$DEST_DIR/.janus.install.XXXXXX")"
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

hint_caps=false
if [[ "$(uname -s)" == Linux ]]; then
  if [[ "$HAD_CAPS" == *cap_net_bind_service* ]]; then
    info "restoring cap_net_bind_service (upgrades drop it with the old inode)"
    if [[ "$(id -u)" == 0 ]]; then setcap cap_net_bind_service=+ep "$DEST"
    else sudo setcap cap_net_bind_service=+ep "$DEST"; fi
  elif [[ "$(getcap "$DEST" 2>/dev/null || true)" != *cap_net_bind_service* ]]; then
    hint_caps=true
  fi
fi

printf "${Green}janus was installed to ${Bold_Green}%s${Color_Off}\n" "$(tildify "$DEST")"

case ":$PATH:" in
  *":$DEST_DIR:"*)
    info "Run 'janus version' to get started"
    ;;
  *)
    printf '\n'
    info "$(tildify "$DEST_DIR") is not on your PATH. Add it:"
    printf "  ${Bold_White}echo 'export PATH=\"%s:\$PATH\"' >> ~/.zshrc${Color_Off}${Dim}   # or ~/.bashrc${Color_Off}\n" "$DEST_DIR"
    ;;
esac

if $hint_caps; then
  printf '\n'
  info "To let janus bind :80/:443 as non-root, run:"
  printf "  ${Bold_White}sudo setcap cap_net_bind_service=+ep %s${Color_Off}\n" "$DEST"
fi
