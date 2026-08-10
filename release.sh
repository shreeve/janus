#!/usr/bin/env bash
#
# release.sh — build caddy+janus binaries
#
#   ./release.sh              dev build of the working tree -> bin/caddy-janus
#   ./release.sh v1.6.4       cross-compile the tagged module for every
#                             platform -> dist/, then publish the binaries as
#                             GitHub release assets on that tag
#
# The Caddy version comes from go.mod so builds always match the module's
# pinned dependency. Release builds compile from the pushed tag (never the
# working tree), so the binary's build-info reports the exact module version
# and sum. GOPRIVATE forces a direct git fetch, avoiding module-proxy lag on
# freshly pushed tags.

set -euo pipefail
cd "$(dirname "$0")"

CADDY_VERSION="$(awk '$1 == "github.com/caddyserver/caddy/v2" { print $2 }' go.mod)"
[[ -n "$CADDY_VERSION" ]] || { echo "error: caddy version not found in go.mod" >&2; exit 1; }

export PATH="$(go env GOPATH)/bin:$PATH"
export GOPRIVATE=github.com/shreeve/janus

TAG="${1:-}"

if [[ -z "$TAG" ]]; then
  # Dev: build the working tree for this machine.
  mkdir -p bin
  xcaddy build "$CADDY_VERSION" \
    --with github.com/shreeve/janus=. \
    --output bin/caddy-janus
  bin/caddy-janus list-modules | grep -q janus || { echo "error: janus module missing from build" >&2; exit 1; }
  echo "ok: bin/caddy-janus ($(bin/caddy-janus version))"
  exit 0
fi

[[ "$TAG" == v* ]] || { echo "usage: $0 [vX.Y.Z]" >&2; exit 1; }
git rev-parse -q --verify "refs/tags/$TAG" >/dev/null || { echo "error: tag $TAG not found — tag and push first" >&2; exit 1; }

PLATFORMS=(darwin-arm64 linux-amd64 linux-arm64 windows-amd64)

mkdir -p dist
ASSETS=()
for platform in "${PLATFORMS[@]}"; do
  GOOS="${platform%-*}"
  GOARCH="${platform#*-}"
  out="dist/caddy-janus-$TAG-$platform"
  [[ "$GOOS" == windows ]] && out+=".exe"
  echo "== $platform"
  GOOS="$GOOS" GOARCH="$GOARCH" xcaddy build "$CADDY_VERSION" \
    --with "github.com/shreeve/janus@$TAG" \
    --output "$out"
  ASSETS+=("$out")
done

if ! gh release view "$TAG" >/dev/null 2>&1; then
  gh release create "$TAG" --title "$TAG" \
    --notes "Caddy $CADDY_VERSION + Janus $TAG. Download the binary for your platform, chmod +x, and run."
fi
gh release upload "$TAG" "${ASSETS[@]}" --clobber

echo "ok: https://github.com/shreeve/janus/releases/tag/$TAG"
