// Package verify runs a task's verification commands and captures evidence.
// Switch never trusts a worker's claim of completion; only these results
// move a task forward.
package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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
	Flaky    bool // passed on a rerun; the result before it is the failure
}

func (r Result) Passed() bool { return r.ExitCode == 0 && !r.TimedOut }

// Run executes each command with `sh -c` in dir, in order, stopping at the
// first failure. env adds to the commands' environment. A command that fails
// runs once more before it counts, since flaky tests fail now and then, and
// both results are kept. reset, unless nil, first undoes what the failed run
// wrote, so the rerun cannot pass on that; if it fails, there is no rerun. A
// timeout is not rerun, as it would only take as long again. No commands
// means nothing to check, which passes.
func Run(ctx context.Context, dir string, commands []string, timeout time.Duration, reset func() error, env ...string) ([]Result, bool) {
	var results []Result
	for _, command := range commands {
		if strings.TrimSpace(command) == "" {
			continue
		}
		result := runOne(ctx, dir, command, timeout, env)
		if !result.Passed() && !result.TimedOut && ctx.Err() == nil && (reset == nil || reset() == nil) {
			results = append(results, result) // the failure is evidence too
			result = runOne(ctx, dir, command, timeout, env)
			result.Flaky = result.Passed()
		}
		results = append(results, result)
		if !result.Passed() {
			return results, false
		}
	}
	return results, true
}

func runOne(ctx context.Context, dir, command string, timeout time.Duration, env []string) Result {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
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
		case r.Flaky:
			status = "passed on a rerun (flaky)"
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
