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
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/store"
)

func TestHandoffContinuesTheTaskWithAnotherAgent(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, builderTask("a"))
	e := f.engine(config.RunConfig{MaxAttempts: 3, HandoffAt: 80})
	e.opts.Handoffs = map[string]string{"builder-1": "builder-2"}
	var mu sync.Mutex
	var who, problems []string
	problem := func(s string) { mu.Lock(); problems = append(problems, s); mu.Unlock() }
	f.review = func(TurnRequest) string { problem("the lead reviewed its own work"); return "APPROVE" }
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
	if a := f.task("run:a"); a.Status != StatusReview || a.Assignee != "lead" || !strings.Contains(a.Review, "review it yourself") {
		t.Fatalf("task a = %s by %s, review %q", a.Status, a.Assignee, a.Review)
	}
}

// The lead never reviews a diff it worked on, even after passing the task on.
func TestLeadDoesNotReviewATaskItPassedOn(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, builderTask("a"))
	e := f.engine(config.RunConfig{MaxAttempts: 3})
	f.review = func(TurnRequest) string { t.Error("the lead reviewed a task it worked on"); return "APPROVE" }
	next := map[string]string{"builder-1": "lead", "lead": "builder-2"}
	f.worker = func(req TurnRequest) (string, error) {
		if req.Agent.Name == "lead" {
			writeFile(t, req.Cwd, "a.txt", "done\n") // the change is the lead's
		}
		if to := next[req.Agent.Name]; to != "" {
			if _, err := e.Handoff(req.Agent.Name, req.TaskID, to, "over to you"); err != nil {
				t.Error(err)
			}
		}
		return "done", nil
	}

	if outcome, err := e.Run(context.Background()); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if a := f.task("run:a"); a.Status != StatusReview || a.Assignee != "builder-2" || !strings.Contains(a.Review, "lead worked on this task") {
		t.Fatalf("task a = %s by %s, review %q", a.Status, a.Assignee, a.Review)
	}
}

// Context use belongs to a session: an agent that handed off near full is not
// told to hand off its next task, which starts a fresh session.
func TestContextUseBelongsToTheSession(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	e := f.engine(config.RunConfig{HandoffAt: 80})
	e.useSession("builder-1", "ses-a")
	e.setUsage("ses-a", acpdriver.UsageInfo{Used: 85, Size: 100})
	if !e.Limits("builder-1").Near() {
		t.Fatal("85% full is not near the limit")
	}
	e.useSession("builder-1", "ses-c")
	if l := e.Limits("builder-1"); l.Near() || l.Percent() != -1 {
		t.Fatalf("a fresh session reads %d%% full", l.Percent())
	}
	e.useSession("builder-1", "ses-a") // a retry resumes the full one
	if !e.Limits("builder-1").Near() {
		t.Fatal("the resumed session lost its context use")
	}
}

// fakeControl keeps the blocks workers report, as the control server does.
type fakeControl struct {
	mu     sync.Mutex
	blocks map[string]string
}

func (c *fakeControl) Grant(store.RunAgent, string) (Grant, error) { return Grant{}, nil }

func (c *fakeControl) block(taskID, reason string) {
	c.mu.Lock()
	c.blocks[taskID] = reason
	c.mu.Unlock()
}

func (c *fakeControl) TakeBlock(taskID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reason, ok := c.blocks[taskID]
	delete(c.blocks, taskID)
	return reason, ok
}

// A block or handoff reported in one attempt goes with that attempt, however
// it ends, and never decides the task's next attempt.
func TestSignalsDoNotOutliveTheirAttempt(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, builderTask("a"))
	ctx := context.Background()
	e := f.engine(config.RunConfig{})
	control := &fakeControl{blocks: map[string]string{}}
	e.opts.Control = control
	f.worker = func(req TurnRequest) (string, error) {
		if req.Agent.Name == "builder-1" {
			control.block(req.TaskID, "cannot find the API key")
			_, err := e.Handoff("builder-1", req.TaskID, "builder-2", "over to you")
			return "handing off", err
		}
		writeFile(t, req.Cwd, "a.txt", "done\n")
		return "done", nil
	}
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v (a: %s)", outcome, err, f.task("run:a").LastError)
	}
	if a := f.task("run:a"); a.Status != StatusReview || a.Assignee != "builder-2" {
		t.Fatalf("a = %s by %s, want review by builder-2", a.Status, a.Assignee)
	}

	// A handoff asked for in a turn the user stopped is dropped with it.
	g := newFixture(t, []store.RunAgent{lead, builder1, builder2}, builderTask("a"))
	e = g.engine(config.RunConfig{})
	stop := true
	g.worker = func(req TurnRequest) (string, error) {
		if stop {
			stop = false
			_, _ = e.Handoff(req.Agent.Name, req.TaskID, "builder-2", "over to you")
			e.StopWorker(req.Agent.Name)
			return "", nil
		}
		writeFile(t, req.Cwd, "a.txt", "done\n")
		return "done", nil
	}
	if _, err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if a := g.task("run:a"); a.Status != StatusBlocked {
		t.Fatalf("a = %s, want blocked by the stop", a.Status)
	}
	if err := e.Retry(ctx, "run:a"); err != nil {
		t.Fatal(err)
	}
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run after retry = %q, %v", outcome, err)
	}
	if a := g.task("run:a"); a.Status != StatusReview || a.Assignee != "builder-1" {
		t.Fatalf("a = %s by %s, want review by builder-1, not handed off", a.Status, a.Assignee)
	}
}

// A block the worker reported wins over a rate limit that ends its turn
// after it: the task waits for the user, not for the lead to redo it.
func TestBlockWinsOverALimitAfterIt(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	e := f.engine(config.RunConfig{})
	control := &fakeControl{blocks: map[string]string{}}
	e.opts.Control = control
	f.worker = func(req TurnRequest) (string, error) {
		if req.Agent.Name == "builder-1" {
			control.block(req.TaskID, "needs the user's API key")
			return "", errors.New("429 Too Many Requests: rate limit")
		}
		writeFile(t, req.Cwd, "a.txt", "done\n")
		return "done", nil
	}
	if outcome, err := e.Run(context.Background()); err != nil || outcome != RunBlocked {
		t.Fatalf("Run = %q, %v; want blocked", outcome, err)
	}
	if a := f.task("run:a"); a.Status != StatusBlocked || !strings.Contains(a.LastError, "needs the user's API key") {
		t.Fatalf("a = %s by %s %q", a.Status, a.Assignee, a.LastError)
	}
}

// A task whose worker runs out of attempts goes to the worker's escalation
// agent, with the evidence, for one attempt more; then it is blocked.
func TestEscalationGetsOneMoreAttempt(t *testing.T) {
	for _, fixed := range []bool{true, false} {
		f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, builderTask("a"))
		e := f.engine(config.RunConfig{MaxAttempts: 1})
		e.opts.Escalations = map[string]string{"builder-1": "builder-2", "builder-2": "builder-1"}
		var who []string
		f.worker = func(req TurnRequest) (string, error) {
			who = append(who, req.Agent.Name)
			if req.Agent.Name != "builder-2" {
				return "tried", nil
			}
			if !strings.Contains(req.Prompt, "taking over this task from builder-1") || !strings.Contains(req.Prompt, "builder-1 reached the attempt limit (1)") || !strings.Contains(req.Prompt, "failed (exit 1)") {
				return "", errors.New("escalated without the evidence:\n" + req.Prompt)
			}
			if fixed {
				writeFile(t, req.Cwd, "a.txt", "done\n")
			}
			return "tried", nil
		}
		outcome, err := e.Run(context.Background())
		a := f.task("run:a")
		switch {
		case err != nil || strings.Join(who, ",") != "builder-1,builder-2":
			t.Fatalf("Run = %q, %v; turns went to %v (a: %s)", outcome, err, who, a.LastError)
		case fixed && (a.Status != StatusReview || a.Assignee != "builder-2" || a.Attempts != 2):
			t.Fatalf("a = %s by %s on attempt %d, want review by builder-2 on attempt 2", a.Status, a.Assignee, a.Attempts)
		case !fixed && (a.Status != StatusBlocked || !strings.HasPrefix(a.LastError, "attempt limit 1 reached: verification failed")):
			t.Fatalf("a = %s: %s; want blocked after the escalated attempt, not escalated back", a.Status, a.LastError)
		}
	}
}

func TestTheLeadHandsOffToAStandbyLead(t *testing.T) {
	// After /lead builder-1, planning saves the configured lead as "standby".
	standby := store.RunAgent{Name: "old-lead", Role: "standby", Driver: "fake"}
	f := newFixture(t, []store.RunAgent{standby, lead, builder1})
	e := f.engine(config.RunConfig{})
	for from, want := range map[string]string{"builder-1": "lead", "lead": "old-lead", "old-lead": "lead"} {
		if got := e.Limits(from).HandoffTo; got != want {
			t.Errorf("%s hands off to %q, want %q", from, got, want)
		}
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

	// With an escalation, a handoff on the last attempt goes to that agent.
	g := newFixture(t, []store.RunAgent{lead, builder1, builder2}, builderTask("a"))
	e = g.engine(config.RunConfig{MaxAttempts: 1})
	e.opts.Escalations = map[string]string{"builder-1": "lead"}
	g.worker = func(req TurnRequest) (string, error) {
		if req.Agent.Name == "builder-1" {
			_, err := e.Handoff("builder-1", req.TaskID, "builder-2", "over to you")
			return "handing off", err
		}
		if !strings.Contains(req.Prompt, "over to you") {
			return "", errors.New("escalated without the handoff note:\n" + req.Prompt)
		}
		writeFile(t, req.Cwd, "a.txt", "done\n")
		return "done", nil
	}
	if outcome, err := e.Run(context.Background()); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v (a: %s)", outcome, err, g.task("run:a").LastError)
	}
	if a := g.task("run:a"); a.Status != StatusReview || a.Assignee != "lead" {
		t.Fatalf("a = %s by %s, want review by lead", a.Status, a.Assignee)
	}
}
