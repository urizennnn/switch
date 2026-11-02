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

	header := lipgloss.NewStyle().
		Width(width).
		Padding(0, 1).
		Render(m.header())

	body := m.viewport.View()

	var bottom string
	if m.permission != nil {
		bottom = m.permissionView(width)
	} else {
		bottom = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(0, 1).
			Width(width - 2).
			Render(dimStyle.Render("› ") + m.input.View())
	}

	state := m.state
	if m.busy && state == "ready" {
		state = "working"
	}
	status := dimStyle.Render(fmt.Sprintf("%s  ·  %s  ·  esc cancel  ·  ctrl+c quit", state, m.ruleSummary()))
	status = lipgloss.NewStyle().Width(width).Padding(0, 1).Render(status)

	view := lipgloss.JoinVertical(lipgloss.Left, header, body, bottom, status)
	return tea.NewView(view)
}

func (m model) permissionView(width int) string {
	var b strings.Builder
	b.WriteString(errorStyle.Render("permission required"))
	b.WriteString("  ")
	b.WriteString(textStyle.Render(m.permission.Title))
	b.WriteString("\n")
	for i, option := range m.permission.Options {
		fmt.Fprintf(&b, "  %d. %s (%s)\n", i+1, option.Name, option.Kind)
	}
	b.WriteString(dimStyle.Render("press 1-9 to choose · esc to deny"))

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(red).
		Padding(0, 1).
		Width(width - 2).
		Render(b.String())
}
