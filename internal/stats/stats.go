// Package stats samples system-wide CPU and memory usage.
package stats

import "sync"

// Snapshot is a point-in-time reading of host resource usage.
type Snapshot struct {
	CPUPercent    float64 `json:"cpuPercent"`
	MemoryUsedMB  float64 `json:"memoryUsedMB"`
	MemoryTotalMB float64 `json:"memoryTotalMB"`
	MemoryPercent float64 `json:"memoryPercent"`
}

// Sampler maintains state between samples so it can compute CPU% as a delta
// between Mach host_statistics calls. It is safe for concurrent use.
type Sampler struct {
	mu        sync.Mutex
	lastBusy  uint64
	lastTotal uint64
	primed    bool
}

// New returns a Sampler bound to the host.
func New() *Sampler { return &Sampler{} }

// Sample returns the latest CPU% (since the previous Sample call) and current
// host memory usage. The first call returns 0% CPU because there is no prior
// reference point.
func (s *Sampler) Sample() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	var snap Snapshot

	if busy, total, ok := readCPUTicks(); ok {
		if s.primed {
			dBusy := busy - s.lastBusy
			dTotal := total - s.lastTotal
			if dTotal > 0 && dBusy <= dTotal {
				snap.CPUPercent = float64(dBusy) / float64(dTotal) * 100
			}
		}
		s.lastBusy = busy
		s.lastTotal = total
		s.primed = true
	}

	used, total := readMemory()
	if total > 0 {
		const mb = 1024 * 1024
		snap.MemoryUsedMB = float64(used) / mb
		snap.MemoryTotalMB = float64(total) / mb
		snap.MemoryPercent = float64(used) / float64(total) * 100
	}

	return snap
}
