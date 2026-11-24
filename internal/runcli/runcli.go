// Package runcli is the headless form of /run and the task commands:
// `switch __control run|status|stop|tasks|approve|reject|retry|diff|log|summary|prune`.
// It drives the same engine as the TUI, so both produce the same state.
package runcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/control"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

// Run executes one run subcommand and returns a process exit code.
func Run(command string, args []string) int {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", ".", "project directory")
	runID := fs.String("run", "", "run ID or a unique prefix of it (default: the project's latest run)")
	permission := fs.String("permission", "deny", "run: allow or deny what build mode would ask a worker about")
	agentLogs := fs.Bool("agent-logs", false, "stream adapter stderr")
	detach := fs.Bool("detach", false, "run: keep running in the background after this command exits")
	wait := fs.Bool("wait", false, "run: keep waiting while tasks wait for review, polling for approvals")
	noProjectRules := fs.Bool("no-project-rules", false, "run: leave out project and local rules, as when they are declined in the TUI")
	asJSON := fs.Bool("json", false, "run, status, tasks: print the run and its tasks as JSON")
	all := fs.Bool("all", false, "prune: also delete uncommitted changes and branches whose work was never merged")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	err := run(command, fs.Args(), flags{cwd: *cwd, runID: *runID, permission: *permission, agentLogs: *agentLogs, detach: *detach, wait: *wait, noProjectRules: *noProjectRules, json: *asJSON, all: *all})
	var code exitCode
	if errors.As(err, &code) {
		return int(code)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", command, err)
		return 1
	}
	return 0
}

type flags struct {
	cwd, runID, permission                             string
	agentLogs, detach, wait, noProjectRules, json, all bool
}

// exitCode ends a command that already printed its result with that code.
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit status %d", int(c)) }

// outcomeCodes are the exit codes of `run` for outcomes other than completed,
// so scripts can tell them apart; 130 is the usual code for an interrupt.
var outcomeCodes = map[string]exitCode{execution.RunWaiting: 3, execution.RunBlocked: 4, execution.RunBudget: 5, execution.RunStopped: 130}

func run(command string, args []string, f flags) error {
	runID, permission, agentLogs := f.runID, f.permission, f.agentLogs
	project, err := filepath.Abs(f.cwd)
	if err != nil {
		return err
	}
	project = config.ProjectRoot(project)
	cfg, err := config.Load(project)
	if err != nil {
		return err
	}
	ruleSet, err := rules.Load(cfg.Rules)
	if err != nil {
		return err
	}
	state, err := store.Open(config.StatePath())
	if err != nil {
		return err
	}
	defer state.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ruleSet = ruleSet.Trust(func() (string, error) { return state.TrustedRules(ctx, project) })
	if f.noProjectRules {
		ruleSet = ruleSet.WithoutProject()
	}

	if command == "prune" {
		return prune(ctx, state, project, config.StateDir(), f.all)
	}
	if runID == "" {
		latest, ok, err := state.LatestRun(ctx, project)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("no run for this project; plan one with `switch __control plan`")
		}
		runID = latest.ID
	} else if runID, err = state.ResolveRun(ctx, project, runID); err != nil {
		return err
	}
	switch command {
	case "run", "approve", "reject", "retry":
		// The integration worktree is made with the plan, so only prune removes it.
		run, _, err := state.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if _, err := os.Stat(run.IntegrationPath); run.IntegrationPath != "" && os.IsNotExist(err) {
			return fmt.Errorf("run %s was pruned: its worktrees are gone, and its unmerged branches are kept; plan a new run", shortID(runID))
		}
	}
	if f.json && (command == "status" || command == "tasks") {
		return printJSON(ctx, state, runID)
	}
	opts := execution.Options{Store: state, RunID: runID, Rules: ruleSet, Config: cfg.Run, Handoffs: config.Handoffs(cfg.Fleet()), Escalations: config.Escalations(cfg.Fleet()), Headless: true}

	taskArg := func() (string, error) {
		if len(args) == 0 {
			return "", fmt.Errorf("usage: %s <task>", command)
		}
		return resolveTask(ctx, state, runID, args[0])
	}

	switch command {
	case "run":
		mode, err := acpdriver.ParsePermissionMode(permission)
		if err != nil || mode == acpdriver.PermissionAsk {
			return errors.New("--permission must be allow or deny headless")
		}
		if ruleSet.Untrusted() {
			fmt.Fprintf(os.Stderr, "%s: %s\n", command, rules.UntrustedNote)
		}
		human := io.Writer(os.Stdout)
		if f.json {
			human = os.Stderr // stdout carries only the JSON
		}
		if f.detach {
			pid, logPath, err := Detach(project, config.StateDir(), runID, string(mode), f.noProjectRules)
			if err != nil {
				return err
			}
			fmt.Fprintf(human, "run %s is running in the background (pid %d)\nlog: %s\nstatus: switch __control status · stop: switch __control stop\n", shortID(runID), pid, logPath)
			if f.json {
				return printJSON(ctx, state, runID)
			}
			return nil
		}
		opts.Permission = mode
		if f.wait {
			opts.WaitForReview = true
			opts.Poll = 2 * time.Second
		}
		if agentLogs {
			opts.AgentLogs = os.Stderr
		}
		printer := &linePrinter{out: human, partial: map[string]string{}}
		opts.OnText = printer.write
		server, err := control.Listen(control.Options{Store: state, RunID: runID, StateDir: config.StateDir(), OnText: printer.write})
		if err != nil {
			return err
		}
		defer server.Close()
		opts.Control = server
		engine, err := execution.New(ctx, opts)
		if err != nil {
			return err
		}
		server.SetSubagents(engine)
		outcome, err := engine.Run(ctx)
		printer.flush()
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		ctx = context.WithoutCancel(ctx) // a stopped run is still reported
		fmt.Fprintf(human, "\nrun %s: %s\n", shortID(runID), outcome)
		_ = printWaits(ctx, human, state, runID, outcome != execution.RunBudget)
		switch outcome {
		case execution.RunWaiting:
			fmt.Fprintln(human, "tasks are waiting for review: `switch __control tasks`, then approve or reject them")
		case execution.RunCompleted:
			reportSummary(ctx, state, command, runID, human)
		}
		if f.json {
			if err := printJSON(ctx, state, runID); err != nil {
				return err
			}
		}
		if code, ok := outcomeCodes[outcome]; ok {
			return code
		}
		return nil

	case "tasks":
		tasks, err := state.TasksForRun(ctx, runID)
		if err != nil {
			return err
		}
		subagents, err := state.SubagentsForRun(ctx, runID)
		if err != nil {
			return err
		}
		for _, task := range tasks {
			fmt.Printf("%-16s %-11s %-10s attempts=%d  %s\n", planID(task.ID), task.Status, orDash(task.Assignee), task.Attempts, task.Title)
			if task.LastError != "" {
				fmt.Printf("%16s last error: %s\n", "", firstLine(task.LastError))
			}
			for _, sa := range subagents {
				if sa.TaskID == task.ID {
					fmt.Printf("%16s └ %-12s %-11s %s\n", "", sa.Name, sa.Status, firstLine(sa.Brief))
				}
			}
		}
		return nil

	case "status":
		run, _, err := state.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		tasks, err := state.TasksForRun(ctx, runID)
		if err != nil {
			return err
		}
		counts := map[string]int{}
		for _, task := range tasks {
			counts[task.Status]++
		}
		owner := "not running"
		if pid, ok := execution.RunOwner(config.StateDir(), runID); ok {
			owner = fmt.Sprintf("driven by pid %d", pid)
		}
		fmt.Printf("run %s  %s  (%s)\n", shortID(runID), run.Status, owner)
		fmt.Printf("goal: %s\nintegration: %s\n", run.Goal, run.IntegrationBranch)
		var parts []string
		for _, status := range []string{"queued", "interrupted", "running", "verifying", "reviewing", "review", "approved", "merged", "blocked"} {
			if counts[status] > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", status, counts[status]))
			}
		}
		fmt.Printf("tasks: %s\n", strings.Join(parts, " · "))
		return printWaits(ctx, os.Stdout, state, runID, run.Status != execution.RunBudget)

	case "stop":
		if err := Stop(config.StateDir(), runID); err != nil {
			return err
		}
		fmt.Printf("run %s stopped\n", shortID(runID))
		return nil

	case "summary":
		text, err := Summary(ctx, state, runID)
		if err != nil {
			return err
		}
		fmt.Print(text)
		return nil

	case "log":
		events, err := state.Events(ctx, runID, 200)
		if err != nil {
			return err
		}
		for _, e := range events {
			fmt.Printf("%s  %-8s %-14s %-10s %s\n", e.CreatedAt.Local().Format("15:04:05"), e.Kind, planID(e.TaskID), e.Agent, firstLine(e.Message))
		}
		return nil

	case "approve", "reject", "retry", "diff":
		id, err := taskArg()
		if err != nil {
			return err
		}
		engine, err := execution.New(ctx, opts)
		if err != nil {
			return err
		}
		switch command {
		case "approve":
			err = engine.Approve(ctx, id)
		case "reject":
			err = engine.Reject(ctx, id, strings.Join(args[1:], " "))
		case "retry":
			err = engine.Retry(ctx, id)
		case "diff":
			var diff string
			if diff, err = engine.Diff(ctx, id); err == nil {
				fmt.Println(diff)
			}
			return err
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s done\n", planID(id), command)
		if command != "approve" {
			return nil
		}
		// Approving the last task completes the run once its checks pass;
		// without this only another `run` would write its PR description.
		run, ok, err := state.GetRun(ctx, runID)
		if err != nil || !ok || run.Status != execution.RunCompleted {
			if note := ChecksFailed(ctx, state, run); note != "" {
				fmt.Println(strings.TrimSpace(note))
			}
			return err
		}
		reportSummary(ctx, state, command, runID, os.Stdout)
		return nil
	}
	return fmt.Errorf("unknown command %q", command)
}

// printWaits prints what each unfinished task of the run waits for. budget
// is whether the run has turns and tokens left.
func printWaits(ctx context.Context, w io.Writer, state *store.Store, runID string, budget bool) error {
	tasks, err := state.TasksForRun(ctx, runID)
	if err != nil {
		return err
	}
	agents, err := state.RunAgents(ctx, runID)
	if err != nil {
		return err
	}
	why := execution.Explain(tasks, agents, budget)
	for _, task := range tasks {
		if why[task.ID] != "" {
			fmt.Fprintf(w, "  %-16s %s\n", planID(task.ID), why[task.ID])
		}
	}
	return nil
}

// printJSON prints the --json form of run, status and tasks.
func printJSON(ctx context.Context, state *store.Store, runID string) error {
	run, _, err := state.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	tasks, err := state.TasksForRun(ctx, runID)
	if err != nil {
		return err
	}
	type taskJSON struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Status    string `json:"status"`
		Assignee  string `json:"assignee"`
		Attempts  int    `json:"attempts"`
		LastError string `json:"last_error"`
	}
	out := struct {
		Run         string     `json:"run"`
		Status      string     `json:"status"`
		Goal        string     `json:"goal"`
		Integration string     `json:"integration"`
		Owner       int        `json:"owner"` // PID driving the run, 0 when none
		Tasks       []taskJSON `json:"tasks"`
	}{Run: run.ID, Status: run.Status, Goal: run.Goal, Integration: run.IntegrationBranch, Tasks: []taskJSON{}}
	out.Owner, _ = execution.RunOwner(config.StateDir(), runID)
	for _, task := range tasks {
		out.Tasks = append(out.Tasks, taskJSON{planID(task.ID), task.Title, task.Status, task.Assignee, task.Attempts, task.LastError})
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}

func resolveTask(ctx context.Context, state *store.Store, runID, name string) (string, error) {
	tasks, err := state.TasksForRun(ctx, runID)
	if err != nil {
		return "", err
	}
	for _, task := range tasks {
		if task.ID == name || planID(task.ID) == name {
			return task.ID, nil
		}
	}
	return "", fmt.Errorf("unknown task %q", name)
}

// linePrinter prints each agent's output a whole line at a time, prefixed
// with the agent name, so concurrent workers stay readable.
type linePrinter struct {
	mu      sync.Mutex
	out     io.Writer
	partial map[string]string
}

func (p *linePrinter) write(agent, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	lines := strings.Split(p.partial[agent]+text, "\n")
	p.partial[agent] = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		if strings.TrimSpace(line) != "" {
			fmt.Fprintf(p.out, "%-10s %s\n", agent, line)
		}
	}
}

func (p *linePrinter) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for agent, line := range p.partial {
		if strings.TrimSpace(line) != "" {
			fmt.Fprintf(p.out, "%-10s %s\n", agent, line)
		}
	}
	p.partial = map[string]string{}
}

func planID(id string) string {
	return id[strings.LastIndexByte(id, ':')+1:]
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[:i] + " …"
	}
	return text
}
