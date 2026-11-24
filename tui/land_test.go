package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/store"
)

// /land shows what a completed run changes and the merge it runs; enter in
// that panel, or /land again, merges it into the checkout's branch. A dirty
// checkout or a conflict leaves the branch as it was.
func TestLandMergesACompletedRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, v := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(v, "t")
	}
	for _, v := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(v, "t@t")
	}
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write("README.md", "hello\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")
	run("checkout", "-q", "-b", "switch/r1/integration")
	write("flag.go", "package flag\n")
	run("add", ".")
	run("commit", "-q", "-m", "switch: merge add the flag")
	integration := run("rev-parse", "HEAD")
	run("checkout", "-q", "-")

	m := model{ctx: context.Background(), project: repo,
		run: &store.Run{ID: "r1", Status: execution.RunCompleted, BaseRef: base, IntegrationBranch: "switch/r1/integration"}}
	do := func(cmd tea.Cmd) {
		t.Helper()
		if cmd == nil {
			t.Fatal("no command")
		}
		next, _ := m.Update(cmd())
		m = next.(model)
	}
	slash := func(text string) {
		t.Helper()
		next, cmd := m.handleSlash(text)
		m = next.(model)
		do(cmd)
	}
	enter := func() {
		t.Helper()
		next, cmd := m.Update(keyEnter())
		m = next.(model)
		do(cmd)
	}

	slash("/land")
	for _, want := range []string{"flag.go | 1 +", "git merge --no-ff --no-edit switch/r1/integration", "Nothing is pushed"} {
		if !strings.Contains(m.modal, want) {
			t.Errorf("preview misses %q:\n%s", want, m.modal)
		}
	}

	// esc is a no: the next /land shows the preview again.
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(model)
	slash("/land")
	if m.modalTitle != landTitle || run("rev-parse", "HEAD") != base {
		t.Fatalf("/land after esc did not show the preview again:\n%s", m.modal)
	}

	// Enter in the panel waits for the lead's turn, as /land does.
	busy := m
	busy.busy = true
	if next, cmd := busy.update(keyEnter()); cmd != nil || !strings.Contains(next.(model).toastText(), "busy") {
		t.Fatal("enter in the panel merged while the lead was mid-turn")
	}

	// The panel can open while the user types: that enter is not a yes, and
	// neither is a key that closes it unread, so the next /land shows it again.
	m.input.SetValue("and then")
	next, cmd := m.Update(keyEnter())
	if m = next.(model); cmd != nil || m.modal != "" || m.landing.branch != "" {
		t.Fatalf("enter typed for the input merged the run or left it armed: %q", m.landing.branch)
	}
	m.input.Reset()
	slash("/land")
	next, _ = m.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	if m = next.(model); m.modal != "" || m.landing.branch != "" {
		t.Fatalf("a key that closed the panel left it armed: %q", m.landing.branch)
	}
	m.input.Reset()

	write("README.md", "edited\n")
	slash("/land")
	slash("/land")
	if !strings.Contains(m.modal, "uncommitted changes") || run("rev-parse", "HEAD") != base {
		t.Fatalf("landed into a dirty checkout:\n%s", m.modal)
	}
	run("checkout", "-q", "--", "README.md")

	// A yes lands on the branch the preview named, not on whatever is checked out since.
	slash("/land")
	run("checkout", "-q", "-b", "release")
	slash("/land")
	if !strings.Contains(m.modal, "on release now") || run("rev-parse", "HEAD") != base {
		t.Fatalf("landed on a branch the preview did not name:\n%s", m.modal)
	}
	run("checkout", "-q", "-")

	// A yes merges what the preview showed, not what the branch holds since.
	slash("/land")
	run("checkout", "-q", "switch/r1/integration")
	write("later.go", "package later\n")
	run("add", ".")
	run("commit", "-q", "-m", "unchecked")
	run("checkout", "-q", "-")
	slash("/land")
	if !strings.Contains(m.modal, "moved since /land showed it") || run("rev-parse", "HEAD") != base {
		t.Fatalf("landed a commit the preview did not show:\n%s", m.modal)
	}
	run("branch", "-q", "-f", "switch/r1/integration", integration)

	write("flag.go", "package other\n")
	run("add", ".")
	run("commit", "-q", "-m", "a clashing flag.go")
	clash := run("rev-parse", "HEAD")
	slash("/land")
	enter()
	if !strings.Contains(m.modal, "conflicts in flag.go") || run("rev-parse", "HEAD") != clash || run("status", "--porcelain") != "" {
		t.Fatalf("a conflict left the checkout changed:\n%s\n%s", m.modal, run("status"))
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "MERGE_HEAD")); err == nil {
		t.Fatal("the conflicting merge was left half-done")
	}

	run("reset", "-q", "--hard", base)
	slash("/land")
	stale := landPreviewCmd(repo, *m.run)() // a second /land's preview, still on its way
	slash("/land")
	if parents := run("rev-list", "--parents", "-n", "1", "HEAD"); !strings.HasSuffix(parents, " "+base+" "+integration) {
		t.Fatalf("HEAD is not a merge of the run into its branch: %s\n%s", parents, m.modal)
	}
	if !strings.Contains(m.modal, "switch __control prune") || m.landing.branch != "" {
		t.Fatalf("after landing: landing %q, modal:\n%s", m.landing.branch, m.modal)
	}

	// git merges a landed run again without a commit and without an error.
	landed := run("rev-parse", "HEAD")
	slash("/land")
	if !strings.Contains(m.modal, "already in") || m.modalTitle == landTitle {
		t.Fatalf("/land on a landed run:\n%s", m.modal)
	}
	next, _ = m.Update(stale)
	m = next.(model)
	enter()
	if !strings.Contains(m.modal, "already in") || run("rev-parse", "HEAD") != landed {
		t.Fatalf("a stale preview's yes:\n%s", m.modal)
	}
}

// Only a completed run lands. Approvals without /run can merge every task
// and leave the run's own checks, which complete it, to /run.
func TestLandRefusesAnUnfinishedRun(t *testing.T) {
	m := model{ctx: context.Background(), run: &store.Run{ID: "r1", Status: execution.RunWaiting},
		tasks: []store.Task{{ID: "r1:flag", Status: execution.StatusReview}}}
	next, cmd := m.handleSlash("/land")
	if got := next.(model); cmd != nil || !strings.Contains(got.toastText(), "/why") {
		t.Fatalf("/land on an unfinished run: toast %q", got.toastText())
	}
	m.run.Status, m.tasks[0].Status = execution.RunStopped, execution.StatusMerged
	next, cmd = m.handleSlash("/land")
	if got := next.(model); cmd != nil || !strings.Contains(got.toastText(), "/run") {
		t.Fatalf("/land on a stopped run with every task merged: toast %q", got.toastText())
	}
}
