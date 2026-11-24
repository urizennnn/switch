package execution

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
	"github.com/urizennnn/switch/internal/verify"
	"github.com/urizennnn/switch/internal/worktree"
)

// process takes one dispatched task through work, verification, commit, and
// lead review. It ends at review (waiting for the user), back in the queue
// with feedback, blocked, or interrupted. turns, the run's context for agent
// turns, also ends when the run's budget runs out.
func (e *Engine) process(ctx, turns context.Context, task store.Task, agent store.RunAgent, slot int) {
	env := e.env(task.ID, slot)
	// The first attempt sets up the worktree: a new one, or one /retry starts
	// over in, after the user fixed what failed. A conflict with the
	// integration branch left for the worker to resolve, as an approval that
	// conflicted leaves it, would fail setup on its markers: the worker
	// resolves it first, in the worktree an earlier attempt set up, and the
	// integration checks set up the resolved tree.
	if task.Attempts == 1 {
		if unresolved, _ := worktree.Unresolved(ctx, task.Worktree); len(unresolved) == 0 && !e.setUp(ctx, task, agent, env) {
			return
		}
	}
	stopReason, err := e.work(turns, &task, agent, env)
	// The worker handles its subagents; whatever it left running must not
	// keep editing the worktree while it is verified and committed.
	if n, _ := e.Stop(agent.Name, task.ID, ""); n > 0 {
		e.update(task, agent.Name, fmt.Sprintf("stopped %d subagents still running when the turn ended", n))
	}
	// Take every signal of this turn whatever ends it, so none is left to
	// decide the task's next attempt.
	e.stopMu.Lock()
	stopped := e.stopping[task.ID]
	delete(e.stopping, task.ID)
	e.stopMu.Unlock()
	h, handedOff := e.takeHandoff(task.ID)
	block, blocked := "", false
	if e.opts.Control != nil {
		block, blocked = e.opts.Control.TakeBlock(task.ID)
	}
	// A turn the budget cut short resumes later; one that finished first counts.
	if e.interrupted(ctx, task, agent) || err != nil && e.interrupted(turns, task, agent) {
		return
	}
	if stopped {
		e.finish(task, agent, StatusBlocked, "stopped by you; /retry runs it again")
		return
	}
	if handedOff {
		e.handOff(task, agent, h)
		return
	}
	// A block the worker reported wins over how its turn ended, such as a
	// rate limit on the request after it.
	if blocked {
		e.finish(task, agent, StatusBlocked, "worker blocked: "+block)
		return
	}
	if e.limitHandoff(task, agent, stopReason, err) {
		return
	}
	if err != nil {
		e.retry(task, agent, "worker error: "+err.Error())
		return
	}

	// Committing would conclude the integration branch's merge with the
	// markers in it, and nothing after would notice them.
	if unresolved, err := worktree.Unresolved(ctx, task.Worktree); err == nil && len(unresolved) > 0 {
		e.retry(task, agent, "conflict markers are still in "+strings.Join(unresolved, ", ")+": resolve them, keeping both changes; Switch commits the result.")
		return
	}
	task.Status = StatusVerifying
	e.update(task, agent.Name, "")
	// Commit first, so the checks run on exactly what is reviewed and merged,
	// and what they write is cleaned up instead of committed.
	commit, _, err := worktree.Commit(ctx, task.Worktree, commitMessage(task, agent))
	if e.interrupted(ctx, task, agent) {
		return
	}
	if err != nil {
		e.finish(task, agent, StatusBlocked, "commit: "+err.Error())
		return
	}
	task.Commit = commit
	// A rerun starts again from the commit, not from what the failure wrote.
	results, passed := verify.Run(ctx, e.dir(task.Worktree), task.Verify, e.verifyTimeout(), func() error { return worktree.Clean(ctx, task.Worktree) }, env...)
	e.recordEvidence(task, "task", results)
	_ = worktree.Clean(context.WithoutCancel(ctx), task.Worktree)
	if e.interrupted(ctx, task, agent) {
		return
	}
	if !passed {
		e.retry(task, agent, "verification failed:\n"+verify.Summary(results))
		return
	}
	if diff, err := e.diff(ctx, task); err == nil && diff == "" {
		e.retry(task, agent, "no changes: the worktree has nothing to merge. Make the change; if none is needed, or your edits were refused, block the task and say why.")
		return
	}
	if conflict, _ := worktree.Conflicts(ctx, task.Worktree, e.run.IntegrationBranch, task.Commit); conflict {
		e.retry(task, agent, e.mergeIntegration(ctx, task))
		return
	}
	task.Feedback, task.Review = "", "" // a note on an earlier commit, such as a fix by hand, is not about this one

	if e.opts.Config.LeadReview {
		task.Status = StatusReviewing
		e.update(task, agent.Name, "")
		verdict, notes := e.leadReview(ctx, task, agent, verify.Summary(results))
		if e.interrupted(ctx, task, agent) {
			return
		}
		task.Review = notes
		if verdict == verdictReject {
			e.retry(task, agent, "lead review rejected the change:\n"+notes)
			return
		}
	}
	task.Status = StatusReview
	e.update(task, agent.Name, "waiting for approval")
}

// setUp runs [run] setup in the task's worktree, where StopWorker stops it
// as it would the worker's turn. It reports whether the task goes on to work.
func (e *Engine) setUp(ctx context.Context, task store.Task, agent store.RunAgent, env []string) bool {
	setupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.stopMu.Lock()
	e.turns[agent.Name] = cancel
	e.stopMu.Unlock()
	results, ok := verify.Run(setupCtx, e.dir(task.Worktree), e.opts.Config.Setup, e.verifyTimeout(), nil, env...)
	e.stopMu.Lock()
	delete(e.turns, agent.Name)
	stopped := e.stopping[task.ID]
	delete(e.stopping, task.ID)
	e.stopMu.Unlock()
	e.noteFlaky(task.ID, agent.Name, "setup", results)
	switch {
	case e.interrupted(ctx, task, agent):
	case stopped:
		e.finish(task, agent, StatusBlocked, "stopped by you; /retry runs it again")
	case !ok:
		e.finish(task, agent, StatusBlocked, "setup failed:\n"+verify.Summary(results))
	default:
		return true
	}
	return false
}

// work runs the worker turn and records its session and usage.
func (e *Engine) work(ctx context.Context, task *store.Task, agent store.RunAgent, env []string) (string, error) {
	scoped, err := rules.Scoped(e.opts.Rules, agent.Role, agent.Name, planID(task.ID))
	if err != nil {
		return "", err
	}
	turnCtx, cancel := context.WithTimeout(ctx, time.Duration(e.opts.Config.TurnTimeout)*time.Second)
	defer cancel()
	idle := time.Duration(e.opts.Config.IdleTimeout) * time.Second
	var silent atomic.Bool
	watchdog := time.AfterFunc(idle, func() { silent.Store(true); cancel() })
	defer watchdog.Stop()
	e.stopMu.Lock()
	e.turns[agent.Name] = cancel
	e.watchdogs[task.ID] = watchdog
	e.stopMu.Unlock()
	defer func() {
		e.stopMu.Lock()
		delete(e.turns, agent.Name)
		delete(e.watchdogs, task.ID)
		e.stopMu.Unlock()
	}()
	result, err := e.opts.Turn(turnCtx, TurnRequest{
		Label:      task.Title,
		Agent:      agent,
		TaskID:     task.ID,
		Cwd:        e.dir(task.Worktree),
		SessionID:  task.SessionID,
		Prompt:     WorkerPrompt(*task, agent, scoped) + envNote(env) + subagentNote(e.opts.Config.Subagents.MaxPerBuilder) + handoffNote(e.Limits(agent.Name)),
		Permission: e.opts.Permission,
		Ask:        e.opts.OnPermission != nil,
		Env:        env,
	})
	if err != nil && silent.Load() {
		err = fmt.Errorf("%s sent nothing for %s, so Switch stopped its turn", agent.Name, idle)
	}
	e.recordTurn(*task, agent, result)
	if result.SessionID != "" {
		task.SessionID = result.SessionID
	}
	saveCtx, done := saveContext()
	stored, _, _ := e.opts.Store.GetTask(saveCtx, task.ID)
	done()
	switch text := strings.TrimSpace(result.Output); {
	case stored.Summary != "": // switch_complete saved it mid-turn; like a subagent's report, it wins
		task.Summary = stored.Summary
	case text != "":
		task.Summary = text
	}
	e.update(*task, agent.Name, "")
	return result.StopReason, err
}

// beat restarts the no-events watchdog of a task's worker turn when agent,
// the worker or one of its subagents, sent an event. It stays off while any
// of them waits for a permission answer, since the user may take their time;
// the asking agent's next event, or the end of its turn, ends that wait.
func (e *Engine) beat(taskID, agent string, asking bool) {
	e.stopMu.Lock()
	defer e.stopMu.Unlock()
	if asking {
		e.asking[agent] = taskID
	} else {
		delete(e.asking, agent)
	}
	watchdog, ok := e.watchdogs[taskID]
	if !ok {
		return
	}
	watchdog.Stop()
	for _, waiting := range e.asking {
		if waiting == taskID {
			return
		}
	}
	watchdog.Reset(time.Duration(e.opts.Config.IdleTimeout) * time.Second)
}

type verdict int

const (
	verdictUndecided verdict = iota
	verdictApprove
	verdictReject
)

// StopWorker cancels a worker's running task turn, or the setup before it.
// The task is blocked for the user instead of retried.
func (e *Engine) StopWorker(name string) bool {
	e.stopMu.Lock()
	defer e.stopMu.Unlock()
	cancel, ok := e.turns[name]
	if !ok {
		return false
	}
	ctx, done := saveContext()
	defer done()
	tasks, _ := e.opts.Store.TasksForRun(ctx, e.run.ID)
	for _, task := range tasks {
		if task.Assignee == name && task.Status == StatusRunning {
			e.stopping[task.ID] = true
		}
	}
	cancel()
	return true
}

// leadReview asks the run's lead to review the worker's task diff. A missing
// lead or a failed review leaves the decision to the user; only an explicit
// REJECT sends the task back.
func (e *Engine) leadReview(ctx context.Context, task store.Task, worker store.RunAgent, evidence string) (verdict, string) {
	lead, ok := e.lead()
	if !ok {
		return verdictUndecided, "no lead in this run; review it yourself"
	}
	worked, err := e.worked(task.ID, lead.Name)
	if err != nil {
		return verdictUndecided, "lead review skipped: " + err.Error()
	}
	if worked {
		return verdictUndecided, lead.Name + " worked on this task itself, so no other agent reviewed it; review it yourself"
	}
	diff, err := e.diff(ctx, task)
	if err != nil {
		return verdictUndecided, "lead review skipped: " + err.Error()
	}
	scoped, err := rules.Scoped(e.opts.Rules, "lead", lead.Name, planID(task.ID))
	if err != nil {
		return verdictUndecided, "lead review skipped: " + err.Error()
	}
	turnCtx, cancel := context.WithTimeout(ctx, time.Duration(e.opts.Config.TurnTimeout)*time.Second)
	defer cancel()
	result, err := e.opts.Turn(turnCtx, TurnRequest{
		Label:      "review " + task.Title,
		Agent:      lead,
		TaskID:     task.ID,
		Cwd:        e.dir(task.Worktree),
		Prompt:     ReviewPrompt(task, worker, diff, evidence, scoped),
		Permission: acpdriver.PermissionDeny, // reviewers read; they do not edit
		Ephemeral:  true,
	})
	e.recordTurn(task, lead, result)
	if err != nil {
		return verdictUndecided, "lead review failed: " + err.Error()
	}
	return parseVerdict(result.Output)
}

// worked reports whether agent ever held the task, even if it passed it on:
// the engine logs each status change under the agent working on the task.
func (e *Engine) worked(taskID, agent string) (bool, error) {
	ctx, cancel := saveContext()
	defer cancel()
	// shortcut: reads all the run's events once per review; add a per-task query to the store if runs log enough events to notice.
	events, err := e.opts.Store.Events(ctx, e.run.ID, math.MaxInt32)
	if err != nil {
		return false, err
	}
	for _, event := range events {
		if event.TaskID == taskID && event.Agent == agent && event.Kind == "status" {
			return true, nil
		}
	}
	return false, nil
}

// parseVerdict reads the verdict from the first line only, as the review
// prompt asks: later lines are reasons, which may well start with "Rejected".
func parseVerdict(output string) (verdict, string) {
	notes := strings.TrimSpace(output)
	first, _, _ := strings.Cut(notes, "\n")
	word := strings.ToUpper(strings.Trim(strings.TrimSpace(first), "*#`> "))
	word = strings.Trim(strings.TrimPrefix(word, "VERDICT:"), " *`") // "**Verdict: APPROVE**"
	switch {
	case strings.HasPrefix(word, "APPROVE"):
		return verdictApprove, notes
	case strings.HasPrefix(word, "REJECT"):
		return verdictReject, notes
	}
	return verdictUndecided, notes
}

func (e *Engine) lead() (store.RunAgent, bool) {
	ctx, cancel := saveContext()
	defer cancel()
	agents, _ := e.opts.Store.RunAgents(ctx, e.run.ID)
	for _, agent := range agents {
		if agent.Role == "lead" {
			return agent, true
		}
	}
	return store.RunAgent{}, false
}

// Diff returns the task's change relative to the integration branch.
func (e *Engine) Diff(ctx context.Context, taskID string) (string, error) {
	task, ok, err := e.opts.Store.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("task %s not found", taskID)
	}
	return e.diff(ctx, task)
}

func (e *Engine) diff(ctx context.Context, task store.Task) (string, error) {
	if task.Status == StatusMerged && task.Commit != "" {
		// Its worktree is gone; the repository still has what its merge brought in.
		merge, before, err := worktree.Merged(ctx, e.manager.Repo, e.run.IntegrationBranch, task.Commit)
		if err != nil {
			return "", err
		}
		return worktree.Diff(ctx, e.manager.Repo, before, merge)
	}
	if task.Commit == "" || task.Worktree == "" {
		return "", errors.New("task has no commit yet")
	}
	base, err := worktree.MergeBase(ctx, task.Worktree, e.run.IntegrationBranch, task.Commit)
	if err != nil {
		return "", err
	}
	return worktree.Diff(ctx, task.Worktree, base, task.Commit)
}

// Approve merges a reviewed task into the integration branch once the merge
// passes the integration checks, and ends a run nothing drives once its last
// task is merged. Only the user approves; agents cannot call this.
func (e *Engine) Approve(ctx context.Context, taskID string) error {
	defer e.signal()
	lock, err := lockMerge(ctx, e.run.IntegrationPath)
	if err != nil {
		return err
	}
	defer lock.Release()

	e.mu.Lock()
	// Only the merge lock's holder sets approved, so an approved task found
	// here was left by an approval that died, and approving it again is safe.
	task, err := e.awaitingReview(ctx, taskID, StatusApproved)
	if err == nil {
		from := task.Status
		task.Status = StatusApproved
		err = e.claim(ctx, task, from, "user", "")
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}

	// The approved task is this call's alone now, and the run's other
	// transitions go on while the integration check runs.
	err = e.integrate(ctx, task)
	switch {
	case err == nil:
		task.Status, task.LastError = StatusMerged, ""
		if e.manager.Remove(context.WithoutCancel(ctx), task.Worktree) == nil {
			task.Worktree = "" // the branch and commit remain
		}
		e.update(task, "user", "merged into "+e.run.IntegrationBranch)
		lock.Release() // the run's checks take it again
		e.finishRun(ctx)
		return nil
	case ctx.Err() != nil:
		task.Status = StatusReview
		e.update(task, "user", "approval interrupted; nothing was merged")
		return fmt.Errorf("approval of %s interrupted; nothing was merged, and it waits for review again: %w", planID(task.ID), err)
	case errors.Is(err, worktree.ErrConflict):
		err = errors.New(e.mergeIntegration(ctx, task))
	}
	task.Status, task.LastError = StatusBlocked, err.Error()
	e.update(task, "user", task.LastError)
	return err
}

// finishRun ends the run as its loop would once the user approved the last
// task it waited on while nothing drives it, such as with `__control
// approve`: completed, or blocked by its checks or by a blocked task. A
// process driving the run, which holds its lock, ends it itself. The lock
// names no owner: an approval drives nothing for /stop to stop, nor does it
// make a TUI that approved show its own run as driven elsewhere.
func (e *Engine) finishRun(ctx context.Context) {
	owner, err := lockRun(e.opts.StateDir, e.run.ID, "")
	if err != nil {
		return
	}
	defer owner.Release()
	// Without budget, a task left to start reads as RunBudget: starting it is a run's job.
	switch outcome, stuck := e.outcome(false); outcome {
	case RunCompleted:
		_, _ = e.complete(ctx)
	case RunBlocked:
		e.setRunStatus(outcome, stuck...)
	}
}

// integrate merges the task's reviewed commit in the integration worktree,
// on a detached HEAD, and moves the integration branch there only once the
// integration checks pass: the task's own and the run's. The branch never
// holds an unverified merge, so an approval that is interrupted or killed
// mid-check leaves nothing to undo. The caller holds the merge lock.
func (e *Engine) integrate(ctx context.Context, task store.Task) error {
	dir, branch := e.run.IntegrationPath, e.run.IntegrationBranch
	if err := worktree.Checkout(ctx, dir, "--detach", branch); err != nil {
		return fmt.Errorf("integration worktree: %w", err)
	}
	// Whatever happens, drop what the checks wrote and hold the branch again,
	// so git refuses to check it out anywhere else while the run goes on.
	defer worktree.Checkout(context.WithoutCancel(ctx), dir, branch)
	if err := worktree.Merge(ctx, dir, task.Commit, "switch: merge "+task.Title); err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	// Setup runs after the merge, which may have changed the dependencies.
	// Integration checks run one at a time, so they share slot 0.
	env := e.env(task.ID, 0)
	results, passed := verify.Run(ctx, e.dir(dir), e.opts.Config.Setup, e.verifyTimeout(), nil, env...)
	e.noteFlaky(task.ID, task.Assignee, "integration setup", results)
	if !passed {
		return fmt.Errorf("integration setup failed; nothing was merged:\n%s", verify.Summary(results))
	}
	checks := slices.Clone(task.Verify)
	for _, check := range e.opts.Config.Verify {
		if !slices.Contains(checks, check) {
			checks = append(checks, check)
		}
	}
	results, passed = verify.Run(ctx, e.dir(dir), checks, e.verifyTimeout(), func() error { return worktree.Clean(ctx, dir) }, env...)
	e.recordEvidence(task, "integration", results)
	if !passed {
		return fmt.Errorf("integration verification failed; nothing was merged:\n%s", verify.Summary(results))
	}
	return worktree.Advance(context.WithoutCancel(ctx), dir, branch)
}

// checkRun runs the run's own checks once more on the integration branch, as
// all its tasks left it, before the run is completed.
func (e *Engine) checkRun(ctx context.Context) error {
	if len(e.opts.Config.Verify) == 0 {
		return nil
	}
	dir, branch := e.run.IntegrationPath, e.run.IntegrationBranch
	lock, err := lockMerge(ctx, dir)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := worktree.Checkout(ctx, dir, branch); err != nil {
		return fmt.Errorf("integration worktree: %w", err)
	}
	defer worktree.Clean(context.WithoutCancel(ctx), dir)
	env := e.env("", 0)
	results, passed := verify.Run(ctx, e.dir(dir), e.opts.Config.Setup, e.verifyTimeout(), nil, env...)
	e.noteFlaky("", "", "integration setup", results)
	if passed {
		results, passed = verify.Run(ctx, e.dir(dir), e.opts.Config.Verify, e.verifyTimeout(), func() error { return worktree.Clean(ctx, dir) }, env...)
		e.noteFlaky("", "", "run check", results)
	}
	if !passed {
		return fmt.Errorf("the run's checks failed on %s:\n%s", branch, verify.Summary(results))
	}
	return nil
}

// mergeIntegration merges the integration branch into a task's worktree that
// conflicts with it, leaving the markers for a worker, and says so.
func (e *Engine) mergeIntegration(ctx context.Context, task store.Task) string {
	err := worktree.Update(ctx, task.Worktree, e.run.IntegrationBranch)
	switch {
	case errors.Is(err, worktree.ErrConflict):
		return err.Error() + " with work merged since this task started. Switch merged the integration branch into the worktree: resolve the conflict markers there, keeping both changes; Switch commits the result."
	case err != nil:
		return "the change conflicts with work merged since this task started: " + err.Error()
	}
	return "Switch merged the integration branch, which holds work merged since this task started, into the worktree: check the change still works with it."
}

// lockMerge waits for the merge lock, so approvals from any engine or process
// merge and verify one at a time.
func lockMerge(ctx context.Context, integrationPath string) (*RunLock, error) {
	for {
		if lock, err := TryLockMerge(integrationPath); lock != nil || err != nil {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Reject sends a reviewed task back to its worker with the user's reason.
func (e *Engine) Reject(ctx context.Context, taskID, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "no reason given"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.signal()
	task, err := e.awaitingReview(ctx, taskID)
	if err != nil {
		return err
	}
	task = e.sendBack(task, "the user rejected the change: "+reason)
	return e.claim(ctx, task, StatusReview, task.Assignee, task.LastError)
}

// Retry requeues a blocked or interrupted task with a fresh attempt budget,
// for any worker of its role.
func (e *Engine) Retry(ctx context.Context, taskID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.signal()
	task, ok, err := e.opts.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if !ok || task.RunID != e.run.ID {
		return fmt.Errorf("task %s not found", taskID)
	}
	if task.Status != StatusBlocked && task.Status != StatusInterrupted {
		return fmt.Errorf("task %s is %s; only blocked or interrupted tasks can be retried", planID(task.ID), task.Status)
	}
	from := task.Status
	if task.Attempts > 0 { // a task never attempted, such as a proposal, has no attempt to learn from
		task.Feedback = strings.TrimSpace(task.Feedback + "\n" + task.LastError)
	}
	task.Status = StatusQueued
	task.Attempts = 0
	task.Assignee, task.SessionID = "", "" // sessions belong to one agent, and any may take it
	return e.claim(ctx, task, from, "user", "retry")
}

// awaitingReview loads a task of this run that is in review, or in one of also.
func (e *Engine) awaitingReview(ctx context.Context, taskID string, also ...string) (store.Task, error) {
	task, ok, err := e.opts.Store.GetTask(ctx, taskID)
	if err != nil {
		return store.Task{}, err
	}
	if !ok || task.RunID != e.run.ID {
		return store.Task{}, fmt.Errorf("task %s not found", taskID)
	}
	if task.Status != StatusReview && !slices.Contains(also, task.Status) {
		return store.Task{}, fmt.Errorf("task %s is %s, not waiting for review", planID(task.ID), task.Status)
	}
	return task, nil
}

// retry queues another attempt for the same worker with feedback, or blocks
// the task once attempts run out.
func (e *Engine) retry(task store.Task, agent store.RunAgent, feedback string) {
	task = e.sendBack(task, feedback)
	e.update(task, agent.Name, task.LastError)
}

// sendBack is the task queued again with feedback. Once its attempts run
// out, it is handed off to its worker's escalation agent for one attempt
// more, or else blocked.
func (e *Engine) sendBack(task store.Task, feedback string) store.Task {
	limit := e.opts.Config.MaxAttempts
	if task.Attempts < limit {
		task.Status, task.Feedback, task.LastError = StatusQueued, feedback, feedback
		return task
	}
	if to := e.escalation(task.Assignee); task.Attempts == limit && to != "" {
		note := fmt.Sprintf("%s reached the attempt limit (%d) without finishing:\n%s", task.Assignee, limit, feedback)
		task = passOn(task, task.Assignee, handoff{to: to, note: note})
		task.Status, task.LastError = StatusQueued, fmt.Sprintf("attempt limit %d reached; escalated to %s: %s", limit, to, feedback)
		return task
	}
	task.Status = StatusBlocked
	task.LastError = fmt.Sprintf("attempt limit %d reached: %s", limit, feedback)
	return task
}

func (e *Engine) finish(task store.Task, agent store.RunAgent, status, reason string) {
	task.Status = status
	task.LastError = reason
	e.update(task, agent.Name, reason)
}

func (e *Engine) interrupted(ctx context.Context, task store.Task, agent store.RunAgent) bool {
	if ctx.Err() == nil {
		return false
	}
	task.Status = StatusInterrupted
	e.update(task, agent.Name, "cancelled")
	return true
}

// update saves a transition of a task this goroutine owns.
func (e *Engine) update(task store.Task, agent, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ctx, cancel := saveContext()
	defer cancel()
	e.save(ctx, task, agent, message)
}

func (e *Engine) recordEvidence(task store.Task, scope string, results []verify.Result) {
	ctx, cancel := saveContext()
	defer cancel()
	for _, r := range results {
		_ = e.opts.Store.AddVerification(ctx, store.Verification{
			TaskID: task.ID, Attempt: task.Attempts, Scope: scope, Command: r.Command,
			ExitCode: r.ExitCode, Output: r.Output, Duration: r.Duration, TimedOut: r.TimedOut,
		})
	}
	e.noteFlaky(task.ID, task.Assignee, scope+" check", results)
}

// noteFlaky logs each command, a check or setup, that passed on its rerun.
func (e *Engine) noteFlaky(taskID, agent, what string, results []verify.Result) {
	ctx, cancel := saveContext()
	defer cancel()
	for _, r := range results {
		if r.Flaky {
			e.log(ctx, taskID, agent, "flaky", fmt.Sprintf("%s `%s` failed, then passed on a rerun", what, r.Command))
		}
	}
}

// env is what a task's worker and checks are told about the task, so tasks
// running at once can keep apart, such as on ports and test databases.
func (e *Engine) env(taskID string, slot int) []string {
	return []string{fmt.Sprintf("SWITCH_SLOT=%d", slot), "SWITCH_TASK=" + planID(taskID), "SWITCH_RUN=" + e.run.ID}
}

// recordTurn stores a turn's tokens. The OpenCode, Codex and Claude adapters
// report them per turn, whatever acp-go-sdk's field docs say about session
// totals, so a run's usage is the sum of its turns.
func (e *Engine) recordTurn(task store.Task, agent store.RunAgent, result TurnResult) {
	ctx, cancel := saveContext()
	defer cancel()
	u := result.Usage
	_ = e.opts.Store.AddUsage(ctx, store.Usage{
		RunID: e.run.ID, TaskID: task.ID, Agent: agent.Name,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, TotalTokens: u.TotalTokens,
	})
	message := fmt.Sprintf("tokens=%d", u.TotalTokens)
	if u.CostAmount > 0 {
		message += fmt.Sprintf(" session_cost=%.4f %s", u.CostAmount, u.CostCurrency)
	}
	e.log(ctx, task.ID, agent.Name, "turn", message)
	e.signal() // so the run checks its budget now, not when a task next ends
}

func (e *Engine) verifyTimeout() time.Duration {
	return time.Duration(e.opts.Config.VerifyTimeout) * time.Second
}

// dir is where agents and checks work in a worktree: the project's directory,
// which is a subdirectory when Switch was started in one of the repository.
// git checks out committed files only, so the directory is made while the
// project has none. A missing worktree is not: git would find another
// repository above it.
func (e *Engine) dir(worktree string) string {
	dir := filepath.Join(worktree, e.manager.Prefix)
	if _, err := os.Stat(worktree); err == nil {
		_ = os.MkdirAll(dir, 0o755)
	}
	return dir
}

func commitMessage(task store.Task, agent store.RunAgent) string {
	return fmt.Sprintf("switch: %s\n\nTask %s, attempt %d, by %s (%s).", task.Title, planID(task.ID), task.Attempts, agent.Name, agent.Driver)
}
