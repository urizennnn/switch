package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func (m model) View() tea.View {
	width := m.width
	if width <= 0 {
		width = 80
	}

	var body string
	switch {
	case m.modal != "":
		body = m.modalView()
	case m.home:
		body = m.dashboardView()
		if m.paletteOpen {
			body = overlayBottom(body, m.paletteView(), m.conversationHeight())
		}
	default:
		body = lipgloss.NewStyle().
			Padding(0, 2, 0, 2).
			Height(m.conversationHeight()).
			Render(m.viewport.View())
		if m.paletteOpen {
			body = overlayBottom(body, m.paletteView(), m.conversationHeight())
		}
	}

	return tea.NewView(lipgloss.JoinVertical(
		lipgloss.Left,
		body,
		m.inputBar(width),
		m.statusLine(width),
	))
}

func (m *model) layout() {
	width := m.width
	if width <= 0 {
		width = 80
	}
	m.viewport.SetWidth(max(20, width-2))
	m.viewport.SetHeight(m.conversationHeight())
	m.input.SetWidth(max(10, width-4))
}

func (m model) conversationHeight() int {
	height := m.height
	if height <= 0 {
		height = 24
	}
	return max(5, height-5)
}

// overlayBottom draws overlay over the last lines of base without changing
// the surrounding layout. The command palette floats over the conversation
// instead of resizing it.
func overlayBottom(base, overlay string, height int) string {
	baseLines := strings.Split(strings.TrimRight(base, "\n"), "\n")
	for len(baseLines) < height {
		baseLines = append(baseLines, "")
	}
	if len(baseLines) > height {
		baseLines = baseLines[:height]
	}

	overlayLines := strings.Split(strings.TrimRight(overlay, "\n"), "\n")
	if len(overlayLines) > len(baseLines) {
		overlayLines = overlayLines[len(overlayLines)-len(baseLines):]
	}
	start := len(baseLines) - len(overlayLines)
	for i, line := range overlayLines {
		baseLines[start+i] = line
	}
	return strings.Join(baseLines, "\n")
}

func (m model) inputBar(width int) string {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(width - 2).
		Render(dimStyle.Render("› ") + m.input.View())
}

func (m model) statusLine(width int) string {
	left := fmt.Sprintf("%s · %s · %s", m.chat.Agent, m.chat.Model, shortID(m.chat.Session.ID))
	right := fmt.Sprintf("%s · %s", m.state, m.ruleSummary())
	return lipgloss.NewStyle().
		Foreground(dim).
		Padding(0, 1).
		Width(width).
		Render(left + "  ·  " + right)
}

func (m model) paletteView() string {
	width := m.width
	if width <= 0 {
		width = 80
	}

	var b strings.Builder
	for i, command := range m.palette {
		line := fmt.Sprintf("%-28s  %s", command.label(), command.Description)
		style := mutedStyle
		if i == m.paletteIndex {
			style = paletteSelected
		}
		b.WriteString(style.Render(line))
		b.WriteString("\n")
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(width - 2).
		Render(strings.TrimRight(b.String(), "\n"))
}

func (m model) dashboardView() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	colWidth := max(24, (width/2)-4)

	left := lipgloss.NewStyle().
		Width(colWidth).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			lipgloss.NewStyle().Foreground(accent).Bold(true).Render("switch"),
			dimStyle.Render("project"),
			mutedStyle.Render(truncate(m.chat.Project, colWidth-4)),
			"\n",
			dimStyle.Render("agent"),
			mutedStyle.Render(m.chat.Agent+" · "+m.chat.Model),
			dimStyle.Render(fmt.Sprintf("session %s (%s)", shortID(m.chat.Session.ID), sessionState(m.status.Resumed))),
			dimStyle.Render("state "+m.state),
			"\n",
			dimStyle.Render(m.ruleSummary()),
			dimStyle.Render(fmt.Sprintf("fleet %d agents", len(m.dashboardFleet()))),
		))

	var run strings.Builder
	run.WriteString(sectionTitle("run"))
	if m.run == nil {
		run.WriteString(mutedStyle.Render("no run yet · use /plan <goal>"))
		run.WriteString("\n")
	} else {
		queued, running, done := m.taskCounts()
		run.WriteString(mutedStyle.Render(fmt.Sprintf("%s · %s", shortID(m.run.ID), m.run.Status)))
		run.WriteString("\n")
		run.WriteString(mutedStyle.Render("goal: " + truncate(m.run.Goal, colWidth-4)))
		run.WriteString("\n")
		run.WriteString(mutedStyle.Render(fmt.Sprintf("tasks %d queued · %d running · %d done", queued, running, done)))
		run.WriteString("\n")
		run.WriteString(dimStyle.Render("agents " + fmt.Sprintf("%d", len(m.runAgents))))
	}

	var fleet strings.Builder
	fleet.WriteString(sectionTitle("fleet"))
	for _, agent := range m.dashboardFleet() {
		fleet.WriteString(mutedStyle.Render(fmt.Sprintf("%-10s %-8s %s", agent.Name, agent.Role, agent.Driver)))
		fleet.WriteString("\n")
	}

	var commands strings.Builder
	commands.WriteString(sectionTitle("commands"))
	for i, command := range slashCommands {
		if i >= 4 {
			commands.WriteString(dimStyle.Render("type / for all commands"))
			commands.WriteString("\n")
			break
		}
		commands.WriteString(mutedStyle.Render(fmt.Sprintf("%-24s %s", command.label(), command.Description)))
		commands.WriteString("\n")
	}

	var keys strings.Builder
	keys.WriteString(sectionTitle("keys"))
	for _, line := range keyLines() {
		keys.WriteString(mutedStyle.Render(line))
		keys.WriteString("\n")
	}

	right := lipgloss.NewStyle().
		Width(colWidth).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			run.String(),
			"",
			fleet.String(),
			"",
			commands.String(),
			"",
			keys.String(),
		))

	dividerHeight := max(lipgloss.Height(left), lipgloss.Height(right))
	var dividerStr strings.Builder
	for range dividerHeight {
		dividerStr.WriteString("│\n")
	}
	mid := lipgloss.NewStyle().Foreground(border).Render(dividerStr.String())

	columns := lipgloss.JoinHorizontal(lipgloss.Top, left, mid, right)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Width(width - 2).
		Height(m.conversationHeight()).
		Render(columns)
}

func (m model) modalView() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	height := m.conversationHeight()

	content := sectionTitle(m.modalTitle) + "\n" + m.modal
	content = truncateLines(content, max(3, height-4))
	content += "\n\n" + dimStyle.Render("esc or enter to close")

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Padding(1, 2).
		Width(width - 2).
		Height(height).
		Render(content)
}

func sectionTitle(text string) string {
	return lipgloss.NewStyle().Foreground(teal).Bold(true).Render(text) + "\n"
}

func sessionState(resumed bool) string {
	if resumed {
		return "resumed"
	}
	return "new"
}

func truncate(value string, limit int) string {
	if limit < 1 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

func truncateLines(value string, limit int) string {
	lines := strings.Split(value, "\n")
	if len(lines) <= limit {
		return value
	}
	return strings.Join(lines[:limit], "\n") + "\n" + dimStyle.Render("…")
}
