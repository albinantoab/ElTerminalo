package logging

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureLog redirects the standard logger into a buffer for the duration of
// one test. The buffer is returned; the previous destination and flags are
// restored afterwards so `-count=2` reruns start from the same state.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prevOut, prevFlags := log.Writer(), log.Flags()
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	return &buf
}

// logGenerations lists the log file and its rotated generations in the order
// newest-first: the live file, then .1, then .2.
func logGenerations(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestRotationKeepsExactlyThreeFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFileName)

	// 32-byte budget with 10-byte lines: every fourth line trips the rotation.
	w, err := newRotatingWriter(path, 32, keptRotations)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// Enough writes to rotate several times over, so any generation beyond the
	// two we keep would have had many chances to appear.
	for i := range 40 {
		if _, err := w.Write([]byte(fmt.Sprintf("line %03d\n", i))); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	got := logGenerations(t, dir)
	want := []string{logFileName, logFileName + ".1", logFileName + ".2"}
	if len(got) != len(want) {
		t.Fatalf("log directory holds %v, want exactly %v", got, want)
	}
	for _, name := range want {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("a third rotated generation survived: stat err = %v", err)
	}
}

func TestRotationLiveFileHoldsTheNewestLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFileName)

	w, err := newRotatingWriter(path, 32, keptRotations)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	const lines = 40
	for i := range lines {
		if _, err := w.Write([]byte(fmt.Sprintf("line %03d\n", i))); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}

	live := read(logFileName)
	newest := fmt.Sprintf("line %03d", lines-1)
	if !strings.Contains(live, newest) {
		t.Fatalf("live log does not contain the last line %q; it holds:\n%s", newest, live)
	}

	// The rotated generations must be strictly older than the live file, which
	// is the whole point of the ordering: .1 is the previous file, .2 the one
	// before that.
	first, second := read(logFileName+".1"), read(logFileName+".2")
	if strings.Contains(first, newest) || strings.Contains(second, newest) {
		t.Fatalf("the newest line appears in a rotated generation:\n.1=%s\n.2=%s", first, second)
	}
	if firstNum, secondNum := firstLineNumber(t, first), firstLineNumber(t, second); secondNum >= firstNum {
		t.Fatalf(".2 (starts at line %d) is not older than .1 (starts at line %d)", secondNum, firstNum)
	}
	if liveNum, firstNum := firstLineNumber(t, live), firstLineNumber(t, first); firstNum >= liveNum {
		t.Fatalf(".1 (starts at line %d) is not older than the live file (starts at line %d)", firstNum, liveNum)
	}
}

// firstLineNumber pulls the counter out of the first "line NNN" record.
func firstLineNumber(t *testing.T, contents string) int {
	t.Helper()
	line, _, _ := strings.Cut(strings.TrimSpace(contents), "\n")
	var n int
	if _, err := fmt.Sscanf(line, "line %d", &n); err != nil {
		t.Fatalf("cannot read a line number out of %q: %v", line, err)
	}
	return n
}

// A single line larger than the whole budget must not send the writer into an
// endless rotate-per-write loop that leaves only empty generations behind.
func TestRotationHandlesLineLargerThanBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFileName)

	w, err := newRotatingWriter(path, 16, keptRotations)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	huge := strings.Repeat("x", 200) + "\n"
	for range 3 {
		if _, err := w.Write([]byte(huge)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading live log: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("live log is empty: an oversized line was rotated away instead of written")
	}
	if got := len(logGenerations(t, dir)); got > 3 {
		t.Fatalf("log directory holds %d files, want at most 3", got)
	}
}

func TestWriteAfterCloseIsSilentlyDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFileName)

	w, err := newRotatingWriter(path, maxLogBytes, keptRotations)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A closed writer must not reopen the path — its replacement owns it now —
	// and must not report an error, which would break the MultiWriter.
	if n, err := w.Write([]byte("after close\n")); err != nil || n != len("after close\n") {
		t.Fatalf("Write after Close = (%d, %v), want (%d, nil)", n, err, len("after close\n"))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("closed writer still wrote %q", data)
	}
}

func TestInitCreatesLogAndCapturesStandardLog(t *testing.T) {
	prevOut, prevFlags := log.Writer(), log.Flags()
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	configDir := t.TempDir()
	path, err := Init(configDir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		mu.Lock()
		w := current
		current, currentPath = nil, ""
		mu.Unlock()
		if w != nil {
			_ = w.Close()
		}
	})

	want := filepath.Join(configDir, logDirName, logFileName)
	if path != want {
		t.Fatalf("Init returned %s, want %s", path, want)
	}
	if got := Path(); got != want {
		t.Fatalf("Path() = %s, want %s", got, want)
	}

	log.Printf("marker %s", "one")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}
	if !strings.Contains(string(data), "marker one") {
		t.Fatalf("log file does not contain the line just written:\n%s", data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if got := info.Mode().Perm(); got != logPerm {
		t.Fatalf("log mode = %#o, want %#o", got, logPerm)
	}
}

func TestFrontendSanitizesControlCharacters(t *testing.T) {
	resetFrontendLimiter(t, time.Now())
	buf := captureLog(t)

	// A forged second log line is the attack this guards against: the message
	// carries a newline plus something that looks like a fresh entry.
	Frontend("error", "boom\n2026/01/01 00:00:00 [frontend] info: all fine\rand \x1b[31mred\x07")

	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("logged text is not a single line: %q", out)
	}
	for _, bad := range []string{"\r", "\x1b", "\x07"} {
		if strings.Contains(out, bad) {
			t.Fatalf("control character %q survived sanitising: %q", bad, out)
		}
	}
	if !strings.Contains(out, "[frontend] error: ") {
		t.Fatalf("missing the frontend prefix and level: %q", out)
	}
	if !strings.Contains(out, "boom") || !strings.Contains(out, "red") {
		t.Fatalf("printable text was dropped: %q", out)
	}
}

// A frontend message is held to a quarter of the general cap: it is the one
// source that can produce lines faster than anything else in the app, so a
// burst that gets past the rate limiter must not also be able to spend four
// kilobytes of the log's byte budget per line.
func TestFrontendCapsMessageLength(t *testing.T) {
	resetFrontendLimiter(t, time.Now())
	buf := captureLog(t)

	Frontend("info", strings.Repeat("a", maxMessageBytes*3))

	out := strings.TrimSuffix(buf.String(), "\n")
	msg := strings.TrimPrefix(out, "[frontend] info: ")
	if msg == out {
		t.Fatalf("unexpected log format: %q", out)
	}
	if len(msg) > maxFrontendMessageBytes {
		t.Fatalf("message is %d bytes, want at most %d", len(msg), maxFrontendMessageBytes)
	}
	if !strings.HasSuffix(msg, truncatedMarker) {
		t.Fatalf("truncated message is not marked as such: it ends %q", msg[max(0, len(msg)-40):])
	}
	if maxFrontendMessageBytes >= maxMessageBytes {
		t.Fatalf("the frontend cap (%d) is not tighter than the general one (%d)",
			maxFrontendMessageBytes, maxMessageBytes)
	}
}

// A cap that split a UTF-8 sequence would produce a replacement character and
// make the logged byte length depend on where the cut landed.
func TestTruncateDoesNotSplitRunes(t *testing.T) {
	// "€" is three bytes, so the cut point lands mid-rune for some lengths.
	for extra := range 3 {
		s := strings.Repeat("€", (maxMessageBytes/3)+extra+10)
		got := truncate(s, maxMessageBytes)
		if len(got) > maxMessageBytes {
			t.Fatalf("extra=%d: truncate returned %d bytes, want at most %d", extra, len(got), maxMessageBytes)
		}
		body := strings.TrimSuffix(got, truncatedMarker)
		if strings.ContainsRune(body, '�') {
			t.Fatalf("extra=%d: truncate split a rune", extra)
		}
	}
}

func TestNormalizeLevel(t *testing.T) {
	// A slice rather than a map: several cases are about surrounding whitespace,
	// which a map key hides.
	cases := []struct{ in, want string }{
		{"info", "info"},
		{"warn", "warn"},
		{"error", "error"},
		{"ERROR", "error"},
		{"  Warn ", "warn"},
		{"debug", "info"},
		{"", "info"},
		{"fatal", "info"},
	}
	for _, tc := range cases {
		if got := normalizeLevel(tc.in); got != tc.want {
			t.Errorf("normalizeLevel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- permissions ---

// 0600 used to be applied only when the file was created, so a log left at 0644
// by an older build kept that mode for the life of the install — and rotation's
// os.Rename carried it straight into .1 and .2. These lines hold working
// directories, shell names and dropped file paths.
func TestInitTightensPreExistingPermissions(t *testing.T) {
	prevOut, prevFlags := log.Writer(), log.Flags()
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	configDir := t.TempDir()
	dir := filepath.Join(configDir, logDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("pre-creating %s: %v", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, so a chmod is the only
	// thing that fixes this one.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("pre-setting the directory mode: %v", err)
	}
	path := filepath.Join(dir, logFileName)
	if err := os.WriteFile(path, []byte("from an older build\n"), 0o644); err != nil {
		t.Fatalf("pre-creating %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("pre-setting the file mode: %v", err)
	}

	if _, err := Init(configDir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(closeCurrentWriter)

	if got := statPerm(t, path); got != logPerm {
		t.Fatalf("log mode = %#o after Init, want %#o", got, logPerm)
	}
	if got := statPerm(t, dir); got != 0o700 {
		t.Fatalf("log directory mode = %#o after Init, want %#o", got, 0o700)
	}

	// And the tightened mode is what a rotation then carries into .1.
	mu.Lock()
	w := current
	mu.Unlock()
	if w == nil {
		t.Fatal("Init left no writer behind")
	}
	w.mu.Lock()
	w.rotate()
	w.mu.Unlock()
	if got := statPerm(t, path+".1"); got != logPerm {
		t.Fatalf("rotated generation mode = %#o, want %#o", got, logPerm)
	}
	if got := statPerm(t, path); got != logPerm {
		t.Fatalf("fresh live file mode = %#o, want %#o", got, logPerm)
	}
}

func statPerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// closeCurrentWriter detaches the package-level writer a test's Init installed,
// so a `-count=2` rerun starts from the same state instead of inheriting a
// descriptor on a deleted temp directory.
func closeCurrentWriter() {
	mu.Lock()
	w := current
	current, currentPath = nil, ""
	mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
}

// --- sanitising beyond Cc ---

// Stripping only unicode.Cc left several ways to forge a log line, and none of
// them need anything more exotic than a repository whose directory or script is
// named with one: these strings reach the logger through command names, working
// directories and caught exceptions.
func TestSanitizeDropsInvisibleAndBidiCharacters(t *testing.T) {
	cases := []struct {
		name string
		r    rune
		why  string
	}{
		{"RLO", '\u202e', "reverses the rendered order of the rest of the line"},
		{"LRI", '\u2066', "isolates and reorders the rest of the line"},
		{"PDI", '\u2069', "pairs with an isolate to reorder a span"},
		{"ZWSP", '\u200b', "invisible; breaks up a string someone is grepping for"},
		{"ZWNBSP/BOM", '\ufeff', "invisible"},
		{"LS", '\u2028', "a line break to most viewers, so it splits the entry"},
		{"PS", '\u2029', "a paragraph break to most viewers, so it splits the entry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := "before" + string(tc.r) + "after"
			got := sanitize(in, maxMessageBytes)
			if strings.ContainsRune(got, tc.r) {
				t.Fatalf("U+%04X survived sanitising (%s): %q", tc.r, tc.why, got)
			}
			if got != "beforeafter" {
				t.Fatalf("sanitize(%q) = %q, want %q", in, got, "beforeafter")
			}
		})
	}
}

// The forged-line attack in full, using a directory name rather than a newline:
// U+2028 ends the visible line in most editors and viewers.
func TestFrontendCannotForgeALineWithALineSeparator(t *testing.T) {
	resetFrontendLimiter(t, time.Now())
	buf := captureLog(t)

	Frontend("error", "cd failed\u2028 2026/01/01 00:00:00 [frontend] info: all fine\u202e\u200b")

	out := buf.String()
	for _, bad := range []string{"\u2028", "\u2029", "\u202e", "\u200b"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%q survived sanitising: %q", bad, out)
		}
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("logged text is not a single line: %q", out)
	}
}

// Printable text, including scripts that are genuinely right-to-left, has to
// come through untouched — the sanitiser drops the *formatting* characters, not
// the alphabets that need them.
func TestSanitizeKeepsOrdinaryText(t *testing.T) {
	const in = "cwd=/Users/x/проект/العربية/日本語 exit=1 ✓"
	if got := sanitize(in, maxMessageBytes); got != in {
		t.Fatalf("sanitize mangled ordinary text:\n got %q\nwant %q", got, in)
	}
	// U+FFFD is what invalid UTF-8 decodes to, and it must survive so malformed
	// input shows up rather than vanishing.
	if got := sanitize("a\xffb", maxMessageBytes); got != "a�b" {
		t.Fatalf("sanitize(%q) = %q, want %q", "a\xffb", got, "a�b")
	}
}

// --- the frontend rate limit ---

// resetFrontendLimiter gives one test a fresh bucket and a clock it controls.
// The limiter is package-level state, so without this a test would inherit
// whatever the previous one spent — and `-count=2` would run every case a
// second time against a drained bucket.
func resetFrontendLimiter(t *testing.T, start time.Time) *fakeClock {
	t.Helper()
	prevLimiter, prevClock := frontendLimiter, frontendClock
	t.Cleanup(func() { frontendLimiter, frontendClock = prevLimiter, prevClock })

	clk := &fakeClock{now: start}
	frontendLimiter = newTokenBucket(frontendBurst, frontendRefillPerSec)
	frontendClock = clk.Now
	return clk
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// The measured failure: LogMessage is a binding, so a frontend error handler
// that itself errors can call it in a loop, and 100k lines push every backend
// line out through all three rotations. The log that exists to explain a
// failure then holds nothing but the symptom.
func TestFrontendAllowsABurstThenThrottles(t *testing.T) {
	resetFrontendLimiter(t, time.Unix(1800000000, 0))
	buf := captureLog(t)

	for i := range frontendBurst {
		Frontend("error", fmt.Sprintf("burst %d", i))
		if got := strings.Count(buf.String(), "burst"); got != i+1 {
			t.Fatalf("message %d of the burst was dropped; the log holds %d", i, got)
		}
	}

	before := buf.Len()
	for range 100_000 {
		Frontend("error", "flood")
	}
	if strings.Contains(buf.String()[before:], "[frontend] error: flood") {
		t.Fatalf("a flood past the burst reached the log:\n%s", buf.String()[before:])
	}
}

func TestFrontendBucketRefills(t *testing.T) {
	clk := resetFrontendLimiter(t, time.Unix(1800000000, 0))
	buf := captureLog(t)

	for range frontendBurst + 10 {
		Frontend("info", "drain")
	}
	drained := strings.Count(buf.String(), "[frontend] info: drain")
	if drained != frontendBurst {
		t.Fatalf("the burst let %d messages through, want %d", drained, frontendBurst)
	}

	// One second of quiet buys back exactly frontendRefillPerSec messages, and
	// not one more.
	clk.advance(time.Second)
	for range frontendRefillPerSec + 5 {
		Frontend("info", "after the pause")
	}
	if got := strings.Count(buf.String(), "after the pause"); got != frontendRefillPerSec {
		t.Fatalf("a second of quiet bought %d messages, want %d", got, frontendRefillPerSec)
	}

	// And the bucket never fills past its burst, however long the app idles.
	clk.advance(time.Hour)
	for range frontendBurst + 20 {
		Frontend("info", "after the idle")
	}
	if got := strings.Count(buf.String(), "after the idle"); got != frontendBurst {
		t.Fatalf("an hour of idling bought %d messages, want the burst of %d", got, frontendBurst)
	}
}

// A dropped line is gone, so the log has to say so — otherwise "the frontend
// was quiet" and "the frontend was screaming and we stopped listening" read
// identically afterwards. It must say so at most once a minute, though: a
// summary per dropped message is the same flood by another name.
func TestFrontendSummarisesDroppedMessagesOncePerWindow(t *testing.T) {
	clk := resetFrontendLimiter(t, time.Unix(1800000000, 0))
	buf := captureLog(t)

	const summary = "(rate limited)"
	for range frontendBurst {
		Frontend("error", "burst")
	}
	if strings.Contains(buf.String(), summary) {
		t.Fatal("a summary was logged while nothing had been dropped yet")
	}

	// The first drop says so immediately: that line is the marker that the log
	// is incomplete from here on.
	Frontend("error", "dropped")
	if got := strings.Count(buf.String(), summary); got != 1 {
		t.Fatalf("the first dropped message produced %d summary lines, want 1", got)
	}

	// Then silence for a window, however hard the frontend pushes.
	for range 10_000 {
		Frontend("error", "dropped")
	}
	if got := strings.Count(buf.String(), summary); got != 1 {
		t.Fatalf("%d summary lines inside one window, want 1", got)
	}

	// The next window reports the whole backlog in a single line — and it does
	// so on the next call even though the bucket has refilled by then and that
	// call is allowed through. Flushing only on a drop would mean a flood that
	// stops never gets its count into the log at all.
	clk.advance(frontendDropSummaryGap)
	Frontend("error", "after the flood")
	if got := strings.Count(buf.String(), summary); got != 2 {
		t.Fatalf("the next window produced %d summary lines in total, want 2", got)
	}
	if want := fmt.Sprintf("dropped %d messages (rate limited)", 10_000); !strings.Contains(buf.String(), want) {
		t.Fatalf("the summary does not report the backlog; want a line containing %q, got:\n%s", want, buf.String())
	}
	if !strings.Contains(buf.String(), "[frontend] error: after the flood") {
		t.Fatalf("the message that flushed the summary was itself dropped:\n%s", buf.String())
	}
}

// The bucket is read from whichever goroutine Wails happens to dispatch a
// binding call on, so several LogMessage calls really are concurrent.
func TestFrontendIsSafeForConcurrentUse(t *testing.T) {
	resetFrontendLimiter(t, time.Now())
	frontendClock = time.Now // a real clock: the race detector wants real overlap
	captureLog(t)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				Frontend("error", "concurrent")
			}
		}()
	}
	wg.Wait()
}
