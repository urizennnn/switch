package tui

import (
	"context"
	"errors"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/store"
)

func TestDiffIsColored(t *testing.T) {
	diff := " a.go | 2 +-\n\ndiff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n same"
	got := colorDiff(diff)
	if ansi.Strip(got) != diff || !strings.HasSuffix(got, "\n same") {
		t.Fatalf("coloring changed the diff or its context lines: %q", got)
	}
	for line, c := range map[string]color.Color{"+new": green, "-old": red, "@@ -1 +1 @@": teal} {
		if !strings.Contains(got, lipgloss.NewStyle().Foreground(c).Render(line)) {
			t.Errorf("%q is not colored", line)
		}
	}
}

// /approve all merges the reviewed tasks in plan order and stops at the
// first that fails, leaving the rest waiting. A task ID that all starts with,
// or a task named all, is approved alone.
func TestApproveAllStopsAtTheFirstFailure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "base"}, {"branch", "al"}, {"branch", "a"}, {"branch", "c"}, {"branch", "integration"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	if err := state.CreateRun(ctx, store.Run{ID: "r1", Project: repo, IntegrationPath: repo, IntegrationBranch: "integration", Status: execution.RunWaiting}); err != nil {
		t.Fatal(err)
	}
	for _, task := range []store.Task{
		{ID: "r1:al", Title: "al", Status: execution.StatusReview, Branch: "al"},
		{ID: "r1:a", Title: "a", Status: execution.StatusReview, Branch: "a"},
		{ID: "r1:done", Title: "done", Status: execution.StatusMerged},
		{ID: "r1:b", Title: "b", Status: execution.StatusReview, Branch: "gone"}, // cannot merge
		{ID: "r1:c", Title: "c", Status: execution.StatusReview, Branch: "c"},
	} {
		task.RunID, task.Commit = "r1", task.Branch // approval merges the reviewed commit
		if err := state.SaveTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	m := New(Options{Context: ctx, Project: repo, Store: state, Lead: config.Agent{Name: "lead"}})
	defer m.cancel()
	m.refreshDashboard()

	for _, r := range "/approve al" { // typed, so the palette sees each key
		next, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = next.(model)
	}
	next, cmd := m.Update(keyEnter())
	m = next.(model)
	if msg := cmd().(taskActionMsg); msg.title != "approve al" {
		t.Fatalf("/approve al ran %s:\n%s", msg.title, msg.body)
	}
	m.refreshDashboard()

	_, cmd = m.handleSlash("/approve all")
	msg := cmd().(taskActionMsg)
	if msg.title != "approve all stopped at b" || !strings.HasPrefix(msg.body, "merged a\n\nb did not merge: merge:") ||
		!strings.HasSuffix(msg.body, "still waiting for review: c") {
		t.Fatalf("%s:\n%s", msg.title, msg.body)
	}
	for id, want := range map[string]string{"r1:al": execution.StatusMerged, "r1:a": execution.StatusMerged, "r1:b": execution.StatusBlocked, "r1:c": execution.StatusReview} {
		if task, _, _ := state.GetTask(ctx, id); task.Status != want {
			t.Errorf("%s is %s, want %s", id, task.Status, want)
		}
	}
	m.refreshDashboard()
	if _, cmd := m.handleSlash("/approve all"); cmd().(taskActionMsg).title != "approve all" {
		t.Fatal("the task left waiting did not merge")
	}

	if err := state.SaveTask(ctx, store.Task{ID: "r1:all", RunID: "r1", Title: "all", Status: execution.StatusReview, Branch: "gone"}); err != nil {
		t.Fatal(err)
	}
	m.refreshDashboard()
	if _, cmd := m.handleSlash("/approve all"); cmd().(taskActionMsg).title != "approve all failed" { // approveAll says "stopped at"
		t.Fatal("/approve all did not mean the task named all")
	}
}

// Approving the last task completes the run, and the TUI then writes its PR
// description, as a headless approve does.
func TestApprovingTheLastTaskWritesThePRDescription(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "base"}, {"branch", "a"}, {"branch", "integration"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	if err := state.CreateRun(ctx, store.Run{ID: "r1", Project: repo, IntegrationPath: repo, IntegrationBranch: "integration", Status: execution.RunWaiting}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveTask(ctx, store.Task{ID: "r1:a", RunID: "r1", Title: "a", Status: execution.StatusReview, Branch: "a", Commit: "a"}); err != nil {
		t.Fatal(err)
	}
	m := New(Options{Context: ctx, Project: repo, Store: state, Lead: config.Agent{Name: "lead"}})
	defer m.cancel()
	m.refreshDashboard()

	_, cmd := m.handleSlash("/approve a")
	msg := cmd().(taskActionMsg)
	path := filepath.Join(execution.RunDir(config.StateDir(), "r1"), "summary.md")
	if !strings.Contains(msg.body, "PR description: "+shortenHome(path)) {
		t.Fatalf("%s:\n%s", msg.title, msg.body)
	}
	if run, _, _ := state.GetRun(ctx, "r1"); run.Status != execution.RunCompleted {
		t.Errorf("the run is %s, want completed", run.Status)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error(err)
	}
}

// /fix opens the user's shell in a task's worktree. Once it exits, whatever
// its last command returned, Switch checks what changed and puts the task
// back in review; no run starts, and no other fix, while that check runs.
func TestFixChecksWhatTheShellChanged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	repo, worktree := t.TempDir(), filepath.Join(t.TempDir(), "a")
	git(repo, "init", "-q")
	git(repo, "commit", "-q", "--allow-empty", "-m", "base")
	git(repo, "branch", "integration")
	git(repo, "worktree", "add", "-q", "-b", "a", worktree)
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	if err := state.CreateRun(ctx, store.Run{ID: "r1", Project: repo, IntegrationBranch: "integration",
		IntegrationPath: filepath.Join(t.TempDir(), "integration"), Status: execution.RunBlocked}); err != nil {
		t.Fatal(err)
	}
	for _, task := range []store.Task{
		{ID: "r1:a", Title: "a", Status: execution.StatusBlocked, Worktree: worktree, Commit: git(repo, "rev-parse", "HEAD"), Verify: []string{"test -f a.txt"}, Attempts: 3},
		{ID: "r1:b", Title: "b", Status: execution.StatusRunning, Worktree: worktree},
		{ID: "r1:c", Title: "c", Status: execution.StatusQueued, Worktree: worktree},
	} {
		task.RunID = "r1"
		if err := state.SaveTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	m := New(Options{Context: ctx, Project: repo, Store: state, Lead: config.Agent{Name: "lead"}})
	defer m.cancel()
	m.refreshDashboard()
	slash := func(text string) tea.Cmd {
		t.Helper()
		next, cmd := m.handleSlash(text)
		m = next.(model)
		return cmd
	}
	update := func(msg tea.Msg) tea.Cmd {
		t.Helper()
		next, cmd := m.update(msg)
		m = next.(model)
		return cmd
	}

	if slash("/fix b") != nil || !strings.Contains(m.toastText(), "b is running") {
		t.Fatalf("/fix of a task an agent works on: %q", m.toastText())
	}
	m.job = "run"
	if slash("/fix c") != nil || !strings.Contains(m.toastText(), "/stop first") {
		t.Fatalf("/fix of a queued task the run would hand to a worker: %q", m.toastText())
	}
	m.job = ""
	if slash("/fix a") == nil {
		t.Fatal("/fix a opened no shell")
	}
	t.Setenv("SHELL", "")
	if shell := fixShell(worktree, "a"); shell.Args[3] != "/bin/sh" || shell.Dir != worktree {
		t.Fatalf("the shell is %v in %s", shell.Args, shell.Dir)
	}
	t.Setenv("SHELL", "/bin/echo")
	if out, err := fixShell(worktree, "a").Output(); err != nil || !strings.HasPrefix(string(out), "Fixing a in its worktree.") {
		t.Fatalf("the shell started with %q, %v", out, err)
	}

	if update(fixShellMsg{taskID: "r1:a", err: errors.New("chdir: no such file or directory")}) != nil || m.fixing != "" ||
		!strings.Contains(m.toastText(), "could not open your shell") {
		t.Fatalf("a shell that did not open was checked: %q", m.toastText())
	}
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failed := exec.Command("sh", "-c", "exit 3").Run() // the user's last command in the shell
	check := update(fixShellMsg{taskID: "r1:a", err: failed})
	if check == nil || m.fixing != "r1:a" {
		t.Fatalf("the fix is not checked (fixing %q)", m.fixing)
	}
	if slash("/run") != nil || !strings.Contains(m.toastText(), "/run waits for its check") {
		t.Fatalf("a run started while the fix was checked: %q", m.toastText())
	}
	if slash("/fix a") != nil || !strings.Contains(m.toastText(), "wait for its check") {
		t.Fatalf("another fix started while the fix was checked: %q", m.toastText())
	}
	update(check())
	if task, _, _ := state.GetTask(ctx, "r1:a"); m.fixing != "" || m.modalTitle != "fix a" || task.Status != execution.StatusReview || task.Attempts != 3 {
		t.Fatalf("after the check: fixing %q, %s: %s; a is %s on attempt %d", m.fixing, m.modalTitle, m.modal, task.Status, task.Attempts)
	}

	update(update(fixShellMsg{taskID: "r1:a"})())
	if m.modalTitle != "fix a failed" || !strings.Contains(m.modal, "nothing changed") {
		t.Fatalf("a shell that changed nothing: %s: %s", m.modalTitle, m.modal)
	}

	// A tick handled before the shell's end can find the store unreadable.
	state.Close()
	update(dashboardTickMsg{})
	if m.run != nil {
		t.Fatal("the run survived a failed read")
	}
	update(update(fixShellMsg{taskID: "r1:a"})())
	if m.fixing != "" || !strings.Contains(m.modal, "could not be read") {
		t.Fatalf("the check without a run: fixing %q, %s: %s", m.fixing, m.modalTitle, m.modal)
	}
}
