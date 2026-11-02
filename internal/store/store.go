package store

import (
	"context"
	"database/sql"
	"encoding/json"
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

type Run struct {
	ID                string
	Project           string
	Goal              string
	BaseRef           string
	IntegrationBranch string
	Status            string
	CreatedAt         time.Time
}

type RunAgent struct {
	ID        string
	RunID     string
	Name      string
	Role      string
	Driver    string
	Model     string
	Worktree  string
	Branch    string
	SessionID string
	Status    string
	CreatedAt time.Time
}

type Task struct {
	ID         string
	RunID      string
	Title      string
	Brief      string
	Role       string
	Deps       []string
	Files      []string
	Acceptance []string
	Verify     []string
	Status     string
	Assignee   string
	Branch     string
	Worktree   string
	Attempts   int
	CreatedAt  time.Time
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
		`CREATE TABLE IF NOT EXISTS runs (
			id TEXT PRIMARY KEY,
			project TEXT NOT NULL,
			goal TEXT NOT NULL,
			base_ref TEXT NOT NULL,
			integration_branch TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS run_agents (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			name TEXT NOT NULL,
			role TEXT NOT NULL,
			driver TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '',
			worktree TEXT NOT NULL DEFAULT '',
			branch TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			created_at TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_run_agents_run ON run_agents(run_id);`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			title TEXT NOT NULL,
			brief TEXT NOT NULL,
			role TEXT NOT NULL,
			deps_json TEXT NOT NULL,
			files_json TEXT NOT NULL,
			acceptance_json TEXT NOT NULL,
			verify_json TEXT NOT NULL,
			status TEXT NOT NULL,
			assignee TEXT NOT NULL DEFAULT '',
			branch TEXT NOT NULL DEFAULT '',
			worktree TEXT NOT NULL DEFAULT '',
			attempts INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_run ON tasks(run_id);`,
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

func (s *Store) CreateRun(ctx context.Context, run Run) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs(id, project, goal, base_ref, integration_branch, status, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.Project, run.Goal, run.BaseRef, run.IntegrationBranch, run.Status,
		time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) GetRun(ctx context.Context, id string) (Run, bool, error) {
	var run Run
	var created string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, project, goal, base_ref, integration_branch, status, created_at
		FROM runs WHERE id = ?`, id).
		Scan(&run.ID, &run.Project, &run.Goal, &run.BaseRef, &run.IntegrationBranch, &run.Status, &created)
	if err == sql.ErrNoRows {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	run.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return run, true, nil
}

func (s *Store) SaveRunAgent(ctx context.Context, agent RunAgent) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_agents(id, run_id, name, role, driver, model, worktree, branch, session_id, status, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			role = excluded.role,
			driver = excluded.driver,
			model = excluded.model,
			worktree = excluded.worktree,
			branch = excluded.branch,
			session_id = excluded.session_id,
			status = excluded.status`,
		agent.ID, agent.RunID, agent.Name, agent.Role, agent.Driver, agent.Model,
		agent.Worktree, agent.Branch, agent.SessionID, agent.Status,
		time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) RunAgents(ctx context.Context, runID string) ([]RunAgent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, name, role, driver, model, worktree, branch, session_id, status, created_at
		FROM run_agents WHERE run_id = ? ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []RunAgent
	for rows.Next() {
		var agent RunAgent
		var created string
		if err := rows.Scan(&agent.ID, &agent.RunID, &agent.Name, &agent.Role, &agent.Driver,
			&agent.Model, &agent.Worktree, &agent.Branch, &agent.SessionID, &agent.Status, &created); err != nil {
			return nil, err
		}
		agent.CreatedAt, _ = time.Parse(time.RFC3339, created)
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

func (s *Store) SaveTask(ctx context.Context, task Task) error {
	deps, _ := json.Marshal(task.Deps)
	files, _ := json.Marshal(task.Files)
	acceptance, _ := json.Marshal(task.Acceptance)
	verify, _ := json.Marshal(task.Verify)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks(id, run_id, title, brief, role, deps_json, files_json, acceptance_json,
			verify_json, status, assignee, branch, worktree, attempts, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			brief = excluded.brief,
			role = excluded.role,
			deps_json = excluded.deps_json,
			files_json = excluded.files_json,
			acceptance_json = excluded.acceptance_json,
			verify_json = excluded.verify_json,
			status = excluded.status,
			assignee = excluded.assignee,
			branch = excluded.branch,
			worktree = excluded.worktree,
			attempts = excluded.attempts`,
		task.ID, task.RunID, task.Title, task.Brief, task.Role, string(deps), string(files),
		string(acceptance), string(verify), task.Status, task.Assignee, task.Branch,
		task.Worktree, task.Attempts, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) TasksForRun(ctx context.Context, runID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, title, brief, role, deps_json, files_json, acceptance_json,
			verify_json, status, assignee, branch, worktree, attempts, created_at
		FROM tasks WHERE run_id = ? ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var task Task
		var deps, files, acceptance, verify, created string
		if err := rows.Scan(&task.ID, &task.RunID, &task.Title, &task.Brief, &task.Role,
			&deps, &files, &acceptance, &verify, &task.Status, &task.Assignee,
			&task.Branch, &task.Worktree, &task.Attempts, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(deps), &task.Deps)
		_ = json.Unmarshal([]byte(files), &task.Files)
		_ = json.Unmarshal([]byte(acceptance), &task.Acceptance)
		_ = json.Unmarshal([]byte(verify), &task.Verify)
		task.CreatedAt, _ = time.Parse(time.RFC3339, created)
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}
