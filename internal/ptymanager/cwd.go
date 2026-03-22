package ptymanager

import (
	"fmt"
	"os/exec"
	"strings"
)

// CWD returns the current working directory of the shell process.
func (s *Session) CWD() (string, error) {
	if s.cmd.Process == nil {
		return "", fmt.Errorf("process not running")
	}

	pid := s.cmd.Process.Pid
	out, err := exec.Command("lsof", "-a", "-d", "cwd", "-p", fmt.Sprintf("%d", pid), "-Fn").Output()
	if err != nil {
		return "", fmt.Errorf("lsof failed: %w", err)
	}

	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return line[1:], nil
		}
	}

	return "", fmt.Errorf("could not determine CWD for pid %d", pid)
}
