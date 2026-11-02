package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func (m model) View() tea.View {
	if m.splash {
		return tea.NewView(m.splashView())
	}

	width := m.width
	if width <= 0 {
		width = 80
	}

	conversation := lipgloss.NewStyle().
		Padding(0, 2, 0, 2).
		Height(m.conversationHeight()).
		Render(m.viewport.View())
	if m.paletteOpen {
		conversation = overlayBottom(conversation, m.paletteView(), m.conversationHeight())
	}

	parts := []string{
		conversation,
		m.inputBar(width),
		m.statusLine(width),
	}

	return tea.NewView(lipgloss.JoinVertical(lipgloss.Left, parts...))
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

	start := 0
	end := len(m.palette)

	var b strings.Builder
	for i := start; i < end; i++ {
		command := m.palette[i]
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

func (m model) splashView() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	colWidth := max(20, (width/2)-4)

	left := lipgloss.NewStyle().
		Width(colWidth).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			lipgloss.NewStyle().Foreground(accent).Bold(true).Render("switch"),
			lipgloss.NewStyle().Foreground(dim).Render("multi-agent orchestration"),
			"\n",
			lipgloss.NewStyle().Foreground(muted).Render(
				fmt.Sprintf("%s\n%s", m.chat.Agent, m.chat.Model),
			),
			lipgloss.NewStyle().Foreground(dim).Render(m.chat.Project),
		))

	dividerHeight := lipgloss.Height(left)
	var dividerStr strings.Builder
	for range dividerHeight {
		dividerStr.WriteString("│\n")
	}
	mid := lipgloss.NewStyle().Foreground(border).Render(dividerStr.String())

	right := lipgloss.NewStyle().
		Width(colWidth).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			lipgloss.NewStyle().Foreground(accent).Bold(true).Render("Getting started"),
			lipgloss.NewStyle().Foreground(muted).Render(
				"Type / to browse commands.\nAsk the lead to plan a run.",
			),
			"\n",
			lipgloss.NewStyle().Foreground(teal).Bold(true).Render("Slash commands"),
			lipgloss.NewStyle().Foreground(muted).Render(
				lipgloss.NewStyle().Foreground(text).Render("/plan")+"   plan a run\n"+
					lipgloss.NewStyle().Foreground(text).Render("/doctor")+" check the setup\n"+
					lipgloss.NewStyle().Foreground(text).Render("/rules")+"  show rules",
			),
			"\n",
			lipgloss.NewStyle().Foreground(teal).Bold(true).Render("Keybindings"),
			lipgloss.NewStyle().Foreground(muted).Render(
				lipgloss.NewStyle().Foreground(text).Render("enter")+" send   "+
					lipgloss.NewStyle().Foreground(text).Render("tab")+" complete\n"+
					lipgloss.NewStyle().Foreground(text).Render("esc")+" cancel   "+
					lipgloss.NewStyle().Foreground(text).Render("ctrl+c")+" quit",
			),
		))

	columns := lipgloss.JoinHorizontal(lipgloss.Top, left, mid, right)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Width(width - 2).
		Render(columns)

	return lipgloss.JoinVertical(lipgloss.Left,
		box,
		m.inputBar(width),
		m.statusLine(width),
	)
}
