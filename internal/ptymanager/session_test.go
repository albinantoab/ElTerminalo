package ptymanager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const testHome = "/Users/test/home"

// A permission error must never be mistaken for a missing directory. This is
// the regression that made panes silently reopen in $HOME: ~/Documents is
// TCC-protected, a denial surfaces as EPERM, and the 30s layout autosave then
// wrote the home path back to disk — losing the user's folder permanently.
func TestResolveStartDir_PermissionDeniedKeepsDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	parent := t.TempDir()
	target := filepath.Join(parent, "project")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// Drop traverse permission on the parent so Stat(target) fails with EACCES
	// rather than ENOENT — the same shape a TCC denial has.
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	if _, err := os.Stat(target); err == nil {
		t.Fatal("precondition failed: Stat should have been denied")
	} else if os.IsNotExist(err) {
		t.Fatalf("precondition failed: want a permission error, got %v", err)
	}

	if got := resolveStartDir(target, testHome); got != target {
		t.Errorf("permission error must preserve the directory:\n got %q\nwant %q", got, target)
	}
}

func TestResolveStartDir(t *testing.T) {
	existing := t.TempDir()

	file := filepath.Join(existing, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{"existing directory is used", existing, existing},
		{"empty request falls back to home", "", testHome},
		{"missing directory falls back to home", filepath.Join(existing, "gone"), testHome},
		{"a file is not a working directory", file, testHome},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveStartDir(tt.cwd, testHome); got != tt.want {
				t.Errorf("resolveStartDir(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
		})
	}
}

// --- shutdown ladder ---------------------------------------------------------

// The scripts below stand in for the user's shell. NewSession appends "-l",
// which a script quietly takes as an unused argument, so a test can spell out
// exactly the signal behaviour it needs — and every process the tests touch is
// one they spawned themselves.
//
// They idle the way a real shell idles — blocked reading its tty — rather than
// looping over sleeps. A loop that keeps forking children races the group
// signal: a child forked just after the kill never receives it, and the shell
// then sits waiting for it. The ladder is built for exactly that (it escalates),
// but a test should not have to gamble on the timing.
const (
	// idle blocks reading the tty until the ladder's SIGHUP arrives. Closing the
	// master does not end that read on macOS — the signal is what does.
	idle = ": > ready\nwhile :; do read line; done\n"
	// savesOnHangup stands in for zsh writing ~/.zsh_history on SIGHUP.
	savesOnHangup = "trap 'echo saved > history; exit 0' HUP\n" + idle
	// signalsIgnored survives everything but SIGKILL. It sleeps rather than
	// reads, because after the hangup a read loop would spin on a dead tty for
	// the length of the ladder.
	signalsIgnored = "trap '' HUP TERM\n: > ready\nwhile :; do sleep 1; done\n"
	// The two scripts below are the only ones here that turn job control on, with
	// "set -m". That is what a real (interactive) shell does, and it is what the
	// others hide: without it a child stays in the shell's own process group, so
	// kill(-shellpid) reaches it and the ladder looks like it works. Each runs one
	// foreground job that records its pid, so a test can watch for its death.
	//
	// stuckShellWithOrdinaryJob pairs a shell that will not die politely with a
	// plain job that would. The shell's trap runs a command rather than ignoring
	// the signal: an ignored disposition is inherited across exec — that is how
	// nohup works — and would make the job stubborn too, while a trapped one is
	// reset to the default for the child, which is the point here.
	stuckShellWithOrdinaryJob = "set -m\ntrap 'true' HUP TERM\n" +
		`sh -c 'echo $$ > jobpid; : > ready; while :; do sleep 1; done'` + "\n"
	// foregroundJobInItsOwnGroup is the other way round: the shell dies on the
	// first rung and the job it leaves behind survives everything but SIGKILL.
	foregroundJobInItsOwnGroup = "set -m\n" +
		`sh -c 'echo $$ > jobpid; trap "" HUP TERM; : > ready; while :; do sleep 1; done'` + "\n"
)

func writeTestShell(t *testing.T, body string) string {
	t.Helper()
	// The name must not look like zsh or bash, or NewSession injects its shell
	// integration env into it.
	script := filepath.Join(t.TempDir(), "testchild")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// startTestShell starts a session running body. The script's working directory
// is cwd, so it can drop marker files under relative names.
func startTestShell(t *testing.T, cwd, body string) *Session {
	t.Helper()
	s, err := NewSession(writeTestShell(t, body), t.TempDir(), 80, 24, cwd)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// drainPTY keeps the master read so the child never blocks writing to it, and
// closes the returned channel at EOF — that is, once the child is gone. This is
// what readLoop does in production.
func drainPTY(s *Session) <-chan struct{} {
	eof := make(chan struct{})
	go func() {
		defer close(eof)
		buf := make([]byte, 1024)
		for {
			if _, err := s.Read(buf); err != nil {
				return
			}
		}
	}()
	return eof
}

// waitForReady blocks until the script has installed its traps. Polling beats a
// fixed sleep: the assertions are about the ladder, not about how long a fork
// and exec took on a loaded machine.
func waitForReady(t *testing.T, dir string) {
	t.Helper()
	marker := filepath.Join(dir, "ready")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("test shell never became ready (no %s)", marker)
}

// shortenLadder runs the shutdown ladder on test timescales. These are package
// vars and the tests never run in parallel, so this is safe as long as each
// test's sessions are closed before it returns — which t.Cleanup guarantees,
// since it unwinds in reverse and the restore below is registered first.
func shortenLadder(t *testing.T, hangup, terminate, kill, foreground time.Duration) {
	t.Helper()
	origHangup, origTerminate := hangupGrace, terminateGrace
	origKill, origForeground := killGrace, foregroundGrace
	t.Cleanup(func() {
		hangupGrace, terminateGrace = origHangup, origTerminate
		killGrace, foregroundGrace = origKill, origForeground
	})
	hangupGrace, terminateGrace = hangup, terminate
	killGrace, foregroundGrace = kill, foreground
}

// Closing a pane must leave the shell alive long enough to run its SIGHUP
// handler. That handler is where zsh writes ~/.zsh_history, and the old Close
// SIGKILLed one line after hanging up the tty — a pending SIGKILL is delivered
// before any handler runs, so every line typed in the pane died with it.
func TestCloseLetsTheShellHandleHangup(t *testing.T) {
	// A generous hangup grace on purpose: the assertion below is that Close
	// returns well inside it, which a shell that exits on HUP does in
	// milliseconds. A tight grace turns a loaded CI runner into a false
	// failure at the SIGTERM rung, which is not what this test is about.
	shortenLadder(t, 5*time.Second, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	dir := t.TempDir()
	s := startTestShell(t, dir, savesOnHangup)
	drainPTY(s)
	waitForReady(t, dir)

	start := time.Now()
	s.Close()
	elapsed := time.Since(start)

	if _, err := os.Stat(filepath.Join(dir, "history")); err != nil {
		t.Fatalf("the shell never ran its HUP handler, so a real zsh would have lost its history: %v", err)
	}
	if limit := hangupGrace / 2; elapsed > limit {
		t.Errorf("Close took %v; a shell that exits on the hangup must not wait out the %v grace", elapsed, limit)
	}
	if code, signal := s.ExitStatus(); code != 0 || signal != "" {
		t.Errorf("ExitStatus() = (%d, %q), want (0, \"\")", code, signal)
	}
}

// A shell that declines both polite signals still has to die — but only after
// it has been given both of its chances.
func TestCloseEscalatesWhenTheShellIgnoresSignals(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, 2*time.Second, 100*time.Millisecond)

	dir := t.TempDir()
	s := startTestShell(t, dir, signalsIgnored)
	pid := s.cmd.Process.Pid
	drainPTY(s)
	waitForReady(t, dir)

	start := time.Now()
	s.Close()
	elapsed := time.Since(start)

	if want := hangupGrace + terminateGrace; elapsed < want {
		t.Errorf("Close returned after %v; both grace periods (%v) must elapse before SIGKILL", elapsed, want)
	}
	if limit := hangupGrace + terminateGrace + 2*time.Second; elapsed > limit {
		t.Errorf("Close took %v, want under %v", elapsed, limit)
	}
	if code, signal := s.ExitStatus(); code != -1 || signal != "SIGKILL" {
		t.Errorf("ExitStatus() = (%d, %q), want (-1, \"SIGKILL\")", code, signal)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("shell %d is still alive after Close", pid)
	}
}

// The exit event used to report 0 for everything. A shell that exits on its own
// must report the status it chose.
func TestExitStatusReportsTheShellsOwnCode(t *testing.T) {
	s := startTestShell(t, t.TempDir(), "exit 3\n")

	// Wait for the PTY to report EOF, so the shell is gone on its own terms and
	// the ladder has nothing left to signal.
	select {
	case <-drainPTY(s):
	case <-time.After(10 * time.Second):
		t.Fatal("shell never exited")
	}
	s.Close()

	if code, signal := s.ExitStatus(); code != 3 || signal != "" {
		t.Errorf("ExitStatus() = (%d, %q), want (3, \"\")", code, signal)
	}
}

// The other half of the contract: a pane the app closes reports the signal that
// ended the shell rather than a fabricated 0.
func TestExitStatusReportsTheSignalThatEndedTheShell(t *testing.T) {
	shortenLadder(t, 2*time.Second, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	dir := t.TempDir()
	s := startTestShell(t, dir, idle)
	drainPTY(s)
	waitForReady(t, dir)

	s.Close()

	if code, signal := s.ExitStatus(); code != -1 || signal != "SIGHUP" {
		t.Errorf("ExitStatus() = (%d, %q), want (-1, \"SIGHUP\")", code, signal)
	}
}

// The pane's close button and the read loop reaching EOF both call Close, and
// they race. The second caller must block until the ladder has finished, so the
// status it then reads is the real one.
func TestCloseIsIdempotentAndConcurrencySafe(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	dir := t.TempDir()
	s := startTestShell(t, dir, idle)
	drainPTY(s)
	waitForReady(t, dir)

	var closers sync.WaitGroup
	for range 2 {
		closers.Add(1)
		go func() {
			defer closers.Done()
			s.Close()
			if code, signal := s.ExitStatus(); code != -1 || signal != "SIGHUP" {
				t.Errorf("ExitStatus() after a concurrent Close = (%d, %q), want (-1, \"SIGHUP\")", code, signal)
			}
		}()
	}
	closers.Wait()
	s.Close()

	// The shell has been reaped, so its pid may already belong to a stranger.
	if cwd, err := s.CWD(); err == nil {
		t.Errorf("CWD() on a closed session returned %q; it must not probe a reaped pid", cwd)
	}
	if cmd := s.ForegroundProcess(); cmd != "" {
		t.Errorf("ForegroundProcess() on a closed session returned %q; it must not probe a reaped pid", cmd)
	}
}

// readJobPid returns the pid the foreground job recorded for itself.
func readJobPid(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "jobpid"))
	if err != nil {
		t.Fatalf("the job never recorded its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("unreadable job pid %q: %v", data, err)
	}
	return pid
}

// processGroupOf asks ps which process group a pid belongs to.
func processGroupOf(t *testing.T, pid int) int {
	t.Helper()
	out, err := exec.Command("ps", "-o", "pgid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps -o pgid= -p %d: %v", pid, err)
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("unreadable pgid %q for pid %d: %v", out, pid, err)
	}
	return pgid
}

// startForegroundJob starts a session running body — a script that turns job
// control on and runs one foreground job — and returns the job's pid.
//
// It fails the test unless the job really did end up in a process group of its
// own that holds the terminal. Those two facts are what make the tests below
// reproduce the bug instead of passing by accident, and they depend on the
// shell: every other script in this file leaves job control off, where children
// share the shell's group, kill(-shellpid, …) reaches them, and the gap this is
// about is invisible.
func startForegroundJob(t *testing.T, dir, body string) (*Session, int) {
	t.Helper()
	s := startTestShell(t, dir, body)
	drainPTY(s)
	waitForReady(t, dir)

	jobPid := readJobPid(t, dir)
	t.Cleanup(func() {
		// Never leak the job, however the test ends. On the passing path it has
		// been gone for a while and this is an ESRCH no-op.
		_ = syscall.Kill(jobPid, syscall.SIGKILL)
	})

	if pgid := processGroupOf(t, jobPid); pgid == s.cmd.Process.Pid {
		t.Fatalf("precondition failed: job %d is in the shell's own process group (%d), so `set -m` gave it no group of its own and there is no bug left to reproduce — run the job under an interactive shell instead (`bash --norc -i`, `zsh -f`)", jobPid, pgid)
	}
	// The shell hands the terminal over around the fork, so poll rather than race
	// it. A non-empty name means the tty's foreground group is not the shell's —
	// which is the group Close has to read before it closes the master.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if s.ForegroundProcess() != "" {
			return s, jobPid
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("precondition failed: the tty's foreground group is still the shell, so the job never took the terminal")
	return nil, 0
}

// waitForJobToDie polls until the job is gone, and fails with why after d.
func waitForJobToDie(t *testing.T, jobPid int, d time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if err := syscall.Kill(jobPid, 0); err != nil {
			return // ESRCH: the job is gone, which is the whole point
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d was still alive %v after Close returned: %s", jobPid, d, why)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Closing a pane must hang up on the command it was running straight away, and
// on its own account — not as a side effect of the shell's timing.
//
// The kernel does hang up on the tty's foreground group, but only as the parting
// act of the controlling process, so the job hears about it whenever the shell
// happens to exit. Here the shell does not exit politely: it traps both polite
// signals and only dies at the SIGKILL rung, two graces into the ladder. The
// ordinary job it is running must have been hung up long before that.
func TestCloseHangsUpOnTheForegroundJobImmediately(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, 500*time.Millisecond, 300*time.Millisecond)

	dir := t.TempDir()
	s, jobPid := startForegroundJob(t, dir, stuckShellWithOrdinaryJob)

	// Watch the job from before Close, so the moment it dies is the measurement.
	died := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		for time.Since(start) < 5*time.Second {
			if err := syscall.Kill(jobPid, 0); err != nil {
				died <- time.Since(start)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	s.Close()

	select {
	case d := <-died:
		if d >= hangupGrace {
			t.Errorf("the job was only hung up after %v — that is the shell's %v grace running out and the kernel hanging up on the job for us, not the pane close doing it", d, hangupGrace)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("job %d outlived Close", jobPid)
	}
}

// The stubborn half of the same problem. A real shell is interactive, so job
// control is on and every foreground job runs in a process group of its own:
// kill(-shellpid, …) reaches the shell and nothing else. The shell dies on the
// first rung, Close returns, and a job that ignores SIGHUP and SIGTERM — an npm
// run dev holding a port — is left behind. That is the "N commands are still
// running and will be terminated" dialog promising something that did not happen.
func TestCloseKillsAForegroundJobInItsOwnProcessGroup(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	dir := t.TempDir()
	s, jobPid := startForegroundJob(t, dir, foregroundJobInItsOwnGroup)

	s.Close()

	// Every rung of both ladders, plus room for a loaded machine.
	budget := hangupGrace + terminateGrace + killGrace + 3*foregroundGrace + 2*time.Second
	waitForJobToDie(t, jobPid, budget, "it ignores SIGHUP and SIGTERM and has a process group of its own, so only a SIGKILL aimed at that group ends it")
}

// Quitting with a full window must cost one ladder, not one per pane: closing
// 17 shells sequentially would hang the quit for the better part of a minute.
func TestCloseAllRunsTheLaddersInParallel(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, 2*time.Second, 100*time.Millisecond)

	// No Wails context: readLoop's event emits are nil-guarded, so the manager
	// runs headless here.
	m := NewManager(writeTestShell(t, signalsIgnored), t.TempDir())

	const panes = 4
	dirs := make([]string, panes)
	for i := range dirs {
		dirs[i] = t.TempDir()
		if _, err := m.CreateSession(80, 24, dirs[i]); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	for _, dir := range dirs {
		waitForReady(t, dir)
	}

	start := time.Now()
	m.CloseAll()
	elapsed := time.Since(start)

	if limit := hangupGrace + terminateGrace + 2*time.Second; elapsed > limit {
		t.Errorf("CloseAll of %d panes took %v; it must cost one ladder (under %v), not one per pane", panes, elapsed, limit)
	}
	if got := len(m.GetAllSessionStatuses()); got != 0 {
		t.Errorf("CloseAll left %d sessions behind", got)
	}
}

// --- session liveness --------------------------------------------------------

// HasSession is what App.SessionExists answers with, and the frontend takes a
// false for "this shell is gone". It must therefore track the map, not the
// process: a session is listed from the moment CreateSession returns its id
// until it is closed.
func TestHasSessionFollowsTheSessionsLifetime(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	m := NewManager(writeTestShell(t, idle), t.TempDir())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForReady(t, dir)

	if !m.HasSession(id) {
		t.Fatal("HasSession is false for a session that was just created")
	}
	if m.HasSession(id + "-not-a-session") {
		t.Error("HasSession is true for an id that was never handed out")
	}

	m.CloseSession(id)

	// CloseSession removes the session before it runs the ladder, so this is
	// already decided by the time it returns — the frontend must never be told a
	// pane it just closed is still alive.
	if m.HasSession(id) {
		t.Error("HasSession is still true after CloseSession")
	}
}

// The load-bearing half of the same contract, and the reason a pane could go
// silently dead. A shell that exits at once — a broken rc file, a $SHELL that
// returns immediately — can emit pty:exit before the frontend's subscription
// exists, and Wails does not replay events. The frontend's defence is to ask
// HasSession once it has subscribed, which only works if the session is out of
// the map before the event goes out: "still listed" has to mean "the event is
// still ahead of you". Emitting first reopens the window.
func TestReadLoopRemovesTheSessionBeforeEmittingExit(t *testing.T) {
	type exitEvent struct {
		stillListed bool
		payload     PtyExit
	}

	m := NewManager(writeTestShell(t, "exit 7\n"), t.TempDir())
	// A non-nil context is all readLoop checks before emitting; the emitter
	// below stands in for the Wails runtime, which is not running here.
	m.SetContext(context.Background())

	seen := make(chan exitEvent, 1)
	// Restore first so it unwinds last: CloseAll waits for the read loops, so no
	// goroutine is still reading this var when the original goes back.
	original := emitEvent
	t.Cleanup(func() { emitEvent = original })
	t.Cleanup(m.CloseAll)
	emitEvent = func(_ context.Context, event string, data ...interface{}) {
		// Keyed off the event name rather than the id CreateSession returns: the
		// shell can be gone before that assignment happens, which is the entire
		// point of this test.
		sessionID, ok := strings.CutPrefix(event, "pty:exit:")
		if !ok {
			return // output batches are not what this is about
		}
		var payload PtyExit
		if len(data) == 1 {
			payload, _ = data[0].(PtyExit)
		}
		seen <- exitEvent{stillListed: m.HasSession(sessionID), payload: payload}
	}

	id, err := m.CreateSession(80, 24, t.TempDir())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	select {
	case ev := <-seen:
		if ev.stillListed {
			t.Error("pty:exit was emitted while the session was still listed: a frontend that subscribed just after that check would wait forever for an event it had already missed")
		}
		if ev.payload.ExitCode != 7 || ev.payload.Signal != "" {
			t.Errorf("pty:exit payload = %+v, want {ExitCode:7 Signal:\"\"}", ev.payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the shell exited but no pty:exit event was emitted")
	}

	if m.HasSession(id) {
		t.Error("HasSession is true after the shell exited on its own")
	}
}

// --- the quit path -----------------------------------------------------------

// beforeClose asks this on every Cmd-Q and it has to agree with what
// GetAllSessionStatuses calls busy — without paying for that call's lsof per
// pane, which is ~12ms of forking the answer never looks at.
func TestRunningCommandCount(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	t.Run("an idle pane is not a running command", func(t *testing.T) {
		m := NewManager(writeTestShell(t, idle), t.TempDir())
		t.Cleanup(m.CloseAll)

		dir := t.TempDir()
		if _, err := m.CreateSession(80, 24, dir); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		waitForReady(t, dir)

		if got := m.RunningCommandCount(); got != 0 {
			t.Errorf("RunningCommandCount() = %d for an idle pane, want 0; the quit dialog would interrupt the user for nothing", got)
		}
	})

	t.Run("a foreground job is", func(t *testing.T) {
		m := NewManager(writeTestShell(t, foregroundJobInItsOwnGroup), t.TempDir())
		t.Cleanup(m.CloseAll)

		dir := t.TempDir()
		if _, err := m.CreateSession(80, 24, dir); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		waitForReady(t, dir)
		jobPid := readJobPid(t, dir)
		t.Cleanup(func() {
			// Never leak the job, however the test ends; an ESRCH no-op on the
			// passing path, where CloseAll has already killed it.
			_ = syscall.Kill(jobPid, syscall.SIGKILL)
		})

		// The shell hands the terminal over around the fork, so poll rather than
		// race it.
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if m.RunningCommandCount() == 1 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("RunningCommandCount() = %d with a job in the foreground, want 1", m.RunningCommandCount())
	})
}
