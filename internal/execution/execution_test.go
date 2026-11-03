package execution

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

func TestNextRespectsDepsRoleAndBusyWorkers(t *testing.T) {
	agents := []store.RunAgent{
		{Name: "lead", Role: "lead"},
		{Name: "builder-1", Role: "builder"},
		{Name: "tester-1", Role: "tester"},
	}
	tasks := []store.Task{
		{ID: "a", Role: "builder", Status: StatusRunning, Assignee: "builder-1"},
		{ID: "b", Role: "builder", Status: StatusQueued},
		{ID: "c", Role: "tester", Status: StatusQueued, Deps: []string{"a"}},
		{ID: "d", Role: "tester", Status: StatusInterrupted},
	}

	// b waits for the busy builder, c waits for a; d is resumable.
	task, agent, ok := Next(tasks, agents)
	if !ok || task.ID != "d" || agent.Name != "tester-1" {
		t.Fatalf("Next = %q/%q/%v, want d/tester-1", task.ID, agent.Name, ok)
	}

	tasks[0].Status = "done"
	tasks[3].Status = StatusImplemented
	task, agent, ok = Next(tasks, agents)
	if !ok || task.ID != "b" || agent.Name != "builder-1" {
		t.Fatalf("Next = %q/%q/%v, want b/builder-1", task.ID, agent.Name, ok)
	}

	if _, _, ok := Next([]store.Task{{ID: "x", Role: "lead", Status: StatusQueued}}, agents); ok {
		t.Fatal("Next assigned a task to the lead")
	}
}

func TestPromptIncludesBriefCriteriaAndGuardrails(t *testing.T) {
	prompt := Prompt(
		store.Task{Title: "Add flag", Brief: "Add --dry-run.", Acceptance: []string{"flag parses"}, Verify: []string{"go test ./..."}},
		store.RunAgent{Name: "builder-1", Role: "builder", Worktree: "/tmp/wt", Branch: "switch/run/builder-1"},
		rules.Set{},
	)
	for _, want := range []string{"builder-1", "/tmp/wt", "Add --dry-run.", "- flag parses", "- go test ./...", "Do not commit, push"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestRunRecordsFailureAndFreesWorker(t *testing.T) {
	state := openStore(t)
	ctx := context.Background()
	agent := store.RunAgent{ID: "ag", RunID: "run", Name: "builder-1", Role: "builder", Driver: "no-such-adapter", Worktree: t.TempDir(), Status: "idle"}
	task := store.Task{ID: "run:a", RunID: "run", Title: "a", Role: "builder", Status: StatusQueued}
	mustSave(t, state, task, agent)

	if _, err := RunNext(ctx, Options{Store: state, RunID: "run"}); err == nil {
		t.Fatal("RunNext with an unknown adapter succeeded")
	}
	got := onlyTask(t, state)
	if got.Status != StatusFailed || got.Assignee != "builder-1" || got.Attempts != 1 || got.Worktree != agent.Worktree {
		t.Fatalf("task = %+v, want failed, assigned, 1 attempt", got)
	}
	agents, _ := state.RunAgents(ctx, "run")
	if agents[0].Status != "idle" {
		t.Fatalf("agent status = %q, want idle", agents[0].Status)
	}
}

func TestRecoverInterruptsStaleRunningTasks(t *testing.T) {
	state := openStore(t)
	ctx := context.Background()
	mustSave(t, state,
		store.Task{ID: "run:a", RunID: "run", Role: "builder", Status: StatusRunning, Assignee: "builder-1"},
		store.RunAgent{ID: "ag", RunID: "run", Name: "builder-1", Role: "builder", Status: "busy"},
	)
	if err := Recover(ctx, state, "run"); err != nil {
		t.Fatal(err)
	}
	if got := onlyTask(t, state); got.Status != StatusInterrupted {
		t.Fatalf("task status = %q, want interrupted", got.Status)
	}
	agents, _ := state.RunAgents(ctx, "run")
	if agents[0].Status != "idle" {
		t.Fatalf("agent status = %q, want idle", agents[0].Status)
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	return state
}

func mustSave(t *testing.T, state *store.Store, task store.Task, agent store.RunAgent) {
	t.Helper()
	if err := state.SaveTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveRunAgent(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
}

func onlyTask(t *testing.T, state *store.Store) store.Task {
	t.Helper()
	tasks, err := state.TasksForRun(context.Background(), "run")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("TasksForRun = %d tasks, %v", len(tasks), err)
	}
	return tasks[0]
}
