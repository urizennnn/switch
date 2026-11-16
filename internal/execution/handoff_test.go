package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/store"
)

func TestHandoffContinuesTheTaskWithAnotherAgent(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, builderTask("a"))
	e := f.engine(config.RunConfig{MaxAttempts: 3, HandoffAt: 80})
	e.opts.Handoffs = map[string]string{"builder-1": "builder-2"}
	var mu sync.Mutex
	var who, problems []string
	problem := func(s string) { mu.Lock(); problems = append(problems, s); mu.Unlock() }
	f.worker = func(req TurnRequest) (string, error) {
		mu.Lock()
		who = append(who, req.Agent.Name)
		mu.Unlock()
		switch req.Agent.Name {
		case "builder-1":
			if !strings.Contains(req.Prompt, "switch_handoff") || !strings.Contains(req.Prompt, "builder-2 continues") {
				problem("worker prompt does not explain its handoff")
			}
			writeFile(t, req.Cwd, "half.txt", "x")
			if _, err := e.Handoff("builder-1", req.TaskID, "", "half.txt is written; write a.txt next"); err != nil {
				problem(err.Error())
			}
			return "handing off", nil
		case "builder-2":
			if !strings.Contains(req.Prompt, "taking over this task from builder-1") || !strings.Contains(req.Prompt, "write a.txt next") {
				problem("builder-2 did not get the handoff note")
			}
			if _, err := os.Stat(filepath.Join(req.Cwd, "half.txt")); err != nil {
				problem("the worktree was not kept")
			}
			return "", errors.New("Claude AI usage limit reached|1731600000")
		default: // builder-2 has no handoff, so its limit hands the task to the lead
			if !strings.Contains(req.Prompt, "usage limit reached") {
				problem("the lead was not told why it took over")
			}
			writeFile(t, req.Cwd, "a.txt", "done\n")
			return "finished", nil
		}
	}

	if outcome, err := e.Run(context.Background()); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	for _, p := range problems {
		t.Error(p)
	}
	if strings.Join(who, ",") != "builder-1,builder-2,lead" {
		t.Fatalf("turns went to %v", who)
	}
	if a := f.task("run:a"); a.Status != StatusReview || a.Assignee != "lead" {
		t.Fatalf("task a = %s by %s", a.Status, a.Assignee)
	}
}

func TestHandoffStopsAtTheAttemptLimit(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, builderTask("a"))
	e := f.engine(config.RunConfig{MaxAttempts: 1})
	f.worker = func(req TurnRequest) (string, error) {
		_, _ = e.Handoff(req.Agent.Name, req.TaskID, "builder-2", "over to you")
		return "", nil
	}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := f.task("run:a"); a.Status != StatusBlocked || !strings.Contains(a.LastError, "attempt limit") {
		t.Fatalf("task a = %s: %s", a.Status, a.LastError)
	}
	if _, err := e.Handoff("builder-1", "run:a", "", "x"); err == nil {
		t.Fatal("handed off a task that is not running")
	}
}
