package execution

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/urizennnn/switch/internal/store"
	"github.com/urizennnn/switch/internal/verify"
	"github.com/urizennnn/switch/internal/worktree"
)

// Fixable says why the user cannot finish task by hand in its worktree now,
// or nil when they can: no agent works on it, and it is not merged.
func Fixable(task store.Task) error {
	switch {
	case !slices.Contains([]string{StatusBlocked, StatusReview, StatusQueued, StatusInterrupted}, task.Status):
		return fmt.Errorf("%s is %s; only a blocked, queued or interrupted task, or one waiting for review, can be fixed by hand", planID(task.ID), task.Status)
	case task.Worktree == "":
		return fmt.Errorf("%s has no worktree yet", planID(task.ID))
	}
	return nil
}

// Resubmit takes back a task the user finished by hand in its worktree, as
// /fix does: it commits what they changed, with Switch's own commit settings,
// runs the task's checks again, and puts the task back in review, or blocks
// it with what failed. It costs no attempt, and a worktree holding nothing
// new changes nothing.
func (e *Engine) Resubmit(ctx context.Context, taskID string) error {
	defer e.signal()
	// No approval merges the task meanwhile, and the checks take turns with
	// the integration checks, so they can share their slot.
	lock, err := lockMerge(ctx, e.run.IntegrationPath)
	if err != nil {
		return err
	}
	defer lock.Release()

	task, ok, err := e.opts.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if !ok || task.RunID != e.run.ID {
		return fmt.Errorf("task %s not found", taskID)
	}
	if err := Fixable(task); err != nil {
		return err
	}
	// The markers Switch's merge of the integration branch left are no fix,
	// yet committing would conclude that merge with them in.
	unresolved, err := worktree.Unresolved(ctx, task.Worktree)
	if err != nil {
		return err
	}
	if len(unresolved) > 0 {
		return fmt.Errorf("resolve the conflict markers in %s first; %s is still %s", strings.Join(unresolved, ", "), planID(task.ID), task.Status)
	}
	commit, changed, err := worktree.Commit(ctx, task.Worktree, commitMessage(task, store.RunAgent{Name: "you", Driver: "by hand"}))
	if err != nil {
		return fmt.Errorf("commit your fix: %w", err)
	}
	diff, err := e.diff(ctx, store.Task{Worktree: task.Worktree, Commit: commit})
	if err != nil {
		return err
	}
	// A task blocked before Switch committed anything has no commit to compare
	// with, so a worktree with no change to merge holds nothing new either.
	if !changed && (commit == task.Commit || task.Commit == "" && diff == "") {
		return fmt.Errorf("nothing changed in the worktree of %s; it is still %s", planID(task.ID), task.Status)
	}
	from := task.Status
	task.Status = StatusVerifying
	e.mu.Lock()
	err = e.claim(ctx, task, from, "user", "checking your fix")
	e.mu.Unlock()
	if err != nil {
		return err
	}

	results, passed := verify.Run(ctx, e.dir(task.Worktree), task.Verify, e.verifyTimeout(), func() error { return worktree.Clean(ctx, task.Worktree) }, e.env(task.ID, 0)...)
	e.recordEvidence(task, "task", results)
	_ = worktree.Clean(context.WithoutCancel(ctx), task.Worktree)
	checked, message := task, "fixed by you; waiting for approval"
	checked.Commit = commit
	switch {
	case ctx.Err() != nil:
		// It keeps its last commit, so /fix finds the fix new and checks it.
		checked.Commit = task.Commit
		checked.Status, checked.LastError = StatusBlocked, "checking your fix was interrupted; /fix checks it again"
		message = checked.LastError
	case diff == "": // the fix takes the whole change back, so the old commit must not stay approvable
		checked.Status, checked.LastError = StatusBlocked, "your fix leaves nothing to merge"
		message = checked.LastError
	case !passed:
		checked.Status, checked.LastError = StatusBlocked, "your fix failed verification:\n"+verify.Summary(results)
		message = checked.LastError
	default:
		checked.Status, checked.LastError, checked.Feedback = StatusReview, "", ""
		checked.Review = "Finished by hand; no agent reviewed the change."
	}
	save, cancel := saveContext()
	defer cancel()
	e.mu.Lock()
	err = e.claim(save, checked, StatusVerifying, "user", message)
	e.mu.Unlock()
	if err == nil && checked.Status == StatusBlocked {
		err = errors.New(checked.LastError)
	}
	return err
}
