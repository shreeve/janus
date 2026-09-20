#!/usr/bin/env bash
# bundle-macos.sh — assemble Janus.app from a built janus binary.
#
#   scripts/bundle-macos.sh bin/janus dist/Janus.app [VERSION]
#
# The bundle is what macOS identifies for Local Network privacy: its
# identifier, display name, icon, and usage description. The executable is
# copied in unchanged, the icon is built from docs/janus-circle.png with
# the tools macOS ships, and the whole bundle is signed ad hoc under the
# identifier the Makefile and the installers share. The destination is
# written fresh: an existing directory there is replaced only after the
# new bundle is complete and signed.

set -euo pipefail
cd "$(dirname "$0")/.."

SOURCE=${1:?bundle-macos: source binary}
DEST=${2:?bundle-macos: destination Janus.app}
VERSION=${3:-}

IDENTIFIER=com.github.shreeve.janus
# The icon source: the 1024 px master when the repo has it (every size,
# nothing upscaled); otherwise the 320 px circle, at sizes up to 256 so
# nothing is upscaled either.
ICON_SOURCE=docs/janus-1024.png
ICON_SIZES="16:icon_16x16 32:icon_16x16@2x 32:icon_32x32 64:icon_32x32@2x 128:icon_128x128 256:icon_128x128@2x 256:icon_256x256 512:icon_256x256@2x 512:icon_512x512 1024:icon_512x512@2x"
if [[ ! -f "$ICON_SOURCE" ]]; then
  ICON_SOURCE=docs/janus-circle.png
  ICON_SIZES="16:icon_16x16 32:icon_16x16@2x 32:icon_32x32 64:icon_32x32@2x 128:icon_128x128 256:icon_128x128@2x 256:icon_256x256"
fi

[[ "$(uname -s)" == Darwin ]] || { echo "bundle-macos: macOS only (needs sips, iconutil, codesign)" >&2; exit 2; }
for tool in sips iconutil codesign plutil; do
  command -v "$tool" >/dev/null || { echo "bundle-macos: $tool is required" >&2; exit 2; }
done
[[ -f "$SOURCE" && -x "$SOURCE" ]] || { echo "bundle-macos: $SOURCE is missing or not executable" >&2; exit 2; }
[[ -f "$ICON_SOURCE" ]] || { echo "bundle-macos: $ICON_SOURCE is missing" >&2; exit 2; }
if [[ -z "$VERSION" ]]; then
  # "janus 1.17.0 (caddy 2.11.4)" -> 1.17.0
  VERSION="$("$SOURCE" version | awk '{print $2}')"
fi
[[ -n "$VERSION" ]] || { echo "bundle-macos: cannot determine the version" >&2; exit 2; }
# CFBundleShortVersionString is x.y.z; the full string (a dev build's
# pseudo-version and +dirty marker) goes in CFBundleVersion.
SHORT_VERSION="$(printf '%s' "$VERSION" | sed -E 's/^v?([0-9]+\.[0-9]+\.[0-9]+).*/\1/')"
[[ "$SHORT_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || SHORT_VERSION=0.0.0

mkdir -p "$(dirname "$DEST")"

stage="$(mktemp -d "$(dirname "$DEST")/.Janus.app.XXXXXX")"
trap 'rm -rf "$stage"' EXIT
app="$stage/Janus.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"

cp "$SOURCE" "$app/Contents/MacOS/janus"
chmod 0755 "$app/Contents/MacOS/janus"

# The icon, at the sizes the source can fill without upscaling.
iconset="$stage/Janus.iconset"
mkdir -p "$iconset"
for spec in $ICON_SIZES; do
  size=${spec%%:*}
  name=${spec#*:}
  sips -z "$size" "$size" "$ICON_SOURCE" --out "$iconset/$name.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$app/Contents/Resources/Janus.icns"

cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>$IDENTIFIER</string>
	<key>CFBundleName</key>
	<string>Janus</string>
	<key>CFBundleDisplayName</key>
	<string>Janus</string>
	<key>CFBundleExecutable</key>
	<string>janus</string>
	<key>CFBundleIconFile</key>
	<string>Janus.icns</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>$SHORT_VERSION</string>
	<key>CFBundleVersion</key>
	<string>$VERSION</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSLocalNetworkUsageDescription</key>
	<string>Janus announces this Mac's local names (janus.local and each app's .local name) and answers the devices on your network that look them up.</string>
</dict>
</plist>
PLIST
plutil -lint "$app/Contents/Info.plist" >/dev/null

# Sign the executable under the identifier, then the bundle, which takes
# the identifier from Info.plist.
codesign -s - -f --identifier "$IDENTIFIER" "$app/Contents/MacOS/janus"
codesign -s - -f "$app"
codesign --verify --strict "$app"

if [[ -e "$DEST" ]]; then
  [[ -d "$DEST" && -f "$DEST/Contents/Info.plist" ]] || { echo "bundle-macos: $DEST exists and is not an application bundle" >&2; exit 2; }
  old="$(mktemp -d "$(dirname "$DEST")/.Janus.app.old.XXXXXX")"
  mv "$DEST" "$old/Janus.app"
  mv "$app" "$DEST"
  rm -rf "$old"
else
  mv "$app" "$DEST"
fi
printf '  -> %s (%s, %s)\n' "$DEST" "$IDENTIFIER" "$VERSION"
