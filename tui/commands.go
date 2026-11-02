package tui

import "strings"

type slashCommand struct {
	Name        string
	Args        string
	Description string
	NeedsArg    bool
}

var slashCommands = []slashCommand{
	{Name: "/plan", Args: "<goal>", Description: "Ask the lead to plan and create a run", NeedsArg: true},
	{Name: "/plan --file", Args: "<path>", Description: "Import a plan JSON file", NeedsArg: true},
	{Name: "/doctor", Description: "Check config, rules, adapters, and state"},
	{Name: "/init", Description: "Create .switch config and rules"},
	{Name: "/rules", Description: "Show effective global and project rules"},
	{Name: "/session", Description: "Show the current agent session"},
	{Name: "/new", Description: "Start a new agent session"},
	{Name: "/help", Description: "Show command help"},
	{Name: "/quit", Description: "Exit Switch"},
}

func matchCommands(input string) []slashCommand {
	if !strings.HasPrefix(input, "/") {
		return nil
	}
	var matches []slashCommand
	for _, command := range slashCommands {
		if strings.HasPrefix(command.Name, input) {
			matches = append(matches, command)
		}
	}
	return matches
}

func findCommand(input string) (slashCommand, bool) {
	input = strings.TrimSpace(input)
	for _, command := range slashCommands {
		if command.Name == input {
			return command, true
		}
	}
	return slashCommand{}, false
}

func (c slashCommand) label() string {
	if c.Args == "" {
		return c.Name
	}
	return c.Name + " " + c.Args
}
