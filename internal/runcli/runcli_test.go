//go:build !windows

package runcli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/store"
	"github.com/urizennnn/switch/internal/worktree"
)

// TestMain lets this test binary stand in for another Switch process that
// drives a run, so Stop has an owner to signal, and for the
// `switch __control run` that Detach starts.
func TestMain(m *testing.M) {
	if kind := os.Getenv("SWITCH_TEST_OWNER"); kind != "" {
		os.Exit(driveRun(kind))
	}
	if len(os.Args) > 2 && os.Args[1] == "__control" {
		os.Exit(Run(os.Args[2], os.Args[3:]))
	}
	os.Exit(m.Run())
}

// driveRun holds the run lock as a TUI or headless owner until SIGINT.
func driveRun(kind string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	lock, err := execution.LockRun(os.Getenv("SWITCH_TEST_STATE"), "run", kind == "headless")
	if err != nil {
		fmt.Println(err)
		return 1
	}
	defer lock.Release()
	fmt.Println("ready")
	<-ctx.Done()
	return 0
}

func TestStopInterruptsOnlyAHeadlessOwner(t *testing.T) {
	stateDir := t.TempDir()
	start := func(kind string) *exec.Cmd {
		t.Helper()
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "SWITCH_TEST_OWNER="+kind, "SWITCH_TEST_STATE="+stateDir)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if line, _ := bufio.NewReader(out).ReadString('\n'); line != "ready\n" {
			t.Fatalf("%s owner: %q", kind, line)
		}
		return cmd
	}

	tui := start("tui")
	err := Stop(stateDir, "run")
	if err == nil || !strings.Contains(err.Error(), "/stop") {
		t.Fatalf("Stop with a TUI owner = %v, want a refusal that points to /stop", err)
	}
	if pid, ok := execution.RunOwner(stateDir, "run"); !ok || pid != tui.Process.Pid {
		t.Fatalf("TUI owner after Stop = %d, %v; want it still driving", pid, ok)
	}
	_ = tui.Process.Kill()
	_ = tui.Wait()

	headless := start("headless")
	if err := Stop(stateDir, "run"); err != nil {
		t.Fatalf("Stop with a headless owner: %v", err)
	}
	if err := headless.Wait(); err != nil {
		t.Fatalf("headless owner did not stop cleanly: %v", err)
	}
}

func TestRunExitCodeJSONAndSummary(t *testing.T) {
	for _, tc := range []struct {
		task, outcome string
		code          int
	}{
		{execution.StatusMerged, execution.RunCompleted, 0},
		{execution.StatusReview, execution.RunWaiting, 3},
		{execution.StatusBlocked, execution.RunBlocked, 4},
		{execution.StatusQueued, execution.RunBudget, 5},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			p := newProject(t)
			// A task that is already decided, or queued with no budget left, so
			// the run ends without starting an agent.
			id := p.run(t, "1b4e28ba-2fa1-11d2-883f-0016d3cca427", "planned", tc.task)
			if tc.task == execution.StatusQueued {
				if err := os.MkdirAll(config.GlobalDir(), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(config.GlobalDir(), "config.toml"), []byte("[run]\nmax_turns = 1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := p.state.AddEvent(context.Background(), store.Event{RunID: id, Kind: "turn"}); err != nil {
					t.Fatal(err)
				}
			}

			code, out := capture(t, func() int { return Run("run", []string{"--cwd", p.dir, "--run", id[:12], "--json"}) })
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d", code, tc.code)
			}
			var got struct {
				Run, Status string
				Owner       int
				Tasks       []struct{ ID, Status string }
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, out)
			}
			if got.Run != id || got.Status != tc.outcome || got.Owner != 0 || len(got.Tasks) != 1 || got.Tasks[0].ID != "a" || got.Tasks[0].Status != tc.task {
				t.Fatalf("run --json = %+v", got)
			}

			summary := filepath.Join(execution.RunDir(config.StateDir(), id), "summary.md")
			if _, err := os.Stat(summary); (err == nil) != (tc.outcome == execution.RunCompleted) {
				t.Fatalf("summary.md after %s: %v", tc.outcome, err)
			}

			for _, command := range []string{"status", "tasks"} {
				code, out := capture(t, func() int { return Run(command, []string{"--cwd", p.dir, "--json"}) })
				if code != 0 || !strings.Contains(out, `"status": "`+tc.outcome+`"`) {
					t.Fatalf("%s --json = %d\n%s", command, code, out)
				}
			}
		})
	}
}

func TestStoppedRunExits130WithJSON(t *testing.T) {
	p := newProject(t)
	// A task waiting for review keeps `run --wait` going until it is stopped.
	id := p.run(t, "run", "planned", execution.StatusReview)
	cmd := exec.Command(os.Args[0], "__control", "run", "--cwd", p.dir, "--wait", "--json")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if pid, ok := execution.RunOwner(config.StateDir(), id); ok && pid == cmd.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run --wait never took the run lock\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
		}
	}
	if err := Stop(config.StateDir(), id); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	var got struct{ Status string }
	if err := json.Unmarshal([]byte(stdout.String()), &got); cmd.ProcessState.ExitCode() != 130 || err != nil || got.Status != execution.RunStopped {
		t.Fatalf("stopped run: exit %d, %v\n%s", cmd.ProcessState.ExitCode(), err, stdout.String())
	}
}

func TestRunDetachJSON(t *testing.T) {
	p := newProject(t)
	id := p.run(t, "run", "planned", execution.StatusReview)
	stateDir := config.StateDir()
	defer func() { _ = Stop(stateDir, id) }()

	code, out := capture(t, func() int { return Run("run", []string{"--cwd", p.dir, "--detach", "--json"}) })
	var got struct {
		Run   string
		Owner int
	}
	if err := json.Unmarshal([]byte(out), &got); code != 0 || err != nil || got.Run != id {
		t.Fatalf("run --detach --json = %d, %v\n%s", code, err, out)
	}
	if pid, ok := execution.RunOwner(stateDir, id); !ok || pid != got.Owner || pid == os.Getpid() {
		t.Fatalf("owner = %d, run is driven by %d, %v", got.Owner, pid, ok)
	}
}

func TestSummaryIsAPRDescription(t *testing.T) {
	p := newProject(t)
	state, ctx := p.state, context.Background()
	id := p.run(t, "run", execution.RunCompleted, execution.StatusMerged)
	task, _, _ := state.GetTask(ctx, id+":a")
	task.Summary, task.Review = "Added a.txt.", "APPROVE\nsmall and tested"
	if err := state.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, v := range []store.Verification{
		{TaskID: task.ID, Attempt: 1, Scope: "task", Command: "go test ./...", ExitCode: 1},
		{TaskID: task.ID, Attempt: 2, Scope: "task", Command: "go test ./...", ExitCode: 0},
		{TaskID: task.ID, Attempt: 2, Scope: "integration", Command: "go test ./...", ExitCode: 0},
	} {
		if err := state.AddVerification(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.AddUsage(ctx, store.Usage{RunID: id, InputTokens: 1000, OutputTokens: 200, TotalTokens: 1200}); err != nil {
		t.Fatal(err)
	}

	text, err := Summary(ctx, state, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ship a.txt", "switch/run/integration", "### task a", "Added a.txt.", "> small and tested",
		"`go test ./...` (task): exit 0", "`go test ./...` (integration): exit 0", "1200 tokens"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "exit 1") {
		t.Errorf("summary shows a superseded check:\n%s", text)
	}
}

func TestApprovingTheLastTaskWritesTheSummary(t *testing.T) {
	p := newProject(t)
	ctx := context.Background()
	id := p.run(t, "run", execution.RunWaiting, execution.StatusReview)
	for _, name := range []string{"a", "b"} {
		wt, err := p.manager.Create(ctx, id, "task-"+name, "switch/run/integration")
		if err != nil {
			t.Fatal(err)
		}
		commitIn(t, wt.Path, name+".txt")
		head, err := worktree.Head(ctx, wt.Path) // approval merges the reviewed commit
		if err != nil {
			t.Fatal(err)
		}
		task := store.Task{ID: id + ":" + name, RunID: id, Title: "task " + name, Role: "builder", Status: execution.StatusReview, Worktree: wt.Path, Branch: wt.Branch, Commit: head}
		if err := p.state.SaveTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}

	summary := filepath.Join(execution.RunDir(config.StateDir(), id), "summary.md")
	for _, name := range []string{"a", "b"} {
		code, out := capture(t, func() int { return Run("approve", []string{"--cwd", p.dir, name}) })
		_, err := os.Stat(summary)
		if last := name == "b"; code != 0 || (err == nil) != last || strings.Contains(out, "PR description: "+summary) != last {
			t.Fatalf("approve %s = %d, summary.md: %v\n%s", name, code, err, out)
		}
	}
}

func TestPruneRemovesFinishedRunsAndKeepsUnmergedWork(t *testing.T) {
	p := newProject(t)
	repo, manager, ctx := p.dir, p.manager, context.Background()

	// old: completed, and its integration branch is merged into main.
	old := p.run(t, "old", execution.RunCompleted, execution.StatusMerged)
	commitIn(t, filepath.Join(manager.Root(old), "integration"), "old.txt")
	gitIn(t, repo, "merge", "--ff-only", "switch/old/integration")
	// replaced: stopped, then replaced by newer plans; task a merged, task b not.
	replaced := p.run(t, "replaced", execution.RunStopped, execution.StatusMerged)
	integration := filepath.Join(manager.Root(replaced), "integration")
	for _, name := range []string{"task-a", "task-b"} {
		wt, err := manager.Create(ctx, replaced, name, "switch/replaced/integration")
		if err != nil {
			t.Fatal(err)
		}
		commitIn(t, wt.Path, name+".txt")
	}
	gitIn(t, integration, "merge", "--no-ff", "-m", "merge a", "switch/replaced/task-a")
	// A folder deleted by hand leaves git a stale worktree that blocks deleting its branch.
	if err := os.RemoveAll(filepath.Join(manager.Root(replaced), "task-a")); err != nil {
		t.Fatal(err)
	}
	// interrupted: stopped while its worker was editing, so the edits exist only in the worktree.
	interrupted := p.run(t, "interrupted", execution.RunStopped, execution.StatusInterrupted)
	edited, err := manager.Create(ctx, interrupted, "task-a", "switch/interrupted/integration")
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(edited.Path, "work.go")
	if err := os.WriteFile(work, []byte("package work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// driven: completed, but a process holds its lock.
	driven := p.run(t, "driven", execution.RunCompleted, execution.StatusMerged)
	lock, err := execution.LockRun(config.StateDir(), driven, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	// current: the newest run, still waiting for review.
	current := p.run(t, "current", execution.RunWaiting, execution.StatusReview)

	code, out := capture(t, func() int { return Run("prune", []string{"--cwd", repo}) })
	if code != 0 {
		t.Fatalf("prune exit code %d\n%s", code, out)
	}
	for _, gone := range []string{manager.Root(old), manager.Root(replaced)} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s still exists after prune\n%s", gone, out)
		}
	}
	for _, kept := range []string{manager.Root(driven), manager.Root(current)} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("prune touched %s: %v\n%s", kept, err, out)
		}
	}
	if _, err := os.Stat(work); err != nil || !strings.Contains(out, "kept worktree "+edited.Path) {
		t.Errorf("prune without --all dropped uncommitted work: %v\n%s", err, out)
	}
	wantBranches := "switch/current/integration switch/driven/integration switch/interrupted/task-a switch/replaced/integration switch/replaced/task-b"
	if got := branches(t, repo); got != wantBranches {
		t.Fatalf("branches after prune = %q, want %q\n%s", got, wantBranches, out)
	}
	if !strings.Contains(out, "switch/replaced/task-b") || !strings.Contains(out, "--all") {
		t.Errorf("prune does not say what it kept and why:\n%s", out)
	}
	if worktrees := gitIn(t, repo, "worktree", "list"); strings.Contains(worktrees, "/replaced/") {
		t.Errorf("git still lists pruned worktrees:\n%s", worktrees)
	}

	if code, out = capture(t, func() int { return Run("prune", []string{"--cwd", repo, "--all"}) }); code != 0 {
		t.Fatalf("prune --all exit code %d\n%s", code, out)
	}
	if got := branches(t, repo); got != "switch/current/integration switch/driven/integration" {
		t.Fatalf("branches after prune --all = %q\n%s", got, out)
	}
	if _, err := os.Stat(manager.Root(interrupted)); !os.IsNotExist(err) {
		t.Errorf("prune --all kept %s\n%s", manager.Root(interrupted), out)
	}
}

// git status only warns about a folder it cannot read, such as a database
// folder a container owns, so it cannot vouch that the worktree is clean.
func TestPruneKeepsAWorktreeGitCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every folder")
	}
	p := newProject(t)
	done := p.run(t, "done", execution.RunCompleted, execution.StatusMerged)
	wt, err := p.manager.Create(context.Background(), done, "task-a", "switch/done/integration")
	if err != nil {
		t.Fatal(err)
	}
	pgdata := filepath.Join(wt.Path, "pgdata")
	if err := os.MkdirAll(filepath.Join(pgdata, "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pgdata, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(pgdata, 0o755) }) // so the temporary folder can be removed

	code, out := capture(t, func() int { return Run("prune", []string{"--cwd", p.dir}) })
	if code != 0 || !strings.Contains(out, "kept worktree "+wt.Path) {
		t.Fatalf("prune exit code %d, want 0 and the worktree kept\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "README.md")); err != nil {
		t.Errorf("prune deleted part of the kept worktree: %v\n%s", err, out)
	}
}

// With status.showUntrackedFiles=no, plain git status hides a worker's new
// files, which exist nowhere else.
func TestPruneKeepsNewFilesGitStatusHides(t *testing.T) {
	p := newProject(t)
	gitIn(t, p.dir, "config", "status.showUntrackedFiles", "no")
	old := p.run(t, "old", execution.RunStopped, execution.StatusInterrupted)
	wt, err := p.manager.Create(context.Background(), old, "task-a", "switch/old/integration")
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(wt.Path, "new.go")
	if err := os.WriteFile(work, []byte("package work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.run(t, "current", execution.RunWaiting, execution.StatusReview)

	code, out := capture(t, func() int { return Run("prune", []string{"--cwd", p.dir}) })
	if _, err := os.Stat(work); code != 0 || err != nil || !strings.Contains(out, "kept worktree "+wt.Path) {
		t.Fatalf("prune = %d, new file: %v\n%s", code, err, out)
	}
}

// Every engine leaves merge.lock beside the integration worktree; prune
// removes it with the run's folder and does not report it as a worktree.
func TestPruneRemovesTheMergeLock(t *testing.T) {
	p := newProject(t)
	done := p.run(t, "done", execution.RunCompleted, execution.StatusMerged)
	if err := os.WriteFile(filepath.Join(p.manager.Root(done), "merge.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := capture(t, func() int { return Run("prune", []string{"--cwd", p.dir}) })
	if _, err := os.Stat(p.manager.Root(done)); code != 0 || !os.IsNotExist(err) || strings.Contains(out, "merge.lock") {
		t.Fatalf("prune = %d, run folder: %v\n%s", code, err, out)
	}
}

// An older run prune retired cannot be approved or resumed: its worktrees
// are gone.
func TestAPrunedRunIsRefused(t *testing.T) {
	p := newProject(t)
	ctx := context.Background()
	old := p.run(t, "old", execution.RunWaiting, execution.StatusReview)
	wt, err := p.manager.Create(ctx, old, "task-a", "switch/old/integration")
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, wt.Path, "a.txt")
	task, _, _ := p.state.GetTask(ctx, old+":a")
	task.Worktree, task.Branch = wt.Path, wt.Branch
	if err := p.state.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	p.run(t, "current", execution.RunWaiting, execution.StatusReview)
	if code, out := capture(t, func() int { return Run("prune", []string{"--cwd", p.dir}) }); code != 0 {
		t.Fatalf("prune = %d\n%s", code, out)
	}
	for _, args := range [][]string{{"approve", "--run", "old", "a"}, {"run", "--run", "old", "--permission", "allow"}} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stderr := os.Stderr
		os.Stderr = w
		code, _ := capture(t, func() int { return Run(args[0], append([]string{"--cwd", p.dir}, args[1:]...)) })
		os.Stderr = stderr
		w.Close()
		errText, _ := io.ReadAll(r)
		if code == 0 || !strings.Contains(string(errText), "was pruned") {
			t.Errorf("%v = %d, want it refused as pruned:\n%s", args, code, errText)
		}
	}
	if task, _, _ := p.state.GetTask(ctx, old+":a"); task.Status != execution.StatusReview {
		t.Errorf("task a = %s, want review", task.Status)
	}
}

// project is a git repository with one commit, with Switch state, config and
// worktrees kept in temporary directories.
type project struct {
	dir     string // as runcli names the project, which may differ from manager.Repo by symlinks
	state   *store.Store
	manager *worktree.Manager
}

func newProject(t *testing.T) project {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main")
	commitIn(t, repo, "README.md")
	state, err := store.Open(config.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	manager, err := worktree.New(context.Background(), repo, config.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	return project{dir: repo, state: state, manager: manager}
}

// run saves a planned run with an integration worktree and one task "a".
func (p project) run(t *testing.T, id, status, taskStatus string) string {
	t.Helper()
	ctx := context.Background()
	base, err := p.manager.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	integration, branch, err := p.manager.Integration(ctx, id, base)
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{ID: id, Project: p.dir, Goal: "ship a.txt", BaseRef: base, IntegrationBranch: branch, IntegrationPath: integration.Path, Status: status}
	if err := p.state.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := p.state.SaveTask(ctx, store.Task{ID: id + ":a", RunID: id, Title: "task a", Role: "builder", Status: taskStatus}); err != nil {
		t.Fatal(err)
	}
	return id
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitIn(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", name)
	gitIn(t, dir, "commit", "-m", "add "+name)
}

func branches(t *testing.T, repo string) string {
	t.Helper()
	return strings.Join(strings.Fields(gitIn(t, repo, "for-each-ref", "--format=%(refname:short)", "refs/heads/switch/")), " ")
}

// capture runs f with stdout going to a pipe and returns what it printed.
func capture(t *testing.T, f func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	code := f()
	os.Stdout = stdout
	w.Close()
	return code, <-done
}

// A run its own checks blocked after the last approval says so; one a
// blocked task holds, or any other, says nothing.
func TestChecksFailedNote(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	run := store.Run{ID: "r1", Project: "/p", Status: execution.RunBlocked}
	if err := state.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	task := store.Task{ID: "r1:a", RunID: "r1", Title: "a", Status: execution.StatusMerged}
	if err := state.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if note := ChecksFailed(ctx, state, run); !strings.Contains(note, "own checks then failed") {
		t.Fatalf("every task merged: %q", note)
	}
	task.Status = execution.StatusBlocked
	if err := state.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if note := ChecksFailed(ctx, state, run); note != "" {
		t.Fatalf("a blocked task: %q", note)
	}
	run.Status = execution.RunCompleted
	if note := ChecksFailed(ctx, state, run); note != "" {
		t.Fatalf("a completed run: %q", note)
	}
}
