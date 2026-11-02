package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type ChatSession struct {
	Project   string
	Agent     string
	Model     string
	SessionID string
	UpdatedAt time.Time
}

type Message struct {
	Role      string
	Content   string
	CreatedAt time.Time
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	statements := []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA busy_timeout = 5000;`,
		`CREATE TABLE IF NOT EXISTS chat_sessions (
			project_path TEXT PRIMARY KEY,
			agent TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS chat_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_path TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_chat_messages_project
			ON chat_messages(project_path, id);`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

func (s *Store) GetChatSession(ctx context.Context, project string) (ChatSession, bool, error) {
	var session ChatSession
	var updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT project_path, agent, model, session_id, updated_at
		FROM chat_sessions WHERE project_path = ?`, project).
		Scan(&session.Project, &session.Agent, &session.Model, &session.SessionID, &updated)
	if err == sql.ErrNoRows {
		return ChatSession{}, false, nil
	}
	if err != nil {
		return ChatSession{}, false, err
	}
	session.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return session, true, nil
}

func (s *Store) SaveChatSession(ctx context.Context, session ChatSession) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_sessions(project_path, agent, model, session_id, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(project_path) DO UPDATE SET
			agent = excluded.agent,
			model = excluded.model,
			session_id = excluded.session_id,
			updated_at = excluded.updated_at`,
		session.Project, session.Agent, session.Model, session.SessionID,
		time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) DeleteChatSession(ctx context.Context, project string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM chat_sessions WHERE project_path = ?`, project)
	return err
}

func (s *Store) AppendMessage(ctx context.Context, project, role, content string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_messages(project_path, role, content, created_at)
		VALUES(?, ?, ?, ?)`,
		project, role, content, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) Messages(ctx context.Context, project string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content, created_at
		FROM chat_messages
		WHERE project_path = ?
		ORDER BY id DESC
		LIMIT ?`, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var message Message
		var created string
		if err := rows.Scan(&message.Role, &message.Content, &created); err != nil {
			return nil, err
		}
		message.CreatedAt, _ = time.Parse(time.RFC3339, created)
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Query returned newest first; callers want chronological order.
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return messages, nil
}
