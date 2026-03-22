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

# ── Step 4: Code sign ──
SIGN_IDENTITY="${CODESIGN_IDENTITY:-}"
if [ -z "${SIGN_IDENTITY}" ]; then
  # Auto-detect Developer ID Application certificate
  SIGN_IDENTITY=$(security find-identity -v -p codesigning | grep "Developer ID Application" | head -1 | sed 's/.*"\(.*\)".*/\1/')
fi

if [ -z "${SIGN_IDENTITY}" ]; then
  echo "  ⚠ No Developer ID Application certificate found — using ad-hoc signing"
  echo "  ⚠ Users will see Gatekeeper warnings. Install a Developer ID certificate for proper distribution."
  codesign --force --deep --sign "-" "build/bin/${APP}" 2>/dev/null || true
else
  echo "→ Code signing with: ${SIGN_IDENTITY}"
  codesign --force --options runtime --deep --sign "${SIGN_IDENTITY}" "build/bin/${APP}"
  echo "✓ Code signed"

  # ── Step 4b: Notarize ──
  APPLE_ID="${NOTARIZE_APPLE_ID:-}"
  TEAM_ID="${NOTARIZE_TEAM_ID:-}"
  APP_PASSWORD="${NOTARIZE_PASSWORD:-}"

  if [ -n "${APPLE_ID}" ] && [ -n "${TEAM_ID}" ] && [ -n "${APP_PASSWORD}" ]; then
    echo "→ Creating ZIP for notarization..."
    NOTARIZE_ZIP=$(mktemp -d)/notarize.zip
    ditto -c -k --keepParent "build/bin/${APP}" "${NOTARIZE_ZIP}"

    echo "→ Submitting for notarization (this may take a few minutes)..."
    xcrun notarytool submit "${NOTARIZE_ZIP}" \
      --apple-id "${APPLE_ID}" \
      --team-id "${TEAM_ID}" \
      --password "${APP_PASSWORD}" \
      --wait

    echo "→ Stapling notarization ticket..."
    xcrun stapler staple "build/bin/${APP}"
    echo "✓ Notarization complete"
    rm -f "${NOTARIZE_ZIP}"
  else
    echo "  ⚠ Skipping notarization — set NOTARIZE_APPLE_ID, NOTARIZE_TEAM_ID, and NOTARIZE_PASSWORD to enable"
  fi
fi

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

# ── Step 6: Create ZIP (used by auto-updater) ──
ZIP="${APP_NAME}-${VERSION}-macos-arm64.zip"
echo "→ Creating ZIP for auto-updater..."
cd "build/bin" && zip -qr "../../${RELEASE_DIR}/${ZIP}" "${APP}" && cd ../..
echo "✓ ZIP created"

# ── Step 8: Create checksums ──
echo "→ Generating checksums..."
cd "${RELEASE_DIR}"
shasum -a 256 "${DMG}" "${ZIP}" > checksums-sha256.txt
cd ..

# ── Done ──
DMG_SIZE=$(du -h "${RELEASE_DIR}/${DMG}" | cut -f1 | xargs)
ZIP_SIZE=$(du -h "${RELEASE_DIR}/${ZIP}" | cut -f1 | xargs)
echo ""
echo "╔══════════════════════════════════════╗"
echo "║   Release build complete!            ║"
echo "╠══════════════════════════════════════╣"
echo "║                                      ║"
echo "  Version:   ${VERSION}"
echo "  DMG:       ${RELEASE_DIR}/${DMG} (${DMG_SIZE})"
echo "  ZIP:       ${RELEASE_DIR}/${ZIP} (${ZIP_SIZE})"
echo "  Checksums: ${RELEASE_DIR}/checksums-sha256.txt"
echo "║                                      ║"
echo "╚══════════════════════════════════════╝"
echo ""
echo "To create a GitHub release:"
echo "  git tag v${VERSION}"
echo "  git push origin v${VERSION}"
echo "  gh release create v${VERSION} \\"
echo "    ${RELEASE_DIR}/${DMG} \\"
echo "    ${RELEASE_DIR}/${ZIP} \\"
echo "    ${RELEASE_DIR}/checksums-sha256.txt \\"
echo "    --title \"v${VERSION}\" --generate-notes"
