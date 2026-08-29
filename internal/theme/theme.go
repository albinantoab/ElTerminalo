package theme

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/albinanto/elterminalo/internal/config"
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

// FileName is the user themes file's name inside the config directory.
const FileName = "themes.json"

// filePerm keeps themes.json owner-only, matching every other file this app
// publishes into the config directory. It is not secret, but there is no reason
// for it to be world-readable either, and the mode is applied literally by
// config.WriteFileDurable rather than filtered by the umask.
const filePerm = 0o600

// Path returns the location of the user themes file inside configDir.
func Path(configDir string) string {
	return filepath.Join(configDir, FileName)
}

// LoadUserThemes reads custom themes from the config directory. A missing file
// is the ordinary case and is not an error; anything else — unreadable, or
// present and unparseable — is, and the error names the file so a caller that
// surfaces it says which one to fix.
func LoadUserThemes(configDir string) ([]Theme, error) {
	path := Path(configDir)
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
		return nil, fmt.Errorf("%s does not parse: %w", path, err)
	}
	return file.Themes, nil
}

// SaveUserThemes writes custom themes to the config directory.
//
// Durable, owner-only, and through config.WriteFileDurable rather than a
// hand-rolled write: the previous version wrote a fixed <path>.tmp at 0644 with
// no fsync, which meant two concurrent saves shared one scratch inode and
// published a splice of both, a power loss could commit the rename over
// unwritten data, and the scratch file was invisible to the config directory's
// temp sweep because it did not carry the name that sweep globs for.
func SaveUserThemes(configDir string, themes []Theme) error {
	data, err := json.MarshalIndent(struct {
		Themes []Theme `json:"themes"`
	}{Themes: themes}, "", "  ")
	if err != nil {
		return err
	}
	// Trailing newline: this is a file people edit by hand.
	data = append(data, '\n')
	return config.WriteFileDurable(Path(configDir), data, filePerm)
}

// Upsert saves t into the user's themes.json, replacing any theme of the same
// name and appending it otherwise. Names are compared case-insensitively,
// because Merged resolves a user theme against a built-in the same way: two
// entries that differ only in case would shadow each other unpredictably.
//
// A themes.json that exists but cannot be read or parsed is a hard error, and
// nothing is written. This used to be treated as "there are no themes", which
// meant one stray comma cost the user every custom theme they had the next time
// anything saved one — the file was silently replaced by a document containing
// only the new theme. Failing here instead is the same policy the settings
// package follows: whatever is on disk is the user's text, an import that
// cannot be completed says so, and the file is still there to repair.
func Upsert(configDir string, t Theme) error {
	existing, err := LoadUserThemes(configDir)
	if err != nil {
		return err
	}

	for i, e := range existing {
		if strings.EqualFold(e.Name, t.Name) {
			existing[i] = t
			return SaveUserThemes(configDir, existing)
		}
	}
	return SaveUserThemes(configDir, append(existing, t))
}

// Merged returns built-in themes with user themes appended.
// If a user theme has the same name as a built-in, the user theme replaces it.
//
// A themes.json that cannot be read costs the user every custom theme in the
// picker, which looks exactly like the app having forgotten them, so it is
// logged. Read-only either way: nothing here writes, and Upsert refuses to
// write over a file in that state.
func Merged(configDir string) []Theme {
	builtIns := All()
	userThemes, err := LoadUserThemes(configDir)
	if err != nil {
		log.Printf("theme: %v; only the built-in themes are available until it is fixed", err)
		return builtIns
	}
	if len(userThemes) == 0 {
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
