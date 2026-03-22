package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func configDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "elterminalo")
	os.MkdirAll(dir, 0755)
	return dir
}

func stateFilePath() string {
	return filepath.Join(configDir(), "state.json")
}

// SaveAppState writes the serialized state JSON to disk atomically.
func (a *App) SaveAppState(stateJSON string) error {
	path := stateFilePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(stateJSON), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadAppState reads the saved state JSON from disk.
func (a *App) LoadAppState() string {
	data, err := os.ReadFile(stateFilePath())
	if err != nil {
		return ""
	}
	// Validate it's valid JSON
	var check json.RawMessage
	if json.Unmarshal(data, &check) != nil {
		return ""
	}
	return string(data)
}
