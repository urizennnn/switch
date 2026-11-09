package execution

import (
	"context"
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

func TestSubagentsOff(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	e := f.engine(config.RunConfig{})
	if _, err := e.Spawn("builder-1", "run:a", SpawnRequest{Name: "x", Brief: "y"}); err == nil || !strings.Contains(err.Error(), "turned off") {
		t.Fatalf("Spawn with subagents off = %v", err)
	}
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}
