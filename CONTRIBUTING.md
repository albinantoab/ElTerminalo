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
