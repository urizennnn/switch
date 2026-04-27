package tui

import tea "charm.land/bubbletea/v2"

func (m model) View() tea.View {
	return tea.NewView(m.SplashView())
}
