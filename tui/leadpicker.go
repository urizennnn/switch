package tui

import (
	"fmt"

	"github.com/urizennnn/switch/internal/config"
)

type leadChoice struct {
	Label     string
	Detail    string
	Section   string
	Agent     config.Agent
	Available bool
	Current   bool
}

type leadPicker struct {
	choices []leadChoice
	index   int
	err     string
}

func buildLeadPicker(m model) *leadPicker {
	var choices []leadChoice
	for _, agent := range m.dashboardFleet() {
		available := adapterAvailable(agent.Driver)
		choices = append(choices, leadChoice{
			Label:     agent.Name,
			Detail:    fmt.Sprintf("%s · %s", agent.Role, agent.Driver),
			Section:   "Configured agents",
			Agent:     agent,
			Available: available,
			Current:   agent.Name == m.lead.Name,
		})
	}

	index := 0
	for i, choice := range choices {
		if choice.Current {
			index = i
			break
		}
	}
	return &leadPicker{choices: choices, index: index}
}
