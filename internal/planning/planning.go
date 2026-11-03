package planning

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/plan"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
	"github.com/urizennnn/switch/internal/worktree"
)

type Options struct {
	Project    string
	Goal       string
	File       string
	Agent      string
	Model      string
	Lead       *config.Agent
	Timeout    time.Duration
	Permission acpdriver.PermissionMode
	AgentLogs  io.Writer
	OnText     func(string)
}

type AgentWorktree struct {
	Agent    store.RunAgent
	Worktree worktree.Worktree
}

type Result struct {
	Run         store.Run
	Lead        config.Agent
	Agents      []AgentWorktree
	Tasks       []store.Task
	Plan        plan.Plan
	Integration worktree.Worktree
	Root        string
	Created     []string
}

// Execute asks the lead for a plan (or imports one), creates a run, and
// provisions the integration and per-agent worktrees.
func Execute(ctx context.Context, opts Options) (*Result, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Minute
	}
	if opts.Permission == "" {
		opts.Permission = acpdriver.PermissionDeny
	}

	project, err := filepath.Abs(opts.Project)
	if err != nil {
		return nil, err
	}
	cfg, created, err := config.LoadOrInit(project)
	if err != nil {
		return nil, err
	}

	fleet := cfg.Fleet()
	var lead config.Agent
	if opts.Lead != nil {
		lead = *opts.Lead
	} else {
		for _, candidate := range fleet {
			if candidate.Role == "lead" {
				lead = candidate
				break
			}
		}
	}
	if lead.Name == "" {
		return nil, fmt.Errorf("no agent with role \"lead\" configured")
	}
	if opts.Agent != "" {
		lead.Driver = opts.Agent
	}
	if opts.Model != "" {
		lead.Model = opts.Model
	}
	if opts.Agent == "" && opts.Model == "" && lead.Driver == "opencode" && lead.Model == "" {
		lead.Model = "opencode/deepseek-v4.1-flash-free"
	}

	ruleSet, err := rules.Load(cfg.Rules)
	if err != nil {
		return nil, err
	}

	var planned plan.Plan
	if opts.File != "" {
		data, err := os.ReadFile(opts.File)
		if err != nil {
			return nil, err
		}
		planned, err = plan.Parse(string(data))
		if err != nil {
			return nil, err
		}
	} else {
		goal := strings.TrimSpace(opts.Goal)
		if goal == "" {
			return nil, fmt.Errorf("provide a goal or a plan file")
		}
		planCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
		output, err := runLead(planCtx, lead, project, buildPrompt(goal, fleet, ruleSet, lead.Name), opts.Permission, opts.AgentLogs, opts.OnText)
		cancel()
		if err != nil {
			return nil, err
		}
		planned, err = plan.Parse(output)
		if err != nil {
			return nil, fmt.Errorf("parse lead output: %w", err)
		}
	}

	state, err := store.Open(config.StatePath())
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	defer state.Close()

	manager, err := worktree.New(ctx, project, config.StateDir())
	if err != nil {
		return nil, err
	}
	base, err := manager.Head(ctx)
	if err != nil {
		return nil, err
	}

	runID := uuid.NewString()
	integration, branch, err := manager.Integration(ctx, runID, base)
	if err != nil {
		return nil, err
	}

	run := store.Run{
		ID:                runID,
		Project:           project,
		Goal:              planned.Goal,
		BaseRef:           base,
		IntegrationBranch: branch,
		Status:            "planned",
	}
	if err := state.CreateRun(ctx, run); err != nil {
		return nil, fmt.Errorf("save run: %w", err)
	}

	result := &Result{
		Run:         run,
		Lead:        lead,
		Plan:        planned,
		Integration: integration,
		Root:        manager.Root(runID),
		Created:     created,
	}

	for _, agent := range fleet {
		wt, err := manager.Create(ctx, runID, agent.Name, base)
		if err != nil {
			return nil, fmt.Errorf("create worktree for %s: %w", agent.Name, err)
		}
		role := agent.Role
		if agent.Name == lead.Name {
			role = "lead" // the run's lead plans and reviews; it never takes tasks
		}
		runAgent := store.RunAgent{
			ID:        uuid.NewString(),
			RunID:     runID,
			Name:      agent.Name,
			Role:      role,
			Driver:    agent.Driver,
			Model:     agent.Model,
			Worktree:  wt.Path,
			Branch:    wt.Branch,
			Status:    "idle",
			CreatedAt: time.Now().UTC(),
		}
		if err := state.SaveRunAgent(ctx, runAgent); err != nil {
			return nil, fmt.Errorf("save agent %s: %w", agent.Name, err)
		}
		result.Agents = append(result.Agents, AgentWorktree{Agent: runAgent, Worktree: wt})
	}

	taskIDs := make(map[string]string, len(planned.Tasks))
	for _, task := range planned.Tasks {
		taskIDs[task.ID] = runID + ":" + task.ID
	}
	for _, task := range planned.Tasks {
		deps := make([]string, 0, len(task.Deps))
		for _, dep := range task.Deps {
			if id, ok := taskIDs[dep]; ok {
				deps = append(deps, id)
			}
		}
		stored := store.Task{
			ID:         taskIDs[task.ID],
			RunID:      runID,
			Title:      task.Title,
			Brief:      task.Brief,
			Role:       task.Role,
			Deps:       deps,
			Files:      task.Files,
			Acceptance: task.Acceptance,
			Verify:     task.Verify,
			Status:     "queued",
			CreatedAt:  time.Now().UTC(),
		}
		if err := state.SaveTask(ctx, stored); err != nil {
			return nil, fmt.Errorf("save task %s: %w", task.ID, err)
		}
		result.Tasks = append(result.Tasks, stored)
	}

	return result, nil
}

func buildPrompt(goal string, fleet []config.Agent, ruleSet rules.Set, leadName string) string {
	var b strings.Builder
	b.WriteString("You are the lead agent for a Switch run. Plan the work; do not implement it.\n\n")
	b.WriteString("Goal:\n")
	b.WriteString(goal)
	b.WriteString("\n\nAvailable agents:\n")
	for _, agent := range fleet {
		if agent.Name == leadName {
			continue
		}
		fmt.Fprintf(&b, "- %s (role=%s, driver=%s)\n", agent.Name, agent.Role, agent.Driver)
	}
	if !ruleSet.Empty() {
		b.WriteString("\nOperating rules:\n")
		b.WriteString(ruleSet.Text())
		b.WriteString("\n")
	}
	b.WriteString(`
Return ONLY a JSON object in this shape:
{
  "goal": "restated goal",
  "tasks": [
    {
      "id": "short-kebab-id",
      "title": "short task title",
      "brief": "detailed instructions for the worker",
      "role": "builder",
      "deps": ["another-task-id"],
      "files": ["path/or/glob"],
      "acceptance": ["observable completion criterion"],
      "verify": ["command to run"]
    }
  ]
}

Rules for the plan:
- Use 2-8 tasks unless the goal is genuinely small.
- Use only roles listed above.
- Make dependencies explicit and avoid cycles.
- Keep file scopes narrow so tasks can run in parallel where safe.
- Every task needs at least one acceptance criterion.
- Do not create tasks for the lead.
`)
	return b.String()
}

func runLead(ctx context.Context, lead config.Agent, project, prompt string, mode acpdriver.PermissionMode, logs io.Writer, onText func(string)) (string, error) {
	session, err := acpdriver.Open(ctx, acpdriver.Spec{
		Agent:     lead.Driver,
		Cwd:       project,
		Model:     lead.Model,
		AgentLogs: logs,
	})
	if err != nil {
		return "", err
	}
	defer session.Close(context.Background())
	return session.RunTurn(ctx, prompt, acpdriver.TurnOptions{Permission: mode, OnText: onText})
}
