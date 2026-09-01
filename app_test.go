package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
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

// --- what a terminal-supplied string is allowed to become ---

// The title, the notification text and the Dock badge all end up as strings
// AppKit renders, and all three ultimately come from an escape sequence any
// program the user runs can print.
func TestSanitizersDropTheRunesThatReorderText(t *testing.T) {
	// U+202E reverses everything after it; U+200B is invisible; \r\n and \x1b
	// are control characters.
	nasty := "safe‮desrever​\r\n\x1b[31m"

	if got := sanitizeWindowTitle(nasty); got != "safedesrever[31m" {
		t.Errorf("sanitizeWindowTitle(%q) = %q", nasty, got)
	}
	if got := sanitizeNotificationText(nasty, 128); got != "safedesrever[31m" {
		t.Errorf("sanitizeNotificationText(%q) = %q", nasty, got)
	}
	if got := sanitizeBadgeLabel(nasty); got != "safedesr" {
		t.Errorf("sanitizeBadgeLabel(%q) = %q", nasty, got)
	}
}

func TestSanitizeWindowTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", appTitle},
		{"   ", appTitle},
		{"\x00\x01\x02", appTitle},
		{"  ~/src/elterminalo  ", "~/src/elterminalo"},
		{"zsh", "zsh"},
	}
	for _, c := range cases {
		if got := sanitizeWindowTitle(c.in); got != c.want {
			t.Errorf("sanitizeWindowTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	long := sanitizeWindowTitle(strings.Repeat("a", maxWindowTitleBytes*3))
	if len(long) != maxWindowTitleBytes {
		t.Errorf("a long title came out %d bytes, want %d", len(long), maxWindowTitleBytes)
	}
	// A cut that lands mid-rune must move back, never leave half of one.
	multi := sanitizeWindowTitle(strings.Repeat("é", maxWindowTitleBytes))
	if !utf8.ValidString(multi) {
		t.Errorf("truncation split a rune: %q", multi)
	}
	if len(multi) > maxWindowTitleBytes {
		t.Errorf("a multi-byte title came out %d bytes, want at most %d", len(multi), maxWindowTitleBytes)
	}
}

// An empty title falls back to the app's name; an empty body must not, or every
// notification without a body would claim to be about the app itself.
func TestSanitizeNotificationTextLeavesAnEmptyResultEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\r\n\t", "​"} {
		if got := sanitizeNotificationText(in, maxNotificationBodyBytes); got != "" {
			t.Errorf("sanitizeNotificationText(%q) = %q, want empty", in, got)
		}
	}

	long := sanitizeNotificationText(strings.Repeat("b", maxNotificationBodyBytes*2), maxNotificationBodyBytes)
	if len(long) != maxNotificationBodyBytes {
		t.Errorf("a long body came out %d bytes, want %d", len(long), maxNotificationBodyBytes)
	}
}

func TestSanitizeBadgeLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"3", "3"},
		{"12", "12"},
		{"  7  ", "7"},
		{"a\nb", "a b"},
		{"3\t4", "3 4"},
		{"a  b   c", "a b c"},
		{"123456789", "12345678"},
		{"999+", "999+"},
	}
	for _, c := range cases {
		if got := sanitizeBadgeLabel(c.in); got != c.want {
			t.Errorf("sanitizeBadgeLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// The cap is in runes, not bytes: eight emoji are eight characters, and
	// cutting one in half would put a replacement character on the Dock.
	badge := sanitizeBadgeLabel(strings.Repeat("🚀", 20))
	if n := utf8.RuneCountInString(badge); n != maxDockBadgeRunes {
		t.Errorf("a long badge came out %d runes, want %d", n, maxDockBadgeRunes)
	}
	if !utf8.ValidString(badge) {
		t.Errorf("the badge cap split a rune: %q", badge)
	}
}

// --- transcript filenames ---

// The session id is joined into a path and it arrives from the webview.
func TestShortSessionID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"9f1c2b3a-4d5e-6f70-8a9b-0c1d2e3f4a5b", "9f1c2b3a"},
		{"short", "short"},
		{"", "session"},
		{"../../etc/passwd", "______et"},
		{"a/b\\c:d", "a_b_c_d"},
		{"ABC_def-1", "ABC_def-"},
		{"日本語", "___"},
	}
	for _, c := range cases {
		got := shortSessionID(c.in)
		if got != c.want {
			t.Errorf("shortSessionID(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(got, `/\.`) {
			t.Errorf("shortSessionID(%q) = %q, which can escape its directory", c.in, got)
		}
		if len(got) > transcriptIDChars && c.in != "" {
			t.Errorf("shortSessionID(%q) = %q, longer than %d characters", c.in, got, transcriptIDChars)
		}
	}
}

// --- opening things ---

// OpenPath execs /usr/bin/open with a string from the webview, where a leading
// "-" would be read as a flag.
func TestOpenPathRefusesAnythingButAnExistingAbsolutePath(t *testing.T) {
	silenceLog(t)
	app := &App{}

	dir := t.TempDir()
	for _, path := range []string{
		"", "relative/path", "./also-relative", "-R", "--args",
		filepath.Join(dir, "does-not-exist"),
	} {
		if err := app.OpenPath(path); err == nil {
			t.Errorf("OpenPath(%q) reported success", path)
		}
	}

	// RevealPath cannot report anything, so all that is asserted is that the
	// same inputs do not panic or launch anything.
	for _, path := range []string{"", "relative/path", "-R", filepath.Join(dir, "nope")} {
		app.RevealPath(path)
	}
}

// --- notification logging ---

// A notification is asked for when the frontend sees OSC 9, which is a sequence
// any program can print as fast as it likes. Only the log is bounded — the
// binding's false must keep meaning "unavailable or unauthorized".
func TestNotifyLogIsRateLimited(t *testing.T) {
	silenceLog(t)
	prevLimiter, prevClock := notifyLog, bellClock
	t.Cleanup(func() { notifyLog, bellClock = prevLimiter, prevClock })

	clk := &fakeClock{now: time.Unix(1800000000, 0)}
	notifyLog = newBellLimiter(bellBurst, bellRatePerSec, bellAttentionGap)
	bellClock = clk.Now

	for range bellBurst {
		if allowed, _, _ := notifyLog.take(clk.Now()); !allowed {
			t.Fatal("a line inside the burst was dropped")
		}
	}
	for range 10_000 {
		if allowed, _, _ := notifyLog.take(clk.Now()); allowed {
			t.Fatal("a line past the burst was written with no time having passed")
		}
	}

	// Notify itself still answers, however many times it is called: an
	// unauthorized process returns false, and never blocks doing it.
	app := &App{}
	for range 1000 {
		if app.Notify("Build finished", "exit 0", "tab-1/pane-2") {
			t.Fatal("Notify reported success in a process with no notification center")
		}
	}
	if app.Notify("", "", "k") {
		t.Fatal("Notify reported success for a notification with no text")
	}
}

// --- which editor "Open Folder in Editor" actually opens ---

// fakeAppDir builds a directory of (empty) application bundles and points the
// bundle search at it, so a test's answer does not depend on what is installed
// on the machine running it.
func fakeAppDir(t *testing.T, apps ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range apps {
		if err := os.MkdirAll(filepath.Join(dir, name+".app"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prev := appSearchDirs
	t.Cleanup(func() { appSearchDirs = prev })
	appSearchDirs = func() []string { return []string{dir} }
	return dir
}

// OpenPath used to be a bare `open <path>`, and `open` on a directory hands it
// to Finder — so "Open Folder in Editor" and "Reveal in Finder" were the same
// command twice. This is the order that fixes it.
func TestResolveEditorAppPicksAnEditorRatherThanTheFolderHandler(t *testing.T) {
	silenceLog(t)

	cases := []struct {
		name      string
		installed []string
		visual    string
		editor    string
		isDir     bool
		wantApp   string // "" means: hand it to `open` on its own
		wantWhy   string
	}{{
		name:      "VISUAL wins when it names a bundle",
		installed: []string{"Zed", "Nova", preferredEditorApp},
		visual:    "Zed",
		editor:    "Nova",
		isDir:     true,
		wantApp:   "Zed.app",
		wantWhy:   "$VISUAL",
	}, {
		name:      "EDITOR is next",
		installed: []string{"Nova", preferredEditorApp},
		editor:    "Nova",
		isDir:     true,
		wantApp:   "Nova.app",
		wantWhy:   "$EDITOR",
	}, {
		// The whole point of the .app check: `open -a vim` is not a thing.
		name:      "a terminal editor is skipped",
		installed: []string{preferredEditorApp},
		visual:    "vim",
		editor:    "nano",
		isDir:     true,
		wantApp:   preferredEditorApp + ".app",
		wantWhy:   preferredEditorApp,
	}, {
		name:      "flags are not part of the application name",
		installed: []string{"Zed"},
		visual:    "Zed --wait",
		isDir:     true,
		wantApp:   "Zed.app",
		wantWhy:   "$VISUAL",
	}, {
		name:      "an editor named with a .app suffix still resolves",
		installed: []string{"Zed"},
		editor:    "Zed.app",
		isDir:     true,
		wantApp:   "Zed.app",
		wantWhy:   "$EDITOR",
	}, {
		// A bare application name with spaces in it is the other setting that
		// has to survive the flag-stripping.
		name:      "an application name with spaces",
		installed: []string{"Sublime Text"},
		editor:    "Sublime Text",
		isDir:     true,
		wantApp:   "Sublime Text.app",
		wantWhy:   "$EDITOR",
	}, {
		name:      "Visual Studio Code when nothing is named",
		installed: []string{preferredEditorApp, fileEditorApp},
		isDir:     true,
		wantApp:   preferredEditorApp + ".app",
		wantWhy:   preferredEditorApp,
	}, {
		name:      "TextEdit for a file when there is nothing better",
		installed: []string{fileEditorApp},
		isDir:     false,
		wantApp:   fileEditorApp + ".app",
		wantWhy:   fileEditorApp,
	}, {
		// TextEdit declares no folder document type, so handing it a directory
		// launches it only to show its own error. The OS default is the honest
		// answer here.
		name:      "never TextEdit for a folder",
		installed: []string{fileEditorApp},
		isDir:     true,
		wantApp:   "",
	}, {
		name:      "nothing installed at all",
		installed: nil,
		isDir:     true,
		wantApp:   "",
	}, {
		// $EDITOR is the user's own string, but it is still being joined into a
		// path under /Applications.
		name:      "a name that would climb out of the search directories is refused",
		installed: []string{preferredEditorApp},
		visual:    "../../../../etc",
		isDir:     true,
		wantApp:   preferredEditorApp + ".app",
		wantWhy:   preferredEditorApp,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := fakeAppDir(t, c.installed...)
			t.Setenv("VISUAL", c.visual)
			t.Setenv("EDITOR", c.editor)

			app, why := resolveEditorApp(c.isDir)
			want := ""
			if c.wantApp != "" {
				want = filepath.Join(dir, c.wantApp)
			}
			if app != want {
				t.Errorf("resolveEditorApp(isDir=%t) = %q, want %q", c.isDir, app, want)
			}
			if c.wantWhy != "" && why != c.wantWhy {
				t.Errorf("reason = %q, want %q", why, c.wantWhy)
			}
		})
	}
}

// An editor named by its full path is the escape hatch for one installed
// somewhere the search does not look.
func TestResolveEditorAppAcceptsAnAbsoluteBundlePathAndNothingElse(t *testing.T) {
	silenceLog(t)
	fakeAppDir(t) // nothing installed, so only the env var can answer

	bundle := filepath.Join(t.TempDir(), "Somewhere Else.app")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", "")
	t.Setenv("VISUAL", bundle)
	if got, why := resolveEditorApp(true); got != bundle || why != "$VISUAL" {
		t.Errorf("resolveEditorApp with $VISUAL=%q = (%q, %q)", bundle, got, why)
	}

	// An absolute path that is not a bundle is a terminal editor, whatever its
	// name: `open -a` cannot run one, so it has to fall through.
	cli := filepath.Join(t.TempDir(), "nvim")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", cli)
	if got, _ := resolveEditorApp(true); got != "" {
		t.Errorf("resolveEditorApp with $VISUAL=%q = %q, want the OS default", cli, got)
	}
}

// A screenshot dragged from macOS's thumbnail lives in a directory the system
// deletes seconds later, so the true path the native drop reports is worthless
// by the time anything reads it. These cover the swap that fixes that, and the
// equally important half: a path that is durable must be left exactly alone.

func TestIsVolatilePathFollowsTheConfiguredRoots(t *testing.T) {
	tmp := t.TempDir()
	prev := volatileRoots
	volatileRoots = []string{tmp, "/tmp"}
	t.Cleanup(func() { volatileRoots = prev })

	for _, tc := range []struct {
		path string
		want bool
	}{
		{filepath.Join(tmp, "TemporaryItems", "NSIRD_screencaptureui_x", "Shot.png"), true},
		{tmp, true},
		{"/tmp/x.png", true},
		{"/tmpfoo/x.png", false}, // prefix match must respect the separator
		{"/Users/someone/Documents/x.png", false},
		{"/Users/someone/Desktop/Shot.png", false},
	} {
		if got := isVolatilePath(tc.path); got != tc.want {
			t.Errorf("isVolatilePath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestPreserveVolatileDropsCopiesOutOfTempAndLeavesDurablePathsAlone(t *testing.T) {
	volatile := t.TempDir()
	durable := t.TempDir()
	cfg := t.TempDir()

	prev := volatileRoots
	volatileRoots = []string{volatile}
	t.Cleanup(func() { volatileRoots = prev })

	shot := filepath.Join(volatile, "Screenshot 2026-09-01 at 4.12.37 PM.png")
	want := []byte("\x89PNG\r\n\x1a\n screenshot bytes")
	if err := os.WriteFile(shot, want, 0o600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(durable, "notes.txt")
	if err := os.WriteFile(keep, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := preserveVolatileDrops(cfg, []string{shot, keep})
	if len(got) != 2 {
		t.Fatalf("got %d paths, want 2: %q", len(got), got)
	}

	if got[1] != keep {
		t.Errorf("a durable path was rewritten: %q, want %q", got[1], keep)
	}
	if got[0] == shot {
		t.Fatal("the volatile path was handed back unchanged; it will be gone before anything reads it")
	}
	if dir := filepath.Join(cfg, dropsDirName); !strings.HasPrefix(got[0], dir+string(os.PathSeparator)) {
		t.Errorf("copy landed at %q, want it under %q", got[0], dir)
	}
	if !strings.HasSuffix(got[0], "Screenshot 2026-09-01 at 4.12.37 PM.png") {
		t.Errorf("copy lost the original name: %q", got[0])
	}

	// The copy must survive the source being deleted — that is the whole point.
	if err := os.Remove(shot); err != nil {
		t.Fatal(err)
	}
	back, err := os.ReadFile(got[0])
	if err != nil {
		t.Fatalf("preserved copy unreadable after the source vanished: %v", err)
	}
	if !bytes.Equal(back, want) {
		t.Errorf("preserved copy differs from the dropped file")
	}
	if info, err := os.Stat(got[0]); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("copy mode = %v, want 0600", info.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(cfg, dropsDirName, ".drop-*")); len(left) != 0 {
		t.Errorf("a half-written temp file was left behind: %q", left)
	}
}

func TestPreserveVolatileDropsSkipsAFileThatVanishedMidDrop(t *testing.T) {
	volatile := t.TempDir()
	cfg := t.TempDir()
	prev := volatileRoots
	volatileRoots = []string{volatile}
	t.Cleanup(func() { volatileRoots = prev })

	got := preserveVolatileDrops(cfg, []string{filepath.Join(volatile, "already-gone.png")})
	if len(got) != 0 {
		t.Errorf("got %q, want nothing: a path whose file is gone must not be pasted into a shell", got)
	}
}

func TestPruneOldDropsKeepsRecentCopies(t *testing.T) {
	cfg := t.TempDir()
	dir := filepath.Join(cfg, dropsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "fresh.png")
	stale := filepath.Join(dir, "stale.png")
	for _, p := range []string{fresh, stale} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-dropRetention - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	pruneOldDrops(cfg)

	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a copy inside the retention window was deleted: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a copy past the retention window survived: %v", err)
	}
	pruneOldDrops(t.TempDir()) // no drops directory at all must not panic
}
