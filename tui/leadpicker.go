package tui

import (
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
			Detail:    pickerDetail(m.roleLabel(agent), agent.Driver, m.team.status(agent.Name)),
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

// pickerDetail notes when switching to an agent is instant.
func pickerDetail(role, driver, status string) string {
	detail := role + " · " + driver
	if status == "ready" {
		detail += " · ready"
	}
	return detail
}
