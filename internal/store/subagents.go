package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Subagent is a helper a worker started for its task. It works in the
// worker's worktree and reports only to that worker.
type Subagent struct {
	ID        string
	RunID     string
	TaskID    string
	Parent    string // the worker that started it
	Name      string
	Driver    string
	Model     string
	Brief     string
	Files     []string // files it may edit; none means read-only
	Status    string
	Summary   string
	SessionID string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (s *Store) SaveSubagent(ctx context.Context, sub Subagent) error {
	files, _ := json.Marshal(sub.Files)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO subagents(id, run_id, task_id, parent, name, driver, model, brief, files_json,
			status, summary, session_id, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status = excluded.status,
			summary = excluded.summary,
			session_id = excluded.session_id,
			updated_at = excluded.updated_at`,
		sub.ID, sub.RunID, sub.TaskID, sub.Parent, sub.Name, sub.Driver, sub.Model, sub.Brief,
		string(files), sub.Status, sub.Summary, sub.SessionID, now, now)
	return err
}

func (s *Store) SubagentsForRun(ctx context.Context, runID string) ([]Subagent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, task_id, parent, name, driver, model, brief, files_json,
			status, summary, session_id, created_at, updated_at
		FROM subagents WHERE run_id = ? ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	return scanSubagents(rows)
}

func scanSubagents(rows *sql.Rows) ([]Subagent, error) {
	defer rows.Close()
	var subs []Subagent
	for rows.Next() {
		var sub Subagent
		var files, created, updated string
		if err := rows.Scan(&sub.ID, &sub.RunID, &sub.TaskID, &sub.Parent, &sub.Name, &sub.Driver,
			&sub.Model, &sub.Brief, &files, &sub.Status, &sub.Summary, &sub.SessionID, &created, &updated); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(files), &sub.Files)
		sub.CreatedAt, _ = time.Parse(time.RFC3339, created)
		sub.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}
