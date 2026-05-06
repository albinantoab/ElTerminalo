//go:build !darwin

package stats

func readCPUTicks() (busy, total uint64, ok bool) { return 0, 0, false }
func readMemory() (used, total uint64)            { return 0, 0 }
