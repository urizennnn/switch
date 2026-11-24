package store

import (
	"context"
	"strings"
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

func TestLatestRun(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	for _, id := range []string{"run-1", "run-2"} {
		if err := store.CreateRun(ctx, Run{
			ID:                id,
			Project:           "/tmp/project",
			Goal:              id,
			BaseRef:           "base",
			IntegrationBranch: "switch/" + id + "/integration",
			Status:            "planned",
		}); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
	}

	run, ok, err := store.LatestRun(ctx, "/tmp/project")
	if err != nil || !ok {
		t.Fatalf("LatestRun = ok %v, err %v", ok, err)
	}
	if run.ID != "run-2" {
		t.Fatalf("latest run = %q, want run-2", run.ID)
	}
}

func TestCreatePlannedRunSavesAllOrNothing(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	run := Run{ID: "run-1", Project: "/tmp/project", Goal: "g", BaseRef: "base", IntegrationBranch: "switch/run-1/integration", Status: "planned"}
	agents := []RunAgent{{ID: "agent-1", RunID: run.ID, Name: "builder-1", Role: "builder", Driver: "claude", Status: "idle"}}
	tasks := []Task{
		{ID: "run-1:a", RunID: run.ID, Title: "A", Brief: "a", Role: "builder", Status: "queued"},
		{ID: "run-1:b", RunID: run.ID, Title: "B", Brief: "b", Role: "builder", Deps: []string{"run-1:a"}, Status: "queued"},
	}
	// The second task fails to save, after the run, its agent and the first task.
	if _, err := store.db.Exec(`CREATE TRIGGER fail_b BEFORE INSERT ON tasks WHEN NEW.id = 'run-1:b' BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.CreatePlannedRun(ctx, run, agents, tasks); err == nil {
		t.Fatal("CreatePlannedRun succeeded despite the failing task")
	}
	if _, ok, _ := store.GetRun(ctx, run.ID); ok {
		t.Fatal("the run was kept after a failure")
	}
	if got, _ := store.RunAgents(ctx, run.ID); len(got) != 0 {
		t.Fatalf("agents kept after a failure: %#v", got)
	}
	if got, _ := store.TasksForRun(ctx, run.ID); len(got) != 0 {
		t.Fatalf("tasks kept after a failure: %#v", got)
	}

	if _, err := store.db.Exec(`DROP TRIGGER fail_b`); err != nil {
		t.Fatal(err)
	}
	if err := store.CreatePlannedRun(ctx, run, agents, tasks); err != nil {
		t.Fatalf("CreatePlannedRun: %v", err)
	}
	got, _ := store.TasksForRun(ctx, run.ID)
	if len(got) != 2 || got[1].Deps[0] != "run-1:a" {
		t.Fatalf("tasks = %#v", got)
	}
}

func TestResolveRunAndRuns(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, run := range []Run{
		{ID: "1b4e28ba-2fa1-11d2-883f-0016d3cca427", Project: "/p"},
		{ID: "1b4e9999-2fa1-11d2-883f-0016d3cca427", Project: "/p"},
		{ID: "run", Project: "/q"},
		{ID: "run-2", Project: "/q"},
		{ID: "1b4e0", Project: "/r"},
	} {
		if err := store.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	for prefix, want := range map[string]string{
		"/p 1b4e28ba":     "1b4e28ba-2fa1-11d2-883f-0016d3cca427", // what plan prints
		"/p 1b4e28ba-2fa": "1b4e28ba-2fa1-11d2-883f-0016d3cca427", // what status and the TUI print
		"/p 1b4e28ba-2fa1-11d2-883f-0016d3cca427": "1b4e28ba-2fa1-11d2-883f-0016d3cca427",
		"/q run":  "run",   // exact beats a longer match
		"/r 1b4e": "1b4e0", // other projects' runs do not make it ambiguous
	} {
		project, prefix, _ := strings.Cut(prefix, " ")
		if got, err := store.ResolveRun(ctx, project, prefix); err != nil || got != want {
			t.Errorf("ResolveRun(%q, %q) = %q, %v; want %q", project, prefix, got, err, want)
		}
	}
	for _, prefix := range []string{"/p 1b4e", "/p nope", "/p %", "/p _", "/q 1b4e28ba", "/p run"} {
		project, prefix, _ := strings.Cut(prefix, " ")
		if got, err := store.ResolveRun(ctx, project, prefix); err == nil {
			t.Errorf("ResolveRun(%q, %q) = %q, want an error", project, prefix, got)
		}
	}

	runs, err := store.Runs(ctx, "/q")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "run-2" || runs[1].ID != "run" {
		t.Fatalf("Runs = %+v, want run-2 then run", runs)
	}
}
