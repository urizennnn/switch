package store

import (
	"context"
	"testing"
)

func TestRunAgentsAndTasksRoundTrip(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	run := Run{
		ID:                "run-1",
		Project:           "/tmp/project",
		Goal:              "ship a feature",
		BaseRef:           "abc123",
		IntegrationBranch: "switch/run-1/integration",
		Status:            "planning",
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	gotRun, ok, err := store.GetRun(ctx, run.ID)
	if err != nil || !ok {
		t.Fatalf("GetRun = ok %v, err %v", ok, err)
	}
	if gotRun.Goal != run.Goal || gotRun.IntegrationBranch != run.IntegrationBranch {
		t.Fatalf("run = %#v", gotRun)
	}

	agent := RunAgent{
		ID:     "agent-1",
		RunID:  run.ID,
		Name:   "builder-1",
		Role:   "builder",
		Driver: "opencode",
		Model:  "opencode/deepseek-v4.1-flash-free",
		Status: "idle",
	}
	if err := store.SaveRunAgent(ctx, agent); err != nil {
		t.Fatalf("SaveRunAgent: %v", err)
	}
	agents, err := store.RunAgents(ctx, run.ID)
	if err != nil {
		t.Fatalf("RunAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "builder-1" {
		t.Fatalf("agents = %#v", agents)
	}

	task := Task{
		ID:         "task-1",
		RunID:      run.ID,
		Title:      "do the thing",
		Brief:      "implement it",
		Role:       "builder",
		Deps:       []string{"task-0"},
		Files:      []string{"internal/**"},
		Acceptance: []string{"tests pass"},
		Verify:     []string{"go test ./..."},
		Status:     "queued",
	}
	if err := store.SaveTask(ctx, task); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}
	tasks, err := store.TasksForRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("TasksForRun: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %#v", tasks)
	}
	if len(tasks[0].Deps) != 1 || tasks[0].Deps[0] != "task-0" {
		t.Fatalf("deps = %#v", tasks[0].Deps)
	}
	if tasks[0].Acceptance[0] != "tests pass" {
		t.Fatalf("acceptance = %#v", tasks[0].Acceptance)
	}
}
