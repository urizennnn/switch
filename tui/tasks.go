package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/runcli"
	"github.com/urizennnn/switch/internal/store"
)

// taskPicker lists the run's tasks. Keys act on the selected task: enter
// shows details, a approves, r rejects (with a reason typed in the input),
// d shows the diff, and t retries a blocked task.
type taskPicker struct {
	index int
	err   string
}

type taskActionMsg struct {
	title string
	body  string
}

func shortTaskID(id string) string {
	if i := strings.LastIndexByte(id, ':'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// findTask resolves a plan task ID ("add-flag") or a full ID.
func (m model) findTask(name string) (store.Task, bool) {
	for _, task := range m.tasks {
		if task.ID == name || shortTaskID(task.ID) == name {
			return task, true
		}
	}
	return store.Task{}, false
}

func (m model) handleTaskPickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	p := m.taskPicker
	if len(m.tasks) == 0 {
		m.taskPicker = nil
		return m, nil
	}
	p.index = min(p.index, len(m.tasks)-1)
	task := m.tasks[p.index]
	switch msg.String() {
	case "esc":
		m.taskPicker = nil
	case "up":
		p.index = max(0, p.index-1)
		p.err = ""
	case "down":
		p.index = min(len(m.tasks)-1, p.index+1)
		p.err = ""
	case "enter":
		m.taskPicker = nil
		m.setModal("task "+shortTaskID(task.ID), m.taskDetail(task))
	case "a":
		m.taskPicker = nil
		return m, m.taskAction("approve", task.ID, "")
	case "r":
		m.taskPicker = nil
		m.input.SetValue("/reject " + shortTaskID(task.ID) + " ")
		m.input.CursorEnd()
	case "d":
		m.taskPicker = nil
		return m, m.taskAction("diff", task.ID, "")
	case "t":
		m.taskPicker = nil
		return m, m.taskAction("retry", task.ID, "")
	}
	return m, nil
}

// runID is the run's ID, or "" once a failed read of the store has cleared
// the run, as a dashboard tick can while /fix's shell is open.
func (m model) runID() string {
	if m.run == nil {
		return ""
	}
	return m.run.ID
}

// taskEngine is the running engine, so its scheduler wakes up, or a
// short-lived one.
func (m model) taskEngine() (*execution.Engine, error) {
	if m.engine != nil {
		return m.engine, nil
	}
	return m.newEngine(m.ctx, nil)
}

// taskAction runs an engine operation off the UI goroutine.
func (m model) taskAction(action, taskID, reason string) tea.Cmd {
	engine, err := m.taskEngine()
	if err != nil {
		return func() tea.Msg { return taskActionMsg{title: action + " failed", body: err.Error()} }
	}
	// No deadline of its own: the engine bounds each setup and verify command
	// of an approval's integration check by the run's verify timeout.
	ctx, state, runID := m.ctx, m.store, m.runID()
	name := shortTaskID(taskID)
	next := "\n/run continues the run"
	if m.job == "run" || m.runOwner > 0 {
		next = "\nthe running run picks it up"
	}
	return func() tea.Msg {
		var err error
		body := ""
		switch action {
		case "approve":
			if err = engine.Approve(ctx, taskID); err == nil {
				body = name + " merged into the integration branch" + prDescription(ctx, state, runID)
			}
		case "reject":
			err = engine.Reject(ctx, taskID, reason)
			body = name + " sent back to its worker" + next
		case "retry":
			err = engine.Retry(ctx, taskID)
			body = name + " requeued" + next
		case "fix":
			err = engine.Resubmit(ctx, taskID)
			body = name + " passed its checks and waits for your review\n/approve " + name + " merges it"
		case "diff":
			body, err = engine.Diff(ctx, taskID)
			body = colorDiff(body)
		}
		if err != nil {
			return taskActionMsg{title: action + " " + name + " failed", body: err.Error()}
		}
		return taskActionMsg{title: action + " " + name, body: body}
	}
}

// fixShellMsg ends the shell /fix opened in a task's worktree.
type fixShellMsg struct {
	taskID string
	err    error
}

// fixedMsg is the check of what the user changed in that shell.
type fixedMsg taskActionMsg

// fixTask is /fix: the user's shell in the task's worktree, after which
// Switch checks what they changed.
func (m model) fixTask(name string) (tea.Model, tea.Cmd) {
	m.refreshDashboard() // the shell must not open where an agent works now
	task, ok := m.findTask(name)
	if !ok {
		m.fail("unknown task "+name, "/tasks lists them")
		return m, nil
	}
	switch err := execution.Fixable(task); {
	case m.fixing != "":
		m.notify("checking your fix of "+shortTaskID(m.fixing), "wait for its check to finish")
	case err != nil:
		m.fail("cannot fix "+name, err.Error())
	case (task.Status == execution.StatusQueued || task.Status == execution.StatusInterrupted) && (m.job == "run" || m.runOwner > 0):
		m.notify("run in progress", "it would hand "+name+" to a worker while you work; /stop first")
	default:
		return m, tea.ExecProcess(fixShell(task.Worktree, shortTaskID(task.ID)), func(err error) tea.Msg { return fixShellMsg{taskID: task.ID, err: err} })
	}
	return m, nil
}

// fixShell opens the user's shell in dir, after a line on how /fix ends.
func fixShell(dir, task string) *exec.Cmd {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	note := "Fixing " + task + " in its worktree. Exit this shell when you are done: Switch commits what changed and runs the task's checks."
	cmd := exec.Command("/bin/sh", "-c", `printf '%s\n\n' "$1"; exec "$0"`, shell, note)
	cmd.Dir = dir
	return cmd
}

// checkFix checks the task once its shell exits. A shell exits with the
// status of the last command run in it, which says nothing about the fix.
func (m model) checkFix(msg fixShellMsg) (tea.Model, tea.Cmd) {
	var exit *exec.ExitError
	if msg.err != nil && !errors.As(msg.err, &exit) {
		m.fail("could not open your shell", msg.err.Error())
		return m, nil
	}
	m.fixing = msg.taskID
	m.notify("checking your fix of "+shortTaskID(msg.taskID), "it goes back to review once its checks pass")
	check := m.taskAction("fix", msg.taskID, "")
	return m, func() tea.Msg { return fixedMsg(check().(taskActionMsg)) }
}

// approveAll merges tasks in the order given and stops at the first that
// fails, saying what merged and what still waits.
func (m model) approveAll(ids []string) tea.Cmd {
	engine, err := m.taskEngine()
	if err != nil {
		return func() tea.Msg { return taskActionMsg{title: "approve failed", body: err.Error()} }
	}
	ctx, state, runID := m.ctx, m.store, m.runID()
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = shortTaskID(id)
	}
	return func() tea.Msg {
		for i, id := range ids {
			err := engine.Approve(ctx, id)
			if err == nil {
				continue
			}
			body := names[i] + " did not merge: " + err.Error()
			if i > 0 {
				body = "merged " + strings.Join(names[:i], ", ") + "\n\n" + body
			}
			if i+1 < len(names) {
				body += "\n\nstill waiting for review: " + strings.Join(names[i+1:], ", ")
			}
			return taskActionMsg{title: "approve all stopped at " + names[i], body: body}
		}
		return taskActionMsg{title: "approve all", body: strings.Join(names, ", ") + " merged into the integration branch" + prDescription(ctx, state, runID)}
	}
}

// prDescription writes the PR description of a run that has completed, as
// the last approval can now complete it, and says where it is.
func prDescription(ctx context.Context, state *store.Store, runID string) string {
	run, ok, err := state.GetRun(ctx, runID)
	if err != nil || !ok || run.Status != execution.RunCompleted {
		return runcli.ChecksFailed(ctx, state, run)
	}
	path, err := runcli.WriteSummary(ctx, state, config.StateDir(), runID)
	if err != nil {
		return "\nThe PR description was not written: " + err.Error()
	}
	return "\nPR description: " + shortenHome(path)
}

// colorDiff colors a unified diff's added, removed and hunk lines. One color
// per line keeps a long diff cheap to redraw, which Glamour's highlighting is not.
func colorDiff(diff string) string {
	added, removed, hunk := lipgloss.NewStyle().Foreground(green), lipgloss.NewStyle().Foreground(red), lipgloss.NewStyle().Foreground(teal)
	lines := strings.Split(diff, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "+"):
			lines[i] = added.Render(line)
		case strings.HasPrefix(line, "-"):
			lines[i] = removed.Render(line)
		case strings.HasPrefix(line, "@@"):
			lines[i] = hunk.Render(line)
		}
	}
	return strings.Join(lines, "\n")
}

func (m model) taskDetail(task store.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", task.Title)
	fmt.Fprintf(&b, "status    %s · attempt %d\n", task.Status, task.Attempts)
	fmt.Fprintf(&b, "role      %s · worker %s\n", task.Role, orNone(task.Assignee))
	fmt.Fprintf(&b, "branch    %s\n", orNone(task.Branch))
	if task.Worktree != "" {
		fmt.Fprintf(&b, "worktree  %s\n", task.Worktree)
	}
	if task.Commit != "" {
		fmt.Fprintf(&b, "commit    %s\n", shortID(task.Commit))
	}
	if len(task.Deps) > 0 {
		deps := make([]string, len(task.Deps))
		for i, dep := range task.Deps {
			deps[i] = shortTaskID(dep)
		}
		fmt.Fprintf(&b, "deps      %s\n", strings.Join(deps, ", "))
	}
	section := func(title, text string) {
		if strings.TrimSpace(text) != "" {
			fmt.Fprintf(&b, "\n%s\n%s\n", title, strings.TrimSpace(text))
		}
	}
	section("last error", task.LastError)
	section("feedback for the next attempt", task.Feedback)
	section("lead review", task.Review)
	section("worker summary", task.Summary)
	var subs []string
	for _, sa := range m.subagents {
		if sa.TaskID == task.ID {
			line := fmt.Sprintf("  %-10s %-11s %s", sa.Name, sa.Status, firstLine(sa.Brief))
			if sa.Summary != "" {
				line += "\n  " + strings.Repeat(" ", 22) + "→ " + firstLine(sa.Summary)
			}
			subs = append(subs, line)
		}
	}
	if len(subs) > 0 {
		fmt.Fprintf(&b, "\nsubagents\n%s\n", strings.Join(subs, "\n"))
	}
	if evidence, err := m.store.Verifications(m.ctx, task.ID); err == nil && len(evidence) > 0 {
		b.WriteString("\nverification\n")
		for _, v := range evidence {
			result := "passed"
			if v.TimedOut {
				result = "timed out"
			} else if v.ExitCode != 0 {
				result = fmt.Sprintf("exit %d", v.ExitCode)
			}
			fmt.Fprintf(&b, "  #%d %-11s %-9s %s\n", v.Attempt, v.Scope, result, v.Command)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m model) taskPickerContent(width int) string {
	var b strings.Builder
	if len(m.tasks) == 0 {
		return "no tasks\n/plan <goal> creates a run"
	}
	for i, task := range m.tasks {
		worker := ""
		if task.Assignee != "" {
			worker = " · " + task.Assignee
		}
		if n := countSubagents(m.subagents, task.ID); n > 0 {
			worker += fmt.Sprintf(" · %d subagents", n)
		}
		line := fmt.Sprintf("%-14s %-11s %s%s", truncate(shortTaskID(task.ID), 14), task.Status, task.Title, worker)
		style := mutedStyle
		switch {
		case i == m.taskPicker.index:
			style = activeStyle
		case task.Status == execution.StatusBlocked:
			style = errorStyle
		case task.Status == execution.StatusReview:
			style = warningStyle
		case task.Status == execution.StatusMerged:
			style = dimStyle
		}
		b.WriteString(style.Width(max(10, width-4)).Render(truncate(line, width-4)))
		b.WriteString("\n")
	}
	if m.taskPicker.err != "" {
		b.WriteString("\n" + errorStyle.Render(m.taskPicker.err) + "\n")
	}
	return b.String()
}

func (m model) agentsContent() string {
	if len(m.runAgents) == 0 {
		return "no run yet\n/plan <goal> creates one"
	}
	var b strings.Builder
	for _, agent := range m.runAgents {
		current, lastErr := "idle", ""
		for _, task := range m.tasks {
			if task.Assignee != agent.Name {
				continue
			}
			if isActive(task.Status) {
				current = task.Status + " " + shortTaskID(task.ID)
			}
			if task.LastError != "" {
				lastErr = shortTaskID(task.ID) + ": " + firstLine(task.LastError)
			}
		}
		if agent.Role == "lead" {
			current = "plans and reviews"
		}
		fmt.Fprintf(&b, "%-12s %-8s %-9s %s\n", agent.Name, agent.Role, agent.Driver, current)
		if lastErr != "" {
			fmt.Fprintf(&b, "             last error  %s\n", truncate(lastErr, 60))
		}
	}
	if usage, err := m.store.RunUsage(m.ctx, m.run.ID); err == nil && usage.TotalTokens > 0 {
		fmt.Fprintf(&b, "\ntokens this run: %d\n", usage.TotalTokens)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m model) logContent() string {
	events, err := m.store.Events(m.ctx, m.run.ID, 40)
	if err != nil {
		return err.Error()
	}
	if len(events) == 0 {
		return "no events yet"
	}
	var b strings.Builder
	for _, e := range events {
		who := e.Agent
		if e.TaskID != "" {
			who = strings.TrimSpace(shortTaskID(e.TaskID) + " " + who)
		}
		fmt.Fprintf(&b, "%s  %-6s %-20s %s\n", e.CreatedAt.Local().Format("15:04:05"), e.Kind, truncate(who, 20), firstLine(e.Message))
	}
	return strings.TrimRight(b.String(), "\n")
}

func isActive(status string) bool {
	switch status {
	case execution.StatusRunning, execution.StatusVerifying, execution.StatusReviewing, execution.StatusApproved:
		return true
	}
	return false
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[:i] + " …"
	}
	return text
}

// lastLine is the final line of command output, usually its result.
func lastLine(text string) string {
	text = strings.TrimSpace(text)
	return text[strings.LastIndexByte(text, '\n')+1:]
}

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

func countSubagents(subs []store.Subagent, taskID string) int {
	n := 0
	for _, sa := range subs {
		if sa.TaskID == taskID {
			n++
		}
	}
	return n
}
