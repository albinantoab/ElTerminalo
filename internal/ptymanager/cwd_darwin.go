//go:build darwin

package ptymanager

/*
#include <libproc.h>
#include <sys/proc_info.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// cwdOfPid reads a process's current working directory straight from the kernel.
//
// PROC_PIDVNODEPATHINFO fills a proc_vnodepathinfo, whose pvi_cdir describes the
// vnode the process has as its cwd — path included, already absolute and already
// resolved, which is what lsof was being forked for.
//
// proc_pidinfo returns the number of bytes it wrote, not a 0/-1 status. Anything
// short of the full struct means the kernel filled in something other than what
// was asked for, so it is treated as a failure rather than as a truncated path:
// a half-written vip_path would be reported as a working directory and then
// saved into the layout.
//
// It needs the same privilege lsof did — the shell is our own child and runs as
// the same user, so the call is permitted.
func cwdOfPid(pid int) (string, error) {
	var info C.struct_proc_vnodepathinfo
	size := C.int(unsafe.Sizeof(info))
	// Hoisted out of the call rather than written inline. For an
	// `unsafe.Pointer(&x)` at the call site cgo generates its pointer check as
	// `_cgoCheckPointer(p, 0 == 0)`, and the linter reports that tautology
	// against this line. Through a variable it generates `_cgoCheckPointer(p,
	// nil)` — the same check, minus the false positive.
	buf := unsafe.Pointer(&info)

	n, err := C.proc_pidinfo(C.int(pid), C.PROC_PIDVNODEPATHINFO, 0, buf, size)
	if n != size {
		if err != nil {
			return "", fmt.Errorf("proc_pidinfo(%d, PROC_PIDVNODEPATHINFO): %w", pid, err)
		}
		return "", fmt.Errorf("proc_pidinfo(%d, PROC_PIDVNODEPATHINFO) wrote %d of %d bytes", pid, n, size)
	}

	return C.GoString(&info.pvi_cdir.vip_path[0]), nil
}
