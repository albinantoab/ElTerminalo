package main

import (
	"io"
	"log"
	"os"
	"sync"
	"testing"
	"time"
)

// --- the bell rate limit ---

// resetBellLimiter gives one test a fresh limiter and a clock it controls. The
// limiter is process-wide state — that is the whole point of it — so without
// this a test would inherit whatever the previous one spent, and `-count=2`
// would run every case a second time against a drained bucket.
func resetBellLimiter(t *testing.T, start time.Time) *fakeClock {
	t.Helper()
	prevLimiter, prevClock := bells, bellClock
	t.Cleanup(func() { bells, bellClock = prevLimiter, prevClock })

	clk := &fakeClock{now: start}
	bells = newBellLimiter(bellBurst, bellRatePerSec, bellAttentionGap)
	bellClock = clk.Now
	return clk
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// silenceLog keeps a test's own drop summaries out of the test output.
func silenceLog(t *testing.T) {
	t.Helper()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
}

// The failure this bounds: a BEL byte is something any program the user runs
// can print — `cat` of a binary prints thousands a second — and every one of
// them reaches Bell as a binding call that queues two blocks on the main queue.
func TestBellLimiterAllowsABurstThenDrops(t *testing.T) {
	clk := resetBellLimiter(t, time.Unix(1800000000, 0))

	for i := range bellBurst {
		if beep, _, _ := bells.take(clk.Now()); !beep {
			t.Fatalf("bell %d of the burst was dropped", i)
		}
	}
	for range 10_000 {
		if beep, _, _ := bells.take(clk.Now()); beep {
			t.Fatal("a bell past the burst rang with no time having passed")
		}
	}
}

func TestBellLimiterRefills(t *testing.T) {
	clk := resetBellLimiter(t, time.Unix(1800000000, 0))

	rung := func(n int) int {
		count := 0
		for range n {
			if beep, _, _ := bells.take(clk.Now()); beep {
				count++
			}
		}
		return count
	}

	if got := rung(bellBurst + 10); got != bellBurst {
		t.Fatalf("the burst rang %d bells, want %d", got, bellBurst)
	}

	// One second of quiet buys back exactly bellRatePerSec bells, and not one
	// more.
	clk.advance(time.Second)
	if got := rung(bellRatePerSec + 5); got != bellRatePerSec {
		t.Fatalf("a second of quiet bought %d bells, want %d", got, bellRatePerSec)
	}

	// And the bucket never fills past its burst, however long the terminal sits
	// idle.
	clk.advance(time.Hour)
	if got := rung(bellBurst + 20); got != bellBurst {
		t.Fatalf("an hour of idling bought %d bells, want the burst of %d", got, bellBurst)
	}
}

// The Dock bounce is bounded separately and much harder than the sound: it
// leaves the tile marked, so it is the half of a bell that outlives the event.
func TestBellLimiterBouncesTheDockAtMostOncePerGap(t *testing.T) {
	clk := resetBellLimiter(t, time.Unix(1800000000, 0))

	if beep, attention, _ := bells.take(clk.Now()); !beep || !attention {
		t.Fatalf("the first bell rang=%t bounced=%t, want both", beep, attention)
	}
	// The rest of the burst still rings, and none of it bounces again.
	for i := 1; i < bellBurst; i++ {
		beep, attention, _ := bells.take(clk.Now())
		if !beep {
			t.Fatalf("bell %d of the burst was dropped", i)
		}
		if attention {
			t.Fatalf("bell %d bounced the Dock inside the %s gap", i, bellAttentionGap)
		}
	}

	// Just short of the gap: still nothing, even with tokens available.
	clk.advance(bellAttentionGap - time.Millisecond)
	if _, attention, _ := bells.take(clk.Now()); attention {
		t.Fatalf("the Dock bounced %s after the last one, want at most once per %s",
			bellAttentionGap-time.Millisecond, bellAttentionGap)
	}

	clk.advance(time.Millisecond)
	if _, attention, _ := bells.take(clk.Now()); !attention {
		t.Fatalf("the Dock did not bounce again after a full %s", bellAttentionGap)
	}
}

// A bell suppressed as noise must not still be able to mark the Dock tile, so
// attention is never reported without beep. A bucket that never refills makes
// that testable on its own: the gap elapses while the tokens never come back.
func TestBellLimiterNeverBouncesTheDockWithoutRinging(t *testing.T) {
	b := newBellLimiter(1, 0, time.Second)
	now := time.Unix(1800000000, 0)

	if beep, attention, _ := b.take(now); !beep || !attention {
		t.Fatalf("the first bell rang=%t bounced=%t, want both", beep, attention)
	}

	now = now.Add(10 * time.Second)
	beep, attention, _ := b.take(now)
	if beep {
		t.Fatal("a bucket with no refill rang a second bell")
	}
	if attention {
		t.Fatal("a dropped bell bounced the Dock")
	}
}

// A dropped bell is gone, and the log is the only place that can say so — but a
// line per dropped bell would turn a bell flood into a log flood.
func TestBellLimiterSummarisesDroppedBellsOncePerWindow(t *testing.T) {
	clk := resetBellLimiter(t, time.Unix(1800000000, 0))

	for range bellBurst {
		if _, _, dropped := bells.take(clk.Now()); dropped != 0 {
			t.Fatalf("a summary reported %d drops while nothing had been dropped", dropped)
		}
	}

	// The first drop says so immediately: that line is the marker that says the
	// bells from here on are incomplete.
	if _, _, dropped := bells.take(clk.Now()); dropped != 1 {
		t.Fatalf("the first dropped bell reported %d, want 1", dropped)
	}
	// Then silence for a window, however hard the terminal pushes.
	for range 10_000 {
		if _, _, dropped := bells.take(clk.Now()); dropped != 0 {
			t.Fatalf("a second summary inside one window reported %d", dropped)
		}
	}

	// The next window reports the whole backlog in one go — and does so on the
	// next call even though the bucket has refilled by then and that call is
	// allowed through.
	clk.advance(bellDropSummaryGap)
	beep, _, dropped := bells.take(clk.Now())
	if !beep {
		t.Fatal("a bell after a full minute of quiet was dropped")
	}
	if dropped != 10_000 {
		t.Fatalf("the summary reported %d dropped bells, want 10000", dropped)
	}
}

// Bell is a binding, and Wails dispatches every binding call on its own
// goroutine, so several bells really are concurrent.
func TestBellIsSafeForConcurrentUse(t *testing.T) {
	resetBellLimiter(t, time.Now())
	bellClock = time.Now // a real clock: the race detector wants real overlap
	silenceLog(t)

	app := &App{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				app.Bell()
			}
		}()
	}
	wg.Wait()
}
