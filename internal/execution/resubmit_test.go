package execution

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/store"
)

// The user finishes a blocked task by hand: Resubmit commits what they
// changed as Switch commits, checks it, and puts the task back in review, or
// blocks it with what failed, without spending an attempt. A worktree with
// nothing new changes nothing.
func TestResubmitChecksAFixByHand(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	f.worker = func(TurnRequest) (string, error) { return "tried", nil } // never makes a.txt
	ctx := context.Background()
	e := f.engine(config.RunConfig{MaxAttempts: 1})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunBlocked {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	blocked := f.task("run:a")
	events, _ := f.state.Events(ctx, "run", 1000)
	if err := e.Resubmit(ctx, "run:a"); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("Resubmit with nothing changed = %v", err)
	}
	if after, _ := f.state.Events(ctx, "run", 1000); !reflect.DeepEqual(f.task("run:a"), blocked) || len(after) != len(events) {
		t.Fatalf("nothing changed, yet the task or its log did:\n%+v\n%+v", blocked, f.task("run:a"))
	}

	writeFile(t, blocked.Worktree, "b.txt", "not the fix\n")
	if err := e.Resubmit(ctx, "run:a"); err == nil || !strings.Contains(err.Error(), "test -f a.txt") {
		t.Fatalf("Resubmit of a failing fix = %v, want the failed check", err)
	}
	failed := f.task("run:a")
	if failed.Status != StatusBlocked || failed.Attempts != 1 || !strings.HasPrefix(failed.LastError, "your fix failed verification") || failed.Commit == blocked.Commit {
		t.Fatalf("after a failing fix a = %s on attempt %d, commit changed %v: %s", failed.Status, failed.Attempts, failed.Commit != blocked.Commit, failed.LastError)
	}

	// A repository hook that would refuse the commit does not run: Switch
	// commits with its own settings.
	hook := filepath.Join(f.run.Project, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, blocked.Worktree, "a.txt", "fixed by hand\n")
	if err := e.Resubmit(ctx, "run:a"); err != nil {
		t.Fatalf("Resubmit of a fix that passes: %v", err)
	}
	a := f.task("run:a")
	if a.Status != StatusReview || a.Attempts != 1 || a.LastError != "" || a.Feedback != "" || !strings.Contains(a.Review, "by hand") {
		t.Fatalf("after the fix a = %+v, want review on attempt 1 with a note", a)
	}
	if head := git(t, a.Worktree, "rev-parse", "HEAD"); a.Commit != head {
		t.Fatalf("a's commit %s is not the fix %s", a.Commit, head)
	}
	if log := git(t, a.Worktree, "log", "-1", "--format=%an %B"); !strings.HasPrefix(log, "Switch ") || !strings.Contains(log, "by you") {
		t.Fatalf("the fix was committed as %q", log)
	}
	if err := e.Resubmit(ctx, "run:a"); err == nil || !strings.Contains(err.Error(), "nothing changed") || f.task("run:a").Status != StatusReview {
		t.Fatalf("Resubmit with nothing new = %v", err)
	}

	// A fix that takes the whole change back leaves nothing to approve, and
	// the task keeps the commit its worktree holds, whoever committed it.
	for _, own := range []bool{false, true} {
		git(t, a.Worktree, "rm", "-q", "--ignore-unmatch", "a.txt", "b.txt")
		if own { // the user committed it in the shell
			git(t, a.Worktree, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--no-verify", "-m", "undo")
		}
		if err := e.Resubmit(ctx, "run:a"); err == nil || !strings.Contains(err.Error(), "nothing to merge") {
			t.Fatalf("Resubmit of a fix that undoes the change (committed by the user %v) = %v", own, err)
		}
		if undone := f.task("run:a"); undone.Status != StatusBlocked || undone.Commit != git(t, a.Worktree, "rev-parse", "HEAD") {
			t.Fatalf("after undoing the change a = %s with commit %s", undone.Status, undone.Commit)
		}
		writeFile(t, a.Worktree, "a.txt", "fixed by hand\n")
		if err := e.Resubmit(ctx, "run:a"); err != nil {
			t.Fatalf("Resubmit of the fix made again: %v", err)
		}
	}
	if err := e.Approve(ctx, "run:a"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got := git(t, f.run.Project, "show", f.run.IntegrationBranch+":a.txt"); got != "fixed by hand" {
		t.Fatalf("integration a.txt = %q", got)
	}
}

// A task the user rejected can be fixed by hand, and so can one blocked
// before Switch committed anything; one an agent works on cannot. While the
// fix is checked, the task is Resubmit's alone.
func TestResubmitTakesTasksNoAgentWorksOn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	e := f.engine(config.RunConfig{Setup: []string{"exit 1"}})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunBlocked {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if err := e.Resubmit(ctx, "run:a"); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("Resubmit of a worktree with no change at all = %v", err)
	}
	for _, status := range []string{StatusRunning, StatusMerged} {
		a := f.task("run:a")
		a.Status = status
		if err := f.state.SaveTask(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := e.Resubmit(ctx, "run:a"); err == nil || !strings.Contains(err.Error(), status) {
			t.Fatalf("Resubmit of a %s task = %v", status, err)
		}
	}

	stall := filepath.Join(t.TempDir(), "stall")
	task := builderTask("a")
	task.Verify = append(task.Verify, "while test -f "+stall+"; do sleep 0.05; done")
	g := newFixture(t, []store.RunAgent{lead, builder1}, task)
	e = g.engine(config.RunConfig{})
	e.opts.Config.LeadReview = false // so nothing but the fix writes a review note
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if err := e.Reject(ctx, "run:a", "say it differently"); err != nil {
		t.Fatal(err)
	}
	dir := g.task("run:a").Worktree
	writeFile(t, dir, "a.txt", "said differently\n")
	if err := e.Resubmit(ctx, "run:a"); err != nil {
		t.Fatalf("Resubmit of a rejected task: %v", err)
	}
	if a := g.task("run:a"); a.Status != StatusReview || a.Attempts != 1 || a.Feedback != "" {
		t.Fatalf("after the fix a = %s on attempt %d, feedback %q", a.Status, a.Attempts, a.Feedback)
	}

	writeFile(t, dir, "a.txt", "said better\n")
	writeFile(t, filepath.Dir(stall), "stall", "")
	reviewed := g.task("run:a").Commit
	checking, stop := context.WithCancel(ctx)
	defer stop()
	resubmitted := make(chan error, 1)
	go func() { resubmitted <- e.Resubmit(checking, "run:a") }()
	for g.task("run:a").Status != StatusVerifying {
		select {
		case err := <-resubmitted:
			t.Fatalf("Resubmit ended before its check: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := e.Reject(ctx, "run:a", "too late"); err == nil {
		t.Fatal("a task was rejected while its fix was checked")
	}
	// A run that starts meanwhile leaves the fix to its check and waits for it.
	if _, err := e.recover(); err != nil || g.task("run:a").Status != StatusVerifying {
		t.Fatalf("recover during the check = %v; a is %s", err, g.task("run:a").Status)
	}
	if outcome, _ := e.outcome(true); outcome != RunWaiting {
		t.Fatalf("outcome during the check = %s, want %s", outcome, RunWaiting)
	}
	// An interrupted check keeps the last checked commit, so the next /fix
	// finds the fix new and checks it.
	stop()
	if err := <-resubmitted; err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("Resubmit of an interrupted check = %v", err)
	}
	if a := g.task("run:a"); a.Status != StatusBlocked || a.Commit != reviewed {
		t.Fatalf("after the interrupted check a = %s with commit %s, want blocked with %s", a.Status, a.Commit, reviewed)
	}
	if err := os.Remove(stall); err != nil {
		t.Fatal(err)
	}
	if err := e.Resubmit(ctx, "run:a"); err != nil || g.task("run:a").Status != StatusReview || g.task("run:a").Commit == reviewed {
		t.Fatalf("Resubmit after the interruption = %v; a is %s", err, g.task("run:a").Status)
	}

	// The note is the fix's alone: an agent's next attempt does not keep it.
	if err := e.Reject(ctx, "run:a", "let an agent do it"); err != nil {
		t.Fatal(err)
	}
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run after the reject = %q, %v", outcome, err)
	}
	if a := g.task("run:a"); a.Status != StatusReview || a.Review != "" {
		t.Fatalf("after an agent's attempt a = %s with review note %q", a.Status, a.Review)
	}
}

// Switch's own merge of the integration branch is no fix: /fix refuses while
// its conflict markers remain, and takes the conflict once the user resolved
// it, staged or not.
func TestResubmitWaitsForConflictMarkersToGo(t *testing.T) {
	a, b := builderTask("a"), builderTask("b")
	a.Verify, b.Verify = nil, nil
	f := newFixture(t, []store.RunAgent{lead, builder1, builder2}, a, b)
	f.worker = func(req TurnRequest) (string, error) {
		writeFile(t, req.Cwd, "README.md", planID(req.TaskID)+"\n")
		return "edited", nil
	}
	ctx := context.Background()
	e := f.engine(config.RunConfig{})
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if err := e.Approve(ctx, "run:a"); err != nil {
		t.Fatal(err)
	}
	if err := e.Approve(ctx, "run:b"); err == nil {
		t.Fatal("b merged over a's change to README.md")
	}
	blocked := f.task("run:b")
	events, _ := f.state.Events(ctx, "run", 1000)
	if err := e.Resubmit(ctx, "run:b"); err == nil || !strings.Contains(err.Error(), "conflict markers in README.md") {
		t.Fatalf("Resubmit with the markers left in = %v", err)
	}
	if after, _ := f.state.Events(ctx, "run", 1000); !reflect.DeepEqual(f.task("run:b"), blocked) || len(after) != len(events) ||
		git(t, blocked.Worktree, "rev-parse", "HEAD") != blocked.Commit {
		t.Fatalf("the markers were taken for a fix:\n%+v\n%+v", blocked, f.task("run:b"))
	}
	writeFile(t, blocked.Worktree, "README.md", "a\nb\n")
	if err := e.Resubmit(ctx, "run:b"); err != nil {
		t.Fatalf("Resubmit of the resolved conflict: %v", err)
	}
	if err := e.Approve(ctx, "run:b"); err != nil {
		t.Fatalf("Approve(b) after the fix: %v", err)
	}
	if got := git(t, f.run.Project, "show", f.run.IntegrationBranch+":README.md"); got != "a\nb" {
		t.Fatalf("integration README.md = %q", got)
	}
}
