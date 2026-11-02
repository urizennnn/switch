package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func (m model) View() tea.View {
	if m.Splash {
		return tea.NewView(m.SplashView())
	}
	lipGlossStr := lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.NewStyle().Padding(0, 2, 0, 2).Render(m.Content),
		lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(0, 1).
			Width(m.Width-2).
			Render(
				lipgloss.NewStyle().Foreground(dim).Render("› ")+
					m.Prompter.View(),
			),
	)
	view := tea.NewView(lipGlossStr)
	return view
}
