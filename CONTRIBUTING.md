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

3. **Run the release build:**
   ```bash
   make release
   # or with an explicit version:
   # ./scripts/release.sh 1.2.0
   ```
   This will:
   - Build the app with Wails (version baked into the binary via ldflags)
   - Update `Info.plist` with the version
   - Code sign the `.app` bundle
   - Create a `.dmg` installer and a `.zip` (used by the auto-updater)
   - Generate SHA-256 checksums

   Artifacts are written to the `release/` directory.

4. **Tag the release and push:**
   ```bash
   git tag v1.2.0
   git push origin main
   git push origin v1.2.0
   ```

5. **Create the GitHub release:**
   ```bash
   gh release create v1.2.0 \
     release/ElTerminalo-1.2.0-macos-arm64.dmg \
     release/ElTerminalo-1.2.0-macos-arm64.zip \
     release/checksums-sha256.txt \
     --title "v1.2.0" --generate-notes
   ```

The `.zip` asset is required for the in-app auto-updater to work. Always include it alongside the `.dmg`.
