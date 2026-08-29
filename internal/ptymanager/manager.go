package ptymanager

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	readBufSize      = 4096
	batchChannelSize = 64
	batchFlushBytes  = 8192
	batchAccumCap    = 16384
	batchInterval    = 16 * time.Millisecond
)

// ErrSessionNotFound is returned when an operation targets a session that does not exist.
var ErrSessionNotFound = errors.New("session not found")

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
	session, err := NewSession(m.shell, m.configDir, cols, rows, cwd)
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

func (m *Manager) readLoop(session *Session) {
	defer m.wg.Done()

	dataCh := make(chan []byte, batchChannelSize)
	doneCh := make(chan struct{})

	// Reader goroutine -- blocks on PTY read
	go func() {
		buf := make([]byte, readBufSize)
		for {
			n, err := session.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				select {
				case dataCh <- data:
				case <-doneCh:
					return
				}
			}
			if err != nil {
				close(doneCh)
				return
			}
		}
	}()

	// Flusher -- batches output and sends via Wails events
	accum := make([]byte, 0, batchAccumCap)
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()

	flush := func() {
		if len(accum) > 0 && m.ctx != nil {
			encoded := base64.StdEncoding.EncodeToString(accum)
			emitEvent(m.ctx, "pty:output:"+session.ID, encoded)
			accum = accum[:0]
		}
	}

	for {
		select {
		case data := <-dataCh:
			accum = append(accum, data...)
			if len(accum) >= batchAccumCap {
				flush()
			} else if len(accum) > batchFlushBytes {
				flush()
			}
		case <-ticker.C:
			flush()
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
			flush()
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
			// dead. The frontend's defence is HasSession (App.SessionExists),
			// asked once the subscription is in place, and this ordering is what
			// makes that question decisive: "exists" can never mean "the event
			// is already behind you". Either the check runs after the delete and
			// sees the session gone, so the frontend synthesizes the exit, or it
			// sees the session present and the emit is still ahead of it —
			// reaching a subscription that, by then, exists.
			m.mu.Lock()
			delete(m.sessions, session.ID)
			m.mu.Unlock()

			if m.ctx != nil {
				code, signal := session.ExitStatus()
				emitEvent(m.ctx, "pty:exit:"+session.ID, PtyExit{ExitCode: code, Signal: signal})
			}
			return
		}
	}
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
// away. That one forks lsof per session for the CWD (~12 ms each) on top of the
// ps for the command name (~2 ms), and the only thing the quit path ever reads
// is whether the pane is busy — so a window of a dozen panes paid ~150 ms of
// subprocesses on every Cmd-Q, before the confirmation dialog could even be
// shown, for two numbers it discarded. Sequential is fine at the ps alone.
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
