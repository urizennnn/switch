package runcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/store"
	"github.com/urizennnn/switch/internal/worktree"
)

// prune removes the worktrees and switch/<run>/ branches of the project's
// finished runs: completed ones, and older ones that a newer plan replaced
// (Switch works on the latest run). It never touches a run that a process
// drives. Unless all, it keeps work that was never merged: a worktree with
// uncommitted changes, a task branch not merged into its integration branch,
// and an integration branch that no branch outside switch/ contains.
func prune(ctx context.Context, state *store.Store, project, stateDir string, all bool) error {
	runs, err := state.Runs(ctx, project)
	if err != nil {
		return err
	}
	manager, err := worktree.New(ctx, project, stateDir)
	if err != nil {
		return err
	}
	removed := 0
	for i, run := range runs {
		if i == 0 && run.Status != execution.RunCompleted {
			continue // the current run, which /run resumes
		}
		// Holding the run lock keeps anyone from starting the run meanwhile.
		lock, err := execution.LockRun(stateDir, run.ID, true)
		if errors.Is(err, execution.ErrRunBusy) {
			fmt.Printf("kept run %s: %v\n", shortID(run.ID), err)
			continue
		}
		if err != nil {
			return err
		}
		// `__control approve` takes only the merge lock, so hold it too.
		merge, err := execution.TryLockMerge(filepath.Join(manager.Root(run.ID), "integration"))
		if err == nil && merge == nil {
			lock.Release()
			fmt.Printf("kept run %s: an approval is merging in it\n", shortID(run.ID))
			continue
		} // an error means no run folder, so nothing to merge in
		n, err := pruneRun(ctx, manager, run, all)
		merge.Release()
		lock.Release()
		removed += n
		if err != nil {
			return err
		}
	}
	if removed == 0 {
		fmt.Println("nothing to prune")
	}
	return nil
}

// pruneRun removes one run's worktrees and branches and returns how many.
func pruneRun(ctx context.Context, manager *worktree.Manager, run store.Run, all bool) (int, error) {
	removed := 0
	root := manager.Root(run.ID)
	entries, _ := os.ReadDir(root) // none when an earlier prune removed them
	for _, entry := range entries {
		if !entry.IsDir() {
			continue // merge.lock, removed below
		}
		dir := filepath.Join(root, entry.Name())
		// Uncommitted edits, such as an interrupted or blocked task's, exist
		// nowhere else. git refuses to delete the branch of a worktree that stays.
		if !all {
			// Not worktree.Git, which drops stderr: git only warns there about
			// a folder it cannot read, which may hold uncommitted work.
			output, err := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain", "--untracked-files=normal").CombinedOutput()
			status := strings.TrimSpace(string(output))
			if err != nil {
				err = fmt.Errorf("git status: %w: %s", err, status)
			} else if status != "" {
				err = errors.New("uncommitted changes")
			}
			if err != nil {
				fmt.Printf("kept worktree %s: %v; --all deletes it\n", dir, err)
				continue
			}
		}
		if manager.Remove(ctx, dir) != nil {
			// Not a worktree git knows, such as a half-made one.
			if err := os.RemoveAll(dir); err != nil {
				return removed, err
			}
		}
		fmt.Printf("removed worktree %s\n", dir)
		removed++
	}
	// prune holds the merge lock, so no approval is waiting on this file.
	_ = os.Remove(filepath.Join(root, "merge.lock"))
	_ = os.Remove(root)
	// Forget worktrees whose folders are gone: they would keep their branches.
	if _, err := worktree.Git(ctx, manager.Repo, "worktree", "prune"); err != nil {
		return removed, err
	}

	integration := run.IntegrationBranch
	refs, err := worktree.Git(ctx, manager.Repo, "for-each-ref", "--format=%(refname:short)", "refs/heads/"+path.Dir(integration)+"/")
	if err != nil {
		return removed, err
	}
	branches := strings.Fields(refs)
	for _, branch := range branches {
		if branch == integration {
			continue // last: the task branches are checked against it
		}
		keep := ""
		if !all && !merged(ctx, manager.Repo, branch, "refs/heads/"+integration) {
			keep = "not merged into " + integration + "; --all deletes it"
		}
		removed += deleteBranch(ctx, manager.Repo, branch, keep)
	}
	if slices.Contains(branches, integration) {
		keep := ""
		if !all && !merged(ctx, manager.Repo, integration, "") {
			keep = "no branch outside switch/ contains it; --all deletes it"
		}
		removed += deleteBranch(ctx, manager.Repo, integration, keep)
	}
	return removed, nil
}

// merged reports whether into contains branch, or with into empty, whether
// any branch or remote branch outside switch/ does.
func merged(ctx context.Context, repo, branch, into string) bool {
	if into != "" {
		_, err := worktree.Git(ctx, repo, "merge-base", "--is-ancestor", branch, into)
		return err == nil
	}
	refs, _ := worktree.Git(ctx, repo, "for-each-ref", "--format=%(refname)", "--contains", branch, "refs/heads", "refs/remotes")
	for _, ref := range strings.Fields(refs) {
		if !strings.HasPrefix(ref, "refs/heads/switch/") {
			return true
		}
	}
	return false
}

// deleteBranch deletes branch unless keep says why it stays, and returns
// how many it deleted.
func deleteBranch(ctx context.Context, repo, branch, keep string) int {
	if keep == "" {
		if _, err := worktree.Git(ctx, repo, "branch", "-D", branch); err != nil {
			keep = err.Error() // such as a branch the user has checked out
		}
	}
	if keep != "" {
		fmt.Printf("kept branch %s: %s\n", branch, keep)
		return 0
	}
	fmt.Printf("deleted branch %s\n", branch)
	return 1
}
