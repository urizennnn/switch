// Package execution runs a planned run: it dispatches ready tasks to worker
// agents, verifies their work, has the lead review it, and merges approved
// work into the run integration branch.
//
// Every task gets its own worktree, branched from the integration branch when
// it is first dispatched, so dependents see merged work and rejected work
// never leaks into other tasks. A worker runs one task at a time.
package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
	"github.com/urizennnn/switch/internal/worktree"
)

// Task statuses.
const (
	StatusQueued      = "queued"
	StatusRunning     = "running"   // worker turn in progress
	StatusVerifying   = "verifying" // verification commands running
	StatusReviewing   = "reviewing" // lead reviewing the diff
	StatusReview      = "review"    // waiting for the user's approval
	StatusApproved    = "approved"  // approved; merge in progress
	StatusMerged      = "merged"
	StatusBlocked     = "blocked" // needs the user: retries exhausted, conflict, or worker gave up
	StatusInterrupted = "interrupted"
)

// Run outcomes stored in runs.status.
const (
	RunRunning   = "running"
	RunWaiting   = "review"    // waiting for approvals
	RunCompleted = "completed" // every task merged
	RunBlocked   = "blocked"   // no task can make progress
	RunBudget    = "budget"    // stopped by max_turns or max_tokens
	RunStopped   = "stopped"   // cancelled by the user
)

// TurnRequest is one agent turn: a worker task attempt or a lead review.
type TurnRequest struct {
	Agent      store.RunAgent
	TaskID     string
	Cwd        string
	SessionID  string // resume when non-empty
	Prompt     string
	Permission acpdriver.PermissionMode
	// Ask routes permission requests to Options.OnPermission when set.
	Ask bool
}

type TurnResult struct {
	Output    string
	SessionID string
	Usage     acpdriver.UsageInfo
}

// TurnFunc runs one agent turn. The default starts the agent's ACP adapter.
type TurnFunc func(ctx context.Context, req TurnRequest) (TurnResult, error)

type Options struct {
	Store  *store.Store
	RunID  string
	Rules  rules.Set // global/project/local; role and task rules are added per prompt
	Config config.RunConfig
	// StateDir holds worktrees; defaults to config.StateDir().
	StateDir string
	// WaitForReview keeps Run alive while tasks wait for approval, so a TUI
	// can approve them. Headless callers return instead.
	WaitForReview bool
	// Permission answers worker requests when OnPermission is nil.
	Permission   acpdriver.PermissionMode
	OnPermission func(*acpdriver.PermissionRequest)
	OnText       func(agent, text string)
	// OnChange is called after every task transition.
	OnChange  func()
	AgentLogs io.Writer
	// Turn overrides how agent turns run; tests use a fake.
	Turn TurnFunc
	// Control, when set, is offered to agents as the Switch MCP server.
	Control ControlPlane
}

// ControlPlane lets agents report to Switch during a turn.
type ControlPlane interface {
	// Grant returns the MCP servers and environment for one agent turn.
	Grant(agent store.RunAgent, taskID string) (Grant, error)
	// TakeBlock returns and clears a block reported by the task's worker.
	TakeBlock(taskID string) (string, bool)
}

// Grant is what an agent turn needs to reach the control plane.
type Grant struct {
	MCPServers []acp.McpServer
	Env        []string
}

type Engine struct {
	opts    Options
	run     store.Run
	manager *worktree.Manager

	mu   sync.Mutex // serializes task transitions and merges
	wake chan struct{}
}

// New loads the run and prepares an engine for it.
func New(ctx context.Context, opts Options) (*Engine, error) {
	if opts.Store == nil {
		return nil, errors.New("execution: store is required")
	}
	run, ok, err := opts.Store.GetRun(ctx, opts.RunID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("run %s not found", opts.RunID)
	}
	if opts.StateDir == "" {
		opts.StateDir = config.StateDir()
	}
	defaults := config.Defaults("").Run
	if opts.Config.MaxConcurrency <= 0 {
		opts.Config.MaxConcurrency = defaults.MaxConcurrency
	}
	if opts.Config.MaxAttempts <= 0 {
		opts.Config.MaxAttempts = defaults.MaxAttempts
	}
	if opts.Config.VerifyTimeout <= 0 {
		opts.Config.VerifyTimeout = defaults.VerifyTimeout
	}
	if opts.Config.TurnTimeout <= 0 {
		opts.Config.TurnTimeout = defaults.TurnTimeout
	}
	if opts.Permission == "" {
		opts.Permission = acpdriver.PermissionDeny
	}
	manager, err := worktree.New(ctx, run.Project, opts.StateDir)
	if err != nil {
		return nil, err
	}
	if run.IntegrationPath == "" {
		// Runs planned before integration_path was stored.
		run.IntegrationPath = manager.Root(run.ID) + "/integration"
	}
	e := &Engine{opts: opts, run: run, manager: manager, wake: make(chan struct{}, 1)}
	if e.opts.Turn == nil {
		e.opts.Turn = e.acpTurn
	}
	return e, nil
}

// Run schedules tasks until none can make progress. It returns the run
// outcome; a cancelled context interrupts running tasks and returns
// RunStopped with the context error.
func (e *Engine) Run(ctx context.Context) (string, error) {
	if err := e.recover(); err != nil {
		return "", err
	}
	e.setRunStatus(RunRunning)

	done := make(chan string, e.opts.Config.MaxConcurrency)
	busy := map[string]bool{}
	for {
		budget, err := e.budgetLeft()
		if err != nil {
			return "", err
		}
		if budget && len(busy) < e.opts.Config.MaxConcurrency {
			for _, a := range e.dispatch(busy, e.opts.Config.MaxConcurrency-len(busy)) {
				busy[a.agent.Name] = true
				go func(a assignment) {
					e.process(ctx, a.task, a.agent)
					done <- a.agent.Name
				}(a)
			}
		}

		if len(busy) == 0 {
			outcome := e.outcome(budget)
			if outcome == RunWaiting && e.opts.WaitForReview {
				select {
				case <-e.wake:
					continue
				case <-ctx.Done():
					e.setRunStatus(RunStopped)
					return RunStopped, ctx.Err()
				}
			}
			e.setRunStatus(outcome)
			return outcome, nil
		}

		select {
		case name := <-done:
			delete(busy, name)
		case <-e.wake:
		case <-ctx.Done():
			for len(busy) > 0 {
				delete(busy, <-done)
			}
			e.setRunStatus(RunStopped)
			return RunStopped, ctx.Err()
		}
	}
}

type assignment struct {
	task  store.Task
	agent store.RunAgent
}

// Ready picks up to limit runnable tasks, in plan order, each with a distinct
// idle worker. A retried task goes back to the worker that attempted it.
func Ready(tasks []store.Task, agents []store.RunAgent, busy map[string]bool, limit int) []assignment {
	merged := make(map[string]bool, len(tasks))
	taken := make(map[string]bool, len(busy))
	for name := range busy {
		taken[name] = true
	}
	for _, task := range tasks {
		switch task.Status {
		case StatusMerged, "done":
			merged[task.ID] = true
		case StatusRunning:
			taken[task.Assignee] = true
		}
	}

	var out []assignment
	for _, task := range tasks {
		if len(out) >= limit {
			break
		}
		if task.Status != StatusQueued && task.Status != StatusInterrupted {
			continue
		}
		if !depsMerged(task, merged) {
			continue
		}
		for _, agent := range agents {
			if agent.Role == "lead" || agent.Role != task.Role || taken[agent.Name] {
				continue
			}
			if task.Assignee != "" && task.Assignee != agent.Name && hasAgent(agents, task.Assignee) {
				continue
			}
			taken[agent.Name] = true
			out = append(out, assignment{task: task, agent: agent})
			break
		}
	}
	return out
}

func depsMerged(task store.Task, merged map[string]bool) bool {
	for _, dep := range task.Deps {
		if !merged[dep] {
			return false
		}
	}
	return true
}

func hasAgent(agents []store.RunAgent, name string) bool {
	for _, agent := range agents {
		if agent.Name == name {
			return true
		}
	}
	return false
}

// dispatch marks ready tasks running and gives each one its worktree.
func (e *Engine) dispatch(busy map[string]bool, limit int) []assignment {
	e.mu.Lock()
	defer e.mu.Unlock()
	ctx, cancel := saveContext()
	defer cancel()

	tasks, err := e.opts.Store.TasksForRun(ctx, e.run.ID)
	if err != nil {
		return nil
	}
	agents, err := e.opts.Store.RunAgents(ctx, e.run.ID)
	if err != nil {
		return nil
	}
	var started []assignment
	for _, a := range Ready(tasks, agents, busy, limit) {
		task := a.task
		if task.Worktree == "" {
			wt, err := e.manager.Create(ctx, e.run.ID, "task-"+planID(task.ID), e.run.IntegrationBranch)
			if err != nil {
				task.Status = StatusBlocked
				task.LastError = "create worktree: " + err.Error()
				e.save(ctx, task, a.agent.Name, task.LastError)
				continue
			}
			task.Worktree, task.Branch = wt.Path, wt.Branch
		}
		task.Status = StatusRunning
		task.Assignee = a.agent.Name
		task.Attempts++
		task.LastError = ""
		if !e.save(ctx, task, a.agent.Name, fmt.Sprintf("attempt %d", task.Attempts)) {
			continue
		}
		started = append(started, assignment{task: task, agent: a.agent})
	}
	return started
}

// recover returns tasks a crashed process left mid-flight to a resumable state.
// shortcut: assumes one Switch process drives a run; add run ownership (a
// lock file next to the control socket) before allowing several.
func (e *Engine) recover() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ctx, cancel := saveContext()
	defer cancel()
	tasks, err := e.opts.Store.TasksForRun(ctx, e.run.ID)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		switch task.Status {
		case StatusRunning, StatusVerifying, StatusReviewing:
			task.Status = StatusInterrupted
		case StatusApproved:
			// Merging is idempotent, so the user can approve again.
			task.Status = StatusReview
		default:
			continue
		}
		e.save(ctx, task, task.Assignee, "recovered after restart")
	}
	return nil
}

func (e *Engine) outcome(budget bool) string {
	ctx, cancel := saveContext()
	defer cancel()
	tasks, _ := e.opts.Store.TasksForRun(ctx, e.run.ID)
	all, waiting, pending := true, false, false
	for _, task := range tasks {
		switch task.Status {
		case StatusMerged, "done":
			continue
		case StatusReview, StatusApproved, StatusReviewing:
			waiting = true
		case StatusQueued, StatusInterrupted:
			pending = true
		}
		all = false
	}
	switch {
	case all:
		return RunCompleted
	case waiting:
		return RunWaiting
	case pending && !budget:
		return RunBudget
	default:
		return RunBlocked
	}
}

func (e *Engine) budgetLeft() (bool, error) {
	ctx, cancel := saveContext()
	defer cancel()
	if max := e.opts.Config.MaxTurns; max > 0 {
		turns, err := e.opts.Store.CountEvents(ctx, e.run.ID, "turn")
		if err != nil {
			return false, err
		}
		if turns >= max {
			return false, nil
		}
	}
	if max := e.opts.Config.MaxTokens; max > 0 {
		usage, err := e.opts.Store.RunUsage(ctx, e.run.ID)
		if err != nil {
			return false, err
		}
		if usage.TotalTokens >= max {
			return false, nil
		}
	}
	return true, nil
}

// save persists a transition, logs it, and notifies the UI. Callers hold mu or
// own the task exclusively.
func (e *Engine) save(ctx context.Context, task store.Task, agent, message string) bool {
	if err := e.opts.Store.SaveTask(ctx, task); err != nil {
		return false
	}
	text := task.Status
	if message != "" {
		text += ": " + message
	}
	e.log(ctx, task.ID, agent, "status", text)
	return true
}

func (e *Engine) log(ctx context.Context, taskID, agent, kind, message string) {
	_ = e.opts.Store.AddEvent(ctx, store.Event{RunID: e.run.ID, TaskID: taskID, Agent: agent, Kind: kind, Message: message})
	if e.opts.OnChange != nil {
		e.opts.OnChange()
	}
}

func (e *Engine) setRunStatus(status string) {
	ctx, cancel := saveContext()
	defer cancel()
	_ = e.opts.Store.SetRunStatus(ctx, e.run.ID, status)
	e.log(ctx, "", "", "run", status)
}

func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// saveContext outlives a cancelled run so outcomes are still recorded.
func saveContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// planID strips the run prefix that planning adds to task IDs.
func planID(id string) string {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == ':' {
			return id[i+1:]
		}
	}
	return id
}
