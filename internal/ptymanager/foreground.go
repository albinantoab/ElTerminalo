//go:build darwin || linux

package ptymanager

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// ForegroundProcess returns the name of the foreground process in this PTY.
// Returns empty string if the shell itself is in the foreground (idle).
func (s *Session) ForegroundProcess() string {
	if s.cmd.Process == nil {
		return ""
	}

	fd := int(s.ptmx.Fd())
	pgid, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil {
		return ""
	}

	// If the foreground PGID matches the shell PID, the shell is idle
	if pgid == s.cmd.Process.Pid {
		return ""
	}

	name, err := processName(pgid)
	if err != nil {
		return ""
	}
	return name
}

// processName returns the command name for a given PID.
func processName(pid int) (string, error) {
	switch runtime.GOOS {
	case "linux":
		return processNameFromProc(pid)
	default:
		return processNameFromPS(pid)
	}
}

// processNameFromProc reads /proc/<pid>/comm (Linux).
func processNameFromProc(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// processNameFromPS uses ps to get the command name (macOS).
func processNameFromPS(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "comm=", "-p", fmt.Sprintf("%d", pid)).Output()
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(out))
	// ps returns the full path on macOS — extract just the basename
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name, nil
}
