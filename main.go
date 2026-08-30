package main

import (
	"embed"
	"fmt"
	"log"
	"os"

	"github.com/albinanto/elterminalo/internal/config"
	"github.com/albinanto/elterminalo/internal/logging"
	"github.com/albinanto/elterminalo/internal/settings"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed frontend/dist
var assets embed.FS

// appTitle is the window's title and the name the About panel shows. It is also
// what a pane's title falls back to when the terminal sets an empty one — see
// sanitizeWindowTitle.
const appTitle = "El Terminalo"

// fallbackShell is used when $SHELL is unset, which is how a process launched
// by launchd rather than by a login shell can arrive.
const fallbackShell = "/bin/zsh"

func main() {
	cfg, err := config.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Config error: %v\n", err)
		os.Exit(1)
	}

	// Before anything else that could fail: from here on every package's
	// log.Printf lands in a file the user can send us. A failure here is
	// reported and ignored — log.Printf still reaches stderr, and an app that
	// refuses to start because it cannot open its log would be a far worse bug
	// than the one this log exists to diagnose.
	logPath, logErr := logging.Init(cfg.Dir())
	if logErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", logErr)
	}

	// The shell resolved here is the *fallback*, not the shell every pane gets.
	// ptymanager.Manager takes one at construction, and it is what a session
	// spawns when the setting cannot be used — but App.CreateSession resolves
	// "shell" from the settings file per session and passes it to
	// CreateSessionWithShell, so an edited setting reaches the next pane opened
	// (after Reload Settings) rather than waiting for a relaunch. Existing panes
	// keep the shell they started with, because their process is already
	// running.
	//
	// Resolving it here as well is what makes that fallback a real path rather
	// than a $SHELL that was never checked: a "shell" pointing at something
	// deleted since the last launch is reported once, at startup, instead of on
	// every pane.
	//
	// Warnings from Load are not reported here; App.startup logs the whole
	// resolved settings line, and duplicating it before the banner would only
	// make the log harder to read. The shell is the exception, because this is
	// the one decision made before there is an App to log it.
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = fallbackShell
	}
	set, _ := settings.Load(cfg.Dir())
	shell, shellWarning := settings.ResolveShell(set.Shell, shell)

	log.Printf("=== El Terminalo %s starting: pid=%d configDir=%s shell=%s log=%s",
		Version, os.Getpid(), cfg.Dir(), shell, logPath)
	if logErr != nil {
		log.Printf("logging: file unavailable (%v); this session's log is stderr only", logErr)
	}
	if shellWarning != "" {
		log.Printf("startup: settings: %s", shellWarning)
	}

	app := NewApp(shell, cfg)

	err = wails.Run(&options.App{
		Title:     appTitle,
		Width:     1024,
		Height:    768,
		MinWidth:  400,
		MinHeight: 300,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup:     app.startup,
		OnShutdown:    app.shutdown,
		OnBeforeClose: app.beforeClose,
		Bind: []interface{}{
			app,
		},
		Mac: &mac.Options{
			TitleBar: &mac.TitleBar{
				TitlebarAppearsTransparent: true,
				HideTitle:                  true,
				HideTitleBar:               false,
				FullSizeContent:            true,
			},
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			About: &mac.AboutInfo{
				Title:   appTitle,
				Message: "A modern terminal for agent coding",
			},
		},
		Menu:                     buildMenu(app),
		Frameless:                false,
		EnableDefaultContextMenu: false,
		// EnableFileDrop makes the native drop handler report the real absolute
		// paths of the dropped files (App.startup subscribes with
		// runtime.OnFileDrop). The HTML5 path could not: a webview drop only
		// exposes the file's *bytes*, so the old code copied them into a temp
		// directory and inserted the copy's path — the wrong path, a duplicate
		// of the data, and nothing at all for a dropped folder.
		//
		// DisableWebViewDrop stays false, which is not the obvious choice, so:
		// WailsWebView.m's performDragOperation posts the native "DD:" message
		// for *every* drag it sees and only then branches on this flag. With it
		// true the method returns YES without calling super, so WKWebView never
		// sees the drop and the HTML5 "drop" event never fires — for any drag
		// carrying a pasteboard type list, which is all of them. Two things
		// break as a result. A plain *text* drag onto a pane is swallowed: it
		// has no file URLs, so the native message carries nothing, and the drop
		// event that used to reach xterm's textarea is gone. And a *web link*
		// drag is reported as a bogus path, because the method builds its
		// payload with [NSURL fileSystemRepresentation] over everything
		// readObjectsForClasses:@[NSURL] returned — which, with no
		// NSPasteboardURLReadingFileURLsOnly option, includes http(s) URLs;
		// "https://example.com/foo/bar?q=1" comes out as "/foo/bar".
		//
		// With it false the native message is still posted (it is sent before
		// the branch) *and* super runs, so the frontend also gets the HTML5
		// event: it preventDefault()s that event so the webview cannot navigate
		// to a dropped file, ignores file drops there because the native path
		// already handles them, and uses it for text. registerFileDrop filters
		// the native payload down to paths that really exist, which is what
		// keeps the bogus "/foo/bar" out of the terminal.
		//
		// draggingEntered/draggingUpdated call super on both branches, so
		// "dragover"/"dragleave" — the frontend's hover styling — are unaffected
		// either way.
		DragAndDrop: &options.DragAndDrop{
			EnableFileDrop:     true,
			DisableWebViewDrop: false,
		},
	})

	if err != nil {
		log.Printf("fatal: wails.Run: %v", err)
		os.Exit(1)
	}
	log.Printf("=== El Terminalo exited cleanly")
}

// buildMenu assembles the macOS menu bar.
//
// # Why almost every item here duplicates a keyboard handler in the frontend
//
// The menu is not the primary route to any of these commands, and it cannot be.
// A Wails window's content view is a WKWebView, and AppKit offers a key-down
// event to the key window's view hierarchy — via performKeyEquivalent: — before
// it offers it to the main menu.
//
// What WKWebView does with that offer, measured rather than assumed: it returns
// YES on the first pass and forwards the key to the page, so AppKit stops there
// and the menu is never consulted. If the page's keydown handler then calls
// preventDefault(), that is the end of it. If it does not, WebKit re-injects the
// event, AppKit runs the pass again, and *this* time the menu accelerator fires.
// So the deciding call is preventDefault() and nothing else — stopPropagation()
// does not count, and neither does simply having a handler that did the work.
//
// The frontend calls preventDefault() for every key it handles, and for all
// Cmd-keys while a modal is open, so today each shortcut reaches exactly one
// handler: the frontend's while the webview is first responder — which is all
// the time, since a terminal is what the window contains — and this menu's when
// nobody claimed it. The accelerators here are therefore mostly labels, and the
// items still have to carry them: an accelerator is how a user *discovers* a
// shortcut, and clicking the item with the mouse must do what typing it does.
//
// That is also why the frontend keeps a short de-duplication window on
// menu:action. It is not needed for anything the frontend handles today; it is
// the guard for the next handler that forgets its preventDefault(), where the
// visible symptom would otherwise be one keystroke opening two tabs.
//
// # Accelerators
//
// keys.CmdOrCtrl/Shift/Control lower-case the key for you; keys.Combo does not —
// it stores whatever it is given. Every Combo below therefore passes a
// lower-case key with an explicit ShiftKey modifier, which is also the form
// AppKit wants: a key equivalent of "d" with Command|Shift displays as ⇧⌘D and
// matches it. Punctuation ("=", "-", "[", "]", ",") and digits pass straight
// through to setKeyEquivalent: — only the named keys in keys/parser.go ("tab",
// "space", "f1"…) are translated, and none of those are used here.
//
// The one pair worth watching is ⇧⌘] and ⇧⌘[. A key equivalent of "]" with a
// Shift flag is the form Safari and Chrome use for the same commands, but on a
// US layout the *event* carries "}" once Shift is down, and AppKit's matching of
// punctuation against a shifted equivalent is not something this file can prove
// without a running window. If Next/Previous Tab ever stop firing from the menu
// while every other item works, the equivalents to try are "}" and "{". Nothing
// user-visible depends on the answer today: the frontend binds both spellings
// itself and consumes the keystroke before the menu is offered it.
func buildMenu(app *App) *menu.Menu {
	appMenu := menu.NewMenu()

	// The App menu is a role: Wails builds About/Hide/Hide Others/Show All/Quit
	// on the native side, and Quit in particular has to stay that way — it is
	// wired to the WailsContext's Quit, which is what routes Cmd-Q through
	// OnBeforeClose and therefore through the save-then-quit handshake.
	appMenu.Append(menu.AppMenu())

	file := appMenu.AddSubmenu("File")
	// Settings… belongs under the app's own menu, next to About, and that is
	// the one place it cannot go: menu.AppMenu() is a role, built entirely
	// inside WailsMenu.m's appendRole:, with no seam to insert an item into.
	// The alternative — hand-building the App menu — would mean giving up the
	// native About panel and the Quit wiring above, which is a far worse trade
	// than a misplaced item. So it heads the File menu instead, and Cmd-, works
	// exactly as it would have, which is how nearly everyone reaches it.
	file.AddText("Settings…", keys.CmdOrCtrl(","), func(*menu.CallbackData) {
		app.OpenSettingsFile()
	})
	file.AddSeparator()
	file.AddText("New Tab", keys.CmdOrCtrl("t"), emit(app, "new-tab"))
	file.AddText("Close Tab", keys.CmdOrCtrl("w"), emit(app, "close-tab"))
	file.AddText("Close Pane", keys.Combo("x", keys.CmdOrCtrlKey, keys.ShiftKey), emit(app, "close-pane"))
	file.AddSeparator()
	file.AddText("Split Vertically", keys.CmdOrCtrl("d"), emit(app, "split-vertical"))
	file.AddText("Split Horizontally", keys.Combo("d", keys.CmdOrCtrlKey, keys.ShiftKey), emit(app, "split-horizontal"))
	file.AddSeparator()
	file.AddText("Find…", keys.CmdOrCtrl("f"), emit(app, "find"))
	file.AddText("Clear Terminal", keys.CmdOrCtrl("l"), emit(app, "clear"))
	file.AddSeparator()
	// The frontend answers these by calling RevealPath with the active pane's
	// working directory and by toggling StartTranscript/StopTranscript — it is
	// the only side that knows which pane is active.
	file.AddText("Reveal Folder in Finder", keys.Combo("o", keys.CmdOrCtrlKey, keys.ShiftKey), emit(app, "reveal-folder"))
	// No accelerator, and no "Stop": it is one item whose title the frontend
	// cannot change from here, so it reads as the thing it starts. Recording is
	// a deliberate act, not something anyone wants a keystroke away from the
	// keys that close a pane.
	file.AddText("Record Transcript", nil, emit(app, "toggle-transcript"))
	file.AddSeparator()
	// Both open a prompt rather than acting immediately — one for a name, one
	// for a list — so neither takes an accelerator, for the same reason Import
	// Color Scheme… does not.
	file.AddText("Save Workspace…", nil, emit(app, "save-workspace"))
	file.AddText("Open Workspace…", nil, emit(app, "open-workspace"))
	file.AddSeparator()
	// No accelerator: it opens a file dialog, and it is not a thing anyone does
	// twice a day. The frontend answers this action by calling
	// ImportColorScheme, which is also what its palette command calls — one
	// import path, whichever way the user got here.
	file.AddText("Import Color Scheme…", nil, emit(app, "import-scheme"))

	// The Edit role is what makes Cmd-C/V/X/A reach the webview at all: those
	// items are wired to the first responder's cut:/copy:/paste: selectors, and
	// without them the webview never receives the commands. Not to be replaced
	// with hand-built items.
	appMenu.Append(menu.EditMenu())

	view := appMenu.AddSubmenu("View")
	view.AddText("Zoom In", keys.CmdOrCtrl("="), emit(app, "zoom-in"))
	view.AddText("Zoom Out", keys.CmdOrCtrl("-"), emit(app, "zoom-out"))
	view.AddText("Actual Size", keys.CmdOrCtrl("0"), emit(app, "zoom-reset"))
	view.AddSeparator()
	view.AddText("Next Tab", keys.Combo("]", keys.CmdOrCtrlKey, keys.ShiftKey), emit(app, "next-tab"))
	view.AddText("Previous Tab", keys.Combo("[", keys.CmdOrCtrlKey, keys.ShiftKey), emit(app, "prev-tab"))
	view.AddSeparator()
	// "return" is one of the named keys WailsMenu.m's accel: translates — it
	// and "enter" both become U+000D, which is what AppKit wants for ⇧⌘↩. The
	// rest of this menu passes single characters straight through; these named
	// keys are the exception, and the list of them is in keys/parser.go.
	view.AddText("Zoom Pane", keys.Combo("return", keys.CmdOrCtrlKey, keys.ShiftKey), emit(app, "zoom-pane"))
	// The counterpart to the Dock badge and the notifications: it walks to the
	// next pane whose command finished or rang while the user was elsewhere.
	view.AddText("Next Pane Needing Attention", keys.Combo("a", keys.CmdOrCtrlKey, keys.ShiftKey), emit(app, "next-attention"))
	view.AddSeparator()
	view.AddText("Command Palette", keys.CmdOrCtrl("p"), emit(app, "palette"))
	view.AddText("Session Status", keys.CmdOrCtrl("i"), emit(app, "status"))
	view.AddText("Search History", keys.Combo("r", keys.CmdOrCtrlKey, keys.ShiftKey), emit(app, "history"))
	view.AddSeparator()
	// Handled here rather than sent to the frontend: the webview has no way to
	// full-screen the window it lives in. Note that the Window role below also
	// installs a "Full Screen" item on ^⌘F; this one comes first in the menu
	// bar, and AppKit matches a key equivalent in menu order, so this is the one
	// that runs. It is also the one that works — see App.toggleFullScreen.
	view.AddText("Toggle Full Screen", keys.Combo("f", keys.ControlKey, keys.CmdOrCtrlKey), func(*menu.CallbackData) {
		app.toggleFullScreen()
	})

	// Minimize/Zoom/Full Screen, built natively.
	appMenu.Append(menu.WindowMenu())

	help := appMenu.AddSubmenu("Help")
	help.AddText("Reveal Logs", nil, func(*menu.CallbackData) { app.RevealLogs() })
	help.AddText("Open Settings File", nil, func(*menu.CallbackData) { app.OpenSettingsFile() })
	help.AddText("Report an Issue", nil, func(*menu.CallbackData) { app.openIssues() })

	return appMenu
}

// emit builds a menu callback that forwards one action name to the frontend.
func emit(app *App, action string) menu.Callback {
	return func(*menu.CallbackData) { app.emitMenuAction(action) }
}
