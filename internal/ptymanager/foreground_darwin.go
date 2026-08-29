//go:build darwin

package ptymanager

/*
#include <libproc.h>
*/
import "C"

import (
	"fmt"
	"path/filepath"
	"unsafe"
)

// processName returns the command name for a pid.
//
// proc_pidpath gives the executable's full path, and its basename is exactly
// what the forked `ps -o comm=` used to print — the pane's status modal and the
// quit dialog both compare against those names, so the strings have to stay the
// same while the fork goes away.
//
// proc_name() would be the more direct call and is deliberately not used: it
// returns the kernel's p_comm, which is truncated to 16 characters, so a
// long-named binary would show up cut in half in the status modal.
func processName(pid int) (string, error) {
	buf := make([]byte, C.PROC_PIDPATHINFO_MAXSIZE)

	n, err := C.proc_pidpath(C.int(pid), unsafe.Pointer(&buf[0]), C.uint32_t(len(buf)))
	if n <= 0 {
		if err != nil {
			return "", fmt.Errorf("proc_pidpath(%d): %w", pid, err)
		}
		// The usual reason for a bare zero: the process exited between the
		// TIOCGPGRP that named it and this call.
		return "", fmt.Errorf("proc_pidpath(%d) returned no path", pid)
	}

	return filepath.Base(string(buf[:n])), nil
}
