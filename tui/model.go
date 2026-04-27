package tui

import (
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type model struct {
	Running  bool
	Prompter textinput.Model
	Width    int
	Height   int
	Content  string
	Minds    []string
	Splash   bool
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func New() model {
	prompter := textinput.New()
	prompter.Placeholder = "State your wish"
	prompter.SetVirtualCursor(true)
	prompter.Focus()
	prompter.SetWidth(30)

	return model{
		Prompter: prompter,
		Running:  true,
		Splash:   true,
	}
}
