package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/albinanto/elterminalo/internal/commands"
	"github.com/albinanto/elterminalo/internal/config"
	"github.com/albinanto/elterminalo/internal/history"
	"github.com/albinanto/elterminalo/internal/llm"
	"github.com/albinanto/elterminalo/internal/ptymanager"
	"github.com/albinanto/elterminalo/internal/shellintegration"
	"github.com/albinanto/elterminalo/internal/stats"
	"github.com/albinanto/elterminalo/internal/theme"
	"github.com/albinanto/elterminalo/internal/updater"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// App is the main Wails-bound application struct.
const llmIdleTimeout = 5 * time.Minute

// saveAndQuitTimeout bounds how long we wait for the frontend to persist its
// layout after "app:save-and-quit" before quitting anyway. A hung or crashed
// webview must never be able to make the app unquittable — Force Quit skips
// shutdown() entirely, which is how window geometry and up to a full autosave
// interval of layout changes used to get lost.
const saveAndQuitTimeout = 2 * time.Second

// saveAndQuitEvent asks the frontend to flush its layout and then call
// ConfirmQuit. The frontend contract depends on this exact name.
const saveAndQuitEvent = "app:save-and-quit"

// Labels for the native "still running" confirmation. MessageDialog matches
// DefaultButton/CancelButton against the button titles by string equality, so
// these constants have to be the values passed in both places.
const (
	quitButtonLabel   = "Quit"
	cancelButtonLabel = "Cancel"
)

type App struct {
	ctx     context.Context
	ptyMgr  *ptymanager.Manager
	shell   string
	cfg     *config.Config
	cmds    *commands.Store
	dropDir string
	// closeConfirmed is written from the quit timer / a binding goroutine and
	// read by beforeClose on whichever goroutine Wails runs it on, so it has to
	// be atomic rather than a plain bool.
	closeConfirmed atomic.Bool
	// quitPending marks a confirmation dialog or a save handshake as already in
	// flight, so a second Cmd-Q cannot stack a second dialog or a second timer.
	quitPending    atomic.Bool
	quitOnce       sync.Once
	llmEngine      *llm.Engine
	llmMu          sync.Mutex
	llmIdleTimer   *time.Timer
	downloadCancel context.CancelFunc
	downloadMu     sync.Mutex
	historyStore   *history.Store
	statsSampler   *stats.Sampler
}

// NewApp creates a new App instance.
func NewApp(shell string, cfg *config.Config) *App {
	dropDir, err := os.MkdirTemp("", "elterminalo-drops-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot create drop directory: %v\n", err)
	}
	return &App{
		shell:        shell,
		ptyMgr:       ptymanager.NewManager(shell, cfg.Dir()),
		cfg:          cfg,
		cmds:         commands.NewStore(cfg.Dir()),
		dropDir:      dropDir,
		statsSampler: stats.New(),
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

// beforeClose runs on every quit attempt (Cmd-Q, the window close button, and
// the re-entrant call wailsRuntime.Quit makes). It confirms with the user
// natively — not through the webview, which may be hung — then gives the
// frontend a bounded window to save its layout before the app goes down.
func (a *App) beforeClose(ctx context.Context) (prevent bool) {
	if a.closeConfirmed.Load() {
		return false // committed: let Wails tear down and run shutdown()
	}

	// A second Cmd-Q while the first is still being decided must be a no-op,
	// not another dialog and another fallback timer.
	if !a.quitPending.CompareAndSwap(false, true) {
		return true
	}

	// quitPending set with no fallback timer armed is the one state that makes
	// the app unquittable: every later Cmd-Q takes the CompareAndSwap branch
	// above and returns without arming anything. So the flag is cleared on
	// every way out of here except the one that arms the timer — the Cancel
	// branch, an unexpected result, a panic in the dialog. (A dialog that never
	// returns at all blocks this call and is beyond in-process rescue; what is
	// guaranteed is that no completed beforeClose leaves the trap set.)
	handshakeStarted := false
	defer func() {
		if !handshakeStarted {
			a.quitPending.Store(false)
		}
	}()

	// Only interrupt the user when quitting would actually kill something.
	if running := a.ptyMgr.RunningCommandCount(); running > 0 && !a.confirmQuitWithUser(ctx, running) {
		return true // the user stayed; the deferred clear re-arms the next Cmd-Q
	}

	// Hand off to the frontend to persist its layout, but never depend on it:
	// whichever of ConfirmQuit or this timer arrives first commits the quit.
	wailsRuntime.EventsEmit(ctx, saveAndQuitEvent)
	time.AfterFunc(saveAndQuitTimeout, a.commitQuit)
	handshakeStarted = true

	// Prevent this close; the committed one comes back through beforeClose and
	// takes the closeConfirmed branch above.
	return true
}

// confirmQuitWithUser shows the native "commands still running" alert and
// reports whether the user chose to quit. A dialog that fails to display is
// treated as a Quit: an unquittable app is far worse than a lost confirmation.
func (a *App) confirmQuitWithUser(ctx context.Context, running int) bool {
	message := fmt.Sprintf("%d commands are still running and will be terminated.", running)
	if running == 1 {
		message = "1 command is still running and will be terminated."
	}

	choice, err := wailsRuntime.MessageDialog(ctx, wailsRuntime.MessageDialogOptions{
		Type:    wailsRuntime.QuestionDialog,
		Title:   "Quit El Terminalo?",
		Message: message,
		Buttons: []string{quitButtonLabel, cancelButtonLabel},
		// Wails checks DefaultButton before CancelButton when assigning key
		// equivalents, so naming Cancel as both makes Return dismiss the alert
		// safely instead of terminating the user's commands. Its else-if means
		// a button can have Return or Escape but never both: Escape therefore
		// does not dismiss this dialog. Accepted — Return is the key that would
		// otherwise kill running commands, so it is the one worth claiming.
		DefaultButton: cancelButtonLabel,
		CancelButton:  cancelButtonLabel,
	})
	if err != nil {
		return true
	}
	// Anything that is not an explicit Cancel counts as a Quit, so an
	// unexpected empty result can never trap the user in the app.
	return choice != cancelButtonLabel
}

// commitQuit performs the real quit exactly once, no matter how many of the
// handshake, the fallback timer, and a stray ConfirmQuit reach it.
func (a *App) commitQuit() {
	a.quitOnce.Do(func() {
		// Must be set before Quit: Wails re-enters beforeClose from there, and
		// that call has to see the confirmation and allow the close through.
		a.closeConfirmed.Store(true)
		if a.ctx != nil {
			wailsRuntime.Quit(a.ctx)
		}
	})
}

// ConfirmQuit is called by the frontend once it has finished saving its layout,
// completing the handshake beforeClose started. Calling it with no handshake
// pending still quits cleanly.
func (a *App) ConfirmQuit() {
	a.commitQuit()
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

// SessionExists reports whether a PTY session is still alive.
//
// Lets the frontend detect a shell that exited before it could subscribe to
// pty:exit; Wails events are not replayed. The manager removes a session from
// its map before emitting that event, so a false here means the exit has
// already fired — missed, if the pane never saw it — and the frontend can
// synthesize one rather than leave a silently dead pane on screen.
func (a *App) SessionExists(sessionID string) bool {
	return a.ptyMgr.HasSession(sessionID)
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

// QuarantineAppState moves the saved layout aside and returns the path it was
// moved to — "" when there was nothing to move or the move failed.
//
// The frontend calls this when a saved layout parsed as JSON but could not be
// rebuilt into panes, because the fresh single-tab layout it falls back to gets
// saved straight over the real one otherwise. The backup is no help there: the
// file is valid JSON, so LoadState hands it back rather than falling back, and
// the first save after the fallback rotates the last good copy out of .bak.
// Renaming the file means the next save starts a fresh one while the layout
// that could not be rebuilt survives on disk for manual recovery.
func (a *App) QuarantineAppState() string {
	path, err := a.cfg.QuarantineState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot quarantine state: %v\n", err)
		return ""
	}
	return path
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

// GetHostname returns this machine's hostname. The frontend uses it to accept
// OSC 7 directory reports only from local shells, so an SSH session's remote
// path is never written into the saved layout.
func (a *App) GetHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// GetSystemStats returns the latest CPU% and resident memory for this process.
// CPU% is computed against the previous call, so the first call returns 0%.
func (a *App) GetSystemStats() stats.Snapshot {
	if a.statsSampler == nil {
		return stats.Snapshot{}
	}
	return a.statsSampler.Sample()
}

// updateCheckCacheTTL is how long a recorded update check stays valid before
// we hit GitHub again. The frontend polls more often than this, but each call
// returns the cached result until the TTL elapses.
const updateCheckCacheTTL = 24 * time.Hour

type updateCheckCache struct {
	LastCheckedUnix int64              `json:"lastCheckedUnix"`
	Info            updater.UpdateInfo `json:"info"`
}

func (a *App) updateCacheFile() string {
	return filepath.Join(a.cfg.Dir(), "update_check.json")
}

func (a *App) loadUpdateCache() (updateCheckCache, bool) {
	data, err := os.ReadFile(a.updateCacheFile())
	if err != nil {
		return updateCheckCache{}, false
	}
	var c updateCheckCache
	if err := json.Unmarshal(data, &c); err != nil {
		return updateCheckCache{}, false
	}
	return c, true
}

func (a *App) saveUpdateCache(c updateCheckCache) {
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	path := a.updateCacheFile()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// CheckForUpdate returns the latest update status, throttled to once per day.
// If a check ran within the TTL we return the cached result instead of hitting
// GitHub. The current version is refreshed on every call so cached info from
// a prior install doesn't claim an update is still pending.
func (a *App) CheckForUpdate() updater.UpdateInfo {
	if cached, ok := a.loadUpdateCache(); ok {
		age := time.Since(time.Unix(cached.LastCheckedUnix, 0))
		if age >= 0 && age < updateCheckCacheTTL && cached.Info.LatestVer != "" {
			info := cached.Info
			info.CurrentVer = Version
			// Re-evaluate availability against the running binary in case the
			// user updated by some other means since the last check.
			info.Available = info.LatestVer != "" && info.LatestVer != Version && isVersionNewer(info.LatestVer, Version)
			return info
		}
	}
	info := updater.Check(Version)
	a.saveUpdateCache(updateCheckCache{
		LastCheckedUnix: time.Now().Unix(),
		Info:            info,
	})
	return info
}

// isVersionNewer is a small semver-ish comparator (x.y.z) matching updater's
// internal logic. Kept local so we can revalidate cached results without
// exporting from the updater package.
func isVersionNewer(latest, current string) bool {
	lp, cp := splitDottedVersion(latest), splitDottedVersion(current)
	for i := range 3 {
		if lp[i] != cp[i] {
			return lp[i] > cp[i]
		}
	}
	return false
}

func splitDottedVersion(v string) [3]int {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		n := 0
		for _, ch := range parts[i] {
			if ch < '0' || ch > '9' {
				break
			}
			n = n*10 + int(ch-'0')
		}
		out[i] = n
	}
	return out
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
