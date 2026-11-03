package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/execution"
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

// taskAction runs an engine operation off the UI goroutine. It reuses the
// running engine so its scheduler wakes up, or builds a short-lived one.
func (m model) taskAction(action, taskID, reason string) tea.Cmd {
	engine := m.engine
	if engine == nil {
		var err error
		engine, err = m.newEngine(m.ctx)
		if err != nil {
			return func() tea.Msg { return taskActionMsg{title: action + " failed", body: err.Error()} }
		}
	}
	ctx := m.ctx
	name := shortTaskID(taskID)
	return func() tea.Msg {
		actionCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		var err error
		body := ""
		switch action {
		case "approve":
			err = engine.Approve(actionCtx, taskID)
			body = name + " merged into the integration branch"
		case "reject":
			err = engine.Reject(actionCtx, taskID, reason)
			body = name + " sent back to its worker\n/run continues the run"
		case "retry":
			err = engine.Retry(actionCtx, taskID)
			body = name + " requeued\n/run continues the run"
		case "diff":
			body, err = engine.Diff(actionCtx, taskID)
		}
		if err != nil {
			return taskActionMsg{title: action + " " + name + " failed", body: err.Error()}
		}
		return taskActionMsg{title: action + " " + name, body: body}
	}
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

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
