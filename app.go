package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/albinanto/elterminalo/internal/commands"
	"github.com/albinanto/elterminalo/internal/config"
	"github.com/albinanto/elterminalo/internal/ptymanager"
	"github.com/albinanto/elterminalo/internal/theme"
	"github.com/albinanto/elterminalo/internal/updater"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// App is the main Wails-bound application struct.
type App struct {
	ctx     context.Context
	ptyMgr  *ptymanager.Manager
	shell   string
	cfg     *config.Config
	cmds    *commands.Store
	dropDir string
}

// NewApp creates a new App instance.
func NewApp(shell string, cfg *config.Config) *App {
	dropDir, err := os.MkdirTemp("", "elterminalo-drops-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot create drop directory: %v\n", err)
	}
	return &App{
		shell:   shell,
		ptyMgr:  ptymanager.NewManager(shell),
		cfg:     cfg,
		cmds:    commands.NewStore(cfg.Dir()),
		dropDir: dropDir,
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.ptyMgr.SetContext(ctx)

	// Restore saved window geometry
	if g := a.cfg.LoadWindowGeometry(); g != nil {
		wailsRuntime.WindowSetPosition(ctx, g.X, g.Y)
		wailsRuntime.WindowSetSize(ctx, g.Width, g.Height)
	}
}

func (a *App) shutdown(ctx context.Context) {
	// Save window geometry before closing
	w, h := wailsRuntime.WindowGetSize(ctx)
	x, y := wailsRuntime.WindowGetPosition(ctx)
	a.cfg.SaveWindowGeometry(config.WindowGeometry{
		Width: w, Height: h, X: x, Y: y,
	})

	// Clean up dropped files
	if a.dropDir != "" {
		os.RemoveAll(a.dropDir)
	}

	a.ptyMgr.CloseAll()
}

// CreateSession creates a new PTY session and returns its ID.
// Cols and rows are clamped to defaults if <= 0.
func (a *App) CreateSession(cols, rows int, cwd string) (string, error) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return a.ptyMgr.CreateSession(cols, rows, cwd)
}

// GetSessionCWD returns the current working directory of a session.
func (a *App) GetSessionCWD(sessionID string) string {
	cwd, err := a.ptyMgr.GetSessionCWD(sessionID)
	if err != nil {
		return ""
	}
	return cwd
}

// GetAllSessionCWDs returns CWDs for all active sessions.
func (a *App) GetAllSessionCWDs() map[string]string {
	return a.ptyMgr.GetAllSessionCWDs()
}

// WriteToSession sends base64-encoded input to a PTY session.
func (a *App) WriteToSession(sessionID string, data string) error {
	return a.ptyMgr.WriteToSession(sessionID, data)
}

// ResizeSession resizes a PTY session.
func (a *App) ResizeSession(sessionID string, cols, rows int) {
	a.ptyMgr.ResizeSession(sessionID, cols, rows)
}

// CloseSession closes a PTY session.
func (a *App) CloseSession(sessionID string) {
	a.ptyMgr.CloseSession(sessionID)
}

// GetThemes returns the available themes (built-in merged with user themes).
func (a *App) GetThemes() []theme.Theme {
	return theme.Merged(a.cfg.Dir())
}

// SaveTheme saves a custom theme to the user's themes.json.
func (a *App) SaveTheme(name, background, foreground, accent, accentDim, border, borderActive, statusBg, statusFg, cursorColor, selectionBg, black, red, green, yellow, blue, magenta, cyan, white, brightBlack, brightRed, brightGreen, brightYellow, brightBlue, brightMagenta, brightCyan, brightWhite string) error {
	if name == "" {
		return fmt.Errorf("theme name is required")
	}

	newTheme := theme.Theme{
		Name: name, Background: background, Foreground: foreground,
		Accent: accent, AccentDim: accentDim, Border: border,
		BorderActive: borderActive, StatusBg: statusBg, StatusFg: statusFg,
		CursorColor: cursorColor, SelectionBg: selectionBg,
		Black: black, Red: red, Green: green, Yellow: yellow,
		Blue: blue, Magenta: magenta, Cyan: cyan, White: white,
		BrightBlack: brightBlack, BrightRed: brightRed, BrightGreen: brightGreen,
		BrightYellow: brightYellow, BrightBlue: brightBlue, BrightMagenta: brightMagenta,
		BrightCyan: brightCyan, BrightWhite: brightWhite,
	}

	existing, _ := theme.LoadUserThemes(a.cfg.Dir())

	// Replace if same name exists, otherwise append
	found := false
	for i, t := range existing {
		if strings.EqualFold(t.Name, name) {
			existing[i] = newTheme
			found = true
			break
		}
	}
	if !found {
		existing = append(existing, newTheme)
	}

	return theme.SaveUserThemes(a.cfg.Dir(), existing)
}

// DeleteTheme removes a custom theme by name.
func (a *App) DeleteTheme(name string) error {
	existing, err := theme.LoadUserThemes(a.cfg.Dir())
	if err != nil {
		return err
	}
	filtered := make([]theme.Theme, 0, len(existing))
	for _, t := range existing {
		if !strings.EqualFold(t.Name, name) {
			filtered = append(filtered, t)
		}
	}
	return theme.SaveUserThemes(a.cfg.Dir(), filtered)
}

// SaveAppState writes the serialized state JSON to disk atomically.
func (a *App) SaveAppState(stateJSON string) error {
	return a.cfg.SaveState(stateJSON)
}

// LoadAppState reads the saved state JSON from disk.
func (a *App) LoadAppState() string {
	return a.cfg.LoadState()
}

// GetGlobalCommands reads commands from the global commands file.
func (a *App) GetGlobalCommands() []commands.Command {
	return a.cmds.GetGlobal()
}

// GetLocalCommands walks up from cwd looking for .elterminalo/commands.json.
func (a *App) GetLocalCommands(cwd string) []commands.Command {
	return a.cmds.GetLocal(cwd)
}

// SaveCommand adds a command to the global or local commands file.
func (a *App) SaveCommand(scope, name, command, description, shortcut, cwd string) error {
	return a.cmds.Save(scope, name, command, description, shortcut, cwd)
}

// DeleteCommand removes a command by name from the given scope's file.
func (a *App) DeleteCommand(scope, name, cwd string) error {
	return a.cmds.Delete(scope, name, cwd)
}

// UpdateCommand replaces a command by oldName with new values.
func (a *App) UpdateCommand(scope, oldName, newName, newCommand, newDescription, newShortcut, cwd string) error {
	return a.cmds.Update(scope, oldName, newName, newCommand, newDescription, newShortcut, cwd)
}

// GetVersion returns the current application version.
func (a *App) GetVersion() string {
	return Version
}

// CheckForUpdate checks GitHub for a newer release.
func (a *App) CheckForUpdate() updater.UpdateInfo {
	return updater.Check(Version)
}

// ApplyUpdate downloads and installs the latest release, then relaunches.
func (a *App) ApplyUpdate() error {
	return updater.ApplyUpdate()
}

// SaveDroppedFile saves base64-encoded file data to a temp directory
// and returns the full path. Used for HTML5 drag-and-drop.
// Files are cleaned up when the app shuts down.
func (a *App) SaveDroppedFile(fileName string, dataBase64 string) (string, error) {
	if a.dropDir == "" {
		return "", fmt.Errorf("drop directory not available")
	}

	// Sanitize filename to prevent path traversal
	fileName = filepath.Base(fileName)

	data, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		return "", fmt.Errorf("invalid base64 data: %w", err)
	}

	if err := os.MkdirAll(a.dropDir, 0755); err != nil {
		return "", fmt.Errorf("cannot create temp dir: %w", err)
	}

	dest := filepath.Join(a.dropDir, fileName)
	if err := os.WriteFile(dest, data, 0644); err != nil {
		return "", fmt.Errorf("cannot write file: %w", err)
	}

	return dest, nil
}
