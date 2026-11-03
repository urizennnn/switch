package worktree

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrConflict means a merge stopped on conflicts and was aborted. Switch never
// resolves conflicts itself.
var ErrConflict = errors.New("merge conflict")

// Switch commits under its own identity so it works where git has none
// configured; the message names the agent.
var commitIdentity = []string{"-c", "user.name=Switch", "-c", "user.email=switch@localhost"}

// Head returns the commit checked out in dir.
func Head(ctx context.Context, dir string) (string, error) {
	return gitOutput(ctx, dir, "rev-parse", "HEAD")
}

// Commit stages everything in dir and commits it. It reports the resulting
// HEAD and whether anything changed.
func Commit(ctx context.Context, dir, message string) (string, bool, error) {
	status, err := gitOutput(ctx, dir, "status", "--porcelain")
	if err != nil {
		return "", false, err
	}
	if status == "" {
		head, err := Head(ctx, dir)
		return head, false, err
	}
	if _, err := gitOutput(ctx, dir, "add", "-A"); err != nil {
		return "", false, err
	}
	if _, err := gitOutput(ctx, dir, append(commitIdentity, "commit", "-m", message)...); err != nil {
		return "", false, err
	}
	head, err := Head(ctx, dir)
	return head, true, err
}

// Diff returns the stat and patch between two commits.
func Diff(ctx context.Context, dir, from, to string) (string, error) {
	return gitOutput(ctx, dir, "diff", "--stat", "--patch", from, to)
}

// Merge merges ref into the branch checked out in dir with a merge commit. On
// conflict it aborts, leaves dir unchanged, and returns ErrConflict naming
// the conflicting files.
func Merge(ctx context.Context, dir, ref, message string) error {
	_, err := gitOutput(ctx, dir, append(commitIdentity, "merge", "--no-ff", "-m", message, ref)...)
	if err == nil {
		return nil
	}
	conflicts, _ := gitOutput(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	_, _ = gitOutput(ctx, dir, "merge", "--abort")
	if conflicts != "" {
		return fmt.Errorf("%w in %s", ErrConflict, strings.ReplaceAll(conflicts, "\n", ", "))
	}
	return err
}

// ResetHard moves the branch checked out in dir back to ref. Switch only uses
// it on its own integration branch, to undo a merge that failed verification.
func ResetHard(ctx context.Context, dir, ref string) error {
	_, err := gitOutput(ctx, dir, "reset", "--hard", ref)
	return err
}

// MergeBase returns the best common ancestor of two commits.
func MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	return gitOutput(ctx, dir, "merge-base", a, b)
}
