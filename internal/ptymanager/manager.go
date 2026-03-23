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

// SessionStatus describes the current state of a PTY session.
type SessionStatus struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Command   string `json:"command"`
	IsIdle    bool   `json:"isIdle"`
}

// Manager manages multiple PTY sessions and streams output via Wails events.
type Manager struct {
	ctx      context.Context
	shell    string
	sessions map[string]*Session
	mu       sync.Mutex
	wg       sync.WaitGroup
}

// NewManager creates a new PTY manager.
func NewManager(shell string) *Manager {
	return &Manager{
		shell:    shell,
		sessions: make(map[string]*Session),
	}
}

// SetContext sets the Wails runtime context (called during app startup).
func (m *Manager) SetContext(ctx context.Context) {
	m.ctx = ctx
}

// CreateSession spawns a new PTY and starts streaming output.
func (m *Manager) CreateSession(cols, rows int, cwd string) (string, error) {
	session, err := NewSession(m.shell, cols, rows, cwd)
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
				dataCh <- data
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
			wailsRuntime.EventsEmit(m.ctx, "pty:output:"+session.ID, encoded)
			accum = accum[:0]
		}
	}

	for {
		select {
		case data := <-dataCh:
			accum = append(accum, data...)
			if len(accum) > batchFlushBytes {
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
			if m.ctx != nil {
				wailsRuntime.EventsEmit(m.ctx, "pty:exit:"+session.ID, map[string]int{"exitCode": 0})
			}
			m.mu.Lock()
			delete(m.sessions, session.ID)
			m.mu.Unlock()
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
		if cwd, err := s.CWD(); err == nil {
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

// CloseAll terminates all PTY sessions and waits for readLoop goroutines to finish.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	for id, session := range m.sessions {
		session.Close()
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	m.wg.Wait()
}
