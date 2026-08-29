package ptymanager

import (
	"testing"
	"time"
)

// Temporary: baseline timing for GetAllSessionStatuses. Deleted before commit.
func TestTmpStatusTiming(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	m := NewManager(writeTestShell(t, idle), t.TempDir())
	t.Cleanup(m.CloseAll)

	const panes = 4
	for range panes {
		dir := t.TempDir()
		if _, err := m.CreateSession(80, 24, dir); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		waitForReady(t, dir)
	}

	// warm up
	m.GetAllSessionStatuses()

	const iters = 20
	start := time.Now()
	for range iters {
		m.GetAllSessionStatuses()
	}
	t.Logf("GetAllSessionStatuses with %d sessions: %v/call", panes, time.Since(start)/iters)
}
