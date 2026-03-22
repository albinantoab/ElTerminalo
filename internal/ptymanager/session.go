package ptymanager

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/creack/pty"
	"github.com/google/uuid"
)

// Session manages a single PTY shell session.
type Session struct {
	ID     string
	cmd    *exec.Cmd
	ptmx   *os.File
	closed bool
}

// NewSession spawns a shell in a new PTY. If cwd is empty, defaults to home.
func NewSession(shell string, cols, rows int, cwd string) (*Session, error) {
	if cols < 1 {
		cols = 80
	}
	if rows < 1 {
		rows = 24
	}

	dir := cwd
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	// Verify directory exists, fallback to home
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		dir, _ = os.UserHomeDir()
	}

	cmd := exec.Command(shell)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"TERM_PROGRAM=ElTerminalo",
		"PROMPT_EOL_MARK=",
		fmt.Sprintf("COLUMNS=%d", cols),
		fmt.Sprintf("LINES=%d", rows),
	)

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

// Close terminates the PTY session.
func (s *Session) Close() {
	if s.closed {
		return
	}
	s.closed = true
	s.ptmx.Close()
	if s.cmd.Process != nil {
		s.cmd.Process.Kill()
		s.cmd.Process.Wait()
	}
}
