package execution

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	return newProjectFixture(t, "", agents, tasks...)
}

// newProjectFixture is newFixture with the project in dir, a subdirectory of
// the repository holding package.json, as in a monorepo; "" is the root.
func newProjectFixture(t *testing.T, dir string, agents []store.RunAgent, tasks ...store.Task) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	repo := t.TempDir()
	git(t, repo, "init")
	writeFile(t, repo, "README.md", "base\n")
	if dir != "" {
		if err := os.Mkdir(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(repo, dir), "package.json", "{}\n")
	}
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
	f.run = store.Run{ID: "run", Project: filepath.Join(repo, dir), BaseRef: base, IntegrationBranch: branch, IntegrationPath: integration.Path, Status: "planned"}
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
	// The merged task's worktree is gone, but its diff is still there to see.
	if diff, err := e.Diff(ctx, "run:a"); err != nil || !strings.Contains(diff, "a.txt") {
		t.Fatalf("Diff(a) after merge = %q, %v", diff, err)
	}
	// The integration worktree holds the branch again, so git refuses to check
	// it out elsewhere, where the next merge would move it under that index.
	if out, err := exec.Command("git", "-C", f.run.Project, "checkout", f.run.IntegrationBranch).CombinedOutput(); err == nil {
		t.Fatalf("the integration branch could be checked out in the main repo: %s", out)
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

	// A worker that never fixes it is blocked at the limit, and the run says so.
	g := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	g.worker = func(TurnRequest) (string, error) { return "nope", nil }
	if outcome, _ := g.engine(config.RunConfig{MaxAttempts: 2}).Run(ctx); outcome != RunBlocked {
		t.Fatalf("Run = %q, want blocked", outcome)
	}
	if a := g.task("run:a"); a.Status != StatusBlocked || a.Attempts != 2 || !strings.HasPrefix(a.LastError, "attempt limit 2 reached: verification failed") {
		t.Fatalf("a = %+v", a)
	}
	if events, _ := g.state.Events(ctx, "run", 1); len(events) != 1 || events[0].Message != "blocked: a: blocked: attempt limit 2 reached: verification failed:" {
		t.Fatalf("run outcome event = %+v", events)
	}
}

// flaky is a command that fails on its first run only, as a flaky test does:
// what decides it is outside the worktree.
func flaky(t *testing.T) string {
	mark := filepath.Join(t.TempDir(), "ran")
	return "test -f " + mark + " || { touch " + mark + "; exit 1; }"
}

// A check that fails once and passes when rerun costs no attempt; its failure
// is kept, and the flake noted, as for setup.
func TestFlakyCheckIsRerunNotRetried(t *testing.T) {
	check, setup := flaky(t), flaky(t)
	a := builderTask("a")
	a.Verify = append(a.Verify, check)
	f := newFixture(t, []store.RunAgent{lead, builder1}, a)
	ctx := context.Background()
	var reviewed string
	f.review = func(req TurnRequest) string { reviewed = req.Prompt; return "APPROVE" }
	if outcome, err := f.engine(config.RunConfig{Setup: []string{setup}}).Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if a := f.task("run:a"); a.Status != StatusReview || a.Attempts != 1 {
		t.Fatalf("a = %s on attempt %d, want review on attempt 1", a.Status, a.Attempts)
	}
	evidence, _ := f.state.Verifications(ctx, "run:a")
	if len(evidence) != 3 || evidence[1].ExitCode != 1 || evidence[2].ExitCode != 0 || evidence[2].Command != evidence[1].Command {
		t.Fatalf("evidence = %+v, want the check, the flaky failure and its passing rerun", evidence)
	}
	if !strings.Contains(reviewed, "passed on a rerun (flaky)") {
		t.Fatalf("the lead was not told the check is flaky:\n%s", reviewed)
	}
	events, _ := f.state.Events(ctx, "run", 50)
	for _, command := range []string{check, setup} {
		if !slices.ContainsFunc(events, func(e store.Event) bool { return e.Kind == "flaky" && strings.Contains(e.Message, command) }) {
			t.Fatalf("no flaky event for %s in %+v", command, events)
		}
	}
}

// A rerun starts again from the commit, so a check cannot pass on what its
// failed run wrote, such as a golden file it creates when missing.
func TestRerunCannotPassOnWhatTheFailureWrote(t *testing.T) {
	a := builderTask("a")
	a.Verify = []string{"test -f golden || { touch golden; exit 1; }"}
	f := newFixture(t, []store.RunAgent{lead, builder1}, a)
	f.worker = func(req TurnRequest) (string, error) {
		writeFile(t, req.Cwd, "a.txt", "done\n")
		if strings.Contains(req.Prompt, "verification failed") {
			writeFile(t, req.Cwd, "golden", "{}\n")
		}
		return "tried", nil
	}
	ctx := context.Background()
	e := f.engine(config.RunConfig{Verify: []string{"test -f golden2 || { touch golden2; exit 1; }"}})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if a := f.task("run:a"); a.Attempts != 2 {
		t.Fatalf("a went to review on attempt %d, want 2: its first check failed", a.Attempts)
	}
	if err := e.Approve(ctx, "run:a"); err == nil || !strings.Contains(err.Error(), "golden2") {
		t.Fatalf("Approve = %v, want the run's check to fail", err)
	}
}

// Stopping a worker while its task's setup runs stops the setup, and blocks
// the task as a stopped turn does, before any work; the stop ends with it.
func TestStopWorkerStopsSetup(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	var worked atomic.Bool
	f.worker = func(req TurnRequest) (string, error) {
		worked.Store(true)
		writeFile(t, req.Cwd, "a.txt", "done\n")
		return "done", nil
	}
	ctx := context.Background()
	e := f.engine(config.RunConfig{Setup: []string{"sleep 5"}})
	go func() {
		for !e.StopWorker("builder-1") {
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if outcome, err := e.Run(ctx); err != nil || outcome != RunBlocked {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if a := f.task("run:a"); worked.Load() || a.Status != StatusBlocked || !strings.HasPrefix(a.LastError, "stopped by you") {
		t.Fatalf("a = %s: %s (worked: %v); want blocked by the stop, before any work", a.Status, a.LastError, worked.Load())
	}
	e.opts.Config.Setup = []string{"true"}
	if err := e.Retry(ctx, "run:a"); err != nil {
		t.Fatal(err)
	}
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run after retry = %q, %v (a: %s)", outcome, err, f.task("run:a").LastError)
	}
}

// Tasks running at once get different slots, and every check is told its
// slot, task and run; integration checks, one at a time, get slot 0.
func TestTasksAreToldTheirSlot(t *testing.T) {
	a, b := builderTask("a"), builderTask("b")
	a.Verify = []string{`test "$SWITCH_TASK" = a && test "$SWITCH_RUN" = run && test -n "$SWITCH_SLOT"`}
	b.Verify = []string{`test "$SWITCH_TASK" = b && test -n "$SWITCH_SLOT"`}
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, a, b)
	var slots sync.Map // task ID -> its worker's SWITCH_SLOT
	f.worker = func(req TurnRequest) (string, error) {
		for _, kv := range req.Env {
			if slot, ok := strings.CutPrefix(kv, "SWITCH_SLOT="); ok && strings.Contains(req.Prompt, kv) {
				slots.Store(req.TaskID, slot)
			}
		}
		writeFile(t, req.Cwd, planID(req.TaskID)+".txt", "done\n")
		return "done", nil
	}
	ctx := context.Background()
	e := f.engine(config.RunConfig{Verify: []string{`test "$SWITCH_SLOT" = 0`}})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v (a: %s, b: %s)", outcome, err, f.task("run:a").LastError, f.task("run:b").LastError)
	}
	slotA, okA := slots.Load("run:a")
	slotB, okB := slots.Load("run:b")
	if !okA || !okB || slotA == slotB {
		t.Fatalf("slots a=%v b=%v; want one each, in env and prompt, and different", slotA, slotB)
	}
	if err := e.Approve(ctx, "run:a"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
}

// Setup runs in a task's worktree before its first attempt only, and in the
// integration worktree after each merge, before its checks; when it fails,
// the task is blocked with its output.
func TestSetupPreparesEachWorktree(t *testing.T) {
	log := filepath.Join(t.TempDir(), "setup.log")
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	f.worker = func(req TurnRequest) (string, error) {
		if _, err := os.Stat(log); err != nil {
			return "", errors.New("setup did not run before the worker")
		}
		if strings.Contains(req.Prompt, "verification failed") {
			writeFile(t, req.Cwd, "a.txt", "done\n")
		}
		return "tried", nil
	}
	ctx := context.Background()
	e := f.engine(config.RunConfig{Setup: []string{`echo "$SWITCH_TASK $SWITCH_SLOT" >> ` + log}})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v (a: %s)", outcome, err, f.task("run:a").LastError)
	}
	if err := e.Approve(ctx, "run:a"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got, _ := os.ReadFile(log); string(got) != "a 2\na 0\n" {
		t.Fatalf("setup ran as %q, want once for the task's two attempts, then once for its merge", got)
	}

	g := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	g.worker = func(TurnRequest) (string, error) { return "", errors.New("worked without its setup") }
	if outcome, err := g.engine(config.RunConfig{Setup: []string{"echo no node_modules >&2; exit 1"}}).Run(ctx); err != nil || outcome != RunBlocked {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if a := g.task("run:a"); a.Status != StatusBlocked || !strings.Contains(a.LastError, "setup failed") || !strings.Contains(a.LastError, "no node_modules") {
		t.Fatalf("a = %s: %s", a.Status, a.LastError)
	}

	// Setup that fails in the integration worktree merges nothing.
	h := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	e = h.engine(config.RunConfig{Setup: []string{`test "$SWITCH_SLOT" != 0 || { echo no database >&2; exit 1; }`}})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if err := e.Approve(ctx, "run:a"); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Fatalf("Approve = %v, want the integration setup's failure", err)
	}
	if a := h.task("run:a"); a.Status != StatusBlocked {
		t.Fatalf("a = %s, want blocked", a.Status)
	}
}

// The run's own checks run with every merge, along with the task's, and once
// more before the run is completed; failing then, the run is blocked.
func TestRunChecksGuardTheIntegrationBranch(t *testing.T) {
	dir := t.TempDir()
	broken, flake := filepath.Join(dir, "broken"), filepath.Join(dir, "flake")
	flakes := "test ! -f " + flake + " || { rm " + flake + "; exit 1; }" // fails once after flake is made
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	ctx := context.Background()
	e := f.engine(config.RunConfig{Verify: []string{"test -f a.txt", "test ! -f " + broken, flakes}})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if err := e.Approve(ctx, "run:a"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	var integration []string
	evidence, _ := f.state.Verifications(ctx, "run:a")
	for _, v := range evidence {
		if v.Scope == "integration" {
			integration = append(integration, v.Command)
		}
	}
	if strings.Join(integration, ";") != "test -f a.txt;test ! -f "+broken+";"+flakes {
		t.Fatalf("integration checks = %q, want the task's and then the run's, each once", integration)
	}
	writeFile(t, dir, "broken", "")
	outcome, err := e.Run(ctx)
	if outcome != RunBlocked || err == nil || !strings.Contains(err.Error(), "test ! -f "+broken) {
		t.Fatalf("Run = %q, %v; want blocked by the run's checks", outcome, err)
	}
	if run, _, _ := f.state.GetRun(ctx, "run"); run.Status != RunBlocked {
		t.Fatalf("run status = %s", run.Status)
	}
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "flake", "")
	if outcome, err := e.Run(ctx); err != nil || outcome != RunCompleted {
		t.Fatalf("Run = %q, %v; want completed", outcome, err)
	}
	events, _ := f.state.Events(ctx, "run", 50)
	if !slices.ContainsFunc(events, func(e store.Event) bool { return e.Kind == "flaky" && strings.Contains(e.Message, flakes) }) {
		t.Fatalf("no flaky event for the run's check in %+v", events)
	}
	if status := git(t, f.run.IntegrationPath, "status", "--porcelain"); status != "" {
		t.Fatalf("integration worktree left dirty: %s", status)
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
	// Setup reading a conflicted file, as npm ci reads package.json, must not
	// keep the retried worker from resolving it.
	e := f.engine(config.RunConfig{Setup: []string{"! grep -q '<<<<<<<' README.md"}})
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
	b = f.task("run:b")
	if b.Status != StatusBlocked || !strings.Contains(b.LastError, "README.md") {
		t.Fatalf("b = %s %q", b.Status, b.LastError)
	}
	// The integration branch is merged into b's worktree, markers and all, so
	// the retried worker resolves the conflict instead of redoing its edit.
	if got, _ := os.ReadFile(filepath.Join(b.Worktree, "README.md")); !strings.Contains(string(got), "<<<<<<<") {
		t.Fatalf("b's README.md has no conflict markers:\n%s", got)
	}
	if err := e.Retry(ctx, "run:b"); err != nil {
		t.Fatal(err)
	}
	// Any worker may take the retry, so the last one's session is not resumed.
	if b := f.task("run:b"); b.Status != StatusQueued || b.Attempts != 0 || b.SessionID != "" {
		t.Fatalf("after retry b = %s attempts=%d session=%q", b.Status, b.Attempts, b.SessionID)
	}
	f.worker = func(req TurnRequest) (string, error) {
		if !strings.Contains(req.Prompt, "conflict markers") {
			return "", errors.New("the worker was not told to resolve the conflict")
		}
		writeFile(t, req.Cwd, "README.md", "a\nb\n")
		return "resolved", nil
	}
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run after retry = %q, %v", outcome, err)
	}
	if err := e.Approve(ctx, "run:b"); err != nil {
		t.Fatalf("Approve(b) after resolving: %v", err)
	}
	if got := git(t, f.run.Project, "show", f.run.IntegrationBranch+":README.md"); got != "a\nb" {
		t.Fatalf("integration README.md = %q", got)
	}
}

// A conflict with work merged meanwhile goes back to the worker, with the
// markers in place, before the lead spends a review on it.
func TestConflictGoesBackToTheWorkerBeforeReview(t *testing.T) {
	a, b := builderTask("a"), builderTask("b")
	a.Verify, b.Verify = nil, nil
	f := newFixture(t, []store.RunAgent{lead, builder1}, a, b)
	ctx := context.Background()
	e := f.engine(config.RunConfig{MaxConcurrency: 1})
	var reviews atomic.Int32
	f.review = func(TurnRequest) string { reviews.Add(1); return "APPROVE" }
	f.worker = func(req TurnRequest) (string, error) {
		switch {
		case req.TaskID == "run:a":
			writeFile(t, req.Cwd, "README.md", "a\n")
		case strings.Contains(req.Prompt, "conflict markers"):
			// Keeping its own side and staging it leaves git status empty;
			// the merge must still be committed.
			writeFile(t, req.Cwd, "README.md", "b\n")
			git(t, req.Cwd, "add", "README.md")
		default: // a merges while b is being worked on
			if err := e.Approve(ctx, "run:a"); err != nil {
				return "", err
			}
			writeFile(t, req.Cwd, "README.md", "b\n")
		}
		return "edited", nil
	}
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if b := f.task("run:b"); b.Status != StatusReview || b.Attempts != 2 || reviews.Load() != 2 {
		t.Fatalf("b = %s on attempt %d after %d reviews; want review on attempt 2, the conflict never reviewed", b.Status, b.Attempts, reviews.Load())
	}
	if err := e.Approve(ctx, "run:b"); err != nil {
		t.Fatalf("Approve(b): %v", err)
	}
	if got := git(t, f.run.Project, "show", f.run.IntegrationBranch+":README.md"); got != "b" {
		t.Fatalf("integration README.md = %q", got)
	}
}

// A worker that leaves the markers of a conflict in place is sent back:
// committing would conclude the merge with them, and nothing after notices.
func TestUnresolvedConflictNeverReachesReview(t *testing.T) {
	a, b := builderTask("a"), builderTask("b")
	a.Verify, b.Verify = nil, nil
	f := newFixture(t, []store.RunAgent{lead, builder1}, a, b)
	ctx := context.Background()
	e := f.engine(config.RunConfig{MaxConcurrency: 1, MaxAttempts: 3})
	f.worker = func(req TurnRequest) (string, error) {
		switch {
		case req.TaskID == "run:a":
			writeFile(t, req.Cwd, "README.md", "a\n")
		case strings.Contains(req.Prompt, "conflict markers"): // ignores them
		default:
			if err := e.Approve(ctx, "run:a"); err != nil {
				return "", err
			}
			writeFile(t, req.Cwd, "README.md", "b\n")
		}
		return "edited", nil
	}
	if outcome, err := e.Run(ctx); err != nil || outcome != RunBlocked {
		t.Fatalf("Run = %q, %v; want blocked", outcome, err)
	}
	if b := f.task("run:b"); b.Status != StatusBlocked || !strings.Contains(b.LastError, "conflict markers are still in README.md") {
		t.Fatalf("b = %s %q", b.Status, b.LastError)
	}
	if got := git(t, f.run.Project, "show", f.run.IntegrationBranch+":README.md"); got != "a" {
		t.Fatalf("integration README.md = %q", got)
	}
}

// A crashed worker's check, left verifying while an approval held the merge
// lock at Run start, is taken up again once the lock is free.
func TestStaleVerifyingRecoversOnceTheMergeLockIsFree(t *testing.T) {
	stale := builderTask("a")
	stale.Status, stale.Assignee = StatusVerifying, "builder-1"
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, stale, builderTask("c"))
	lock, err := TryLockMerge(f.run.IntegrationPath)
	if err != nil || lock == nil {
		t.Fatalf("TryLockMerge = %v, %v", lock, err)
	}
	defer lock.Release()
	f.worker = func(req TurnRequest) (string, error) {
		if req.TaskID == "run:c" {
			lock.Release() // the approval ends while c runs
		}
		writeFile(t, req.Cwd, planID(req.TaskID)+".txt", "done\n")
		return "done", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// a's worktree was never made; the stale check is all that is left of it.
	if outcome, err := f.engine(config.RunConfig{}).Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if a := f.task("run:a"); a.Status != StatusReview {
		t.Fatalf("a = %s %q, want recovered and done", a.Status, a.LastError)
	}
}

// An approval ending the run holds its lock with no owner, so neither a TUI
// nor /stop takes it for a process driving the run.
func TestFinishRunNamesNoOwner(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	ctx := context.Background()
	// The run check, after the merge's, is slow enough to be seen.
	mark := filepath.Join(t.TempDir(), "merged")
	e := f.engine(config.RunConfig{Verify: []string{"if test -f " + mark + "; then sleep 1; else touch " + mark + "; fi"}})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	approved := make(chan error, 1)
	go func() { approved <- e.Approve(ctx, "run:a") }()
	for {
		select {
		case err := <-approved:
			if err != nil {
				t.Fatal(err)
			}
			if run, _, _ := f.state.GetRun(ctx, "run"); run.Status != RunCompleted {
				t.Fatalf("run = %s, want completed", run.Status)
			}
			return
		default:
		}
		if pid, ok := RunOwner(e.opts.StateDir, "run"); ok {
			t.Fatalf("RunOwner during the approval = %d, want nobody", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEmptyChangeNeverReachesReview(t *testing.T) {
	a := builderTask("a")
	a.Verify = nil
	f := newFixture(t, []store.RunAgent{lead, builder1}, a)
	f.worker = func(TurnRequest) (string, error) { return "nothing to do", nil }
	var reviews atomic.Int32
	f.review = func(TurnRequest) string { reviews.Add(1); return "APPROVE" }
	if outcome, err := f.engine(config.RunConfig{MaxAttempts: 2}).Run(context.Background()); err != nil || outcome != RunBlocked {
		t.Fatalf("Run = %q, %v; want blocked", outcome, err)
	}
	if a := f.task("run:a"); a.Status != StatusBlocked || !strings.Contains(a.LastError, "no changes") || reviews.Load() != 0 {
		t.Fatalf("a = %s %q after %d reviews", a.Status, a.LastError, reviews.Load())
	}
}

// The worker commits on a branch of its own, as some project rules ask; the
// commit that was verified and reviewed is still what merges.
func TestApproveMergesTheReviewedCommit(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	f.worker = func(req TurnRequest) (string, error) {
		git(t, req.Cwd, "checkout", "-q", "-b", "feature/a")
		writeFile(t, req.Cwd, "a.txt", "done\n")
		return "done", nil
	}
	ctx := context.Background()
	e := f.engine(config.RunConfig{})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if err := e.Approve(ctx, "run:a"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got := git(t, f.run.Project, "show", f.run.IntegrationBranch+":a.txt"); got != "done" {
		t.Fatalf("integration a.txt = %q", got)
	}
}

// Checks that write reports must not get them committed, nor leave the
// integration worktree dirty for the next merge.
func TestVerificationLeavesNoTrace(t *testing.T) {
	a, b := builderTask("a"), builderTask("b")
	a.Verify = append(a.Verify, "echo a > report.xml")
	b.Verify = append(b.Verify, "echo b > report.xml")
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, a, b)
	ctx := context.Background()
	e := f.engine(config.RunConfig{})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	for _, id := range []string{"run:a", "run:b"} {
		if diff, err := e.Diff(ctx, id); err != nil || strings.Contains(diff, "report.xml") {
			t.Fatalf("%s committed the check's report (err %v):\n%s", id, err, diff)
		}
		if status := git(t, f.task(id).Worktree, "status", "--porcelain"); status != "" {
			t.Fatalf("%s worktree left dirty: %s", id, status)
		}
	}
	for _, id := range []string{"run:a", "run:b"} {
		if err := e.Approve(ctx, id); err != nil {
			t.Fatalf("Approve(%s): %v", id, err)
		}
	}
	if status := git(t, f.run.IntegrationPath, "status", "--porcelain"); status != "" {
		t.Fatalf("integration worktree left dirty: %s", status)
	}
}

// Approvals from two engines, as from the TUI and `__control approve`, merge
// and verify one at a time, so one check never sees, or undoes, another's merge.
func TestApprovalsFromTwoEnginesMergeOneAtATime(t *testing.T) {
	a := builderTask("a")
	a.Verify = append(a.Verify, "sleep 0.5; test ! -f b.txt")
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, a, builderTask("b"))
	ctx := context.Background()
	if outcome, err := f.engine(config.RunConfig{}).Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	engines := []*Engine{f.engine(config.RunConfig{}), f.engine(config.RunConfig{})}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, id := range []string{"run:a", "run:b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 100 * time.Millisecond) // b while a's check runs
			errs[i] = engines[i].Approve(ctx, id)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Approve #%d: %v", i, err)
		}
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if got := git(t, f.run.Project, "show", f.run.IntegrationBranch+":"+name); got != "done" {
			t.Errorf("integration %s = %q", name, got)
		}
	}
}

// An approval cut short mid-check, by a timeout or by quitting, leaves the
// integration branch as it was and the task ready to approve again.
func TestInterruptedApprovalLeavesNothingMerged(t *testing.T) {
	stall := filepath.Join(t.TempDir(), "stall")
	a := builderTask("a")
	a.Verify = append(a.Verify, "test ! -f "+stall+" || sleep 30")
	f := newFixture(t, []store.RunAgent{lead, builder1}, a)
	ctx := context.Background()
	e := f.engine(config.RunConfig{})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	base := git(t, f.run.Project, "rev-parse", f.run.IntegrationBranch)
	writeFile(t, filepath.Dir(stall), "stall", "")
	short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := e.Approve(short, "run:a"); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("Approve = %v, want interrupted", err)
	}
	if a := f.task("run:a"); a.Status != StatusReview {
		t.Fatalf("a = %s, want back in review", a.Status)
	}
	if head := git(t, f.run.Project, "rev-parse", f.run.IntegrationBranch); head != base {
		t.Fatal("the integration branch kept an unverified merge")
	}
	if status := git(t, f.run.IntegrationPath, "status", "--porcelain"); status != "" {
		t.Fatalf("integration worktree left dirty: %s", status)
	}
	if err := os.Remove(stall); err != nil {
		t.Fatal(err)
	}
	// An approval killed outright leaves its task approved; approving again works.
	killed := f.task("run:a")
	killed.Status = StatusApproved
	if err := f.state.SaveTask(ctx, killed); err != nil {
		t.Fatal(err)
	}
	if err := e.Approve(ctx, "run:a"); err != nil {
		t.Fatalf("approving again: %v", err)
	}
	if got := git(t, f.run.Project, "show", f.run.IntegrationBranch+":a.txt"); got != "done" {
		t.Fatalf("integration a.txt = %q", got)
	}
}

// The engine's transitions go on while an approval runs its integration check.
func TestApprovalDoesNotHoldUpTheRun(t *testing.T) {
	stall := filepath.Join(t.TempDir(), "stall")
	a := builderTask("a")
	a.Verify = append(a.Verify, "test ! -f "+stall+" || sleep 30")
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, a, builderTask("b"))
	ctx := context.Background()
	e := f.engine(config.RunConfig{})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	writeFile(t, filepath.Dir(stall), "stall", "")
	approving, cancel := context.WithCancel(ctx)
	approved := make(chan error, 1)
	go func() { approved <- e.Approve(approving, "run:a") }()
	for f.task("run:a").Status != StatusApproved {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // into the integration check
	rejected := make(chan error, 1)
	go func() { rejected <- e.Reject(ctx, "run:b", "redo it") }()
	var err error
	select {
	case err = <-rejected:
	case <-time.After(5 * time.Second):
		err = errors.New("Reject waited for another task's integration check")
	}
	cancel()
	<-approved
	if err != nil {
		t.Fatal(err)
	}
}

// Two decisions on one task, from separate engines or processes, never both win.
func TestConcurrentRetriesOnlyOneWins(t *testing.T) {
	blocked := builderTask("a")
	blocked.Status = StatusBlocked
	f := newFixture(t, []store.RunAgent{lead, builder1}, blocked)
	ctx := context.Background()
	engines := make([]*Engine, 8)
	for i := range engines {
		engines[i] = f.engine(config.RunConfig{})
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, e := range engines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.Retry(ctx, "run:a") == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d retries won, want 1", wins.Load())
	}
}

// Accepting a task nobody attempted, such as the chat lead's proposal, does
// not tell its worker that the note holding it back was an earlier attempt.
func TestRetryOfAnUnattemptedTaskBringsNoFeedback(t *testing.T) {
	proposal := builderTask("a")
	proposal.Status, proposal.LastError = StatusBlocked, "proposed by lead in the chat; /retry a accepts it"
	f := newFixture(t, []store.RunAgent{lead, builder1}, proposal)
	e := f.engine(config.RunConfig{})
	ctx := context.Background()
	if err := e.Retry(ctx, "run:a"); err != nil {
		t.Fatal(err)
	}
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if prompt, _ := f.prompts.Load("run:a"); strings.Contains(prompt.(string), "proposed by") {
		t.Fatalf("the worker got the proposal note as feedback:\n%s", prompt)
	}
}

// A proposal the user has not accepted does not hold the run back, and one
// accepted while the run's checks run is worked on, not left queued in a run
// marked completed.
func TestProposalDoesNotHoldTheRunBack(t *testing.T) {
	proposal := builderTask("p")
	proposal.Status, proposal.LastError = StatusBlocked, ProposedBy+"lead in the chat; /retry p accepts it"
	f := newFixture(t, []store.RunAgent{lead, builder1}, proposal)
	ctx := context.Background()
	if outcome, err := f.engine(config.RunConfig{}).Run(ctx); err != nil || outcome != RunCompleted {
		t.Fatalf("Run with a proposal nobody accepted = %q, %v; want completed", outcome, err)
	}

	dir := t.TempDir()
	checking, accepted := filepath.Join(dir, "checking"), filepath.Join(dir, "accepted")
	e := f.engine(config.RunConfig{Verify: []string{"touch " + checking + "; until [ -f " + accepted + " ]; do sleep 0.05; done"}})
	go func() { // the user accepts it while the run's checks run
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(checking); err == nil {
				if err := e.Retry(ctx, "run:p"); err != nil {
					t.Error(err)
				}
				_ = os.WriteFile(accepted, nil, 0o644)
				return
			}
		}
	}()
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting || f.task("run:p").Status != StatusReview {
		t.Fatalf("Run = %q, %v; want the accepted proposal done and waiting for review", outcome, err)
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

// Running out of budget interrupts the turns still running at once, but not
// the checks of work already done, and an interrupted attempt is resumed
// later without spending another one.
func TestBudgetInterruptsAndInterruptionsCostNoAttempt(t *testing.T) {
	a := builderTask("a")
	released := filepath.Join(t.TempDir(), "released")
	a.Verify = append([]string{"until [ -f " + released + " ]; do sleep 0.05; done"}, a.Verify...) // still checking a when b ends
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, a, builderTask("b"))
	e := f.engine(config.RunConfig{MaxTurns: 1, VerifyTimeout: 10})
	fake := e.opts.Turn
	e.opts.Turn = func(ctx context.Context, req TurnRequest) (TurnResult, error) {
		if req.TaskID == "run:b" { // still working when a's turn uses up the budget
			defer os.WriteFile(released, nil, 0o644)
			select {
			case <-ctx.Done():
				return TurnResult{}, ctx.Err()
			case <-time.After(5 * time.Second):
				return TurnResult{}, errors.New("not interrupted")
			}
		}
		return fake(ctx, req)
	}
	if outcome, err := e.Run(context.Background()); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if b := f.task("run:b"); b.Status != StatusInterrupted || b.Attempts != 1 {
		t.Fatalf("b = %s on attempt %d, want interrupted on attempt 1: %s", b.Status, b.Attempts, b.LastError)
	}
	if a := f.task("run:a"); a.Status != StatusReview || a.Attempts != 1 {
		t.Fatalf("a = %s on attempt %d, want its finished turn checked and reviewed: %s", a.Status, a.Attempts, a.LastError)
	}
	if outcome, err := f.engine(config.RunConfig{}).Run(context.Background()); err != nil || outcome != RunWaiting {
		t.Fatalf("second Run = %q, %v", outcome, err)
	}
	if b := f.task("run:b"); b.Status != StatusReview || b.Attempts != 1 {
		t.Fatalf("b = %s on attempt %d, want review still on attempt 1", b.Status, b.Attempts)
	}
}

// A worker turn that sends nothing for idle_timeout is stopped and retried,
// as a timed-out one is; its events, and a pending permission request, keep
// it alive, whatever its subagents do meanwhile.
func TestWatchdogStopsASilentTurn(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	e := f.engine(config.RunConfig{IdleTimeout: 1})
	fake := e.opts.Turn
	var attempts atomic.Int32
	e.opts.Turn = func(ctx context.Context, req TurnRequest) (TurnResult, error) {
		if req.Agent.Role != "lead" {
			if attempts.Add(1) == 1 {
				<-ctx.Done() // hung
				return TurnResult{}, ctx.Err()
			}
			time.Sleep(600 * time.Millisecond)
			e.beat(req.TaskID, req.Agent.Name, false) // an event
			time.Sleep(600 * time.Millisecond)
			e.beat(req.TaskID, req.Agent.Name, true)      // a permission request the user takes a while to answer,
			e.beat(req.TaskID, "builder-1/helper", false) // while a subagent works on
			time.Sleep(1200 * time.Millisecond)
		}
		return fake(ctx, req)
	}
	if outcome, err := e.Run(context.Background()); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if a := f.task("run:a"); a.Status != StatusReview || a.Attempts != 2 {
		t.Fatalf("a = %s on attempt %d, want review on attempt 2", a.Status, a.Attempts)
	}
	if prompt, _ := f.prompts.Load("run:a"); !strings.Contains(prompt.(string), "builder-1 sent nothing for 1s") {
		t.Fatalf("the retry was not told why:\n%s", prompt)
	}
}

// A failed budget read while a worker is mid-turn neither abandons it nor
// releases the run under it.
func TestBudgetReadErrorWaitsForRunningWorkers(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	e := f.engine(config.RunConfig{MaxTurns: 50})
	release := make(chan struct{})
	f.worker = func(TurnRequest) (string, error) {
		f.state.Close() // every store call fails from here on
		e.signal()      // so Run reads the budget while this turn runs
		<-release
		return "", nil
	}
	result := make(chan error, 1)
	go func() { _, err := e.Run(context.Background()); result <- err }()
	select {
	case err := <-result:
		t.Fatalf("Run returned with a worker mid-turn: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("Run hid the store error")
	}
}

// The summary a worker gives with switch_complete, saved mid-turn, is what is
// kept and reviewed, and a new attempt never shows the last one's.
func TestWorkerSummary(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	ctx := context.Background()
	var reviewed string
	f.review = func(req TurnRequest) string { reviewed = req.Prompt; return "APPROVE" }
	f.worker = func(req TurnRequest) (string, error) {
		if !strings.Contains(req.Prompt, "verification failed") {
			return "first try", nil // no a.txt yet
		}
		if task := f.task(req.TaskID); task.Summary != "" {
			return "", errors.New("the new attempt starts with the last one's summary: " + task.Summary)
		}
		writeFile(t, req.Cwd, "a.txt", "done\n")
		task := f.task(req.TaskID) // as switch_complete saves it
		task.Summary = "wrote a.txt"
		if err := f.state.SaveTask(ctx, task); err != nil {
			return "", err
		}
		return "Let me check the file. Done.", nil
	}
	if outcome, err := f.engine(config.RunConfig{}).Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if a := f.task("run:a"); a.Summary != "wrote a.txt" || !strings.Contains(reviewed, "wrote a.txt") {
		t.Fatalf("summary %q; review prompt:\n%s", a.Summary, reviewed)
	}
	if !strings.Contains(reviewed, "Worker: builder-1 (fake)") {
		t.Fatalf("the lead was not told whose work it reviews:\n%s", reviewed)
	}
}

// Started in a subdirectory of the repository, as in a monorepo, Switch runs
// workers, reviews and checks in that subdirectory of each worktree.
func TestProjectInASubdirectory(t *testing.T) {
	a := builderTask("a")
	a.Verify = []string{"test -f package.json && test -f a.txt"}
	f := newProjectFixture(t, "api", []store.RunAgent{lead, builder1}, a)
	var reviewedIn string
	f.review = func(req TurnRequest) string { reviewedIn = req.Cwd; return "APPROVE" }
	ctx := context.Background()
	e := f.engine(config.RunConfig{MaxAttempts: 1})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v (a: %s)", outcome, err, f.task("run:a").LastError)
	}
	if want := filepath.Join(f.task("run:a").Worktree, "api"); reviewedIn != want {
		t.Fatalf("reviewed in %q, want %q", reviewedIn, want)
	}
	if err := e.Approve(ctx, "run:a"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got := git(t, f.run.Project, "show", f.run.IntegrationBranch+":api/a.txt"); got != "done" {
		t.Fatalf("integration api/a.txt = %q", got)
	}

	// A project with nothing committed yet is not checked out, here because
	// the integration branch lacks it, so its directory is made there.
	g := newProjectFixture(t, "web", []store.RunAgent{lead, builder1}, builderTask("b"))
	git(t, g.run.IntegrationPath, "rm", "-rq", "web")
	git(t, g.run.IntegrationPath, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "no web yet")
	g.worker = func(req TurnRequest) (string, error) {
		return "created b.txt", os.WriteFile(filepath.Join(req.Cwd, "b.txt"), []byte("done\n"), 0o644)
	}
	if outcome, err := g.engine(config.RunConfig{MaxAttempts: 1}).Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run with nothing of web committed = %q, %v (b: %s)", outcome, err, g.task("run:b").LastError)
	}
	if got := git(t, g.run.Project, "show", g.task("run:b").Commit+":web/b.txt"); got != "done" {
		t.Fatalf("web/b.txt = %q", got)
	}
}

func TestRecoverAndReady(t *testing.T) {
	running := builderTask("a")
	running.Status, running.Assignee = StatusRunning, "builder-1"
	approved := builderTask("b")
	approved.Status = StatusApproved
	f := newFixture(t, []store.RunAgent{lead, builder1}, running, approved)
	// While an approval holds the merge lock, its task is left to it.
	lock, err := TryLockMerge(f.run.IntegrationPath)
	if err != nil || lock == nil {
		t.Fatalf("TryLockMerge = %v, %v", lock, err)
	}
	if skipped, err := f.engine(config.RunConfig{}).recover(); err != nil || !skipped {
		t.Fatalf("recover with the lock held = %v, %v; want b skipped", skipped, err)
	}
	if b := f.task("run:b"); b.Status != StatusApproved {
		t.Fatalf("b = %s, want left to the approval in progress", b.Status)
	}
	lock.Release()
	if _, err := f.engine(config.RunConfig{}).recover(); err != nil {
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

	// Tasks whose file scopes overlap never run at once: not with work still
	// waiting to merge, nor with a task this pass starts.
	scoped := []store.Task{
		{ID: "p", Role: "builder", Status: StatusReview, Files: []string{"src/**"}},
		{ID: "q", Role: "builder", Status: StatusQueued, Files: []string{"src/api/x.go"}},
		{ID: "r", Role: "builder", Status: StatusQueued, Files: []string{"./docs/*.md"}},
		{ID: "s", Role: "builder", Status: StatusQueued, Files: []string{"docs/guide.md"}},
		{ID: "u", Role: "builder", Status: StatusQueued},
	}
	got = Ready(scoped, agents, map[string]bool{}, 10)
	if len(got) != 2 || got[0].task.ID != "r" || got[1].task.ID != "u" {
		t.Fatalf("Ready with overlapping scopes = %+v", got)
	}
}

func TestExplainSaysWhatEachTaskWaitsFor(t *testing.T) {
	agents := []store.RunAgent{lead, builder1, builder2}
	tasks := []store.Task{
		{ID: "m", Role: "builder", Status: StatusMerged},
		{ID: "b", Role: "builder", Status: StatusBlocked, LastError: "attempt limit 3 reached: verification failed:\n$ go test"},
		{ID: "d", Role: "builder", Status: StatusQueued, Deps: []string{"m", "b"}},
		{ID: "t", Role: "tester", Status: StatusQueued, Deps: []string{"b"}},
		{ID: "r", Role: "builder", Status: StatusRunning, Assignee: "builder-1", Files: []string{"src/x.go"}},
		{ID: "o", Role: "builder", Status: StatusQueued, Files: []string{"src/**"}},
		{ID: "h", Role: "builder", Status: StatusInterrupted, Assignee: "builder-1"},
		{ID: "v", Role: "builder", Status: StatusReview},
		{ID: "q", Role: "builder", Status: StatusQueued},
	}
	want := map[string]string{
		"b": "blocked: attempt limit 3 reached: verification failed:",
		"d": "waits for b, which is blocked",
		"t": "no tester worker in this run",
		"o": "waits for r to merge: their files overlap",
		"h": "waits for builder-1, busy with r",
		"v": "waits for your review",
		"q": "ready to run",
	}
	check := func(got map[string]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("Explain = %q", got)
		}
		for id, reason := range want {
			if got[id] != reason {
				t.Errorf("%s: %q, want %q", id, got[id], reason)
			}
		}
	}
	check(Explain(tasks, agents, true))

	tasks = append(tasks, store.Task{ID: "r2", Role: "builder", Status: StatusVerifying, Assignee: "builder-2"})
	want["q"] = "every builder worker is busy"
	check(Explain(tasks, agents, true))

	budget := "the run budget (max_turns or max_tokens) is used up"
	want["q"], want["o"], want["h"] = budget, budget, budget
	check(Explain(tasks, agents, false))
}

func TestParseVerdict(t *testing.T) {
	for text, want := range map[string]verdict{
		"APPROVE\nfine":            verdictApprove,
		"**Reject**: missing test": verdictReject,
		"Looks fine to me":         verdictUndecided,
		"# APPROVE":                verdictApprove,
		"**Verdict: APPROVE**\nRejected alternatives: none.": verdictApprove,
		"Verdict: **REJECT**\nadd a test":                    verdictReject,
		"Intro text\nREJECT":                                 verdictUndecided,
	} {
		if got, _ := parseVerdict(text); got != want {
			t.Errorf("parseVerdict(%q) = %v, want %v", text, got, want)
		}
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
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

// With nothing running and tasks waiting for review, the run waits as it does
// for workers: an approval in this process wakes it at once, with polling
// off, and a stop ends it as stopped.
func TestRunWaitingForReviewWakesOnApproval(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"), builderTask("b", "a"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := f.engine(config.RunConfig{})
	e.opts.WaitForReview = true
	result := make(chan error, 1)
	go func() { _, err := e.Run(ctx); result <- err }()
	waitFor := func(id, status string) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); f.task(id).Status != status; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s never reached %s (is %s)", id, status, f.task(id).Status)
			}
		}
	}
	waitFor("run:a", StatusReview)
	if err := e.Approve(ctx, "run:a"); err != nil {
		t.Fatal(err)
	}
	waitFor("run:b", StatusReview)

	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v", err)
	}
	if run, _, _ := f.state.GetRun(context.Background(), "run"); run.Status != RunStopped {
		t.Fatalf("run status = %q, want %q", run.Status, RunStopped)
	}
}

// The driver polls while a worker runs too: an approval from another process
// starts the task that waited for it without waiting for that worker.
func TestCrossProcessApprovalWhileAWorkerRuns(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, builderTask("slow"), builderTask("b"), builderTask("c", "b"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := f.worker
	f.worker = func(req TurnRequest) (string, error) {
		if planID(req.TaskID) == "slow" {
			<-ctx.Done() // works until the driver stops
		}
		return worker(req)
	}
	driver := f.engine(config.RunConfig{MaxConcurrency: 2})
	driver.opts.WaitForReview, driver.opts.Poll = true, 50*time.Millisecond
	result := make(chan error, 1)
	go func() { _, err := driver.Run(ctx); result <- err }()

	waitFor := func(id, status string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for f.task(id).Status != status {
			if time.Now().After(deadline) {
				t.Fatalf("%s never reached %s (is %s) while slow runs", id, status, f.task(id).Status)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("run:b", StatusReview)
	if err := f.engine(config.RunConfig{}).Approve(ctx, "run:b"); err != nil {
		t.Fatal(err)
	}
	waitFor("run:c", StatusReview)

	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("driver err = %v", err)
	}
}
