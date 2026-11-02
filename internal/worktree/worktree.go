package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Manager struct {
	Repo     string
	StateDir string
}

type Worktree struct {
	Path   string
	Branch string
}

// New validates the repository and returns a worktree manager rooted under
// the Switch state directory.
func New(ctx context.Context, repo, stateDir string) (*Manager, error) {
	repo, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	root, err := gitOutput(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not a git repository: %w", repo, err)
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	return &Manager{Repo: strings.TrimSpace(root), StateDir: stateDir}, nil
}

func (m *Manager) Head(ctx context.Context) (string, error) {
	return gitOutput(ctx, m.Repo, "rev-parse", "HEAD")
}

// Root is the per-run directory holding all worktrees.
func (m *Manager) Root(runID string) string {
	sum := sha256.Sum256([]byte(m.Repo))
	repoKey := hex.EncodeToString(sum[:])[:8]
	return filepath.Join(m.StateDir, "worktrees", repoKey, safe(runID))
}

// Integration creates the run integration branch and its worktree.
func (m *Manager) Integration(ctx context.Context, runID, base string) (Worktree, string, error) {
	branch := "switch/" + safe(runID) + "/integration"
	path := filepath.Join(m.Root(runID), "integration")
	if err := m.add(ctx, branch, path, base); err != nil {
		return Worktree{}, "", err
	}
	return Worktree{Path: path, Branch: branch}, branch, nil
}

// Create makes a branch and worktree for one agent or task.
func (m *Manager) Create(ctx context.Context, runID, name, base string) (Worktree, error) {
	branch := "switch/" + safe(runID) + "/" + safe(name)
	path := filepath.Join(m.Root(runID), safe(name))
	if err := m.add(ctx, branch, path, base); err != nil {
		return Worktree{}, err
	}
	return Worktree{Path: path, Branch: branch}, nil
}

func (m *Manager) add(ctx context.Context, branch, path, base string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("worktree path already exists: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if strings.TrimSpace(base) == "" {
		base = "HEAD"
	}
	_, err := gitOutput(ctx, m.Repo, "worktree", "add", "-b", branch, path, base)
	if err != nil {
		return fmt.Errorf("create worktree %s: %w", branch, err)
	}
	return nil
}

func (m *Manager) Remove(ctx context.Context, path string) error {
	_, err := gitOutput(ctx, m.Repo, "worktree", "remove", "--force", path)
	if err != nil {
		_, _ = gitOutput(ctx, m.Repo, "worktree", "prune")
	}
	return err
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func safe(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-'
		if allowed {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(b.String(), ".-")
	if result == "" {
		result = "task"
	}
	if len(result) > 64 {
		result = result[:64]
	}
	return result
}
