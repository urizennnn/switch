package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateAddsColumnsToOldTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The tasks table as created before execution columns existed.
	if _, err := db.Exec(`CREATE TABLE tasks (
		id TEXT PRIMARY KEY, run_id TEXT NOT NULL, title TEXT NOT NULL, brief TEXT NOT NULL,
		role TEXT NOT NULL, deps_json TEXT NOT NULL, files_json TEXT NOT NULL,
		acceptance_json TEXT NOT NULL, verify_json TEXT NOT NULL, status TEXT NOT NULL,
		assignee TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '',
		worktree TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks VALUES('r:a','r','a','b','builder','[]','[]','[]','[]','queued','','','',0,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	state, err := Open(path)
	if err != nil {
		t.Fatalf("Open old db: %v", err)
	}
	defer state.Close()
	ctx := context.Background()
	task, ok, err := state.GetTask(ctx, "r:a")
	if err != nil || !ok || task.Status != "queued" {
		t.Fatalf("GetTask = %+v, %v, %v", task, ok, err)
	}
	task.Commit, task.Feedback, task.SessionID = "abc", "fix tests", "ses"
	if err := state.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, _, _ := state.GetTask(ctx, "r:a")
	if got.Commit != "abc" || got.Feedback != "fix tests" || got.SessionID != "ses" {
		t.Fatalf("new columns not stored: %+v", got)
	}
}

func TestEvidenceEventsAndUsage(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()

	if err := state.AddVerification(ctx, Verification{TaskID: "t", Attempt: 1, Scope: "task", Command: "go test", ExitCode: 1, Output: "FAIL", Duration: 1500 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	evidence, err := state.Verifications(ctx, "t")
	if err != nil || len(evidence) != 1 || evidence[0].ExitCode != 1 || evidence[0].Duration != 1500*time.Millisecond {
		t.Fatalf("Verifications = %+v, %v", evidence, err)
	}

	for _, kind := range []string{"turn", "status", "turn"} {
		if err := state.AddEvent(ctx, Event{RunID: "r", Kind: kind, Message: kind}); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := state.CountEvents(ctx, "r", "turn"); n != 2 {
		t.Fatalf("CountEvents = %d, want 2", n)
	}
	if events, _ := state.Events(ctx, "r", 2); len(events) != 2 || events[0].Kind != "status" {
		t.Fatalf("Events = %+v, want last two oldest first", events)
	}

	_ = state.AddUsage(ctx, Usage{RunID: "r", TotalTokens: 100})
	_ = state.AddUsage(ctx, Usage{RunID: "r", TotalTokens: 50})
	if u, _ := state.RunUsage(ctx, "r"); u.TotalTokens != 150 {
		t.Fatalf("RunUsage = %+v", u)
	}
}
