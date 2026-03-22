# El Terminalo

A modern, GPU-accelerated terminal emulator for macOS, built with Go and xterm.js.

[Screenshot placeholder]

## Features

- **Tabbed interface** — Up to 9 tabs with Cmd+1-9 switching
- **Split panes** — Vertical and horizontal splits with draggable dividers
- **Command palette** — Quick access to all commands via Cmd+P
- **Custom commands** — Save frequently used commands (global or per-project)
- **Themes** — Built-in Noctis, Ember, and Aurora themes
- **State persistence** — Tabs, splits, and CWD restored on restart
- **GPU-accelerated rendering** — WebGL-powered terminal via xterm.js
- **Native macOS integration** — Transparent titlebar, proper window management

## Prerequisites

- Go 1.24+
- Node.js 18+
- [Wails v2](https://wails.io/) CLI

## Getting Started

### Install Wails CLI

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@latest
```

### Development

```bash
# Install frontend dependencies
cd frontend && npm install && cd ..

# Run in development mode (hot reload)
wails dev
```

### Build

```bash
# Build production binary
wails build

# Build macOS .app bundle
make app
```

## Keyboard Shortcuts

| Shortcut | Action |
|----------|--------|
| `Cmd + P` | Command palette |
| `Cmd + T` | New tab |
| `Cmd + W` | Close tab |
| `Cmd + 1-9` | Switch to tab |
| `Cmd + B` | Split vertical |
| `Cmd + G` | Split horizontal |
| `Cmd + X` | Close pane |
| `Cmd + Arrow` | Navigate between panes |
| `Cmd + L` | Clear terminal |
| `Cmd + Shift + C` | Create custom command |

## Custom Commands

Commands can be saved globally (`~/.config/elterminalo/commands.json`) or per-project (`.elterminalo/commands.json` in your project directory).

Create commands via the palette (`Cmd + Shift + C`) or edit the JSON files directly:

```json
{
  "commands": [
    {
      "name": "Build",
      "command": "npm run build",
      "description": "Build for production",
      "shortcut": "Cmd+Shift+B"
    }
  ]
}
```

## Architecture

```
├── main.go                    # Wails app entry point
├── app.go                     # Wails-bound API facade
├── internal/
│   ├── config/                # Configuration and state persistence
│   ├── commands/              # Custom command CRUD
│   ├── theme/                 # Theme definitions
│   └── ptymanager/            # PTY session management
│       ├── manager.go         # Multi-session manager with batched output
│       ├── session.go         # Single PTY session lifecycle
│       └── cwd.go             # Working directory detection
└── frontend/
    ├── src/
    │   ├── main.ts            # App orchestrator
    │   ├── terminal/          # xterm.js terminal pane
    │   ├── theme/             # Theme system
    │   ├── palette/           # Command palette
    │   ├── wizard/            # Command creation wizard
    │   └── state/             # State persistence
    └── index.html
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development guidelines.

## License

[MIT](LICENSE)
