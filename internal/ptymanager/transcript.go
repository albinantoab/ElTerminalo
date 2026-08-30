package ptymanager

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"
)

// ErrTranscriptRunning is returned by StartTranscript when the session is
// already recording. Silently adopting the new path would abandon the file the
// caller believes it is still writing to, so a second start is refused instead.
var ErrTranscriptRunning = errors.New("transcript already running")

// TranscriptStoppedEvent is emitted when a recording ends for a reason the
// frontend did not ask for. It is the only way the pane finds out: a retired
// transcript closes its file and TranscriptPath goes back to "", which looks
// exactly like a recording that was never started — so without this the
// indicator keeps pulsing and the pane later reports a file that stopped
// growing hours earlier.
//
// It is deliberately not emitted for a stop the frontend already knows about:
// an explicit StopTranscript, or the reader's teardown when the pane closes
// (which the frontend hears as pty:exit).
const TranscriptStoppedEvent = "transcript:stopped"

// The reasons a recording retires itself. They are a contract with the
// frontend, which renders a different line for each.
const (
	// TranscriptStopError means a write to the file failed — a full disk, a
	// volume that went away — and the recording was abandoned rather than
	// allowed to stall the pane.
	TranscriptStopError = "error"
	// TranscriptStopCap means the recording reached transcriptMaxBytes.
	TranscriptStopCap = "cap"
)

// TranscriptStopped is the payload of TranscriptStoppedEvent. Path is the file
// as it stands on the disk: complete up to the point it stopped, and closed.
type TranscriptStopped struct {
	SessionID string `json:"sessionID"`
	Path      string `json:"path"`
	Reason    string `json:"reason"`
}

// transcriptBufSize is how much of a recording is held in memory between
// flushes. It is one pty read, so an ordinary stream costs the read loop one
// write(2) per read at worst and usually fewer.
const transcriptBufSize = 64 * 1024

// The two knobs below are vars, not consts, only so the tests can run in
// milliseconds and kilobytes instead of seconds and megabytes.
var (
	// transcriptFlushInterval is how often the buffer above reaches the disk.
	// It is what keeps a recording useful while it is still being written — a
	// user tailing the file, or an agent run being watched — without paying a
	// syscall per read.
	transcriptFlushInterval = 1 * time.Second
	// transcriptMaxBytes caps one recording. An agent run left recording
	// overnight must not fill the disk, and a transcript that stops with a line
	// saying so is a far better outcome than a machine with no space left on it.
	transcriptMaxBytes int64 = 256 << 20
)

// openTranscript creates or extends a transcript file.
//
// Indirected through a var for the same reason emitEvent is: the case this code
// has to survive is a disk that fails mid-recording, and there is no way to
// arrange one for real. A test hands back a writer that fails, or one that
// counts its Close so the fd release can be asserted.
//
// O_APPEND, so starting a recording again on the same path extends it rather
// than throwing the earlier one away. The caller creates the parent directory.
var openTranscript = func(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// transcript is one session's recording of the raw bytes its pty produced.
//
// Everything here is guarded by the Session's transcriptMu except the two
// channels, which are only ever closed — stopCh by the one goroutine that takes
// this transcript out of the session, done by the flusher on its way out.
type transcript struct {
	path string
	f    io.WriteCloser
	w    *bufio.Writer
	// written counts every byte handed to w, header included, because what the
	// cap is protecting is the disk rather than the payload.
	written int64
	// stopCh ends the flusher goroutine; done is closed once it has gone. The
	// pair is what makes "a transcript's ticker cannot outlive its session"
	// something stopTranscript can wait for rather than merely hope for.
	stopCh chan struct{}
	done   chan struct{}
}

// write buffers p and counts it. The error is bufio's, which is sticky: once a
// flush has failed every later call reports it, so checking here is enough.
func (t *transcript) write(p []byte) error {
	n, err := t.w.Write(p)
	t.written += int64(n)
	return err
}

func (t *transcript) writeString(s string) error {
	n, err := t.w.WriteString(s)
	t.written += int64(n)
	return err
}

// transcriptLine renders one of the two lines this package adds to a recording.
// They are the only bytes it adds: everything else in the file is exactly what
// came off the master, escape sequences and control bytes included, because a
// transcript that quietly differs from what the terminal saw is worse than
// useless for working out what an agent run actually did.
//
// The leading newline puts the line on a line of its own however the output
// before it ended — a shell half-way through a prompt when the recording starts
// is the normal case, not the exception.
func transcriptLine(id, verb string, at time.Time, note string) string {
	if note != "" {
		note = " · " + note
	}
	return fmt.Sprintf("\n=== elterminalo transcript · session %s · %s %s%s ===\n",
		shortID(id), verb, at.Format(time.RFC3339), note)
}

// startTranscript opens path and begins recording. It returns the transcript so
// the caller can run its flusher — Manager.StartTranscript is the only caller
// and it always does, which is what makes the join in stopTranscript safe.
func (s *Session) startTranscript(path string) (*transcript, error) {
	s.transcriptMu.Lock()
	defer s.transcriptMu.Unlock()

	if s.transcriptSealed {
		// The reader that feeds a recording has already stopped, so nothing
		// would ever write to this file or close it. The session outlives its
		// reader by a little — the exit path holds it through the attach grace
		// — but from a caller's point of view the pane is gone, which is what
		// this says.
		return nil, ErrSessionNotFound
	}
	if s.transcript != nil {
		return nil, ErrTranscriptRunning
	}

	f, err := openTranscript(path)
	if err != nil {
		return nil, fmt.Errorf("open transcript %s: %w", path, err)
	}

	t := &transcript{
		path:   path,
		f:      f,
		w:      bufio.NewWriterSize(f, transcriptBufSize),
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}

	// The header goes out synchronously, so that StartTranscript answers for the
	// disk rather than leaving the caller with a file that turns out to be
	// unwritable a tick later. This runs on a binding goroutine, never on the
	// read loop, so the write costs nobody's keystrokes anything.
	err = t.writeString(transcriptLine(s.ID, "started", time.Now(), ""))
	if err == nil {
		err = t.w.Flush()
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write transcript %s: %w", path, err)
	}

	s.transcript = t
	log.Printf("ptymanager: session %s transcript started path=%q", shortID(s.ID), path)
	return t, nil
}

// writeTranscript records raw pty output, if this session is being transcribed.
//
// Called by the reader with the bytes still in the read buffer. bufio copies
// them, so the buffer may be reused the moment this returns.
//
// The lock is held across the write. That is what makes a concurrent
// StartTranscript or StopTranscript from a Wails binding goroutine safe, and it
// costs an uncontended mutex per read. The disk is only touched when the 64 KiB
// buffer fills — the ticker is what keeps that from becoming a syscall per read
// — so a slow disk costs the read loop at most one write(2) per 64 KiB, and a
// failing one costs it a single retirement.
func (s *Session) writeTranscript(p []byte) {
	s.transcriptMu.Lock()
	t := s.transcript
	if t == nil {
		s.transcriptMu.Unlock()
		return
	}

	// "" while the recording is still going; one of the two reasons once it has
	// retired itself. The event goes out below, after the lock is dropped — an
	// emit is a hop onto the webview's main thread, and this is the goroutine
	// that drains the pty. Nothing about it needs the lock: t.path is set at
	// construction and never written again.
	reason := ""
	if err := t.write(p); err != nil {
		// Once, and then never again for this recording: the transcript is
		// retired here and the read loop carries straight on. A pane whose
		// stream stopped because a log file could not be written would be a far
		// worse bug than a recording that ends early — a broken transcript is
		// not a broken pane.
		log.Printf("ptymanager: session %s transcript write failed after %d bytes, recording stopped path=%q: %v",
			shortID(s.ID), t.written, t.path, err)
		s.finishTranscriptLocked(t, "")
		reason = TranscriptStopError
	} else if t.written >= transcriptMaxBytes {
		// Checked after the write rather than before it, so the cap is crossed
		// by at most one read — the same overshoot the flow-control marks allow,
		// and for the same reason: splitting a read would mean deciding which
		// half of what the terminal saw to keep.
		log.Printf("ptymanager: session %s transcript reached its %d byte cap after %d bytes, recording stopped path=%q",
			shortID(s.ID), transcriptMaxBytes, t.written, t.path)
		note := fmt.Sprintf("%d byte size cap reached", transcriptMaxBytes)
		s.finishTranscriptLocked(t, transcriptLine(s.ID, "stopped", time.Now(), note))
		reason = TranscriptStopCap
	}
	s.transcriptMu.Unlock()

	if reason != "" {
		s.announceTranscriptStopped(t.path, reason)
	}
}

// announceTranscriptStopped tells the frontend a recording nobody asked to stop
// has been retired. Best effort by design: the manager's seam drops the event
// when there is no window to send it to, which is the same thing every other
// event in this package does.
func (s *Session) announceTranscriptStopped(path, reason string) {
	if s.emit == nil {
		// A session built outside a Manager — the package's own tests do this.
		// There is no runtime to publish into.
		return
	}
	s.emit(TranscriptStoppedEvent, TranscriptStopped{
		SessionID: s.ID,
		Path:      path,
		Reason:    reason,
	})
}

// stopTranscript ends the current recording, if there is one.
//
// seal is set by the reader's teardown and only by it: once the goroutine that
// feeds a transcript has gone there must never be another one, because nothing
// would write to it or close it. See startTranscript.
func (s *Session) stopTranscript(seal bool) {
	s.transcriptMu.Lock()
	if seal {
		s.transcriptSealed = true
	}
	t := s.transcript
	if t != nil {
		s.finishTranscriptLocked(t, transcriptLine(s.ID, "stopped", time.Now(), ""))
	}
	s.transcriptMu.Unlock()

	if t == nil {
		return
	}

	// Outside the lock, and it has to be: the flusher takes transcriptMu to
	// decide whether it is still current, so waiting for it while holding that
	// lock would deadlock. finishTranscriptLocked has already closed stopCh, so
	// it is on its way out rather than waiting for its next tick.
	<-t.done
	log.Printf("ptymanager: session %s transcript stopped path=%q bytes=%d",
		shortID(s.ID), t.path, t.written)
}

// finishTranscriptLocked retires t: no more bytes go into it, its flusher is
// told to stop, and its file is closed. Called with transcriptMu held.
//
// It deliberately does not wait for the flusher, because one of its callers is
// the flusher. stopTranscript does the waiting, outside the lock.
//
// footer is the line to close the file with, or "" when the writer has already
// failed — there is nothing to be gained by pushing more bytes at a writer that
// has just refused some, and the failure has been logged once by the caller.
//
// It emits nothing itself. Two of its three callers retire a recording the
// frontend still believes is running and announce it once the lock is dropped
// (see announceTranscriptStopped); the third is stopTranscript, where the stop
// is either the frontend's own or the pane closing, and both are already known
// on that side.
//
// The file is finished before the session lets go of it, not after. Nothing can
// be writing to it either way — transcriptMu is held throughout — and this
// order is what makes TranscriptPath decisive: "" means the recording is on the
// disk and its fd is back, so a caller that reveals the file the moment a pane
// stops recording cannot beat the footer to it.
func (s *Session) finishTranscriptLocked(t *transcript, footer string) {
	if footer != "" {
		err := t.writeString(footer)
		if err == nil {
			err = t.w.Flush()
		}
		if err != nil {
			log.Printf("ptymanager: session %s could not finish its transcript %q: %v",
				shortID(s.ID), t.path, err)
		}
	}
	if err := t.f.Close(); err != nil {
		log.Printf("ptymanager: session %s could not close its transcript %q: %v",
			shortID(s.ID), t.path, err)
	}

	// Unhooked last, and only ever by the goroutine that finds t still hooked
	// up: taking it out of the session under the same lock is what makes the
	// close below exactly-once, and what makes the reader's next write a no-op.
	s.transcript = nil
	close(t.stopCh)
}

// flushTranscript pushes the buffer to the disk. It reports whether t is still
// the session's current recording and still healthy — false means the flusher
// goroutine's job is over.
func (s *Session) flushTranscript(t *transcript) bool {
	s.transcriptMu.Lock()
	if s.transcript != t {
		s.transcriptMu.Unlock()
		return false
	}
	err := t.w.Flush()
	if err != nil {
		log.Printf("ptymanager: session %s transcript flush failed after %d bytes, recording stopped path=%q: %v",
			shortID(s.ID), t.written, t.path, err)
		s.finishTranscriptLocked(t, "")
	}
	s.transcriptMu.Unlock()

	if err != nil {
		// Outside the lock, as in writeTranscript, and for the same reason.
		s.announceTranscriptStopped(t.path, TranscriptStopError)
		return false
	}
	return true
}

// runTranscriptFlusher puts the recording on the disk on a ticker until the
// recording ends.
//
// The read loop cannot do this itself: it is blocked in read(2) for as long as
// the shell is quiet, which is exactly when an idle pane's last few bytes would
// otherwise sit in the buffer unwritten.
//
// It is registered with the manager's WaitGroup, so CloseAll cannot return
// while a ticker is still running — which is only safe because both of its ways
// out are signalled before that wait. stopCh is closed by whoever ends the
// recording; closingCh is closed at the top of Session.Close, before any rung
// of the shutdown ladder can block, and CloseAll closes every session before it
// waits. Without that second case this goroutine's only stop signal would come
// from the reader's teardown, and a reader that never got there would turn a
// quit into a hang rather than into a leak.
func (s *Session) runTranscriptFlusher(t *transcript) {
	defer close(t.done)

	ticker := time.NewTicker(transcriptFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.stopCh:
			return
		case <-s.closing():
			// The session is being torn down. What is buffered goes to the disk
			// now and this goroutine gets out of the way; the footer and the
			// close stay with the reader's teardown, which runs once the ladder
			// has let the shell say its last words. Finishing the file here
			// instead would cost the recording exactly those words.
			s.flushTranscript(t)
			return
		case <-ticker.C:
			if !s.flushTranscript(t) {
				return
			}
		}
	}
}

// transcriptPath reports the file this session is recording to, or "".
func (s *Session) transcriptPath() string {
	s.transcriptMu.Lock()
	defer s.transcriptMu.Unlock()

	if s.transcript == nil {
		return ""
	}
	return s.transcript.path
}

// StartTranscript begins recording everything a session's pty produces to path.
//
// The file holds the raw stream: every byte exactly as it came off the master,
// escape sequences and control bytes included, bracketed by one header line and
// one footer line and nothing else. Anyone who wants clean text can pipe it
// through a stripper afterwards; a recording that had already been sanitised
// could not be trusted to say what the terminal actually saw.
//
// The parent directory is the caller's to create. Errors: the session is
// unknown (or its reader has already stopped), a transcript is already running,
// or the file could not be opened and its header written.
func (m *Manager) StartTranscript(sessionID, path string) error {
	// The lookup and the Add happen under one hold. CloseAll drains the map
	// under this same lock before it waits, so an Add that gets here at all is
	// ordered before that Wait, and one that would have raced it finds no
	// session to record.
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	if ok {
		m.wg.Add(1)
	}
	m.mu.Unlock()
	if !ok {
		return ErrSessionNotFound
	}

	t, err := session.startTranscript(path)
	if err != nil {
		m.wg.Done()
		return err
	}

	// Registered with the manager's WaitGroup like the reader and the flusher,
	// so CloseAll cannot return while a transcript's ticker is still running.
	// The session's own teardown joins it as well; this is the outer belt.
	go func() {
		defer m.wg.Done()
		session.runTranscriptFlusher(t)
	}()
	return nil
}

// StopTranscript ends a session's recording, flushing and closing the file.
//
// Stopping one that was never started is a no-op, so a caller that has lost
// track of which panes are recording can simply ask for them all to stop. Only
// an unknown session is an error.
func (m *Manager) StopTranscript(sessionID string) error {
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return ErrSessionNotFound
	}

	session.stopTranscript(false)
	return nil
}

// TranscriptPath returns the file a session is recording to, or "" when it is
// not recording — including when the id belongs to no session at all.
func (m *Manager) TranscriptPath(sessionID string) string {
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return ""
	}
	return session.transcriptPath()
}
