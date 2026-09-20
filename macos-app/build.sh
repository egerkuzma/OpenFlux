#!/bin/bash
# Build OpenFlux.app (menu-bar app, no Xcode) and install it to Applications.
# The bundle is assembled in a temp dir and never left inside the project tree,
# so only the installed copy is ever registered with LaunchServices (no dupes).
# Usage: ./build.sh              build + install to /Applications (or ~/Applications)
#        ./build.sh --no-install build only; prints the temp bundle path
set -eu
cd "$(dirname "$0")"

APP="OpenFlux.app"
EXE="OpenFlux"
BUNDLE_ID="com.openflux.menubar"
STAGE="$(mktemp -d)/$APP"
LSR=/System/Library/Frameworks/CoreServices.framework/Versions/A/Frameworks/LaunchServices.framework/Versions/A/Support/lsregister

echo "== compile =="
mkdir -p "$STAGE/Contents/MacOS" "$STAGE/Contents/Resources"
swiftc -O -o "$STAGE/Contents/MacOS/$EXE" OpenFluxMenu.swift \
  -framework AppKit -framework Foundation

echo "== Info.plist =="
cat > "$STAGE/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>OpenFlux</string>
  <key>CFBundleDisplayName</key><string>OpenFlux</string>
  <key>CFBundleIdentifier</key><string>$BUNDLE_ID</string>
  <key>CFBundleExecutable</key><string>$EXE</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>1.0</string>
  <key>CFBundleVersion</key><string>1</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <key>LSUIElement</key><true/>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

echo "== icon =="
swiftc -O -o /tmp/oflx-mkicon makeicon.swift -framework AppKit -framework Foundation
/tmp/oflx-mkicon /tmp/oflx-icon >/dev/null
iconutil -c icns /tmp/oflx-icon.iconset -o "$STAGE/Contents/Resources/AppIcon.icns"

echo "built (staged): $STAGE"

if [ "${1:-}" = "--no-install" ]; then
  echo "(no install) bundle left at: $STAGE"
  exit 0
fi

echo "== install =="
if [ -w /Applications ]; then DEST="/Applications"; else DEST="$HOME/Applications"; fi
mkdir -p "$DEST"

# Replacing the bundle means killing the running app, and a signal does not run
# its teardown: openflux would outlive it with utun and the routes still
# installed, while the fresh app reports "disconnected" and knows nothing about
# the process holding the default route. So refuse, and say what to do.
if pgrep -f '/usr/local/bin/openflux' >/dev/null 2>&1; then
  echo "туннель сейчас работает." >&2
  echo "отключи его в меню приложения и выйди из приложения, потом повтори установку." >&2
  echo "(если туннель нужно оставить поднятым, ставь с --force — приложение потеряет с ним связь)" >&2
  [ "${1:-}" = "--force" ] || exit 1
fi

# Stop a running instance so we can replace the bundle cleanly.
pkill -f "$DEST/$APP/Contents/MacOS/$EXE" 2>/dev/null || true
rm -rf "$DEST/$APP"
cp -R "$STAGE" "$DEST/"
rm -rf "$(dirname "$STAGE")"                       # drop the temp staging dir
"$LSR" -f "$DEST/$APP" 2>/dev/null || true         # refresh LaunchServices/icon
touch "$DEST/$APP"
echo "installed: $DEST/$APP"
echo "launch:    open \"$DEST/$APP\""
