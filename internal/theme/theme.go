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
			Name: "Terminalo", Background: "#0d1117", Foreground: "#c9d1d9",
			Accent: "#5e17eb", AccentDim: "#4311b0", Border: "#21262d",
			BorderActive: "#5e17eb", StatusBg: "#010409", StatusFg: "#5e17eb",
			CursorColor: "#5e17eb", SelectionBg: "#163356",
			Black: "#0d1117", Red: "#f85149", Green: "#56d364", Yellow: "#e3b341",
			Blue: "#58a6ff", Magenta: "#bc8cff", Cyan: "#39d2c0", White: "#c9d1d9",
			BrightBlack: "#484f58", BrightRed: "#f85149", BrightGreen: "#56d364",
			BrightYellow: "#e3b341", BrightBlue: "#79c0ff", BrightMagenta: "#d2a8ff",
			BrightCyan: "#56d4dd", BrightWhite: "#f0f6fc",
		},
		{
			Name: "Noctis", Background: "#0d1117", Foreground: "#c9d1d9",
			Accent: "#c792ea", AccentDim: "#7e57c2", Border: "#21262d",
			BorderActive: "#c792ea", StatusBg: "#010409", StatusFg: "#c792ea",
			CursorColor: "#c792ea", SelectionBg: "#163356",
			Black: "#0d1117", Red: "#f85149", Green: "#56d364", Yellow: "#e3b341",
			Blue: "#58a6ff", Magenta: "#bc8cff", Cyan: "#39d2c0", White: "#c9d1d9",
			BrightBlack: "#484f58", BrightRed: "#f85149", BrightGreen: "#56d364",
			BrightYellow: "#e3b341", BrightBlue: "#79c0ff", BrightMagenta: "#d2a8ff",
			BrightCyan: "#56d4dd", BrightWhite: "#f0f6fc",
		},
		{
			Name: "Ember", Background: "#0d1117", Foreground: "#c9d1d9",
			Accent: "#e94560", AccentDim: "#a8324a", Border: "#21262d",
			BorderActive: "#e94560", StatusBg: "#010409", StatusFg: "#e94560",
			CursorColor: "#e94560", SelectionBg: "#163356",
			Black: "#0d1117", Red: "#f85149", Green: "#56d364", Yellow: "#e3b341",
			Blue: "#58a6ff", Magenta: "#bc8cff", Cyan: "#39d2c0", White: "#c9d1d9",
			BrightBlack: "#484f58", BrightRed: "#f85149", BrightGreen: "#56d364",
			BrightYellow: "#e3b341", BrightBlue: "#79c0ff", BrightMagenta: "#d2a8ff",
			BrightCyan: "#56d4dd", BrightWhite: "#f0f6fc",
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
