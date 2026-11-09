package store

import (
	"context"
	"fmt"
	"time"
)

// Verification is the evidence from one verification command.
type Verification struct {
	TaskID    string
	Attempt   int
	Scope     string // "task" (task worktree) or "integration" (after merge)
	Command   string
	ExitCode  int
	Output    string
	Duration  time.Duration
	TimedOut  bool
	CreatedAt time.Time
}

// Event is one audit-log entry for a run.
type Event struct {
	RunID     string
	TaskID    string
	Agent     string
	Kind      string
	Message   string
	CreatedAt time.Time
}

// Usage is token accounting for one agent turn. Adapters report cost as a
// session total, so it goes to the audit log instead of being summed here.
type Usage struct {
	RunID        string
	TaskID       string
	Agent        string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

var executionTables = []string{
	`CREATE TABLE IF NOT EXISTS verifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL,
		attempt INTEGER NOT NULL,
		scope TEXT NOT NULL,
		command TEXT NOT NULL,
		exit_code INTEGER NOT NULL,
		output TEXT NOT NULL,
		duration_ms INTEGER NOT NULL,
		timed_out INTEGER NOT NULL,
		created_at TEXT NOT NULL
	);`,
	`CREATE INDEX IF NOT EXISTS idx_verifications_task ON verifications(task_id);`,
	`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL,
		task_id TEXT NOT NULL DEFAULT '',
		agent TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL,
		message TEXT NOT NULL,
		created_at TEXT NOT NULL
	);`,
	`CREATE INDEX IF NOT EXISTS idx_events_run ON events(run_id);`,
	`CREATE TABLE IF NOT EXISTS usage (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL,
		task_id TEXT NOT NULL DEFAULT '',
		agent TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL,
		output_tokens INTEGER NOT NULL,
		total_tokens INTEGER NOT NULL,
		created_at TEXT NOT NULL
	);`,
	`CREATE INDEX IF NOT EXISTS idx_usage_run ON usage(run_id);`,
	`CREATE TABLE IF NOT EXISTS session_history (
		id TEXT PRIMARY KEY,
		project TEXT NOT NULL,
		agent_name TEXT NOT NULL DEFAULT '',
		driver TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);`,
	`CREATE INDEX IF NOT EXISTS idx_session_history_project ON session_history(project);`,
	`CREATE TABLE IF NOT EXISTS subagents (
		id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL,
		task_id TEXT NOT NULL,
		parent TEXT NOT NULL,
		name TEXT NOT NULL,
		driver TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		brief TEXT NOT NULL,
		files_json TEXT NOT NULL,
		status TEXT NOT NULL,
		summary TEXT NOT NULL DEFAULT '',
		session_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);`,
	`CREATE INDEX IF NOT EXISTS idx_subagents_run ON subagents(run_id);`,
}

// executionColumns are added to tables created by older builds.
var executionColumns = []struct{ table, column, def string }{
	{"runs", "integration_path", "TEXT NOT NULL DEFAULT ''"},
	// Messages saved before this column existed have no session and stay hidden.
	{"chat_messages", "session_id", "TEXT NOT NULL DEFAULT ''"},
	{"tasks", "session_id", "TEXT NOT NULL DEFAULT ''"},
	{"tasks", "commit_sha", "TEXT NOT NULL DEFAULT ''"},
	{"tasks", "feedback", "TEXT NOT NULL DEFAULT ''"},
	{"tasks", "review", "TEXT NOT NULL DEFAULT ''"},
	{"tasks", "last_error", "TEXT NOT NULL DEFAULT ''"},
	{"tasks", "summary", "TEXT NOT NULL DEFAULT ''"},
	{"project_settings", "trusted_rules", "TEXT NOT NULL DEFAULT ''"},
}

func (s *Store) migrateExecution() error {
	for _, statement := range executionTables {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	for _, c := range executionColumns {
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&count); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", c.table, c.column, err)
		}
		if count > 0 {
			continue
		}
		if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, c.table, c.column, c.def)); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", c.table, c.column, err)
		}
	}
	_, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_chat_messages_session ON chat_messages(session_id, id);`)
	return err
}

func (s *Store) GetTask(ctx context.Context, id string) (Task, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	if err != nil {
		return Task{}, false, err
	}
	tasks, err := scanTasks(rows)
	if err != nil || len(tasks) == 0 {
		return Task{}, false, err
	}
	return tasks[0], true, nil
}

func (s *Store) SetRunStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) AddVerification(ctx context.Context, v Verification) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO verifications(task_id, attempt, scope, command, exit_code, output, duration_ms, timed_out, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.TaskID, v.Attempt, v.Scope, v.Command, v.ExitCode, v.Output, v.Duration.Milliseconds(),
		v.TimedOut, time.Now().UTC().Format(time.RFC3339))
	return err
}

// Verifications returns a task's evidence, oldest first.
func (s *Store) Verifications(ctx context.Context, taskID string) ([]Verification, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id, attempt, scope, command, exit_code, output, duration_ms, timed_out, created_at
		FROM verifications WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Verification
	for rows.Next() {
		var v Verification
		var ms int64
		var created string
		if err := rows.Scan(&v.TaskID, &v.Attempt, &v.Scope, &v.Command, &v.ExitCode, &v.Output, &ms, &v.TimedOut, &created); err != nil {
			return nil, err
		}
		v.Duration = time.Duration(ms) * time.Millisecond
		v.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) AddEvent(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO events(run_id, task_id, agent, kind, message, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
		e.RunID, e.TaskID, e.Agent, e.Kind, e.Message, time.Now().UTC().Format(time.RFC3339))
	return err
}

// Events returns the newest limit events of a run, oldest first.
func (s *Store) Events(ctx context.Context, runID string, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, task_id, agent, kind, message, created_at FROM (
			SELECT * FROM events WHERE run_id = ? ORDER BY id DESC LIMIT ?
		) ORDER BY id`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var created string
		if err := rows.Scan(&e.RunID, &e.TaskID, &e.Agent, &e.Kind, &e.Message, &created); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountEvents counts a run's events of one kind.
func (s *Store) CountEvents(ctx context.Context, runID, kind string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE run_id = ? AND kind = ?`, runID, kind).Scan(&count)
	return count, err
}

func (s *Store) AddUsage(ctx context.Context, u Usage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage(run_id, task_id, agent, input_tokens, output_tokens, total_tokens, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		u.RunID, u.TaskID, u.Agent, u.InputTokens, u.OutputTokens, u.TotalTokens,
		time.Now().UTC().Format(time.RFC3339))
	return err
}

// RunUsage sums a run's usage.
func (s *Store) RunUsage(ctx context.Context, runID string) (Usage, error) {
	u := Usage{RunID: runID}
	err := s.db.QueryRowContext(ctx, `
		SELECT coalesce(sum(input_tokens), 0), coalesce(sum(output_tokens), 0),
			coalesce(sum(total_tokens), 0)
		FROM usage WHERE run_id = ?`, runID).
		Scan(&u.InputTokens, &u.OutputTokens, &u.TotalTokens)
	return u, err
}

// SessionSummary describes a past chat session for the resume picker.
type SessionSummary struct {
	ID        string
	AgentName string
	Driver    string
	Model     string
	Title     string // the session's first user message
	Messages  int
	UpdatedAt time.Time // last message, or creation when empty
}

// Sessions lists a project's chat sessions that have messages, most recent
// first.
func (s *Store) Sessions(ctx context.Context, project string, limit int) ([]SessionSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT h.id, h.agent_name, h.driver, h.model,
			coalesce((SELECT content FROM chat_messages m WHERE m.session_id = h.id AND m.role = 'user' ORDER BY m.id LIMIT 1), ''),
			(SELECT count(*) FROM chat_messages m WHERE m.session_id = h.id) AS messages,
			coalesce((SELECT max(created_at) FROM chat_messages m WHERE m.session_id = h.id), h.created_at),
			(SELECT max(id) FROM chat_messages m WHERE m.session_id = h.id) AS latest
		FROM session_history h
		WHERE h.project = ? AND messages > 0
		ORDER BY latest DESC
		LIMIT ?`, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var summary SessionSummary
		var updated string
		var latest int64
		if err := rows.Scan(&summary.ID, &summary.AgentName, &summary.Driver, &summary.Model,
			&summary.Title, &summary.Messages, &updated, &latest); err != nil {
			return nil, err
		}
		summary.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
		out = append(out, summary)
	}
	return out, rows.Err()
}
