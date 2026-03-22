#!/bin/bash
set -e

# ── ElTerminalo Release Builder ──
# Builds a signed .app bundle and creates a .dmg installer.
#
# Usage:
#   ./scripts/release.sh              # builds with version from VERSION file
#   ./scripts/release.sh 1.2.0        # builds with explicit version

VERSION="${1:-$(cat VERSION 2>/dev/null || echo "0.1.0")}"
APP_NAME="ElTerminalo"
APP="${APP_NAME}.app"
DMG="${APP_NAME}-${VERSION}-macos-arm64.dmg"
RELEASE_DIR="release"
LOGO="assets/logo.png"
WAILS="${HOME}/go/bin/wails"

# Use wails from PATH if available, otherwise try ~/go/bin
command -v wails > /dev/null 2>&1 && WAILS="wails"

echo "╔══════════════════════════════════════╗"
echo "║   El Terminalo Release Builder       ║"
echo "║   Version: ${VERSION}                      ║"
echo "╚══════════════════════════════════════╝"
echo ""

# ── Step 1: Clean ──
echo "→ Cleaning previous builds..."
rm -rf "${RELEASE_DIR}"
mkdir -p "${RELEASE_DIR}"

# ── Step 2: Build with Wails ──
echo "→ Building application with Wails..."
LDFLAGS="-X 'main.Version=${VERSION}'"
${WAILS} build -ldflags "${LDFLAGS}" 2>&1 | grep -E "•|Built|Error" || true

if [ ! -d "build/bin/${APP}" ]; then
  echo "✗ Wails build failed — build/bin/${APP} not found"
  exit 1
fi

echo "✓ Wails build complete"

# ── Step 3: Update version in Info.plist ──
echo "→ Setting version to ${VERSION}..."
PLIST="build/bin/${APP}/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion ${VERSION}" "${PLIST}" 2>/dev/null || true
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString ${VERSION}" "${PLIST}" 2>/dev/null || true

# ── Step 4: Code sign (ad-hoc if no identity provided) ──
SIGN_IDENTITY="${CODESIGN_IDENTITY:--}"
echo "→ Code signing with identity: ${SIGN_IDENTITY}"
codesign --force --deep --sign "${SIGN_IDENTITY}" "build/bin/${APP}" 2>/dev/null || {
  echo "  ⚠ Code signing failed (app will still work but macOS may show warnings)"
}

# ── Step 5: Create DMG ──
echo "→ Creating DMG installer..."
DMG_STAGING=$(mktemp -d)
cp -R "build/bin/${APP}" "${DMG_STAGING}/"
ln -s /Applications "${DMG_STAGING}/Applications"

# Create the DMG
hdiutil create \
  -volname "El Terminalo" \
  -srcfolder "${DMG_STAGING}" \
  -ov \
  -format UDZO \
  -imagekey zlib-level=9 \
  "${RELEASE_DIR}/${DMG}" \
  > /dev/null 2>&1

rm -rf "${DMG_STAGING}"
echo "✓ DMG created"

# ── Step 6: Also copy the .app for direct distribution ──
cp -R "build/bin/${APP}" "${RELEASE_DIR}/${APP}"

# ── Step 7: Create checksums ──
echo "→ Generating checksums..."
cd "${RELEASE_DIR}"
shasum -a 256 "${DMG}" > "${DMG}.sha256"
cd ..

# ── Done ──
DMG_SIZE=$(du -h "${RELEASE_DIR}/${DMG}" | cut -f1 | xargs)
echo ""
echo "╔══════════════════════════════════════╗"
echo "║   Release build complete!            ║"
echo "╠══════════════════════════════════════╣"
echo "║                                      ║"
echo "  Version:  ${VERSION}"
echo "  DMG:      ${RELEASE_DIR}/${DMG} (${DMG_SIZE})"
echo "  App:      ${RELEASE_DIR}/${APP}"
echo "  Checksum: ${RELEASE_DIR}/${DMG}.sha256"
echo "║                                      ║"
echo "╚══════════════════════════════════════╝"
echo ""
echo "To create a GitHub release:"
echo "  git tag v${VERSION}"
echo "  git push origin v${VERSION}"
echo "  gh release create v${VERSION} ${RELEASE_DIR}/${DMG} --title \"v${VERSION}\" --generate-notes"
