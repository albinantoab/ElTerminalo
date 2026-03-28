package history

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Entry struct {
	ID        int64  `json:"id"`
	Command   string `json:"command"`
	CWD       string `json:"cwd"`
	ExitCode  int    `json:"exitCode"`
	Shell     string `json:"shell"`
	Timestamp int64  `json:"timestamp"`
	SessionID string `json:"sessionId"`
}

type SearchParams struct {
	Query string `json:"query"`
	CWD   string `json:"cwd"`
	Limit int    `json:"limit"`
}

type SearchResult struct {
	CWDMatches    []Entry `json:"cwdMatches"`
	GlobalMatches []Entry `json:"globalMatches"`
}

type Store struct {
	db *sql.DB
}

func NewStore(configDir string) (*Store, error) {
	dbPath := filepath.Join(configDir, "history.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open history db: %w", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS command_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			command TEXT NOT NULL,
			cwd TEXT NOT NULL,
			exit_code INTEGER,
			shell TEXT,
			timestamp INTEGER NOT NULL,
			session_id TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_history_cwd ON command_history(cwd);
		CREATE INDEX IF NOT EXISTS idx_history_timestamp ON command_history(timestamp DESC);
		CREATE INDEX IF NOT EXISTS idx_history_command ON command_history(command);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate history db: %w", err)
	}

	return &Store{db: db}, nil
}

// Prune removes old entries, keeping only the most recent maxEntries records.
func (s *Store) Prune(maxEntries int) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec(
		"DELETE FROM command_history WHERE id NOT IN (SELECT id FROM command_history ORDER BY timestamp DESC LIMIT ?)",
		maxEntries,
	)
	return err
}

// Add records a command. Skips if identical to the most recent entry for the same CWD.
func (s *Store) Add(command, cwd string, exitCode int, shell, sessionID string) error {
	if s.db == nil {
		return nil
	}
	if command == "" {
		return nil
	}

	// Consecutive dedup: skip if same command+cwd as last entry
	var lastCmd string
	err := s.db.QueryRow(
		"SELECT command FROM command_history WHERE cwd = ? ORDER BY timestamp DESC LIMIT 1",
		cwd,
	).Scan(&lastCmd)
	if err == nil && lastCmd == command {
		return nil
	}

	_, err = s.db.Exec(
		"INSERT INTO command_history (command, cwd, exit_code, shell, timestamp, session_id) VALUES (?, ?, ?, ?, ?, ?)",
		command, cwd, exitCode, shell, time.Now().Unix(), sessionID,
	)
	return err
}

// Search returns history entries: CWD-contextual matches first, then global.
func (s *Store) Search(params SearchParams) (SearchResult, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}

	result := SearchResult{
		CWDMatches:    []Entry{},
		GlobalMatches: []Entry{},
	}

	query := "%" + params.Query + "%"

	if s.db == nil {
		return result, nil
	}

	// CWD-contextual matches
	rows, err := s.db.Query(
		"SELECT id, command, cwd, exit_code, shell, timestamp, session_id FROM command_history WHERE cwd = ? AND command LIKE ? ORDER BY timestamp DESC LIMIT ?",
		params.CWD, query, limit,
	)
	if err != nil {
		return result, err
	}
	result.CWDMatches = scanEntries(rows)

	// Global matches (excluding CWD to avoid duplicates)
	rows, err = s.db.Query(
		"SELECT id, command, cwd, exit_code, shell, timestamp, session_id FROM command_history WHERE cwd != ? AND command LIKE ? ORDER BY timestamp DESC LIMIT ?",
		params.CWD, query, limit,
	)
	if err != nil {
		return result, err
	}
	result.GlobalMatches = scanEntries(rows)

	return result, nil
}

// Clear removes all history entries.
func (s *Store) Clear() error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec("DELETE FROM command_history")
	return err
}

// Close closes the database.
func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func scanEntries(rows *sql.Rows) []Entry {
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		var e Entry
		var sessionID sql.NullString
		if err := rows.Scan(&e.ID, &e.Command, &e.CWD, &e.ExitCode, &e.Shell, &e.Timestamp, &sessionID); err != nil {
			continue
		}
		e.SessionID = sessionID.String
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return []Entry{}
	}
	return entries
}
