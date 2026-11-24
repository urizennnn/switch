package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/store"
)

func TestWorkerRunsItsSubagents(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	e := f.engine(config.RunConfig{Subagents: config.SubagentConfig{MaxPerBuilder: 2}})
	release := make(chan struct{})
	var mu sync.Mutex
	prompts := map[string]string{} // first prompt of each subagent
	envs := map[string]string{}    // env of each turn, by agent
	var errs []string
	fail := func(msg string) {
		mu.Lock()
		errs = append(errs, msg)
		mu.Unlock()
	}
	expect := func(err error, want string) {
		if err == nil || !strings.Contains(err.Error(), want) {
			fail("want error containing " + want + ", got " + errString(err))
		}
	}

	f.worker = func(req TurnRequest) (string, error) {
		mu.Lock()
		envs[req.Agent.Name] = strings.Join(req.Env, " ")
		mu.Unlock()
		if req.Agent.Role == "subagent" {
			mu.Lock()
			if prompts[req.Agent.Name] == "" {
				prompts[req.Agent.Name] = req.Prompt
			}
			mu.Unlock()
			switch req.Agent.Name {
			case "builder-1/tests":
				writeFile(t, req.Cwd, "a.txt", "done\n")
				<-release
				return "wrote a.txt", nil
			case "builder-1/scout":
				if strings.Contains(req.Prompt, "look again") {
					time.Sleep(time.Second) // still running when the worker's turn ends
					return "late", nil
				}
				<-release
				if err := e.Report(req.Agent.ID, "found 3 callers"); err != nil {
					fail(err.Error())
				}
				return "ignored, the report wins", nil
			}
			return "", nil
		}
		if !strings.Contains(req.Prompt, "switch_spawn") {
			fail("worker prompt does not mention switch_spawn")
		}
		if _, err := e.Spawn("builder-1", req.TaskID, SpawnRequest{Name: "Tests", Brief: "write a.txt", Files: []string{"a.txt"}}); err != nil {
			fail(err.Error())
		}
		_, err := e.Spawn("builder-1", req.TaskID, SpawnRequest{Name: "dup", Brief: "also a.txt", Files: []string{"./a.txt"}})
		expect(err, "already edits a.txt")
		_, err = e.Spawn("builder-1", req.TaskID, SpawnRequest{Name: "root", Brief: "all files", Files: []string{"."}})
		expect(err, "already edits")
		for _, path := range []string{"../outside", filepath.Join(string(filepath.Separator), "outside")} {
			_, err = e.Spawn("builder-1", req.TaskID, SpawnRequest{Name: "outside", Brief: "outside", Files: []string{path}})
			expect(err, "relative to its root")
		}
		if _, err := e.Spawn("builder-1", req.TaskID, SpawnRequest{Name: "scout", Brief: "find callers"}); err != nil {
			fail(err.Error())
		}
		_, err = e.Spawn("builder-1", req.TaskID, SpawnRequest{Name: "third", Brief: "x"})
		expect(err, "limit")
		_, err = e.Spawn("builder-2", req.TaskID, SpawnRequest{Name: "intruder", Brief: "x"})
		expect(err, "your running task")

		close(release)
		subs, err := e.Wait(context.Background(), "builder-1", req.TaskID, nil)
		if err != nil || len(subs) != 2 {
			fail("wait: " + errString(err))
		}
		for _, sa := range subs {
			if sa.Status != SubDone {
				fail(sa.Name + " is " + sa.Status)
			}
			if sa.Name == "scout" && sa.Summary != "found 3 callers" {
				fail("scout summary " + sa.Summary)
			}
		}
		// A follow-up restarts a finished subagent; the worker then ends its
		// turn without waiting, so Switch stops it.
		if _, err := e.Message("builder-1", req.TaskID, "scout", "look again"); err != nil {
			fail(err.Error())
		}
		return "done", nil
	}

	if outcome, err := e.Run(context.Background()); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	for _, msg := range errs {
		t.Error(msg)
	}
	if !strings.Contains(prompts["builder-1/scout"], "read-only") || !strings.Contains(prompts["builder-1/tests"], "a.txt") {
		t.Errorf("subagent prompts lack their file claims:\n%s\n%s", prompts["builder-1/scout"], prompts["builder-1/tests"])
	}
	// Subagents run the worker's commands in the worker's slot.
	if want := envs["builder-1"]; !strings.Contains(want, "SWITCH_SLOT=") || envs["builder-1/tests"] != want || envs["builder-1/scout"] != want || !strings.Contains(prompts["builder-1/tests"], "SWITCH_SLOT=") {
		t.Errorf("subagent env = %v, want the worker's %q, in env and prompt", envs, want)
	}
	subs, _ := f.state.SubagentsForRun(context.Background(), "run")
	status := map[string]string{}
	for _, sa := range subs {
		status[sa.Name] = sa.Status
	}
	if status["tests"] != SubDone || status["scout"] != SubStopped || len(subs) != 2 {
		t.Fatalf("subagents = %v, want tests done and scout stopped", status)
	}
	// The subagent's file is part of the worker's commit.
	if a := f.task("run:a"); a.Status != StatusReview {
		t.Fatalf("task a = %s", a.Status)
	} else if _, err := os.Stat(filepath.Join(a.Worktree, "a.txt")); err != nil {
		t.Fatal(err)
	}
}

// A follow-up that starts a finished subagent again keeps the limits Spawn
// enforces, and reaches a subagent an earlier Switch process ran. Subagent
// turns that use up the budget interrupt their worker's turn.
func TestMessageRestartKeepsSubagentLimits(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	ctx := context.Background()
	e := f.engine(config.RunConfig{MaxTurns: 3, Subagents: config.SubagentConfig{MaxPerBuilder: 2}})
	if err := f.state.SaveSubagent(ctx, store.Subagent{ID: "run:a/old", RunID: "run", TaskID: "run:a", Parent: "builder-1",
		Name: "old", Driver: "fake", Brief: "look around", Status: SubInterrupted, SessionID: "ses-old"}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var mu sync.Mutex
	var errs []string
	fail := func(msg string) { mu.Lock(); errs = append(errs, msg); mu.Unlock() }
	expect := func(err error, want string) {
		if err == nil || !strings.Contains(err.Error(), want) {
			fail("want error containing " + want + ", got " + errString(err))
		}
	}
	f.worker = func(req TurnRequest) (string, error) {
		switch req.Agent.Name {
		case "builder-1/old":
			if req.SessionID != "ses-old" || req.Prompt != "carry on" {
				fail("old was not resumed in its session: " + req.SessionID + " " + req.Prompt)
			}
			return "carried on", nil
		case "builder-1/other":
			<-release
			return "ok", nil
		case "builder-1/tests":
			return "ok", nil
		}
		if _, err := e.Message("builder-1", req.TaskID, "old", "carry on"); err != nil {
			fail(err.Error())
		}
		if _, err := e.Wait(ctx, "builder-1", req.TaskID, []string{"old"}); err != nil {
			fail(err.Error())
		}
		if _, err := e.Spawn("builder-1", req.TaskID, SpawnRequest{Name: "tests", Brief: "x", Files: []string{"a.txt"}}); err != nil {
			fail(err.Error())
		}
		_, _ = e.Wait(ctx, "builder-1", req.TaskID, []string{"tests"})
		if _, err := e.Spawn("builder-1", req.TaskID, SpawnRequest{Name: "other", Brief: "y", Files: []string{"a.txt"}}); err != nil {
			fail(err.Error())
		}
		_, err := e.Message("builder-1", req.TaskID, "tests", "again")
		expect(err, "other already edits a.txt")
		close(release)
		_, _ = e.Wait(ctx, "builder-1", req.TaskID, nil)
		_, err = e.Message("builder-1", req.TaskID, "tests", "again") // old, tests and other used the 3 turns
		expect(err, "budget")
		return "done", nil
	}
	fake := e.opts.Turn
	e.opts.Turn = func(ctx context.Context, req TurnRequest) (TurnResult, error) {
		result, err := fake(ctx, req)
		if req.Agent.Name != "builder-1" || err != nil {
			return result, err
		}
		select { // the worker's turn runs on until the budget interrupts it
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(5 * time.Second):
			return result, errors.New("not interrupted")
		}
	}
	if outcome, err := e.Run(ctx); err != nil || outcome != RunBudget {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if a := f.task("run:a"); a.Status != StatusInterrupted || a.Attempts != 1 {
		t.Fatalf("a = %s on attempt %d, want interrupted by the budget: %s", a.Status, a.Attempts, a.LastError)
	}
	for _, msg := range errs {
		t.Error(msg)
	}
}

// A subagent whose turn outlasts Stop's grace may end after Run has returned;
// that must not crash Switch.
func TestSubagentEndingAfterTheRun(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	e := f.engine(config.RunConfig{})
	release := make(chan struct{})
	e.opts.Turn = func(ctx context.Context, req TurnRequest) (TurnResult, error) {
		<-release // stuck, deaf to its context
		return TurnResult{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	sa := &subagent{rec: store.Subagent{ID: "run:a/x", RunID: "run", TaskID: "run:a", Parent: "builder-1", Name: "x"}}
	e.subMu.Lock()
	e.runCtx = ctx
	e.startSubagent(sa, f.task("run:a"), builder1, nil, "x", "go")
	e.runCtx = nil // Run returned
	e.subMu.Unlock()
	cancel()
	close(release)
	<-sa.done
	if sa.rec.Status != SubInterrupted {
		t.Fatalf("subagent = %s, want interrupted", sa.rec.Status)
	}
}

func TestSubagentsOff(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	e := f.engine(config.RunConfig{})
	if _, err := e.Spawn("builder-1", "run:a", SpawnRequest{Name: "x", Brief: "y"}); err == nil || !strings.Contains(err.Error(), "turned off") {
		t.Fatalf("Spawn with subagents off = %v", err)
	}
}

func TestSubagentOfAWorkerAnOlderBuildPlanned(t *testing.T) {
	worker := store.RunAgent{Name: "builder-1", Role: "builder", Driver: "Fake"} // stored as the config spelled it
	f := newFixture(t, []store.RunAgent{lead, worker}, builderTask("a"))
	e := f.engine(config.RunConfig{Subagents: config.SubagentConfig{MaxPerBuilder: 1, Drivers: []string{"fake"}}})
	work := f.worker
	var spawnErr error
	f.worker = func(req TurnRequest) (string, error) {
		if req.Agent.Role == "builder" {
			_, spawnErr = e.Spawn("builder-1", req.TaskID, SpawnRequest{Name: "scout", Brief: "look"})
		}
		return work(req)
	}
	if _, err := e.Run(context.Background()); err != nil || spawnErr != nil {
		t.Fatalf("Run = %v, Spawn = %v", err, spawnErr)
	}
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}
