package tui

import (
	"strings"

	"github.com/urizennnn/switch/internal/driver/acpdriver"
)

type adapterChoice struct {
	Name    string
	State   string
	Action  string
	Managed bool
	Note    string
}

type adapterPicker struct {
	choices []adapterChoice
	index   int
	pending string
	err     string
}

func buildAdapterPicker() *adapterPicker {
	var choices []adapterChoice
	for _, status := range acpdriver.AdapterStatuses() {
		management, _ := acpdriver.Managed(status.Name)
		state := status.Source
		if !status.Available {
			state = "missing"
		}
		action := "install"
		managed := true
		note := ""
		switch {
		case management.External:
			action = ""
			managed = false
			note = "managed externally"
		case status.Available && status.Source == "installed":
			action = "uninstall"
		case status.Available:
			action = "install globally"
		}
		choices = append(choices, adapterChoice{
			Name:    status.Name,
			State:   state,
			Action:  action,
			Managed: managed,
			Note:    note,
		})
	}
	return &adapterPicker{choices: choices}
}

func (p *adapterPicker) selected() adapterChoice {
	if len(p.choices) == 0 {
		return adapterChoice{}
	}
	return p.choices[p.index]
}

func (p *adapterPicker) detail(choice adapterChoice) string {
	parts := []string{choice.State}
	if choice.Note != "" {
		parts = append(parts, choice.Note)
	} else if choice.Action != "" {
		parts = append(parts, choice.Action)
	}
	return strings.Join(parts, " · ")
}
