package commands

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const maxTraversalDepth = 20

var (
	// ErrInvalidScope is returned when a scope value is not "global" or "local".
	ErrInvalidScope = errors.New("scope must be \"global\" or \"local\"")
)

// Command is a user-defined command shown in the palette.
type Command struct {
	Name        string `json:"name"`
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
	Shortcut    string `json:"shortcut,omitempty"`
	Scope       string `json:"scope"`
}

// commandsFile is the on-disk JSON structure.
type commandsFile struct {
	Commands []Command `json:"commands"`
}

// cmdOut is used for serializing commands to disk (scope is not persisted).
type cmdOut struct {
	Name        string `json:"name"`
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
	Shortcut    string `json:"shortcut,omitempty"`
}

// Store manages reading and writing custom commands.
type Store struct {
	configDir string
}

// NewStore creates a command store backed by the given config directory.
func NewStore(configDir string) *Store {
	return &Store{configDir: configDir}
}

// GetGlobal reads commands from the global commands file.
func (s *Store) GetGlobal() []Command {
	path := filepath.Join(s.configDir, "commands.json")
	return s.readFile(path, "global")
}

// GetLocal walks up from cwd looking for .elterminalo/commands.json.
func (s *Store) GetLocal(cwd string) []Command {
	if cwd == "" {
		return nil
	}
	root := s.findLocalRoot(cwd)
	if root == "" {
		return nil
	}
	path := filepath.Join(root, ".elterminalo", "commands.json")
	return s.readFile(path, "local")
}

// Save adds a command to the global or local commands file.
func (s *Store) Save(scope, name, command, description, shortcut, cwd string) error {
	if scope != "global" && scope != "local" {
		return ErrInvalidScope
	}

	path := s.filePath(scope, cwd)

	// Ensure parent directory exists
	os.MkdirAll(filepath.Dir(path), 0755)

	// Read existing
	existing := s.readFile(path, scope)

	// Append new command
	newCmd := Command{
		Name:        name,
		Command:     command,
		Description: description,
		Shortcut:    shortcut,
		Scope:       scope,
	}
	existing = append(existing, newCmd)

	return s.writeFile(path, existing)
}

// Delete removes a command by name from the given scope's file.
func (s *Store) Delete(scope, name, cwd string) error {
	if scope != "global" && scope != "local" {
		return ErrInvalidScope
	}

	path := s.filePath(scope, cwd)

	cmds := s.readFile(path, scope)
	filtered := make([]Command, 0, len(cmds))
	for _, c := range cmds {
		if c.Name != name {
			filtered = append(filtered, c)
		}
	}
	return s.writeFile(path, filtered)
}

// Update replaces a command by oldName with new values.
func (s *Store) Update(scope, oldName, newName, newCommand, newDescription, newShortcut, cwd string) error {
	if scope != "global" && scope != "local" {
		return ErrInvalidScope
	}

	path := s.filePath(scope, cwd)

	cmds := s.readFile(path, scope)
	for i, c := range cmds {
		if c.Name == oldName {
			cmds[i].Name = newName
			cmds[i].Command = newCommand
			cmds[i].Description = newDescription
			cmds[i].Shortcut = newShortcut
			break
		}
	}
	return s.writeFile(path, cmds)
}

// filePath returns the commands file path for a given scope and cwd.
func (s *Store) filePath(scope, cwd string) string {
	if scope == "global" {
		return filepath.Join(s.configDir, "commands.json")
	}
	// For local scope, try to find an existing root first
	root := s.findLocalRoot(cwd)
	if root != "" {
		return filepath.Join(root, ".elterminalo", "commands.json")
	}
	// Default to cwd if no existing root found
	return filepath.Join(cwd, ".elterminalo", "commands.json")
}

// findLocalRoot walks up from cwd looking for a directory containing
// .elterminalo/commands.json. Returns the directory containing it,
// or empty string if not found.
func (s *Store) findLocalRoot(cwd string) string {
	dir := cwd
	for depth := 0; depth < maxTraversalDepth; depth++ {
		path := filepath.Join(dir, ".elterminalo", "commands.json")
		if _, err := os.Stat(path); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// readFile reads and parses a commands JSON file, tagging each command with scope.
func (s *Store) readFile(path, scope string) []Command {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var cf commandsFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil
	}

	for i := range cf.Commands {
		cf.Commands[i].Scope = scope
	}
	return cf.Commands
}

// writeFile serializes commands to JSON and writes them to disk.
func (s *Store) writeFile(path string, cmds []Command) error {
	out := make([]cmdOut, len(cmds))
	for i, c := range cmds {
		out[i] = cmdOut{
			Name:        c.Name,
			Command:     c.Command,
			Description: c.Description,
			Shortcut:    c.Shortcut,
		}
	}
	data, err := json.MarshalIndent(struct {
		Commands []cmdOut `json:"commands"`
	}{Commands: out}, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	return os.WriteFile(path, data, 0644)
}
