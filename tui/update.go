package tui

import (
	"fmt"
	"log"

	tea "charm.land/bubbletea/v2"
)

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
				return m, nil
			}
			m.Content += fmt.Sprintf("%s\n", m.Prompter.Value())
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
