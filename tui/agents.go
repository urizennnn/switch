package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/store"
)

// The main pane can follow any agent instead of the lead's chat: a worker
// with its subagents as cards, or one subagent. focus is "" for the lead,
// "builder-1" for a worker, or "builder-1/scout" for a subagent. Agent views
// are read-only; workers handle their subagents themselves.

// subagentsOf is a worker's subagents for its current task, or for its most
// recent one when it is idle.
func (m model) subagentsOf(worker string) []store.Subagent {
	if _, inRun := m.agentTask(worker); !inRun {
		var chat []store.Subagent
		for _, sa := range m.chatSubs {
			if sa.Parent == worker {
				chat = append(chat, sa)
			}
		}
		if len(chat) > 0 {
			return chat
		}
	}
	taskID := ""
	if task, ok := m.agentTask(worker); ok {
		taskID = task.ID
	} else {
		for _, sa := range m.subagents {
			if sa.Parent == worker {
				taskID = sa.TaskID // the latest, since they are in creation order
			}
		}
	}
	var subs []store.Subagent
	for _, sa := range m.subagents {
		if sa.Parent == worker && sa.TaskID == taskID {
			subs = append(subs, sa)
		}
	}
	return subs
}

func (m model) runningSubagents() int {
	n := 0
	for _, sa := range append(m.subagents[:len(m.subagents):len(m.subagents)], m.chatSubs...) {
		if sa.Status == execution.SubRunning {
			n++
		}
	}
	return n
}

// agentOrder is the order tab cycles through: the lead, then each worker
// followed by its subagents.
func (m model) agentOrder() []string {
	order := []string{""}
	for _, worker := range m.workers() {
		order = append(order, worker.Name)
		for _, sa := range m.subagentsOf(worker.Name) {
			order = append(order, worker.Name+"/"+sa.Name)
		}
	}
	return order
}

func (m *model) cycleFocus(step int) {
	order := m.agentOrder()
	i := 0
	for j, name := range order {
		if name == m.focus {
			i = j
		}
	}
	m.setFocus(order[(i+step+len(order))%len(order)])
}

func (m *model) setFocus(name string) {
	m.focus, m.card, m.agentTop = name, 0, -1
}

// focusUp goes from a subagent to its worker, and from a worker to the lead.
func (m *model) focusUp() {
	parent, _, isSub := strings.Cut(m.focus, "/")
	if isSub {
		m.setFocus(parent)
		return
	}
	m.setFocus("")
}

// openAgent is /agent <name>: a worker, a subagent as worker/name, or a
// subagent's own name when it is unique.
func (m *model) openAgent(name string) {
	name = strings.ReplaceAll(strings.TrimSpace(name), " › ", "/")
	if name == "" || name == m.lead.Name {
		m.setFocus("")
		return
	}
	var matches []string
	for _, candidate := range m.agentOrder()[1:] {
		_, sub, _ := strings.Cut(candidate, "/")
		if candidate == name || sub == name {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 1:
		m.setFocus(matches[0])
	case 0:
		m.fail("no agent named "+name, "tab cycles through the lead, workers and subagents")
	default:
		m.fail(name+" is ambiguous", "use worker/name: "+strings.Join(matches, ", "))
	}
}

// breadcrumb is the main pane title while following an agent.
func (m model) breadcrumb() string {
	return strings.Join(append([]string{m.lead.Name}, strings.Split(m.focus, "/")...), " › ")
}

func (m model) focusedSubagent() (store.Subagent, bool) {
	worker, name, ok := strings.Cut(m.focus, "/")
	if !ok {
		return store.Subagent{}, false
	}
	for _, sa := range m.subagentsOf(worker) {
		if sa.Name == name {
			return sa, true
		}
	}
	return store.Subagent{}, false
}

func subagentMark(status string) string {
	switch status {
	case execution.SubRunning:
		return warningStyle.Render("◐")
	case execution.SubDone:
		return successStyle.Render("✓")
	case execution.SubFailed:
		return errorStyle.Render("✕")
	default:
		return dimStyle.Render("■")
	}
}

// subagentRows are a worker's subagents under it in the sidebar, as a tree,
// or one summary line when there are more than fit.
func (m model) subagentRows(worker string, inner int) string {
	subs := m.subagentsOf(worker)
	if len(subs) == 0 {
		return ""
	}
	if len(subs) > 3 {
		running := 0
		for _, sa := range subs {
			if sa.Status == execution.SubRunning {
				running++
			}
		}
		return dimStyle.Render("  └ ") + mutedStyle.Render(truncate(fmt.Sprintf("%d subagents · %d running", len(subs), running), inner-4)) + "\n"
	}
	var b strings.Builder
	for i, sa := range subs {
		branch := "├ "
		if i == len(subs)-1 {
			branch = "└ "
		}
		nameStyle := mutedStyle
		if m.focus == worker+"/"+sa.Name {
			nameStyle = activeStyle
		}
		status := truncate(sa.Status, max(4, inner/3))
		name := truncate(sa.Name, max(4, inner-8-lipgloss.Width(status)))
		gap := max(1, inner-6-lipgloss.Width(name)-lipgloss.Width(status))
		b.WriteString(dimStyle.Render("  "+branch) + subagentMark(sa.Status) + " " + nameStyle.Render(name) + strings.Repeat(" ", gap) + dimStyle.Render(status) + "\n")
	}
	return b.String()
}

// agentContent is the main pane while following a worker or subagent.
func (m model) agentContent(width, height int) string {
	inner := max(10, width-4)
	head := m.agentHead(inner)
	if head == nil {
		return mutedStyle.Render(m.focus+" has left the run") + "\n" + dimStyle.Render("esc goes back")
	}
	headText := strings.Join(head, "\n")
	lines := strings.Split(m.agentBody(inner), "\n")
	body := strings.Join(window(lines, m.agentTop, m.agentRoom(lipgloss.Height(headText))), "\n")
	return headText + "\n\n" + body + "\n\n" + dimStyle.Render(m.agentHint())
}

// agentHead is the followed agent's header: who it is, what it is doing,
// and a worker's subagent cards. It is nil when the agent is gone.
func (m model) agentHead(inner int) []string {
	var head []string
	if sa, ok := m.focusedSubagent(); ok {
		head = append(head,
			subagentMark(sa.Status)+" "+textStyle.Bold(true).Render(sa.Name)+mutedStyle.Render(" · "+sa.Status+" · "+sa.Driver),
			mutedStyle.Render(truncate(firstLine(sa.Brief), inner)))
		if len(sa.Files) > 0 {
			head = append(head, dimStyle.Render(truncate("edits "+strings.Join(sa.Files, ", "), inner)))
		} else {
			head = append(head, dimStyle.Render("read-only"))
		}
		if sa.Summary != "" && sa.Status != execution.SubRunning {
			head = append(head, "", titleStyle.Render("RESULT"), textStyle.Render(truncateLines(lipgloss.NewStyle().Width(inner).Render(sa.Summary), 6)))
		}
		return head
	}
	agent, ok := m.fleetAgent(m.focus)
	if !ok {
		return nil
	}
	status := m.team.status(agent.Name)
	if task, ok := m.agentTask(agent.Name); ok {
		status = task.Status + " · " + task.Title
	}
	head = append(head, textStyle.Bold(true).Render(agent.Name)+mutedStyle.Render(" · "+agent.Driver+" · "+truncate(status, max(10, inner-20))))
	if subs := m.subagentsOf(agent.Name); len(subs) > 0 {
		head = append(head, "", titleStyle.Render("SUBAGENTS"))
		for i, sa := range subs {
			head = append(head, m.subagentCard(sa, i == m.card, inner))
		}
	}
	return head
}

// agentBody is the followed agent's transcript, with its activity line.
func (m model) agentBody(inner int) string {
	name := m.focus[strings.LastIndexByte(m.focus, '/')+1:]
	body := renderEntries(m.agentEntries[m.focus], inner, name, m.frame, m.showThoughts)
	if since, busy := m.agentBusy[m.focus]; busy {
		body = strings.TrimLeft(body+"\n\n", "\n") + activityLine(activity(m.agentEntries[m.focus]), since, m.frame, "ctrl+x to stop")
	}
	if body == "" {
		body = dimStyle.Render("nothing from " + name + " in this session yet")
	}
	return body
}

// subagentCard is one subagent in its worker's view.
func (m model) subagentCard(sa store.Subagent, selected bool, inner int) string {
	marker := "  "
	nameStyle := textStyle
	if selected {
		marker, nameStyle = "› ", activeStyle
	}
	detail := firstLine(sa.Brief)
	if sa.Status != execution.SubRunning && sa.Summary != "" {
		detail = firstLine(sa.Summary)
	}
	left := marker + subagentMark(sa.Status) + " " + nameStyle.Render(fmt.Sprintf("%-10s", truncate(sa.Name, 10))) + " "
	status := dimStyle.Render(" " + sa.Status)
	detail = truncate(detail, max(4, inner-lipgloss.Width(left)-lipgloss.Width(status)-1))
	gap := max(1, inner-lipgloss.Width(left)-lipgloss.Width(detail)-lipgloss.Width(status))
	return left + mutedStyle.Render(detail) + strings.Repeat(" ", gap) + status
}

// agentHint is the key hint under an agent view.
func (m model) agentHint() string {
	hint := "tab next agent · esc back"
	if _, isSub := m.focusedSubagent(); !isSub && len(m.subagentsOf(m.focus)) > 0 {
		hint = "←/→ select · enter open · esc back"
	}
	if _, busy := m.agentBusy[m.focus]; busy {
		hint = "ctrl+x stop · " + hint
	}
	return hint + " · ctrl+t thinking"
}
