package ptymanager

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

	if got, why := resolveStartDir(target, testHome); got != target {
		t.Errorf("permission error must preserve the directory:\n got %q (%s)\nwant %q", got, why, target)
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
		// wantWhy is whether a reason must be given. It is what the spawn log
		// prints, and a fallback the log cannot explain is the bug this pair of
		// return values exists for.
		wantWhy bool
	}{
		{"existing directory is used", existing, existing, false},
		{"empty request falls back to home", "", testHome, true},
		{"missing directory falls back to home", filepath.Join(existing, "gone"), testHome, true},
		{"a file is not a working directory", file, testHome, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := resolveStartDir(tt.cwd, testHome)
			if got != tt.want {
				t.Errorf("resolveStartDir(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
			if (why != "") != tt.wantWhy {
				t.Errorf("resolveStartDir(%q) reason = %q, want a reason: %v", tt.cwd, why, tt.wantWhy)
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
	// sleepJobInItsOwnGroup execs its foreground job into a sleep, so the tty's
	// foreground group is led by a process genuinely named "sleep" — the name
	// ForegroundProcess has to report now that it reads it from the kernel
	// instead of forking ps.
	sleepJobInItsOwnGroup = "set -m\n" +
		`sh -c 'echo $$ > jobpid; : > ready; exec sleep 30'` + "\n"

	// speaksThenIdles says three things and then blocks, so a test can attach
	// long after the output was produced and still expect all of it, in order.
	speaksThenIdles = "echo one\necho two\necho three\n" + idle
	// dyingWords fails the way a broken rc file does: it says why, then exits —
	// before any frontend could have subscribed to hear it. The marker is its
	// last act before the exit, so a test can wait for the failure instead of
	// sleeping at it.
	dyingWords = "echo could-not-start\n: > ready\nexit 5\n"
	// ticks trickles output forever. Slow on purpose: a pane that has to keep
	// streaming for the length of a test must not be the one that trips flow
	// control while doing it.
	ticks = ": > ready\nwhile :; do echo tick; sleep 0.02; done\n"
)

// floodBytes is how much the producer below writes. Comfortably several
// high-water marks, so the reader has to park more than once to deliver it all.
const floodBytes = 4 << 20

// floods writes floodBytes of 'x' to the tty as fast as the kernel will take
// it. No newlines in the payload, so the tty's ONLCR translation cannot change
// the byte count out from under an assertion, and nothing else is written: the
// pane receives exactly floodBytes or the test has found something.
var floods = fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x\n", floodBytes)

func writeTestShell(t testing.TB, body string) string {
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
func waitForReady(t testing.TB, dir string) {
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
func shortenLadder(t testing.TB, hangup, terminate, kill, foreground time.Duration) {
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

// shortenAttachGrace runs the exit path's wait for a late attach on test
// timescales. Same rules as shortenLadder: a package var, no parallel tests, and
// every session closed before the test returns.
func shortenAttachGrace(t testing.TB, d time.Duration) {
	t.Helper()
	original := attachGrace
	t.Cleanup(func() { attachGrace = original })
	attachGrace = d
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
	// Nothing attaches here, so the exit path would otherwise hold its flush for
	// the full production grace before getting to the ordering this is about.
	shortenAttachGrace(t, 50*time.Millisecond)

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

// --- process probes (R-08) ---------------------------------------------------

// The pane title and the layout file both come from this, and it used to fork
// lsof to get it. The kernel reports the resolved path, so the comparison has to
// resolve too: t.TempDir hands out something under /var, which on macOS is a
// symlink to /private/var.
func TestCWDReportsTheShellsWorkingDirectory(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}

	s := startTestShell(t, dir, idle)
	drainPTY(s)
	waitForReady(t, dir)

	got, err := s.CWD()
	if err != nil {
		t.Fatalf("CWD: %v", err)
	}
	if got != want {
		t.Errorf("CWD() = %q, want %q", got, want)
	}
}

// The status modal and the quit dialog both compare against these names, so
// reading them from the kernel rather than from `ps -o comm=` has to produce the
// same strings — a basename, not a path and not a 16-character truncation.
func TestForegroundProcessNamesTheRunningCommand(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	dir := t.TempDir()
	s, _ := startForegroundJob(t, dir, sleepJobInItsOwnGroup)

	// The job execs into sleep just after it drops its marker, so poll rather
	// than race the exec.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if s.ForegroundProcess() == "sleep" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("ForegroundProcess() = %q, want %q", s.ForegroundProcess(), "sleep")
}

// The status modal polls this every two seconds. With lsof and ps it cost ~12 ms
// and ~2 ms of forking per session — 46 ms per call at four panes, measured, and
// four times that at seventeen. Both probes are syscalls now.
func BenchmarkGetAllSessionStatuses(b *testing.B) {
	shortenLadder(b, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	m := NewManager(writeTestShell(b, idle), b.TempDir())
	b.Cleanup(m.CloseAll)

	const panes = 4
	for range panes {
		dir := b.TempDir()
		if _, err := m.CreateSession(80, 24, dir); err != nil {
			b.Fatalf("CreateSession: %v", err)
		}
		waitForReady(b, dir)
	}

	for b.Loop() {
		m.GetAllSessionStatuses()
	}
}

// --- the output bridge (R-11, R-12, R-20) ------------------------------------

// emitRecorder stands in for the Wails runtime, which is not running in a test.
// It keeps every event in emission order and decodes the output ones, because
// what these tests are about is not only what reached the pane but in what
// order it got there.
type emitRecorder struct {
	mu     sync.Mutex
	names  []string
	output map[string][]byte
	exits  map[string]PtyExit
	// hook runs before an event is recorded, on the goroutine that emitted it.
	// It is where a test injects a panic into the bridge; set it before the
	// first session exists, so no goroutine is reading it while it is written.
	hook func(event string)
}

// installRecorder swaps the emit seam for a recorder and restores it on
// cleanup. Call it *before* registering anything that stops the read loops, so
// the restore unwinds last: a goroutine still emitting into a var being written
// back is a data race, and one the race detector will find.
func installRecorder(t testing.TB) *emitRecorder {
	r := &emitRecorder{
		output: make(map[string][]byte),
		exits:  make(map[string]PtyExit),
	}
	original := emitEvent
	t.Cleanup(func() { emitEvent = original })
	emitEvent = func(_ context.Context, event string, data ...interface{}) {
		r.record(event, data)
	}
	return r
}

func (r *emitRecorder) record(event string, data []interface{}) {
	r.mu.Lock()
	hook := r.hook
	r.mu.Unlock()
	if hook != nil {
		hook(event) // may panic: that is what R-20 is about
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, event)

	if id, ok := strings.CutPrefix(event, "pty:output:"); ok {
		if len(data) == 1 {
			encoded, _ := data[0].(string)
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				panic("pty:output carried something that is not base64: " + err.Error())
			}
			r.output[id] = append(r.output[id], decoded...)
		}
		return
	}
	if id, ok := strings.CutPrefix(event, "pty:exit:"); ok {
		if len(data) == 1 {
			r.exits[id], _ = data[0].(PtyExit)
		}
	}
}

// outputOf returns everything emitted for one session, decoded and concatenated
// in emission order.
func (r *emitRecorder) outputOf(id string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.output[id]...)
}

// outputLen is the same measurement without the copy, for polling loops.
func (r *emitRecorder) outputLen(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.output[id])
}

// eventNames returns every event name in emission order.
func (r *emitRecorder) eventNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

// exitOf reports the session's pty:exit payload and whether one has been
// emitted at all — which, on the exit path, is a question about timing.
func (r *emitRecorder) exitOf(id string) (PtyExit, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	exit, ok := r.exits[id]
	return exit, ok
}

// waitForExit blocks until the session's pty:exit arrives, and fails the test if
// it never does.
func (r *emitRecorder) waitForExit(t *testing.T, id string, d time.Duration) PtyExit {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if exit, ok := r.exitOf(id); ok {
			return exit
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pty:exit for session %s within %v; emitted: %v", id, d, r.eventNames())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// sessionOf reaches into the manager for the session behind an id. Flow control
// has no reason to be visible from outside the package, so the tests that assert
// on it go through here.
func sessionOf(t *testing.T, m *Manager, id string) *Session {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		t.Fatalf("session %s is no longer in the map", id)
	}
	return s
}

// waitUntilPaused blocks until the session's reader has parked on flow control,
// and returns the credit it stopped at.
func waitUntilPaused(t *testing.T, s *Session, rec *emitRecorder, id string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		unacked, paused := s.flowState()
		if paused {
			return unacked
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reader never paused: unacked=%d with %d bytes already emitted and nothing acked — it is still draining the pty as fast as the shell fills it", unacked, rec.outputLen(id))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// containsInOrder reports whether every want appears in s, in the order given.
func containsInOrder(s string, want []string) bool {
	for _, w := range want {
		i := strings.Index(s, w)
		if i < 0 {
			return false
		}
		s = s[i+len(w):]
	}
	return true
}

// A shell dumping a large file used to flood the bridge: every batch is an
// evaluateJavaScript call on the webview's main thread, and nothing stopped the
// reader from producing them faster than the UI could render them. With no acks
// at all the reader must come to a complete stop — not slow down — and stay
// stopped until the frontend catches up.
func TestOutputBackpressureStopsTheReader(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, floods), t.TempDir())
	// A non-nil, live context is all the emit guard checks; the recorder above
	// stands in for the runtime it would otherwise reach.
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	id, err := m.CreateSession(80, 24, t.TempDir())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !m.AttachSession(id) {
		t.Fatal("AttachSession returned false for a session that was just created")
	}
	session := sessionOf(t, m, id)

	unacked := waitUntilPaused(t, session, rec, id)
	if unacked < highWater {
		t.Errorf("the reader parked at unacked=%d, below the %d high-water mark", unacked, highWater)
	}
	// It may overshoot by the read that crossed the mark, and by no more: the
	// check happens before each read, so at most one read's worth gets through.
	if limit := highWater + readBufSize; unacked > limit {
		t.Errorf("the reader parked at unacked=%d, more than one read past the mark (%d)", unacked, limit)
	}

	// Let the flusher hand over whatever it was still holding, then watch: parked
	// means parked, however long the producer is left pushing.
	time.Sleep(10 * batchInterval)
	settled := rec.outputLen(id)
	time.Sleep(200 * time.Millisecond)
	if grew := rec.outputLen(id) - settled; grew != 0 {
		t.Errorf("%d more bytes were emitted while the reader was parked; it is not the pty that is being throttled", grew)
	}

	// Acking releases it, and the file arrives whole. Nothing is dropped by the
	// pause — it is the tty buffer that holds the backlog, and the child that
	// blocks on it.
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
			t.Fatalf("only %d of %d bytes arrived after acking %d of them; the reader never resumed", got, floodBytes, acked)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := rec.outputLen(id); got != floodBytes {
		t.Errorf("the pane received %d bytes, want exactly %d", got, floodBytes)
	}
}

// Over-acking is a frontend bug, and it must not turn into an unbounded reader.
func TestAckOutputIsUnfazedByNonsense(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	m := NewManager(writeTestShell(t, idle), t.TempDir())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForReady(t, dir)
	session := sessionOf(t, m, id)

	m.AckOutput(id, 1<<30)
	m.AckOutput(id, -5)
	m.AckOutput(id, 0)
	m.AckOutput("not-a-session", 1024)

	if unacked, _ := session.flowState(); unacked != 0 {
		t.Errorf("unacked = %d after acking far more than was ever sent, want 0: credit must never go negative", unacked)
	}
}

// A pane closed while its reader is parked must not leave that goroutine behind.
// Nothing below the close can satisfy the condition it is waiting on — the
// frontend that owed the acks has already thrown the pane away — so Close has to
// wake it explicitly.
func TestCloseWakesAPausedReader(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, floods), t.TempDir())
	m.SetContext(context.Background())

	id, err := m.CreateSession(80, 24, t.TempDir())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Not CloseAll: on the failing path that would block on the very goroutine
	// this is about, and a hung cleanup is a worse report than a failed
	// assertion. CloseSession only runs the ladder.
	t.Cleanup(func() { m.CloseSession(id) })

	if !m.AttachSession(id) {
		t.Fatal("AttachSession returned false for a session that was just created")
	}
	waitUntilPaused(t, sessionOf(t, m, id), rec, id)

	// CloseAll waits on the manager's WaitGroup, which both the flusher and the
	// reader are registered with. A reader still parked on credit would hang it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.CloseAll()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("CloseAll did not return: the parked reader was never released, so its goroutine is still waiting for credit on a pane that no longer exists")
	}

	if m.HasSession(id) {
		t.Error("CloseAll left the session in the map")
	}
}

// Output produced before the frontend subscribed used to be emitted into nothing
// and lost — Wails does not replay events, and CreateSession starts reading the
// pty before it has even returned the id. A session now starts unattached and
// holds its output until the frontend says it is listening.
func TestAttachFlushesOutputBufferedBeforeTheSubscription(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, speaksThenIdles), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// The marker is written after the three echoes, so by here the shell has
	// definitely produced everything this test is waiting for.
	waitForReady(t, dir)

	// Several flusher ticks, to tell "holding it back" apart from "hasn't got
	// round to it".
	time.Sleep(10 * batchInterval)
	if got := rec.outputLen(id); got != 0 {
		t.Fatalf("an unattached session emitted %d bytes (%q); nobody is subscribed yet, so those events are lost", got, rec.outputOf(id))
	}

	if !m.AttachSession(id) {
		t.Fatal("AttachSession returned false for a live session")
	}

	want := []string{"one", "two", "three"}
	for deadline := time.Now().Add(10 * time.Second); ; {
		if containsInOrder(string(rec.outputOf(id)), want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after attaching, the pane never got the output its shell produced before it subscribed, or got it out of order: %q", rec.outputOf(id))
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Attaching again is what a reload does. It must not replay anything.
	before := rec.outputLen(id)
	if !m.AttachSession(id) {
		t.Error("a second AttachSession on a live session returned false")
	}
	time.Sleep(10 * batchInterval)
	if after := rec.outputLen(id); after != before {
		t.Errorf("a second AttachSession emitted %d more bytes; attaching has to be idempotent", after-before)
	}

	if m.AttachSession(id + "-not-a-session") {
		t.Error("AttachSession returned true for an id that was never handed out")
	}
}

// assertOutputPrecedesExit checks that the pane's buffered output went out
// ahead of its exit. An exit that overtakes it reaches a frontend which has
// already torn the pane down, which loses the line just as surely as emitting it
// into no subscription at all.
func assertOutputPrecedesExit(t *testing.T, rec *emitRecorder, id string) {
	t.Helper()
	names := rec.eventNames()
	exitAt := slices.Index(names, "pty:exit:"+id)
	outputAt := slices.Index(names, "pty:output:"+id)
	if exitAt < 0 || outputAt < 0 || outputAt > exitAt {
		t.Errorf("events went out in the order %v; the buffered output has to precede the exit", names)
	}
}

// The other half of R-12, and the case the attach buffer exists for. A shell
// that fails to start says why and dies before the frontend could possibly have
// attached: it only learns the session id when the CreateSession promise
// resolves — a full round trip — and subscribes after that.
//
// The exit path used to walk straight past all of it and force the flush.
// Emitting is not delivering: Wails does not buffer events, so a batch emitted
// into a subscription that does not exist yet never happened, and the pane
// rendered "[Process exited with code 1]" with nothing above it to say why. So
// an unattached session's dying words are held for a bounded grace first.
func TestExitWaitsForALateAttachBeforeFlushing(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	// Generous on purpose: the settle below has to be unmistakably inside the
	// grace even on a loaded machine, or this measures the wrong thing.
	shortenAttachGrace(t, 5*time.Second)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, dyingWords), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// The marker is the shell's last act before `exit 5`, so by here it has said
	// its piece and is on its way out.
	waitForReady(t, dir)

	// Several flusher ticks. This is the window in which a real frontend holds
	// the id and has not finished subscribing, and nothing may go out into it.
	time.Sleep(10 * batchInterval)
	if got := rec.outputLen(id); got != 0 {
		t.Fatalf("the dying shell's output was emitted %d bytes deep (%q) before anything attached; nothing is subscribed in that window, so a real frontend receives none of it", got, rec.outputOf(id))
	}
	if exit, ok := rec.exitOf(id); ok {
		t.Fatalf("pty:exit (%+v) went out before anything attached, so the pane it belongs to never hears it", exit)
	}

	// The frontend gets there, inside the grace, as it does in practice.
	if !m.AttachSession(id) {
		t.Fatal("AttachSession returned false while the exit path was still holding this session's output: the pane is told to give up on output that has not been sent yet")
	}

	if exit := rec.waitForExit(t, id, 10*time.Second); exit.ExitCode != 5 || exit.Signal != "" {
		t.Errorf("pty:exit payload = %+v, want {ExitCode:5 Signal:\"\"}", exit)
	}
	if out := string(rec.outputOf(id)); !strings.Contains(out, "could-not-start") {
		t.Errorf("the pane attached inside the grace and still never got the shell's last words: got %q", out)
	}
	assertOutputPrecedesExit(t, rec, id)
}

// The other side of the same trade: nothing ever attaches. The grace is a bound,
// not a promise — when it runs out the output goes to whoever is listening, and
// the exit follows it. Holding a dead pane's bytes any longer would be worse
// than losing them: the frontend would never be told the shell had died at all,
// and a pane that is merely silent is one the user keeps typing into.
//
// A frontend that misses this flush is the case AttachSession answers for: the
// session is out of the map by then, false comes back, and the pane renders as
// "status unknown" rather than waiting for events that are already behind it.
func TestExitFlushesUnattachedOutputWhenTheGraceRunsOut(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	const grace = 250 * time.Millisecond
	shortenAttachGrace(t, grace)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, dyingWords), t.TempDir())
	m.SetContext(context.Background())
	t.Cleanup(m.CloseAll)

	// Measured from before the shell even exists, so it is a strict lower bound
	// on a grace that only starts once the shell has died.
	start := time.Now()
	id, err := m.CreateSession(80, 24, t.TempDir())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Deliberately never attached.

	if exit := rec.waitForExit(t, id, 10*time.Second); exit.ExitCode != 5 || exit.Signal != "" {
		t.Errorf("pty:exit payload = %+v, want {ExitCode:5 Signal:\"\"}", exit)
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Errorf("pty:exit arrived after %v, well inside the %v grace: the exit path never waited for the frontend, so a shell that dies during startup still loses its output", elapsed, grace)
	}
	if out := string(rec.outputOf(id)); !strings.Contains(out, "could-not-start") {
		t.Errorf("the shell's last words were dropped once the grace ran out; the flush past it is best effort, not no effort: got %q", out)
	}
	assertOutputPrecedesExit(t, rec, id)

	if m.AttachSession(id) {
		t.Error("AttachSession returned true after the exit path was done with the session; a frontend that arrives this late has to be told the pane is gone")
	}
}

// The grace must never be something a quit waits on. A pane whose shell has just
// died sits in it holding its last output, and CloseAll waits on that read loop
// — so without the close ending the wait, quitting a window with one dying pane
// costs the full grace, every time, for a pane that is already gone.
func TestClosingEndsTheAttachGraceWait(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)
	// Long enough that sitting the grace out would be unmistakable.
	const grace = 10 * time.Second
	shortenAttachGrace(t, grace)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, dyingWords), t.TempDir())
	m.SetContext(context.Background())

	dir := t.TempDir()
	id, err := m.CreateSession(80, 24, dir)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForReady(t, dir)
	// Several ticks, so the exit path is certainly parked in the grace by now.
	time.Sleep(10 * batchInterval)
	if got := rec.outputLen(id); got != 0 {
		t.Fatalf("precondition failed: %d bytes were already emitted, so the exit path is not holding anything and there is no wait to interrupt", got)
	}

	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.CloseAll()
	}()
	select {
	case <-done:
	case <-time.After(grace / 2):
		t.Fatalf("CloseAll was still running %v in: it is sitting out the attach grace of a pane whose shell has already died, which is the quit hanging on a dead pane", grace/2)
	}
	if elapsed, limit := time.Since(start), 3*time.Second; elapsed > limit {
		t.Errorf("CloseAll took %v with one pane holding output in a %v attach grace; closing has to end that wait, not join it", elapsed, grace)
	}

	if m.HasSession(id) {
		t.Error("CloseAll left the session in the map")
	}
}

// One panicking goroutine used to take the whole window with it: a panic at the
// top of a read loop is a panic at the top of the process. Only the pane that
// caused it may die, and it has to die completely — out of the map, one pty:exit,
// no goroutine left over.
func TestAPanicTakesDownOnlyItsOwnSession(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, ticks), t.TempDir())
	m.SetContext(context.Background())

	// Whichever pane emits first is the victim; which one that is comes down to
	// scheduling, so the hook picks it rather than the test. Installed before any
	// session exists, so nothing is reading it while it is written.
	victimCh := make(chan string, 1)
	var once sync.Once
	rec.hook = func(event string) {
		id, ok := strings.CutPrefix(event, "pty:output:")
		if !ok {
			return // the teardown's own pty:exit must get through
		}
		chosen := false
		once.Do(func() {
			chosen = true
			victimCh <- id
		})
		if chosen {
			panic("an emit that goes wrong must not take the window with it")
		}
	}

	const panes = 2
	ids := make([]string, panes)
	for i := range ids {
		dir := t.TempDir()
		id, err := m.CreateSession(80, 24, dir)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		ids[i] = id
		waitForReady(t, dir)
	}
	// Registered after installRecorder so the emit seam is restored last, and
	// deliberately not CloseAll: the WaitGroup check below is the assertion, and
	// it should not be pre-empted by a hanging cleanup.
	t.Cleanup(func() {
		for _, id := range ids {
			m.CloseSession(id)
		}
	})
	for _, id := range ids {
		if !m.AttachSession(id) {
			t.Fatalf("AttachSession returned false for session %s", id)
		}
	}

	var victim string
	select {
	case victim = <-victimCh:
	case <-time.After(20 * time.Second):
		t.Fatal("no output was ever emitted, so the panic was never injected")
	}
	survivor := ids[0]
	if survivor == victim {
		survivor = ids[1]
	}

	// A panic in the bridge says nothing about how the shell fared, so the pane
	// is reported as unknown rather than as whatever the ladder happened to see.
	if exit := rec.waitForExit(t, victim, 10*time.Second); exit.ExitCode != -1 || exit.Signal != "" {
		t.Errorf("pty:exit for the panicking pane = %+v, want {ExitCode:-1 Signal:\"\"}", exit)
	}
	if m.HasSession(victim) {
		t.Error("the panicking session was left in the map, so the frontend would be told it is still alive")
	}

	// The whole point: the other pane never noticed.
	before := rec.outputLen(survivor)
	for deadline := time.Now().Add(10 * time.Second); rec.outputLen(survivor) <= before; {
		if time.Now().After(deadline) {
			t.Fatal("the surviving pane stopped emitting after the other one panicked")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// And it left nothing behind. CloseAll waits on the WaitGroup that both the
	// flusher and the reader are registered with, so a stranded reader — one
	// blocked handing a batch to a flusher that is no longer there — hangs it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.CloseAll()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("CloseAll did not return: the panicking session left a goroutine behind")
	}
}

// Containing a panic is only containment if the containment cannot panic.
// recoverFlusher runs the shutdown ladder, a map delete and one last m.emit with
// nothing to catch a second panic — and that emit is an EventsEmit onto a
// webview main thread that may already be tearing down, which is exactly the
// call likeliest to go wrong next. A panic there escaped the deferred call that
// was containing the first one, reached the top of the goroutine, and closed
// every pane in the window: the same failure the recover handlers were added to
// prevent, one level further in.
func TestAPanicInTheRecoverHandlerDoesNotTakeTheProcess(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	rec := installRecorder(t)
	m := NewManager(writeTestShell(t, ticks), t.TempDir())
	m.SetContext(context.Background())

	// Whichever pane emits first is the victim, and from then on every event of
	// its own panics: the output batch that kills its read loop, and then the
	// pty:exit that its recover handler tries to send while tearing it down.
	// Installed before any session exists, so nothing is reading it while it is
	// written.
	victimCh := make(chan string, 1)
	var victimMu sync.Mutex
	var victim string
	rec.hook = func(event string) {
		victimMu.Lock()
		chosen := victim
		if chosen == "" {
			if id, ok := strings.CutPrefix(event, "pty:output:"); ok {
				victim, chosen = id, id
				victimCh <- id
			}
		}
		victimMu.Unlock()

		if chosen == "" {
			return // nobody has been picked yet, and this is not an output event
		}
		if event == "pty:output:"+chosen || event == "pty:exit:"+chosen {
			panic("an emit into a runtime being torn down must not take the window with it")
		}
	}

	const panes = 2
	ids := make([]string, panes)
	for i := range ids {
		dir := t.TempDir()
		id, err := m.CreateSession(80, 24, dir)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		ids[i] = id
		waitForReady(t, dir)
	}
	// Registered after installRecorder so the emit seam is restored last, and
	// deliberately not CloseAll: the WaitGroup check below is the assertion.
	t.Cleanup(func() {
		for _, id := range ids {
			m.CloseSession(id)
		}
	})
	for _, id := range ids {
		if !m.AttachSession(id) {
			t.Fatalf("AttachSession returned false for session %s", id)
		}
	}

	var victimID string
	select {
	case victimID = <-victimCh:
	case <-time.After(20 * time.Second):
		t.Fatal("no output was ever emitted, so the panic was never injected")
	}
	survivor := ids[0]
	if survivor == victimID {
		survivor = ids[1]
	}

	// The teardown's own emit panicked, so there is no pty:exit to wait for. What
	// has to be true is that the teardown got as far as it could — the session is
	// out of the map — and that the process is still here to be asked.
	for deadline := time.Now().Add(10 * time.Second); m.HasSession(victimID); {
		if time.Now().After(deadline) {
			t.Fatal("the panicking session was never removed from the map, so the frontend would be told a dead pane is still alive")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The whole point: the other pane never noticed. Reaching this line at all is
	// half the assertion — before the fix the second panic ended the test binary.
	before := rec.outputLen(survivor)
	for deadline := time.Now().Add(10 * time.Second); rec.outputLen(survivor) <= before; {
		if time.Now().After(deadline) {
			t.Fatal("the surviving pane stopped emitting after the other one's teardown panicked")
		}
		time.Sleep(5 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.CloseAll()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("CloseAll did not return: the session whose teardown panicked left a goroutine behind")
	}
}

// --- the master's fd (R-15) --------------------------------------------------

// The probes hand the master's raw fd number to an ioctl, and Close gives that
// number back to the kernel. A descriptor is a small integer and the kernel
// reuses it at once, so a probe that got past the closed guard and into Fd() as
// the master went away used to ask TIOCGPGRP of whatever held the number next —
// realistically another pane's master, in this same process — and report that
// pane's foreground job as this one's, for this one's ladder to signal.
//
// Under -race this is also where the unsynchronised read of the fd shows up.
func TestFdProbesDoNotRaceTheMastersClose(t *testing.T) {
	shortenLadder(t, 300*time.Millisecond, 300*time.Millisecond, time.Second, 100*time.Millisecond)

	dir := t.TempDir()
	s := startTestShell(t, dir, idle)
	drainPTY(s)
	waitForReady(t, dir)

	// Several probers, so one of them is inside the ioctl when the master is
	// retired rather than merely near it.
	stop := make(chan struct{})
	var probing sync.WaitGroup
	for range 4 {
		probing.Add(1)
		go func() {
			defer probing.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.ForegroundProcess()
				s.Resize(80, 24)
			}
		}()
	}

	s.Close()
	close(stop)
	probing.Wait()

	// The master is retired, so every user of its fd has to answer "closed"
	// rather than answer for whatever the kernel has since done with the number.
	if err := s.Resize(100, 40); !errors.Is(err, os.ErrClosed) {
		t.Errorf("Resize after Close = %v, want %v: the tty it would reshape is no longer ours", err, os.ErrClosed)
	}
	if pgid := s.foregroundPgid(); pgid != 0 {
		t.Errorf("foregroundPgid() after Close = %d, want 0: the tty it asked is no longer ours", pgid)
	}
}
