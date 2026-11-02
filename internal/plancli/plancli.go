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

	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/planning"
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

	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" && *file == "" {
		fmt.Fprintln(os.Stderr, "plan: provide a goal or use --file")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var logs io.Writer
	if *agentLogs {
		logs = os.Stderr
	}
	result, err := planning.Execute(ctx, planning.Options{
		Project:    project,
		Goal:       goal,
		File:       *file,
		Agent:      *agent,
		Model:      *model,
		Timeout:    *timeout,
		Permission: mode,
		AgentLogs:  logs,
		OnText:     func(text string) { fmt.Print(text) },
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "plan: %v\n", err)
		return 1
	}

	if len(result.Created) > 0 {
		fmt.Printf("initialized switch in %s\n", project)
	}
	fmt.Println()
	fmt.Printf("lead:        %s (%s)\n", result.Lead.Name, result.Lead.Driver)
	fmt.Printf("run:         %s\n", short(result.Run.ID))
	fmt.Printf("integration: %s\n", result.Run.IntegrationBranch)
	fmt.Println("agents:")
	for _, item := range result.Agents {
		fmt.Printf("  %-10s %-8s %-10s %s\n",
			item.Agent.Name, item.Agent.Role, item.Agent.Driver, item.Worktree.Path)
	}
	fmt.Println("tasks:")
	for _, task := range result.Plan.Tasks {
		deps := "none"
		if len(task.Deps) > 0 {
			deps = strings.Join(task.Deps, ",")
		}
		fmt.Printf("  %-12s %-8s deps=%-12s %s\n", task.ID, task.Role, deps, task.Title)
	}
	fmt.Printf("\nrun saved. worktrees: %s\n", result.Root)
	fmt.Println("task execution is the next Phase 2 step.")
	return 0
}

func short(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
