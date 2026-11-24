package planning

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	// Rules overrides the project's rules, for a caller that has already
	// loaded them (and may have left out untrusted project rules). Without
	// it, project rules the user has not accepted are left out.
	Rules *rules.Set
	// OnPermission, when set, answers the lead's requests instead of Permission.
	OnPermission func(*acpdriver.PermissionRequest)
	// Conversation is the chat so far, oldest first. The planner is a fresh
	// session, so this is how it plans what was discussed; with it, Goal may
	// be empty.
	Conversation string
}

type Result struct {
	Run         store.Run
	Lead        config.Agent
	Agents      []store.RunAgent
	Tasks       []store.Task
	Plan        plan.Plan
	Integration worktree.Worktree
	Root        string
	Created     []string
	Uncommitted int // changed files the run cannot see: workers start from the last commit
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
	if agent := acpdriver.DriverName(opts.Agent); agent != "" && agent != lead.Driver {
		lead.Driver, lead.Model = agent, "" // the configured model belongs to the old driver
	}
	if opts.Model != "" {
		lead.Model = opts.Model
	}
	if lead.Driver == "opencode" && lead.Model == "" {
		lead.Model = "opencode/deepseek-v4.1-flash-free"
	}

	state, err := store.Open(config.StatePath())
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	defer state.Close()

	var loaded rules.Set
	if opts.Rules != nil {
		loaded = *opts.Rules
	} else {
		if loaded, err = rules.Load(cfg.Rules); err != nil {
			return nil, err
		}
		loaded = loaded.Trust(func() (string, error) { return state.TrustedRules(ctx, project) })
		if loaded.Untrusted() {
			fmt.Fprintln(os.Stderr, "plan: "+rules.UntrustedNote)
		}
	}
	ruleSet, err := rules.Scoped(loaded, "lead", lead.Name, "")
	if err != nil {
		return nil, err
	}

	// Check the repository before the lead spends minutes planning: every
	// worker gets its own worktree branched from HEAD.
	manager, err := worktree.New(ctx, project, config.StateDir())
	if err != nil {
		return nil, fmt.Errorf("this folder is not a git repository. Each worker gets its own git worktree, so run `git init` and make a first commit here, or start Switch inside a repository")
	}
	base, err := manager.Head(ctx)
	if err != nil {
		return nil, fmt.Errorf("this repository has no commits yet. Make a first commit so workers have a base to branch from")
	}
	if _, err := worktree.Git(ctx, manager.Repo, "show-ref", "--verify", "--quiet", "refs/heads/switch"); err == nil {
		return nil, fmt.Errorf("a local branch named \"switch\" blocks the run branches Switch creates (switch/<run>/...); rename it first, for example: git branch -m switch switch-old")
	}
	// The lead plans in the working tree, but workers branch from HEAD.
	// Switch reads its own .switch files from here, so they do not count.
	status, _ := worktree.Git(ctx, project, "--no-optional-locks", "status", "--porcelain", "--", ":/", ":!.switch")

	// Each JSON object in the output gets the worker check too, so a kept
	// plan loads whichever of its copies the user fixed.
	check := func(output string) (plan.Plan, error) {
		return plan.Parse(output, func(planned plan.Plan) error { return checkWorkers(planned, fleet, lead.Name) })
	}
	var planned plan.Plan
	if opts.File != "" {
		data, err := os.ReadFile(opts.File)
		if err != nil {
			return nil, err
		}
		if planned, err = check(string(data)); err != nil {
			return nil, err
		}
	} else {
		goal, conversation := strings.TrimSpace(opts.Goal), strings.TrimSpace(opts.Conversation)
		if goal == "" && conversation == "" {
			return nil, fmt.Errorf("provide a goal or a plan file")
		}
		planCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
		planned, err = runLead(planCtx, lead, project, buildPrompt(goal, conversation, fleet, ruleSet, lead.Name), opts.Permission, opts.OnPermission, opts.AgentLogs, opts.OnText, check)
		cancel()
		if err != nil {
			return nil, err
		}
	}

	// The plan is final: a cancel from here on would leave a half-made run.
	ctx = context.WithoutCancel(ctx)
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
		IntegrationPath:   integration.Path,
		Status:            "planned",
	}
	result := &Result{
		Run:         run,
		Lead:        lead,
		Plan:        planned,
		Integration: integration,
		Root:        manager.Root(runID),
		Created:     created,
		Uncommitted: len(slices.Collect(strings.Lines(status))),
	}

	// Tasks get their own worktrees when dispatched; agents only need records.
	for _, agent := range fleet {
		role := agent.Role
		if agent.Name == lead.Name {
			agent = lead
			role = "lead" // the run's lead plans and reviews; it never takes tasks
		} else if role == "lead" {
			role = "standby" // only the run's lead reviews and takes default handoffs
		}
		result.Agents = append(result.Agents, store.RunAgent{
			ID:        uuid.NewString(),
			RunID:     runID,
			Name:      agent.Name,
			Role:      role,
			Driver:    agent.Driver,
			Model:     agent.Model,
			Status:    "idle",
			CreatedAt: time.Now().UTC(),
		})
	}

	for _, task := range planned.Tasks {
		deps := make([]string, 0, len(task.Deps))
		for _, dep := range task.Deps {
			deps = append(deps, runID+":"+dep) // Validate checked every dep names a task
		}
		result.Tasks = append(result.Tasks, store.Task{
			ID:         runID + ":" + task.ID,
			RunID:      runID,
			Title:      task.Title,
			Brief:      task.Brief,
			Role:       task.Role,
			Deps:       deps,
			Files:      task.Files,
			Acceptance: task.Acceptance,
			Verify:     task.Verify,
			Status:     "queued",
			Assignee:   task.Agent, // a pinned task goes only to its agent
			CreatedAt:  time.Now().UTC(),
		})
	}

	if err := state.CreatePlannedRun(ctx, run, result.Agents, result.Tasks); err != nil {
		// Leave nothing behind: a run that is not saved has no use for its checkout.
		_ = manager.Remove(ctx, integration.Path)
		_ = os.Remove(result.Root)
		_, _ = worktree.Git(ctx, manager.Repo, "branch", "-D", branch)
		return nil, fmt.Errorf("save run: %w", err)
	}
	return result, nil
}

func buildPrompt(goal, conversation string, fleet []config.Agent, ruleSet rules.Set, leadName string) string {
	var b strings.Builder
	b.WriteString("You are the lead agent for a Switch run. Plan the work; do not implement it.\n\n")
	if goal == "" {
		goal = "What the user and the lead agreed on in the conversation below."
	}
	b.WriteString("Goal:\n")
	b.WriteString(goal)
	if conversation != "" {
		b.WriteString("\n\nConversation so far between the user and the lead, oldest first:\n\n")
		b.WriteString(conversation)
	}
	b.WriteString("\n\nAvailable agents:\n")
	for _, agent := range fleet {
		if agent.Name == leadName || agent.Role == "lead" {
			continue // only workers take tasks
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
- To give a task to one agent above, add "agent": "<its name>"; otherwise any agent with the task's role takes it.
- Ids name git branches: letters, digits, dots, underscores and dashes, at most 40 characters.
`)
	return b.String()
}

func runLead(ctx context.Context, lead config.Agent, project, prompt string, mode acpdriver.PermissionMode, onPermission func(*acpdriver.PermissionRequest), logs io.Writer, onText func(string), check func(string) (plan.Plan, error)) (plan.Plan, error) {
	session, err := acpdriver.Open(ctx, acpdriver.Spec{
		Agent:     lead.Driver,
		Cwd:       project,
		Model:     lead.Model,
		AgentLogs: logs,
		Ephemeral: true, // nothing resumes a planning session
	})
	if err != nil {
		return plan.Plan{}, err
	}
	defer session.Close(context.Background())
	turn := func(prompt string) (string, error) {
		return session.RunTurn(ctx, prompt, acpdriver.TurnOptions{Permission: mode, OnPermission: onPermission, OnText: onText})
	}
	return askLead(turn, prompt, check, onText)
}

// askLead runs the planning turn. A plan that check rejects gets one more
// turn, in the same session, to fix the exact error; a plan that still fails
// is kept on disk, so minutes of the lead's work are not lost.
func askLead(turn func(string) (string, error), prompt string, check func(string) (plan.Plan, error), onText func(string)) (plan.Plan, error) {
	output, err := turn(prompt)
	if err != nil {
		return plan.Plan{}, err
	}
	planned, err := check(output)
	if err == nil {
		return planned, nil
	}
	// The error may say how the user fixes the config; the lead fixes only its plan.
	fix := fmt.Sprintf("Switch rejected your plan: %v\nChange the plan, not any file. Return ONLY the corrected JSON object, in the same shape.", err)
	if onText != nil {
		onText("\n" + fix + "\n")
	}
	fixed, turnErr := turn(fix)
	if turnErr != nil {
		return plan.Plan{}, keep(output, fmt.Errorf("%w (the turn to fix it failed: %v)", err, turnErr))
	}
	if planned, err = check(fixed); err == nil {
		return planned, nil
	}
	return plan.Plan{}, keep(output+"\n\n"+fix+"\n\n"+fixed, err)
}

// keep saves the lead's rejected output under the state directory and says
// where in the error.
func keep(output string, rejected error) error {
	dir := filepath.Join(config.StateDir(), "plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.Join(rejected, err)
	}
	file, err := os.CreateTemp(dir, "rejected-*.txt")
	if err != nil {
		return errors.Join(rejected, err)
	}
	_, err = fmt.Fprintf(file, "Switch rejected this plan: %v\nFix the JSON below, then plan from this file with --file.\n\n%s\n", rejected, output)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.Join(rejected, err)
	}
	return fmt.Errorf("the lead's plan was rejected: %w\nit is kept in %s: fix it, then plan from it with --file", rejected, file.Name())
}

// checkWorkers rejects a plan with a role no worker can take. The lead never
// takes tasks, so a role covered only by the lead would block the run.
func checkWorkers(planned plan.Plan, fleet []config.Agent, leadName string) error {
	roles := map[string]bool{}
	var workers []string
	for _, agent := range fleet {
		if agent.Name != leadName && agent.Role != "lead" {
			roles[agent.Role] = true
			workers = append(workers, agent.Name)
		}
	}
	for _, task := range planned.Tasks {
		// A pinned task still needs its role covered: /retry hands it to any worker of that role.
		if task.Agent != "" && !slices.Contains(workers, task.Agent) {
			return fmt.Errorf("task %s is given to %q, which is not a worker; the workers are %s", task.ID, task.Agent, strings.Join(workers, ", "))
		}
		if !roles[task.Role] {
			return fmt.Errorf("task %s needs a %q worker, but no configured agent other than the lead (%s) has that role; add one to .switch/config.toml or pick another lead", task.ID, task.Role, leadName)
		}
	}
	return nil
}
