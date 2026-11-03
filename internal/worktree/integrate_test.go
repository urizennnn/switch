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

	if err := Merge(ctx, integration.Path, first.Branch, "merge a"); err != nil {
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

	if err := ResetHard(ctx, integration.Path, base); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, integration.Path, "a.txt"); got != "one\n" {
		t.Fatalf("after reset a.txt = %q", got)
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
