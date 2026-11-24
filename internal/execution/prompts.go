package execution

import (
	"fmt"
	"strings"

	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

// maxReviewDiff keeps the review prompt within adapter limits.
const maxReviewDiff = 60 * 1024

// WorkerPrompt builds the brief for one task attempt.
func WorkerPrompt(task store.Task, agent store.RunAgent, ruleSet rules.Set) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a %s agent in a Switch run.\n", agent.Name, agent.Role)
	fmt.Fprintf(&b, "Work only inside this git worktree: %s (branch %s).\n\n", task.Worktree, task.Branch)
	fmt.Fprintf(&b, "Task: %s\n\n%s\n", task.Title, task.Brief)
	writeList(&b, "Files in scope", task.Files)
	writeList(&b, "Acceptance criteria", task.Acceptance)
	writeList(&b, "Verification commands (Switch runs these after you finish; run them yourself first)", task.Verify)
	if handoff, ok := strings.CutPrefix(task.Feedback, handoffPrefix); ok {
		from, note, _ := strings.Cut(handoff, ":\n")
		fmt.Fprintf(&b, "\nYou are taking over this task from %s, which handed it off. Its changes so far are in this worktree. Its note:\n%s\n", from, note)
	} else if task.Feedback != "" {
		fmt.Fprintf(&b, "\nThis is attempt %d. The previous attempt was sent back:\n%s\n", task.Attempts, task.Feedback)
	}
	writeRules(&b, ruleSet)
	b.WriteString("\nDo not commit, push, merge, or switch branches; Switch commits and integrates approved work.\n")
	b.WriteString("Report through the switch MCP tools (switch_progress, switch_complete, switch_block) if they are available,\n")
	b.WriteString("or with `$SWITCH_BIN __control agent progress|complete|block <message>`. For a decision only the user can make, ask with switch_question;\n")
	b.WriteString("if the task cannot be done, block it and say why.\n")
	b.WriteString("Any other switch_ tool works from the shell too, with its arguments as JSON: `$SWITCH_BIN __control agent handoff '{\"to\": \"<agent>\", \"message\": \"...\"}'`.\n")
	b.WriteString("When you are done, reply with a short summary of what you changed and anything left open.\n")
	return b.String()
}

// envNote tells a worker what its environment and checks have to keep its
// task apart from others running at once.
func envNote(env []string) string {
	return fmt.Sprintf(`
You and the verification commands run with %s set. SWITCH_SLOT is unique among the tasks running at once:
derive ports, test databases and container names from it, so parallel tasks do not collide.
`, strings.Join(env, ", "))
}

// subagentNote tells a worker it may start subagents and that it owns them.
func subagentNote(limit int) string {
	if limit <= 0 {
		return ""
	}
	return fmt.Sprintf(`
You can start up to %d subagents at once for parts of this task with switch_spawn (name, brief, and the files each may edit;
no files means read-only). They work in this worktree and report only to you, so you are responsible for them:
collect results with switch_wait, steer them with switch_message, stop them with switch_stop, and check their work
before you rely on it. Give each subagent different files. Finish or stop every subagent before switch_complete;
any still running when your turn ends are stopped. Use them only when splitting the work clearly helps.
`, limit)
}

// SubagentPrompt is the first turn of a subagent a worker started.
func SubagentPrompt(task store.Task, sub store.Subagent, ruleSet rules.Set) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a subagent that %s started in a Switch run. You report only to %s.\n", sub.Name, sub.Parent, sub.Parent)
	fmt.Fprintf(&b, "Work only inside this git worktree: %s. %s and its other subagents work in it too.\n\n", task.Worktree, sub.Parent)
	fmt.Fprintf(&b, "%s is working on the task %q.\n\nYour job:\n%s\n", sub.Parent, task.Title, sub.Brief)
	if len(sub.Files) > 0 {
		writeList(&b, "Files you may edit (edit nothing else)", sub.Files)
	} else {
		b.WriteString("\nYou are read-only: do not create, edit, or delete any file.\n")
	}
	writeRules(&b, ruleSet)
	b.WriteString("\nDo not commit, push, merge, or switch branches, and do not start subagents of your own.\n")
	b.WriteString("When you are done, give your result with the switch_report MCP tool (or `$SWITCH_BIN __control agent report <result>`),\n")
	fmt.Fprintf(&b, "then reply with the same result. Keep it short and concrete: %s reads only that.\n", sub.Parent)
	return b.String()
}

// ReviewPrompt asks the lead to review one verified task.
func ReviewPrompt(task store.Task, worker store.RunAgent, diff, evidence string, ruleSet rules.Set) string {
	if len(diff) > maxReviewDiff {
		diff = diff[:maxReviewDiff] + "\n… (diff truncated)"
	}
	var b strings.Builder
	b.WriteString("You are the lead of a Switch run. Review this change; do not edit files.\n\n")
	fmt.Fprintf(&b, "Task: %s\n\n%s\n", task.Title, task.Brief)
	fmt.Fprintf(&b, "\nWorker: %s (%s)\n", worker.Name, worker.Driver)
	writeList(&b, "Acceptance criteria", task.Acceptance)
	fmt.Fprintf(&b, "\nVerification evidence:\n%s\n", evidence)
	if task.Summary != "" {
		fmt.Fprintf(&b, "\nWorker summary:\n%s\n", task.Summary)
	}
	fmt.Fprintf(&b, "\nDiff:\n```diff\n%s\n```\n", diff)
	writeRules(&b, ruleSet)
	b.WriteString(`
Start your reply with exactly one line: APPROVE or REJECT.
Then give your reasons. On REJECT, say precisely what the worker must change.
The user makes the final merge decision.
`)
	return b.String()
}

func writeList(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s:\n", title)
	for _, item := range items {
		fmt.Fprintf(b, "- %s\n", item)
	}
}

func writeRules(b *strings.Builder, ruleSet rules.Set) {
	if ruleSet.Empty() {
		return
	}
	b.WriteString("\nOperating rules:\n")
	b.WriteString(ruleSet.Text())
	b.WriteString("\n")
}
