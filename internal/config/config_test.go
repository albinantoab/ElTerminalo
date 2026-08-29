package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestConfig returns a Config rooted at a fresh temp dir, plus the paths of
// the three files the state rotation deals with.
func newTestConfig(t *testing.T) (c *Config, state, bak string) {
	t.Helper()
	dir := t.TempDir()
	c = newWithDir(dir)
	return c, filepath.Join(dir, stateFileName), filepath.Join(dir, stateFileName+bakSuffix)
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func mustHavePerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %#o, want %#o", filepath.Base(path), got, want)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s not to exist, stat err = %v", path, err)
	}
}

// corruptFiles lists the quarantined copies of state.json in the config dir.
func corruptFiles(t *testing.T, c *Config) []string {
	t.Helper()
	return globConfig(t, c, stateFileName+corruptPrefix+"*")
}

// failedFiles lists the copies of state.json set aside by QuarantineState.
func failedFiles(t *testing.T, c *Config) []string {
	t.Helper()
	return globConfig(t, c, stateFileName+failedPrefix+"*")
}

func globConfig(t *testing.T, c *Config, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(c.Dir(), pattern))
	if err != nil {
		t.Fatalf("globbing %s: %v", pattern, err)
	}
	return matches
}

func TestLoadStateMissingReturnsEmpty(t *testing.T) {
	c, _, _ := newTestConfig(t)
	if got := c.LoadState(); got != "" {
		t.Fatalf("LoadState on empty dir = %q, want \"\"", got)
	}
	if files := corruptFiles(t, c); len(files) != 0 {
		t.Fatalf("a missing state.json must not be quarantined, found %v", files)
	}
}

func TestSaveLoadStateRoundTrip(t *testing.T) {
	c, state, bak := newTestConfig(t)

	const payload = `{"tabs":[{"id":"a"},{"id":"b"}]}`
	if err := c.SaveState(payload); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if got := c.LoadState(); got != payload {
		t.Fatalf("LoadState = %q, want %q", got, payload)
	}
	if got := mustRead(t, state); got != payload {
		t.Fatalf("state.json on disk = %q, want %q", got, payload)
	}
	// This file holds the user's tab names and the working directory of every
	// pane, and the mode it lands with is set by a Chmod that the umask does not
	// filter — so it is exactly what the call site names and nothing else keeps
	// it off the rest of the machine. Pin it.
	mustHavePerm(t, state, statePerm)

	// Nothing to rotate on the very first save.
	mustNotExist(t, bak)
	// The temp file must not survive the rename. Globbed, not named: the temp
	// name is unique per write, so asserting on a fixed one would pass even if
	// every scratch file were left behind.
	if leftovers := globConfig(t, c, "*.tmp*"); len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestSaveStateRotatesBackup(t *testing.T) {
	c, state, bak := newTestConfig(t)

	const first = `{"v":1}`
	const second = `{"v":2}`
	const third = `{"v":3}`

	if err := c.SaveState(first); err != nil {
		t.Fatalf("SaveState(first): %v", err)
	}
	if err := c.SaveState(second); err != nil {
		t.Fatalf("SaveState(second): %v", err)
	}

	if got := mustRead(t, state); got != second {
		t.Fatalf("state.json = %q, want %q", got, second)
	}
	if got := mustRead(t, bak); got != first {
		t.Fatalf("state.json.bak = %q, want %q", got, first)
	}
	// The backup is a whole copy of the same private data, so it is held to the
	// same mode as the primary.
	mustHavePerm(t, bak, statePerm)

	// Only one generation is kept: the third save pushes the second into .bak
	// and the first is gone.
	if err := c.SaveState(third); err != nil {
		t.Fatalf("SaveState(third): %v", err)
	}
	if got := mustRead(t, state); got != third {
		t.Fatalf("state.json = %q, want %q", got, third)
	}
	if got := mustRead(t, bak); got != second {
		t.Fatalf("state.json.bak = %q, want %q", got, second)
	}
}

func TestSaveStateIdenticalContentDoesNotRotate(t *testing.T) {
	c, state, bak := newTestConfig(t)

	const first = `{"v":1}`
	const second = `{"v":2}`

	if err := c.SaveState(first); err != nil {
		t.Fatalf("SaveState(first): %v", err)
	}
	if err := c.SaveState(second); err != nil {
		t.Fatalf("SaveState(second): %v", err)
	}
	if got := mustRead(t, bak); got != first {
		t.Fatalf("precondition: state.json.bak = %q, want %q", got, first)
	}

	stateBefore, err := os.Stat(state)
	if err != nil {
		t.Fatalf("stat state.json: %v", err)
	}

	// The 30-second autosave writes identical bytes; that must not push the
	// last genuinely different state out of the single backup slot.
	for range 3 {
		if err := c.SaveState(second); err != nil {
			t.Fatalf("SaveState(second, repeat): %v", err)
		}
	}

	if got := mustRead(t, bak); got != first {
		t.Fatalf("state.json.bak = %q after identical saves, want %q", got, first)
	}
	if got := mustRead(t, state); got != second {
		t.Fatalf("state.json = %q, want %q", got, second)
	}

	// The primary is left completely untouched, not rewritten with the same
	// bytes — same inode, same mtime.
	stateAfter, err := os.Stat(state)
	if err != nil {
		t.Fatalf("stat state.json: %v", err)
	}
	if !stateAfter.ModTime().Equal(stateBefore.ModTime()) {
		t.Fatalf("identical save rewrote state.json (mtime %v -> %v)",
			stateBefore.ModTime(), stateAfter.ModTime())
	}
}

func TestLoadStateCorruptPrimaryFallsBackToBackup(t *testing.T) {
	c, state, bak := newTestConfig(t)

	const good = `{"v":1}`
	const newer = `{"v":2}`

	if err := c.SaveState(good); err != nil {
		t.Fatalf("SaveState(good): %v", err)
	}
	if err := c.SaveState(newer); err != nil {
		t.Fatalf("SaveState(newer): %v", err)
	}
	if got := mustRead(t, bak); got != good {
		t.Fatalf("precondition: state.json.bak = %q, want %q", got, good)
	}

	// Simulate the truncated write a power loss used to leave behind.
	const garbage = `{"tabs":[{"id":`
	if err := os.WriteFile(state, []byte(garbage), 0644); err != nil {
		t.Fatalf("corrupting state.json: %v", err)
	}

	if got := c.LoadState(); got != good {
		t.Fatalf("LoadState = %q, want the backup %q", got, good)
	}

	// The damaged primary is moved aside, never deleted.
	mustNotExist(t, state)
	files := corruptFiles(t, c)
	if len(files) != 1 {
		t.Fatalf("want exactly one quarantined file, got %v", files)
	}
	if got := mustRead(t, files[0]); got != garbage {
		t.Fatalf("quarantined file = %q, want the original garbage %q", got, garbage)
	}

	// A save after the fallback rebuilds the primary and keeps the quarantine.
	if err := c.SaveState(`{"v":3}`); err != nil {
		t.Fatalf("SaveState after recovery: %v", err)
	}
	if len(corruptFiles(t, c)) != 1 {
		t.Fatalf("quarantined file disappeared after a save")
	}
}

func TestLoadStateEmptyPrimaryTreatedAsCorrupt(t *testing.T) {
	c, state, bak := newTestConfig(t)

	const good = `{"v":1}`
	if err := os.WriteFile(bak, []byte(good), 0644); err != nil {
		t.Fatalf("seeding state.json.bak: %v", err)
	}
	// A zero-length file is the classic result of a rename that landed before
	// the data was flushed.
	if err := os.WriteFile(state, nil, 0644); err != nil {
		t.Fatalf("seeding empty state.json: %v", err)
	}

	if got := c.LoadState(); got != good {
		t.Fatalf("LoadState = %q, want the backup %q", got, good)
	}
	files := corruptFiles(t, c)
	if len(files) != 1 {
		t.Fatalf("want exactly one quarantined file, got %v", files)
	}
	if got := mustRead(t, files[0]); got != "" {
		t.Fatalf("quarantined file = %q, want empty", got)
	}
}

func TestLoadStateCorruptPrimaryAndNoUsableBackup(t *testing.T) {
	c, state, bak := newTestConfig(t)

	if err := os.WriteFile(state, []byte("not json"), 0644); err != nil {
		t.Fatalf("seeding state.json: %v", err)
	}
	if err := os.WriteFile(bak, []byte("also not json"), 0644); err != nil {
		t.Fatalf("seeding state.json.bak: %v", err)
	}

	if got := c.LoadState(); got != "" {
		t.Fatalf("LoadState = %q, want \"\" when nothing parses", got)
	}
	if len(corruptFiles(t, c)) != 1 {
		t.Fatalf("the unparseable primary should still have been quarantined")
	}
}

func TestSaveStateDoesNotOverwriteBackupWithCorruptPrimary(t *testing.T) {
	c, state, bak := newTestConfig(t)

	const good = `{"v":1}`
	if err := os.WriteFile(bak, []byte(good), 0644); err != nil {
		t.Fatalf("seeding state.json.bak: %v", err)
	}
	if err := os.WriteFile(state, []byte("{broken"), 0644); err != nil {
		t.Fatalf("seeding corrupt state.json: %v", err)
	}

	if err := c.SaveState(`{"v":2}`); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if got := mustRead(t, bak); got != good {
		t.Fatalf("state.json.bak = %q, want the last good state %q", got, good)
	}
}

// concurrentPayload builds a distinct, valid state document for one writer.
//
// Two properties matter, and the test is blind to the bug without either. The
// documents are tens of KB, so a saver's write is not a single indivisible
// syscall from the reader's point of view. And they are of clearly *different*
// lengths, which is what turns a shared scratch file into visible corruption:
// a short writer overwriting a long writer's bytes in place leaves the long
// one's tail past its own end, and the published file is one prefix glued to
// another's suffix. Equal-length payloads mask exactly that, because the second
// write covers the first completely.
func concurrentPayload(writer int) string {
	name := strings.Repeat(string(rune('A'+writer)), 24)
	var b strings.Builder
	b.Grow(160 * 1024)
	fmt.Fprintf(&b, `{"version":1,"writer":%d,"tabs":[`, writer)
	for i := range 1200 + writer*500 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":%q,"index":%d,"layout":{"type":"leaf"}}`, name, i)
	}
	b.WriteString(`]}`)
	return b.String()
}

// TestSaveStateConcurrentWritesStayValid is the regression test for two
// overlapping SaveState calls corrupting state.json. Wails runs every binding
// call on its own goroutine, so the 30s autosave really does overlap a save
// from a divider drag or from the quit handshake; before the lock and the
// unique temp names, the survivor of such a pair was a byte-level splice of
// both payloads and the next launch quarantined it and fell back to .bak.
func TestSaveStateConcurrentWritesStayValid(t *testing.T) {
	const writers = 4
	const iterations = 25

	payloads := make([]string, writers)
	for i := range payloads {
		payloads[i] = concurrentPayload(i)
		if !json.Valid([]byte(payloads[i])) {
			t.Fatalf("payload %d is not valid JSON to begin with", i)
		}
		if len(payloads[i]) < 50*1024 {
			t.Fatalf("payload %d is only %d bytes; too small to expose a splice",
				i, len(payloads[i]))
		}
		if i > 0 && len(payloads[i]) <= len(payloads[i-1]) {
			t.Fatalf("payloads %d and %d must differ in length (%d vs %d)",
				i-1, i, len(payloads[i-1]), len(payloads[i]))
		}
	}

	// Distinct from every payload, and the content the first concurrent save
	// finds on disk. See the .bak assertion below for what it is for.
	const seed = `{"version":1,"writer":-1,"tabs":[]}`

	for iter := range iterations {
		// A fresh directory per iteration: each round has to start from the
		// same empty state, not inherit the previous round's winner.
		c := newWithDir(t.TempDir())
		if err := c.SaveState(seed); err != nil {
			t.Fatalf("iteration %d: seeding: %v", iter, err)
		}

		var wg sync.WaitGroup
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := c.SaveState(payloads[i]); err != nil {
					t.Errorf("iteration %d, writer %d: SaveState: %v", iter, i, err)
				}
			}()
		}
		wg.Wait()

		// The published file must be exactly one payload — valid JSON is not
		// enough on its own, since a splice of two documents can occasionally
		// still parse.
		got := mustRead(t, filepath.Join(c.Dir(), stateFileName))
		if !json.Valid([]byte(got)) {
			t.Fatalf("iteration %d: state.json is not valid JSON (%d bytes): %.160q",
				iter, len(got), got)
		}
		if !slices.Contains(payloads, got) {
			t.Fatalf("iteration %d: state.json (%d bytes) matches no single payload",
				iter, len(got))
		}

		// The backup is the copy LoadState falls back to, so a splice there is
		// just as damaging.
		bak := mustRead(t, filepath.Join(c.Dir(), stateFileName+bakSuffix))
		if !slices.Contains(payloads, bak) {
			// Still holding the seed means two savers both read the seed as
			// "the previous good copy" and both rotated it — a save that never
			// reached the backup at all. Serialized, the saves rotate in a
			// chain (seed -> p1 -> ... -> pN-1), so with more than one writer
			// the backup is always a payload and never the seed. This is the
			// half of the bug that unique temp names alone do not fix.
			what := fmt.Sprintf("matches no single payload (%d bytes)", len(bak))
			if bak == seed {
				what = "is still the seed: a whole generation never reached the backup"
			}
			t.Fatalf("iteration %d: state.json.bak %s", iter, what)
		}

		if leftovers := globConfig(t, c, "*.tmp*"); len(leftovers) != 0 {
			t.Fatalf("iteration %d: temp files left behind: %v", iter, leftovers)
		}
	}
}

func TestQuarantineStateMovesPresentFile(t *testing.T) {
	c, state, bak := newTestConfig(t)

	const older = `{"tabs":[{"name":"one"}]}`
	const current = `{"tabs":[{"name":"two"}]}`
	if err := c.SaveState(older); err != nil {
		t.Fatalf("SaveState(older): %v", err)
	}
	if err := c.SaveState(current); err != nil {
		t.Fatalf("SaveState(current): %v", err)
	}

	path, err := c.QuarantineState()
	if err != nil {
		t.Fatalf("QuarantineState: %v", err)
	}
	if path == "" {
		t.Fatal("QuarantineState returned no path for an existing state.json")
	}
	if want := regexp.MustCompile(`^` + regexp.QuoteMeta(state+failedPrefix) + `\d+$`); !want.MatchString(path) {
		t.Fatalf("quarantine path = %q, want %s", path, want)
	}

	// Moved, not copied and not deleted: the content has to survive intact.
	if got := mustRead(t, path); got != current {
		t.Fatalf("quarantined file = %q, want %q", got, current)
	}
	// A rename carries the mode with it, so the layout stays as private in
	// quarantine as it was in state.json — and it stays there indefinitely.
	mustHavePerm(t, path, statePerm)
	mustNotExist(t, state)
	if files := failedFiles(t, c); len(files) != 1 {
		t.Fatalf("want exactly one quarantined file, got %v", files)
	}
	// The backup is a separate recovery path and must be left alone.
	if got := mustRead(t, bak); got != older {
		t.Fatalf("state.json.bak = %q, want %q", got, older)
	}

	// The whole point: the layout that could not be rebuilt must not come back
	// on the next launch, or it would fail in exactly the same way again.
	if got := c.LoadState(); got != "" {
		t.Fatalf("LoadState after quarantine = %q, want \"\"", got)
	}
	// And it is still there afterwards for manual recovery.
	if got := mustRead(t, path); got != current {
		t.Fatalf("quarantined file changed after LoadState: %q", got)
	}
}

func TestQuarantineStateMissingFileIsNoOp(t *testing.T) {
	c, _, _ := newTestConfig(t)

	path, err := c.QuarantineState()
	if err != nil {
		t.Fatalf("QuarantineState on an empty dir: %v", err)
	}
	if path != "" {
		t.Fatalf("QuarantineState = %q, want \"\" when there is nothing to move", path)
	}
	if files := failedFiles(t, c); len(files) != 0 {
		t.Fatalf("nothing should have been created, found %v", files)
	}
	if got := c.LoadState(); got != "" {
		t.Fatalf("LoadState = %q, want \"\"", got)
	}
}

func TestQuarantineStateTwiceKeepsBothCopies(t *testing.T) {
	c, _, _ := newTestConfig(t)

	const first = `{"v":1}`
	const second = `{"v":2}`
	if err := c.SaveState(first); err != nil {
		t.Fatalf("SaveState(first): %v", err)
	}
	firstPath, err := c.QuarantineState()
	if err != nil {
		t.Fatalf("QuarantineState(first): %v", err)
	}
	if err := c.SaveState(second); err != nil {
		t.Fatalf("SaveState(second): %v", err)
	}
	secondPath, err := c.QuarantineState()
	if err != nil {
		t.Fatalf("QuarantineState(second): %v", err)
	}

	// Both quarantines land in the same second on any reasonable machine; the
	// second must not overwrite the first, because nothing here is recoverable.
	if firstPath == secondPath {
		t.Fatalf("both quarantines used the same path %q", firstPath)
	}
	if got := mustRead(t, firstPath); got != first {
		t.Fatalf("first quarantine = %q, want %q", got, first)
	}
	if got := mustRead(t, secondPath); got != second {
		t.Fatalf("second quarantine = %q, want %q", got, second)
	}
}

// seedFile writes a file into dir and backdates it by age, so a test can put a
// scratch file on either side of the sweep's cutoff.
func seedFile(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", name, err)
	}
	if age > 0 {
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatalf("backdating %s: %v", name, err)
		}
	}
	return path
}

// A crash between WriteFileDurable's CreateTemp and its rename leaves a scratch
// file that nothing ever comes back for, so without a sweep they accumulate for
// the life of the install. New() calls this against the real config directory;
// the sweep is a function of its own precisely so it can be tested against one
// that is not the user's.
func TestSweepTempFilesRemovesCrashDebris(t *testing.T) {
	dir := t.TempDir()

	debris := []string{
		stateFileName + ".tmp-1836274510",
		stateFileName + bakSuffix + ".tmp-99",
		"window.json.tmp-a",
	}
	// Everything the app publishes, plus both flavours of quarantine: the sweep
	// runs at startup, before anything is loaded, so mistaking one of these for
	// scratch would silently destroy the layout it was meant to protect.
	keep := []string{
		stateFileName,
		stateFileName + bakSuffix,
		"window.json",
		stateFileName + corruptPrefix + "1836274510",
		stateFileName + failedPrefix + "1836274510",
		"themes.json",
	}
	// Debris is old by definition — it is what a run that is already over left
	// behind — so it is seeded past the cutoff. The files that must survive are
	// seeded old too, so nothing here passes merely by being young.
	for _, name := range append(append([]string{}, debris...), keep...) {
		seedFile(t, dir, name, tempSweepMinAge+time.Hour)
	}

	sweepTempFiles(dir)

	for _, name := range debris {
		mustNotExist(t, filepath.Join(dir, name))
	}
	for _, name := range keep {
		if got := mustRead(t, filepath.Join(dir, name)); got != `{}` {
			t.Fatalf("%s = %q after the sweep, want it untouched", name, got)
		}
	}
}

// The sweep used to unlink every match, which made it destructive rather than
// tidy the moment two instances overlapped — and ApplyUpdate creates exactly
// that overlap on purpose, launching the new bundle with `open -n` before the
// old one has saved. Measured: the new instance's New() unlinked the old one's
// in-flight temp and the old one's rename then failed with ENOENT, so the
// layout at the moment of the update was the one save that never landed.
func TestSweepTempFilesSparesAnInFlightWrite(t *testing.T) {
	dir := t.TempDir()

	// What another instance's WriteFileDurable has open right now.
	inFlight := seedFile(t, dir, stateFileName+".tmp-inflight", 0)
	// Just inside the cutoff: a write that has been going for nine minutes is
	// implausible, but it is not this sweep's business to decide it is dead.
	recent := seedFile(t, dir, stateFileName+".tmp-recent", tempSweepMinAge-time.Minute)
	// Debris: nothing has come back for this since the crash that left it.
	stale := seedFile(t, dir, stateFileName+".tmp-stale", tempSweepMinAge+time.Minute)

	sweepTempFiles(dir)

	if got := mustRead(t, inFlight); got != `{}` {
		t.Fatalf("the sweep destroyed a temp file another instance is writing: %q", got)
	}
	if got := mustRead(t, recent); got != `{}` {
		t.Fatalf("the sweep removed a temp file younger than %s", tempSweepMinAge)
	}
	mustNotExist(t, stale)
}

// The sweep unlinks whatever the glob matches, in a directory the user owns, so
// it has to be narrower than the pattern: a directory is never one of our
// scratch files.
func TestSweepTempFilesLeavesDirectoriesAlone(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "shell.tmp-x")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatalf("creating %s: %v", nested, err)
	}

	sweepTempFiles(dir)

	if info, err := os.Stat(nested); err != nil || !info.IsDir() {
		t.Fatalf("the sweep removed a directory (stat err = %v)", err)
	}
}

// The sweep must survive a first run, where New has just created the directory
// and there is nothing in it at all.
func TestSweepTempFilesOnEmptyDirectory(t *testing.T) {
	sweepTempFiles(t.TempDir())
}

func TestWindowGeometryRoundTrip(t *testing.T) {
	c, _, _ := newTestConfig(t)

	if got := c.LoadWindowGeometry(); got != nil {
		t.Fatalf("LoadWindowGeometry with no file = %+v, want nil", got)
	}

	want := WindowGeometry{Width: 1280, Height: 800, X: 120, Y: 64}
	if err := c.SaveWindowGeometry(want); err != nil {
		t.Fatalf("SaveWindowGeometry: %v", err)
	}
	if leftovers := globConfig(t, c, "*.tmp*"); len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
	mustHavePerm(t, filepath.Join(c.Dir(), "window.json"), statePerm)

	got := c.LoadWindowGeometry()
	if got == nil || *got != want {
		t.Fatalf("LoadWindowGeometry = %+v, want %+v", got, want)
	}
}

func TestLoadWindowGeometryRejectsImplausibleValues(t *testing.T) {
	cases := map[string]WindowGeometry{
		"too narrow":    {Width: 100, Height: 800},
		"too short":     {Width: 1280, Height: 10},
		"far offscreen": {Width: 1280, Height: 800, X: 99999},
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			c, _, _ := newTestConfig(t)
			if err := c.SaveWindowGeometry(g); err != nil {
				t.Fatalf("SaveWindowGeometry: %v", err)
			}
			if got := c.LoadWindowGeometry(); got != nil {
				t.Fatalf("LoadWindowGeometry = %+v, want nil", got)
			}
		})
	}
}
