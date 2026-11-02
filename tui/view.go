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

	return tea.NewView(lipgloss.JoinVertical(
		lipgloss.Left,
		m.headerView(width),
		m.bodyView(width),
		inputBar(width, m.input.View()),
		m.statusView(width),
	))
}

func (m *model) layout() {
	width := m.width
	if width <= 0 {
		width = 80
	}
	_, mainWidth := m.paneWidths(width)

	m.viewport.SetWidth(max(20, mainWidth-4))
	m.viewport.SetHeight(max(5, m.bodyHeight()-3))
	m.input.SetWidth(max(10, width-4))
}

func (m model) bodyHeight() int {
	height := m.height
	if height <= 0 {
		height = 24
	}
	return max(8, height-7)
}

func (m model) paneWidths(width int) (sidebarWidth, mainWidth int) {
	sidebarWidth = min(34, max(26, width/4))
	mainWidth = max(24, width-sidebarWidth-5)
	return sidebarWidth, mainWidth
}

func (m model) headerView(width int) string {
	left := brandStyle.Render("switch") +
		dimStyle.Render("  "+truncate(m.chat.Project, max(12, width/3)))

	parts := []string{m.lead.Name}
	if m.chat.Model != "" {
		parts = append(parts, m.chat.Model)
	}
	parts = append(parts, shortID(m.chat.Session.ID))
	right := mutedStyle.Render(strings.Join(parts, " · "))

	return appHeader(width, left, right)
}

func (m model) bodyView(width int) string {
	sidebarWidth, mainWidth := m.paneWidths(width)
	height := m.bodyHeight()

	sidebar := panel(sidebarWidth, height, "workspace", m.sidebarContent(sidebarWidth))
	mainContent := m.mainContent(mainWidth, height)
	if m.paletteOpen {
		mainContent = overlayBottom(mainContent, m.paletteView(mainWidth), max(5, height-3))
	}
	main := panel(mainWidth, height, m.mainTitle(), mainContent)

	return lipgloss.JoinHorizontal(lipgloss.Top, sidebar, " ", main)
}

func (m model) sidebarContent(width int) string {
	var b strings.Builder

	b.WriteString(section("agents"))
	for _, agent := range m.dashboardFleet() {
		marker := "○"
		style := mutedStyle
		if agent.Name == m.lead.Name {
			marker = "●"
			style = selectedStyle
		}
		line := fmt.Sprintf("%s %-10s %s", marker, agent.Name, agent.Role)
		b.WriteString(style.Width(max(10, width-4)).Render(truncate(line, width-4)))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  " + truncate(agent.Driver, width-6)))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(section("run"))
	if m.run == nil {
		b.WriteString(mutedStyle.Render("no run yet"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("/plan <goal>"))
		b.WriteString("\n")
	} else {
		queued, running, done := m.taskCounts()
		b.WriteString(mutedStyle.Render(shortID(m.run.ID) + " · " + m.run.Status))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(fmt.Sprintf("q:%d  r:%d  d:%d", queued, running, done)))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("agents " + fmt.Sprintf("%d", len(m.runAgents))))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(section("rules"))
	b.WriteString(mutedStyle.Render(truncate(m.ruleSummary(), width-4)))
	b.WriteString("\n")

	return b.String()
}

func (m model) mainTitle() string {
	switch {
	case m.modal != "":
		if m.modalTitle != "" {
			return m.modalTitle
		}
		return "output"
	case m.home:
		return "overview"
	default:
		return "conversation"
	}
}

func (m model) mainContent(width, height int) string {
	switch {
	case m.modal != "":
		content := m.modal + "\n\n" + dimStyle.Render("esc or enter to close")
		return truncateLines(content, max(3, height-3))
	case m.home:
		return m.overviewContent(width)
	default:
		return m.viewport.View()
	}
}

func (m model) overviewContent(width int) string {
	var b strings.Builder

	b.WriteString(section("lead"))
	b.WriteString(keyValue("agent", m.lead.Name))
	b.WriteString("\n")
	b.WriteString(keyValue("driver", m.chat.Agent))
	b.WriteString("\n")
	b.WriteString(keyValue("model", m.chat.Model))
	b.WriteString("\n")
	b.WriteString(keyValue("session", shortID(m.chat.Session.ID)+" ("+sessionState(m.status.Resumed)+")"))
	b.WriteString("\n")

	b.WriteString("\n")
	b.WriteString(section("commands"))
	for _, command := range slashCommands {
		line := fmt.Sprintf("%-26s %s", command.label(), command.Description)
		b.WriteString(mutedStyle.Render(truncate(line, width-4)))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(section("keys"))
	for _, line := range keyLines() {
		b.WriteString(mutedStyle.Render(line))
		b.WriteString("\n")
	}

	return b.String()
}

func (m model) statusView(width int) string {
	left := fmt.Sprintf("%s · %s · %s", m.lead.Name, m.state, m.ruleSummary())
	right := "/ commands · ctrl+c quit"
	return statusBar(width, left, right)
}

func (m model) paletteView(width int) string {
	var b strings.Builder
	for i, command := range m.palette {
		line := fmt.Sprintf("%-26s %s", command.label(), command.Description)
		style := mutedStyle
		if i == m.paletteIndex {
			style = selectedStyle
		}
		b.WriteString(style.Width(max(10, width-4)).Render(truncate(line, width-4)))
		b.WriteString("\n")
	}
	return palettePanel(width, strings.TrimRight(b.String(), "\n"))
}

// overlayBottom draws overlay over the last lines of base without changing
// the surrounding layout.
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

func sessionState(resumed bool) string {
	if resumed {
		return "resumed"
	}
	return "new"
}
