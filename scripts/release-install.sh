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
# Source builds pass an absolute binary path; extracted releases use the
# adjacent binary. Both paths use the same atomic replacement below.
SOURCE=${1:-}
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

SOURCE=${SOURCE:-"$(pwd -P)/janus"}

# What is being installed. macOS ships Janus.app: the bundle is what Local
# Network privacy identifies (name, icon, usage description), and the
# command on PATH is a symlink into it. The archive's own `janus` is that
# symlink, and `make install` passes the built bundle; either resolves to
# bundle mode. A bare binary (Linux, or a macOS build installed without
# the bundle) takes the file path below.
APP=
if [[ -d "$SOURCE" && -f "$SOURCE/Contents/Info.plist" ]]; then
  APP="$(cd "$SOURCE" && pwd -P)"
elif [[ -L "$SOURCE" || -f "$SOURCE" ]]; then
  resolved="$(cd "$(dirname "$SOURCE")" && pwd -P)/$(basename "$SOURCE")"
  while [[ -L "$resolved" ]]; do
    target="$(readlink "$resolved")"
    [[ "$target" = /* ]] || target="$(dirname "$resolved")/$target"
    resolved="$(cd "$(dirname "$target")" && pwd -P)/$(basename "$target")"
  done
  case "$resolved" in
    *.app/Contents/MacOS/*) APP="${resolved%/Contents/MacOS/*}" ;;
  esac
  SOURCE="$resolved"
fi
FROM_BUNDLE=false
if [[ -n "$APP" ]]; then
  [[ "$(uname -s)" == Darwin ]] || fail "$APP is a macOS application bundle; this is not macOS"
  [[ -x "$APP/Contents/MacOS/janus" ]] || fail "$APP has no executable at Contents/MacOS/janus"
  if [[ "$(id -u)" == 0 ]]; then
    # Root runs the bare executable from /usr/local/bin: a daemon is exempt
    # from Local Network privacy, so the bundle buys it nothing, and a
    # system service must not run from the group-writable /Applications.
    SOURCE="$APP/Contents/MacOS/janus"
    APP=
    FROM_BUNDLE=true
  fi
else
  [[ -f "$SOURCE" && -x "$SOURCE" ]] || fail "janus is missing or not executable: $SOURCE"
fi

# System-wide for root (a deploy's systemd unit and setcap keep their path),
# user-owned for everyone else — the XDG home for user executables.
if [[ "$(id -u)" == 0 ]]; then BIN=${BIN:-/usr/local/bin}
else BIN=${BIN:-$HOME/.local/bin}; fi

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

# --- macOS: the application bundle and the command symlink -----------------
if [[ -n "$APP" ]]; then
  IDENTIFIER=com.github.shreeve.janus
  got="$( (codesign -dv "$APP" 2>&1 || true) | sed -n 's/^Identifier=//p')"
  [[ "$got" == "$IDENTIFIER" ]] || fail "$APP is signed as '${got:-nothing}', not $IDENTIFIER"
  APP_DIR="$HOME/Applications"
  APP_DEST="$APP_DIR/Janus.app"
  if [[ -e "$APP_DEST" && ! ( -d "$APP_DEST" && -f "$APP_DEST/Contents/Info.plist" ) ]]; then
    fail "$APP_DEST exists and is not an application bundle"
  fi
  if [[ "$APP" != "$APP_DEST" ]]; then
    [[ -d "$APP_DIR" ]] || install -d -m 0755 "$APP_DIR" || fail "cannot create $APP_DIR"
    [[ -w "$APP_DIR" ]] || fail "$APP_DIR is not writable"
    # A complete copy beside the destination, verified, then swapped in by
    # rename: nothing is ever written into a bundle the edge may be
    # running from, and no cached inode is overwritten.
    APP_STAGE="$(mktemp -d "$APP_DIR/.Janus.app.install.XXXXXX")"
    trap 'rm -rf "$APP_STAGE"' EXIT
    ditto "$APP" "$APP_STAGE/Janus.app"
    # A bundle downloaded by a browser carries the quarantine flag through
    # extraction and copying, and launchd's start of it is killed with no
    # dialog anyone sees. Refuse, and name the command that clears it.
    if xattr -p com.apple.quarantine "$APP_STAGE/Janus.app" >/dev/null 2>&1 || xattr -p com.apple.quarantine "$APP_STAGE/Janus.app/Contents/MacOS/janus" >/dev/null 2>&1; then
      fail "$APP is quarantined (downloaded by a browser); clear it first: xattr -dr com.apple.quarantine '$APP'"
    fi
    codesign --verify --strict "$APP_STAGE/Janus.app" || fail "the copied bundle does not verify"
    # The copy keeps the build's mtime; 'janus status' compares that to the
    # running edge's start to say a newer binary is installed.
    touch "$APP_STAGE/Janus.app/Contents/MacOS/janus"
    APP_OLD=
    if [[ -e "$APP_DEST" ]]; then
      APP_OLD="$(mktemp -d "$APP_DIR/.Janus.app.old.XXXXXX")"
      mv "$APP_DEST" "$APP_OLD/Janus.app"
    fi
    mv "$APP_STAGE/Janus.app" "$APP_DEST"
    rm -rf "$APP_STAGE" "$APP_OLD"
    trap - EXIT
  fi
  # The command: a symlink into the bundle, replacing a bare binary or an
  # older link atomically. A directory in the way is someone else's.
  if [[ -d "$DEST" && ! -L "$DEST" ]]; then
    fail "$DEST is a directory"
  fi
  REPLACED_FILE=false
  [[ -f "$DEST" && ! -L "$DEST" ]] && REPLACED_FILE=true
  LINK_STAGE="$(run_as_owner mktemp "$DEST_DIR/.janus.install.XXXXXX")"
  run_as_owner rm -f "$LINK_STAGE"
  run_as_owner ln -s "$APP_DEST/Contents/MacOS/janus" "$LINK_STAGE"
  run_as_owner mv -f "$LINK_STAGE" "$DEST"
  # Tell LaunchServices about the bundle so System Settings shows its name
  # and icon at once. Best effort: the edge does not depend on it.
  lsregister="$(command -v lsregister || true)"
  [[ -n "$lsregister" ]] || lsregister=/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister
  if [[ -x "$lsregister" ]]; then
    "$lsregister" -f "$APP_DEST" >/dev/null 2>&1 || true
  fi
  printf "${Green}Janus was installed to ${Bold_Green}%s${Color_Off}\n" "$(tildify "$APP_DEST")"
  printf "${Green}janus was installed to ${Bold_Green}%s${Color_Off}\n" "$(tildify "$DEST")"
  if $REPLACED_FILE; then
    info "the command was a bare binary; a running edge keeps it until 'janus restart', which re-registers the service item"
  fi
  case ":$PATH:" in
    *":$DEST_DIR:"*) info "Run 'janus version' to get started" ;;
    *)
      printf '\n'
      info "$(tildify "$DEST_DIR") is not on your PATH. Add it:"
      printf "  ${Bold_White}echo 'export PATH=\"%s:\$PATH\"' >> ~/.zshrc${Color_Off}${Dim}   # or ~/.bashrc${Color_Off}\n" "$DEST_DIR"
      ;;
  esac
  exit 0
fi

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
# macOS: Local Network privacy files its allow/deny under the executable's
# code-signing identifier (with its path and build UUID). Every install gets
# the same name, so an upgrade keeps its grant instead of appearing as a new
# program. The staged copy is a fresh file, so signing it never touches an
# inode macOS has already cached. Makefile and install.sh carry the same
# literal. A file codesign cannot read is left alone.
IDENTIFIER=com.github.shreeve.janus
if [[ "$(uname -s)" == Darwin ]] && command -v codesign >/dev/null; then
  current="$( (codesign -dv "$STAGE" 2>&1 || true) | sed -n 's/^Identifier=//p')"
  # An executable lifted out of the bundle carries the bundle's seal; sign
  # it again on its own.
  if [[ -n "$current" && "$current" != "$IDENTIFIER" ]] || $FROM_BUNDLE; then
    info "signing as $IDENTIFIER"
    run_as_owner codesign -s - -f -i "$IDENTIFIER" "$STAGE"
  fi
fi
# Preserve the ability to bind before committing the replacement. A failed
# capability update must leave the working binary at its original path.
if [[ "$HAD_CAPS" == *cap_net_bind_service* ]]; then
  info "preserving cap_net_bind_service on the replacement"
  if [[ "$(id -u)" == 0 ]]; then setcap cap_net_bind_service=+ep "$STAGE"
  else sudo setcap cap_net_bind_service=+ep "$STAGE"; fi
fi
run_as_owner mv -f "$STAGE" "$DEST"
STAGE=""
trap - EXIT HUP INT TERM

hint_caps=false
if [[ "$(uname -s)" == Linux ]]; then
  if [[ "$(getcap "$DEST" 2>/dev/null || true)" != *cap_net_bind_service* ]]; then
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
