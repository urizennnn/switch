package runcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/store"
)

// Summary is a pull request description built from the run's saved state:
// the goal and integration branch, each task's worker summary, lead review
// notes and latest verification results, and the tokens the run used.
func Summary(ctx context.Context, state *store.Store, runID string) (string, error) {
	run, ok, err := state.GetRun(ctx, runID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("run %s not found", shortID(runID))
	}
	tasks, err := state.TasksForRun(ctx, runID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Goal\n\n%s\n\nIntegration branch: `%s`\n\n## Tasks\n", strings.TrimSpace(run.Goal), run.IntegrationBranch)
	for _, task := range tasks {
		fmt.Fprintf(&b, "\n### %s\n\n`%s` · %s\n", task.Title, planID(task.ID), task.Status)
		if text := strings.TrimSpace(task.Summary); text != "" {
			fmt.Fprintf(&b, "\n%s\n", text)
		}
		if text := strings.TrimSpace(task.Review); text != "" {
			fmt.Fprintf(&b, "\nReview notes:\n\n> %s\n", strings.ReplaceAll(text, "\n", "\n> "))
		}
		checks, err := state.Verifications(ctx, task.ID)
		if err != nil {
			return "", err
		}
		// The latest result of each command, per scope: attempts restart at 1
		// on a retry, so the order the results were saved in decides.
		latest := map[string]store.Verification{}
		var order []string
		for _, check := range checks {
			key := check.Scope + "\x00" + check.Command
			if _, seen := latest[key]; !seen {
				order = append(order, key)
			}
			latest[key] = check
		}
		if len(order) > 0 {
			b.WriteString("\nVerification:\n\n")
		}
		for _, key := range order {
			check := latest[key]
			result := fmt.Sprintf("exit %d", check.ExitCode)
			if check.TimedOut {
				result = "timed out"
			}
			fmt.Fprintf(&b, "- `%s` (%s): %s\n", check.Command, check.Scope, result)
		}
	}
	usage, err := state.RunUsage(ctx, runID)
	if err != nil {
		return "", err
	}
	if usage.TotalTokens > 0 {
		fmt.Fprintf(&b, "\n## Usage\n\n%d tokens (%d input, %d output)\n", usage.TotalTokens, usage.InputTokens, usage.OutputTokens)
	}
	return b.String(), nil
}

// WriteSummary saves the run's Summary as summary.md in its run directory and
// returns the file's path.
func WriteSummary(ctx context.Context, state *store.Store, stateDir, runID string) (string, error) {
	text, err := Summary(ctx, state, runID)
	if err != nil {
		return "", err
	}
	dir := execution.RunDir(stateDir, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "summary.md")
	return path, os.WriteFile(path, []byte(text), 0o600)
}

// reportSummary writes the PR description of a completed run and tells out
// where it is.
func reportSummary(ctx context.Context, state *store.Store, command, runID string, out io.Writer) {
	if path, err := WriteSummary(ctx, state, config.StateDir(), runID); err != nil {
		fmt.Fprintf(os.Stderr, "%s: summary: %v\n", command, err)
	} else {
		fmt.Fprintf(out, "PR description: %s\n", path)
	}
}

// ChecksFailed says, for a run whose tasks are all merged but which is
// blocked, that its own checks failed after the last approval, which only
// the event log records; "" for any other run.
func ChecksFailed(ctx context.Context, state *store.Store, run store.Run) string {
	if run.Status != execution.RunBlocked {
		return ""
	}
	tasks, err := state.TasksForRun(ctx, run.ID)
	if err != nil || len(tasks) == 0 {
		return ""
	}
	for _, task := range tasks {
		if task.Status != execution.StatusMerged {
			return ""
		}
	}
	return "\nThe run's own checks then failed, so the run is blocked: `switch __control log` shows why, and running the run again reruns them."
}
