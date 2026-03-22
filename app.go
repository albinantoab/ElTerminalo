package main

import (
	"context"

	"github.com/albinanto/elterminalo/internal/ptymanager"
)

// App is the main Wails-bound application struct.
type App struct {
	ctx    context.Context
	ptyMgr *ptymanager.Manager
	shell  string
}

// NewApp creates a new App instance.
func NewApp(shell string) *App {
	return &App{
		shell:  shell,
		ptyMgr: ptymanager.NewManager(shell),
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.ptyMgr.SetContext(ctx)
}

func (a *App) shutdown(ctx context.Context) {
	a.ptyMgr.CloseAll()
}

// CreateSession creates a new PTY session and returns its ID.
func (a *App) CreateSession(cols, rows int, cwd string) (string, error) {
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

// GetThemes returns the available themes.
func (a *App) GetThemes() []ThemeDTO {
	return AllThemes()
}

// ThemeDTO is a theme sent to the frontend.
type ThemeDTO struct {
	Name         string `json:"name"`
	Background   string `json:"background"`
	Foreground   string `json:"foreground"`
	Accent       string `json:"accent"`
	AccentDim    string `json:"accentDim"`
	Border       string `json:"border"`
	BorderActive string `json:"borderActive"`
	StatusBg     string `json:"statusBg"`
	StatusFg     string `json:"statusFg"`
	CursorColor  string `json:"cursorColor"`
	SelectionBg  string `json:"selectionBg"`
	Black        string `json:"black"`
	Red          string `json:"red"`
	Green        string `json:"green"`
	Yellow       string `json:"yellow"`
	Blue         string `json:"blue"`
	Magenta      string `json:"magenta"`
	Cyan         string `json:"cyan"`
	White        string `json:"white"`
	BrightBlack  string `json:"brightBlack"`
	BrightRed    string `json:"brightRed"`
	BrightGreen  string `json:"brightGreen"`
	BrightYellow string `json:"brightYellow"`
	BrightBlue   string `json:"brightBlue"`
	BrightMagenta string `json:"brightMagenta"`
	BrightCyan   string `json:"brightCyan"`
	BrightWhite  string `json:"brightWhite"`
}

func AllThemes() []ThemeDTO {
	return []ThemeDTO{
		{
			Name: "Noctis", Background: "#1b1d2b", Foreground: "#d6deeb",
			Accent: "#c792ea", AccentDim: "#7e57c2", Border: "#2e3250",
			BorderActive: "#c792ea", StatusBg: "#0f111a", StatusFg: "#a599e9",
			CursorColor: "#c792ea", SelectionBg: "#2e3250",
			Black: "#1b1d2b", Red: "#ff5370", Green: "#c3e88d", Yellow: "#ffcb6b",
			Blue: "#82aaff", Magenta: "#c792ea", Cyan: "#89ddff", White: "#d6deeb",
			BrightBlack: "#7e57c2", BrightRed: "#ff5370", BrightGreen: "#c3e88d",
			BrightYellow: "#ffcb6b", BrightBlue: "#82aaff", BrightMagenta: "#c792ea",
			BrightCyan: "#89ddff", BrightWhite: "#ffffff",
		},
		{
			Name: "Ember", Background: "#1a1a2e", Foreground: "#eaeaea",
			Accent: "#e94560", AccentDim: "#a8324a", Border: "#2a2a4a",
			BorderActive: "#e94560", StatusBg: "#0f0f23", StatusFg: "#e94560",
			CursorColor: "#e94560", SelectionBg: "#2a2a4a",
			Black: "#1a1a2e", Red: "#e94560", Green: "#7ec699", Yellow: "#f5a623",
			Blue: "#6fc1ff", Magenta: "#e94560", Cyan: "#89ddff", White: "#eaeaea",
			BrightBlack: "#a8324a", BrightRed: "#e94560", BrightGreen: "#7ec699",
			BrightYellow: "#f5a623", BrightBlue: "#6fc1ff", BrightMagenta: "#e94560",
			BrightCyan: "#89ddff", BrightWhite: "#ffffff",
		},
		{
			Name: "Aurora", Background: "#0d1117", Foreground: "#c9d1d9",
			Accent: "#58a6ff", AccentDim: "#388bfd", Border: "#21262d",
			BorderActive: "#58a6ff", StatusBg: "#010409", StatusFg: "#58a6ff",
			CursorColor: "#58a6ff", SelectionBg: "#163356",
			Black: "#0d1117", Red: "#f85149", Green: "#56d364", Yellow: "#e3b341",
			Blue: "#58a6ff", Magenta: "#bc8cff", Cyan: "#39d2c0", White: "#c9d1d9",
			BrightBlack: "#484f58", BrightRed: "#f85149", BrightGreen: "#56d364",
			BrightYellow: "#e3b341", BrightBlue: "#79c0ff", BrightMagenta: "#d2a8ff",
			BrightCyan: "#56d4dd", BrightWhite: "#f0f6fc",
		},
	}
}
