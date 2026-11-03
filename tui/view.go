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

	base := lipgloss.JoinVertical(
		lipgloss.Left,
		m.headerView(width),
		m.bodyView(width),
		inputBar(width, m.input.View()),
		m.statusView(width),
	)

	switch {
	case m.adapterPicker != nil:
		footer := "↑/↓ select · enter act · esc cancel"
		if m.adapterPicker.pending != "" {
			footer = "enter again to confirm · esc cancel"
		}
		return tea.NewView(m.withModal(base, width, "ACP adapters", m.adapterPickerContent(width), footer))
	case m.picker != nil:
		return tea.NewView(m.withModal(base, width, "choose lead", m.pickerContent(width), "↑/↓ select · enter choose · esc cancel"))
	case m.modal != "":
		return tea.NewView(m.withModal(base, width, m.modalTitle, m.modal, "esc or enter to close"))
	}

	if !m.paletteOpen {
		return tea.NewView(base)
	}

	sidebarWidth, mainWidth := m.paneWidths(width)
	palette := m.paletteView(mainWidth)
	paletteHeight := lipgloss.Height(palette)
	y := lipgloss.Height(base) - 5 - paletteHeight
	y = max(1, y)
	x := sidebarWidth + 3

	comp := lipgloss.NewCompositor(
		lipgloss.NewLayer(base).X(0).Y(0).Z(0),
		lipgloss.NewLayer(palette).X(x).Y(y).Z(1),
	)
	return tea.NewView(comp.Render())
}

func (m model) withModal(base string, screenWidth int, title, content, footer string) string {
	panelWidth := min(82, max(44, screenWidth*2/3))
	content = truncateLines(content, max(4, m.height-10))
	panel := modalPanel(panelWidth, title, content, footer)
	x := max(1, (screenWidth-lipgloss.Width(panel))/2)
	y := max(1, (lipgloss.Height(base)-lipgloss.Height(panel))/2)
	comp := lipgloss.NewCompositor(
		lipgloss.NewLayer(base).X(0).Y(0).Z(0),
		lipgloss.NewLayer(panel).X(x).Y(y).Z(1),
	)
	return comp.Render()
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
	return max(8, height-9)
}

func (m model) paneWidths(width int) (sidebarWidth, mainWidth int) {
	sidebarWidth = min(34, max(26, width/4))
	mainWidth = max(24, width-sidebarWidth-5)
	return sidebarWidth, mainWidth
}

func (m model) headerView(width int) string {
	project := m.project
	if project == "" && m.chat != nil {
		project = m.chat.Project
	}
	left := brandStyle.Render("switch") +
		mutedStyle.Render("  "+truncate(project, max(12, width/3)))

	right := mutedStyle.Render(m.lead.Name + " · lead")
	return appHeader(width, left, right)
}

func (m model) bodyView(width int) string {
	sidebarWidth, mainWidth := m.paneWidths(width)
	height := m.bodyHeight()

	sidebar := panel(sidebarWidth, height, "workspace", m.sidebarContent(sidebarWidth))
	mainContent := m.mainContent(mainWidth, height)
	main := panel(mainWidth, height, m.mainTitle(), mainContent)

	return lipgloss.JoinHorizontal(lipgloss.Top, sidebar, " ", main)
}

func (m model) sidebarContent(width int) string {
	var b strings.Builder

	b.WriteString(section("agents"))
	for _, agent := range m.dashboardFleet() {
		active := agent.Name == m.lead.Name
		marker := dimStyle.Render("○")
		nameStyle := textStyle
		if active {
			marker = brandStyle.Render("●")
			nameStyle = textStyle.Bold(true)
		}
		name := fmt.Sprintf("%-10s", truncate(agent.Name, 10))
		state := "idle"
		if active {
			state = "active"
		}
		line := marker + " " + nameStyle.Render(name) + " " + mutedStyle.Render(state)
		if active {
			line = selectedStyle.Width(max(10, width-4)).Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  " + truncate(agent.Role+" · "+agent.Driver, width-6)))
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
		b.WriteString(textStyle.Render(shortID(m.run.ID)) + " " + mutedStyle.Render(m.run.Status))
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render(fmt.Sprintf("q:%d  r:%d  d:%d", queued, running, done)))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(fmt.Sprintf("agents %d · /run next task", len(m.runAgents))))
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
	case m.chat == nil:
		return "connecting"
	case m.job != "":
		return m.job
	case m.home:
		return "overview"
	default:
		return "conversation"
	}
}

func (m model) mainContent(width, height int) string {
	switch {
	case m.chat == nil:
		if m.connectErr != nil {
			return errorStyle.Render("could not start the lead") + "\n\n" +
				textStyle.Render(m.connectErr.Error()) + "\n\n" +
				dimStyle.Render("enter to retry · ctrl+c to quit")
		}
		return textStyle.Render("starting "+m.lead.Name+" ("+m.lead.Driver+")…") + "\n\n" +
			dimStyle.Render("the TUI is ready; the agent is connecting in the background")
	case m.job != "":
		text := strings.TrimSpace(m.jobBuffer.String())
		if text == "" {
			text = "waiting for the agent…"
		}
		return textStyle.Render(tailLines(text, max(3, height-5))) + "\n\n" + dimStyle.Render("streaming "+m.job+" output · esc to cancel")
	case m.home:
		return m.overviewContent(width)
	default:
		return m.viewport.View()
	}
}

func (m model) overviewContent(width int) string {
	var b strings.Builder

	driver := m.lead.Driver
	model := m.lead.Model
	if m.chat != nil && m.chat.Agent == m.lead.Driver && m.chat.Model != "" {
		model = m.chat.Model
	}
	if model == "" {
		model = "default"
	}
	b.WriteString(section("lead"))
	b.WriteString(keyValue("agent", m.lead.Name))
	b.WriteString("\n")
	b.WriteString(keyValue("driver", driver))
	b.WriteString("\n")
	b.WriteString(keyValue("model", model))
	b.WriteString("\n")
	sessionLabel := shortID(m.chat.Session.ID) + " (" + sessionState(m.status.Resumed) + ")"
	if m.state == "starting lead" {
		sessionLabel = "connecting…"
	}
	b.WriteString(keyValue("session", sessionLabel))
	b.WriteString("\n")

	b.WriteString("\n")
	b.WriteString(section("commands"))
	for _, command := range slashCommands {
		label := truncate(command.label(), 26)
		desc := truncate(command.Description, max(0, width-32))
		b.WriteString(commandRow(label, desc))
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

func (m model) pickerContent(width int) string {
	if m.picker == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "current: %s (%s)\n", m.lead.Name, m.lead.Driver)
	if len(m.picker.choices) == 0 {
		b.WriteString("\nno configured agents with an available adapter\nrun /adapters or /doctor\n")
	}
	currentSection := ""
	for i, choice := range m.picker.choices {
		if choice.Section != currentSection {
			b.WriteString("\n" + section(choice.Section))
			currentSection = choice.Section
		}
		marker := "  "
		if choice.Current {
			marker = "● "
		}
		detail := choice.Detail
		if !choice.Available {
			detail += " · unavailable"
		}
		line := fmt.Sprintf("%s%-12s %s", marker, choice.Label, detail)
		style := mutedStyle
		if i == m.picker.index {
			style = activeStyle
		} else if !choice.Available {
			style = dimStyle
		}
		b.WriteString(style.Width(max(10, width-4)).Render(truncate(line, width-4)))
		b.WriteString("\n")
	}
	if m.picker.err != "" {
		b.WriteString("\n" + errorStyle.Render(m.picker.err))
		b.WriteString("\n")
	}
	return b.String()
}

func (m model) adapterPickerContent(width int) string {
	if m.adapterPicker == nil {
		return ""
	}
	var b strings.Builder
	for i, choice := range m.adapterPicker.choices {
		line := fmt.Sprintf("%-12s %s", choice.Name, m.adapterPicker.detail(choice))
		style := mutedStyle
		if choice.State == "missing" && i != m.adapterPicker.index {
			style = dimStyle
		}
		if i == m.adapterPicker.index {
			style = activeStyle
		}
		b.WriteString(style.Width(max(10, width-4)).Render(truncate(line, width-4)))
		b.WriteString("\n")
	}
	if m.adapterPicker.err != "" {
		b.WriteString("\n" + errorStyle.Render(m.adapterPicker.err))
		b.WriteString("\n")
	}
	return b.String()
}

func (m model) statusView(width int) string {
	driver := m.lead.Driver
	model := m.lead.Model
	sessionID := ""
	if m.chat != nil {
		if m.state != "starting lead" {
			sessionID = m.chat.Session.ID
		}
		if m.chat.Agent == m.lead.Driver {
			model = m.chat.Model
		}
	}
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	if model == "" {
		model = "default"
	}
	left := brandStyle.Render("● ") +
		textStyle.Render(m.lead.Name) +
		dimStyle.Render(" · ") +
		mutedStyle.Render(driver+" · "+truncate(model, 28))
	if sessionID != "" {
		left += dimStyle.Render(" · " + shortID(sessionID))
	}

	stateStyle := mutedStyle
	switch m.state {
	case "ready":
		stateStyle = successStyle
	case "connecting", "planning", "running", "checking", "new session", "starting lead", "working", "cancelling":
		stateStyle = warningStyle
	case "error", "exited", "agent unavailable":
		stateStyle = errorStyle
	}

	parts := make([]string, 0, 3)
	if m.run != nil {
		queued, running, done := m.taskCounts()
		parts = append(parts, dimStyle.Render("run "+shortID(m.run.ID))+" "+mutedStyle.Render(fmt.Sprintf("%dq/%dr/%dd", queued, running, done)))
	}
	parts = append(parts, stateStyle.Render(strings.ToUpper(m.state)))
	parts = append(parts, dimStyle.Render("/ commands"))
	right := strings.Join(parts, dimStyle.Render("  ·  "))

	return statusBar(width, left, right)
}

func (m model) paletteView(width int) string {
	maxRows := min(6, max(3, m.bodyHeight()-5))
	start := 0
	if m.paletteIndex >= maxRows {
		start = m.paletteIndex - maxRows + 1
	}
	end := min(len(m.palette), start+maxRows)

	var b strings.Builder
	for i := start; i < end; i++ {
		command := m.palette[i]
		label := truncate(command.label(), 26)
		desc := truncate(command.Description, max(0, width-32))
		line := commandRow(label, desc)
		if i == m.paletteIndex {
			line = selectedStyle.Width(max(10, width-4)).Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return palettePanel(width, strings.TrimRight(b.String(), "\n"))
}

func sessionState(resumed bool) string {
	if resumed {
		return "resumed"
	}
	return "new"
}
