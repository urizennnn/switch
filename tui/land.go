package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/store"
	"github.com/urizennnn/switch/internal/worktree"
)

// /land brings a completed run home: it shows what the run changed and the
// merge that lands it, and enter in that panel, or /land again, runs the
// merge in the project checkout. Nothing is pushed.

// landTitle names the panel in which enter merges.
const landTitle = "land"

// landMsg is the preview, with branch and onto set so the merge can follow,
// or how the merge went.
type landMsg struct {
	branch, onto string // merge branch into onto, the branch the preview named
	tip          string // the commit branch was at when the preview showed it
	title, body  string
}

const notOnBranch = "your checkout is not on a branch; check out the branch the run should land on, then /land again"

const pruneHint = "\n\n`switch __control prune` now removes the run's worktrees and branches."

func landArgs(branch string) []string { return []string{"merge", "--no-ff", "--no-edit", branch} }

func landPreviewCmd(project string, run store.Run) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		current, refused := landTarget(ctx, project, run.IntegrationBranch)
		if refused != nil {
			return refused
		}
		tip, err := worktree.Git(ctx, project, "rev-parse", "--verify", run.IntegrationBranch+"^{commit}")
		if err != nil {
			return landMsg{title: "land failed", body: err.Error()}
		}
		// From where HEAD and the run part: what the merge brings into this
		// branch, which is more than the run's own work when it started elsewhere.
		stat, err := worktree.Git(ctx, project, "diff", "--stat", "HEAD..."+tip)
		if err != nil {
			return landMsg{title: "land failed", body: err.Error()}
		}
		stat = strings.ReplaceAll(stat, "\n ", "\n") // git indents every line; Git trimmed only the first
		return landMsg{branch: run.IntegrationBranch, onto: current, tip: tip, title: landTitle, body: fmt.Sprintf(
			"%s (%.12s) into %s\n\n%s\n\ngit %s\n\nenter or /land again runs this in %s. Nothing is pushed.",
			run.IntegrationBranch, tip, current, stat, strings.Join(landArgs(run.IntegrationBranch), " "), shortenHome(project))}
	}
}

// landCmd merges branch into onto, the checkout's branch, while branch is
// still at tip, or leaves it as it was.
func landCmd(project, branch, onto, tip string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background() // quitting must not kill git mid-merge
		status, err := worktree.Git(ctx, project, "status", "--porcelain", "--untracked-files=no")
		switch {
		case err != nil:
			return landMsg{title: "land failed", body: err.Error()}
		case status != "":
			// git merge --abort may not restore them, should the merge need undoing.
			return landMsg{title: "land refused", body: "your checkout has uncommitted changes; commit or stash them, then /land again"}
		}
		current, refused := landTarget(ctx, project, branch)
		switch {
		case refused != nil:
			return refused
		case current != onto:
			return landMsg{title: "land refused", body: fmt.Sprintf("your checkout is on %s now, not %s, which /land showed. /land again shows what landing on %s does.", current, onto, current)}
		}
		if now, err := worktree.Git(ctx, project, "rev-parse", "--verify", branch+"^{commit}"); err != nil || now != tip {
			return landMsg{title: "land refused", body: branch + " moved since /land showed it; /land again shows what it holds now"}
		}
		if _, err := worktree.Git(ctx, project, landArgs(branch)...); err != nil {
			conflicts, _ := worktree.Git(ctx, project, "diff", "--name-only", "--diff-filter=U")
			_, _ = worktree.Git(ctx, project, "merge", "--abort")
			if _, mid := worktree.Git(ctx, project, "rev-parse", "-q", "--verify", "MERGE_HEAD"); mid == nil {
				return landMsg{title: "land failed", body: fmt.Sprintf("%s did not merge, and Switch could not undo the merge: run git merge --abort in %s\n%v", branch, shortenHome(project), err)}
			}
			if conflicts != "" {
				return landMsg{title: "land refused", body: fmt.Sprintf("merging %s into %s conflicts in %s.\nSwitch undid the merge, so %s is as it was. To resolve it yourself, run: git merge --no-ff %s",
					branch, current, strings.ReplaceAll(conflicts, "\n", ", "), current, branch)}
			}
			return landMsg{title: "land failed", body: fmt.Sprintf("%s did not merge, and %s is as it was:\n%v", branch, current, err)}
		}
		head, _ := worktree.Git(ctx, project, "rev-parse", "--short", "HEAD")
		return landMsg{title: "landed", body: fmt.Sprintf("merged %s into %s (%s). Nothing was pushed.", branch, current, head) + pruneHint}
	}
}

// landTarget returns the checkout's branch, or, as a message, why branch
// cannot land there.
func landTarget(ctx context.Context, project, branch string) (string, tea.Msg) {
	current, err := worktree.Git(ctx, project, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", landMsg{title: "land refused", body: notOnBranch}
	}
	// git merges a branch it already has without a commit and without an error.
	if _, err := worktree.Git(ctx, project, "merge-base", "--is-ancestor", branch, "HEAD"); err == nil {
		return "", landMsg{title: "already landed", body: branch + " is already in " + current + "; there is nothing to merge." + pruneHint}
	}
	return current, nil
}

// land merges as the preview /land showed; a third /land shows it again first.
func (m *model) land() tea.Cmd {
	preview := m.landing
	m.landing = landMsg{}
	return landCmd(m.project, preview.branch, preview.onto, preview.tip)
}
