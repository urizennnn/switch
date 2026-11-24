package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestChatSessionRoundTrip(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if _, ok, err := store.GetChatSession(ctx, "/tmp/project"); err != nil || ok {
		t.Fatalf("initial GetChatSession = ok %v, err %v", ok, err)
	}

	want := ChatSession{
		Project:   "/tmp/project",
		Agent:     "opencode",
		Model:     "opencode/deepseek-v4.1-flash-free",
		SessionID: "ses_123",
	}
	if err := store.SaveChatSession(ctx, want); err != nil {
		t.Fatalf("SaveChatSession: %v", err)
	}

	got, ok, err := store.GetChatSession(ctx, want.Project)
	if err != nil || !ok {
		t.Fatalf("GetChatSession = ok %v, err %v", ok, err)
	}
	if got.Agent != want.Agent || got.Model != want.Model || got.SessionID != want.SessionID {
		t.Fatalf("session = %#v, want %#v", got, want)
	}
}

func TestMessagesChronological(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	for _, message := range []string{"one", "two", "three"} {
		if err := store.AppendMessage(ctx, "/tmp/project", "ses-1", "user", message); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}

	if err := store.AppendMessage(ctx, "/tmp/project", "ses-2", "user", "other session"); err != nil {
		t.Fatal(err)
	}
	messages, err := store.Messages(ctx, "/tmp/project", "ses-1", 10)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3", len(messages))
	}
	if messages[0].Content != "one" || messages[2].Content != "three" {
		t.Fatalf("messages out of order: %#v", messages)
	}
}

func TestLeadAgentSetting(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	if _, ok, err := store.LeadAgent(ctx, "/tmp/project"); err != nil || ok {
		t.Fatalf("initial LeadAgent = ok %v, err %v", ok, err)
	}
	if err := store.SetLeadAgent(ctx, "/tmp/project", "builder-1"); err != nil {
		t.Fatalf("SetLeadAgent: %v", err)
	}
	name, ok, err := store.LeadAgent(ctx, "/tmp/project")
	if err != nil || !ok {
		t.Fatalf("LeadAgent = ok %v, err %v", ok, err)
	}
	if name != "builder-1" {
		t.Fatalf("lead = %q, want builder-1", name)
	}
}

// Several processes open the database at once: the first launch plus a
// __control command, or runs started from a script. Each Open has its own
// connection, as a separate process would.
func TestOpenConcurrently(t *testing.T) {
	for _, older := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "switch.db")
		if older { // a database from a build before the execution columns
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`PRAGMA journal_mode = WAL; CREATE TABLE runs (id TEXT PRIMARY KEY, project TEXT NOT NULL,
				goal TEXT NOT NULL, base_ref TEXT NOT NULL, integration_branch TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL);`); err != nil {
				t.Fatal(err)
			}
			db.Close()
		}
		errs := make(chan error, 16)
		for range cap(errs) {
			go func() {
				s, err := Open(path)
				if err == nil {
					err = s.Close()
				}
				errs <- err
			}()
		}
		for range cap(errs) {
			if err := <-errs; err != nil {
				t.Errorf("older database %v: %v", older, err)
			}
		}
	}
}

// The database holds transcripts and plans, so only you can read it, even
// where an older build or another component created the folder first.
func TestStateIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "switch.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for file, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", file, got, want)
		}
	}
}
