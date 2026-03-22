//go:build darwin || linux

package ptymanager

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// CWD returns the current working directory of the shell process.
// On macOS it uses lsof; on Linux it reads /proc/PID/cwd.
func (s *Session) CWD() (string, error) {
	if s.cmd.Process == nil {
		return "", fmt.Errorf("process not running")
	}

	pid := s.cmd.Process.Pid

	switch runtime.GOOS {
	case "linux":
		return cwdFromProc(pid)
	default:
		// darwin and other unix-like systems
		return cwdFromLsof(pid)
	}
}

// cwdFromProc reads the CWD via the /proc filesystem (Linux).
func cwdFromProc(pid int) (string, error) {
	link := fmt.Sprintf("/proc/%d/cwd", pid)
	target, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("readlink %s failed: %w", link, err)
	}
	return target, nil
}

// cwdFromLsof determines the CWD via lsof (macOS/darwin).
func cwdFromLsof(pid int) (string, error) {
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
