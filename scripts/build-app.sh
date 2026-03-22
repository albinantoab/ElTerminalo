#!/bin/bash
set -e

APP="ElTerminalo.app"
BINARY="elterminalo"

echo "Building ${APP}..."

# Create bundle structure
mkdir -p "${APP}/Contents/MacOS"
mkdir -p "${APP}/Contents/Resources"

# Copy binary
cp "${BINARY}" "${APP}/Contents/MacOS/${BINARY}-bin"

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
    <key>NSHighResolutionCapable</key>
    <true/>
</dict>
</plist>
EOF

echo "Built ${APP}"
echo ""
echo "Launch:  open ${APP}"
echo "Install: cp -r ${APP} /Applications/"
