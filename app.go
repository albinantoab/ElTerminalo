package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/albinanto/elterminalo/internal/commands"
	"github.com/albinanto/elterminalo/internal/config"
	"github.com/albinanto/elterminalo/internal/history"
	"github.com/albinanto/elterminalo/internal/llm"
	"github.com/albinanto/elterminalo/internal/logging"
	"github.com/albinanto/elterminalo/internal/macos"
	"github.com/albinanto/elterminalo/internal/ptymanager"
	"github.com/albinanto/elterminalo/internal/settings"
	"github.com/albinanto/elterminalo/internal/shellintegration"
	"github.com/albinanto/elterminalo/internal/stats"
	"github.com/albinanto/elterminalo/internal/theme"
	"github.com/albinanto/elterminalo/internal/updater"
	"github.com/albinanto/elterminalo/internal/workspace"
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

// menuActionEvent carries a native menu item's action name to the frontend as
// {"action": "<name>"}. The names are listed next to the menu in main.go and
// the frontend switches on them; they are a contract, not a label.
const menuActionEvent = "menu:action"

// themesChangedEvent tells the frontend a theme was added or replaced, as
// {"name": "<theme name>"}. It refetches the list and switches to that theme.
const themesChangedEvent = "themes:changed"

// notificationActivatedEvent tells the frontend the user clicked one of our
// native notifications, as {"paneKey": "<whatever was passed to Notify>"}. The
// key is opaque to Go: the frontend chose it when it asked for the
// notification, and it is the only thing that says which pane to focus.
const notificationActivatedEvent = "notification:activated"

// transcriptsDirName is the subdirectory of the config directory that recorded
// pane output goes into, one directory per day.
const transcriptsDirName = "transcripts"

// issuesURL is where Help › Report an Issue goes.
const issuesURL = "https://github.com/albinantoab/ElTerminalo/issues"

// maxWindowTitleBytes bounds the title a pane may set. The value comes from the
// terminal — an OSC 0/2 sequence any program can emit — so it is neither short
// nor trusted; 256 bytes is far more than a title bar can show.
const maxWindowTitleBytes = 256

// maxSchemeBytes bounds an imported colour scheme. The largest .itermcolors in
// circulation is a few tens of kilobytes; this is only here so that picking a
// disk image in the file dialog cannot read it all into memory.
const maxSchemeBytes = 4 << 20

// Bounds on what a notification may carry. Like a window title, all three come
// from the terminal in the end — OSC 9 and OSC 777 are sequences any program
// the user runs can print — so none of them is trusted to be short or to be one
// line. macOS truncates a banner to a couple of lines anyway; these numbers only
// keep an unbounded string from being handed to AppKit.
const (
	maxNotificationTitleBytes = 128
	maxNotificationBodyBytes  = 512
	// maxPaneKeyBytes bounds the round-tripped key. Nothing reads it but the
	// frontend, and it is stored in the notification's userInfo, which the
	// system persists.
	maxPaneKeyBytes = 128
)

// maxDockBadgeRunes bounds the Dock tile's badge. The badge is a small red
// bubble that holds a number or a couple of characters; anything longer is
// ellipsised by AppKit into something unreadable.
const maxDockBadgeRunes = 8

// transcriptIDChars is how much of a session id goes into a transcript's
// filename. Session ids are UUIDs, so eight characters name the pane uniquely
// among the handful that are open while keeping the name readable.
const transcriptIDChars = 8

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

	// Preserved copies of dropped files outlive the drop on purpose; they must
	// not outlive it by weeks.
	pruneOldDrops(a.cfg.Dir())

	// Clean up partial downloads and old model versions
	llm.CleanStaleFiles(a.cfg.Dir())

	a.registerFileDrop(ctx)

	// Native notifications. The handler is registered before the request goes
	// out so that a click can never arrive with nowhere to go, and
	// InitNotifications returns immediately — the permission prompt it may put
	// on screen is answered by a human, and the answer comes back through the
	// package's own callback some seconds or minutes later. Until then
	// NotificationsReady reports false, which is the truth.
	macos.OnNotificationActivated(a.notificationActivated)
	macos.InitNotifications()

	// One line for what the user's config.json actually resolved to, and one
	// per key we had to repair. Without it, "my font size does nothing" is
	// unanswerable: the file is on their disk, not ours, and a clamped or
	// misspelled value looks exactly like a setting that is not wired up.
	set, warnings := settings.Load(a.cfg.Dir())
	log.Printf("startup: settings %s", describeSettings(set))
	for _, w := range warnings {
		log.Printf("startup: settings: %s", w)
	}

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
		// A path is only useful for as long as the file behind it exists, and
		// some of the things people drag hardest do not last. See
		// preserveVolatileDrops.
		clean = preserveVolatileDrops(a.cfg.Dir(), clean)
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

// dropsDirName is where a volatile dropped file is copied so its path keeps
// meaning something. Inside the config directory, so it is durable and ours.
const dropsDirName = "drops"

// dropCopyMaxBytes bounds what is worth copying. Past this, handing back the
// original path is the better of two bad answers: the copy would stall the drop
// and could fill the disk, and a file that large is unlikely to be a screenshot
// macOS is about to delete.
const dropCopyMaxBytes = 512 << 20

// dropRetention is how long a preserved copy is kept. Long enough to still be
// there tomorrow, short enough that the directory does not grow without end.
const dropRetention = 7 * 24 * time.Hour

// volatileRoots are the trees whose contents the system may delete at any
// moment. A var so the tests can point it somewhere they control.
var volatileRoots = defaultVolatileRoots()

func defaultVolatileRoots() []string {
	roots := []string{"/tmp", "/private/tmp"}
	if t := os.TempDir(); t != "" {
		t = filepath.Clean(t)
		roots = append(roots, t)
		// /var is a symlink to /private/var on macOS, and a dropped path can
		// arrive spelled either way.
		if strings.HasPrefix(t, "/var/") {
			roots = append(roots, "/private"+t)
		} else if rest, ok := strings.CutPrefix(t, "/private/"); ok {
			roots = append(roots, "/"+rest)
		}
	}
	return roots
}

// isVolatilePath reports whether p lives somewhere the system prunes.
func isVolatilePath(p string) bool {
	p = filepath.Clean(p)
	for _, root := range volatileRoots {
		if p == root || strings.HasPrefix(p, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// preserveVolatileDrops copies anything dropped from a volatile location into
// the config directory and returns the copy's path in its place. Paths that
// name a durable file are returned untouched.
//
// This exists because of how a screenshot is dragged. macOS puts the thumbnail's
// file in $TMPDIR/TemporaryItems/NSIRD_screencaptureui_XXXX/, and deletes that
// directory once the thumbnail goes away — seconds later. The native drop
// reports the true path, which is the right answer for a file in ~/Documents and
// a useless one here: by the time anything reads it, the file is gone. So the
// true path is kept where it stays true, and swapped for a copy where it does
// not.
func preserveVolatileDrops(configDir string, paths []string) []string {
	out := make([]string, 0, len(paths))
	var dir string
	for _, p := range paths {
		if !isVolatilePath(p) {
			out = append(out, p)
			continue
		}
		if info, err := os.Stat(p); err == nil && info.Size() > dropCopyMaxBytes {
			log.Printf("file drop: %q is %d bytes, too large to preserve; using the original path", p, info.Size())
			out = append(out, p)
			continue
		}
		if dir == "" {
			dir = filepath.Join(configDir, dropsDirName)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				log.Printf("file drop: cannot create %q (%v); using the original paths", dir, err)
				return paths
			}
		}
		copied, err := copyDroppedFile(p, dir)
		if err != nil {
			// The file went away between the drop and the copy, which is the
			// whole problem this function exists for. Nothing to hand back.
			log.Printf("file drop: could not preserve %q: %v", p, err)
			continue
		}
		log.Printf("file drop: preserved %q as %q", p, copied)
		out = append(out, copied)
	}
	return out
}

// copyDroppedFile copies src into dir under a timestamped name and returns it.
// The copy lands on a temporary name first, so the path handed to the frontend
// never names a half-written file.
func copyDroppedFile(src, dir string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()

	name := filepath.Base(src)
	if name == "." || name == string(os.PathSeparator) || name == ".." {
		name = "dropped-file"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	dest := filepath.Join(dir, time.Now().Format("20060102-150405")+"-"+name)

	tmp, err := os.CreateTemp(dir, ".drop-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return dest, nil
}

// pruneOldDrops deletes preserved copies past their retention. Best effort: a
// drop that cannot be cleaned up is not worth failing a startup over.
func pruneOldDrops(configDir string) {
	dir := filepath.Join(configDir, dropsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-dropRetention)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Printf("startup: removed %d dropped file(s) older than %s", removed, dropRetention)
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

	if !a.shouldQuit(ctx) {
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

// shouldQuit applies the user's "confirmQuit" setting and reports whether the
// quit may go ahead.
//
// The setting is read here, at the moment of the quit, rather than at startup:
// someone who has just edited config.json expects the next Cmd-Q to honour it,
// and a settings file is a few hundred bytes read once per quit attempt.
func (a *App) shouldQuit(ctx context.Context) bool {
	// Warnings are not logged from here. GetSettings already reports them
	// whenever the frontend asks, and a quit is the worst moment to add lines
	// to a log somebody is about to go looking through.
	set, _ := settings.Load(a.cfg.Dir())
	running := a.ptyMgr.RunningCommandCount()

	switch set.ConfirmQuit {
	case settings.ConfirmQuitNever:
		log.Printf("quit: confirmQuit is %q; quitting with %d command(s) running", set.ConfirmQuit, running)
		return true
	case settings.ConfirmQuitAlways:
		return a.confirmQuitWithUser(ctx, running)
	default:
		// "running": only interrupt when quitting would actually kill something.
		return running == 0 || a.confirmQuitWithUser(ctx, running)
	}
}

// confirmQuitWithUser shows the native quit confirmation and reports whether
// the user chose to quit. A dialog that fails to display is treated as a Quit:
// an unquittable app is far worse than a lost confirmation.
//
// running may be zero — that is the "always" policy, where there is nothing to
// warn about and the question is the whole message.
func (a *App) confirmQuitWithUser(ctx context.Context, running int) bool {
	message := fmt.Sprintf("%d commands are still running and will be terminated.", running)
	switch running {
	case 0:
		// Reached only under confirmQuit "always": nothing is at stake, so the
		// question is the whole message.
		message = "Quit El Terminalo?"
	case 1:
		message = "1 command is still running and will be terminated."
	}

	log.Printf("quit: showing the confirmation dialog (%d command(s) running)", running)
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
//
// The shell is resolved here, per session, from the settings file, so a change
// to "shell" reaches the next pane (after Reload Settings) instead of waiting
// for a relaunch. The default resolved at startup remains the fallback for an
// empty or unusable value; settings.ResolveShell validates against the
// filesystem and says why it fell back.
func (a *App) CreateSession(cols, rows int, cwd string) (string, error) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	set, _ := settings.Load(a.cfg.Dir())
	shell, warning := settings.ResolveShell(set.Shell, a.shell)
	if warning != "" {
		log.Printf("session: %s", warning)
	}
	id, err := a.ptyMgr.CreateSessionWithShell(shell, cols, rows, cwd)
	if err != nil {
		log.Printf("session: create failed (shell=%q cols=%d rows=%d cwd=%q): %v", shell, cols, rows, cwd, err)
		return "", err
	}
	log.Printf("session: created %q (shell=%q cols=%d rows=%d cwd=%q)", id, shell, cols, rows, cwd)
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

// GetSettings returns the user's preferences from config.json, with every
// missing or unusable key filled from the defaults. It never fails.
//
// The file is read on every call rather than cached. It is a few hundred bytes,
// the frontend asks for it when a pane is built or the user reloads settings,
// and caching would turn "Reload Settings" into a command that reloads whatever
// we happened to parse at launch.
func (a *App) GetSettings() settings.Settings {
	set, warnings := settings.Load(a.cfg.Dir())
	for _, w := range warnings {
		log.Printf("settings: %s", w)
	}
	return set
}

// OpenSettingsFile opens config.json in whatever the user has associated with
// .json, writing it with the defaults first if it is not there yet.
//
// It never overwrites an existing file — including one that does not parse.
// That is the case this guard is for: a stray comma makes Load fall back to the
// defaults, and if opening the file then rewrote it, the fix the user came here
// to make would be gone before they saw it.
func (a *App) OpenSettingsFile() {
	path := settings.Path(a.cfg.Dir())

	switch _, err := os.Stat(path); {
	case err == nil:
		// Already there — the user's text, left exactly as it is.
	case os.IsNotExist(err):
		if err := settings.Save(a.cfg.Dir(), settings.Defaults()); err != nil {
			log.Printf("settings: could not create %q: %v", path, err)
			return
		}
		log.Printf("settings: created %q with the defaults", path)
	default:
		// Present but not stat-able. Opening it may still work, and `open` will
		// say so far more usefully than we can.
		log.Printf("settings: %q cannot be inspected (%v); opening it anyway", path, err)
	}

	if err := startAndReap(exec.Command(updater.OpenPath, path)); err != nil {
		log.Printf("settings: could not open %q: %v", path, err)
	}
}

// SetWindowTitle sets the native window title.
//
// The string comes from a pane, which got it from the terminal, which got it
// from whatever the shell printed — an OSC 0/2 sequence is something any
// program the user runs can emit. So it is sanitised on the way in: control
// characters and the invisible format class go, and the length is capped. An
// empty result restores the app's own name rather than leaving a blank title
// bar.
func (a *App) SetWindowTitle(title string) {
	if a.ctx == nil {
		return
	}
	wailsRuntime.WindowSetTitle(a.ctx, sanitizeWindowTitle(title))
}

// What a flood of BEL bytes is allowed to cost.
//
// Bell is a binding, and the byte behind each call is one that a program the
// user ran printed: `cat` of a binary emits thousands in a second, and every
// one of them used to become two dispatch_async blocks on the main queue — the
// queue that also draws the window. The frontend coalesces too, but per pane,
// which is the wrong unit twice over: a pane cannot see what the other panes
// are doing, and its allowance multiplies by however many are open.
//
// bellBurst then bellRatePerSec: five beeps back to back, then five a second.
// The system alert sound is roughly that long, so a higher rate is not audible
// as separate bells anyway — it is only main-queue work.
//
// bellAttentionGap bounds the Dock bounce separately and much harder. It leaves
// the tile marked and is the more expensive of the two calls, and one bounce a
// second already says everything a bounce can say.
const (
	bellBurst        = 5
	bellRatePerSec   = 5
	bellAttentionGap = time.Second
	// bellDropSummaryGap is the shortest interval between two "dropped" lines.
	// Logging every dropped bell would turn a bell flood into a log flood.
	bellDropSummaryGap = time.Minute
)

var (
	// bellClock is the clock the limiter reads. A variable so tests can step
	// time instead of sleeping through a refill window — the same seam
	// logging.frontendClock uses.
	bellClock = time.Now

	// bells is process-wide on purpose: the point of this layer is to bound the
	// beeps from *all* panes together.
	bells = newBellLimiter(bellBurst, bellRatePerSec, bellAttentionGap)
)

// bellLimiter is the backend half of the bell rate limit: a token bucket for
// the sound, and a separate minimum gap for the Dock bounce.
type bellLimiter struct {
	mu     sync.Mutex
	burst  float64
	refill float64
	gap    time.Duration

	tokens float64
	// last is when tokens was last brought up to date. Zero until the first
	// take, so the bucket starts full rather than crediting the refill for every
	// second since the zero time.
	last time.Time
	// attentionAt is the earliest the next Dock bounce may happen.
	attentionAt time.Time
	// dropped counts the bells refused since the last summary line, and
	// summaryAt is the earliest the next one may be written.
	dropped   int
	summaryAt time.Time
}

func newBellLimiter(burst, refillPerSecond float64, gap time.Duration) *bellLimiter {
	return &bellLimiter{burst: burst, refill: refillPerSecond, gap: gap, tokens: burst}
}

// take decides what one bell may do: nothing, ring, or ring and bounce the Dock
// icon.
//
// attention is never true without beep. A bell suppressed as noise must not
// still be able to mark the Dock tile — the tile is the part that outlives the
// event, so it is the part a flood must not be able to drive.
//
// dropped is non-zero at most once per bellDropSummaryGap and carries how many
// bells have been refused since the previous summary; the caller logs it as one
// line. As in logging's bucket, a pending count is flushed on the first call at
// or after the window closes whether or not that call was itself dropped, so a
// flood that stops still gets its final count into the log.
func (b *bellLimiter) take(now time.Time) (beep, attention bool, dropped int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.last.IsZero() {
		if elapsed := now.Sub(b.last); elapsed > 0 {
			b.tokens += elapsed.Seconds() * b.refill
			if b.tokens > b.burst {
				b.tokens = b.burst
			}
		}
	}
	b.last = now

	beep = b.tokens >= 1
	if beep {
		b.tokens--
		if !now.Before(b.attentionAt) {
			attention = true
			b.attentionAt = now.Add(b.gap)
		}
	} else {
		b.dropped++
	}

	if b.dropped > 0 && !now.Before(b.summaryAt) {
		dropped = b.dropped
		b.dropped = 0
		b.summaryAt = now.Add(bellDropSummaryGap)
	}
	return beep, attention, dropped
}

// Bell rings the system alert sound and, when El Terminalo is in the
// background, bounces its Dock icon once.
//
// The frontend calls this only when the user's "bell" setting includes sound;
// the visual flash is a pane-level effect it does itself, because only it knows
// which pane rang.
//
// Rate limited across the whole process — see bellLimiter. The frontend's own
// coalescing is per pane and cannot bound the total, and a bell that is not
// rung is not worth an alert of its own, so a dropped one is only ever reported
// as a periodic count.
func (a *App) Bell() {
	beep, attention, dropped := bells.take(bellClock())
	if dropped > 0 {
		log.Printf("bell: dropped %d bell(s) (rate limited)", dropped)
	}
	if !beep {
		return
	}
	macos.Beep()
	if attention {
		macos.RequestAttention()
	}
}

// notifyLog bounds how many log lines a burst of notifications can produce.
//
// It is the same hazard the bell limiter exists for and the same bucket: a
// notification is asked for by the frontend when it sees OSC 9 or OSC 777, and
// those are sequences any program the user runs can print as fast as it likes.
// Only the *log* is limited here — never the notification itself, because the
// binding's contract is that false means "unavailable or unauthorized" and
// nothing else. Its attention result is unused.
var notifyLog = newBellLimiter(bellBurst, bellRatePerSec, bellAttentionGap)

// logNotification writes one rate-limited line about a notification.
func logNotification(format string, args ...any) {
	allowed, _, dropped := notifyLog.take(bellClock())
	if dropped > 0 {
		log.Printf("notification: %d further line(s) about notifications were suppressed (rate limited)", dropped)
	}
	if allowed {
		log.Printf(format, args...)
	}
}

// Notify shows a native notification and reports whether it was posted.
//
// False means the notification will not reach the user: either this process
// cannot use notifications at all (an unbundled build — see internal/macos) or
// the user has not granted permission. The frontend uses that to fall back to
// its own in-window indication rather than silently doing nothing.
//
// paneKey is opaque here and in the macos package. Whatever the frontend passes
// comes back to it on notificationActivatedEvent when the user clicks the
// notification, and it is the only thing that says which pane to focus.
//
// Never blocks: delivery happens on the main queue after this returns.
func (a *App) Notify(title, body, paneKey string) bool {
	title = sanitizeNotificationText(title, maxNotificationTitleBytes)
	body = sanitizeNotificationText(body, maxNotificationBodyBytes)
	paneKey = truncateBytes(paneKey, maxPaneKeyBytes)

	if title == "" && body == "" {
		// A banner with nothing in it is worse than no banner: it appears, says
		// nothing, and cannot be explained afterwards.
		logNotification("notification: suppressed, it had no text after sanitising")
		return false
	}
	if title == "" {
		title = appTitle
	}

	if !macos.Notify(title, body, paneKey) {
		logNotification("notification: %q suppressed; notifications are unavailable or not authorized", title)
		return false
	}
	logNotification("notification: posted %q (paneKey=%q)", title, paneKey)
	return true
}

// NotificationsReady reports whether Notify would actually reach the user —
// notifications are usable in this process and the user has said yes.
//
// The frontend asks before offering "notify me when this finishes", so that the
// offer is not made in a build or a state where it cannot be kept.
func (a *App) NotificationsReady() bool {
	return macos.NotificationsReady()
}

// SetDockBadge puts a badge on the app's Dock tile; an empty label clears it.
//
// The frontend uses it for the number of panes wanting attention, which is the
// one piece of state worth showing to someone who has switched to another app.
// Not logged: it changes as often as a command finishes, and a line per change
// would bury everything else.
func (a *App) SetDockBadge(label string) {
	macos.SetDockBadge(sanitizeBadgeLabel(label))
}

// notificationActivated brings the window forward and tells the frontend which
// pane a clicked notification was about.
//
// It runs on its own goroutine — see macos.OnNotificationActivated — and that
// is what makes the Wails runtime calls below safe from here: each of them hops
// to the main queue, and the AppKit delegate thread the click actually arrived
// on is the one that would have had to drain it.
func (a *App) notificationActivated(paneKey string) {
	if a.ctx == nil {
		// Only reachable from a notification delivered before the window
		// existed, which this app never sends; EventsEmit on a nil context
		// panics, and a panic here would take the app down.
		log.Printf("notification: clicked (paneKey=%q) before there was a window", paneKey)
		return
	}
	// Unminimise before Show: a window sitting in the Dock is not brought back
	// by makeKeyAndOrderFront alone, and clicking a notification is a request to
	// look at something.
	wailsRuntime.WindowUnminimise(a.ctx)
	wailsRuntime.WindowShow(a.ctx)
	log.Printf("notification: clicked (paneKey=%q); window brought forward", paneKey)
	wailsRuntime.EventsEmit(a.ctx, notificationActivatedEvent, map[string]string{"paneKey": paneKey})
}

// ImportColorScheme asks the user for an iTerm2 .itermcolors or a Ghostty theme
// file, converts it to a theme, saves it, and returns the theme's name.
//
// ("", nil) means the user cancelled the dialog, which is not a failure and
// must not be reported as one. An error is written to be shown: it names the
// format we tried to read and what was wrong with it.
//
// themesChangedEvent is emitted from here rather than from the menu callback so
// that there is one import path, whether the user came through File › Import
// Color Scheme… (which sends the menu action, and the frontend calls this) or
// through the palette (which calls this directly).
//
// The event is emitted last, after theme.Upsert has the theme durably on disk,
// and only on success: it is the frontend's sole cue to refetch, so an event
// sent before the write would have it read the file it is being told about
// before that file exists, and one sent after a failed write would have it
// switch to a theme that is not there.
func (a *App) ImportColorScheme() (string, error) {
	if a.ctx == nil {
		return "", fmt.Errorf("the window is not ready yet")
	}

	path, err := wailsRuntime.OpenFileDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "Import Color Scheme",
		Filters: []wailsRuntime.FileFilter{
			// Ghostty themes have no extension at all, which is why the second
			// filter is not a courtesy: without it they cannot be picked.
			{DisplayName: "iTerm2 Color Schemes (*.itermcolors)", Pattern: "*.itermcolors"},
			{DisplayName: "All Files", Pattern: "*"},
		},
	})
	if err != nil {
		log.Printf("theme import: the file dialog failed: %v", err)
		return "", err
	}
	if path == "" {
		log.Printf("theme import: cancelled")
		return "", nil
	}

	data, err := readBounded(path, maxSchemeBytes)
	if err != nil {
		log.Printf("theme import: %q could not be read: %v", path, err)
		return "", fmt.Errorf("could not read %s: %w", filepath.Base(path), err)
	}

	name := theme.SchemeName(path)
	imported, err := theme.ParseScheme(name, data)
	if err != nil {
		log.Printf("theme import: %q did not parse: %v", path, err)
		return "", fmt.Errorf("%s could not be read as a colour scheme — %w", filepath.Base(path), err)
	}

	if err := theme.Upsert(a.cfg.Dir(), imported); err != nil {
		log.Printf("theme import: %q parsed but could not be saved: %v", path, err)
		return "", fmt.Errorf("could not save the theme: %w", err)
	}

	log.Printf("theme import: saved %q from %q", imported.Name, path)
	wailsRuntime.EventsEmit(a.ctx, themesChangedEvent, map[string]string{"name": imported.Name})
	return imported.Name, nil
}

// readBounded reads at most limit bytes from path, and reports an error rather
// than a truncated file when there is more. A truncated colour scheme would
// parse into a half-applied theme, which is worse than not importing one.
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// limit+1 so a file of exactly limit bytes still reads whole and only a
	// larger one trips the check.
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("the file is larger than %d bytes", limit)
	}
	return data, nil
}

// emitMenuAction forwards a native menu item to the frontend.
//
// Called from an AppKit menu callback, which can only happen once the window is
// up — but a nil context here would panic inside Wails and take the app down
// with it, so it is checked rather than assumed.
func (a *App) emitMenuAction(action string) {
	if a.ctx == nil {
		log.Printf("menu: %q ignored, there is no window yet", action)
		return
	}
	wailsRuntime.EventsEmit(a.ctx, menuActionEvent, map[string]string{"action": action})
}

// toggleFullScreen flips the window between full screen and not.
//
// Done in Go rather than through the Window menu's own "Full Screen" item
// because that one is a role Wails builds with enterFullScreenMode:, which acts
// on the *view*; this asks the window, which is what the user means.
func (a *App) toggleFullScreen() {
	if a.ctx == nil {
		return
	}
	if wailsRuntime.WindowIsFullscreen(a.ctx) {
		wailsRuntime.WindowUnfullscreen(a.ctx)
		return
	}
	wailsRuntime.WindowFullscreen(a.ctx)
}

// openIssues sends Help › Report an Issue to the default browser.
func (a *App) openIssues() {
	if a.ctx == nil {
		return
	}
	wailsRuntime.BrowserOpenURL(a.ctx, issuesURL)
}

// stripUnsafeRunes drops the character classes that let a string reorder or
// hide what is printed around it, plus every control character.
//
// The classes are the same ones logging.sanitize drops, and for the same
// reason: U+202E and its neighbours reverse the rendering of everything after
// them, so a program that sets its own title — or asks for a notification —
// could otherwise make the window or the banner claim to be something it is
// not.
func stripUnsafeRunes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// truncateBytes caps s at max bytes without ever splitting a rune — a tail cut
// mid-sequence renders as a replacement character.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// sanitizeWindowTitle makes an arbitrary terminal-supplied string safe to put
// in the title bar: one line, bounded, and free of the characters that reorder
// or hide what is around them. An empty result restores the app's own name
// rather than leaving a blank title bar.
func sanitizeWindowTitle(title string) string {
	out := strings.TrimSpace(stripUnsafeRunes(title))
	if len(out) > maxWindowTitleBytes {
		out = strings.TrimSpace(truncateBytes(out, maxWindowTitleBytes))
	}
	if out == "" {
		return appTitle
	}
	return out
}

// sanitizeNotificationText bounds one field of a notification. Unlike a window
// title an empty result is left empty — Notify decides what an empty title or
// body means, and a body that falls back to the app's name would be worse than
// no body at all.
func sanitizeNotificationText(s string, maxBytes int) string {
	out := strings.TrimSpace(stripUnsafeRunes(s))
	if len(out) > maxBytes {
		out = strings.TrimSpace(truncateBytes(out, maxBytes))
	}
	return out
}

// sanitizeBadgeLabel reduces a label to something that fits in a Dock tile
// badge: one line, no character that can reorder what is around it, and at most
// maxDockBadgeRunes runes. It is normally a small number.
//
// It cannot just call stripUnsafeRunes: a newline is a control character, so
// "two\nlines" would come out as "twolines" with the two halves jammed
// together. Whitespace is folded to a single space first, and only then is the
// rest dropped.
func sanitizeBadgeLabel(label string) string {
	var b strings.Builder
	b.Grow(len(label))
	for _, r := range label {
		switch {
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp):
			// The classes stripUnsafeRunes drops, for the same reason.
		default:
			b.WriteRune(r)
		}
	}

	out := strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(out) <= maxDockBadgeRunes {
		return out
	}
	count := 0
	for i := range out {
		if count == maxDockBadgeRunes {
			return strings.TrimSpace(out[:i])
		}
		count++
	}
	return out
}

// describeSettings renders the loaded settings as one log line. Only the values
// are interesting — the keys are documented in the README and in the file
// itself — so this stays short enough to read at a glance in a bug report.
func describeSettings(s settings.Settings) string {
	shell := s.Shell
	if shell == "" {
		shell = "(default)"
	}
	return fmt.Sprintf(
		"font=%q size=%v lineHeight=%v cursor=%s blink=%t scrollback=%d shell=%s optionIsMeta=%t bell=%s copyOnSelect=%t confirmQuit=%s",
		s.FontFamily, s.FontSize, s.LineHeight, s.CursorStyle, s.CursorBlink,
		s.Scrollback, shell, s.OptionIsMeta, s.Bell, s.CopyOnSelect, s.ConfirmQuit)
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

	if err := theme.Upsert(a.cfg.Dir(), newTheme); err != nil {
		// Logged here because the theme wizard only reports this to the
		// webview's console, which goes nowhere a user can retrieve — and the
		// failure this now produces, a themes.json that stopped parsing, is
		// exactly what someone would come to the log to understand.
		log.Printf("theme: %q could not be saved: %v", name, err)
		return err
	}
	return nil
}

// DeleteTheme removes a custom theme by name.
func (a *App) DeleteTheme(name string) error {
	existing, err := theme.LoadUserThemes(a.cfg.Dir())
	if err != nil {
		log.Printf("theme: %q could not be deleted: %v", name, err)
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

// StartTranscript begins recording a pane's output to a file and returns the
// path it is writing to.
//
// The path is chosen here rather than by the caller, for two reasons. The
// webview must not be able to name a file the backend then opens for writing;
// and a transcript is only useful if it can be found again, which means one
// predictable place — <config>/transcripts/<date>/<time>-<session>.log — rather
// than wherever the pane's shell happened to be.
//
// What lands in the file is the raw stream, escape sequences and all; see the
// README. That is the point of it — a transcript that had been stripped of
// colour and cursor motion would no longer be a record of what the pane did.
//
// A recording does not always end because somebody asked it to: it stops itself
// at 256 MiB, and it gives up if a write fails. The frontend hears about both on
// ptymanager.TranscriptStoppedEvent ("transcript:stopped"), carrying
// {sessionID, path, reason} with reason "cap" or "error". Without listening for
// it a pane's recording indicator would keep pulsing over a file that stopped
// growing, because TranscriptPath then reports "" — which is also what a pane
// that never recorded reports.
func (a *App) StartTranscript(sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("a transcript needs a session")
	}

	root := filepath.Join(a.cfg.Dir(), transcriptsDirName)
	now := time.Now()
	dir := filepath.Join(root, now.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("transcript: cannot create %q: %v", dir, err)
		return "", fmt.Errorf("cannot create the transcripts folder: %w", err)
	}
	// MkdirAll only applies its mode to directories it actually creates, so a
	// transcripts/ left at 0755 by an older build keeps it forever — and these
	// files hold everything a pane printed. Best-effort, as in logging.Init: a
	// directory we cannot chmod is still one we can record into.
	if err := os.Chmod(root, 0o700); err != nil {
		log.Printf("transcript: could not tighten the mode of %q: %v", root, err)
	}

	path := filepath.Join(dir, fmt.Sprintf("%s-%s.log", now.Format("150405"), shortSessionID(sessionID)))
	if err := a.ptyMgr.StartTranscript(sessionID, path); err != nil {
		log.Printf("transcript: could not start recording %q to %q: %v", sessionID, path, err)
		return "", err
	}
	log.Printf("transcript: recording %q to %q", sessionID, path)
	return path, nil
}

// StopTranscript stops recording a pane's output.
func (a *App) StopTranscript(sessionID string) error {
	// Read before the stop: afterwards the manager reports "" and the log line
	// could not say which file was just finished.
	path := a.ptyMgr.TranscriptPath(sessionID)
	if err := a.ptyMgr.StopTranscript(sessionID); err != nil {
		log.Printf("transcript: could not stop recording %q: %v", sessionID, err)
		return err
	}
	log.Printf("transcript: stopped recording %q (%q)", sessionID, path)
	return nil
}

// TranscriptPath returns the file a pane is being recorded to, or "" when it is
// not being recorded. The frontend calls it to render the menu item's state.
func (a *App) TranscriptPath(sessionID string) string {
	return a.ptyMgr.TranscriptPath(sessionID)
}

// shortSessionID reduces a session id to the leading characters that go into a
// transcript's filename, with anything that is not plainly filename-safe
// replaced.
//
// Session ids are UUIDs the manager generated, so in practice this only ever
// takes the first eight hex characters — but the id arrives from the webview,
// and it is being joined into a path.
func shortSessionID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if b.Len() >= transcriptIDChars {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "session"
	}
	return b.String()
}

// RevealPath shows a file or folder in Finder, with it selected.
//
// Absolute paths only, and it has to exist: `open` treats a leading "-" as a
// flag, and the string arrives from the webview. Failures are logged rather
// than returned — this is behind a menu item, and there is nothing the caller
// could do with the error.
func (a *App) RevealPath(path string) {
	if path == "" {
		return
	}
	if !filepath.IsAbs(path) {
		log.Printf("reveal: %q is not an absolute path", path)
		return
	}
	// Lstat, not Stat: a symlink whose target is gone is still a thing Finder
	// can show.
	if _, err := os.Lstat(path); err != nil {
		log.Printf("reveal: %q is not there (%v)", path, err)
		return
	}
	if err := startAndReap(exec.Command(updater.OpenPath, "-R", path)); err != nil {
		log.Printf("reveal: could not reveal %q: %v", path, err)
	}
}

// The two editors OpenPath falls back on when the user has not named one.
//
// Visual Studio Code first because it opens a *folder*, which is what the
// palette's "Open Folder in Editor" always hands over; TextEdit only after it,
// and only for a file, because it declares no folder document type and answers
// a directory with an error sheet of its own.
const (
	preferredEditorApp = "Visual Studio Code"
	// fileEditorApp is the one editor macOS is guaranteed to have.
	fileEditorApp = "TextEdit"
)

// appSearchDirs lists where a named application bundle is looked for, most
// specific first. A var so a test can point it somewhere of its own.
//
// This is a deliberate stand-in for asking LaunchServices. `open -a <name>`
// would resolve a bundle anywhere on the disk, but it only answers by exiting
// non-zero *after* the fact — so a chain built on it would have to run up to
// three subprocesses and wait for each, on a Wails binding the palette is
// awaiting. Looking on the disk answers the same question before anything is
// launched, at the cost of not finding an editor installed somewhere unusual;
// such a user can name it in $VISUAL by full path.
var appSearchDirs = func() []string {
	dirs := make([]string, 0, 5)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}
	return append(dirs,
		"/Applications",
		"/Applications/Utilities",
		"/System/Applications",
		"/System/Applications/Utilities",
	)
}

// findAppBundle resolves an application name — "Zed", "Zed.app", or a full path
// to a bundle — to the bundle on disk, or "" when there is none.
func findAppBundle(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.HasPrefix(name, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			name = filepath.Join(home, name[2:])
		}
	}
	// A bundle is a directory. Anything else with the name — a symlink to one is
	// still fine, which is why this is Stat rather than Lstat — is not one.
	isBundle := func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && info.IsDir()
	}

	if filepath.IsAbs(name) {
		if strings.EqualFold(filepath.Ext(name), ".app") && isBundle(name) {
			return name
		}
		// An absolute path that is not a bundle: a terminal editor named in full,
		// like /opt/homebrew/bin/nvim. `open -a` cannot run one.
		return ""
	}

	base := name
	if !strings.EqualFold(filepath.Ext(base), ".app") {
		base += ".app"
	}
	// Reject a name that would climb out of the search directories; $EDITOR is
	// the user's own, but it is still a string being joined into a path.
	if strings.ContainsRune(base, filepath.Separator) {
		return ""
	}
	for _, dir := range appSearchDirs() {
		if candidate := filepath.Join(dir, base); isBundle(candidate) {
			return candidate
		}
	}
	return ""
}

// resolveEditorApp picks the application OpenPath should hand a path to, and
// says why. An empty app means there is no editor to name and the path goes to
// `open` on its own, which is the OS default — Finder, for a folder.
//
// The order, which the README documents and the frontend's "Open Folder in
// Editor" command depends on:
//
//  1. $VISUAL, then $EDITOR. Only if the value names an application bundle:
//     the common settings are terminal editors — vim, nano, an absolute path to
//     nvim — and `open -a vim` is not a thing macOS can do. "code" is not a
//     bundle either, but it falls through to step 2 and lands where its user
//     meant it to.
//  2. Visual Studio Code, if it is installed.
//  3. TextEdit, but only for a file. It cannot open a folder.
//  4. Nothing: `open <path>`.
//
// Worth knowing about step 1: a GUI app launched from the Dock or Finder
// inherits launchd's environment, not a login shell's, so $VISUAL and $EDITOR
// are normally unset here however carefully they are set in ~/.zshrc. They are
// read because a user who wants them honoured can set them for the GUI session
// (`launchctl setenv VISUAL "/Applications/Zed.app"`), not because most users
// will have them.
func resolveEditorApp(isDir bool) (app, why string) {
	for _, key := range []string{"VISUAL", "EDITOR"} {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			continue
		}
		// The whole value first: both an application name and a bundle path can
		// contain spaces ("Visual Studio Code", "/Applications/Sublime
		// Text.app"), so splitting before trying it would break the two settings
		// most likely to work.
		if bundle := findAppBundle(value); bundle != "" {
			return bundle, "$" + key
		}
		// Then the first field alone: $EDITOR routinely carries flags ("code -w",
		// "subl --wait"), and `open -a` takes an application, not a command line.
		if fields := strings.Fields(value); len(fields) > 1 {
			if bundle := findAppBundle(fields[0]); bundle != "" {
				return bundle, "$" + key
			}
		}
		log.Printf("open: $%s is %q, which is not an application bundle macOS can open a path with; trying the next editor", key, value)
	}

	if bundle := findAppBundle(preferredEditorApp); bundle != "" {
		return bundle, preferredEditorApp
	}
	if !isDir {
		if bundle := findAppBundle(fileEditorApp); bundle != "" {
			return bundle, fileEditorApp
		}
	}
	return "", "the default handler"
}

// OpenPath opens a file or folder in the user's editor.
//
// It used to be a bare `open <path>`, which hands a *folder* to its default
// handler — Finder. That made the palette's "Open Folder in Editor" a second,
// slower "Reveal in Finder", and made this binding's own documentation false.
// See resolveEditorApp for the order the editor is chosen in.
//
// Unlike RevealPath it reports failure: the caller asked for something to
// happen on screen, and nothing did. It does not wait for the editor — that is
// a GUI app launching, and this runs on a binding the palette is awaiting — so
// the error it can report is that `open` itself could not be started, not that
// the editor declined the path.
func (a *App) OpenPath(path string) error {
	if path == "" {
		return fmt.Errorf("no path to open")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s is not an absolute path", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("open: %q cannot be opened: %v", path, err)
		return fmt.Errorf("%s cannot be opened: %w", filepath.Base(path), err)
	}

	editor, why := resolveEditorApp(info.IsDir())
	args := []string{path}
	if editor != "" {
		args = []string{"-a", editor, path}
	}
	log.Printf("open: %q in %q (from %s)", path, editor, why)

	if err := startAndReap(exec.Command(updater.OpenPath, args...)); err != nil {
		log.Printf("open: could not open %q: %v", path, err)
		return err
	}
	return nil
}

// SaveWorkspace stores the frontend's layout JSON under a name the user chose.
// stateJSON is the same document SaveAppState writes; see internal/workspace.
func (a *App) SaveWorkspace(name, stateJSON string) error {
	if err := workspace.Save(a.cfg.Dir(), name, stateJSON); err != nil {
		log.Printf("workspace: %q was not saved: %v", name, err)
		return err
	}
	log.Printf("workspace: saved %q (%d bytes)", name, len(stateJSON))
	return nil
}

// ListWorkspaces returns every saved workspace, sorted by name. Never nil.
func (a *App) ListWorkspaces() []workspace.Info {
	return workspace.List(a.cfg.Dir())
}

// LoadWorkspace returns the layout JSON saved under a name. The frontend
// rebuilds its tabs and panes from it exactly as it does from state.json.
func (a *App) LoadWorkspace(name string) (string, error) {
	slug := a.workspaceSlug(name)
	state, err := workspace.Load(a.cfg.Dir(), slug)
	if err != nil {
		log.Printf("workspace: %q could not be loaded: %v", name, err)
		return "", err
	}
	log.Printf("workspace: loaded %q from %q (%d bytes)", name, slug, len(state))
	return state, nil
}

// DeleteWorkspace removes a saved workspace.
func (a *App) DeleteWorkspace(name string) error {
	slug := a.workspaceSlug(name)
	if err := workspace.Delete(a.cfg.Dir(), slug); err != nil {
		log.Printf("workspace: %q could not be deleted: %v", name, err)
		return err
	}
	log.Printf("workspace: deleted %q (%s)", name, slug)
	return nil
}

// workspaceSlug maps whatever the frontend passed onto the file on disk.
//
// Both spellings have to work. The picker lists workspaces and hands back the
// Slug it was given; a user typing into a "which workspace?" prompt gives the
// display Name. Slugging the name is nearly always enough — the function is
// idempotent, so a slug slugs to itself — but not when two different names
// collided and the second one was stored under a numbered slug: slugging *that*
// name would point at the first one's file. So the list is consulted first, and
// slugging is only the fallback that gets a recognisable name into the error.
func (a *App) workspaceSlug(name string) string {
	name = strings.TrimSpace(name)
	for _, info := range workspace.List(a.cfg.Dir()) {
		if info.Slug == name || strings.EqualFold(info.Name, name) {
			return info.Slug
		}
	}
	return workspace.Slug(name)
}
