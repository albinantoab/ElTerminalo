package shellintegration

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:shell
var shellFiles embed.FS

// Install writes the bundled shell integration scripts to the config directory.
// Overwrites existing files to keep them in sync with the app version.
func Install(configDir string) error {
	return fs.WalkDir(shellFiles, "shell", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Strip the "shell/" prefix to get the relative path
		rel, _ := filepath.Rel("shell", path)
		dest := filepath.Join(configDir, "shell", rel)

		if d.IsDir() {
			return os.MkdirAll(dest, 0755)
		}

		data, err := shellFiles.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0644)
	})
}
