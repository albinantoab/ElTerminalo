package main

import (
	"embed"
	"fmt"
	"log"
	"os"

	"github.com/albinanto/elterminalo/internal/config"
	"github.com/albinanto/elterminalo/internal/logging"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed frontend/dist
var assets embed.FS

func main() {
	shell := "/bin/zsh"
	if s := os.Getenv("SHELL"); s != "" {
		shell = s
	}

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
	log.Printf("=== El Terminalo %s starting: pid=%d configDir=%s shell=%s log=%s",
		Version, os.Getpid(), cfg.Dir(), shell, logPath)
	if logErr != nil {
		log.Printf("logging: file unavailable (%v); this session's log is stderr only", logErr)
	}

	app := NewApp(shell, cfg)

	// Minimal menu — Edit menu is required for Cmd+C/V/X/A to reach the webview
	appMenu := menu.NewMenu()
	appMenu.Append(menu.AppMenu())  // keeps About, Hide, Quit
	appMenu.Append(menu.EditMenu()) // enables Cut, Copy, Paste, Select All

	err = wails.Run(&options.App{
		Title:     "El Terminalo",
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
				Title:   "El Terminalo",
				Message: "A modern terminal for agent coding",
			},
		},
		Menu:                     appMenu,
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
