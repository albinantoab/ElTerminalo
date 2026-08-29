package ptymanager

import (
	"fmt"
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
	ptmx      *os.File
	closeOnce sync.Once

	// closed is set before the shutdown ladder starts. Once the child has been
	// reaped the kernel is free to hand its pid to an unrelated process, so
	// CWD() and ForegroundProcess() must stop probing it.
	closed atomic.Bool

	statusMu   sync.Mutex
	exitCode   int
	exitSignal string
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
	dir := resolveStartDir(cwd, home)

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
	}
	if err != nil {
		return nil, fmt.Errorf("failed to start pty: %w", err)
	}

	return &Session{
		ID:   uuid.New().String(),
		cmd:  cmd,
		ptmx: ptmx,
		// Until the shell has been reaped there is no status to report; -1 with
		// no signal is the same "unknown" the ladder falls back to.
		exitCode: -1,
	}, nil
}

// Read reads from the PTY. Blocks until data is available.
func (s *Session) Read(buf []byte) (int, error) {
	return s.ptmx.Read(buf)
}

// Write sends data to the PTY.
func (s *Session) Write(data []byte) (int, error) {
	return s.ptmx.Write(data)
}

// Resize changes the PTY dimensions.
func (s *Session) Resize(cols, rows int) error {
	return pty.Setsize(s.ptmx, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
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
		// Set before anything else: from here on the pid is on its way to being
		// reaped and must not be probed by CWD()/ForegroundProcess().
		s.closed.Store(true)

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
		s.ptmx.Close()

		if s.cmd.Process == nil {
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
		if !isReaped(reaped) {
			signalGroup(pgid, syscall.SIGHUP)
		}
		if !waitFor(reaped, hangupGrace) {
			// The shell declined the hangup. Escalate on it — and on the foreground
			// job, which is the likeliest reason the shell is not going anywhere:
			// it is waiting on the job, and the job ignored its own hangup.
			signalGroup(pgid, syscall.SIGTERM)
			signalGroup(fgPgid, syscall.SIGTERM)
			if !waitFor(reaped, terminateGrace) {
				signalGroup(pgid, syscall.SIGKILL)
				signalGroup(fgPgid, syscall.SIGKILL)
				waitFor(reaped, killGrace)
			}
		}

		// The shell is gone, but the job it was running in the foreground can
		// outlive it: it has a process group of its own and is reparented to
		// launchd, so being reaped above says nothing about it. This is the
		// npm-run-dev-still-holding-its-port case behind the quit dialog's
		// "N commands are still running and will be terminated".
		stopForegroundGroup(fgPgid)
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

// resolveStartDir decides which directory a new shell should start in.
//
// It falls back to home only when the requested directory is provably gone.
// The Stat below runs in the app process against a path that is normally under
// a TCC-protected root (~/Documents, ~/Desktop, ~/Downloads), and a privacy
// denial returns EPERM, not ENOENT: the folder is fine, this process just could
// not stat it at that instant. Collapsing both cases into "fall back to home"
// reopened every pane in $HOME, and the 30s layout autosave then wrote $HOME
// back into state.json — turning a momentary denial into permanent loss of the
// user's folders. Anything other than "does not exist" keeps the directory.
func resolveStartDir(cwd, home string) string {
	dir := cwd
	if dir == "" {
		return home
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return home
		}
		return dir
	}
	if !info.IsDir() {
		return home
	}
	return dir
}
