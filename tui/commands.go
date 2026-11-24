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
	{Name: "/plan", Args: "[goal]", Description: "plan a run; with no goal, from the conversation", NeedsArg: true},
	{Name: "/plan --file", Args: "<path>", Description: "import plan JSON", NeedsArg: true},
	{Name: "/run", Description: "start or resume the run"},
	{Name: "/run --detach", Args: "[--allow]", Description: "run in the background"},
	{Name: "/mode", Args: "[build|plan|full]", Description: "show or set permission mode for this session"},
	{Name: "/parallel", Args: "[count]", Description: "show or set foreground worker concurrency"},
	{Name: "/stop", Description: "stop the run"},
	{Name: "/tasks", Description: "tasks: details, approve, reject, diff"},
	{Name: "/review", Description: "tasks waiting for approval"},
	// No "/approve all" row: the palette would run it for a task ID it starts with.
	{Name: "/approve", Args: "<task|all>", Description: "merge a reviewed task, or all of them in plan order", NeedsArg: true},
	{Name: "/reject", Args: "<task> <reason>", Description: "send a task back", NeedsArg: true},
	{Name: "/retry", Args: "<task>", Description: "requeue a blocked task", NeedsArg: true},
	{Name: "/fix", Args: "<task>", Description: "fix a task by hand in a shell in its worktree; exit to check it", NeedsArg: true},
	{Name: "/diff", Args: "<task>", Description: "show a task diff", NeedsArg: true},
	{Name: "/why", Description: "why unfinished tasks wait"},
	{Name: "/land", Description: "merge a completed run into your branch"},
	{Name: "/agent", Args: "<name>", Description: "follow a worker or subagent live; tab cycles, esc goes back", NeedsArg: true},
	{Name: "/steer", Args: "<agent> <text>", Description: "redirect a worker or teammate mid-turn", NeedsArg: true},
	{Name: "/agents", Description: "run agents and usage"},
	{Name: "/log", Description: "run output while it runs, else its event log"},
	{Name: "/copy", Description: "copy the last reply to the clipboard"},
	{Name: "/copy code", Description: "copy the last code block to the clipboard"},
	{Name: "/doctor", Description: "check setup"},
	{Name: "/init", Description: "create config"},
	{Name: "/rules", Description: "edit rule files; /rules <agent> shows what it gets"},
	{Name: "/remember", Args: "<rule>", Description: "add a rule to your local rules for this project", NeedsArg: true},
	{Name: "/session", Description: "show and switch sessions"},
	{Name: "/lead", Args: "<agent>", Description: "choose the lead", NeedsArg: true},
	{Name: "/adapters", Description: "manage ACP adapters"},
	{Name: "/new", Description: "start a new session"},
	{Name: "/resume", Description: "resume a past session"},
	{Name: "/zen", Description: "hide or show the sidebar"},
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
	{Keys: "tab", Description: "complete · next agent"},
	{Keys: "up/down", Description: "select or scroll"},
	{Keys: "esc", Description: "close or cancel"},
	{Keys: "pgup/pgdn", Description: "scroll half a page"},
	{Keys: "shift+up/down", Description: "scroll a line"},
	{Keys: "ctrl+home/end", Description: "first or latest output"},
	{Keys: "ctrl+x", Description: "stop the agent you follow"},
	{Keys: "ctrl+t", Description: "thinking"},
	{Keys: "ctrl+e", Description: "command output"},
	{Keys: "ctrl+o", Description: "zen mode"},
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
	fmt.Fprintf(&b, "  %-26s %s\n", "@<teammate> <message>", "ask a teammate, not the lead; tab completes")
	b.WriteString("\nKeys\n")
	for _, line := range keyLines() {
		b.WriteString("  " + line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
