package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config manages the application's configuration directory and state persistence.
type Config struct {
	dir string
}

// New creates a Config, determining and creating the config directory once.
func New() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}
	dir := filepath.Join(home, ".config", "elterminalo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create config directory: %w", err)
	}
	return &Config{dir: dir}, nil
}

// Dir returns the configuration directory path.
func (c *Config) Dir() string {
	return c.dir
}

// SaveState writes the serialized state JSON to disk atomically.
func (c *Config) SaveState(stateJSON string) error {
	path := filepath.Join(c.dir, "state.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(stateJSON), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadState reads the saved state JSON from disk.
func (c *Config) LoadState() string {
	data, err := os.ReadFile(filepath.Join(c.dir, "state.json"))
	if err != nil {
		return ""
	}
	var check json.RawMessage
	if json.Unmarshal(data, &check) != nil {
		return ""
	}
	return string(data)
}

// WindowGeometry stores the window size and position.
type WindowGeometry struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	X      int `json:"x"`
	Y      int `json:"y"`
}

// SaveWindowGeometry persists window size and position to disk.
func (c *Config) SaveWindowGeometry(g WindowGeometry) error {
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}
	path := filepath.Join(c.dir, "window.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadWindowGeometry reads the saved window geometry from disk.
func (c *Config) LoadWindowGeometry() *WindowGeometry {
	data, err := os.ReadFile(filepath.Join(c.dir, "window.json"))
	if err != nil {
		return nil
	}
	var g WindowGeometry
	if json.Unmarshal(data, &g) != nil {
		return nil
	}
	if g.Width < 400 || g.Height < 300 {
		return nil
	}
	// Reject positions that are unreasonably far off-screen
	if g.X < -10000 || g.X > 20000 || g.Y < -10000 || g.Y > 20000 {
		return nil
	}
	return &g
}
