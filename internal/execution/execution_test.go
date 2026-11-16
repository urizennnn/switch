package execution

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
	"github.com/urizennnn/switch/internal/worktree"
)

// fixture is a git repo with a planned run and an engine over a fake turn.
type fixture struct {
	t       *testing.T
	state   *store.Store
	run     store.Run
	worker  func(req TurnRequest) (string, error) // runs in the task worktree
	review  func(req TurnRequest) string
	prompts sync.Map // task ID -> last worker prompt
}

func newFixture(t *testing.T, agents []store.RunAgent, tasks ...store.Task) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	repo := t.TempDir()
	git(t, repo, "init")
	writeFile(t, repo, "README.md", "base\n")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "init")

	stateDir := t.TempDir()
	manager, err := worktree.New(ctx, repo, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := manager.Head(ctx)
	integration, branch, err := manager.Integration(ctx, "run", base)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })

	f := &fixture{t: t, state: state}
	f.run = store.Run{ID: "run", Project: repo, BaseRef: base, IntegrationBranch: branch, IntegrationPath: integration.Path, Status: "planned"}
	if err := state.CreateRun(ctx, f.run); err != nil {
		t.Fatal(err)
	}
	for i, agent := range agents {
		agent.RunID, agent.ID = "run", agent.Name+string(rune('0'+i))
		if err := state.SaveRunAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range tasks {
		task.RunID = "run"
		if task.Status == "" {
			task.Status = StatusQueued
		}
		if err := state.SaveTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	// By default a worker creates <task>.txt and the lead approves.
	f.worker = func(req TurnRequest) (string, error) {
		writeFile(t, req.Cwd, planID(req.TaskID)+".txt", "done\n")
		return "created " + planID(req.TaskID) + ".txt", nil
	}
	f.review = func(TurnRequest) string { return "APPROVE\nlooks good" }
	t.Setenv("XDG_STATE_HOME", stateDir) // keep config.StateDir() out of $HOME
	return f
}

func (f *fixture) engine(cfg config.RunConfig) *Engine {
	f.t.Helper()
	cfg.LeadReview = true
	e, err := New(context.Background(), Options{
		Store:    f.state,
		RunID:    "run",
		Config:   cfg,
		StateDir: filepath.Dir(filepath.Dir(f.run.IntegrationPath)),
		Rules:    rules.Set{},
		Turn: func(ctx context.Context, req TurnRequest) (TurnResult, error) {
			if strings.HasPrefix(req.Label, "review ") {
				return TurnResult{Output: f.review(req)}, nil
			}
			f.prompts.Store(req.TaskID, req.Prompt)
			out, err := f.worker(req)
			if ctx.Err() != nil {
				return TurnResult{}, ctx.Err()
			}
			return TurnResult{Output: out, SessionID: "ses-" + planID(req.TaskID)}, err
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}

func (f *fixture) task(id string) store.Task {
	f.t.Helper()
	task, ok, err := f.state.GetTask(context.Background(), id)
	if err != nil || !ok {
		f.t.Fatalf("GetTask(%s) = %v, %v", id, ok, err)
	}
	return task
}

var (
	lead     = store.RunAgent{Name: "lead", Role: "lead", Driver: "fake"}
	builder1 = store.RunAgent{Name: "builder-1", Role: "builder", Driver: "fake"}
	builder2 = store.RunAgent{Name: "builder-2", Role: "builder", Driver: "fake"}
)

func builderTask(id string, deps ...string) store.Task {
	for i, dep := range deps {
		deps[i] = "run:" + dep
	}
	return store.Task{ID: "run:" + id, Title: id, Brief: "make " + id, Role: "builder", Deps: deps, Verify: []string{"test -f " + id + ".txt"}}
}

func TestPipelineDependencyFlowsThroughMerge(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"), builderTask("b", "a"))
	ctx := context.Background()
	e := f.engine(config.RunConfig{})

	outcome, err := e.Run(ctx)
	if err != nil || outcome != RunWaiting {
		t.Fatalf("first Run = %q, %v; want review", outcome, err)
	}
	a := f.task("run:a")
	if a.Status != StatusReview || a.Commit == "" || !strings.Contains(a.Review, "APPROVE") || a.SessionID != "ses-a" {
		t.Fatalf("a = %+v, want review with commit, lead notes, session", a)
	}
	if b := f.task("run:b"); b.Status != StatusQueued {
		t.Fatalf("b dispatched before its dependency merged: %s", b.Status)
	}
	if diff, err := e.Diff(ctx, "run:a"); err != nil || !strings.Contains(diff, "a.txt") {
		t.Fatalf("Diff(a) = %q, %v", diff, err)
	}

	if err := e.Approve(ctx, "run:a"); err != nil {
		t.Fatalf("Approve(a): %v", err)
	}
	if a := f.task("run:a"); a.Status != StatusMerged || a.Worktree != "" {
		t.Fatalf("after approve a = %s worktree=%q", a.Status, a.Worktree)
	}
	if _, err := os.Stat(filepath.Join(f.run.IntegrationPath, "a.txt")); err != nil {
		t.Fatalf("a.txt not merged into integration: %v", err)
	}

	// b's worktree branches from the integration branch, so it sees a's work.
	f.worker = func(req TurnRequest) (string, error) {
		if _, err := os.Stat(filepath.Join(req.Cwd, "a.txt")); err != nil {
			return "", errors.New("dependency work missing from worktree")
		}
		writeFile(t, req.Cwd, "b.txt", "done\n")
		return "ok", nil
	}
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("second Run = %q, %v", outcome, err)
	}
	if err := e.Approve(ctx, "run:b"); err != nil {
		t.Fatalf("Approve(b): %v", err)
	}
	if outcome, _ := e.Run(ctx); outcome != RunCompleted {
		t.Fatalf("final Run = %q, want completed", outcome)
	}
	if evidence, _ := f.state.Verifications(ctx, "run:b"); len(evidence) != 2 || evidence[1].Scope != "integration" {
		t.Fatalf("evidence = %+v, want task and integration checks", evidence)
	}
	if err := e.Approve(ctx, "run:b"); err == nil {
		t.Fatal("approved a merged task twice")
	}
}

func TestVerificationFailureRetriesSameWorkerThenBlocks(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, builderTask("a"))
	ctx := context.Background()

	var workers []string
	var mu sync.Mutex
	f.worker = func(req TurnRequest) (string, error) {
		mu.Lock()
		workers = append(workers, req.Agent.Name)
		mu.Unlock()
		if strings.Contains(req.Prompt, "verification failed") {
			writeFile(t, req.Cwd, "a.txt", "fixed\n")
		}
		return "tried", nil
	}
	e := f.engine(config.RunConfig{MaxAttempts: 3})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if a := f.task("run:a"); a.Attempts != 2 || a.Status != StatusReview {
		t.Fatalf("a = attempts %d status %s; want review on attempt 2", a.Attempts, a.Status)
	}
	if len(workers) != 2 || workers[0] != workers[1] {
		t.Fatalf("workers = %v, want the same worker twice", workers)
	}
	prompt, _ := f.prompts.Load("run:a")
	if !strings.Contains(prompt.(string), "test -f a.txt") || !strings.Contains(prompt.(string), "attempt 2") {
		t.Fatalf("retry prompt lacks failure evidence:\n%s", prompt)
	}

	// A worker that never fixes it is blocked at the limit.
	g := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	g.worker = func(TurnRequest) (string, error) { return "nope", nil }
	if outcome, _ := g.engine(config.RunConfig{MaxAttempts: 2}).Run(ctx); outcome != RunBlocked {
		t.Fatalf("Run = %q, want blocked", outcome)
	}
	if a := g.task("run:a"); a.Status != StatusBlocked || a.Attempts != 2 || !strings.Contains(a.LastError, "attempt limit") {
		t.Fatalf("a = %+v", a)
	}
}

func TestLeadRejectionAndUserRejectionSendWorkBack(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	ctx := context.Background()
	var reviews atomic.Int32
	f.review = func(TurnRequest) string {
		if reviews.Add(1) == 1 {
			return "REJECT\nadd a newline"
		}
		return "**APPROVE**"
	}
	e := f.engine(config.RunConfig{})
	if outcome, _ := e.Run(ctx); outcome != RunWaiting {
		t.Fatalf("Run = %q", outcome)
	}
	a := f.task("run:a")
	if a.Attempts != 2 || reviews.Load() != 2 {
		t.Fatalf("attempts=%d reviews=%d, want lead reject then approve", a.Attempts, reviews.Load())
	}
	prompt, _ := f.prompts.Load("run:a")
	if !strings.Contains(prompt.(string), "add a newline") {
		t.Fatal("lead feedback not sent to the worker")
	}

	if err := e.Reject(ctx, "run:a", "rename the file"); err != nil {
		t.Fatal(err)
	}
	if a := f.task("run:a"); a.Status != StatusQueued || !strings.Contains(a.Feedback, "rename the file") {
		t.Fatalf("after user reject a = %s %q", a.Status, a.Feedback)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	for _, limit := range []int{1, 2} {
		f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, builderTask("a"), builderTask("b"))
		var inFlight, peak atomic.Int32
		f.worker = func(req TurnRequest) (string, error) {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(150 * time.Millisecond)
			inFlight.Add(-1)
			writeFile(t, req.Cwd, planID(req.TaskID)+".txt", "x\n")
			return "ok", nil
		}
		if outcome, err := f.engine(config.RunConfig{MaxConcurrency: limit}).Run(context.Background()); err != nil || outcome != RunWaiting {
			t.Fatalf("limit %d: Run = %q, %v", limit, outcome, err)
		}
		if int(peak.Load()) != limit {
			t.Fatalf("limit %d: peak concurrency %d", limit, peak.Load())
		}
		if a, b := f.task("run:a"), f.task("run:b"); a.Worktree == b.Worktree {
			t.Fatal("two tasks shared a worktree")
		}
	}
}

func TestMergeConflictBlocksTask(t *testing.T) {
	a, b := builderTask("a"), builderTask("b")
	a.Verify, b.Verify = nil, nil
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, a, b)
	f.worker = func(req TurnRequest) (string, error) {
		writeFile(t, req.Cwd, "README.md", planID(req.TaskID)+"\n")
		return "edited", nil
	}
	ctx := context.Background()
	e := f.engine(config.RunConfig{})
	if outcome, _ := e.Run(ctx); outcome != RunWaiting {
		t.Fatalf("Run = %q", outcome)
	}
	if err := e.Approve(ctx, "run:a"); err != nil {
		t.Fatal(err)
	}
	err := e.Approve(ctx, "run:b")
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("Approve(b) = %v, want conflict", err)
	}
	if b := f.task("run:b"); b.Status != StatusBlocked || !strings.Contains(b.LastError, "README.md") {
		t.Fatalf("b = %s %q", b.Status, b.LastError)
	}
	if err := e.Retry(ctx, "run:b"); err != nil {
		t.Fatal(err)
	}
	if b := f.task("run:b"); b.Status != StatusQueued || b.Attempts != 0 {
		t.Fatalf("after retry b = %s attempts=%d", b.Status, b.Attempts)
	}
}

func TestCancelInterruptsAndBudgetStops(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	f.worker = func(req TurnRequest) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	go func() { <-started; cancel() }()
	outcome, err := f.engine(config.RunConfig{}).Run(ctx)
	if outcome != RunStopped || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if a := f.task("run:a"); a.Status != StatusInterrupted {
		t.Fatalf("a = %s, want interrupted", a.Status)
	}

	g := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"), builderTask("b"))
	// One worker turn plus the lead review uses the budget of two turns.
	if outcome, _ := g.engine(config.RunConfig{MaxTurns: 2}).Run(context.Background()); outcome != RunWaiting {
		t.Fatalf("budget Run = %q", outcome)
	}
	if b := g.task("run:b"); b.Status != StatusQueued {
		t.Fatalf("b = %s, want left queued by the budget", b.Status)
	}
}

func TestRecoverAndReady(t *testing.T) {
	running := builderTask("a")
	running.Status, running.Assignee = StatusRunning, "builder-1"
	approved := builderTask("b")
	approved.Status = StatusApproved
	f := newFixture(t, []store.RunAgent{lead, builder1}, running, approved)
	if err := f.engine(config.RunConfig{}).recover(); err != nil {
		t.Fatal(err)
	}
	if a := f.task("run:a"); a.Status != StatusInterrupted {
		t.Fatalf("a = %s, want interrupted", a.Status)
	}
	if b := f.task("run:b"); b.Status != StatusReview {
		t.Fatalf("b = %s, want back in review", b.Status)
	}

	agents := []store.RunAgent{lead, builder1, builder2, {Name: "tester-1", Role: "tester"}}
	tasks := []store.Task{
		{ID: "x", Role: "builder", Status: StatusQueued, Assignee: "builder-2"}, // retry goes back to builder-2
		{ID: "y", Role: "builder", Status: StatusQueued},
		{ID: "z", Role: "tester", Status: StatusQueued, Deps: []string{"x"}},
		{ID: "w", Role: "lead", Status: StatusQueued},
	}
	got := Ready(tasks, agents, map[string]bool{}, 10)
	if len(got) != 2 || got[0].agent.Name != "builder-2" || got[1].agent.Name != "builder-1" {
		t.Fatalf("Ready = %+v", got)
	}
	if got := Ready(tasks, agents, map[string]bool{"builder-1": true, "builder-2": true}, 10); len(got) != 0 {
		t.Fatalf("Ready with busy builders = %+v", got)
	}
}

func TestParseVerdict(t *testing.T) {
	for text, want := range map[string]verdict{
		"APPROVE\nfine":            verdictApprove,
		"**Reject**: missing test": verdictReject,
		"Looks fine to me":         verdictUndecided,
		"# APPROVE":                verdictApprove,
	} {
		if got, _ := parseVerdict(text); got != want {
			t.Errorf("parseVerdict(%q) = %v, want %v", text, got, want)
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunLockAndCrossProcessApproval(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"), builderTask("b", "a"))
	stateDir := filepath.Dir(filepath.Dir(f.run.IntegrationPath))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The driving engine waits for approvals and polls the store for them.
	driver := f.engine(config.RunConfig{})
	driver.opts.WaitForReview, driver.opts.Poll = true, 50*time.Millisecond
	result := make(chan error, 1)
	go func() { _, err := driver.Run(ctx); result <- err }()

	waitFor := func(id, status string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for f.task(id).Status != status {
			if time.Now().After(deadline) {
				t.Fatalf("%s never reached %s (is %s)", id, status, f.task(id).Status)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("run:a", StatusReview)

	if pid, ok := RunOwner(stateDir, "run"); !ok || pid != os.Getpid() {
		t.Fatalf("RunOwner = %d, %v", pid, ok)
	}
	if _, err := f.engine(config.RunConfig{}).Run(ctx); !errors.Is(err, ErrRunBusy) {
		t.Fatalf("second driver err = %v, want ErrRunBusy", err)
	}

	// Another process approves; the driver notices on its next poll.
	if err := f.engine(config.RunConfig{}).Approve(ctx, "run:a"); err != nil {
		t.Fatal(err)
	}
	waitFor("run:b", StatusReview)

	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("driver err = %v", err)
	}
	if _, ok := RunOwner(stateDir, "run"); ok {
		t.Fatal("lock still held after the driver stopped")
	}
}
