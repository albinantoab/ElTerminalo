//go:build linux

package ptymanager

import (
	"fmt"
	"os"
)

// cwdOfPid reads a process's working directory from /proc. The link is a
// magic symlink maintained by the kernel, so this is the same free answer
// proc_pidinfo gives on darwin.
func cwdOfPid(pid int) (string, error) {
	link := fmt.Sprintf("/proc/%d/cwd", pid)
	target, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("readlink %s failed: %w", link, err)
	}
	return target, nil
}
