package theme

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Theme represents a terminal color theme sent to the frontend.
type Theme struct {
	Name          string `json:"name"`
	Background    string `json:"background"`
	Foreground    string `json:"foreground"`
	Accent        string `json:"accent"`
	AccentDim     string `json:"accentDim"`
	Border        string `json:"border"`
	BorderActive  string `json:"borderActive"`
	StatusBg      string `json:"statusBg"`
	StatusFg      string `json:"statusFg"`
	CursorColor   string `json:"cursorColor"`
	SelectionBg   string `json:"selectionBg"`
	Black         string `json:"black"`
	Red           string `json:"red"`
	Green         string `json:"green"`
	Yellow        string `json:"yellow"`
	Blue          string `json:"blue"`
	Magenta       string `json:"magenta"`
	Cyan          string `json:"cyan"`
	White         string `json:"white"`
	BrightBlack   string `json:"brightBlack"`
	BrightRed     string `json:"brightRed"`
	BrightGreen   string `json:"brightGreen"`
	BrightYellow  string `json:"brightYellow"`
	BrightBlue    string `json:"brightBlue"`
	BrightMagenta string `json:"brightMagenta"`
	BrightCyan    string `json:"brightCyan"`
	BrightWhite   string `json:"brightWhite"`
}

// All returns the built-in terminal color themes.
func All() []Theme {
	return []Theme{
		{
			Name: "Terminalo", Background: "#0a0a12", Foreground: "#e0e0e8",
			Accent: "#5e17eb", AccentDim: "#4311b0", Border: "#1a1a2e",
			BorderActive: "#5e17eb", StatusBg: "#06060c", StatusFg: "#5e17eb",
			CursorColor: "#5e17eb", SelectionBg: "#2a1a4e",
			Black: "#0a0a12", Red: "#ff5572", Green: "#7dd6a0", Yellow: "#f0c674",
			Blue: "#7aa2f7", Magenta: "#bb9af7", Cyan: "#7dcfff", White: "#e0e0e8",
			BrightBlack: "#4311b0", BrightRed: "#ff7a93", BrightGreen: "#a8e6b0",
			BrightYellow: "#f5d8a0", BrightBlue: "#9ab8f7", BrightMagenta: "#d0b8ff",
			BrightCyan: "#a0dcff", BrightWhite: "#ffffff",
		},
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

// LoadUserThemes reads custom themes from the config directory.
func LoadUserThemes(configDir string) ([]Theme, error) {
	path := filepath.Join(configDir, "themes.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var file struct {
		Themes []Theme `json:"themes"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("invalid themes.json: %w", err)
	}
	return file.Themes, nil
}

// SaveUserThemes writes custom themes to the config directory.
func SaveUserThemes(configDir string, themes []Theme) error {
	path := filepath.Join(configDir, "themes.json")
	data, err := json.MarshalIndent(struct {
		Themes []Theme `json:"themes"`
	}{Themes: themes}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// Merged returns built-in themes with user themes appended.
// If a user theme has the same name as a built-in, the user theme replaces it.
func Merged(configDir string) []Theme {
	builtIns := All()
	userThemes, err := LoadUserThemes(configDir)
	if err != nil || len(userThemes) == 0 {
		return builtIns
	}

	// Build result: start with built-ins, replace by name if user overrides
	result := make([]Theme, len(builtIns))
	copy(result, builtIns)

	for _, ut := range userThemes {
		found := false
		for i, bt := range result {
			if strings.EqualFold(bt.Name, ut.Name) {
				result[i] = ut
				found = true
				break
			}
		}
		if !found {
			result = append(result, ut)
		}
	}
	return result
}
