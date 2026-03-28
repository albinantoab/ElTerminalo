package ptymanager

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/creack/pty"
	"github.com/google/uuid"
)

const (
	defaultCols = 80
	defaultRows = 24
)

// Session manages a single PTY shell session.
type Session struct {
	ID        string
	cmd       *exec.Cmd
	ptmx      *os.File
	closeOnce sync.Once
}

// NewSession spawns a shell in a new PTY. If cwd is empty, defaults to home.
// configDir is used to locate shell integration scripts.
func NewSession(shell, configDir string, cols, rows int, cwd string) (*Session, error) {
	if cols < 1 {
		cols = defaultCols
	}
	if rows < 1 {
		rows = defaultRows
	}

	dir := cwd
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	// Verify directory exists, fallback to home
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		dir, _ = os.UserHomeDir()
	}

	cmd := exec.Command(shell, "-l")
	cmd.Dir = dir

	shellIntegrationDir := filepath.Join(configDir, "shell")
	env := append(os.Environ(),
		"TERM=xterm-256color",
		"TERM_PROGRAM=ElTerminalo",
		"PROMPT_EOL_MARK=",
		fmt.Sprintf("COLUMNS=%d", cols),
		fmt.Sprintf("LINES=%d", rows),
		"ELTERMINALO_SHELL_INTEGRATION_DIR="+shellIntegrationDir,
	)

	// Shell-specific integration injection
	shellName := filepath.Base(shell)
	switch {
	case strings.Contains(shellName, "zsh"):
		// ZDOTDIR trick: point zsh to our bootstrap .zshenv which sources
		// the user's real config and then loads shell integration.
		origZdotdir := os.Getenv("ZDOTDIR")
		env = append(env,
			"ZDOTDIR="+filepath.Join(shellIntegrationDir, "zdotdir"),
			"ELTERMINALO_ORIG_ZDOTDIR="+origZdotdir,
		)
	case strings.Contains(shellName, "bash"):
		// Bootstrap via PROMPT_COMMAND: sources integration on first prompt,
		// then restores any original PROMPT_COMMAND.
		script := filepath.Join(shellIntegrationDir, "elterminalo-integration-bash.sh")
		env = append(env, "PROMPT_COMMAND=source '"+script+"'")
	}

	cmd.Env = env

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start pty: %w", err)
	}

	return &Session{
		ID:   uuid.New().String(),
		cmd:  cmd,
		ptmx: ptmx,
	}, nil
}

// Read reads from the PTY. Blocks until data is available.
func (s *Session) Read(buf []byte) (int, error) {
	return s.ptmx.Read(buf)
}

// Write sends data to the PTY.
func (s *Session) Write(data []byte) (int, error) {
	return s.ptmx.Write(data)
}

// Resize changes the PTY dimensions.
func (s *Session) Resize(cols, rows int) error {
	return pty.Setsize(s.ptmx, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
}

// Close terminates the PTY session. Safe to call multiple times.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.ptmx.Close()
		if s.cmd.Process != nil {
			s.cmd.Process.Kill()
			s.cmd.Process.Wait()
		}
	})
}
