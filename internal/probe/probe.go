package probe

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/urizennnn/switch/internal/driver/acpdriver"
)

// Run executes `switch probe` and returns a process exit code.
func Run(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	agent := fs.String("agent", "opencode", "agent adapter: opencode, codex, claude, openclaw, or custom")
	command := fs.String("command", "", "custom adapter command (for example: \"my-agent acp\")")
	cwd := fs.String("cwd", ".", "working directory for the agent session")
	prompt := fs.String("prompt", "Reply with exactly: switch probe ok", "prompt to send")
	model := fs.String("model", "", "model config value to select before prompting")
	sessionID := fs.String("session", "", "resume an existing ACP session ID")
	timeout := fs.Duration("timeout", 5*time.Minute, "prompt timeout")
	permission := fs.String("permission", "ask", "permission mode: ask, allow, or deny")
	jsonOut := fs.Bool("json", false, "emit normalized JSONL events")
	verbose := fs.Bool("verbose", false, "show thoughts, tool status updates, and plans")
	agentLogs := fs.Bool("agent-logs", false, "stream adapter stderr to the terminal")
	noPrompt := fs.Bool("no-prompt", false, "connect and report capabilities without prompting")
	list := fs.Bool("list", false, "list known adapters and local availability")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *list {
		acpdriver.PrintAdapterStatuses(os.Stdout)
		return 0
	}

	mode, err := acpdriver.ParsePermissionMode(*permission)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		return 2
	}

	var cmd []string
	if *command != "" {
		cmd = strings.Fields(*command)
		if len(cmd) == 0 {
			fmt.Fprintln(os.Stderr, "probe: --command is empty")
			return 2
		}
		if *agent == "opencode" {
			*agent = "custom"
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	err = acpdriver.Probe(ctx, acpdriver.ProbeOptions{
		Agent:      *agent,
		Command:    cmd,
		Cwd:        *cwd,
		Prompt:     *prompt,
		Model:      *model,
		SessionID:  *sessionID,
		Timeout:    *timeout,
		Permission: mode,
		JSON:       *jsonOut,
		Verbose:    *verbose,
		AgentLogs:  *agentLogs,
		NoPrompt:   *noPrompt,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		return 1
	}
	return 0
}
