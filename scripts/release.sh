#!/bin/bash
#
# ── ElTerminalo Release Builder ──
#
# Builds, signs, notarizes, staples and verifies a distributable macOS release.
#
# Usage:
#   ./scripts/release.sh              # version from the VERSION file
#   ./scripts/release.sh 1.2.0        # explicit version
#
# Credentials — pick ONE (a keychain profile is strongly preferred):
#
#   Keychain profile (recommended, set up once):
#     xcrun notarytool store-credentials "elterminalo" \
#       --apple-id you@example.com --team-id Z4D9F3U5MP --password abcd-efgh-ijkl-mnop
#     NOTARIZE_KEYCHAIN_PROFILE=elterminalo ./scripts/release.sh
#
#   Inline (type the quotes yourself — never paste from a notes app or a doc;
#   smart quotes are rejected below because they silently corrupt credentials):
#     NOTARIZE_APPLE_ID='you@example.com' \
#     NOTARIZE_TEAM_ID='Z4D9F3U5MP' \
#     NOTARIZE_PASSWORD='abcd-efgh-ijkl-mnop' \
#     ./scripts/release.sh
#
# Local testing only — produces clearly-marked, undistributable artifacts:
#     ALLOW_ADHOC_SIGN=1 ./scripts/release.sh
#
# Design rule: this script must never emit a release artifact that is not
# signed, notarized, stapled and verified. Every check below is a hard failure.
# An unnotarized build breaks macOS privacy (TCC): the app's folder grants stop
# applying mid-session and the shell starts returning "Operation not permitted"
# in ~/Documents until it is relaunched.

set -Eeuo pipefail

# ── Output helpers ──
step()  { printf '\n\033[1;35m→ %s\033[0m\n' "$*"; }
ok()    { printf '  \033[0;32m✓\033[0m %s\n' "$*"; }
warn()  { printf '  \033[0;33m⚠\033[0m %s\n' "$*"; }
info()  { printf '    %s\n' "$*"; }
die()   { printf '\n\033[0;31m✗ %s\033[0m\n' "$1" >&2; shift; for l in "$@"; do printf '  %s\n' "$l" >&2; done; printf '\n' >&2; exit 1; }

trap 'die "Release aborted at line ${LINENO}." "Nothing in ${RELEASE_DIR:-release}/ should be published."' ERR

# ── Temp dir bookkeeping ──
TMPDIRS=()
cleanup() {
  local d
  for d in ${TMPDIRS[@]+"${TMPDIRS[@]}"}; do [ -n "$d" ] && rm -rf "$d"; done
  if [ -n "${MOUNTED_DMG:-}" ]; then hdiutil detach "${MOUNTED_DMG}" -quiet 2>/dev/null || true; fi
}
trap cleanup EXIT
mktempdir() { local d; d=$(mktemp -d); TMPDIRS+=("$d"); printf '%s' "$d"; }

# ── Config ──
APP_NAME="ElTerminalo"
APP="${APP_NAME}.app"
RELEASE_DIR="release"
BUILD_APP="build/bin/${APP}"
ENTITLEMENTS="build/darwin/entitlements.plist"
VOLNAME="El Terminalo"

VERSION="${1:-$(cat VERSION 2>/dev/null || echo "")}"
[ -n "${VERSION}" ] || die "No version given and VERSION file is missing or empty."

# The updater compares versions as x.y.z integers; anything else breaks update
# detection for every user already running an older build.
if ! [[ "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  die "Version '${VERSION}' is not x.y.z." \
      "internal/updater compares three dotted integers; other formats never" \
      "register as an available update."
fi

ARCH=$(uname -m)
case "$ARCH" in
  arm64)  ARCH_LABEL="arm64" ;;
  x86_64) ARCH_LABEL="amd64" ;;
  *)      die "Unsupported architecture '${ARCH}'." ;;
esac

ADHOC=0
[ "${ALLOW_ADHOC_SIGN:-}" = "1" ] && ADHOC=1

if [ "${ADHOC}" -eq 1 ]; then
  SUFFIX="-ADHOC-DO-NOT-DISTRIBUTE"
else
  SUFFIX=""
fi
DMG="${APP_NAME}-${VERSION}-macos-${ARCH_LABEL}${SUFFIX}.dmg"
ZIP="${APP_NAME}-${VERSION}-macos-${ARCH_LABEL}${SUFFIX}.zip"

printf '\n\033[1;35m╔════════════════════════════════════════════╗\033[0m\n'
printf '\033[1;35m║\033[0m  El Terminalo release  ·  v%-16s\033[1;35m║\033[0m\n' "${VERSION}"
printf '\033[1;35m╚════════════════════════════════════════════╝\033[0m\n'

# ═══════════════════════════════════════════════════════════════════════
# Step 1 — Preflight
#
# Everything that can fail cheaply fails here, before a five-minute build.
# ═══════════════════════════════════════════════════════════════════════
step "Preflight"

WAILS="wails"
command -v wails >/dev/null 2>&1 || WAILS="${HOME}/go/bin/wails"
[ -x "${WAILS}" ] || command -v "${WAILS}" >/dev/null 2>&1 \
  || die "wails not found on PATH or at ~/go/bin/wails." \
         "Install it: go install github.com/wailsapp/wails/v2/cmd/wails@latest"

for tool in codesign xcrun hdiutil ditto shasum security; do
  command -v "$tool" >/dev/null 2>&1 || die "Required tool '$tool' not found."
done
[ -x /usr/libexec/PlistBuddy ] || die "PlistBuddy not found at /usr/libexec/PlistBuddy."
[ -f "${ENTITLEMENTS}" ] || die "Entitlements missing: ${ENTITLEMENTS}"

xcrun notarytool --version >/dev/null 2>&1 \
  || die "notarytool unavailable." "Install the Xcode command line tools: xcode-select --install"
ok "toolchain present"

# ── Signing identity ──
SIGN_IDENTITY="${CODESIGN_IDENTITY:-}"
if [ -z "${SIGN_IDENTITY}" ]; then
  SIGN_IDENTITY=$(security find-identity -v -p codesigning 2>/dev/null \
    | grep "Developer ID Application" | head -1 | sed 's/.*"\(.*\)".*/\1/') || true
fi

if [ -z "${SIGN_IDENTITY}" ] && [ "${ADHOC}" -eq 0 ]; then
  die "No 'Developer ID Application' certificate found." \
      "Releases must be signed with a stable Developer ID. Ad-hoc and unsigned" \
      "builds break macOS privacy (TCC): folder grants stop applying mid-session," \
      "so 'ls ~/Documents' starts failing with 'Operation not permitted'." \
      "" \
      "Install a Developer ID certificate, or set CODESIGN_IDENTITY." \
      "For local testing only: ALLOW_ADHOC_SIGN=1 ./scripts/release.sh"
fi
[ "${ADHOC}" -eq 0 ] && ok "signing identity: ${SIGN_IDENTITY}"

# ── Notarization credentials ──
#
# Validated here, before the build, and validated strictly. A credential that
# is merely non-empty is not enough: curly quotes copied from a notes app or a
# web page survive as literal U+201C/U+201D bytes inside the value, pass a
# [ -n "$VAR" ] test, and are then rejected by Apple — or worse, cause the
# notarize branch to be skipped while the release still builds.
NOTARY_AUTH=()

assert_clean_credential() {
  local name="$1" value="$2"
  if printf '%s' "${value}" | LC_ALL=C grep -q '[^ -~]'; then
    local dump
    dump=$(printf '%s' "${value}" | head -c 32 | xxd | head -2)
    die "${name} contains non-ASCII characters." \
        "This is almost always curly quotes (U+201C \" / U+201D \") pasted from" \
        "a notes app, a document, or a chat window. They become part of the value:" \
        "" \
        "${dump}" \
        "" \
        "Retype the value with straight quotes, or avoid the problem entirely:" \
        "  xcrun notarytool store-credentials \"elterminalo\" --apple-id … --team-id … --password …" \
        "  NOTARIZE_KEYCHAIN_PROFILE=elterminalo ./scripts/release.sh"
  fi
  case "${value}" in
    " "*|*" ") die "${name} has leading or trailing whitespace." "Check the quoting in your command." ;;
  esac
}

if [ "${ADHOC}" -eq 1 ]; then
  warn "AD-HOC BUILD — local testing only"
  info "Artifacts will be named ${SUFFIX#-} and must never be published."
  info "Ad-hoc signatures cannot be notarized, so TCC grants will be unstable."
elif [ -n "${NOTARIZE_KEYCHAIN_PROFILE:-}" ]; then
  assert_clean_credential "NOTARIZE_KEYCHAIN_PROFILE" "${NOTARIZE_KEYCHAIN_PROFILE}"
  NOTARY_AUTH=(--keychain-profile "${NOTARIZE_KEYCHAIN_PROFILE}")
  ok "notarization: keychain profile '${NOTARIZE_KEYCHAIN_PROFILE}'"
else
  MISSING=()
  [ -n "${NOTARIZE_APPLE_ID:-}" ] || MISSING+=("NOTARIZE_APPLE_ID")
  [ -n "${NOTARIZE_TEAM_ID:-}"  ] || MISSING+=("NOTARIZE_TEAM_ID")
  [ -n "${NOTARIZE_PASSWORD:-}" ] || MISSING+=("NOTARIZE_PASSWORD")
  if [ ${#MISSING[@]} -gt 0 ]; then
    die "Notarization credentials missing: ${MISSING[*]}" \
        "Notarization is mandatory for a signed release — it is not optional and" \
        "is never skipped. Without a stapled ticket macOS must validate this app" \
        "online every time it re-checks a privacy decision, and a failed check" \
        "denies folder access mid-session." \
        "" \
        "Set up a keychain profile once:" \
        "  xcrun notarytool store-credentials \"elterminalo\" \\" \
        "    --apple-id you@example.com --team-id XXXXXXXXXX --password abcd-efgh-ijkl-mnop" \
        "  NOTARIZE_KEYCHAIN_PROFILE=elterminalo ./scripts/release.sh" \
        "" \
        "For local testing only: ALLOW_ADHOC_SIGN=1 ./scripts/release.sh"
  fi

  assert_clean_credential "NOTARIZE_APPLE_ID" "${NOTARIZE_APPLE_ID}"
  assert_clean_credential "NOTARIZE_TEAM_ID"  "${NOTARIZE_TEAM_ID}"
  assert_clean_credential "NOTARIZE_PASSWORD" "${NOTARIZE_PASSWORD}"

  case "${NOTARIZE_APPLE_ID}" in
    *@*.*) ;;
    *) die "NOTARIZE_APPLE_ID ('${NOTARIZE_APPLE_ID}') is not an email address." ;;
  esac
  if ! [[ "${NOTARIZE_TEAM_ID}" =~ ^[A-Z0-9]{10}$ ]]; then
    die "NOTARIZE_TEAM_ID ('${NOTARIZE_TEAM_ID}') is not a 10-character team ID." \
        "Find yours at https://developer.apple.com/account under Membership." \
        "Your signing certificate says: ${SIGN_IDENTITY}"
  fi
  if ! [[ "${NOTARIZE_PASSWORD}" =~ ^[a-z]{4}(-[a-z]{4}){3}$ ]]; then
    warn "NOTARIZE_PASSWORD is not in app-specific-password form (xxxx-xxxx-xxxx-xxxx)."
    info "Continuing — Apple will reject it if it is wrong."
  fi
  NOTARY_AUTH=(--apple-id "${NOTARIZE_APPLE_ID}" --team-id "${NOTARIZE_TEAM_ID}" --password "${NOTARIZE_PASSWORD}")
  ok "notarization credentials validated"
fi

# ═══════════════════════════════════════════════════════════════════════
# Step 2 — Clean
#
# build/bin is cleaned too. Previously only release/ was cleared, so a failed
# Wails build left a stale bundle behind that passed the existence check and
# got published with the new version number stamped on the old binary.
# ═══════════════════════════════════════════════════════════════════════
step "Clean"
rm -rf "${RELEASE_DIR}" "build/bin"
mkdir -p "${RELEASE_DIR}"
ok "removed build/bin and ${RELEASE_DIR}"

# ═══════════════════════════════════════════════════════════════════════
# Step 3 — Build
# ═══════════════════════════════════════════════════════════════════════
step "Build (wails)"
BUILD_LOG="$(mktempdir)/build.log"
if ! "${WAILS}" build -ldflags "-X 'main.Version=${VERSION}'" >"${BUILD_LOG}" 2>&1; then
  printf '\n'; cat "${BUILD_LOG}" >&2
  die "Wails build failed." "Full output above."
fi
grep -E "•|Built" "${BUILD_LOG}" || true
[ -d "${BUILD_APP}" ] || die "Build reported success but ${BUILD_APP} does not exist."
ok "built ${BUILD_APP}"

# ═══════════════════════════════════════════════════════════════════════
# Step 4 — Stamp version, verify privacy strings
#
# The NS*UsageDescription keys are what let macOS show a consent prompt instead
# of hard-denying with EPERM. If Wails ever regenerates Info.plist from a
# template without them, the app fails closed on TCC-protected folders.
# ═══════════════════════════════════════════════════════════════════════
step "Info.plist"
PLIST="${BUILD_APP}/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion ${VERSION}" "${PLIST}" >/dev/null \
  || die "Could not set CFBundleVersion in ${PLIST}"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString ${VERSION}" "${PLIST}" >/dev/null \
  || die "Could not set CFBundleShortVersionString in ${PLIST}"

STAMPED=$(/usr/libexec/PlistBuddy -c "Print :CFBundleShortVersionString" "${PLIST}")
[ "${STAMPED}" = "${VERSION}" ] || die "Version stamp did not take: plist says '${STAMPED}', expected '${VERSION}'."
ok "version stamped: ${VERSION}"

for key in NSDesktopFolderUsageDescription NSDocumentsFolderUsageDescription \
           NSDownloadsFolderUsageDescription NSRemovableVolumesUsageDescription \
           NSNetworkVolumesUsageDescription NSFileProviderDomainUsageDescription; do
  /usr/libexec/PlistBuddy -c "Print :${key}" "${PLIST}" >/dev/null 2>&1 \
    || die "Info.plist is missing ${key}." \
           "Without this string macOS cannot prompt for the folder and denies" \
           "access outright. Restore it in build/darwin/Info.plist."
done
ok "all 6 privacy usage strings present"

# ═══════════════════════════════════════════════════════════════════════
# Step 5 — Code sign
#
# No --deep: Apple deprecated it and it can produce signatures the notary
# service rejects. This bundle has no nested code; if that changes, sign the
# nested items inside-out before this call.
# ═══════════════════════════════════════════════════════════════════════
step "Code sign"
if [ "${ADHOC}" -eq 1 ]; then
  codesign --force --options runtime --timestamp=none \
    --entitlements "${ENTITLEMENTS}" --sign "-" "${BUILD_APP}" \
    || die "Ad-hoc signing failed."
  ok "ad-hoc signed (not distributable)"
else
  codesign --force --options runtime --timestamp \
    --entitlements "${ENTITLEMENTS}" --sign "${SIGN_IDENTITY}" "${BUILD_APP}" \
    || die "Code signing failed."
  codesign --verify --strict --verbose=2 "${BUILD_APP}" 2>/dev/null \
    || die "Signature verification failed immediately after signing."

  for ent in com.apple.security.cs.allow-jit \
             com.apple.security.cs.allow-unsigned-executable-memory \
             com.apple.security.cs.disable-library-validation; do
    codesign -d --entitlements - "${BUILD_APP}" 2>/dev/null | grep -q "${ent}" \
      || die "Entitlement ${ent} did not survive signing." \
             "The WKWebView frontend and the embedded llama.cpp need these under" \
             "Hardened Runtime; without them the app crashes on launch."
  done
  ok "signed and verified · ${SIGN_IDENTITY}"
fi

# ═══════════════════════════════════════════════════════════════════════
# Step 6 — Notarize and staple the app
#
# notarytool's exit code is not trusted on its own — the JSON status is read
# and must be exactly "Accepted". On anything else the notary log is printed,
# which names the offending file and reason.
# ═══════════════════════════════════════════════════════════════════════
notarize() {
  local target="$1" label="$2" submission json status subid

  step "Notarize ${label}"
  info "submitting — this usually takes 1-5 minutes"

  if [ "${target}" = "${BUILD_APP}" ]; then
    submission="$(mktempdir)/notarize.zip"
    ditto -c -k --sequesterRsrc --keepParent "${target}" "${submission}" \
      || die "Could not build the notarization archive."
  else
    submission="${target}"
  fi

  json=$(xcrun notarytool submit "${submission}" "${NOTARY_AUTH[@]}" --wait --output-format json 2>&1) || true

  status=$(printf '%s\n' "${json}" | grep -o '"status"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*"\([^"]*\)"$/\1/') || true
  subid=$(printf '%s\n'  "${json}" | grep -o '"id"[[:space:]]*:[[:space:]]*"[^"]*"'     | head -1 | sed 's/.*"\([^"]*\)"$/\1/') || true

  if [ "${status}" != "Accepted" ]; then
    printf '\n%s\n\n' "${json}" >&2
    if [ -n "${subid}" ]; then
      printf '  ── notary log ──\n' >&2
      xcrun notarytool log "${subid}" "${NOTARY_AUTH[@]}" >&2 2>&1 || true
    fi
    die "Notarization of ${label} was not accepted (status: ${status:-unknown})." \
        "Details above. Nothing has been published."
  fi
  ok "accepted by Apple (${subid})"

  step "Staple ${label}"
  xcrun stapler staple "${target}" >/dev/null \
    || die "Stapling ${label} failed even though notarization was accepted."
  xcrun stapler validate "${target}" >/dev/null \
    || die "Stapled ticket did not validate on ${label}."
  ok "ticket stapled and validated"
}

if [ "${ADHOC}" -eq 1 ]; then
  warn "skipping notarization — ad-hoc build"
else
  notarize "${BUILD_APP}" "app bundle"
fi

# ═══════════════════════════════════════════════════════════════════════
# Step 7 — DMG (built from the stapled bundle, then itself notarized)
# ═══════════════════════════════════════════════════════════════════════
step "DMG"
DMG_STAGING=$(mktempdir)
ditto "${BUILD_APP}" "${DMG_STAGING}/${APP}" || die "Could not stage the app for the DMG."
ln -s /Applications "${DMG_STAGING}/Applications"
hdiutil create -volname "${VOLNAME}" -srcfolder "${DMG_STAGING}" -ov \
  -format UDZO -imagekey zlib-level=9 "${RELEASE_DIR}/${DMG}" >/dev/null \
  || die "hdiutil failed to create the DMG."
ok "created ${DMG}"

# The app inside is already stapled, so Gatekeeper is satisfied either way —
# but notarizing the disk image itself avoids the "damaged and can't be opened"
# warning some users hit when opening the DMG directly from a browser download.
[ "${ADHOC}" -eq 1 ] || notarize "${RELEASE_DIR}/${DMG}" "DMG"

# ═══════════════════════════════════════════════════════════════════════
# Step 8 — ZIP for the auto-updater
#
# ditto, not zip: the updater extracts with Go's archive/zip, and `zip` plus
# that extractor turns symlinks into regular files containing the link text.
# Harmless for today's flat bundle, fatal the moment a framework is added.
# ═══════════════════════════════════════════════════════════════════════
step "ZIP (auto-updater)"
ditto -c -k --sequesterRsrc --keepParent "${BUILD_APP}" "${RELEASE_DIR}/${ZIP}" \
  || die "Could not create the updater ZIP."
ok "created ${ZIP}"

# ═══════════════════════════════════════════════════════════════════════
# Step 9 — Verify the artifacts that actually ship
#
# Everything above verified build/bin. This verifies what a user downloads,
# after every packaging step that could have damaged it. This is the gate that
# would have caught the unnotarized 0.1.22 release.
# ═══════════════════════════════════════════════════════════════════════
step "Verify shipped artifacts"

verify_app() {
  local path="$1" label="$2"
  codesign --verify --strict --verbose=2 "${path}" 2>/dev/null \
    || die "${label}: code signature is invalid after packaging."
  if [ "${ADHOC}" -eq 0 ]; then
    xcrun stapler validate "${path}" >/dev/null \
      || die "${label}: no stapled notarization ticket." \
             "This is exactly the defect that shipped in 0.1.22 — do not publish."
    local gk
    gk=$(spctl -a -vvv -t exec "${path}" 2>&1 || true)
    printf '%s' "${gk}" | grep -q "accepted" \
      || die "${label}: Gatekeeper rejects this bundle." "$(printf '%s' "${gk}" | sed 's/^/  /')"
  fi
  ok "${label}: signature, staple and Gatekeeper all pass"
}

# ZIP — extract and check what the auto-updater would install
ZIP_CHECK=$(mktempdir)
ditto -x -k "${RELEASE_DIR}/${ZIP}" "${ZIP_CHECK}" || die "Could not extract the ZIP for verification."
[ -d "${ZIP_CHECK}/${APP}" ] || die "ZIP does not contain ${APP} at its root."
verify_app "${ZIP_CHECK}/${APP}" "ZIP payload"

# DMG — mount and check what a user dragging to /Applications would get
MOUNT_POINT=$(mktempdir)
hdiutil attach "${RELEASE_DIR}/${DMG}" -nobrowse -readonly -mountpoint "${MOUNT_POINT}" >/dev/null \
  || die "Could not mount the DMG for verification."
MOUNTED_DMG="${MOUNT_POINT}"
[ -d "${MOUNT_POINT}/${APP}" ] || die "DMG does not contain ${APP}."
verify_app "${MOUNT_POINT}/${APP}" "DMG payload"
hdiutil detach "${MOUNT_POINT}" -quiet
MOUNTED_DMG=""

if [ "${ADHOC}" -eq 0 ]; then
  xcrun stapler validate "${RELEASE_DIR}/${DMG}" >/dev/null \
    || die "The DMG itself carries no stapled ticket."
  ok "DMG image: ticket stapled"
fi

# ═══════════════════════════════════════════════════════════════════════
# Step 10 — Checksums
# ═══════════════════════════════════════════════════════════════════════
step "Checksums"
( cd "${RELEASE_DIR}" && shasum -a 256 "${DMG}" "${ZIP}" > checksums-sha256.txt )
ok "checksums-sha256.txt written"

# ── Done ──
trap - ERR
DMG_SIZE=$(du -h "${RELEASE_DIR}/${DMG}" | cut -f1 | xargs)
ZIP_SIZE=$(du -h "${RELEASE_DIR}/${ZIP}" | cut -f1 | xargs)

printf '\n\033[1;32m╔════════════════════════════════════════════╗\033[0m\n'
if [ "${ADHOC}" -eq 1 ]; then
  printf '\033[1;33m║\033[0m  AD-HOC BUILD — DO NOT DISTRIBUTE          \033[1;33m║\033[0m\n'
else
  printf '\033[1;32m║\033[0m  Release verified and ready to publish      \033[1;32m║\033[0m\n'
fi
printf '\033[1;32m╚════════════════════════════════════════════╝\033[0m\n\n'
printf '  Version    %s\n' "${VERSION}"
printf '  DMG        %s (%s)\n' "${RELEASE_DIR}/${DMG}" "${DMG_SIZE}"
printf '  ZIP        %s (%s)\n' "${RELEASE_DIR}/${ZIP}" "${ZIP_SIZE}"
printf '  Checksums  %s/checksums-sha256.txt\n' "${RELEASE_DIR}"

if [ "${ADHOC}" -eq 1 ]; then
  printf '\n  This build is ad-hoc signed. macOS will not keep its TCC grants\n'
  printf '  reliably, so folder access will break mid-session. Local testing only.\n\n'
  exit 0
fi

printf '\n  Publish:\n'
printf '    git tag v%s && git push origin v%s\n' "${VERSION}" "${VERSION}"
printf '    gh release create v%s \\\n' "${VERSION}"
printf '      %s/%s \\\n' "${RELEASE_DIR}" "${DMG}"
printf '      %s/%s \\\n' "${RELEASE_DIR}" "${ZIP}"
printf '      %s/checksums-sha256.txt \\\n' "${RELEASE_DIR}"
printf '      --title "v%s" --generate-notes\n\n' "${VERSION}"
