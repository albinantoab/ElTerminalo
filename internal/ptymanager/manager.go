package ptymanager

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"runtime/debug"
	"sync"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	// readBufSize is one pty read. 4 KiB meant a shell dumping a large file paid
	// a syscall every four kilobytes and woke the flusher often enough that the
	// 16 ms batching barely coalesced anything.
	readBufSize = 64 * 1024
	// batchChannelSize is how many reads may sit between the reader and the
	// flusher. Flow control bounds the real depth long before this does: the
	// reader parks at highWater, so at most highWater/readBufSize buffers can
	// ever be queued here.
	batchChannelSize = 64
	// batchAccumCap is the batch buffer's starting size — one read, so an
	// ordinary stream never reallocates on its first append.
	batchAccumCap = readBufSize
	// batchInterval is the flusher's cadence: output produced within the same
	// 16 ms goes out as one event rather than one per read.
	batchInterval = 16 * time.Millisecond
	// maxEventBytes caps a single pty:output event, and doubles as the size at
	// which the flusher stops waiting for the next tick. Each event is an
	// evaluateJavaScript hop onto the webview's main thread: one multi-megabyte
	// string there is a visible stall, several bounded ones are not.
	maxEventBytes = 256 * 1024
	// accumShrinkBytes is the size above which the batch buffer is handed back to
	// the collector instead of reused. A steady stream tops out around
	// maxEventBytes plus one read, so only a buffer grown by an unattached or
	// throttled pane gets here — and 17 panes each holding on to a megabyte for
	// the rest of the session is not a trade worth making for one allocation.
	accumShrinkBytes = 2 * maxEventBytes

	// highWater and lowWater are the flow-control marks, counted in raw bytes the
	// frontend has not acknowledged. See the flow-control block on Session for
	// what they gate and why there are two of them.
	highWater = 1024 * 1024
	lowWater  = 256 * 1024
)

// ErrSessionNotFound is returned when an operation targets a session that does not exist.
var ErrSessionNotFound = errors.New("session not found")

// attachGrace bounds how long a dead shell's last output is held for a pane
// that has not attached yet. A var, not a const, only so the tests can run the
// wait in milliseconds instead of seconds. See the exit path in readLoop for
// what it buys and what it costs.
var attachGrace = 3 * time.Second

// PtyExit is the payload of the pty:exit:<sessionID> event. ExitCode is the
// shell's own exit status, or -1 when a signal ended it — in which case Signal
// names the signal and is otherwise empty. Closing a pane produces either shape:
// zsh catches the hangup and exits 1, while a shell that does not handle it is
// reported as (-1, "SIGHUP").
//
// ExitCode -1 with an empty Signal is the one remaining case: status unknown.
// Every rung of the shutdown ladder is bounded, so a shell that outlives SIGKILL
// leaves nothing to report; the frontend renders that as "status unknown" rather
// than as an exit code or a signal.
type PtyExit struct {
	ExitCode int    `json:"exitCode"`
	Signal   string `json:"signal"`
}

// SessionStatus describes the current state of a PTY session.
type SessionStatus struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Command   string `json:"command"`
	IsIdle    bool   `json:"isIdle"`
}

// emitEvent publishes a Wails event. It is indirected through a var so the
// tests can observe what readLoop publishes: the order of the exit event
// against the session's removal from the map is load-bearing for the frontend
// (see readLoop), and there is no live Wails runtime in a test to assert it
// against.
var emitEvent = func(ctx context.Context, event string, data ...interface{}) {
	wailsRuntime.EventsEmit(ctx, event, data...)
}

// Manager manages multiple PTY sessions and streams output via Wails events.
type Manager struct {
	ctx       context.Context
	shell     string
	configDir string
	sessions  map[string]*Session
	mu        sync.Mutex
	wg        sync.WaitGroup
}

// NewManager creates a new PTY manager.
func NewManager(shell, configDir string) *Manager {
	return &Manager{
		shell:     shell,
		configDir: configDir,
		sessions:  make(map[string]*Session),
	}
}

// SetContext sets the Wails runtime context (called during app startup).
func (m *Manager) SetContext(ctx context.Context) {
	m.ctx = ctx
}

// CreateSession spawns a new PTY and starts streaming output.
func (m *Manager) CreateSession(cols, rows int, cwd string) (string, error) {
	return m.CreateSessionWithShell(m.shell, cols, rows, cwd)
}

// CreateSessionWithShell is CreateSession with an explicit shell. The settings
// file can name a shell, and a change there should reach the next pane rather
// than wait for a relaunch, so the app resolves it per session and passes it
// in; the Manager's own shell stays the default for callers that don't care.
func (m *Manager) CreateSessionWithShell(shell string, cols, rows int, cwd string) (string, error) {
	if shell == "" {
		shell = m.shell
	}
	session, err := NewSession(shell, m.configDir, cols, rows, cwd)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	m.wg.Add(1)
	go m.readLoop(session)

	return session.ID, nil
}

// emit publishes one Wails event, if there is still a runtime to publish into.
//
// Two guards, not one. Before SetContext there is nothing to emit to at all;
// after the context is cancelled the webview is being torn down, and every event
// is an evaluateJavaScript hop onto a main thread that is on its way out — which
// is exactly where a pane whose shell dies during quit would otherwise send its
// last batch.
func (m *Manager) emit(event string, data ...interface{}) {
	if m.ctx == nil || m.ctx.Err() != nil {
		return
	}
	emitEvent(m.ctx, event, data...)
}

// readPTY takes bytes off the pty and hands them to the flusher.
//
// This is the goroutine flow control parks. While it is not reading, nothing
// drains the tty: the kernel's buffer fills and the child blocks in write(2),
// which is the only backpressure a pty offers and the reason the pause has to
// happen here rather than at the emit.
//
// It owns doneCh — closing it is what tells the flusher the session is over —
// and it is released from a full dataCh by stop, which readLoop closes on its
// way out however it leaves.
func (m *Manager) readPTY(session *Session, dataCh chan<- []byte, doneCh chan<- struct{}, stop <-chan struct{}) {
	// Deferred in this order so they unwind in the other: recover first, so
	// session.panicked is set before the flusher's exit path reads it; then
	// doneCh, so that path runs at all; then the WaitGroup, so CloseAll knows
	// this goroutine is really gone.
	defer m.wg.Done()
	defer close(doneCh)
	defer m.recoverReader(session)

	buf := make([]byte, readBufSize)
	for session.waitForCredit() {
		n, err := session.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			// Charged at the hand-off rather than at the emit. These bytes are
			// already off the pty and the frontend cannot acknowledge them yet, so
			// they weigh on its credit exactly like emitted ones — and that is what
			// lets a single counter bound both the render backlog and the output
			// held for a pane that has not attached.
			session.charge(n)
			select {
			case dataCh <- data:
			case <-stop:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (m *Manager) readLoop(session *Session) {
	// Registered first so it unwinds last, after the recover handler has finished
	// tearing this session down.
	defer m.wg.Done()

	dataCh := make(chan []byte, batchChannelSize)
	doneCh := make(chan struct{})
	// stop releases a reader blocked handing over a batch. Without it a panic in
	// here would strand that goroutine on a full dataCh for the life of the
	// process: doneCh is the reader's own signal and nobody else closes it.
	// Registered after the recover handler so it unwinds *before* it — the
	// handler runs the shutdown ladder, and the reader has to be free to notice.
	stop := make(chan struct{})
	defer m.recoverFlusher(session)
	defer close(stop)

	// Safe to Add here rather than in CreateSession: this goroutine's own count
	// is still held, so the counter cannot be at zero under a concurrent Wait.
	m.wg.Add(1)
	go m.readPTY(session, dataCh, doneCh, stop)

	// accum holds output waiting for the next tick — and, until the pane
	// attaches, everything the shell has produced since spawn. That is why it is
	// allowed to grow far past a tick's worth: what bounds it is the reader's
	// high-water mark, not this loop.
	accum := make([]byte, 0, batchAccumCap)
	attached := false
	attachedCh := session.attachedCh
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()

	// flush emits what has accumulated, in order, as one or more bounded events.
	// force is the exit path: a dead shell's last words go out whether or not the
	// pane ever attached — see the note on that path below.
	flush := func(force bool) {
		if len(accum) == 0 || (!attached && !force) {
			return
		}
		for off := 0; off < len(accum); off += maxEventBytes {
			end := min(off+maxEventBytes, len(accum))
			m.emit("pty:output:"+session.ID, base64.StdEncoding.EncodeToString(accum[off:end]))
		}
		accum = resetAccum(accum)
	}

	for {
		select {
		case data := <-dataCh:
			accum = append(accum, data...)
			if len(accum) >= maxEventBytes {
				// A full event's worth already: stop waiting for the tick, or a
				// flood would sit here growing until one of the marks stopped it.
				flush(false)
			}
		case <-attachedCh:
			// The frontend has subscribed. Everything held since spawn goes out
			// now, in order, ahead of anything the reader produces next.
			attached = true
			// A closed channel is ready forever; nil is what takes this case back
			// out of the select.
			attachedCh = nil
			flush(false)
		case <-ticker.C:
			flush(false)
		case <-doneCh:
			// Drain any remaining buffered data from the reader goroutine
		drain:
			for {
				select {
				case data := <-dataCh:
					accum = append(accum, data...)
				default:
					break drain
				}
			}

			// The shell's dying words go out before its exit, and they are the
			// output that matters most: a broken ~/.zshrc or a $SHELL that exits at
			// once says why on its way out, and that line is all the user has.
			//
			// Emitting it is not the same as delivering it. Wails does not buffer
			// events, and the frontend does not learn this session's id until the
			// CreateSession binding promise resolves — a full round trip — and only
			// subscribes after that. A shell that dies inside that window would have
			// its batch emitted into a subscription that does not exist yet, which
			// is indistinguishable from dropping it; during a restore, several
			// CreateSession round trips race each other on one JS thread, so the
			// window is at its widest exactly when a bad rc file breaks every pane
			// at once. So an unattached pane gets a bounded grace to arrive first.
			//
			// The cost, stated plainly: for a shell that dies before anyone
			// attached, pty:exit is delayed by up to attachGrace. That is the right
			// trade. The pane is already dead, so nothing interactive is waiting on
			// it; the alternative is a pane that renders "[Process exited with code
			// 1]" with nothing above it to explain why. And nobody pays it while
			// waiting to quit: closing the pane, or the app, ends the wait at once.
			if !attached && awaitAttach(session, attachedCh) {
				// Only the flag is worth setting here: this path returns, so
				// nothing re-enters the select that attachedCh takes a case out of.
				attached = true
			}
			// force, so the grace expiring is still followed by a flush. That is
			// best effort by definition — the frontend that never subscribed cannot
			// be reached — and it is the same emit it always was.
			//
			// The invariant this leaves: an attach that lands after the flush but
			// before the delete below still returns true, and that is now fine.
			// Attaching is the frontend's second step, so an attach inside the grace
			// means the subscription was already in place when the flush went out —
			// the pane has its output and will have its exit. Past the grace, the
			// frontend has spent more than attachGrace between being handed the id
			// and subscribing, which is the pathological case the grace is sized
			// against; it gets the exit event either way.
			flush(true)

			// Release the PTY fd and reap the child. A shell that exits on its
			// own (exit/Ctrl-D, SSH drop, crash) would otherwise leak its ptmx
			// fd and leave a zombie: CloseSession can't recover it once it's
			// removed from the map below. Close is sync.Once-guarded, so when an
			// explicit CloseSession already started the shutdown ladder this
			// just blocks until that ladder finishes — which is exactly what
			// makes the exit status below well defined. The flush above stays
			// ahead of it, so the pane's last output still precedes its exit.
			session.Close()

			// Drop the session from the map *before* the exit event, and never
			// the other way round. Wails does not replay events, so a shell that
			// dies before the frontend has subscribed to pty:exit — a broken rc
			// file, a $SHELL that exits at once — leaves a pane that is silently
			// dead. The frontend's defence is to ask (AttachSession, or
			// HasSession behind App.SessionExists) once the subscription is in
			// place, and this ordering is what makes that question decisive:
			// "exists" can never mean "the event is already behind you". Either
			// the check runs after the delete and sees the session gone, so the
			// frontend synthesizes the exit, or it sees the session present and
			// the emit is still ahead of it — reaching a subscription that, by
			// then, exists.
			m.mu.Lock()
			delete(m.sessions, session.ID)
			m.mu.Unlock()

			code, signal := session.ExitStatus()
			if session.panicked.Load() {
				// The reader died on a panic, so the ladder above signalled a shell
				// that was doing nothing wrong. Whatever it reported describes our
				// bug, not the shell's fate: report the unknown pair.
				code, signal = -1, ""
			}
			m.emit("pty:exit:"+session.ID, PtyExit{ExitCode: code, Signal: signal})
			return
		}
	}
}

// awaitAttach holds a dead shell's last output for up to attachGrace while the
// frontend finishes subscribing, and reports whether it got there.
//
// Three ways out, and the last two are the same answer. The attach arrives, and
// the output can be flushed to a pane that will actually receive it. The session
// starts closing — CloseSession or CloseAll — and there is no longer anyone to
// wait for: a quit must not cost a grace per dying pane, which is the whole
// reason this selects on more than a timer. Or the grace runs out, and the flush
// goes ahead unheard rather than being held forever.
func awaitAttach(session *Session, attachedCh <-chan struct{}) bool {
	timer := time.NewTimer(attachGrace)
	defer timer.Stop()

	select {
	case <-attachedCh:
		return true
	case <-session.closing():
		return false
	case <-timer.C:
		log.Printf("ptymanager: session %s waited %s for an attach that never came; flushing its last output unheard",
			shortID(session.ID), attachGrace)
		return false
	}
}

// resetAccum empties the batch buffer, giving back the capacity a pane that
// buffered its way up to the high-water mark made it grow to.
func resetAccum(accum []byte) []byte {
	if cap(accum) > accumShrinkBytes {
		return make([]byte, 0, batchAccumCap)
	}
	return accum[:0]
}

// logPanic records a panic with the stack that produced it. A pane dying this
// way is rare enough that the whole stack earns its lines in the log.
func logPanic(session *Session, where string, r any) {
	log.Printf("ptymanager: session %s panic in %s: %v\n%s", shortID(session.ID), where, r, debug.Stack())
}

// containTeardown is deferred first by both recover handlers, so a panic in the
// containment itself stops there instead of at the top of the goroutine.
//
// This is not hypothetical tidiness. The handlers below run session.Close(), a
// map delete and one last m.emit while something has already gone wrong — and
// that emit is an EventsEmit onto a webview main thread that may be half torn
// down, which is precisely the call likeliest to panic next. Without this, a
// panic there escapes the deferred call that was containing the first one and
// takes the process with it: every pane in the window closed over one pane's
// bug, which is the failure mode the recover handlers exist to prevent.
//
// It logs and swallows, deliberately. There is no further teardown to attempt:
// whatever did not finish is a session whose goroutines are already unwinding.
func containTeardown(session *Session, where string) {
	if r := recover(); r != nil {
		log.Printf("ptymanager: session %s panic while containing a panic in %s: %v\n%s",
			shortID(session.ID), where, r, debug.Stack())
	}
}

// recoverReader contains a panic in the pty reader.
//
// It deliberately does not tear the session down: close(doneCh) is deferred
// immediately after this, so readLoop's exit path runs the usual sequence —
// flush, ladder, remove from the map, one pty:exit — and the pane is not told it
// died twice.
func (m *Manager) recoverReader(session *Session) {
	// First, so it unwinds last and covers everything below — including the
	// logging, which formats a stack of unbounded size. See containTeardown.
	defer containTeardown(session, "pty reader")

	r := recover()
	if r == nil {
		return
	}
	logPanic(session, "pty reader", r)
	session.panicked.Store(true)
}

// recoverFlusher contains a panic in readLoop, which is the goroutine that owns
// the exit sequence — so nothing else will finish this session off and it has to
// be done here: ladder, out of the map, one pty:exit reporting the unknown pair.
//
// Only this pane dies. Before this, a panic anywhere in the bridge — a bad batch,
// a nil the emitter did not expect — went to the top of a goroutine and took the
// process with it, closing every pane in the window over one pane's bug.
func (m *Manager) recoverFlusher(session *Session) {
	// First, so it unwinds last and covers the whole teardown below: the ladder,
	// the delete, and above all the final emit, which reaches a webview that may
	// itself be on its way out. See containTeardown.
	defer containTeardown(session, "output flusher")

	r := recover()
	if r == nil {
		return
	}
	logPanic(session, "output flusher", r)
	session.panicked.Store(true)

	// The ladder is contained on its own so that a panic inside it cannot skip
	// what follows: a session left in the map would be probed forever by the
	// status paths and its pane would never learn that it died.
	func() {
		defer containTeardown(session, "output flusher close")
		session.Close()
	}()

	m.mu.Lock()
	delete(m.sessions, session.ID)
	m.mu.Unlock()

	// Same ordering as the normal exit path: removed from the map first, so a
	// frontend asking whether the session is still there cannot be told "yes"
	// after the event has already gone out.
	m.emit("pty:exit:"+session.ID, PtyExit{ExitCode: -1, Signal: ""})
}

// WriteToSession sends input data (base64-encoded) to a PTY session.
func (m *Manager) WriteToSession(sessionID string, data string) error {
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return ErrSessionNotFound
	}

	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return err
	}

	_, err = session.Write(decoded)
	return err
}

// ResizeSession changes a PTY's dimensions.
func (m *Manager) ResizeSession(sessionID string, cols, rows int) {
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return
	}
	session.Resize(cols, rows)
}

// CloseSession terminates a PTY session.
func (m *Manager) CloseSession(sessionID string) {
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
	if ok {
		session.Close()
	}
}

// HasSession reports whether a session is still live — that is, still in the
// map. A session leaves the map when it is closed explicitly or when its shell
// exits, in which case readLoop removes it before emitting pty:exit; see the
// ordering note there for why that is what makes this answer usable.
func (m *Manager) HasSession(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessions[id]
	return ok
}

// AttachSession marks a pane's session as attached and flushes everything its
// shell has produced since spawn. Reports whether the session was still there.
//
// This is the second half of the frontend's handshake: subscribe to
// pty:output:<id> and pty:exit:<id>, then call this. Wails does not replay
// events and CreateSession starts reading the pty before it has even returned
// the id, so a fast shell's first prompt — or the error from one that fails to
// start — used to be emitted into a subscription that did not exist yet and was
// simply lost. Between spawn and this call the output is held instead, bounded
// by the same high-water mark as everything else.
//
// False means the session is gone: it exited or was closed before the frontend
// got here, and the pty:exit for it may or may not have been seen. That is the
// window this answers for — the frontend renders such a pane as "status
// unknown" rather than waiting for an event that is already behind it.
//
// A shell that has already died is the interesting case, and true is still the
// right answer there: the exit path holds its last output for attachGrace
// waiting for exactly this call, so an attach that lands inside the grace gets
// the shell's dying words and then its exit. See that path in readLoop.
func (m *Manager) AttachSession(id string) bool {
	// The lookup and the attach happen under one hold, so a session cannot be
	// removed in between and leave a caller believing it attached to a live pane.
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[id]
	if !ok {
		return false
	}
	session.attach()
	return true
}

// AckOutput records that the frontend has rendered n raw (decoded) bytes of a
// session's output, releasing that much of the reader's credit.
//
// This is the far end of the backpressure loop. Without it the reader takes
// bytes off the pty as fast as the shell writes them, and every batch becomes an
// evaluateJavaScript call on the webview's main thread — which is how a `cat` of
// a large file outran the UI and stalled it. An unknown id or a non-positive
// count is a no-op, and over-acking cannot drive the credit below zero.
func (m *Manager) AckOutput(id string, n int) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	session, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return
	}
	session.ack(n)
}

// GetSessionCWD returns the current working directory of a session's shell.
func (m *Manager) GetSessionCWD(sessionID string) (string, error) {
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return "", nil
	}
	return session.CWD()
}

// GetAllSessionCWDs returns CWDs for all active sessions.
func (m *Manager) GetAllSessionCWDs() map[string]string {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	sessions := make([]*Session, 0, len(m.sessions))
	for id, s := range m.sessions {
		ids = append(ids, id)
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	result := make(map[string]string)
	for i, s := range sessions {
		// Report only what we could actually read. Sessions whose cwd could not
		// be determined are omitted rather than mapped to "", so callers can tell
		// "unknown" apart from "at the filesystem root" and keep their own last
		// known value instead of overwriting it.
		if cwd, err := s.CWD(); err == nil && cwd != "" {
			result[ids[i]] = cwd
		}
	}
	return result
}

// GetAllSessionStatuses returns the status of all active sessions.
func (m *Manager) GetAllSessionStatuses() map[string]SessionStatus {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	sessions := make([]*Session, 0, len(m.sessions))
	for id, s := range m.sessions {
		ids = append(ids, id)
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	result := make(map[string]SessionStatus)
	for i, s := range sessions {
		cwd, _ := s.CWD()
		cmd := s.ForegroundProcess()
		result[ids[i]] = SessionStatus{
			SessionID: ids[i],
			CWD:       cwd,
			Command:   cmd,
			IsIdle:    cmd == "",
		}
	}
	return result
}

// RunningCommandCount reports how many sessions have something other than the
// shell itself in the foreground.
//
// This is deliberately not GetAllSessionStatuses with the unused fields thrown
// away. That one also reads each session's cwd, and the only thing the quit path
// ever looks at is whether the pane is busy. It used to matter far more than it
// does now — both probes forked (lsof at ~12 ms, ps at ~2 ms), so a window of a
// dozen panes paid ~150 ms of subprocesses on every Cmd-Q before the
// confirmation dialog could be shown, for two numbers it discarded. Both are
// syscalls today, so this is cheap either way; asking only what it needs is
// still the right shape.
func (m *Manager) RunningCommandCount() int {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	running := 0
	for _, s := range sessions {
		if s.ForegroundProcess() != "" {
			running++
		}
	}
	return running
}

// CloseAll terminates all PTY sessions and waits for readLoop goroutines to finish.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for id, session := range m.sessions {
		sessions = append(sessions, session)
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	// Each Close now walks a shutdown ladder that gives the shell seconds, not
	// microseconds, to save its history. Sequentially that is one ladder per
	// pane — a window of 17 shells would hang the quit for half a minute — so
	// they run together and the quit costs one ladder, not seventeen.
	var closing sync.WaitGroup
	for _, session := range sessions {
		closing.Add(1)
		go func() {
			defer closing.Done()
			session.Close()
		}()
	}
	closing.Wait()
	m.wg.Wait()
}
