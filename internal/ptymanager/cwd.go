//go:build darwin || linux

package ptymanager

import "fmt"

// CWD returns the current working directory of the shell process.
//
// The per-OS half is cwdOfPid: a proc_pidinfo syscall on darwin, a readlink of
// /proc/PID/cwd on linux. Neither forks. This used to shell out to lsof, which
// cost ~12 ms per probe — and GetAllSessionStatuses asks once per session while
// the status modal polls it every two seconds, so a window of 17 panes spent a
// fifth of a second forking, twice a second, to answer a question two syscalls
// answer for free.
func (s *Session) CWD() (string, error) {
	// Once the session has been closed its shell is reaped and the kernel may
	// hand that pid to an unrelated process — whose cwd would then be reported
	// as the pane's and saved into the layout.
	if s.closed.Load() {
		return "", fmt.Errorf("session closed")
	}
	if s.cmd.Process == nil {
		return "", fmt.Errorf("process not running")
	}

	return cwdOfPid(s.cmd.Process.Pid)
}
