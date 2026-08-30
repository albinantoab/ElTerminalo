package ptymanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- transcripts (per-session raw recordings) --------------------------------

// The scripts below all hold their tongue until release() drops a "go" marker,
// so a test can have a recording running before the shell has produced a single
// byte. Without that the assertions would be racing a fork and an exec, and "the
// transcript is missing the first line" would mean nothing.
const (
	waitForGo = "while [ ! -f go ]; do sleep 0.01; done\n"
	// waitsThenSpeaks says three things and then idles.
	waitsThenSpeaks = waitForGo + "echo one\necho two\necho three\n: > ready\n" + idle
	// waitsThenPaints produces a colour escape around a word and no newline at
	// all: nothing here the tty's ONLCR could rewrite, and nothing after it, so
	// a recording of this shell can be compared byte for byte. It is also the
	// point of the whole feature — the escape bytes have to survive.
	waitsThenPaints = waitForGo + "printf '\\033[31mred\\033[0m'\n: > ready\n" + idle
	// waitsThenTicks trickles output for as long as the pane lives, so a test
	// can close the session while the shell is still talking.
	waitsThenTicks = waitForGo + ticks
)

// waitsThenFloods is floods with the same starting gun on it.
var waitsThenFloods = waitForGo + floods

// waitsThenFloodsThenIdles floods and then stays alive, so a test can still ask
// the manager about the session once the whole flood has arrived.
var waitsThenFloodsThenIdles = waitForGo + floods + idle

// paintedBytes is exactly what waitsThenPaints puts on the wire.
const paintedBytes = "\x1b[31mred\x1b[0m"

// shortenTranscript runs the recording knobs on test timescales. Same rules as
// shortenLadder: package vars, no parallel tests, and every read loop finished
// before the test returns — so this must be called before whatever cleanup
// stops those loops, never after.
func shortenTranscript(t testing.TB, flush time.Duration, maxBytes int64) {
	t.Helper()
	origFlush, origMax := transcriptFlushInterval, transcriptMaxBytes
	t.Cleanup(func() { transcriptFlushInterval, transcriptMaxBytes = origFlush, origMax })
	transcriptFlushInterval, transcriptMaxBytes = flush, maxBytes
}

// installTranscriptOpener swaps the seam every recording opens its file
// through. Call it before anything that stops the read loops, so the restore
// unwinds last: a goroutine still writing through a var being written back is a
// race, and one the race detector will find.
func installTranscriptOpener(t testing.TB, open func(path string) (io.WriteCloser, error)) {
	t.Helper()
	original := openTranscript
	t.Cleanup(func() { openTranscript = original })
	openTranscript = open
}

// countingCloser is a real transcript file that says how often it was closed. A
// recording that flushes everything and then leaks its fd passes every content
// assertion there is.
type countingCloser struct {
	io.WriteCloser
	closes *atomic.Int64
}

func (c *countingCloser) Close() error {
	c.closes.Add(1)
	return c.WriteCloser.Close()
}

// failingWriter is a disk that goes away mid-recording: the first failAfter
// writes land and everything after that is an error. Counting writes rather
// than bytes is what puts the failure where a test wants it — the header's
// flush is one write, and the first flush carrying pty output is the next.
type failingWriter struct {
	failAfter int64
	writes    atomic.Int64
	closes    atomic.Int64
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.writes.Add(1) > f.failAfter {
		return 0, errors.New("no space left on device")
	}
	return len(p), nil
}

func (f *failingWriter) Close() error {
	f.closes.Add(1)
	return nil
}

// release lets a waiting test shell get on with it.
func release(t testing.TB, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// readTranscript returns a recording as it currently stands on disk.
func readTranscript(t testing.TB, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the transcript: %v", err)
	}
	return string(body)
}

// transcriptMarker is what both of the lines this package adds start with.
const transcriptMarker = "\n=== elterminalo transcript · session "

// splitTranscriptBody pulls a recording apart into its header line, the raw
// bytes between, and its footer line. ok is false while the file does not yet
// hold a whole header — which is only reachable from a polling loop, since
// StartTranscript does not return until the header is on the disk.
func splitTranscriptBody(body string) (header, payload, footer string, ok bool) {
	if !strings.HasPrefix(body, transcriptMarker) {
		return "", "", "", false
	}
	end := strings.Index(body, " ===\n")
	if end < 0 {
		return "", "", "", false
	}
	header = body[:end+len(" ===\n")]

	rest := body[len(header):]
	// The last marker is the footer's, if the recording has been closed off.
	if start := strings.LastIndex(rest, transcriptMarker); start >= 0 {
		return header, rest[:start], rest[start:], true
	}
	return header, rest, "", true
}

// splitTranscript is splitTranscriptBody for a file that must already be well
// formed.
func splitTranscript(t testing.TB, body string) (header, payload, footer string) {
	t.Helper()
	header, payload, footer, ok := splitTranscriptBody(body)
	if !ok {
		t.Fatalf("the recording does not start with a header line: %q", body[:min(len(body), 160)])
	}
	return header, payload, footer
}

// waitForTranscript polls a recording until it holds every want in order, and
// fails with what it did hold if it never does. Polling, because the buffer
// only reaches the disk on the flush ticker.
func waitForTranscript(t testing.TB, path string, want []string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		body, err := os.ReadFile(path)
		if err == nil && containsInOrder(string(body), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the transcript at %s never held %v in order; it holds %q (err=%v)", path, want, body, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForTranscriptPayload polls until a recording holds at least n bytes of
// pty output, its own two lines excluded, and returns them.
func waitForTranscriptPayload(t testing.TB, path string, n int, why string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		body, err := os.ReadFile(path)
		if err == nil {
			if _, payload, _, ok := splitTranscriptBody(string(body)); ok && len(payload) >= n {
				return payload
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the transcript at %s holds %d bytes, want at least %d: %s", path, len(body), n, why)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForTranscriptStopped polls until the session has published at least one
// transcript:stopped event, and returns every one it has published.
func waitForTranscriptStopped(t testing.TB, rec *emitRecorder, id, why string) []TranscriptStopped {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if stops := rec.transcriptStopsOf(id); len(stops) > 0 {
			return stops
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s never published %s: %s", id, TranscriptStoppedEvent, why)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForRecordingToStop polls until the session is no longer transcribing,
// which is how a recording that retires itself — a failed write, the size cap —
// announces it.
func waitForRecordingToStop(t testing.TB, m *Manager, id, why string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if m.TranscriptPath(id) == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s is still recording to %q: %s", id, m.TranscriptPath(id), why)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForPaneOutput polls until the pane has received want. That is the
// earliest moment at which the reader has certainly taken those bytes off the
// pty, which is what an assertion about the recording has to be synchronised
// against.
func waitForPaneOutput(t testing.TB, rec *emitRecorder, id, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if strings.Contains(string(rec.outputOf(id)), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pane never received %q; it received %q", want, rec.outputOf(id))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForMoreOutput blocks until the pane emits something it had not already,
// which is how these tests ask "is this pane still alive" after doing something
// unpleasant to its transcript.
func waitForMoreOutput(t testing.TB, rec *emitRecorder, id, why string) {
	t.Helper()
	before := rec.outputLen(id)
	for deadline := time.Now().Add(20 * time.Second); rec.outputLen(id) <= before; {
		if time.Now().After(deadline) {
			t.Fatal(why)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A recording is the raw stream, byte for byte. What a transcript is for is
// working out what an agent run actually did, and one that quietly differs from
// what the terminal saw cannot answer that — so escape sequences and control
// bytes go in exactly as they came off the master, and the only bytes this
// package adds are the two lines bracketing the file.
func TestTranscriptRecordsTheRawBytesTheShellProduced(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	shortenTranscript(t, 10*time.Millisecond, transcriptMaxBytes)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, waitsThenPaints), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !m.AttachSession(id) {
		t.Fatal("AttachSession returned false for a session that was just created")
	}

	path := filepath.Join(t.TempDir(), "session.log")
	if err := m.StartTranscript(id, path); err != nil {
		t.Fatalf("StartTranscript: %v", err)
	}
	if got := m.TranscriptPath(id); got != path {
		t.Errorf("TranscriptPath() = %q while recording, want %q", got, path)
	}

	// Only now does the shell get to speak, so nothing can have been produced
	// before the recording started.
	release(t, dir)
	waitForPaneOutput(t, rec, id, paintedBytes)

	if err := m.StopTranscript(id); err != nil {
		t.Fatalf("StopTranscript: %v", err)
	}
	if got := m.TranscriptPath(id); got != "" {
		t.Errorf("TranscriptPath() = %q after StopTranscript, want %q", got, "")
	}

	header, payload, footer := splitTranscript(t, readTranscript(t, path))
	if payload != paintedBytes {
		t.Errorf("the recording holds %q, want %q byte for byte: the escape sequences are the point", payload, paintedBytes)
	}
	if !strings.Contains(header, "started ") {
		t.Errorf("header = %q, want a started line", header)
	}
	if !strings.Contains(footer, "stopped ") {
		t.Errorf("footer = %q, want a stopped line", footer)
	}
	if !strings.Contains(header, shortID(id)) || !strings.Contains(footer, shortID(id)) {
		t.Errorf("the recording does not name its session: header=%q footer=%q", header, footer)
	}
	// The pane and the file have to agree, or one of them is not the terminal's
	// output.
	if got := string(rec.outputOf(id)); got != payload {
		t.Errorf("the pane received %q and the recording holds %q", got, payload)
	}
}

// A recording must not depend on a pane ever subscribing. The frontend attaches
// a full round trip after CreateSession returns, and a headless agent run may
// never attach at all; a transcript that only started once somebody was
// watching would miss exactly the output nobody was there to see.
func TestTranscriptDoesNotDependOnAttach(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	shortenTranscript(t, 10*time.Millisecond, transcriptMaxBytes)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, waitsThenSpeaks), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Deliberately never attached.

	path := filepath.Join(t.TempDir(), "unattached.log")
	if err := m.StartTranscript(id, path); err != nil {
		t.Fatalf("StartTranscript: %v", err)
	}
	release(t, dir)
	waitForReady(t, dir)

	waitForTranscript(t, path, []string{"one", "two", "three"})
	if got := rec.outputLen(id); got != 0 {
		t.Fatalf("precondition failed: the pane emitted %d bytes without ever attaching, so this says nothing about a recording surviving without one", got)
	}

	if err := m.StopTranscript(id); err != nil {
		t.Fatalf("StopTranscript: %v", err)
	}
	_, payload, footer := splitTranscript(t, readTranscript(t, path))
	if !containsInOrder(payload, []string{"one", "two", "three"}) {
		t.Errorf("the recording holds %q, want the three lines in order", payload)
	}
	if !strings.Contains(footer, "stopped ") {
		t.Errorf("footer = %q, want a stopped line", footer)
	}
}

// Flow control parks the reader when the frontend falls behind, and the pane
// stops receiving. The recording must not: it is written before the credit is
// charged, so everything the reader takes off the pty reaches the disk whether
// or not anybody ever acknowledges a byte of it.
func TestTranscriptKeepsRecordingWhileTheReaderIsPaused(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	shortenTranscript(t, 10*time.Millisecond, transcriptMaxBytes)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, waitsThenFloods), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	path := filepath.Join(t.TempDir(), "flood.log")
	if err := m.StartTranscript(id, path); err != nil {
		t.Fatalf("StartTranscript: %v", err)
	}
	// Never attached and never acked, so the reader runs up to the high-water
	// mark and parks there with a megabyte it has already read.
	release(t, dir)

	session := sessionOf(t, m, id)
	unacked := waitUntilPaused(t, session, rec, id)
	if unacked < highWater {
		t.Fatalf("precondition failed: the reader parked at unacked=%d, below the %d high-water mark", unacked, highWater)
	}

	payload := waitForTranscriptPayload(t, path, highWater,
		"the reader read a high-water mark's worth before parking, and every byte of it belongs in the recording without a single ack")
	if i := strings.IndexFunc(payload, func(r rune) bool { return r != 'x' }); i >= 0 {
		t.Errorf("the recording is corrupt at offset %d: %q", i, payload[i:min(len(payload), i+32)])
	}

	if err := m.StopTranscript(id); err != nil {
		t.Fatalf("StopTranscript: %v", err)
	}
}

// A quit must leave a complete file behind. CloseAll waits on the same
// WaitGroup the reader is registered with, and the reader's teardown is what
// finishes the recording — so once CloseAll has returned the tail is on the
// disk, the footer is written and the fd is back.
func TestCloseAllFinishesAndClosesTheTranscript(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	shortenTranscript(t, 10*time.Millisecond, transcriptMaxBytes)

	var closes atomic.Int64
	installTranscriptOpener(t, func(path string) (io.WriteCloser, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		return &countingCloser{WriteCloser: f, closes: &closes}, nil
	})

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, waitsThenTicks), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !m.AttachSession(id) {
		t.Fatal("AttachSession returned false for a session that was just created")
	}

	path := filepath.Join(t.TempDir(), "quit.log")
	if err := m.StartTranscript(id, path); err != nil {
		t.Fatalf("StartTranscript: %v", err)
	}
	release(t, dir)
	// Closed while the shell is still talking, which is what a Cmd-Q looks like.
	waitForPaneOutput(t, rec, id, "tick")

	m.CloseAll()

	// Read once, immediately: whatever is on the disk now is what a quit leaves
	// behind, and no later flush is coming to rescue it.
	_, payload, footer := splitTranscript(t, readTranscript(t, path))
	if !strings.Contains(footer, "stopped ") {
		t.Errorf("the recording has no stopped line after CloseAll: %q", footer)
	}
	// Everything the pane was shown came off the pty before it was emitted, so
	// the pane's stream has to be a prefix of the recording. Anything less is a
	// missing tail.
	if emitted := string(rec.outputOf(id)); !strings.HasPrefix(payload, emitted) {
		t.Errorf("the recording is missing output the pane was shown: it holds %d bytes, the pane received %d", len(payload), len(emitted))
	}
	if got := closes.Load(); got != 1 {
		t.Errorf("the transcript file was closed %d times by the end of CloseAll, want exactly 1: an fd per pane is a leak, and a double close is a bug", got)
	}
	if got := m.TranscriptPath(id); got != "" {
		t.Errorf("TranscriptPath() = %q for a session CloseAll has finished with", got)
	}
}

// The start/stop contract the app's bindings are written against.
func TestTranscriptStartStopContract(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	shortenTranscript(t, 10*time.Millisecond, transcriptMaxBytes)

	m := NewManager(writeTestShell(t, idle), t.TempDir())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForReady(t, dir)

	logs := t.TempDir()
	path := filepath.Join(logs, "session.log")
	other := filepath.Join(logs, "other.log")
	missing := filepath.Join(logs, "no-such-directory", "session.log")
	stranger := id + "-not-a-session"

	if err := m.StartTranscript(stranger, path); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("StartTranscript on an unknown session = %v, want %v", err, ErrSessionNotFound)
	}
	if err := m.StopTranscript(stranger); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("StopTranscript on an unknown session = %v, want %v", err, ErrSessionNotFound)
	}
	if got := m.TranscriptPath(stranger); got != "" {
		t.Errorf("TranscriptPath on an unknown session = %q, want %q", got, "")
	}

	if got := m.TranscriptPath(id); got != "" {
		t.Errorf("TranscriptPath before any recording = %q, want %q", got, "")
	}
	if err := m.StopTranscript(id); err != nil {
		t.Errorf("StopTranscript with nothing running = %v, want nil: a caller that has lost track has to be able to stop them all", err)
	}

	// The parent directory is the caller's to create, and a path that cannot be
	// opened is reported rather than swallowed.
	if err := m.StartTranscript(id, missing); err == nil {
		t.Error("StartTranscript into a directory that does not exist returned nil")
	}
	if got := m.TranscriptPath(id); got != "" {
		t.Errorf("TranscriptPath = %q after a StartTranscript that failed, want %q", got, "")
	}

	if err := m.StartTranscript(id, path); err != nil {
		t.Fatalf("StartTranscript: %v", err)
	}
	if err := m.StartTranscript(id, other); !errors.Is(err, ErrTranscriptRunning) {
		t.Errorf("a second StartTranscript = %v, want %v", err, ErrTranscriptRunning)
	}
	if got := m.TranscriptPath(id); got != path {
		t.Errorf("TranscriptPath = %q after a refused second start, want the first path %q", got, path)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Errorf("a refused StartTranscript created %s anyway (err=%v)", other, err)
	}

	if err := m.StopTranscript(id); err != nil {
		t.Errorf("StopTranscript = %v, want nil", err)
	}
	if err := m.StopTranscript(id); err != nil {
		t.Errorf("a second StopTranscript = %v, want nil: stopping twice is a no-op", err)
	}

	// Recording to the same path again extends the file rather than throwing
	// the first run away.
	if err := m.StartTranscript(id, path); err != nil {
		t.Fatalf("StartTranscript after a stop: %v", err)
	}
	if err := m.StopTranscript(id); err != nil {
		t.Fatalf("StopTranscript: %v", err)
	}
	body := readTranscript(t, path)
	if got := strings.Count(body, "started "); got != 2 {
		t.Errorf("the file holds %d started lines after two recordings, want 2 — O_APPEND, not O_TRUNC: %q", got, body)
	}
	if got := strings.Count(body, "stopped "); got != 2 {
		t.Errorf("the file holds %d stopped lines after two recordings, want 2: %q", got, body)
	}
}

// A disk that fills up or goes away must cost the recording and nothing else.
// This is the half of that discovered by the flush ticker — an idle-ish pane
// whose few buffered bytes only reach the disk on the tick. The read loop's own
// half is the test below it.
func TestTranscriptFlushFailureLeavesThePaneStreaming(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	shortenTranscript(t, 10*time.Millisecond, transcriptMaxBytes)

	// The header's flush is the one write that lands; the first flush carrying
	// the shell's output is refused.
	disk := &failingWriter{failAfter: 1}
	installTranscriptOpener(t, func(string) (io.WriteCloser, error) { return disk, nil })

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, waitsThenTicks), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !m.AttachSession(id) {
		t.Fatal("AttachSession returned false for a session that was just created")
	}

	path := filepath.Join(t.TempDir(), "doomed.log")
	if err := m.StartTranscript(id, path); err != nil {
		t.Fatalf("StartTranscript: %v", err)
	}
	release(t, dir)

	waitForRecordingToStop(t, m, id, "the disk it is writing to has refused every write since the header")
	if got := disk.closes.Load(); got != 1 {
		t.Errorf("the failed transcript was closed %d times, want exactly 1: a recording that gives up still has to give its fd back", got)
	}

	// The whole point: the pane never noticed.
	waitForMoreOutput(t, rec, id, "the pane stopped streaming when its transcript failed; a broken transcript is not a broken pane")

	// And it can be stopped like any other, and started again.
	if err := m.StopTranscript(id); err != nil {
		t.Errorf("StopTranscript after a failed recording = %v, want nil", err)
	}
}

// The other half, and the one that matters most: the failure discovered by the
// read loop itself.
//
// A recording is buffered, so most reads cost nothing — but the read that fills
// the buffer performs the write(2), on the goroutine that drains the pty. A
// failure propagated out of there would stop the reader, the tty buffer would
// fill, and the shell would block in write(2): a pane frozen because a log file
// could not be written. It has to cost the recording and nothing else — the
// whole flood still reaches the pane, to the byte.
func TestTranscriptWriteFailureOnTheReadLoopDoesNotStallThePane(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	// A flush interval longer than this test could possibly run, so the ticker
	// cannot be the one to find the broken disk. The only writer left is the
	// read loop, which is the point. (It doubles as a check that the ticker
	// goroutine is not what CloseAll waits for: an hour is a long quit.)
	shortenTranscript(t, time.Hour, transcriptMaxBytes)

	disk := &failingWriter{failAfter: 1} // the header lands, nothing after it
	installTranscriptOpener(t, func(string) (io.WriteCloser, error) { return disk, nil })

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, waitsThenFloodsThenIdles), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !m.AttachSession(id) {
		t.Fatal("AttachSession returned false for a session that was just created")
	}
	if err := m.StartTranscript(id, filepath.Join(t.TempDir(), "doomed.log")); err != nil {
		t.Fatalf("StartTranscript: %v", err)
	}
	release(t, dir)

	// Acking as it arrives, exactly as the frontend does, so the only thing that
	// could hold the flood up is the transcript.
	acked := 0
	for deadline := time.Now().Add(60 * time.Second); ; {
		got := rec.outputLen(id)
		if got >= floodBytes {
			break
		}
		if got > acked {
			m.AckOutput(id, got-acked)
			acked = got
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d bytes reached the pane after the transcript's disk failed (%d writes attempted); the read loop is stalled on a log file",
				got, floodBytes, disk.writes.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := rec.outputLen(id); got != floodBytes {
		t.Errorf("the pane received %d bytes, want exactly %d: a broken transcript must not cost the pane a byte", got, floodBytes)
	}

	if got := disk.writes.Load(); got < 2 {
		t.Fatalf("the read loop only ever attempted %d writes, so the buffer never filled and the failure this is about never happened", got)
	}
	if got := m.TranscriptPath(id); got != "" {
		t.Errorf("the session is still recording to %q on a disk that has refused every write since the header", got)
	}
	if got := disk.closes.Load(); got != 1 {
		t.Errorf("the failed transcript was closed %d times, want exactly 1: a recording that gives up still has to give its fd back", got)
	}
}

// An agent run left recording overnight must not fill the disk. The recording
// stops at the cap, says so in the file, and the pane carries on.
func TestTranscriptStopsAtItsSizeCap(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	const sizeCap = 128 * 1024
	shortenTranscript(t, 10*time.Millisecond, sizeCap)

	installRecorder(t)
	m := NewManager(writeTestShell(t, waitsThenFloods), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	path := filepath.Join(t.TempDir(), "capped.log")
	if err := m.StartTranscript(id, path); err != nil {
		t.Fatalf("StartTranscript: %v", err)
	}
	// Nothing attaches and nothing acks, so the reader still runs to the
	// high-water mark — eight times this cap — and it is the cap that stops the
	// recording rather than the shell running out of things to say.
	release(t, dir)

	waitForRecordingToStop(t, m, id, "the flood is many times the size cap")

	body := readTranscript(t, path)
	if len(body) < sizeCap {
		t.Errorf("the recording stopped at %d bytes, before its %d byte cap", len(body), sizeCap)
	}
	// One read past the mark at worst, plus the line that says why.
	if limit := sizeCap + readBufSize + 512; len(body) > limit {
		t.Errorf("the recording ran to %d bytes with a %d byte cap (limit %d): it has to stop within one read of the mark", len(body), sizeCap, limit)
	}
	_, _, footer := splitTranscript(t, body)
	if !strings.Contains(footer, "cap") {
		t.Errorf("the recording ends with %q; a file that stops at the cap has to say so", footer)
	}
}

// The bindings run on a Wails goroutine and the reader runs on its own, so
// starting and stopping a recording races the writing of it by construction.
// Under -race, with a shell talking throughout.
func TestTranscriptStartAndStopAreSafeWhileTheShellIsTalking(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	shortenTranscript(t, 5*time.Millisecond, transcriptMaxBytes)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, waitsThenTicks), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !m.AttachSession(id) {
		t.Fatal("AttachSession returned false for a session that was just created")
	}
	release(t, dir)
	waitForPaneOutput(t, rec, id, "tick")

	logs := t.TempDir()
	stop := make(chan struct{})
	var workers sync.WaitGroup
	for i := range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			// Every worker ends its iteration with a stop, so once they have all
			// finished nothing is left recording — whichever of them owned it.
			path := filepath.Join(logs, fmt.Sprintf("worker-%d.log", i))
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = m.StartTranscript(id, path)
				_ = m.TranscriptPath(id)
				m.AckOutput(id, 64)
				_ = m.StopTranscript(id)
				time.Sleep(time.Millisecond)
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	workers.Wait()

	if got := m.TranscriptPath(id); got != "" {
		t.Errorf("TranscriptPath = %q after every worker stopped, want %q", got, "")
	}
	waitForMoreOutput(t, rec, id, "the pane stopped streaming while its transcript was being started and stopped")

	// Every file has to be a well formed recording, however many times it was
	// extended and whoever it was that stopped it.
	entries, err := os.ReadDir(logs)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no recordings were written at all")
	}
	for _, entry := range entries {
		body := readTranscript(t, filepath.Join(logs, entry.Name()))
		started, stopped := strings.Count(body, "started "), strings.Count(body, "stopped ")
		if started != stopped {
			t.Errorf("%s holds %d started lines and %d stopped ones: every recording has to be closed off", entry.Name(), started, stopped)
		}
	}
}

// --- transcript:stopped (a recording that ends without being asked to) -------

// A recording retired by its size cap has to say so.
//
// Nothing else can. The transcript closes its file and TranscriptPath goes back
// to "", which is indistinguishable from a pane that was never recording — so
// without the event the frontend's indicator keeps pulsing, and the line it
// prints when the user finally stops "recording" names a file that stopped
// growing hours earlier.
func TestTranscriptStoppedEventReportsTheSizeCap(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	const sizeCap = 128 * 1024
	shortenTranscript(t, 10*time.Millisecond, sizeCap)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, waitsThenFloods), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	path := filepath.Join(t.TempDir(), "capped.log")
	if err := m.StartTranscript(id, path); err != nil {
		t.Fatalf("StartTranscript: %v", err)
	}
	release(t, dir)

	waitForRecordingToStop(t, m, id, "the flood is many times the size cap")
	stops := waitForTranscriptStopped(t, rec, id,
		"a recording that hits its cap is retired behind the frontend's back, and this event is the only thing that says so")

	if len(stops) != 1 {
		t.Fatalf("the cap published %d events, want exactly 1: %+v", len(stops), stops)
	}
	if got := stops[0].Reason; got != TranscriptStopCap {
		t.Errorf("reason = %q, want %q", got, TranscriptStopCap)
	}
	if got := stops[0].Path; got != path {
		t.Errorf("path = %q, want %q: the frontend has nothing else to name the file with", got, path)
	}
	if got := stops[0].SessionID; got != id {
		t.Errorf("sessionID = %q, want %q", got, id)
	}

	// The user stopping a recording that has already retired itself must not
	// produce a second one.
	if err := m.StopTranscript(id); err != nil {
		t.Fatalf("StopTranscript: %v", err)
	}
	if got := rec.transcriptStopsOf(id); len(got) != 1 {
		t.Errorf("%d events after a StopTranscript that found nothing running, want 1: %+v", len(got), got)
	}
}

// The same for a recording the disk refuses — discovered here by the flush
// ticker, which is the half an idle-ish pane hits.
func TestTranscriptStoppedEventReportsAFlushFailure(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	shortenTranscript(t, 10*time.Millisecond, transcriptMaxBytes)

	// The header's flush is the one write that lands; the first flush carrying
	// the shell's output is refused.
	disk := &failingWriter{failAfter: 1}
	installTranscriptOpener(t, func(string) (io.WriteCloser, error) { return disk, nil })

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, waitsThenTicks), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !m.AttachSession(id) {
		t.Fatal("AttachSession returned false for a session that was just created")
	}

	path := filepath.Join(t.TempDir(), "doomed.log")
	if err := m.StartTranscript(id, path); err != nil {
		t.Fatalf("StartTranscript: %v", err)
	}
	release(t, dir)

	stops := waitForTranscriptStopped(t, rec, id,
		"the disk has refused every write since the header, and the pane is still being told it is recording")
	if got := stops[0].Reason; got != TranscriptStopError {
		t.Errorf("reason = %q, want %q", got, TranscriptStopError)
	}
	if got := stops[0].Path; got != path {
		t.Errorf("path = %q, want %q", got, path)
	}
	if got := stops[0].SessionID; got != id {
		t.Errorf("sessionID = %q, want %q", got, id)
	}
}

// And a stop the frontend already knows about publishes nothing. Both shapes:
// the user asking for it, and the pane closing under a live recording — the
// second of which the frontend hears as pty:exit.
//
// The event is not decoration. A frontend that saw one here would report a
// recording as having failed every time a pane was closed, which is the same
// lie in the other direction.
func TestNoTranscriptStoppedEventWhenTheStopWasAskedFor(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	shortenTranscript(t, 10*time.Millisecond, transcriptMaxBytes)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, waitsThenTicks), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	logs := t.TempDir()

	// One pane the user stops by hand.
	stoppedDir := t.TempDir()
	stopped, err := m.CreateSession(80, 24, stoppedDir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !m.AttachSession(stopped) {
		t.Fatal("AttachSession returned false for a session that was just created")
	}
	if err := m.StartTranscript(stopped, filepath.Join(logs, "stopped.log")); err != nil {
		t.Fatalf("StartTranscript: %v", err)
	}
	release(t, stoppedDir)
	waitForPaneOutput(t, rec, stopped, "tick")
	if err := m.StopTranscript(stopped); err != nil {
		t.Fatalf("StopTranscript: %v", err)
	}

	// And one closed with the recording still running, which is the reader's
	// teardown finishing the file.
	closedDir := t.TempDir()
	closed, err := m.CreateSession(80, 24, closedDir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !m.AttachSession(closed) {
		t.Fatal("AttachSession returned false for a session that was just created")
	}
	if err := m.StartTranscript(closed, filepath.Join(logs, "closed.log")); err != nil {
		t.Fatalf("StartTranscript: %v", err)
	}
	release(t, closedDir)
	waitForPaneOutput(t, rec, closed, "tick")
	m.CloseSession(closed)
	rec.waitForExit(t, closed, 20*time.Second)

	for _, id := range []string{stopped, closed} {
		if got := rec.transcriptStopsOf(id); len(got) != 0 {
			t.Errorf("session %s published %+v; a stop the frontend asked for, or a pane it watched close, is not news", id, got)
		}
	}
}

// --- the two invariants the recording rests on -------------------------------

// The tap is taken at the top of the read path, before the batch is handed to
// the flusher — see readPTY. Those bytes are already off the master, so a
// recording that waited for the hand-off to succeed would be missing whatever
// the reader is holding at the moment the flusher stops draining: a flusher that
// died on a panic releases the reader through `stop` with a batch still in its
// hand, and that batch is exactly what the pane never saw and the transcript is
// supposed to have.
//
// It cannot be seen through the Manager. Flow control parks the reader at
// highWater (1 MiB) long before dataCh (64 buffers) can fill, so in ordinary
// running the hand-off never blocks and the tap runs before the next read either
// way — which is why moving it below the select passes the rest of this suite.
// So this drives readPTY directly against a consumer that never receives, which
// is that failure in miniature: the reader is stopped at the hand-off, and the
// recording has to already hold what it is stopped holding.
func TestTranscriptIsTakenBeforeTheHandOffToTheFlusher(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	shortenTranscript(t, 10*time.Millisecond, transcriptMaxBytes)

	const spoken = "tapped"
	dir := t.TempDir()
	s := startTestShell(t, dir, waitForGo+"printf '"+spoken+"'\n"+idle)

	path := filepath.Join(t.TempDir(), "handoff.log")
	tr, err := s.startTranscript(path)
	if err != nil {
		t.Fatalf("startTranscript: %v", err)
	}
	go s.runTranscriptFlusher(tr)

	// Unbuffered, and nothing ever receives from it: the reader gets one read in
	// and then stops at the hand-off, holding it.
	dataCh := make(chan []byte)
	doneCh := make(chan struct{})
	stop := make(chan struct{})
	m := NewManager("", t.TempDir())
	m.wg.Add(1)
	go m.readPTY(s, dataCh, doneCh, stop)
	// Registered after startTestShell's own cleanup so it unwinds first: the
	// reader has to be released before the session is torn down, or its teardown
	// — which is what closes the recording off — never runs.
	t.Cleanup(func() {
		close(stop)
		<-doneCh
	})

	release(t, dir)

	waitForTranscript(t, path, []string{spoken})
}

// runTranscriptFlusher's second way out — `case <-s.closing():` — is what keeps
// a quit from hanging. The recording's own stopCh is closed by whoever ends the
// recording, and in production that is the reader's teardown; the ticker is
// registered with the manager's WaitGroup, which CloseAll waits on. So for a
// session whose reader never reaches its teardown, that arm is the difference
// between a quit that completes and a quit that never returns.
//
// A session with no reader at all is that case in its simplest form, and it
// pins the arm's behaviour as well as its existence: the flush it does on the
// way out is what puts a quiet pane's last buffered bytes on the disk.
func TestTranscriptFlusherStopsWhenTheSessionCloses(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	// A flush interval longer than this test could possibly run, so the ticker
	// can be neither what ends the goroutine nor what puts the bytes below on
	// the disk. The closing arm has to do both.
	shortenTranscript(t, time.Hour, transcriptMaxBytes)

	dir := t.TempDir()
	s := startTestShell(t, dir, idle)
	waitForReady(t, dir)

	path := filepath.Join(t.TempDir(), "closing.log")
	tr, err := s.startTranscript(path)
	if err != nil {
		t.Fatalf("startTranscript: %v", err)
	}
	go s.runTranscriptFlusher(tr)

	const buffered = "buffered-when-the-session-closed"
	s.writeTranscript([]byte(buffered))
	if body := readTranscript(t, path); strings.Contains(body, buffered) {
		t.Fatalf("precondition failed: the bytes reached the disk before the close, so this says nothing about the closing arm: %q", body)
	}

	s.Close()

	select {
	case <-tr.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the transcript flusher was still running 10s after Session.Close; it is registered with the manager's WaitGroup, so CloseAll would never return")
	}
	if body := readTranscript(t, path); !strings.Contains(body, buffered) {
		t.Errorf("the recording holds %q; the bytes buffered when the session closed never reached the disk", body)
	}

	// Still current on the session by design — the footer and the close belong
	// to the reader's teardown, which this session never had — so finish it here
	// rather than leaving the fd open.
	s.stopTranscript(false)
}
