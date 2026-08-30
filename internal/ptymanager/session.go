package ptymanager

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	defaultCols = 80
	defaultRows = 24
)

// Deadlines of the shutdown ladder in Close. They are vars, not consts, only so
// the tests can run the ladder in milliseconds instead of seconds.
var (
	// hangupGrace is how long the shell gets to shut itself down after the
	// hangup. zsh writes ~/.zsh_history from its HUP path, and that is the
	// whole point of the ladder, so this is the generous one.
	hangupGrace = 2 * time.Second
	// terminateGrace is the second chance, for a shell that ignored the hangup.
	terminateGrace = 1 * time.Second
	// killGrace bounds the final wait. SIGKILL cannot be caught, so this only
	// stops a wedged child from hanging app shutdown forever.
	killGrace = 1 * time.Second
	// foregroundGrace bounds each of the three rungs run against the job the
	// shell had in the foreground, once the shell itself is gone. Shorter than
	// the shell's grace periods, and paid only by a job that is still there when
	// its rung starts: the shell's history — what the generous grace above is
	// for — has already been written by this point, and every millisecond here
	// is a millisecond of a quit the user is watching.
	foregroundGrace = 300 * time.Millisecond
)

// groupPollInterval is how often the foreground rungs re-ask whether a process
// group still has members. There is no event to wait on for "some process that
// is not my child exited", so this polls.
const groupPollInterval = 10 * time.Millisecond

// Session manages a single PTY shell session.
type Session struct {
	ID        string
	cmd       *exec.Cmd
	closeOnce sync.Once

	// ptmxMu guards the master's fd *number*, not the bytes that go through it.
	// See withPtmx for who has to hold it and why Read and Write do not.
	ptmxMu     sync.RWMutex
	ptmx       *os.File
	ptmxClosed bool

	// closed is set before the shutdown ladder starts. Once the child has been
	// reaped the kernel is free to hand its pid to an unrelated process, so
	// CWD() and ForegroundProcess() must stop probing it.
	closed atomic.Bool

	// closingCh is the selectable form of closed: it is closed as Close begins,
	// before anything on the ladder can block. readLoop's exit path waits on it
	// so that closing a pane — or quitting the app — never has to sit out the
	// attach grace for a shell that has already died.
	closingCh chan struct{}

	// panicked records that a goroutine of this session's own died on a panic.
	// The exit status is then meaningless — the shell may be perfectly healthy
	// and simply have nobody left to read it — so the pane is reported as the
	// unknown pair rather than as whatever the shutdown ladder happened to see.
	panicked atomic.Bool

	statusMu   sync.Mutex
	exitCode   int
	exitSignal string

	// --- flow control (the reader's half of the output bridge) ---
	//
	// Every batch the flusher emits becomes an evaluateJavaScript call on the
	// webview's main thread. Reading the pty as fast as the shell writes it —
	// which is what the reader did before — means a `cat` of a large file
	// outruns the UI by megabytes and stalls it, because nothing in Wails pushes
	// back. So the frontend acknowledges what it has rendered and the reader
	// spends that credit.
	//
	// unacked is the credit outstanding: raw bytes taken off the pty that
	// AckOutput has not confirmed. It is charged when the bytes leave the pty
	// rather than when they are emitted, which makes it one counter for two jobs
	// — bytes waiting in the batch buffer are equally unrenderable, and output
	// held back because the pane has not attached yet is bounded by the same
	// mark with no second accounting.
	//
	// At highWater the reader parks; an ack that brings the backlog back to
	// lowWater releases it. The gap between the two marks is the whole point:
	// waking at the same mark it slept on would restart the reader once per
	// batch. While it is parked nothing drains the tty, the kernel's tty buffer
	// fills, and the child blocks in write(2) — that is the only backpressure a
	// pty offers, and reaching it is the goal.
	flowMu   sync.Mutex
	flowCond *sync.Cond
	unacked  int
	// paused is the state, not a derived value: the reader stays parked all the
	// way down from highWater to lowWater, so "should I be reading" cannot be
	// recomputed from unacked alone.
	paused bool
	// flowClosed releases a parked reader for good. Without it a session closed
	// while the reader is parked would leave that goroutine waiting on a
	// condition only the frontend could satisfy — for a pane the frontend has
	// already thrown away.
	flowClosed bool

	// --- attach (the frontend's subscription handshake) ---
	//
	// A session starts unattached and its output accumulates instead of being
	// emitted. Wails does not replay events and CreateSession starts reading the
	// pty before its id has even been returned, so a fast shell's first prompt
	// used to be emitted into a subscription that did not exist yet. attachedCh
	// is closed once, by attach, to wake the flusher.
	attachOnce sync.Once
	attachedCh chan struct{}

	// --- transcript (the raw recording of everything the pty produced) ---
	//
	// transcriptMu guards the pointer below, everything reachable through it,
	// and the seal. Deliberately its own lock rather than ptmxMu: the reader
	// takes this on every read that has something to record, and ptmxMu's whole
	// design is that the read path never touches it (see withPtmx).
	transcriptMu sync.Mutex
	transcript   *transcript
	// transcriptSealed is set once the reader that feeds a recording has
	// stopped. A session stays in the manager's map for a little while after
	// that — the exit path holds it through the attach grace — and a recording
	// started in that window would have nobody left to write to it or close it.
	transcriptSealed bool
	// emit is the manager's event seam, handed over at creation. A recording
	// that retires *itself* — a write that failed, the size cap — has to be able
	// to say so, and the goroutines that discover it (the reader and the flush
	// ticker) belong to the session rather than to the manager. Nil for a
	// session built outside a Manager, which is why every use is guarded.
	emit func(event string, data ...interface{})
}

// NewSession spawns a shell in a new PTY. If cwd is empty, defaults to home.
// configDir is used to locate shell integration scripts.
func NewSession(shell, configDir string, cols, rows int, cwd string) (*Session, error) {
	if cols < 1 {
		cols = defaultCols
	}
	if rows < 1 {
		rows = defaultRows
	}

	home, _ := os.UserHomeDir()
	dir, fallback := resolveStartDir(cwd, home)

	cmd := exec.Command(shell, "-l")
	cmd.Dir = dir

	shellIntegrationDir := filepath.Join(configDir, "shell")
	env := append(os.Environ(),
		"TERM=xterm-256color",
		"TERM_PROGRAM=ElTerminalo",
		"PROMPT_EOL_MARK=",
		fmt.Sprintf("COLUMNS=%d", cols),
		fmt.Sprintf("LINES=%d", rows),
		"ELTERMINALO_SHELL_INTEGRATION_DIR="+shellIntegrationDir,
	)

	// Shell-specific integration injection
	shellName := filepath.Base(shell)
	switch {
	case strings.Contains(shellName, "zsh"):
		// ZDOTDIR trick: point zsh to our bootstrap .zshenv which sources
		// the user's real config and then loads shell integration.
		origZdotdir := os.Getenv("ZDOTDIR")
		env = append(env,
			"ZDOTDIR="+filepath.Join(shellIntegrationDir, "zdotdir"),
			"ELTERMINALO_ORIG_ZDOTDIR="+origZdotdir,
		)
	case strings.Contains(shellName, "bash"):
		// Bootstrap via PROMPT_COMMAND: sources integration on first prompt,
		// then restores any original PROMPT_COMMAND.
		script := filepath.Join(shellIntegrationDir, "elterminalo-integration-bash.sh")
		env = append(env, "PROMPT_COMMAND=source '"+script+"'")
	}

	cmd.Env = env

	ws := &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}

	ptmx, err := pty.StartWithSize(cmd, ws)
	if err != nil && dir != home && home != "" {
		// The shell could not chdir into the restored directory (unmounted
		// volume, revoked access). Open the pane in home rather than failing —
		// but note that the caller keeps the original path in the layout, so the
		// pane returns to it once the directory is reachable again.
		retry := exec.Command(shell, "-l")
		retry.Dir = home
		retry.Env = env
		cmd = retry
		ptmx, err = pty.StartWithSize(cmd, ws)
		fallback = fmt.Sprintf("shell could not start in %s", dir)
		dir = home
	}
	if err != nil {
		return nil, fmt.Errorf("failed to start pty: %w", err)
	}

	s := &Session{
		ID:   uuid.New().String(),
		cmd:  cmd,
		ptmx: ptmx,
		// Until the shell has been reaped there is no status to report; -1 with
		// no signal is the same "unknown" the ladder falls back to.
		exitCode:   -1,
		closingCh:  make(chan struct{}),
		attachedCh: make(chan struct{}),
	}
	s.flowCond = sync.NewCond(&s.flowMu)

	// One line per pane, with the fallback reason spelled out rather than left
	// to be inferred from the directory: panes silently reopening in $HOME is a
	// bug this package has already shipped once, and it is invisible in a log
	// that only records where the shell ended up.
	log.Printf("ptymanager: session %s spawned pid=%d shell=%s dir=%q fallback=%q",
		shortID(s.ID), cmd.Process.Pid, shell, dir, fallback)

	return s, nil
}

// shortID is the first 8 characters of a session id — enough to follow one pane
// through the log without pasting a full uuid onto every line.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// withPtmx runs fn against the pty master while the fd number underneath it is
// still ours, and answers os.ErrClosed once it is not.
//
// Every caller that reaches for the raw descriptor has to come through here:
// foregroundPgid's TIOCGPGRP, and Resize by way of pty.Setsize. A file
// descriptor is a small integer the kernel reuses the moment it is free, so an
// ioctl issued on a number Close has already given up is not a failed ioctl —
// it is an ioctl answered by whatever holds that number now, which in this
// process is realistically another pane's master. TIOCGPGRP would then report
// that pane's foreground job as this one's, and Close would signal its process
// group.
//
// The write side is taken only around the master's close, so a probe either
// completes with the fd alive or finds ptmxClosed already set: the kernel cannot
// release the number before *os.File.Close has been called, and this holds it
// shut for exactly that call. Probes are two syscalls long, so Close never waits
// on one for any measurable time.
//
// Read and Write deliberately do not use this. They block for as long as the
// shell is quiet — holding a lock across that would make Close wait for the next
// keystroke — and they go through *os.File rather than the bare number, which
// the runtime already makes safe against a concurrent Close: the reader holds a
// reference for the length of the call, and the descriptor is not released until
// it returns.
func (s *Session) withPtmx(fn func(ptmx *os.File) error) error {
	s.ptmxMu.RLock()
	defer s.ptmxMu.RUnlock()
	if s.ptmxClosed {
		return os.ErrClosed
	}
	return fn(s.ptmx)
}

// closePtmx retires the master, under the write lock so that no probe can be
// part-way through an ioctl on its fd while the number is being given up.
func (s *Session) closePtmx() {
	s.ptmxMu.Lock()
	defer s.ptmxMu.Unlock()
	s.ptmxClosed = true
	s.ptmx.Close()
}

// Read reads from the PTY. Blocks until data is available.
func (s *Session) Read(buf []byte) (int, error) {
	return s.ptmx.Read(buf)
}

// Write sends data to the PTY.
func (s *Session) Write(data []byte) (int, error) {
	return s.ptmx.Write(data)
}

// charge counts n bytes of output against the frontend's credit. Called by the
// reader as the bytes come off the pty, before they are handed to the flusher.
func (s *Session) charge(n int) {
	s.flowMu.Lock()
	s.unacked += n
	s.flowMu.Unlock()
}

// ack releases n bytes of credit and, once the backlog is back under lowWater,
// wakes a parked reader.
func (s *Session) ack(n int) {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()

	s.unacked -= n
	if s.unacked < 0 {
		// A frontend that acks more than it was sent must not be able to buy
		// itself credit it never spent, or a bug on that side turns straight back
		// into the unbounded reader this exists to prevent.
		s.unacked = 0
	}
	if s.paused && s.unacked <= lowWater {
		s.paused = false
		log.Printf("ptymanager: session %s reader resumed unacked=%d", shortID(s.ID), s.unacked)
		s.flowCond.Broadcast()
	}
}

// waitForCredit parks the reader while the frontend is too far behind, and
// reports whether it should read again.
//
// It is called before each read, not after: the point is to leave the bytes in
// the tty buffer, where they block the child, rather than to pull them out and
// queue them somewhere of our own.
//
// False means stop. Still being paused after the wait is what says the wake came
// from Close rather than from an ack — the frontend is never going to catch up,
// the master is closed, and reading on would only park this goroutine on a tty
// nobody will ever write to again.
//
// One consequence is worth stating outright: a parked reader is not watching the
// pty, so it does not see EOF either. A shell that produces a megabyte and then
// dies while the frontend owes every byte of it stays in the map, and its
// pty:exit waits for the ack that releases the reader. Only two things reach it
// there — an ack, or Close — and that is the correct trade: noticing the exit
// would mean reading, and reading is the thing being refused. Close is what
// covers the case where the frontend never comes back.
func (s *Session) waitForCredit() bool {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()

	if !s.paused && s.unacked >= highWater {
		s.paused = true
		log.Printf("ptymanager: session %s reader paused unacked=%d", shortID(s.ID), s.unacked)
	}
	for s.paused && !s.flowClosed {
		s.flowCond.Wait()
	}
	return !s.paused
}

// closeFlow releases a parked reader permanently. Close calls it first, before
// anything that could block, because the ladder below it ends with a wait for a
// child whose output nobody is draining.
func (s *Session) closeFlow() {
	s.flowMu.Lock()
	s.flowClosed = true
	s.flowMu.Unlock()
	s.flowCond.Broadcast()
}

// flowState reports the reader's flow-control state, for the tests that assert
// the reader really stops rather than merely slows down.
func (s *Session) flowState() (unacked int, paused bool) {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	return s.unacked, s.paused
}

// attach marks the pane as subscribed and wakes the flusher, which then emits
// everything held since spawn before anything newer.
//
// Idempotent on purpose: the frontend re-attaches whenever it re-subscribes,
// and closing a channel twice panics.
func (s *Session) attach() {
	s.attachOnce.Do(func() {
		log.Printf("ptymanager: session %s attached", shortID(s.ID))
		close(s.attachedCh)
	})
}

// closing is closed once Close has started. It is what lets a wait for the
// frontend be abandoned the moment the pane is being torn down anyway — see the
// attach grace in readLoop's exit path.
func (s *Session) closing() <-chan struct{} {
	return s.closingCh
}

// Resize changes the PTY dimensions.
//
// pty.Setsize is a TIOCSWINSZ on the master's raw fd, so it goes through
// withPtmx for the same reason the foreground probe does: a resize landing on a
// recycled descriptor would reshape another pane's terminal.
func (s *Session) Resize(cols, rows int) error {
	return s.withPtmx(func(ptmx *os.File) error {
		return pty.Setsize(ptmx, &pty.Winsize{
			Cols: uint16(cols),
			Rows: uint16(rows),
		})
	})
}

// Close terminates the PTY session and reaps the shell. Safe to call multiple
// times and concurrently with the read loop: a second caller blocks on the same
// sync.Once until the ladder below has finished, which is what makes
// ExitStatus well defined once Close returns.
//
// The shell is shut down the way a terminal emulator does it, not the way a
// process supervisor does. The previous version closed the PTY and SIGKILLed on
// the next line; a pending SIGKILL is delivered before any handler runs, so zsh
// never reached the HUP path that writes ~/.zsh_history and every line typed in
// the pane was lost — on each pane close and on every quit. Escalation only
// happens when the shell declines the polite signals.
//
// Two process groups are walked down, not one. The shell is interactive, so it
// has job control on and runs each foreground job in a process group of its own;
// nothing aimed at the shell's group reaches that job. See stopForegroundGroup.
//
// Note that on macOS every one of these signals has to be sent explicitly.
// Closing the master does not hang anything up: measured on darwin 25, a shell
// blocked reading its tty does not see EOF and is not signalled, and neither is
// its foreground job — both were still running two seconds after the master was
// closed with nothing else sent. That is why this reads like a ladder of signals
// rather than "close the fd and wait".
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		start := time.Now()

		// Set before anything else: from here on the pid is on its way to being
		// reaped and must not be probed by CWD()/ForegroundProcess(). The channel
		// says the same thing to anyone who has to wait on it rather than poll it
		// — readLoop's exit path, which must not hold a quit up for its own attach
		// grace once the pane is being closed anyway.
		s.closed.Store(true)
		close(s.closingCh)

		// Release the reader before anything that waits. If flow control has it
		// parked — the frontend fell behind, or the pane was closed before it ever
		// attached — nothing below will ever satisfy the condition it is waiting
		// on, and it would sit there for the life of the process.
		s.closeFlow()

		// Ask the tty which process group it has in the foreground before the
		// master is closed — afterwards the ioctl has no answer, and this is the
		// only handle on the command the user is actually running.
		fgPgid := s.foregroundPgid()

		// Hang up on that job now, while the group just read is still the current
		// one. It is the only hangup the job is guaranteed to get. The kernel does
		// send SIGHUP to the tty's foreground group — but only when the shell
		// exits, as the parting act of the controlling process, so a shell that
		// declines to exit leaves the job with nothing at all. Sending it here
		// makes the job's hangup ours rather than a side effect of the shell's
		// timing, and it is the polite rung of the ladder in stopForegroundGroup:
		// a job with anything to save gets asked before it is told.
		signalGroup(fgPgid, syscall.SIGHUP)

		// Retire the master. This is bookkeeping, not the release of an fd. On
		// macOS it signals nobody and does not end the shell's tty reads (see the
		// note above Close); it is the SIGHUP below that shuts the shell down.
		// And it does not perform the close(2) either: the master is opened in
		// blocking mode, so it is not registered with the runtime's poller and
		// *os.File.Close cannot interrupt the Read that readLoop is parked in.
		// What it does is mark the file closed — every later Read/Write gets
		// ErrClosed — and hand the actual close to whoever holds the last
		// reference, which is that blocked reader. It returns there with EOF once
		// the session leader dies, and only then is the fd really released
		// (measured on darwin 25, EOF included, even with a setsid'd grandchild
		// still holding the slave open). Close deliberately does not wait for
		// that: a blocking fd would deadlock the ladder that is about to make it
		// happen.
		//
		// Done under the write lock so that no probe is part-way through an ioctl
		// on this fd as the number is given up, and so every probe after this
		// point is answered with ErrClosed rather than by whoever the kernel hands
		// that number to next. See withPtmx.
		s.closePtmx()

		if s.cmd.Process == nil {
			log.Printf("ptymanager: session %s closed before its shell started", shortID(s.ID))
			return
		}
		// pty.StartWithSize starts the shell with Setsid, so it leads its own
		// session and its pid is also its process-group id.
		pgid := s.cmd.Process.Pid

		reaped := make(chan struct{})
		go func() {
			defer close(reaped)
			_ = s.cmd.Wait()
			code, signal := exitStatusOf(s.cmd.ProcessState)
			s.statusMu.Lock()
			s.exitCode, s.exitSignal = code, signal
			s.statusMu.Unlock()
		}()

		// Hang up on the shell. This is the signal that actually ends it, and it
		// goes to its whole process group so that a child still sitting in that
		// group gets it too — which is where a shell with job control off puts all
		// of them. It does not reach the foreground job of an interactive shell;
		// that job's group is fgPgid, hung up above and escalated on below.
		//
		// Skipped when the shell has already been reaped: that is the read loop's
		// EOF path, where the shell exited on its own before Close was called and
		// cmd.Wait returns within microseconds. Until it does, the pid is still
		// ours — the kernel cannot hand a pid out again while a zombie holds it —
		// so this check is what keeps the signal off a recycled group. It narrows
		// that window rather than closing it: the goroutine's close(reaped) is
		// deferred and so runs after cmd.Wait has already released the pid, and in
		// that gap this still reports "not reaped".
		// rung records how far down the ladder this session had to be walked, for
		// the log line at the end. "self" means it was already gone when Close
		// started — the read loop's EOF path — and needed no signal at all.
		rung := "self"
		if !isReaped(reaped) {
			signalGroup(pgid, syscall.SIGHUP)
			rung = "hangup"
		}
		if !waitFor(reaped, hangupGrace) {
			// The shell declined the hangup. Escalate on it — and on the foreground
			// job, which is the likeliest reason the shell is not going anywhere:
			// it is waiting on the job, and the job ignored its own hangup.
			signalGroup(pgid, syscall.SIGTERM)
			signalGroup(fgPgid, syscall.SIGTERM)
			rung = "terminate"
			if !waitFor(reaped, terminateGrace) {
				signalGroup(pgid, syscall.SIGKILL)
				signalGroup(fgPgid, syscall.SIGKILL)
				rung = "kill"
				if !waitFor(reaped, killGrace) {
					// Outlived SIGKILL: wedged in an uninterruptible syscall. This is
					// the (-1, "") status ExitStatus documents.
					rung = "unresponsive"
				}
			}
		}

		// The shell is gone, but the job it was running in the foreground can
		// outlive it: it has a process group of its own and is reparented to
		// launchd, so being reaped above says nothing about it. This is the
		// npm-run-dev-still-holding-its-port case behind the quit dialog's
		// "N commands are still running and will be terminated".
		stopForegroundGroup(fgPgid)

		code, signal := s.ExitStatus()
		// The rung and the elapsed time are the two numbers that turn "quitting is
		// slow" into an answer: a quit that costs seconds is one pane sitting on
		// the terminate rung, and this says which.
		log.Printf("ptymanager: session %s exited code=%d signal=%q rung=%s ms=%d",
			shortID(s.ID), code, signal, rung, time.Since(start).Milliseconds())
	})
}

// stopForegroundGroup finishes off whatever the shell was running in the tty's
// foreground process group, once the shell itself has been dealt with.
//
// The shell's own rungs cannot do this. kill(-shellpid, …) does not reach a
// process group the shell created for a job, and the shell exits on the very
// first rung — so without this, Close returned while the job was still holding
// its port. The hangup Close already sent to this group is the first rung here;
// the wait for it is the same kind of grace the shell gets, because a job can
// have as much to save on the way out as the shell does.
//
// The waits are short by design and none of them is paid by a job that is
// already gone: waitGroupGone asks first and sleeps second. An idle pane, which
// has no foreground group distinct from the shell, does not reach the syscall at
// all — pgid is 0 and this returns on the first line.
func stopForegroundGroup(pgid int) {
	if pgid <= 1 || waitGroupGone(pgid, foregroundGrace) {
		return
	}
	signalGroup(pgid, syscall.SIGTERM)
	if waitGroupGone(pgid, foregroundGrace) {
		return
	}
	signalGroup(pgid, syscall.SIGKILL)
	waitGroupGone(pgid, foregroundGrace)
}

// ExitStatus reports how the shell finished: its exit status (0-255) and an
// empty signal name for a normal exit, or -1 and the signal name when it was
// terminated by a signal — the usual outcome when the app closed the pane.
//
// (-1, "") is a third, deliberate case: status unknown. Every rung of the
// ladder is bounded, so a child that outlives SIGKILL — wedged in an
// uninterruptible syscall — leaves Close with nothing to report, and a session
// that has not been closed yet has nothing to report either. Callers render it
// as "unknown", not as an exit code.
//
// Valid once Close has returned; Close is the only place the child is reaped.
func (s *Session) ExitStatus() (int, string) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.exitCode, s.exitSignal
}

// exitStatusOf renders a finished process's status for the pty:exit event.
func exitStatusOf(state *os.ProcessState) (int, string) {
	if state == nil {
		// Wait returned without a status (it had already been called, say).
		// Reported as the same "unknown" pair ExitStatus documents, because that
		// is exactly what it is.
		return -1, ""
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		// ExitCode is already -1 here; the signal name is the informative half.
		return -1, unix.SignalName(ws.Signal())
	}
	return state.ExitCode(), ""
}

// signalGroup sends sig to one process group.
//
// Note what "the group" is and is not. The shell's group holds the shell and any
// child that stayed in it — which, for a shell with job control off, is all of
// them. It does not hold the foreground job of an interactive shell: job control
// means each job gets a process group of its own, and kill(-shellpid, …) never
// reaches it. Close therefore signals two groups, not one.
//
// The guard is not paranoia: kill(-1, …) means "every process this user owns",
// so a pgid of 0 or 1 must never reach the syscall. ESRCH is ignored — the
// group already being gone is the outcome we are asking for.
func signalGroup(pgid int, sig syscall.Signal) {
	if pgid <= 1 {
		return
	}
	_ = syscall.Kill(-pgid, sig)
}

// groupGone reports whether a process group has no members left. Signal 0 runs
// the existence and permission checks without delivering anything, so ESRCH is
// the answer being looked for. Any other error means the group is not ours to
// reason about, which is also a reason to stop signalling it.
func groupGone(pgid int) bool {
	if pgid <= 1 {
		return true
	}
	return syscall.Kill(-pgid, 0) != nil
}

// waitGroupGone reports whether the group emptied out within d. Unlike waitFor
// these processes are not our children — the shell forked them — so there is no
// Wait to block on and nothing to reap; polling is the only way to ask.
func waitGroupGone(pgid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if groupGone(pgid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(groupPollInterval)
	}
}

// waitFor reports whether the child was reaped within d.
func waitFor(reaped <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-reaped:
		return true
	case <-timer.C:
		// The child may have exited just as the timer fired. Re-check before
		// escalating, so a pid the kernel has already recycled is not signalled.
		return isReaped(reaped)
	}
}

// isReaped reports whether the child has been reaped, without waiting for it.
func isReaped(reaped <-chan struct{}) bool {
	select {
	case <-reaped:
		return true
	default:
		return false
	}
}

// resolveStartDir decides which directory a new shell should start in, and says
// why when it did not use the one it was asked for. why is "" when the request
// was honoured; it goes straight into the spawn log line, because a pane that
// quietly reopens somewhere else is precisely the bug this function exists to
// prevent and is invisible in a log that only records where the shell ended up.
//
// It falls back to home only when the requested directory is provably gone.
// The Stat below runs in the app process against a path that is normally under
// a TCC-protected root (~/Documents, ~/Desktop, ~/Downloads), and a privacy
// denial returns EPERM, not ENOENT: the folder is fine, this process just could
// not stat it at that instant. Collapsing both cases into "fall back to home"
// reopened every pane in $HOME, and the 30s layout autosave then wrote $HOME
// back into state.json — turning a momentary denial into permanent loss of the
// user's folders. Anything other than "does not exist" keeps the directory.
func resolveStartDir(cwd, home string) (dir, why string) {
	if cwd == "" {
		return home, "no directory requested"
	}
	info, err := os.Stat(cwd)
	if err != nil {
		if os.IsNotExist(err) {
			return home, "requested directory does not exist"
		}
		return cwd, ""
	}
	if !info.IsDir() {
		return home, "requested path is not a directory"
	}
	return cwd, ""
}
