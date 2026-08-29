//go:build linux

package ptymanager

import (
	"fmt"
	"os"
	"strings"
)

// processName reads /proc/<pid>/comm, which is already the bare command name —
// no path to take a basename of.
func processName(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
