package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
	"github.com/urizennnn/switch/internal/verify"
	"github.com/urizennnn/switch/internal/worktree"
)

// process takes one dispatched task through work, verification, commit, and
// lead review. It ends at review (waiting for the user), back in the queue
// with feedback, blocked, or interrupted.
func (e *Engine) process(ctx context.Context, task store.Task, agent store.RunAgent) {
	err := e.work(ctx, &task, agent)
	if e.interrupted(ctx, task, agent) {
		return
	}
	if err != nil {
		e.retry(task, agent, "worker error: "+err.Error())
		return
	}
	if e.opts.Control != nil {
		if reason, ok := e.opts.Control.TakeBlock(task.ID); ok {
			e.finish(task, agent, StatusBlocked, "worker blocked: "+reason)
			return
		}
	}

	task.Status = StatusVerifying
	e.update(task, agent.Name, "")
	results, passed := verify.Run(ctx, task.Worktree, task.Verify, e.verifyTimeout())
	e.recordEvidence(task, "task", results)
	if e.interrupted(ctx, task, agent) {
		return
	}
	if !passed {
		e.retry(task, agent, "verification failed:\n"+verify.Summary(results))
		return
	}

	commit, _, err := worktree.Commit(ctx, task.Worktree, commitMessage(task, agent))
	if err != nil {
		e.finish(task, agent, StatusBlocked, "commit: "+err.Error())
		return
	}
	task.Commit = commit
	task.Feedback = ""

	if e.opts.Config.LeadReview {
		task.Status = StatusReviewing
		e.update(task, agent.Name, "")
		verdict, notes := e.leadReview(ctx, task, verify.Summary(results))
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

// work runs the worker turn and records its session and usage.
func (e *Engine) work(ctx context.Context, task *store.Task, agent store.RunAgent) error {
	scoped, err := rules.Scoped(e.opts.Rules, agent.Role, agent.Name, planID(task.ID))
	if err != nil {
		return err
	}
	turnCtx, cancel := context.WithTimeout(ctx, time.Duration(e.opts.Config.TurnTimeout)*time.Second)
	defer cancel()
	result, err := e.opts.Turn(turnCtx, TurnRequest{
		Agent:      agent,
		TaskID:     task.ID,
		Cwd:        task.Worktree,
		SessionID:  task.SessionID,
		Prompt:     WorkerPrompt(*task, agent, scoped),
		Permission: e.opts.Permission,
		Ask:        e.opts.OnPermission != nil,
	})
	e.recordTurn(*task, agent, result)
	if result.SessionID != "" {
		task.SessionID = result.SessionID
	}
	if text := strings.TrimSpace(result.Output); text != "" {
		task.Summary = text
	}
	e.update(*task, agent.Name, "")
	return err
}

type verdict int

const (
	verdictUndecided verdict = iota
	verdictApprove
	verdictReject
)

// leadReview asks the run's lead to review the task diff. A missing lead or a
// failed review leaves the decision to the user; only an explicit REJECT
// sends the task back.
func (e *Engine) leadReview(ctx context.Context, task store.Task, evidence string) (verdict, string) {
	lead, ok := e.lead()
	if !ok {
		return verdictUndecided, "no lead in this run; review it yourself"
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
		Agent:      lead,
		TaskID:     task.ID,
		Cwd:        task.Worktree,
		Prompt:     ReviewPrompt(task, diff, evidence, scoped),
		Permission: acpdriver.PermissionDeny, // reviewers read; they do not edit
		Ephemeral:  true,
	})
	e.recordTurn(task, lead, result)
	if err != nil {
		return verdictUndecided, "lead review failed: " + err.Error()
	}
	return parseVerdict(result.Output)
}

func parseVerdict(output string) (verdict, string) {
	notes := strings.TrimSpace(output)
	for _, line := range strings.Split(notes, "\n") {
		word := strings.ToUpper(strings.Trim(strings.TrimSpace(line), "*#`> "))
		switch {
		case strings.HasPrefix(word, "APPROVE"):
			return verdictApprove, notes
		case strings.HasPrefix(word, "REJECT"):
			return verdictReject, notes
		}
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
	if task.Commit == "" || task.Worktree == "" {
		return "", errors.New("task has no commit yet")
	}
	base, err := worktree.MergeBase(ctx, task.Worktree, e.run.IntegrationBranch, task.Commit)
	if err != nil {
		return "", err
	}
	return worktree.Diff(ctx, task.Worktree, base, task.Commit)
}

// Approve merges a reviewed task into the integration branch, then verifies
// the integration. Only the user approves; agents cannot call this.
func (e *Engine) Approve(ctx context.Context, taskID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.signal()

	task, err := e.awaitingReview(ctx, taskID)
	if err != nil {
		return err
	}
	task.Status = StatusApproved
	e.save(ctx, task, "user", "")

	before, err := worktree.Head(ctx, e.run.IntegrationPath)
	if err != nil {
		return e.block(ctx, task, "integration worktree: "+err.Error())
	}
	if err := worktree.Merge(ctx, e.run.IntegrationPath, task.Branch, "switch: merge "+task.Title); err != nil {
		return e.block(ctx, task, "merge: "+err.Error())
	}
	results, passed := verify.Run(ctx, e.run.IntegrationPath, task.Verify, e.verifyTimeout())
	e.recordEvidence(task, "integration", results)
	if !passed {
		resetErr := worktree.ResetHard(ctx, e.run.IntegrationPath, before)
		return e.block(ctx, task, errors.Join(
			fmt.Errorf("integration verification failed; merge undone:\n%s", verify.Summary(results)),
			resetErr).Error())
	}

	task.Status = StatusMerged
	task.LastError = ""
	e.save(ctx, task, "user", "merged into "+e.run.IntegrationBranch)
	if err := e.manager.Remove(ctx, task.Worktree); err == nil {
		task.Worktree = "" // the branch and commit remain
		_ = e.opts.Store.SaveTask(ctx, task)
	}
	return nil
}

// Reject sends a reviewed task back to its worker with the user's reason.
func (e *Engine) Reject(ctx context.Context, taskID, reason string) error {
	e.mu.Lock()
	task, err := e.awaitingReview(ctx, taskID)
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		reason = "no reason given"
	}
	e.retry(task, store.RunAgent{Name: task.Assignee}, "the user rejected the change: "+reason)
	e.signal()
	return nil
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
	task.Status = StatusQueued
	task.Attempts = 0
	task.Assignee = ""
	task.Feedback = strings.TrimSpace(task.Feedback + "\n" + task.LastError)
	e.save(ctx, task, "user", "retry")
	return nil
}

func (e *Engine) awaitingReview(ctx context.Context, taskID string) (store.Task, error) {
	task, ok, err := e.opts.Store.GetTask(ctx, taskID)
	if err != nil {
		return store.Task{}, err
	}
	if !ok || task.RunID != e.run.ID {
		return store.Task{}, fmt.Errorf("task %s not found", taskID)
	}
	if task.Status != StatusReview {
		return store.Task{}, fmt.Errorf("task %s is %s, not waiting for review", planID(task.ID), task.Status)
	}
	return task, nil
}

func (e *Engine) block(ctx context.Context, task store.Task, reason string) error {
	task.Status = StatusBlocked
	task.LastError = reason
	e.save(ctx, task, "user", reason)
	return errors.New(reason)
}

// retry queues another attempt for the same worker with feedback, or blocks
// the task once attempts run out.
func (e *Engine) retry(task store.Task, agent store.RunAgent, feedback string) {
	if task.Attempts >= e.opts.Config.MaxAttempts {
		e.finish(task, agent, StatusBlocked, fmt.Sprintf("%s\n(attempt limit %d reached)", feedback, e.opts.Config.MaxAttempts))
		return
	}
	task.Feedback = feedback
	e.finish(task, agent, StatusQueued, feedback)
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
}

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
}

func (e *Engine) verifyTimeout() time.Duration {
	return time.Duration(e.opts.Config.VerifyTimeout) * time.Second
}

func commitMessage(task store.Task, agent store.RunAgent) string {
	return fmt.Sprintf("switch: %s\n\nTask %s, attempt %d, by %s (%s).", task.Title, planID(task.ID), task.Attempts, agent.Name, agent.Driver)
}
