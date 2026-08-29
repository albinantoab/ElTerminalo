//go:build darwin || linux

package ptymanager

import (
	"os"

	"golang.org/x/sys/unix"
)

// ForegroundProcess returns the name of the foreground process in this PTY.
// Returns empty string if the shell itself is in the foreground (idle).
//
// Both halves are syscalls: TIOCGPGRP for the group, then processName for the
// name. The name used to come from a forked `ps -o comm=`, which cost ~2 ms and
// was paid once per session by GetAllSessionStatuses and once more by
// RunningCommandCount on every Cmd-Q.
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
//
// The ioctl goes through withPtmx because it needs the raw fd number, and a
// number Close has already given up is not an fd that fails — it is an fd the
// kernel has handed to somebody else, whose foreground group would then be
// reported as this pane's and signalled by this pane's ladder. Holding the read
// side for the length of two syscalls is what keeps the number ours for the
// length of the question; the answer once the master is retired is "no job".
func (s *Session) foregroundPgid() int {
	if s.cmd.Process == nil {
		return 0
	}
	var pgid int
	err := s.withPtmx(func(ptmx *os.File) error {
		var ioctlErr error
		pgid, ioctlErr = unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPGRP)
		return ioctlErr
	})
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
