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
	"path/filepath"
	"slices"
	"strings"
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

// ProposedBy starts the note that blocks a task the chat's lead proposed
// until the user accepts it with /retry. Such a task does not hold the run
// back: once the other tasks merge, the run completes.
const ProposedBy = "proposed by "

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
	// Ephemeral ends the agent session after the turn; worker sessions stay
	// resumable for retries.
	Ephemeral bool
	// Label describes the turn in live views, such as the task title.
	Label string
	// Env adds to the agent's environment.
	Env []string
}

type TurnResult struct {
	Output     string
	SessionID  string
	Usage      acpdriver.UsageInfo
	StopReason string // why the turn ended, such as max_tokens
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
	// Poll rechecks the store this often while waiting, for approvals made
	// by another process (a detached run approved from the TUI).
	Poll time.Duration
	// Permission answers worker requests when OnPermission is nil, for what
	// build mode would ask the user about.
	Permission   acpdriver.PermissionMode
	OnPermission func(*acpdriver.PermissionRequest)
	OnText       func(agent, text string)
	// OnEvent receives each agent's session events, for live views.
	OnEvent func(agent string, event acpdriver.Event)
	// Handoffs names, per agent, who continues its task when it runs out of
	// room; agents without one hand off to the run's lead.
	Handoffs map[string]string
	// Escalations names, per agent, who gets one more attempt at its task
	// when its attempts run out; without one the task is blocked.
	Escalations map[string]string
	// OnChange is called after every task transition.
	OnChange  func()
	AgentLogs io.Writer
	// Turn overrides how agent turns run; tests use a fake.
	Turn TurnFunc
	// Control, when set, is offered to agents as the Switch MCP server.
	Control ControlPlane
	// Headless marks a run driven without a TUI, which `__control stop`
	// may interrupt; a TUI's run is stopped from that TUI.
	Headless bool
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
	Revoke     func() // ends the grant's access when its turn ends
}

type Engine struct {
	opts    Options
	run     store.Run
	manager *worktree.Manager

	mu   sync.Mutex // serializes task transitions; the merge lock serializes merges
	wake chan struct{}

	subMu  sync.Mutex
	subs   map[string]*subagent // by ID, started by this process
	runCtx context.Context      // the active Run's context; subagents end with it

	stopMu    sync.Mutex
	turns     map[string]context.CancelFunc  // worker name -> its running task turn or setup
	live      map[string]liveTurn            // agent -> its running turn, for Steer
	steers    map[[2]string]string           // agent and task -> what Steer could not get into its turn on the task, for its next one
	watchdogs map[string]*time.Timer         // task ID -> no-events watchdog of its worker turn
	asking    map[string]string              // agent -> task whose watchdog waits while it asks for permission
	stopping  map[string]bool                // task IDs the user stopped
	usage     map[string]acpdriver.UsageInfo // context use by session ID, as adapters report it
	sessions  map[string]string              // agent -> session of its latest task turn
	handoffs  map[string]handoff             // task ID -> handoff its worker asked for
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
	if opts.Config.IdleTimeout <= 0 {
		opts.Config.IdleTimeout = defaults.IdleTimeout
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
	e := &Engine{opts: opts, run: run, manager: manager, wake: make(chan struct{}, 1), subs: map[string]*subagent{},
		turns: map[string]context.CancelFunc{}, live: map[string]liveTurn{}, steers: map[[2]string]string{},
		watchdogs: map[string]*time.Timer{}, asking: map[string]string{}, stopping: map[string]bool{},
		usage: map[string]acpdriver.UsageInfo{}, sessions: map[string]string{}, handoffs: map[string]handoff{}}
	if e.opts.Turn == nil {
		e.opts.Turn = e.acpTurn
	}
	return e, nil
}

// Run schedules tasks until none can make progress. It returns the run
// outcome; a cancelled context interrupts running tasks and returns
// RunStopped with the context error, and the run's own checks ([run] verify)
// failing once every task is merged return RunBlocked with their evidence.
func (e *Engine) Run(ctx context.Context) (string, error) {
	lock, err := LockRun(e.opts.StateDir, e.run.ID, e.opts.Headless)
	if err != nil {
		return "", err
	}
	defer lock.Release()
	skipped, err := e.recover()
	if err != nil {
		return "", err
	}
	// Running out of budget interrupts the worker and subagent turns still
	// running, as a stop does, so they resume in a later run. The checks and
	// review of work already done go on.
	turns, interrupt := context.WithCancel(ctx)
	defer interrupt()
	e.subMu.Lock()
	e.runCtx = turns
	e.subMu.Unlock()
	defer func() {
		e.subMu.Lock()
		e.runCtx = nil
		e.subMu.Unlock()
	}()
	e.setRunStatus(RunRunning)

	done := make(chan string, e.opts.Config.MaxConcurrency)
	busy := map[string]bool{}
	for {
		budget, err := e.budgetLeft()
		switch {
		case err != nil && len(busy) == 0:
			return "", err
		case err != nil:
			budget = false // a failed read must not abandon running workers; the next pass reads it again
		case !budget:
			interrupt()
		}
		if budget && len(busy) < e.opts.Config.MaxConcurrency {
			for _, a := range e.dispatch(busy, e.opts.Config.MaxConcurrency-len(busy)) {
				busy[a.agent.Name] = true
				go func(a assignment) {
					e.process(ctx, turns, a.task, a.agent, a.slot)
					done <- a.agent.Name
				}(a)
			}
		}

		if len(busy) == 0 {
			// With no worker of its own busy, a task recover left verifying or
			// approved is stale once the merge lock is free.
			if skipped {
				if s, err := e.recover(); err == nil && !s {
					skipped = false
					continue // dispatch what it recovered
				}
			}
			outcome, stuck := e.outcome(budget)
			if outcome == RunCompleted {
				if outcome, err := e.complete(ctx); outcome != "" {
					return outcome, err
				}
				continue // the user accepted a proposal while the checks ran
			}
			if outcome != RunWaiting || !e.opts.WaitForReview { // otherwise it waits for approvals below
				e.setRunStatus(outcome, stuck...)
				return outcome, nil
			}
		}

		select {
		case name := <-done:
			delete(busy, name)
		case <-e.wake:
		case <-e.pollTick(): // another process may have approved a task others wait for
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
	slot  int // SWITCH_SLOT while the task runs
}

// Ready picks up to limit runnable tasks, in plan order, each with a distinct
// idle worker. A retried task goes back to the worker that attempted it. A
// task whose file scope overlaps work started and not merged yet waits.
// Explain says why the others wait.
//
// A task's slot is its agent's place in the run, from 1: each agent runs one
// task at a time, so no two running tasks share one. Integration checks, one
// at a time, have slot 0.
func Ready(tasks []store.Task, agents []store.RunAgent, busy map[string]bool, limit int) []assignment {
	merged := make(map[string]bool, len(tasks))
	taken := make(map[string]bool, len(busy))
	for name := range busy {
		taken[name] = true
	}
	var started [][]string // file scopes of work started and not merged yet
	for _, task := range tasks {
		switch task.Status {
		case StatusMerged, "done":
			merged[task.ID] = true
		case StatusRunning:
			taken[task.Assignee] = true
			started = append(started, scopePaths(task.Files))
		case StatusVerifying, StatusReviewing, StatusReview, StatusApproved:
			started = append(started, scopePaths(task.Files))
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
		scope := scopePaths(task.Files)
		if !depsMerged(task, merged) || clashes(scope, started) {
			continue
		}
		assigned := task.Assignee != "" && hasAgent(agents, task.Assignee)
		for i, agent := range agents {
			if taken[agent.Name] || !canTake(agent, task, assigned) {
				continue
			}
			taken[agent.Name] = true
			started = append(started, scope)
			out = append(out, assignment{task: task, agent: agent, slot: i + 1})
			break
		}
	}
	return out
}

// canTake reports whether agent may work on task: an assigned task goes back
// to its agent, whatever its role: the worker that attempted it, or whoever
// it was handed off to (the lead too). Others go to a worker of their role.
func canTake(agent store.RunAgent, task store.Task, assigned bool) bool {
	if assigned {
		return agent.Name == task.Assignee
	}
	return agent.Role != "lead" && agent.Role == task.Role
}

// Explain gives, by task ID, the first reason each unfinished task that is
// not under way cannot run now: what it waits for. budget is whether the run
// has turns and tokens left. It follows Ready.
func Explain(tasks []store.Task, agents []store.RunAgent, budget bool) map[string]string {
	status := make(map[string]string, len(tasks))
	busy := map[string]string{} // agent -> the task it works on
	var started []store.Task    // work started and not merged yet
	for _, task := range tasks {
		status[task.ID] = task.Status
		switch task.Status {
		case StatusRunning, StatusVerifying, StatusReviewing:
			busy[task.Assignee] = task.ID
			started = append(started, task)
		case StatusReview, StatusApproved:
			started = append(started, task)
		}
	}
	waits := func(task store.Task) string {
		assigned := task.Assignee != "" && hasAgent(agents, task.Assignee)
		if !assigned && !slices.ContainsFunc(agents, func(a store.RunAgent) bool { return canTake(a, task, false) }) {
			return fmt.Sprintf("no %s worker in this run", task.Role)
		}
		for _, dep := range task.Deps {
			if status[dep] != StatusMerged && status[dep] != "done" {
				return fmt.Sprintf("waits for %s, which is %s", planID(dep), status[dep])
			}
		}
		if !budget {
			return "the run budget (max_turns or max_tokens) is used up"
		}
		for _, other := range started {
			if clashes(scopePaths(task.Files), [][]string{scopePaths(other.Files)}) {
				return fmt.Sprintf("waits for %s to merge: their files overlap", planID(other.ID))
			}
		}
		if assigned && busy[task.Assignee] != "" {
			return fmt.Sprintf("waits for %s, busy with %s", task.Assignee, planID(busy[task.Assignee]))
		}
		if !assigned && !slices.ContainsFunc(agents, func(a store.RunAgent) bool { return canTake(a, task, false) && busy[a.Name] == "" }) {
			return fmt.Sprintf("every %s worker is busy", task.Role)
		}
		return "ready to run"
	}
	why := map[string]string{}
	for _, task := range tasks {
		switch task.Status {
		case StatusBlocked:
			why[task.ID] = "blocked: " + firstLine(task.LastError)
		case StatusReview:
			why[task.ID] = "waits for your review"
		case StatusQueued, StatusInterrupted:
			why[task.ID] = waits(task)
		}
	}
	return why
}

func depsMerged(task store.Task, merged map[string]bool) bool {
	for _, dep := range task.Deps {
		if !merged[dep] {
			return false
		}
	}
	return true
}

// clashes reports whether a file scope overlaps any of others, so the tasks
// would likely conflict when merged.
func clashes(scope []string, others [][]string) bool {
	for _, other := range others {
		if _, ok := overlap(scope, other); ok {
			return true
		}
	}
	return false
}

// scopePaths turns a task's file scope, paths and globs, into paths for
// overlap: a glob stands for the directory all its matches are in. No scope
// claims nothing.
func scopePaths(files []string) []string {
	paths := make([]string, len(files))
	for i, file := range files {
		if j := strings.IndexAny(file, "*?[{"); j >= 0 {
			file = filepath.Dir(file[:j])
		}
		paths[i] = filepath.Clean(file)
	}
	return paths
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
		if a.task.Status != StatusInterrupted { // a stop, quit or crash cut it short; it resumes
			task.Attempts++
		}
		task.LastError, task.Summary = "", ""
		if e.claim(ctx, task, a.task.Status, a.agent.Name, fmt.Sprintf("attempt %d", task.Attempts)) != nil {
			continue
		}
		a.task = task
		started = append(started, a)
	}
	return started
}

// recover returns tasks a crashed process left mid-flight to a resumable state.
// It reports whether it left any verifying or approved task as it was, because
// the merge lock was held: Run calls it again then, once its own workers are idle.
// shortcut: assumes one Switch process drives a run; add run ownership (a
// lock file next to the control socket) before allowing several.
func (e *Engine) recover() (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ctx, cancel := saveContext()
	defer cancel()
	// An approved task belongs to the approval holding the merge lock, and a
	// verifying one may be a fix by hand that /fix checks under it. With the
	// lock free that approval died, and the integration branch never took its
	// unverified merge, so the user can simply approve again. The lock is taken
	// before the tasks are read, so no approval finishes in between.
	merge, _ := TryLockMerge(e.run.IntegrationPath)
	defer merge.Release()
	tasks, err := e.opts.Store.TasksForRun(ctx, e.run.ID)
	if err != nil {
		return false, err
	}
	skipped := false
	for _, task := range tasks {
		switch task.Status {
		case StatusRunning, StatusVerifying, StatusReviewing:
			if task.Status == StatusVerifying && merge == nil {
				skipped = true
				continue
			}
			task.Status = StatusInterrupted
		case StatusApproved:
			if merge == nil {
				skipped = true
				continue
			}
			task.Status = StatusReview
		default:
			continue
		}
		e.save(ctx, task, task.Assignee, "recovered after restart")
	}
	e.recoverSubagents(ctx)
	return skipped, nil
}

// complete runs the run's own checks once more on a run whose tasks are all
// merged, and records how it ends: completed, blocked when they fail, or
// stopped when ctx ends first. It returns "" when a task came up meanwhile,
// such as a proposal the user accepted, so the run goes on.
func (e *Engine) complete(ctx context.Context) (string, error) {
	err := e.checkRun(ctx)
	switch {
	case ctx.Err() != nil:
		e.setRunStatus(RunStopped)
		return RunStopped, ctx.Err()
	case err != nil:
		e.setRunStatus(RunBlocked, firstLine(err.Error()))
		return RunBlocked, err
	}
	outcome, stuck := e.outcome(true) // the budget words only queued tasks' waits, and a completed run has none
	if outcome != RunCompleted {
		return "", nil
	}
	e.setRunStatus(outcome, stuck...)
	return outcome, nil
}

// outcome is where the run stands once no task is running, and why each
// unfinished task waits, in plan order.
func (e *Engine) outcome(budget bool) (string, []string) {
	ctx, cancel := saveContext()
	defer cancel()
	tasks, _ := e.opts.Store.TasksForRun(ctx, e.run.ID)
	agents, _ := e.opts.Store.RunAgents(ctx, e.run.ID)
	why := Explain(tasks, agents, budget)
	var stuck []string
	all, waiting, pending := true, false, false
	for _, task := range tasks {
		if why[task.ID] != "" {
			stuck = append(stuck, planID(task.ID)+": "+why[task.ID])
		}
		switch task.Status {
		case StatusMerged, "done":
			continue
		case StatusBlocked:
			if strings.HasPrefix(task.LastError, ProposedBy) {
				continue
			}
		case StatusReview, StatusApproved, StatusReviewing, StatusVerifying: // with no worker on it, verifying is a fix by hand
			waiting = true
		case StatusQueued, StatusInterrupted:
			pending = true
		}
		all = false
	}
	switch {
	case all:
		return RunCompleted, stuck
	case waiting:
		return RunWaiting, stuck
	case pending && !budget:
		return RunBudget, stuck
	default:
		return RunBlocked, stuck
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
func (e *Engine) save(ctx context.Context, task store.Task, agent, message string) {
	if err := e.opts.Store.SaveTask(ctx, task); err != nil {
		return
	}
	e.logStatus(ctx, task, agent, message)
}

// claim saves a transition out of from, a status that other engines and
// processes may act on too, unless one of them moved the task first.
func (e *Engine) claim(ctx context.Context, task store.Task, from, agent, message string) error {
	ok, err := e.opts.Store.SaveTaskFrom(ctx, task, from)
	if err == nil && !ok {
		err = fmt.Errorf("task %s is no longer %s; it changed meanwhile", planID(task.ID), from)
	}
	if err != nil {
		return err
	}
	e.logStatus(ctx, task, agent, message)
	return nil
}

func (e *Engine) logStatus(ctx context.Context, task store.Task, agent, message string) {
	text := task.Status
	if message != "" {
		text += ": " + message
	}
	e.log(ctx, task.ID, agent, "status", text)
	if task.Status == StatusReview || task.Status == StatusBlocked {
		e.notify(task.Status, task.ID, task.Title+": "+message)
	}
}

func (e *Engine) log(ctx context.Context, taskID, agent, kind, message string) {
	_ = e.opts.Store.AddEvent(ctx, store.Event{RunID: e.run.ID, TaskID: taskID, Agent: agent, Kind: kind, Message: message})
	if e.opts.OnChange != nil {
		e.opts.OnChange()
	}
}

// setRunStatus records the run's status, and in its log entry why, such as
// what each unfinished task waits for.
func (e *Engine) setRunStatus(status string, why ...string) {
	ctx, cancel := saveContext()
	defer cancel()
	_ = e.opts.Store.SetRunStatus(ctx, e.run.ID, status)
	event := ""
	switch status {
	case RunCompleted:
		event = "completed"
	case RunBlocked, RunBudget: // the budget holds the run until the user raises it
		event = RunBlocked
	}
	if len(why) > 0 {
		status += ": " + strings.Join(why, "; ")
	}
	e.log(ctx, "", "", "run", status)
	if event != "" {
		e.notify(event, "", "run "+status)
	}
}

// pollTick fires after Poll, or never when polling is off.
func (e *Engine) pollTick() <-chan time.Time {
	if e.opts.Poll <= 0 {
		return nil
	}
	return time.After(e.opts.Poll)
}

func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Wake has the run look at its tasks again now, such as after one was added.
func (e *Engine) Wake() { e.signal() }

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
