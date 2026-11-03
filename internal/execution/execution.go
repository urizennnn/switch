// Package execution dispatches queued run tasks to worker agents.
//
// Each run agent owns one worktree, so an agent works on one task at a time;
// tasks for the same agent are serialized rather than given their own
// worktrees.
package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

// Task statuses written by this package.
const (
	StatusQueued      = "queued"
	StatusRunning     = "running"
	StatusImplemented = "implemented" // worker finished; waiting for verification
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
)

var ErrNoReadyTask = errors.New("no task is ready: every queued task is blocked by dependencies or has no idle worker for its role")

type Options struct {
	Store *store.Store
	RunID string
	Rules rules.Set
	// Permission answers worker requests when OnPermission is nil.
	Permission   acpdriver.PermissionMode
	OnPermission func(*acpdriver.PermissionRequest)
	OnText       func(string)
	AgentLogs    io.Writer
}

type Result struct {
	Task   store.Task
	Agent  store.RunAgent
	Output string
}

// Next picks the first runnable task, in plan order, whose dependencies are
// done and whose role has an idle worker.
func Next(tasks []store.Task, agents []store.RunAgent) (store.Task, store.RunAgent, bool) {
	finished := make(map[string]bool, len(tasks))
	busy := make(map[string]bool)
	for _, task := range tasks {
		switch task.Status {
		case "done", "approved", "merged":
			finished[task.ID] = true
		case StatusRunning:
			busy[task.Assignee] = true
		}
	}
	for _, task := range tasks {
		if task.Status != StatusQueued && task.Status != StatusInterrupted {
			continue
		}
		if !depsFinished(task, finished) {
			continue
		}
		for _, agent := range agents {
			if agent.Role == task.Role && agent.Role != "lead" && !busy[agent.Name] {
				return task, agent, true
			}
		}
	}
	return store.Task{}, store.RunAgent{}, false
}

func depsFinished(task store.Task, finished map[string]bool) bool {
	for _, dep := range task.Deps {
		if !finished[dep] {
			return false
		}
	}
	return true
}

// Recover marks tasks left running by a crashed process as interrupted so
// Next can pick them up again.
// shortcut: assumes one Switch process drives a run; add run ownership when
// the run actor lands.
func Recover(ctx context.Context, state *store.Store, runID string) error {
	tasks, err := state.TasksForRun(ctx, runID)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.Status == StatusRunning {
			task.Status = StatusInterrupted
			if err := state.SaveTask(ctx, task); err != nil {
				return err
			}
		}
	}
	agents, err := state.RunAgents(ctx, runID)
	if err != nil {
		return err
	}
	for _, agent := range agents {
		if agent.Status == "busy" {
			agent.Status = "idle"
			if err := state.SaveRunAgent(ctx, agent); err != nil {
				return err
			}
		}
	}
	return nil
}

// RunNext runs the next ready task of the run for one worker turn.
func RunNext(ctx context.Context, opts Options) (*Result, error) {
	if err := Recover(ctx, opts.Store, opts.RunID); err != nil {
		return nil, err
	}
	tasks, err := opts.Store.TasksForRun(ctx, opts.RunID)
	if err != nil {
		return nil, err
	}
	agents, err := opts.Store.RunAgents(ctx, opts.RunID)
	if err != nil {
		return nil, err
	}
	task, agent, ok := Next(tasks, agents)
	if !ok {
		return nil, ErrNoReadyTask
	}
	return Run(ctx, opts, task, agent)
}

// Run executes one task in the agent's worktree and records the outcome. A
// cancelled context marks the task interrupted and stops the adapter.
func Run(ctx context.Context, opts Options, task store.Task, agent store.RunAgent) (*Result, error) {
	task.Status = StatusRunning
	task.Assignee = agent.Name
	task.Worktree = agent.Worktree
	task.Branch = agent.Branch
	task.Attempts++
	if err := opts.Store.SaveTask(ctx, task); err != nil {
		return nil, err
	}
	agent.Status = "busy"
	if err := opts.Store.SaveRunAgent(ctx, agent); err != nil {
		return nil, err
	}

	result := &Result{Task: task, Agent: agent}
	output, err := runWorker(ctx, opts, task, &agent)
	result.Output = output

	switch {
	case ctx.Err() != nil:
		task.Status = StatusInterrupted
		err = ctx.Err()
	case err != nil:
		task.Status = StatusFailed
	default:
		task.Status = StatusImplemented
	}
	agent.Status = "idle"

	// The run context may be cancelled; still record the outcome.
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if saveErr := errors.Join(opts.Store.SaveTask(saveCtx, task), opts.Store.SaveRunAgent(saveCtx, agent)); saveErr != nil {
		err = errors.Join(err, saveErr)
	}
	result.Task = task
	result.Agent = agent
	return result, err
}

func runWorker(ctx context.Context, opts Options, task store.Task, agent *store.RunAgent) (string, error) {
	spec := acpdriver.Spec{
		Agent:     agent.Driver,
		Cwd:       agent.Worktree,
		Model:     agent.Model,
		SessionID: agent.SessionID,
		AgentLogs: opts.AgentLogs,
	}
	session, err := acpdriver.Open(ctx, spec)
	if err != nil && spec.SessionID != "" {
		spec.SessionID = ""
		session, err = acpdriver.Open(ctx, spec)
	}
	if err != nil {
		return "", fmt.Errorf("start %s: %w", agent.Name, err)
	}
	defer session.Close(context.Background())

	agent.SessionID = session.ID
	if err := opts.Store.SaveRunAgent(ctx, *agent); err != nil {
		return "", err
	}

	return session.RunTurn(ctx, Prompt(task, *agent, opts.Rules), acpdriver.TurnOptions{
		Permission:   opts.Permission,
		OnText:       opts.OnText,
		OnPermission: opts.OnPermission,
	})
}

// Prompt builds the worker brief for one task.
func Prompt(task store.Task, agent store.RunAgent, ruleSet rules.Set) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a %s agent in a Switch run.\n", agent.Name, agent.Role)
	fmt.Fprintf(&b, "Work only inside this git worktree: %s (branch %s).\n\n", agent.Worktree, agent.Branch)
	fmt.Fprintf(&b, "Task: %s\n\n%s\n", task.Title, task.Brief)
	writeList(&b, "Files in scope", task.Files)
	writeList(&b, "Acceptance criteria", task.Acceptance)
	writeList(&b, "Verification commands (Switch runs these after you finish; run them yourself first)", task.Verify)
	if !ruleSet.Empty() {
		b.WriteString("\nOperating rules:\n")
		b.WriteString(ruleSet.Text())
		b.WriteString("\n")
	}
	b.WriteString("\nDo not commit, push, merge, or switch branches; Switch integrates approved work.\n")
	b.WriteString("When you are done, reply with a short summary of what you changed and anything left open.\n")
	return b.String()
}

func writeList(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s:\n", title)
	for _, item := range items {
		fmt.Fprintf(b, "- %s\n", item)
	}
}
