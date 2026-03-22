package main

import (
	"embed"
	"fmt"
	"os"

	"github.com/albinanto/elterminalo/internal/config"
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

	app := NewApp(shell, cfg)

	// Minimal menu with NO accelerators — all Cmd+ shortcuts go to the webview
	appMenu := menu.NewMenu()
	appMenu.Append(menu.AppMenu()) // keeps About, Hide, Quit

	err = wails.Run(&options.App{
		Title:     "El Terminalo",
		Width:     1024,
		Height:    768,
		MinWidth:  400,
		MinHeight: 300,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind: []interface{}{
			app,
		},
		Mac: &mac.Options{
			TitleBar: &mac.TitleBar{
				TitlebarAppearsTransparent: true,
				HideTitle:                 true,
				HideTitleBar:              false,
				FullSizeContent:           true,
			},
			WebviewIsTransparent: true,
			WindowIsTranslucent:  false,
			About: &mac.AboutInfo{
				Title:   "El Terminalo",
				Message: "A modern terminal for agent coding",
			},
		},
		Menu:                     appMenu,
		Frameless:                false,
		EnableDefaultContextMenu: true,
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
