package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/albinanto/elterminalo/internal/commands"
	"github.com/albinanto/elterminalo/internal/config"
	"github.com/albinanto/elterminalo/internal/history"
	"github.com/albinanto/elterminalo/internal/llm"
	"github.com/albinanto/elterminalo/internal/logging"
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

// filesDroppedEvent carries a native file drop to the frontend as
// {x, y, paths}. x and y are webview-local CSS pixels with the origin at the
// top left — WailsWebView.m flips AppKit's bottom-left origin before sending
// them — so the frontend can hand them straight to document.elementFromPoint to
// find the pane that was dropped on.
const filesDroppedEvent = "files:dropped"

// Labels for the native "still running" confirmation. MessageDialog matches
// DefaultButton/CancelButton against the button titles by string equality, so
// these constants have to be the values passed in both places.
const (
	quitButtonLabel   = "Quit"
	cancelButtonLabel = "Cancel"
)

type App struct {
	ctx    context.Context
	ptyMgr *ptymanager.Manager
	shell  string
	cfg    *config.Config
	cmds   *commands.Store
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
	return &App{
		shell:        shell,
		ptyMgr:       ptymanager.NewManager(shell, cfg.Dir()),
		cfg:          cfg,
		cmds:         commands.NewStore(cfg.Dir()),
		statsSampler: stats.New(),
	}
}

func (a *App) startup(ctx context.Context) {
	started := time.Now()
	a.ctx = ctx
	a.ptyMgr.SetContext(ctx)

	// Clean up any stale backup from a prior update.
	//
	// On a goroutine because of what it costs in the one case where it does
	// anything: a backup left behind by an interrupted install makes it shell out
	// to `codesign --verify` over a whole bundle, and every line below — including
	// the geometry restore, which is the part the user sees — waits behind it. A
	// cold start that stalls before the window appears is a bad way to greet
	// someone whose last update did not finish. It needs nothing from the window
	// and reports only to the log, and it claims the updater's install latch, so
	// an ApplyUpdate started in the meantime cannot race it.
	go func() {
		cleanupStarted := time.Now()
		log.Printf("startup: checking for a stale update backup in the background")
		updater.CleanupStaleBackup()
		log.Printf("startup: stale-backup check finished in %s", time.Since(cleanupStarted))
	}()

	// Install shell integration scripts (zsh/bash hooks for OSC 133)
	if err := shellintegration.Install(a.cfg.Dir()); err != nil {
		log.Printf("startup: shell integration not installed: %v", err)
	}

	// Initialize command history database
	if store, err := history.NewStore(a.cfg.Dir()); err == nil {
		a.historyStore = store
	} else {
		log.Printf("startup: history db unavailable: %v", err)
	}

	// Clean up partial downloads and old model versions
	llm.CleanStaleFiles(a.cfg.Dir())

	a.registerFileDrop(ctx)

	// Restore saved window geometry
	if g := a.cfg.LoadWindowGeometry(); g != nil {
		log.Printf("startup: restoring window geometry %dx%d at (%d,%d)", g.Width, g.Height, g.X, g.Y)
		wailsRuntime.WindowSetPosition(ctx, g.X, g.Y)
		wailsRuntime.WindowSetSize(ctx, g.Width, g.Height)
	} else {
		log.Printf("startup: no usable saved window geometry; using the default size")
	}

	log.Printf("startup: complete in %s", time.Since(started))
}

// registerFileDrop subscribes to Wails' native file drop and forwards it to the
// frontend as filesDroppedEvent.
//
// This replaces an HTML5 drop handler that read the dropped file's *bytes* in
// JavaScript, sent them back over the bridge, wrote them into a temp directory
// and inserted that copy's path into the terminal. A drop of a 2 GB video
// copied 2 GB; a drop of a folder produced nothing; and the path pasted into
// the shell was never the path the user dropped. The native handler reports the
// real absolute paths and copies nothing.
func (a *App) registerFileDrop(ctx context.Context) {
	wailsRuntime.OnFileDrop(ctx, func(x, y int, paths []string) {
		clean := realPaths(paths)
		if len(clean) == 0 {
			return
		}
		// %q, not %s: the path is whatever the user dragged, and a filename is
		// the one string in this log line an attacker gets to choose. A name
		// carrying a newline and a plausible-looking prefix would otherwise write
		// a second, forged line into the log — and quoting also keeps a name made
		// of control characters or trailing spaces readable as what it is.
		log.Printf("file drop: %d path(s) at (%d,%d), first=%q", len(clean), x, y, clean[0])
		wailsRuntime.EventsEmit(ctx, filesDroppedEvent, map[string]any{
			"x":     x,
			"y":     y,
			"paths": clean,
		})
	})
}

// realPaths keeps only the entries of a native drop payload that name something
// that actually exists on this filesystem.
//
// Everything in that payload went through [NSURL fileSystemRepresentation] in
// WailsWebView.m, which is applied to *every* URL on the pasteboard rather than
// only the file ones. A dragged web link therefore arrives as the path part of
// its URL — "https://example.com/foo/bar?q=1" becomes "/foo/bar" — which is a
// plausible-looking absolute path that does not exist, and which the frontend
// would otherwise paste into a shell. A drag that carried no URLs at all (plain
// text) arrives as a single empty string, because Wails splits the payload on
// "\n" and an empty payload splits into one empty field.
//
// So: absolute paths only, and each one has to pass Lstat. Lstat rather than
// Stat, because a dropped symlink is a real thing to paste even when its target
// is gone.
func realPaths(paths []string) []string {
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || !strings.HasPrefix(p, "/") {
			continue
		}
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		clean = append(clean, p)
	}
	if discarded := len(paths) - len(clean); discarded > 0 {
		log.Printf("file drop: kept %d of %d entries; %d were not existing filesystem paths",
			len(clean), len(paths), discarded)
	}
	return clean
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

	a.armQuitHandshake(ctx)
	handshakeStarted = true

	// Prevent this close; the committed one comes back through beforeClose and
	// takes the closeConfirmed branch above.
	return true
}

// armQuitHandshake hands off to the frontend to persist its layout, but never
// depends on it: whichever of ConfirmQuit or the fallback timer arrives first
// commits the quit. Callers must already have claimed quitPending.
func (a *App) armQuitHandshake(ctx context.Context) {
	log.Printf("quit: asking the frontend to save (%s); committing in %s regardless", saveAndQuitEvent, saveAndQuitTimeout)
	wailsRuntime.EventsEmit(ctx, saveAndQuitEvent)
	time.AfterFunc(saveAndQuitTimeout, func() {
		a.commitQuit("fallback timer after " + saveAndQuitTimeout.String())
	})
}

// beginQuit starts the same save-then-quit handshake beforeClose uses, from a
// caller that is not a close attempt — today, a finished update. It reports
// false when a quit is already in flight, in which case there is nothing to do:
// that quit will run shutdown() just as well as this one would have.
//
// Unlike beforeClose it asks nothing: the caller has already decided to quit,
// and there are no running commands worth a second opinion once the bundle
// under this process has been replaced.
func (a *App) beginQuit(ctx context.Context) bool {
	if ctx == nil {
		// Unreachable from a binding — Wails sets the context in OnStartup
		// before the webview exists — but EventsEmit on a nil context panics,
		// and a panic on a binding goroutine takes the whole app down.
		log.Printf("quit: cannot start the handshake, no window context yet")
		return false
	}
	if !a.quitPending.CompareAndSwap(false, true) {
		return false
	}
	a.armQuitHandshake(ctx)
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

	log.Printf("quit: %d command(s) still running; showing the confirmation dialog", running)
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
		log.Printf("quit: confirmation dialog failed (%v); treating that as Quit rather than trapping the user", err)
		return true
	}
	log.Printf("quit: user chose %q", choice)
	// Anything that is not an explicit Cancel counts as a Quit, so an
	// unexpected empty result can never trap the user in the app.
	return choice != cancelButtonLabel
}

// commitQuit performs the real quit exactly once, no matter how many of the
// handshake, the fallback timer, and a stray ConfirmQuit reach it. reason names
// whichever of them got there first; only the winner logs.
func (a *App) commitQuit(reason string) {
	a.quitOnce.Do(func() {
		log.Printf("quit: committing (%s)", reason)
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
	log.Printf("quit: ConfirmQuit received from the frontend")
	a.commitQuit("frontend confirmed the save")
}

func (a *App) shutdown(ctx context.Context) {
	started := time.Now()
	log.Printf("shutdown: starting")

	// Save window geometry before closing — skip if maximised or fullscreen
	if !wailsRuntime.WindowIsMaximised(ctx) && !wailsRuntime.WindowIsFullscreen(ctx) {
		w, h := wailsRuntime.WindowGetSize(ctx)
		x, y := wailsRuntime.WindowGetPosition(ctx)
		if err := a.cfg.SaveWindowGeometry(config.WindowGeometry{
			Width: w, Height: h, X: x, Y: y,
		}); err != nil {
			log.Printf("shutdown: window geometry not saved: %v", err)
		} else {
			log.Printf("shutdown: saved window geometry %dx%d at (%d,%d)", w, h, x, y)
		}
	} else {
		log.Printf("shutdown: window is maximised or fullscreen; keeping the previous geometry")
	}

	// CloseAll is the step that can actually take time — it signals and reaps
	// every child process — so it is the one worth timing when a user reports a
	// slow quit.
	step := time.Now()
	a.ptyMgr.CloseAll()
	log.Printf("shutdown: CloseAll took %s", time.Since(step))

	// Close history database
	if a.historyStore != nil {
		step = time.Now()
		if err := a.historyStore.Close(); err != nil {
			log.Printf("shutdown: closing the history db failed after %s: %v", time.Since(step), err)
		} else {
			log.Printf("shutdown: history db closed in %s", time.Since(step))
		}
	}

	// Stop idle timer and free LLM model
	a.llmMu.Lock()
	if a.llmIdleTimer != nil {
		a.llmIdleTimer.Stop()
		a.llmIdleTimer = nil
	}
	a.llmMu.Unlock()
	step = time.Now()
	a.unloadEngine()
	log.Printf("shutdown: complete in %s (model unload %s)", time.Since(started), time.Since(step))
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
	id, err := a.ptyMgr.CreateSession(cols, rows, cwd)
	if err != nil {
		log.Printf("session: create failed (cols=%d rows=%d cwd=%q): %v", cols, rows, cwd, err)
		return "", err
	}
	log.Printf("session: created %q (cols=%d rows=%d cwd=%q)", id, cols, rows, cwd)
	return id, nil
}

// AttachSession re-attaches the frontend to an existing PTY session and reports
// whether it is still alive.
//
// It replaces the old SessionExists probe, which could only answer the question
// and not act on it. A pane that reloads has to both find out whether its shell
// is still there and be re-registered as its reader; asking and then attaching
// as two calls leaves a window in which output is produced with nobody
// listening. A false here means the shell is gone — the exit event has already
// fired and the pane missed it — and the frontend can synthesize one rather
// than leave a silently dead pane on screen.
func (a *App) AttachSession(sessionID string) bool {
	return a.ptyMgr.AttachSession(sessionID)
}

// AckOutput tells the manager the frontend has consumed n bytes of a session's
// output, so it can let more through. Without this the backend has no idea
// whether the webview is keeping up, and a command like `yes` buries it.
func (a *App) AckOutput(sessionID string, n int) {
	a.ptyMgr.AckOutput(sessionID, n)
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
	// The id arrives from the webview, so it is as much user-controlled as a
	// dropped filename is; every string in this file that reaches a log line
	// from outside the backend is quoted for the same reason.
	log.Printf("session: closing %q", sessionID)
	a.ptyMgr.CloseSession(sessionID)
}

// LogMessage records a line from the frontend in the app log.
//
// The webview has no other way to leave a trace: console.log and console.error
// inside WKWebView go nowhere a user can retrieve after the fact, so a frontend
// failure used to be entirely invisible unless it happened while Safari's
// inspector was attached. level is "info", "warn" or "error"; anything else is
// recorded as info. The message is sanitised — see logging.Frontend.
func (a *App) LogMessage(level string, message string) {
	logging.Frontend(level, message)
}

// RevealLogs shows the log file in Finder.
//
// The point of a log nobody can find is small, and "~/.config/elterminalo/logs"
// is not a path a user can reach through Finder's UI without knowing the
// keystroke for hidden directories.
func (a *App) RevealLogs() {
	path := logging.Path()
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			log.Printf("reveal logs: %q is gone (%v); opening the config directory instead", path, err)
			path = ""
		}
	}
	if path == "" {
		// Logging never came up. The config directory is still the right place
		// to land: it is where a log would have been, and where the state files
		// worth looking at live.
		if err := startAndReap(exec.Command(updater.OpenPath, a.cfg.Dir())); err != nil {
			log.Printf("reveal logs: could not open %q: %v", a.cfg.Dir(), err)
		}
		return
	}
	// -R reveals the file with it selected, rather than opening it in whatever
	// the user has associated with .log.
	if err := startAndReap(exec.Command(updater.OpenPath, "-R", path)); err != nil {
		log.Printf("reveal logs: could not reveal %q: %v", path, err)
	}
}

// startAndReap starts cmd and waits for it on a goroutine.
//
// Both halves matter. Start with no matching Wait leaves the finished process as
// a zombie for as long as the app runs, and this is behind a menu item a user can
// click any number of times; Wait on this goroutine would instead block a Wails
// binding — the webview's call does not return until the binding does — on a
// subprocess, which is not something a menu item may do.
//
// updater.OpenPath rather than "open": the same $PATH argument the updater makes
// for its own tools applies to anything this process execs, and it costs nothing
// to name the tool on the read-only system volume.
func startAndReap(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
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
	if err := a.cfg.SaveState(stateJSON); err != nil {
		// The frontend surfaces nothing for this, and it is the failure that
		// loses a user's whole layout — it has to leave a trace somewhere.
		log.Printf("state: save failed (%d bytes): %v", len(stateJSON), err)
		return err
	}
	return nil
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
		log.Printf("state: cannot quarantine the layout that failed to rebuild: %v", err)
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

// ApplyUpdate downloads, verifies and installs the latest release, launches it,
// and then quits this instance through the same save-then-quit handshake Cmd-Q
// uses — so shutdown() runs and the window geometry and layout are written out.
//
// The updater used to finish with os.Exit(0), which skipped shutdown()
// entirely: updating cost the user their pane layout and window position, and
// left the PTY children to be reaped by launchd.
//
// Ordering hazard, deliberately left in place for this phase: the updater
// launches the new bundle with `open -n` *before* this instance has saved
// anything, so for a moment two instances are alive and both will write
// state.json. In practice the old one wins — it saves within milliseconds of
// the handshake, while the new instance needs more than a second to bring up
// its webview and read the file — but that is a race, not a guarantee, and it
// resolves the wrong way on a machine slow enough or a save large enough. The
// fix is to stage the swap and the relaunch into commitQuit, after this
// instance has finished saving; that is a follow-up, kept out of here to keep
// this change small.
func (a *App) ApplyUpdate() error {
	if err := updater.ApplyUpdate(); err != nil {
		return err
	}
	if !a.beginQuit(a.ctx) {
		// Usually because a quit is already in flight — the user hit Cmd-Q
		// while the download was running — and that one runs shutdown() just as
		// well as ours would. The new version is running either way.
		log.Printf("update: installed and relaunched; this instance did not start a quit of its own")
	}
	return nil
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
