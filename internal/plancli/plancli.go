package plancli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
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

// Run executes `switch plan` and returns a process exit code.
func Run(args []string) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	agent := fs.String("agent", "", "lead driver override")
	model := fs.String("model", "", "lead model override")
	cwd := fs.String("cwd", ".", "project directory")
	file := fs.String("file", "", "import a plan JSON file instead of asking the lead")
	timeout := fs.Duration("timeout", 10*time.Minute, "lead planning timeout")
	permission := fs.String("permission", "deny", "lead permission mode: ask, allow, or deny")
	agentLogs := fs.Bool("agent-logs", false, "stream lead adapter stderr")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	mode, err := acpdriver.ParsePermissionMode(*permission)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plan: %v\n", err)
		return 2
	}

	project, err := filepath.Abs(*cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plan: %v\n", err)
		return 1
	}

	cfg, created, err := config.LoadOrInit(project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plan: %v\n", err)
		return 1
	}
	if len(created) > 0 {
		fmt.Printf("initialized switch in %s\n", project)
	}

	fleet := cfg.Fleet()
	var lead config.Agent
	for _, candidate := range fleet {
		if candidate.Role == "lead" {
			lead = candidate
			break
		}
	}
	if lead.Name == "" {
		fmt.Fprintln(os.Stderr, "plan: no agent with role \"lead\" configured")
		return 1
	}
	if *agent != "" {
		lead.Driver = *agent
	}
	if *model != "" {
		lead.Model = *model
	}
	if *agent == "" && *model == "" && lead.Driver == "opencode" && lead.Model == "" {
		lead.Model = "opencode/deepseek-v4.1-flash-free"
	}

	ruleSet, err := rules.Load(cfg.Rules)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plan: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var planned plan.Plan
	if *file != "" {
		data, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "plan: %v\n", err)
			return 1
		}
		planned, err = plan.Parse(string(data))
		if err != nil {
			fmt.Fprintf(os.Stderr, "plan: %v\n", err)
			return 1
		}
	} else {
		goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if goal == "" {
			fmt.Fprintln(os.Stderr, "plan: provide a goal or use --file")
			return 2
		}
		var logs io.Writer
		if *agentLogs {
			logs = os.Stderr
		}
		prompt := buildPrompt(goal, fleet, ruleSet)
		planCtx, cancel := context.WithTimeout(ctx, *timeout)
		output, err := runLead(planCtx, lead, project, prompt, mode, logs)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "plan: %v\n", err)
			return 1
		}
		planned, err = plan.Parse(output)
		if err != nil {
			fmt.Fprintf(os.Stderr, "plan: parse lead output: %v\n", err)
			return 1
		}
	}

	storePath := config.StatePath()
	state, err := store.Open(storePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plan: state: %v\n", err)
		return 1
	}
	defer state.Close()

	manager, err := worktree.New(ctx, project, config.StateDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "plan: %v\n", err)
		return 1
	}
	base, err := manager.Head(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plan: %v\n", err)
		return 1
	}

	runID := uuid.NewString()
	_, branch, err := manager.Integration(ctx, runID, base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plan: %v\n", err)
		return 1
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
		fmt.Fprintf(os.Stderr, "plan: save run: %v\n", err)
		return 1
	}

	fmt.Println()
	fmt.Printf("run:         %s\n", short(runID))
	fmt.Printf("integration: %s\n", branch)
	fmt.Println("agents:")

	for _, agent := range fleet {
		wt, err := manager.Create(ctx, runID, agent.Name, base)
		if err != nil {
			fmt.Fprintf(os.Stderr, "plan: create worktree for %s: %v\n", agent.Name, err)
			return 1
		}
		if err := state.SaveRunAgent(ctx, store.RunAgent{
			ID:        uuid.NewString(),
			RunID:     runID,
			Name:      agent.Name,
			Role:      agent.Role,
			Driver:    agent.Driver,
			Model:     agent.Model,
			Worktree:  wt.Path,
			Branch:    wt.Branch,
			Status:    "idle",
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "plan: save agent %s: %v\n", agent.Name, err)
			return 1
		}
		fmt.Printf("  %-10s %-8s %-10s %s\n", agent.Name, agent.Role, agent.Driver, wt.Path)
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
		if err := state.SaveTask(ctx, store.Task{
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
		}); err != nil {
			fmt.Fprintf(os.Stderr, "plan: save task %s: %v\n", task.ID, err)
			return 1
		}
	}

	fmt.Println("tasks:")
	for _, task := range planned.Tasks {
		deps := "none"
		if len(task.Deps) > 0 {
			deps = strings.Join(task.Deps, ",")
		}
		fmt.Printf("  %-12s %-8s deps=%-12s %s\n", task.ID, task.Role, deps, task.Title)
	}
	fmt.Printf("\nrun saved. worktrees: %s\n", manager.Root(runID))
	fmt.Println("task execution is the next Phase 2 step.")
	return 0
}

func buildPrompt(goal string, fleet []config.Agent, ruleSet rules.Set) string {
	var b strings.Builder
	b.WriteString("You are the lead agent for a Switch run. Plan the work; do not implement it.\n\n")
	b.WriteString("Goal:\n")
	b.WriteString(goal)
	b.WriteString("\n\nAvailable agents:\n")
	for _, agent := range fleet {
		if agent.Role == "lead" {
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

func runLead(ctx context.Context, lead config.Agent, project, prompt string, mode acpdriver.PermissionMode, logs io.Writer) (string, error) {
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

	fmt.Printf("lead: %s (%s)\n\n", lead.Name, lead.Driver)
	done := make(chan error, 1)
	go func() {
		_, err := session.Prompt(ctx, prompt)
		done <- err
	}()

	var output strings.Builder
	for {
		select {
		case event := <-session.Events:
			switch event.Type {
			case acpdriver.EventMessage:
				fmt.Print(event.Text)
				output.WriteString(event.Text)
			case acpdriver.EventToolCall:
				if event.Tool != nil {
					fmt.Fprintf(os.Stderr, "\n[lead tool] %s (%s)\n", event.Tool.Title, event.Tool.Status)
				}
			case acpdriver.EventTurn:
				fmt.Println()
			case acpdriver.EventError:
				if event.Err != nil {
					fmt.Fprintf(os.Stderr, "\nplan: lead error: %v\n", event.Err)
				}
			case acpdriver.EventExit:
				return "", fmt.Errorf("lead adapter exited")
			}
		case request := <-session.Permissions():
			respondPermission(request, mode)
		case err := <-done:
			if err != nil {
				return "", err
			}
			return output.String(), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func respondPermission(request *acpdriver.PermissionRequest, mode acpdriver.PermissionMode) {
	switch mode {
	case acpdriver.PermissionAllow:
		for _, option := range request.Options {
			if strings.HasPrefix(option.Kind, "allow") {
				request.Respond(option.ID)
				return
			}
		}
	case acpdriver.PermissionDeny:
		for _, option := range request.Options {
			if strings.HasPrefix(option.Kind, "reject") {
				request.Respond(option.ID)
				return
			}
		}
	}
	request.Cancel()
}

func short(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
