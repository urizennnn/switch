package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateAndRemove(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	ctx := context.Background()
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Switch Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")

	manager, err := New(ctx, repo, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if manager.Prefix != "" {
		t.Fatalf("Prefix at the repository root = %q", manager.Prefix)
	}
	if err := os.MkdirAll(filepath.Join(repo, "packages", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if sub, err := New(ctx, filepath.Join(repo, "packages", "api"), t.TempDir()); err != nil || sub.Prefix != "packages/api/" || sub.Repo != manager.Repo {
		t.Fatalf("New in a subdirectory = %+v, %v", sub, err)
	}
	head, err := manager.Head(ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head == "" {
		t.Fatal("empty HEAD")
	}

	integration, branch, err := manager.Integration(ctx, "run-1", head)
	if err != nil {
		t.Fatalf("Integration: %v", err)
	}
	if branch != "switch/run-1/integration" {
		t.Fatalf("branch = %q", branch)
	}
	if _, err := os.Stat(filepath.Join(integration.Path, "README.md")); err != nil {
		t.Fatalf("integration worktree missing file: %v", err)
	}

	worker, err := manager.Create(ctx, "run-1", "builder-1", head)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gotBranch := runGit(t, worker.Path, "rev-parse", "--abbrev-ref", "HEAD")
	if gotBranch != "switch/run-1/builder-1" {
		t.Fatalf("worker branch = %q", gotBranch)
	}
	// One left behind, say by a crash before it was recorded, is taken over,
	// also when git was stopped halfway: still locked and partly checked out.
	runGit(t, repo, "worktree", "lock", "--reason", "initializing", worker.Path)
	if err := os.Remove(filepath.Join(worker.Path, "README.md")); err != nil {
		t.Fatal(err)
	}
	if again, err := manager.Create(ctx, "run-1", "builder-1", head); err != nil || again != worker {
		t.Fatalf("Create again = %+v, %v; want %+v", again, err, worker)
	}
	if _, err := os.Stat(filepath.Join(worker.Path, "README.md")); err != nil {
		t.Fatalf("taken over partly checked out: %v", err)
	}

	if err := manager.Remove(ctx, worker.Path); err != nil {
		t.Fatalf("Remove worker: %v", err)
	}
	if _, err := os.Stat(worker.Path); !os.IsNotExist(err) {
		t.Fatalf("worker path still exists: %v", err)
	}
	if err := manager.Remove(ctx, integration.Path); err != nil {
		t.Fatalf("Remove integration: %v", err)
	}
}

// Git returns what git prints on stdout, which callers parse. What it prints
// on stderr, such as a warning about a broken branch, goes only into errors.
func TestGitKeepsStderrOutOfItsOutput(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-m", "init")
	head := runGit(t, repo, "rev-parse", "HEAD")
	t.Setenv("GIT_TRACE", "1") // git now writes a trace line to stderr for each command
	if got, err := Git(ctx, repo, "rev-parse", "HEAD"); err != nil || got != head {
		t.Fatalf("Git = %q, %v; want %s", got, err, head)
	}
	if _, err := Git(ctx, repo, "rev-parse", "--verify", "nosuch"); err == nil || !strings.Contains(err.Error(), "Needed a single revision") {
		t.Fatalf("err = %v, want git's own message", err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return stringTrim(string(output))
}

func stringTrim(value string) string {
	for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r' || value[len(value)-1] == ' ') {
		value = value[:len(value)-1]
	}
	return value
}
