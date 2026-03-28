package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/albinanto/elterminalo/internal/commands"
	"github.com/albinanto/elterminalo/internal/config"
	"github.com/albinanto/elterminalo/internal/history"
	"github.com/albinanto/elterminalo/internal/llm"
	"github.com/albinanto/elterminalo/internal/ptymanager"
	"github.com/albinanto/elterminalo/internal/shellintegration"
	"github.com/albinanto/elterminalo/internal/theme"
	"github.com/albinanto/elterminalo/internal/updater"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// App is the main Wails-bound application struct.
const llmIdleTimeout = 5 * time.Minute

type App struct {
	ctx            context.Context
	ptyMgr         *ptymanager.Manager
	shell          string
	cfg            *config.Config
	cmds           *commands.Store
	dropDir        string
	closeConfirmed bool
	llmEngine      *llm.Engine
	llmMu          sync.Mutex
	llmIdleTimer   *time.Timer
	downloadCancel context.CancelFunc
	downloadMu     sync.Mutex
	historyStore   *history.Store
}

// NewApp creates a new App instance.
func NewApp(shell string, cfg *config.Config) *App {
	dropDir, err := os.MkdirTemp("", "elterminalo-drops-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot create drop directory: %v\n", err)
	}
	return &App{
		shell:   shell,
		ptyMgr:  ptymanager.NewManager(shell, cfg.Dir()),
		cfg:     cfg,
		cmds:    commands.NewStore(cfg.Dir()),
		dropDir: dropDir,
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.ptyMgr.SetContext(ctx)

	// Clean up any stale backup from a prior update
	updater.CleanupStaleBackup()

	// Install shell integration scripts (zsh/bash hooks for OSC 133)
	_ = shellintegration.Install(a.cfg.Dir())

	// Initialize command history database
	if store, err := history.NewStore(a.cfg.Dir()); err == nil {
		a.historyStore = store
	} else {
		fmt.Fprintf(os.Stderr, "Warning: history db: %v\n", err)
	}

	// Clean up partial downloads and old model versions
	llm.CleanStaleFiles(a.cfg.Dir())

	// Restore saved window geometry
	if g := a.cfg.LoadWindowGeometry(); g != nil {
		wailsRuntime.WindowSetPosition(ctx, g.X, g.Y)
		wailsRuntime.WindowSetSize(ctx, g.Width, g.Height)
	}
}

func (a *App) beforeClose(ctx context.Context) (prevent bool) {
	if a.closeConfirmed {
		return false // allow close
	}
	// Ask the frontend to show a confirmation dialog
	wailsRuntime.EventsEmit(ctx, "app:confirm-close")
	return true // prevent close for now
}

// ConfirmQuit is called by the frontend after the user confirms they want to quit.
func (a *App) ConfirmQuit() {
	a.closeConfirmed = true
	wailsRuntime.Quit(a.ctx)
}

func (a *App) shutdown(ctx context.Context) {
	// Save window geometry before closing — skip if maximised or fullscreen
	if !wailsRuntime.WindowIsMaximised(ctx) && !wailsRuntime.WindowIsFullscreen(ctx) {
		w, h := wailsRuntime.WindowGetSize(ctx)
		x, y := wailsRuntime.WindowGetPosition(ctx)
		a.cfg.SaveWindowGeometry(config.WindowGeometry{
			Width: w, Height: h, X: x, Y: y,
		})
	}

	// Clean up dropped files
	if a.dropDir != "" {
		os.RemoveAll(a.dropDir)
	}

	a.ptyMgr.CloseAll()

	// Close history database
	if a.historyStore != nil {
		a.historyStore.Close()
	}

	// Stop idle timer and free LLM model
	a.llmMu.Lock()
	if a.llmIdleTimer != nil {
		a.llmIdleTimer.Stop()
		a.llmIdleTimer = nil
	}
	a.llmMu.Unlock()
	a.unloadEngine()
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

// GetAllSessionStatuses returns the status of all active PTY sessions.
func (a *App) GetAllSessionStatuses() map[string]ptymanager.SessionStatus {
	return a.ptyMgr.GetAllSessionStatuses()
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

// IsModelReady returns true if the model is loaded in memory OR exists on disk.
func (a *App) IsModelReady() bool {
	a.llmMu.Lock()
	loaded := a.llmEngine != nil
	a.llmMu.Unlock()
	return loaded || llm.ModelExists(a.cfg.Dir())
}

// IsModelDownloaded checks if the model file exists on disk (for download/update UI).
func (a *App) IsModelDownloaded() bool {
	return llm.ModelExists(a.cfg.Dir())
}

// DownloadModel downloads the AI model from HuggingFace.
// Emits "model:download-progress" events with {downloaded, total} during download.
// The download can be cancelled via SkipDownload().
// Also used to update the model when a new version is available.
func (a *App) DownloadModel() error {
	dlCtx, cancel := context.WithCancel(a.ctx)
	a.downloadMu.Lock()
	a.downloadCancel = cancel
	a.downloadMu.Unlock()
	defer func() {
		a.downloadMu.Lock()
		a.downloadCancel = nil
		a.downloadMu.Unlock()
	}()

	return llm.DownloadModel(dlCtx, a.cfg.Dir(), func(downloaded, total int64) {
		if a.ctx != nil {
			wailsRuntime.EventsEmit(a.ctx, "model:download-progress", map[string]int64{
				"downloaded": downloaded,
				"total":      total,
			})
		}
	})
}

// SkipDownload cancels an in-progress model download.
func (a *App) SkipDownload() {
	a.downloadMu.Lock()
	cancel := a.downloadCancel
	a.downloadMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// CheckModelUpdate checks HuggingFace for a newer model version (ETag comparison).
func (a *App) CheckModelUpdate() bool {
	return llm.CheckModelUpdate(a.cfg.Dir())
}

// InitLLM loads the AI model into memory for inference.
func (a *App) InitLLM() error {
	a.llmMu.Lock()
	defer a.llmMu.Unlock()
	return a.loadEngineLocked()
}

// loadEngineLocked loads the model. Caller must hold llmMu.
func (a *App) loadEngineLocked() error {
	if a.llmEngine != nil {
		return nil // already loaded
	}
	if !llm.ModelExists(a.cfg.Dir()) {
		return fmt.Errorf("model not downloaded")
	}
	engine, err := llm.NewEngine(llm.ModelPath(a.cfg.Dir()), a.shell)
	if err != nil {
		return err
	}
	a.llmEngine = engine
	return nil
}

// unloadEngine frees the model from memory.
func (a *App) unloadEngine() {
	a.llmMu.Lock()
	defer a.llmMu.Unlock()
	if a.llmEngine != nil {
		a.llmEngine.Close()
		a.llmEngine = nil
	}
}

// resetIdleTimer resets (or starts) the idle unload timer.
func (a *App) resetIdleTimer() {
	a.llmMu.Lock()
	defer a.llmMu.Unlock()
	if a.llmIdleTimer != nil {
		a.llmIdleTimer.Stop()
	}
	a.llmIdleTimer = time.AfterFunc(llmIdleTimeout, func() {
		a.unloadEngine()
	})
}

// AskAI generates a shell command from a natural language prompt.
// Loads the model on first use and unloads after idle.
func (a *App) AskAI(prompt string, cwd string) (string, error) {
	if prompt == "" {
		return "", fmt.Errorf("prompt is required")
	}

	a.llmMu.Lock()
	if err := a.loadEngineLocked(); err != nil {
		a.llmMu.Unlock()
		return "", err
	}
	engine := a.llmEngine
	a.llmMu.Unlock()

	result, err := engine.Generate(prompt, cwd)

	// Reset idle timer after each use
	a.resetIdleTimer()

	return result, err
}

// SaveDroppedFile saves base64-encoded file data to a temp directory
// and returns the full path. Used for HTML5 drag-and-drop.
// Files are cleaned up when the app shuts down.
// RecordCommand records a completed command in the history database.
func (a *App) RecordCommand(command, cwd string, exitCode int, sessionID string) error {
	if a.historyStore == nil {
		return nil
	}
	return a.historyStore.Add(command, cwd, exitCode, filepath.Base(a.shell), sessionID)
}

// SearchHistory searches command history with CWD-contextual results first.
func (a *App) SearchHistory(query, cwd string, limit int) history.SearchResult {
	if a.historyStore == nil {
		return history.SearchResult{CWDMatches: []history.Entry{}, GlobalMatches: []history.Entry{}}
	}
	result, err := a.historyStore.Search(history.SearchParams{Query: query, CWD: cwd, Limit: limit})
	if err != nil {
		return history.SearchResult{CWDMatches: []history.Entry{}, GlobalMatches: []history.Entry{}}
	}
	return result
}

// ClearHistory removes all command history.
func (a *App) ClearHistory() error {
	if a.historyStore == nil {
		return nil
	}
	return a.historyStore.Clear()
}

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
	absDrop, _ := filepath.Abs(a.dropDir)
	absDest, _ := filepath.Abs(dest)
	if !strings.HasPrefix(absDest, absDrop+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid file name")
	}
	if err := os.WriteFile(dest, data, 0644); err != nil {
		return "", fmt.Errorf("cannot write file: %w", err)
	}

	return dest, nil
}
