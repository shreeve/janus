#!/usr/bin/env bash
#
# install.sh — install janus with one command (macOS and Linux):
#
#   curl -fsSL https://raw.githubusercontent.com/shreeve/janus/main/install.sh | bash
#
# Pin a version by passing a tag (with or without the leading v):
#
#   curl -fsSL .../install.sh | bash -s v1.10.0
#
# Downloads the release archive for this platform, verifies its sha256
# against the published checksums, and runs the archive's own installer.
# As a user, janus lands in ~/.local/bin — the XDG home for user
# executables; as root it lands in /usr/local/bin, so a system deploy
# (systemd unit, setcap) keeps its path. Override either with BIN=...;
# sudo is used only if the destination dir is root-owned. Windows support
# comes later.

set -euo pipefail

REPO=shreeve/janus
NAME=janus

say()  { printf '%s\n' "$*"; }
fail() { printf 'install: %s\n' "$*" >&2; exit 1; }

# Everything lives in main() so a truncated `curl | bash` download can
# never execute a half-delivered script.
main() {
  command -v curl >/dev/null || fail "curl is required"
  command -v tar  >/dev/null || fail "tar is required"

  # --- platform -> release asset suffix ------------------------------------
  os=$(uname -s) arch=$(uname -m)
  # A shell running under Rosetta reports x86_64 on Apple Silicon.
  if [ "$os" = Darwin ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
    arch=arm64
  fi
  case "$os-$arch" in
    Darwin-arm64)          plat=osx-arm64    ;;
    Linux-x86_64)          plat=linux-amd64  ;;
    Linux-aarch64|Linux-arm64) plat=linux-arm64 ;;
    Darwin-x86_64)         fail "no Intel macOS build is published (Apple Silicon only)" ;;
    MINGW*|MSYS*|CYGWIN*)  fail "Windows is not supported by this installer yet" ;;
    *)                     fail "unsupported platform: $os $arch" ;;
  esac

  # --- version: argument, or the tag `releases/latest` redirects to --------
  tag=${1:-}
  if [ -n "$tag" ]; then
    case "$tag" in v*) ;; *) tag="v$tag" ;; esac
  else
    tag=$(curl -fsSLI --retry 3 --retry-delay 1 -o /dev/null -w '%{url_effective}' \
      "https://github.com/$REPO/releases/latest") || fail "cannot reach github.com"
    tag=${tag##*/}
  fi
  # With no releases, GitHub redirects .../latest to .../releases — so the
  # resolved "tag" is only real if it looks like one.
  case "$tag" in v*) ;; *) fail "no releases found for $REPO" ;; esac

  asset="$NAME-$tag-$plat.tar.gz"
  base="https://github.com/$REPO/releases/download/$tag"

  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT

  say "installing $NAME $tag ($plat)"
  curl -fSL --retry 3 --retry-delay 1 --progress-bar -o "$tmp/$asset" "$base/$asset" \
    || fail "download failed: $base/$asset"

  # --- verify against the release's published checksums --------------------
  curl -fsSL --retry 3 --retry-delay 1 -o "$tmp/checksums.txt" "$base/$NAME-$tag-checksums.txt" \
    || fail "download failed: $NAME-$tag-checksums.txt"
  if command -v sha256sum >/dev/null; then
    sum=$(sha256sum "$tmp/$asset" | cut -d' ' -f1)
  else
    sum=$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)
  fi
  want=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
  [ -n "$want" ]        || fail "no checksum published for $asset"
  [ "$sum" = "$want" ]  || fail "checksum mismatch for $asset"

  # --- extract and hand off to the archive's own installer ------------------
  tar -xzf "$tmp/$asset" -C "$tmp"

  # The destination: system-wide for root, user-owned for everyone else.
  # Exported so the archive's installer — including the one inside every
  # already-published release — uses the same answer.
  if [ "$(id -u)" = 0 ]; then BIN="${BIN:-/usr/local/bin}"
  else BIN="${BIN:-$HOME/.local/bin}"; fi
  export BIN

  # Create the destination as ourselves when we can, so the archive's
  # installer never reaches for sudo to make a directory under $HOME
  # (a missing ~/.local reads as unwritable to its ownership check).
  [ -d "$BIN" ] || install -d -m 0755 "$BIN" 2>/dev/null || true

  # Linux binds :80/:443 as non-root only with cap_net_bind_service, and the
  # install writes a fresh inode — which silently drops any capability the
  # old binary carried. Capture before, restore after; hint when absent.
  #
  # Releases packaged since the cap logic landed carry it in their own
  # embedded installer, so acting here too would restore twice and print
  # the hint twice. Only compensate when the archive's installer predates
  # the logic (no mention of setcap).
  dest="$BIN/$NAME"
  had_caps=
  if [ "$os" = Linux ] && command -v getcap >/dev/null; then
    had_caps=$(getcap "$dest" 2>/dev/null || true)
  fi

  bash "$tmp/$NAME-$tag-$plat/install.sh"

  if [ "$os" = Linux ] && ! grep -q setcap "$tmp/$NAME-$tag-$plat/install.sh"; then
    case "$had_caps" in
      *cap_net_bind_service*)
        say "restoring cap_net_bind_service (upgrades drop it with the old inode)"
        if [ "$(id -u)" = 0 ]; then setcap cap_net_bind_service=+ep "$dest"
        else sudo setcap cap_net_bind_service=+ep "$dest"; fi
        ;;
      *)
        case "$(getcap "$dest" 2>/dev/null || true)" in
          *cap_net_bind_service*) ;;
          *)
            say ""
            say "note: to let $NAME bind :80/:443 as non-root, run:"
            say "  sudo setcap cap_net_bind_service=+ep $dest"
            ;;
        esac
        ;;
    esac
  fi
}

main "$@"
