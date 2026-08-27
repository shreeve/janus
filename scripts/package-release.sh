#!/usr/bin/env bash
# package-release.sh — assemble one platform's self-contained release archive.
#
#   TAG=v1.7.2 PLAT=osx-arm64 scripts/package-release.sh
#
# The workflow builds bin/caddy-janus[.exe] first. This script packages that
# static binary with an installer, operator-facing configuration, README, and
# license. Windows archives omit the Unix install.sh and run in place.

set -euo pipefail
cd "$(dirname "$0")/.."

TAG=${TAG:?package-release: set TAG (for example, v1.7.2)}
PLAT=${PLAT:?package-release: set PLAT}
OUT=${OUT:-dist}

[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] || {
  echo "package-release: TAG must be a safe semantic version (for example, v1.7.2)" >&2
  exit 2
}
case "$PLAT" in
  osx-arm64 | linux-amd64 | linux-arm64 | windows-amd64 | windows-arm64) ;;
  *)
    echo "package-release: unsupported PLAT $PLAT" >&2
    exit 2
    ;;
esac

name="janus-$TAG-$PLAT"
root="$OUT/$name"
mkdir -p "$OUT"
rm -rf "$root"
mkdir -p "$root"

if [[ "$PLAT" == windows-* ]]; then
  [[ -f bin/caddy-janus.exe ]] || {
    echo "package-release: bin/caddy-janus.exe not found" >&2
    exit 2
  }
  cp bin/caddy-janus.exe "$root/caddy-janus.exe"
else
  [[ -f bin/caddy-janus ]] || {
    echo "package-release: bin/caddy-janus not found" >&2
    exit 2
  }
  cp bin/caddy-janus "$root/caddy-janus"
  chmod 0755 "$root/caddy-janus"
  install -m 0755 scripts/release-install.sh "$root/install.sh"
fi

cp Caddyfile.minimal Caddyfile.example README.md LICENSE "$root/"

if [[ "$PLAT" == windows-* ]]; then
  rm -f "$OUT/$name.zip"
  (cd "$OUT" && 7z a -tzip "$name.zip" "$name" >/dev/null)
  archive="$OUT/$name.zip"
else
  archive="$OUT/$name.tar.gz"
  tar -C "$OUT" -czf "$archive" "$name"
fi

printf '  -> %s (%s)\n' "$archive" "$(du -h "$archive" | cut -f1)"
