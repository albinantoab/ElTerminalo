// Package stats samples the running process's CPU and memory usage.
package stats

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Snapshot is a point-in-time reading of process resource usage.
type Snapshot struct {
	CPUPercent float64 `json:"cpuPercent"`
	MemoryMB   float64 `json:"memoryMB"`
}

// Sampler maintains state between samples so it can compute CPU% as a delta
// over wall time. It is safe for concurrent use.
type Sampler struct {
	mu       sync.Mutex
	pid      int
	lastWall time.Time
	lastCPU  time.Duration
	primed   bool
}

// New returns a Sampler bound to the current process.
func New() *Sampler {
	return &Sampler{pid: os.Getpid()}
}

// Sample returns the latest CPU% (since the previous Sample call) and current
// resident memory. The first call returns 0% CPU because there is no prior
// reference point.
func (s *Sampler) Sample() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	cpu := processCPUTime()

	var pct float64
	if s.primed {
		dWall := now.Sub(s.lastWall).Seconds()
		dCPU := (cpu - s.lastCPU).Seconds()
		if dWall > 0 {
			pct = (dCPU / dWall) * 100
			if pct < 0 {
				pct = 0
			}
		}
	}
	s.lastWall = now
	s.lastCPU = cpu
	s.primed = true

	return Snapshot{
		CPUPercent: pct,
		MemoryMB:   processRSSMB(s.pid),
	}
}

// processCPUTime returns total user+system CPU time consumed by this process.
func processCPUTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	utime := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
	stime := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
	return utime + stime
}

// processRSSMB returns the current resident set size in MB. macOS does not
// expose RSS via syscall without cgo, so we shell out to ps which is fast
// enough at the polling cadence we use.
func processRSSMB(pid int) float64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	kb, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return kb / 1024.0
}
