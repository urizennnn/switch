package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/store"
)

// View draws the TUI in the terminal's alternate screen, so it starts on a
// clean screen and leaves the shell's scrollback untouched on exit.
func (m model) View() tea.View {
	v := tea.NewView(m.screen())
	v.AltScreen = true
	return v
}

func (m model) screen() string {
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
		return m.withModal(base, width, "ACP adapters", m.adapterPickerContent(width), footer)
	case m.rulesReview != nil:
		return m.withModal(base, width, "this project has rules", "Switch sends these to agents in this project. They come from the repository, so read them before they are used.\n\n"+m.rulesReview.text,
			"enter use them · esc not this session")
	case m.rulesManager != nil:
		return m.withModal(base, width, "rules", m.rulesManagerContent(min(82, max(44, width*2/3))), rulesManagerFooter(m.rulesManager))
	case m.sessionPicker != nil:
		return m.withModal(base, width, "resume a session", m.sessionPickerContent(min(82, max(44, width*2/3))), "↑/↓ select · enter resume · esc cancel")
	case m.taskPicker != nil:
		return m.withModal(base, width, "tasks", m.taskPickerContent(min(82, max(44, width*2/3))), "enter details · a approve · r reject · d diff · t retry · esc")
	case m.picker != nil:
		return m.withModal(base, width, "choose lead", m.pickerContent(width), "↑/↓ select · enter choose · esc cancel")
	case m.modal != "":
		return m.withModal(base, width, m.modalTitle, m.modal, "esc or enter to close")
	}

	if !m.paletteOpen {
		return base
	}

	sidebarWidth, mainWidth := m.paneWidths(width)
	palette := m.paletteView(mainWidth)
	paletteHeight := lipgloss.Height(palette)
	y := lipgloss.Height(base) - 5 - paletteHeight
	y = max(1, y)
	x := sidebarWidth + 3
	if m.zen {
		x = 2
	}

	comp := lipgloss.NewCompositor(
		lipgloss.NewLayer(base).X(0).Y(0).Z(0),
		lipgloss.NewLayer(palette).X(x).Y(y).Z(1),
	)
	return comp.Render()
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
	m.viewport.SetHeight(max(3, m.mainPaneHeight()-3))
	m.input.SetWidth(max(10, width-4))
}

// mainPaneHeight leaves room for the job log while a job runs.
func (m model) mainPaneHeight() int {
	if m.job == "" {
		return m.bodyHeight()
	}
	return m.bodyHeight() - m.jobLogHeight()
}

func (m model) jobLogHeight() int {
	return max(6, m.bodyHeight()*2/5)
}

func (m model) bodyHeight() int {
	height := m.height
	if height <= 0 {
		height = 24
	}
	return max(8, height-9)
}

func (m model) paneWidths(width int) (sidebarWidth, mainWidth int) {
	if m.zen {
		return 0, max(24, width-4) // the main pane takes the sidebar's place
	}
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

	var main string
	if m.job != "" {
		// The job log sits under the main pane so chat stays usable during a run.
		logHeight, mainHeight := m.jobLogHeight(), m.mainPaneHeight()
		main = lipgloss.JoinVertical(lipgloss.Left,
			panel(mainWidth, mainHeight, m.mainTitle(), m.mainContent(mainWidth, mainHeight)),
			panel(mainWidth, logHeight, m.jobLogTitle(), m.jobLogView(mainWidth, logHeight)))
	} else {
		main = panel(mainWidth, height, m.mainTitle(), m.mainContent(mainWidth, height))
	}

	if m.zen {
		return main
	}
	sidebar := panel(sidebarWidth, height, "workspace", m.sidebarContent(sidebarWidth))
	return lipgloss.JoinHorizontal(lipgloss.Top, sidebar, " ", main)
}

func (m model) sidebarContent(width int) string {
	var b strings.Builder

	inner := max(10, width-4) // inside the panel border and padding

	// Exactly one agent is shown as the lead.
	b.WriteString(section("lead"))
	b.WriteString(brandStyle.Render("● ") + textStyle.Bold(true).Render(truncate(m.lead.Name, inner-2)))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("  " + truncate(m.leadDetail(), inner-2)))
	b.WriteString("\n")

	// Workers take tasks. An agent configured as lead that is not leading
	// cannot, so it is left out; /lead still offers it.
	if workers := m.workers(); len(workers) > 0 {
		b.WriteString("\n")
		b.WriteString(section("workers"))
		for _, agent := range workers {
			state, stateStyle := m.team.status(agent.Name), dimStyle
			if state == "answering" {
				stateStyle = warningStyle
			}
			if task, ok := m.agentTask(agent.Name); ok {
				state, stateStyle = shortTaskID(task.ID), warningStyle
			}
			state = truncate(state, max(4, inner/2))
			name := truncate(agent.Name, max(4, inner-2-lipgloss.Width(state)-1))
			gap := max(1, inner-2-lipgloss.Width(name)-lipgloss.Width(state))
			b.WriteString(dimStyle.Render("○ ") + textStyle.Render(name) + strings.Repeat(" ", gap) + stateStyle.Render(state))
			b.WriteString("\n")
			b.WriteString(dimStyle.Render("  " + truncate(agent.Role+" · "+agent.Driver, inner-2)))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(section("run"))
	if m.run == nil {
		b.WriteString(mutedStyle.Render("no run yet"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("/plan <goal>"))
		b.WriteString("\n")
	} else {
		queued, active, review, merged, blocked := m.taskCounts()
		b.WriteString(textStyle.Render(shortID(m.run.ID)) + " " + mutedStyle.Render(m.run.Status))
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render(fmt.Sprintf("queued %d · active %d", queued, active)))
		b.WriteString("\n")
		reviewLine := fmt.Sprintf("review %d · merged %d", review, merged)
		if review > 0 {
			b.WriteString(warningStyle.Render(reviewLine))
		} else {
			b.WriteString(mutedStyle.Render(reviewLine))
		}
		b.WriteString("\n")
		if blocked > 0 {
			b.WriteString(errorStyle.Render(fmt.Sprintf("blocked %d", blocked)))
			b.WriteString("\n")
		}
		hint := "/run to start"
		switch {
		case m.runOwner > 0:
			hint = fmt.Sprintf("detached · pid %d · /stop", m.runOwner)
		case m.job == "run":
			hint = "/tasks · /stop"
		case review > 0:
			hint = "/review to approve"
		}
		b.WriteString(dimStyle.Render(hint))
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
	case m.home:
		return "overview"
	default:
		return "conversation"
	}
}

func (m model) mainContent(width, height int) string {
	switch {
	case m.chat == nil && m.connectErr != nil && m.home:
		return errorStyle.Render("could not start "+m.lead.Name) + "\n\n" +
			textStyle.Width(max(10, width-4)).Render(firstLine(m.connectErr.Error())) + "\n\n" +
			dimStyle.Render("enter to retry · /doctor to check setup · ctrl+c to quit")
	case m.home:
		return m.overviewContent(width)
	default:
		return m.viewport.View()
	}
}

func (m model) overviewContent(width int) string {
	var b strings.Builder

	model := m.lead.Model
	if m.chat != nil && m.chat.Agent == m.lead.Driver && m.chat.Model != "" {
		model = m.chat.Model
	}
	if model == "" {
		model = "default"
	}
	session := "connecting…"
	switch {
	case m.chat == nil && m.connectErr != nil:
		session = "not connected"
	case m.chat != nil && m.state != "starting lead":
		session = shortID(m.chat.Session.ID) + " (" + sessionState(m.status.Resumed) + ")"
	}

	b.WriteString(section("lead"))
	for _, row := range [][2]string{{"agent", m.lead.Name}, {"driver", m.lead.Driver}, {"model", model}, {"session", session}} {
		b.WriteString(alignedValue(row[0], truncate(row[1], max(10, width-14))))
		b.WriteString("\n")
	}
	if adapterSource(m.lead.Driver) == "npx" {
		// npx resolves the package on every start, which is the slow part.
		b.WriteString(dimStyle.Render(truncate("tip: /adapters installs "+m.lead.Driver+" so it starts faster", max(10, width-4))))
		b.WriteString("\n")
	}

	if m.run != nil {
		queued, active, review, merged, blocked := m.taskCounts()
		b.WriteString("\n")
		b.WriteString(section("run"))
		b.WriteString(alignedValue("status", m.run.Status))
		b.WriteString("\n")
		b.WriteString(alignedValue("tasks", fmt.Sprintf("%d queued · %d active · %d review · %d merged · %d blocked", queued, active, review, merged, blocked)))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(section("get started"))
	b.WriteString(mutedStyle.Render(truncate("Type a message to talk to "+m.lead.Name+".", max(10, width-4))))
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render(truncate("/plan <goal> plans work for the team · / shows every command", max(10, width-4))))
	b.WriteString("\n")

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
		queued, active, review, merged, _ := m.taskCounts()
		parts = append(parts, dimStyle.Render("run "+shortID(m.run.ID))+" "+mutedStyle.Render(fmt.Sprintf("%dq/%da/%dr/%dm", queued, active, review, merged)))
	}
	if m.installing != "" {
		parts = append(parts, warningStyle.Render(m.installing+"…"))
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

func (m model) jobLogView(width, height int) string {
	text := m.jobLogText()
	if text == "" {
		text = dimStyle.Render("waiting for the agent…")
	}
	var lines []string
	for _, line := range strings.Split(tailLines(text, max(1, height-3)), "\n") {
		lines = append(lines, truncate(line, max(10, width-4)))
	}
	return mutedStyle.Render(strings.Join(lines, "\n"))
}

// agentTask is the task an agent is working on, if any.
func (m model) agentTask(name string) (store.Task, bool) {
	for _, task := range m.tasks {
		if task.Assignee == name && isActive(task.Status) {
			return task, true
		}
	}
	return store.Task{}, false
}

func (m model) jobLogTitle() string {
	if m.job == "plan" {
		return "plan log · esc to cancel"
	}
	return "run log · /stop to end"
}

// adapterSource reports how an adapter is launched: installed, npx, or missing.
func adapterSource(name string) string {
	for _, status := range acpdriver.AdapterStatuses() {
		if status.Name == name {
			if !status.Available {
				return "missing"
			}
			return status.Source
		}
	}
	return ""
}

// alignedValue renders a key-value row with keys in one column.
func alignedValue(key, value string) string {
	return mutedStyle.Render(fmt.Sprintf("%-8s", key)) + " " + textStyle.Render(value)
}

// leadDetail is the lead's driver and short model name.
func (m model) leadDetail() string {
	model := m.lead.Model
	if m.chat != nil && m.chat.Agent == m.lead.Driver && m.chat.Model != "" {
		model = m.chat.Model
	}
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	if model == "" {
		return m.lead.Driver
	}
	return m.lead.Driver + " · " + model
}

// workers are the agents that can take tasks: not the lead, and not an agent
// configured only to lead.
func (m model) workers() []config.Agent {
	var out []config.Agent
	for _, agent := range m.dashboardFleet() {
		if agent.Name != m.lead.Name && agent.Role != "lead" {
			out = append(out, agent)
		}
	}
	return out
}
