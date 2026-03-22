#!/bin/bash
set -e

APP="ElTerminalo.app"
BINARY="elterminalo"
LOGO="assets/logo.png"

echo "Building ${APP}..."

# Create bundle structure
mkdir -p "${APP}/Contents/MacOS"
mkdir -p "${APP}/Contents/Resources"

# Copy binary
cp "${BINARY}" "${APP}/Contents/MacOS/${BINARY}-bin"

# Generate .icns from logo
if [ -f "${LOGO}" ]; then
  ICONSET_DIR=$(mktemp -d)/ElTerminalo.iconset
  mkdir -p "${ICONSET_DIR}"
  sips -z 16 16     "${LOGO}" --out "${ICONSET_DIR}/icon_16x16.png"      > /dev/null 2>&1
  sips -z 32 32     "${LOGO}" --out "${ICONSET_DIR}/icon_16x16@2x.png"   > /dev/null 2>&1
  sips -z 32 32     "${LOGO}" --out "${ICONSET_DIR}/icon_32x32.png"      > /dev/null 2>&1
  sips -z 64 64     "${LOGO}" --out "${ICONSET_DIR}/icon_32x32@2x.png"   > /dev/null 2>&1
  sips -z 128 128   "${LOGO}" --out "${ICONSET_DIR}/icon_128x128.png"    > /dev/null 2>&1
  sips -z 256 256   "${LOGO}" --out "${ICONSET_DIR}/icon_128x128@2x.png" > /dev/null 2>&1
  sips -z 256 256   "${LOGO}" --out "${ICONSET_DIR}/icon_256x256.png"    > /dev/null 2>&1
  sips -z 512 512   "${LOGO}" --out "${ICONSET_DIR}/icon_256x256@2x.png" > /dev/null 2>&1
  sips -z 512 512   "${LOGO}" --out "${ICONSET_DIR}/icon_512x512.png"    > /dev/null 2>&1
  sips -z 1024 1024 "${LOGO}" --out "${ICONSET_DIR}/icon_512x512@2x.png" > /dev/null 2>&1
  iconutil -c icns "${ICONSET_DIR}" -o "${APP}/Contents/Resources/iconfile.icns"
  rm -rf "$(dirname "${ICONSET_DIR}")"
  echo "App icon generated from ${LOGO}"
fi

# Create launcher script
cat > "${APP}/Contents/MacOS/ElTerminalo" << 'EOF'
#!/bin/bash
DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="${DIR}/elterminalo-bin"

osascript - "$BIN" << 'APPLESCRIPT'
on run argv
    set binPath to item 1 of argv
    tell application "Terminal"
        activate
        do script "\"" & binPath & "\"; exit"
    end tell
end run
APPLESCRIPT
EOF
chmod +x "${APP}/Contents/MacOS/ElTerminalo"

# Create Info.plist
cat > "${APP}/Contents/Info.plist" << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleExecutable</key>
    <string>ElTerminalo</string>
    <key>CFBundleIdentifier</key>
    <string>com.elterminalo.app</string>
    <key>CFBundleName</key>
    <string>El Terminalo</string>
    <key>CFBundleDisplayName</key>
    <string>El Terminalo</string>
    <key>CFBundleVersion</key>
    <string>0.1.0</string>
    <key>CFBundleShortVersionString</key>
    <string>0.1.0</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>LSMinimumSystemVersion</key>
    <string>11.0</string>
    <key>CFBundleIconFile</key>
    <string>iconfile</string>
    <key>NSHighResolutionCapable</key>
    <true/>
</dict>
</plist>
EOF

echo "Built ${APP}"
echo ""
echo "Launch:  open ${APP}"
echo "Install: cp -r ${APP} /Applications/"
