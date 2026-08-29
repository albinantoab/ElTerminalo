# El Terminalo

A modern, GPU-accelerated terminal emulator for macOS.

![El Terminalo](assets/logo.png)

## Install

Download the latest `.dmg` from [Releases](https://github.com/albinantoab/ElTerminalo/releases/latest), open it, and drag **El Terminalo** to your Applications folder.

## Features

- **Tabbed interface** — Up to 9 tabs with Cmd+1-9 switching
- **Split panes** — Vertical and horizontal splits with draggable dividers
- **Command palette** — Quick access to all commands via Cmd+P
- **Custom commands** — Save frequently used commands globally or per-project
- **Themes** — Built-in themes + create your own via the palette
- **State persistence** — Tabs, splits, and working directories restored on restart
- **GPU-accelerated rendering** — WebGL-powered terminal via xterm.js
- **Native macOS look** — Transparent titlebar, proper window management

## Demos

### AI Assistant
https://github.com/user-attachments/assets/e657f481-2b18-42e9-a978-a57fe2bd120c

### JSON Viewer
https://github.com/user-attachments/assets/3b7b4f84-1efa-46d7-b6dc-9716ecd186dd

### Table View
https://github.com/user-attachments/assets/3535f539-9767-4935-ab8a-d3f9117e63e5

### Custom Shortcuts
https://github.com/user-attachments/assets/0b5a8134-3b50-4bf2-95ed-6ceff2162413

### Status Modal
https://github.com/user-attachments/assets/91a65c8d-0ce1-4052-980d-c3a78a00c360

## Keyboard Shortcuts

The menu bar lists these too — every item there does exactly what its shortcut does.

| Shortcut | Action |
|----------|--------|
| `Cmd + P` | Command palette |
| `Cmd + ,` | Open settings |
| `Cmd + T` | New tab |
| `Cmd + W` | Close tab |
| `Cmd + 1-9` | Switch to tab |
| `Cmd + Shift + ]` / `[` | Next / previous tab |
| `Cmd + D` | Split vertically |
| `Cmd + Shift + D` | Split horizontally |
| `Cmd + Shift + X` | Close pane |
| `Cmd + Arrow` | Navigate between panes |
| `Cmd + F` | Find in scrollback |
| `Cmd + L` | Clear terminal |
| `Cmd + I` | Session status |
| `Cmd + Shift + R` | Search command history |
| `Cmd + =` / `-` / `0` | Zoom in / out / actual size |
| `Cmd + Shift + C` | Create custom command |
| `Ctrl + Cmd + F` | Toggle full screen |

## Configuration

Settings live in `~/.config/elterminalo/config.json`. Open it with **Cmd + ,** (File › Settings…, or Help › Open Settings File) — the file is created with the defaults the first time you open it, and never rewritten after that.

Once you have saved your edits, run **Reload Settings** from the command palette (`Cmd + P`) to apply them without restarting. A changed `shell` applies to panes opened from then on; existing panes keep the shell they started with.

Shell integration — command blocks, the exit-status and duration badges, history recording, and working-directory tracking via OSC 7 — is installed for **zsh and bash only**. Any executable shell works as a `shell`, but one that is neither (fish, `/bin/sh`, nushell) runs as a plain terminal without those features.

```json
{
  "fontFamily": "'MonaspiceNe NFM', 'SF Mono', 'Menlo', monospace",
  "fontSize": 12,
  "lineHeight": 1.2,
  "cursorStyle": "block",
  "cursorBlink": true,
  "scrollback": 10000,
  "shell": "",
  "optionIsMeta": true,
  "bell": "sound",
  "copyOnSelect": true,
  "confirmQuit": "running"
}
```

| Key | Type | Default | Values |
|-----|------|---------|--------|
| `fontFamily` | string | `'MonaspiceNe NFM', 'SF Mono', 'Menlo', monospace` | Any CSS font stack. Keep a `monospace` fallback at the end. |
| `fontSize` | number | `12` | 6–72 (integer; fractional values are rounded) |
| `lineHeight` | number | `1.2` | 1.0–2.0, as a multiple of the font size |
| `cursorStyle` | string | `"block"` | `"block"`, `"underline"`, `"bar"` |
| `cursorBlink` | boolean | `true` | |
| `scrollback` | integer | `10000` | 0–1000000 lines kept per pane |
| `shell` | string | `""` | `""` uses `$SHELL` (or `/bin/zsh`). Otherwise an **absolute path** to an executable — e.g. `"/opt/homebrew/bin/fish"`. A change applies to panes opened from then on. Shell integration is installed for zsh and bash only; anything else loses command blocks, badges, history recording and OSC 7 directory tracking. |
| `optionIsMeta` | boolean | `true` | `true` sends Option-key presses to the shell as Meta; `false` lets macOS compose characters (`Option+n` → `~`). |
| `bell` | string | `"sound"` | `"none"`, `"sound"`, `"visual"`, `"both"` |
| `copyOnSelect` | boolean | `true` | Copy a selection to the clipboard as soon as it is made. |
| `confirmQuit` | string | `"running"` | `"running"` asks only when commands are still running, `"always"` always asks, `"never"` quits immediately. |

Anything the app cannot use is repaired in memory and reported in the log — a number outside its range is clamped, an unrecognised value falls back to the default, and a file that does not parse at all leaves every setting at its default. **Your file is never rewritten**, so a typo is always still there to fix.

Unknown keys are ignored, which makes them a convenient place to leave yourself a note.

### Logs

`~/.config/elterminalo/logs/elterminalo.log`, with two rotated generations beside it. **Help › Reveal Logs** opens the folder in Finder. This is where a rejected setting, a shell that could not be launched, or a colour scheme that failed to import explains itself.

### Importing a colour scheme

**File › Import Color Scheme…** reads two formats and converts either into a theme:

- **iTerm2** `.itermcolors` files (the XML property lists from [iterm2colorschemes.com](https://iterm2colorschemes.com))
- **Ghostty** theme files (`key = value` lines: `palette = 0=#1d1f21`, `background`, `foreground`, `cursor-color`, `selection-background`, …)

The theme is named after the file and saved to `~/.config/elterminalo/themes.json`, replacing any theme of the same name. The 16 ANSI colours, background, foreground, cursor and selection come straight from the file; the app's own accent, borders and status bar are derived from them (the accent is the scheme's blue).

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

In the command palette, use `Cmd + E` to edit or `Cmd + D` to delete a selected command.

## Themes

El Terminalo ships with 4 built-in themes: **Terminalo**, **Noctis**, **Ember**, and **Aurora**.

Create your own theme via the command palette ("Create Theme") or edit existing ones with `Cmd + E`. Custom themes are saved to `~/.config/elterminalo/themes.json`. You can also import an iTerm2 or Ghostty colour scheme — see [Importing a colour scheme](#importing-a-colour-scheme).

## Building from Source

<details>
<summary>For contributors and developers</summary>

### Prerequisites

- Go 1.24+
- Node.js 18+
- [Wails v2](https://wails.io/) CLI

### Setup

```bash
# Install Wails CLI
go install github.com/wailsapp/wails/v2/cmd/wails@latest

# Install frontend dependencies
cd frontend && npm install && cd ..

# Run in development mode (hot reload)
wails dev

# Build production binary
wails build
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for development guidelines.

</details>

## License

[MIT](LICENSE)
