package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrConflict means a merge stopped on conflicts and was aborted. Switch never
// resolves conflicts itself.
var ErrConflict = errors.New("merge conflict")

// switchGit configures the git commands that change Switch's own worktrees.
// Switch commits under its own identity so it works where git has none
// configured; the message names the agent. It skips the repository's hooks
// and commit signing: verification is the gate, and a hook or a pinentry
// prompt must not reject or stall its commits, merges, and resets. Its merges
// ignore merge.ff and signature checks, which its unsigned commits would fail.
var switchGit = []string{"-c", "user.name=Switch", "-c", "user.email=switch@localhost",
	"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
	"-c", "merge.ff=true", "-c", "merge.verifySignatures=false"}

// Head returns the commit checked out in dir.
func Head(ctx context.Context, dir string) (string, error) {
	return Git(ctx, dir, "rev-parse", "HEAD")
}

// Commit stages everything in dir and commits it. It reports the resulting
// HEAD and whether anything changed.
func Commit(ctx context.Context, dir, message string) (string, bool, error) {
	// Count new files even where status.showUntrackedFiles hides them.
	status, err := Git(ctx, dir, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return "", false, err
	}
	// A merge in progress needs its commit even when it changes nothing, as
	// when a worker resolves a conflict by keeping its side and staging it.
	_, noMerge := Git(ctx, dir, "rev-parse", "-q", "--verify", "MERGE_HEAD")
	if status == "" && noMerge != nil {
		head, err := Head(ctx, dir)
		return head, false, err
	}
	if _, err := Git(ctx, dir, "add", "-A"); err != nil {
		return "", false, err
	}
	if _, err := Git(ctx, dir, append(switchGit, "commit", "-m", message)...); err != nil {
		return "", false, err
	}
	head, err := Head(ctx, dir)
	return head, true, err
}

// Diff returns the stat and patch between two commits.
func Diff(ctx context.Context, dir, from, to string) (string, error) {
	return Git(ctx, dir, "diff", "--stat", "--patch", from, to)
}

// Merge merges ref into HEAD in dir with a merge commit. On conflict it
// aborts, leaves dir unchanged, and returns ErrConflict naming the
// conflicting files.
func Merge(ctx context.Context, dir, ref, message string) error {
	_, err := Git(ctx, dir, append(switchGit, "merge", "--no-ff", "-m", message, ref)...)
	if err == nil {
		return nil
	}
	conflicts, _ := Git(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	_, _ = Git(ctx, dir, append(switchGit, "merge", "--abort")...)
	if conflicts != "" {
		return fmt.Errorf("%w in %s", ErrConflict, strings.ReplaceAll(conflicts, "\n", ", "))
	}
	return err
}

// Update merges ref into HEAD in dir. On conflict it leaves the markers for a
// worker to resolve and returns ErrConflict naming the files; committing the
// worktree then concludes the merge.
func Update(ctx context.Context, dir, ref string) error {
	_, err := Git(ctx, dir, append(switchGit, "merge", "--no-edit", ref)...)
	if err == nil {
		return nil
	}
	if conflicts, _ := Git(ctx, dir, "diff", "--name-only", "--diff-filter=U"); conflicts != "" {
		return fmt.Errorf("%w in %s", ErrConflict, strings.ReplaceAll(conflicts, "\n", ", "))
	}
	return err
}

// conflictMarker starts or ends a conflict git left in a file.
var conflictMarker = regexp.MustCompile(`(?m)^(<{7}|>{7})`)

// Unresolved lists the files of a merge in progress in dir that still hold
// conflict markers, which Commit would commit as they are.
func Unresolved(ctx context.Context, dir string) ([]string, error) {
	out, err := Git(ctx, dir, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, name := range strings.Split(out, "\x00") {
		// A file deleted to resolve its conflict has no markers left.
		data, err := os.ReadFile(filepath.Join(dir, name))
		if name != "" && err == nil && conflictMarker.Match(data) {
			files = append(files, name)
		}
	}
	return files, nil
}

// Conflicts reports whether merging a and b would conflict, without touching
// a worktree. It needs git 2.38; older git returns an error.
func Conflicts(ctx context.Context, dir, a, b string) (bool, error) {
	_, err := Git(ctx, dir, "merge-tree", "--write-tree", a, b)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}

// Clean drops uncommitted changes, untracked files, and any merge in progress
// in dir, such as files a verification command wrote.
func Clean(ctx context.Context, dir string) error {
	if _, err := Git(ctx, dir, append(switchGit, "reset", "--hard")...); err != nil {
		return err
	}
	_, err := Git(ctx, dir, "clean", "-fd")
	return err
}

// Checkout cleans dir, then runs git checkout with args there.
func Checkout(ctx context.Context, dir string, args ...string) error {
	if err := Clean(ctx, dir); err != nil {
		return err
	}
	_, err := Git(ctx, dir, append(append(switchGit, "checkout"), args...)...)
	return err
}

// Advance checks out branch in dir and fast-forwards it to the commit dir had
// checked out. git refuses while another worktree has the branch checked out,
// so it never moves under someone's index, and a fast-forward never drops a
// commit the branch gained meanwhile.
func Advance(ctx context.Context, dir, branch string) error {
	head, err := Head(ctx, dir)
	if err != nil {
		return err
	}
	if err := Checkout(ctx, dir, branch); err != nil {
		return err
	}
	_, err = Git(ctx, dir, append(switchGit, "merge", "--ff-only", head)...)
	return err
}

// Merged returns the merge commit on branch that merged commit, and that
// merge's first parent: the branch just before it.
func Merged(ctx context.Context, dir, branch, commit string) (string, string, error) {
	out, err := Git(ctx, dir, "rev-list", "--first-parent", "--merges", "--parents", commit+".."+branch)
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) == 3 && fields[2] == commit {
			return fields[0], fields[1], nil
		}
	}
	return "", "", fmt.Errorf("%s is not merged into %s", commit, branch)
}

// MergeBase returns the best common ancestor of two commits.
func MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	return Git(ctx, dir, "merge-base", a, b)
}
