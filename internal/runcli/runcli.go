// Package runcli is the headless form of /run and the task commands:
// `switch __control run|tasks|approve|reject|retry|diff|log`. It drives the
// same engine as the TUI, so both produce the same state.
package runcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

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
	runID := fs.String("run", "", "run ID (default: the project's latest run)")
	permission := fs.String("permission", "deny", "worker permission mode for run: allow or deny")
	agentLogs := fs.Bool("agent-logs", false, "stream adapter stderr")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := run(command, fs.Args(), *cwd, *runID, *permission, *agentLogs); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", command, err)
		return 1
	}
	return 0
}

func run(command string, args []string, cwd, runID, permission string, agentLogs bool) error {
	project, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if runID == "" {
		latest, ok, err := state.LatestRun(ctx, project)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("no run for this project; plan one with `switch __control plan`")
		}
		runID = latest.ID
	}
	opts := execution.Options{Store: state, RunID: runID, Rules: ruleSet, Config: cfg.Run}

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
		opts.Permission = mode
		if agentLogs {
			opts.AgentLogs = os.Stderr
		}
		printer := &linePrinter{out: os.Stdout, partial: map[string]string{}}
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
		outcome, err := engine.Run(ctx)
		printer.flush()
		fmt.Printf("\nrun %s: %s\n", shortID(runID), outcome)
		if outcome == execution.RunWaiting {
			fmt.Println("tasks are waiting for review: `switch __control tasks`, then approve or reject them")
		}
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err

	case "tasks":
		tasks, err := state.TasksForRun(ctx, runID)
		if err != nil {
			return err
		}
		for _, task := range tasks {
			fmt.Printf("%-16s %-11s %-10s attempts=%d  %s\n", planID(task.ID), task.Status, orDash(task.Assignee), task.Attempts, task.Title)
			if task.LastError != "" {
				fmt.Printf("%16s last error: %s\n", "", firstLine(task.LastError))
			}
		}
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
		return nil
	}
	return fmt.Errorf("unknown command %q", command)
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
