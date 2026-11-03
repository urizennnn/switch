// Package verify runs a task's verification commands and captures evidence.
// Switch never trusts a worker's claim of completion; only these results
// move a task forward.
package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// maxOutput caps captured output per command; the tail is kept because
// failures are usually reported last.
const maxOutput = 16 * 1024

type Result struct {
	Command  string
	ExitCode int
	Output   string // combined stdout and stderr
	Duration time.Duration
	TimedOut bool
}

func (r Result) Passed() bool { return r.ExitCode == 0 && !r.TimedOut }

// Run executes each command with `sh -c` in dir, in order, stopping at the
// first failure. No commands means nothing to check, which passes.
func Run(ctx context.Context, dir string, commands []string, timeout time.Duration) ([]Result, bool) {
	var results []Result
	for _, command := range commands {
		if strings.TrimSpace(command) == "" {
			continue
		}
		result := runOne(ctx, dir, command, timeout)
		results = append(results, result)
		if !result.Passed() {
			return results, false
		}
	}
	return results, true
}

func runOne(ctx context.Context, dir, command string, timeout time.Duration) Result {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = dir
	configureProcess(cmd)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	// Do not wait on pipes held open by background children after a kill.
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err := cmd.Run()
	result := Result{Command: command, Duration: time.Since(start), Output: tail(output.String())}

	var exitErr *exec.ExitError
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		result.TimedOut = true
		result.ExitCode = -1
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	case err != nil:
		result.ExitCode = -1
		result.Output = strings.TrimSpace(result.Output + "\n" + err.Error())
	}
	return result
}

func tail(output string) string {
	if len(output) <= maxOutput {
		return output
	}
	return "…" + output[len(output)-maxOutput:]
}

// Summary renders results for a worker or reviewer prompt.
func Summary(results []Result) string {
	if len(results) == 0 {
		return "no verification commands"
	}
	var b strings.Builder
	for _, r := range results {
		status := "passed"
		switch {
		case r.TimedOut:
			status = "timed out"
		case r.ExitCode != 0:
			status = fmt.Sprintf("failed (exit %d)", r.ExitCode)
		}
		fmt.Fprintf(&b, "$ %s\n%s in %s\n", r.Command, status, r.Duration.Round(time.Millisecond))
		if !r.Passed() && strings.TrimSpace(r.Output) != "" {
			b.WriteString(strings.TrimSpace(r.Output))
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
