package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// CustomCommand is a user-defined command shown in the palette.
type CustomCommand struct {
	Name        string `json:"name"`
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
	Shortcut    string `json:"shortcut,omitempty"`
	Scope       string `json:"scope"`
}

type commandsFile struct {
	Commands []CustomCommand `json:"commands"`
}

// GetGlobalCommands reads commands from ~/.config/elterminalo/commands.json.
func (a *App) GetGlobalCommands() []CustomCommand {
	path := filepath.Join(configDir(), "commands.json")
	return readCommandsFile(path, "global")
}

// GetLocalCommands walks up from cwd looking for .elterminalo/commands.json.
func (a *App) GetLocalCommands(cwd string) []CustomCommand {
	if cwd == "" {
		return nil
	}

	dir := cwd
	for depth := 0; depth < 20; depth++ {
		path := filepath.Join(dir, ".elterminalo", "commands.json")
		if _, err := os.Stat(path); err == nil {
			return readCommandsFile(path, "local")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil
}

// SaveCommand adds a command to the global or local commands file.
func (a *App) SaveCommand(scope, name, command, description, shortcut, cwd string) error {
	var path string
	if scope == "global" {
		path = filepath.Join(configDir(), "commands.json")
	} else {
		// Local: save in cwd/.elterminalo/commands.json
		dir := filepath.Join(cwd, ".elterminalo")
		os.MkdirAll(dir, 0755)
		path = filepath.Join(dir, "commands.json")
	}

	// Read existing
	existing := readCommandsFile(path, scope)

	// Append new command
	newCmd := CustomCommand{
		Name:        name,
		Command:     command,
		Description: description,
		Shortcut:    shortcut,
		Scope:       scope,
	}
	existing = append(existing, newCmd)

	// Write back (strip scope from JSON output)
	type cmdOut struct {
		Name        string `json:"name"`
		Command     string `json:"command"`
		Description string `json:"description,omitempty"`
	}
	out := make([]cmdOut, len(existing))
	for i, c := range existing {
		out[i] = cmdOut{Name: c.Name, Command: c.Command, Description: c.Description}
	}

	data, err := json.MarshalIndent(struct {
		Commands []cmdOut `json:"commands"`
	}{Commands: out}, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// DeleteCommand removes a command by name from the given scope's file.
func (a *App) DeleteCommand(scope, name, cwd string) error {
	path := commandsFilePath(scope, cwd)
	if path == "" {
		return nil
	}

	cmds := readCommandsFile(path, scope)
	filtered := make([]CustomCommand, 0, len(cmds))
	for _, c := range cmds {
		if c.Name != name {
			filtered = append(filtered, c)
		}
	}
	return writeCommandsFile(path, filtered)
}

// UpdateCommand replaces a command by oldName with new values.
func (a *App) UpdateCommand(scope, oldName, newName, newCommand, newDescription, newShortcut, cwd string) error {
	path := commandsFilePath(scope, cwd)
	if path == "" {
		return nil
	}

	cmds := readCommandsFile(path, scope)
	for i, c := range cmds {
		if c.Name == oldName {
			cmds[i].Name = newName
			cmds[i].Command = newCommand
			cmds[i].Description = newDescription
			cmds[i].Shortcut = newShortcut
			break
		}
	}
	return writeCommandsFile(path, cmds)
}

func commandsFilePath(scope, cwd string) string {
	if scope == "global" {
		return filepath.Join(configDir(), "commands.json")
	}
	// Find existing .elterminalo/commands.json walking up
	dir := cwd
	for depth := 0; depth < 20; depth++ {
		path := filepath.Join(dir, ".elterminalo", "commands.json")
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Default to cwd
	return filepath.Join(cwd, ".elterminalo", "commands.json")
}

func writeCommandsFile(path string, cmds []CustomCommand) error {
	type cmdOut struct {
		Name        string `json:"name"`
		Command     string `json:"command"`
		Description string `json:"description,omitempty"`
		Shortcut    string `json:"shortcut,omitempty"`
	}
	out := make([]cmdOut, len(cmds))
	for i, c := range cmds {
		out[i] = cmdOut{Name: c.Name, Command: c.Command, Description: c.Description, Shortcut: c.Shortcut}
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

func readCommandsFile(path, scope string) []CustomCommand {
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
