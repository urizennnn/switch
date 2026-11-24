package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitMergeAndConflict(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	repo := t.TempDir()
	runGit(t, repo, "init")
	writeFile(t, repo, "a.txt", "one\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "init")

	manager, err := New(ctx, repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, _ := manager.Head(ctx)
	integration, branch, err := manager.Integration(ctx, "r", base)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := manager.Create(ctx, "r", "task-a", branch)
	second, _ := manager.Create(ctx, "r", "task-b", branch)

	if _, changed, err := Commit(ctx, first.Path, "noop"); err != nil || changed {
		t.Fatalf("clean commit: changed=%v err=%v", changed, err)
	}
	writeFile(t, first.Path, "a.txt", "from a\n")
	writeFile(t, first.Path, "new.txt", "added\n")
	sha, changed, err := Commit(ctx, first.Path, "switch: task a")
	if err != nil || !changed {
		t.Fatalf("Commit: changed=%v err=%v", changed, err)
	}
	diff, err := Diff(ctx, first.Path, base, sha)
	if err != nil || !strings.Contains(diff, "new.txt") || !strings.Contains(diff, "+from a") {
		t.Fatalf("Diff = %q, %v", diff, err)
	}

	if err := Merge(ctx, integration.Path, sha, "merge a"); err != nil {
		t.Fatalf("Merge a: %v", err)
	}
	if got := readFile(t, integration.Path, "a.txt"); got != "from a\n" {
		t.Fatalf("integration a.txt = %q", got)
	}

	// b edits the same line from the old base, so merging it conflicts.
	writeFile(t, second.Path, "a.txt", "from b\n")
	if _, _, err := Commit(ctx, second.Path, "switch: task b"); err != nil {
		t.Fatal(err)
	}
	before, _ := Head(ctx, integration.Path)
	err = Merge(ctx, integration.Path, second.Branch, "merge b")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "a.txt") {
		t.Fatalf("Merge b err = %v, want conflict in a.txt", err)
	}
	after, _ := Head(ctx, integration.Path)
	if before != after || runGit(t, integration.Path, "status", "--porcelain") != "" {
		t.Fatal("conflicting merge was not aborted cleanly")
	}

	// The conflict shows without touching a worktree, and Update leaves its
	// markers in b's worktree for a worker to resolve.
	bHead, _ := Head(ctx, second.Path)
	if conflict, err := Conflicts(ctx, second.Path, branch, bHead); err != nil || !conflict {
		t.Fatalf("Conflicts = %v, %v; want a conflict", conflict, err)
	}
	if conflict, err := Conflicts(ctx, first.Path, branch, sha); err != nil || conflict {
		t.Fatalf("Conflicts(merged a) = %v, %v; want none", conflict, err)
	}
	if err := Update(ctx, second.Path, branch); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "a.txt") {
		t.Fatalf("Update err = %v, want conflict in a.txt", err)
	}
	if got := readFile(t, second.Path, "a.txt"); !strings.Contains(got, "<<<<<<<") {
		t.Fatalf("no conflict markers left: %q", got)
	}

	// Checkout drops leftovers, such as a check's report, and detaches.
	writeFile(t, integration.Path, "report.xml", "x")
	writeFile(t, integration.Path, "a.txt", "dirty\n")
	if err := Checkout(ctx, integration.Path, base); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, integration.Path, "a.txt"); got != "one\n" || runGit(t, integration.Path, "status", "--porcelain") != "" {
		t.Fatalf("after Checkout a.txt = %q, status %q", got, runGit(t, integration.Path, "status", "--porcelain"))
	}
}

// Switch's own commits and merges must not depend on the repository's hooks,
// its signing setup, or how its status shows untracked files.
func TestSwitchGitSkipsHooksSigningAndStatusConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	repo := t.TempDir()
	runGit(t, repo, "init")
	writeFile(t, repo, "a.txt", "one\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "init")
	manager, err := New(ctx, repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, _ := manager.Head(ctx)
	integration, branch, err := manager.Integration(ctx, "r", base)
	if err != nil {
		t.Fatal(err)
	}
	task, err := manager.Create(ctx, "r", "task-a", branch)
	if err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"pre-commit", "commit-msg", "prepare-commit-msg", "pre-merge-commit", "post-checkout", "reference-transaction"} {
		if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", hook), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repo, "config", "commit.gpgsign", "true")
	runGit(t, repo, "config", "gpg.program", "false")
	runGit(t, repo, "config", "status.showUntrackedFiles", "no")
	runGit(t, repo, "config", "merge.ff", "only")
	runGit(t, repo, "config", "merge.verifySignatures", "true")

	// A task that only adds a file still has a change.
	writeFile(t, task.Path, "new.txt", "added\n")
	sha, changed, err := Commit(ctx, task.Path, "switch: add new.txt")
	if err != nil || !changed || sha == base {
		t.Fatalf("Commit = %s, %v, %v; want a new commit", sha, changed, err)
	}
	if err := Checkout(ctx, integration.Path, "--detach", branch); err != nil {
		t.Fatal(err)
	}
	if err := Merge(ctx, integration.Path, sha, "switch: merge a"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	// Advance never moves the branch under another worktree that has it checked out.
	runGit(t, repo, "-c", "core.hooksPath=/dev/null", "checkout", "-q", branch)
	if err := Advance(ctx, integration.Path, branch); err == nil {
		t.Fatal("Advance moved a branch checked out in another worktree")
	}
	runGit(t, repo, "-c", "core.hooksPath=/dev/null", "checkout", "-q", "-")
	if err := Advance(ctx, integration.Path, branch); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := runGit(t, repo, "show", branch+":new.txt"); got != "added" {
		t.Fatalf("integration branch new.txt = %q", got)
	}
	// Nor does it drop a commit the branch gained meanwhile.
	if err := Checkout(ctx, integration.Path, "--detach", base); err != nil {
		t.Fatal(err)
	}
	writeFile(t, integration.Path, "other.txt", "x\n")
	if _, _, err := Commit(ctx, integration.Path, "switch: diverge"); err != nil {
		t.Fatal(err)
	}
	if err := Advance(ctx, integration.Path, branch); err == nil {
		t.Fatal("Advance dropped a commit the branch had gained")
	}
	writeFile(t, task.Path, "a.txt", "two\n")
	if _, _, err := Commit(ctx, task.Path, "switch: edit a"); err != nil {
		t.Fatal(err)
	}
	if err := Update(ctx, task.Path, branch); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
