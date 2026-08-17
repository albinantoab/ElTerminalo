# Contributing to El Terminalo

## Development Setup

1. Install prerequisites: Go 1.24+, Node.js 18+, Wails CLI
2. Clone the repository
3. Install frontend dependencies: `cd frontend && npm install`
4. Run in dev mode: `wails dev`

## Code Style

### Go
- Follow [Effective Go](https://go.dev/doc/effective_go) conventions
- Run `golangci-lint run` before submitting
- All exported types and functions must have doc comments

### TypeScript
- Strict mode enabled
- No `any` types unless absolutely necessary
- Use `const` by default, `let` when reassignment is needed

## Pull Requests

1. Create a feature branch from `main`
2. Make your changes with clear commit messages
3. Ensure `wails build` succeeds
4. Submit a PR with a description of what and why

## Making a Release

### Prerequisites

- Go 1.24+, Node.js 18+, Wails CLI installed
- [GitHub CLI](https://cli.github.com/) (`gh`) installed and authenticated
- (Optional) A code signing identity — set `CODESIGN_IDENTITY` env var. Without it, the app is signed ad-hoc.

### Steps

1. **Bump the version** in the `VERSION` file at the repo root:
   ```bash
   echo "1.2.0" > VERSION
   ```

2. **Commit the version bump:**
   ```bash
   git add VERSION
   git commit -m "chore: version bump to 1.2.0"
   ```

3. **Set up notarization credentials — once, ever.** Releases are signed *and*
   notarized; the build refuses to produce artifacts without this. Store an
   app-specific password in the keychain so you never type it again:
   ```bash
   xcrun notarytool store-credentials "elterminalo" \
     --apple-id you@example.com \
     --team-id XXXXXXXXXX \
     --password abcd-efgh-ijkl-mnop
   ```
   Create the app-specific password at [appleid.apple.com](https://appleid.apple.com)
   → Sign-In and Security → App-Specific Passwords. The team ID is on
   [developer.apple.com](https://developer.apple.com/account) under Membership.

4. **Run the release build:**
   ```bash
   NOTARIZE_KEYCHAIN_PROFILE=elterminalo make release
   # or with an explicit version:
   # NOTARIZE_KEYCHAIN_PROFILE=elterminalo ./scripts/release.sh 1.2.0
   ```
   This will:
   - Validate credentials and tooling *before* building, so mistakes fail in seconds
   - Clean `build/bin` and `release/` (a stale bundle must never be republished)
   - Build the app with Wails (version baked into the binary via ldflags)
   - Update `Info.plist` and verify the six `NS*UsageDescription` privacy strings
   - Code sign the `.app` with Developer ID + Hardened Runtime, then verify entitlements
   - **Notarize and staple** the app, requiring an `Accepted` status from Apple
   - Create a `.dmg` (also notarized and stapled) and a `.zip` for the auto-updater
   - Re-verify the *packaged* artifacts: signature, stapled ticket, and Gatekeeper
   - Generate SHA-256 checksums

   Every step is a hard failure. If the script completes, the artifacts are
   publishable; if it stops, nothing in `release/` should be uploaded.

   > **Never paste credentials from a notes app, a doc, or a chat window.**
   > Smart quotes (`“ ”`) survive as literal bytes inside the value and silently
   > corrupt it. The script rejects non-ASCII credentials for this reason.

   For local testing only, `ALLOW_ADHOC_SIGN=1 make release` produces artifacts
   named `-ADHOC-DO-NOT-DISTRIBUTE`. Ad-hoc signatures cannot be notarized, and
   macOS will not hold their folder-access grants — never publish one.

   Artifacts are written to the `release/` directory.

5. **Tag the release and push:**
   ```bash
   git tag v1.2.0
   git push origin main
   git push origin v1.2.0
   ```

6. **Create the GitHub release:**
   ```bash
   gh release create v1.2.0 \
     release/ElTerminalo-1.2.0-macos-arm64.dmg \
     release/ElTerminalo-1.2.0-macos-arm64.zip \
     release/checksums-sha256.txt \
     --title "v1.2.0" --generate-notes
   ```

The `.zip` asset is required for the in-app auto-updater to work. Always include it alongside the `.dmg`.
