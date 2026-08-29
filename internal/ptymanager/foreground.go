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
	// A closed session's pid is reaped and can be reused, so stop asking about
	// it rather than reporting a stranger's command as this pane's.
	if s.closed.Load() {
		return ""
	}

	pgid := s.foregroundPgid()
	if pgid == 0 {
		return ""
	}

	name, err := processName(pgid)
	if err != nil {
		return ""
	}
	return name
}

// foregroundPgid returns the process group the tty currently has in the
// foreground, or 0 when that group is the shell itself (an idle pane), when
// there is no shell, or when the tty cannot answer.
//
// This — not the shell's pgid — is the group a command the user started belongs
// to. A login shell is interactive and has job control on, so it puts every
// foreground job in a process group of its own; the shell's own group does not
// contain it. Close needs that distinction to reach the job at all.
//
// It asks the tty, so it only works while the master is open: Close must call it
// before closing the master, not after.
func (s *Session) foregroundPgid() int {
	if s.cmd.Process == nil {
		return 0
	}
	pgid, err := unix.IoctlGetInt(int(s.ptmx.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return 0
	}
	// 0 and 1 are never a job of ours, and signalling either group would mean
	// "every process this user owns" or launchd. Report them as "no job".
	if pgid <= 1 || pgid == s.cmd.Process.Pid {
		return 0
	}
	return pgid
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
