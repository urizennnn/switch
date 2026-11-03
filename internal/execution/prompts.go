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
	if task.Feedback != "" {
		fmt.Fprintf(&b, "\nThis is attempt %d. The previous attempt was sent back:\n%s\n", task.Attempts, task.Feedback)
	}
	writeRules(&b, ruleSet)
	b.WriteString("\nDo not commit, push, merge, or switch branches; Switch commits and integrates approved work.\n")
	b.WriteString("If the task cannot be done, say why and call the switch_block tool if it is available.\n")
	b.WriteString("When you are done, reply with a short summary of what you changed and anything left open.\n")
	return b.String()
}

// ReviewPrompt asks the lead to review one verified task.
func ReviewPrompt(task store.Task, diff, evidence string, ruleSet rules.Set) string {
	if len(diff) > maxReviewDiff {
		diff = diff[:maxReviewDiff] + "\n… (diff truncated)"
	}
	var b strings.Builder
	b.WriteString("You are the lead of a Switch run. Review this change; do not edit files.\n\n")
	fmt.Fprintf(&b, "Task: %s\n\n%s\n", task.Title, task.Brief)
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
