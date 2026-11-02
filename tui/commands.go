package tui

import (
	"fmt"
	"strings"
)

type slashCommand struct {
	Name        string
	Args        string
	Description string
	NeedsArg    bool
}

var slashCommands = []slashCommand{
	{Name: "/plan", Args: "<goal>", Description: "plan a run", NeedsArg: true},
	{Name: "/plan --file", Args: "<path>", Description: "import plan JSON", NeedsArg: true},
	{Name: "/doctor", Description: "check setup"},
	{Name: "/init", Description: "create config"},
	{Name: "/rules", Description: "show rules"},
	{Name: "/session", Description: "show session"},
	{Name: "/lead", Args: "<agent>", Description: "choose the lead", NeedsArg: true},
	{Name: "/new", Description: "new session"},
	{Name: "/home", Description: "dashboard"},
	{Name: "/help", Description: "show help"},
	{Name: "/quit", Description: "exit"},
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

type keyBinding struct {
	Keys        string
	Description string
}

var keyBindings = []keyBinding{
	{Keys: "enter", Description: "send"},
	{Keys: "tab", Description: "complete"},
	{Keys: "up/down", Description: "select"},
	{Keys: "esc", Description: "close or cancel"},
	{Keys: "ctrl+c", Description: "quit"},
}

func (c slashCommand) label() string {
	if c.Args == "" {
		return c.Name
	}
	return c.Name + " " + c.Args
}

func keyLines() []string {
	var lines []string
	for i := 0; i < len(keyBindings); i += 2 {
		line := keyBindings[i].Keys + " " + keyBindings[i].Description
		if i+1 < len(keyBindings) {
			line += " · " + keyBindings[i+1].Keys + " " + keyBindings[i+1].Description
		}
		lines = append(lines, line)
	}
	return lines
}

func helpText() string {
	var b strings.Builder
	b.WriteString("Commands\n")
	for _, command := range slashCommands {
		fmt.Fprintf(&b, "  %-26s %s\n", command.label(), command.Description)
	}
	b.WriteString("\nKeys\n")
	for _, line := range keyLines() {
		b.WriteString("  " + line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
