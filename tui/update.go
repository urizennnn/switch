package tui

import (
	"log"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var (
	userLabel   = lipgloss.NewStyle().Foreground(accent).Bold(true).Render("you")
	switchLabel = lipgloss.NewStyle().Foreground(teal).Bold(true).Render("switch")
	userBody    = lipgloss.NewStyle().Foreground(text)
	switchBody  = lipgloss.NewStyle().Foreground(muted).Italic(true)
)

func appendMessage(content, label, body string) string {
	var b strings.Builder
	b.WriteString(content)
	if content != "" && !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(label)
	b.WriteString("  ")
	b.WriteString(body)
	b.WriteString("\n")
	return b.String()
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "enter":
			if m.Splash {
				m.Splash = false
			}
			input := strings.TrimSpace(m.Prompter.Value())
			if input == "" {
				break
			}
			m.Content = appendMessage(m.Content, userLabel, userBody.Render(input))
			m.Content = appendMessage(m.Content, switchLabel, switchBody.Render("deliberating across minds…"))
			m.Prompter.SetValue("")
		}
	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		log.Printf("Height: %d, Width: %d\n", m.Height, m.Width)
	}

	m.Prompter, cmd = m.Prompter.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}
