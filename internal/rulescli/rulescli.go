// Package rulescli is `switch __control rules`: the headless counterpart of
// the TUI's /rules manager.
package rulescli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

const usage = `usage: switch __control rules [list]
       switch __control rules show [agent]
       switch __control rules path <file>
       switch __control rules edit <file>

<file> is global, project, local, roles/<role>, agents/<agent>, tasks/<task>,
global/roles/<role>, or global/agents/<agent>.`

func Run(args []string) int {
	project, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "switch:", err)
		return 1
	}
	if err := run(config.ProjectRoot(project), args, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "switch:", err)
		return 1
	}
	return 0
}

func run(project string, args []string, stdout io.Writer) error {
	cfg, _, err := config.LoadOrInit(project)
	if err != nil {
		return err
	}
	base, err := rules.Load(cfg.Rules)
	if err != nil {
		return err
	}
	command := "list"
	if len(args) > 0 {
		command = args[0]
	}
	switch {
	case command == "list":
		global, local := base.Files()
		for _, file := range append(global, local...) {
			state := ""
			if !file.Exists {
				state = "  (not created)"
			}
			fmt.Fprintf(stdout, "%-22s %s%s\n", file.Scope, file.Path, state)
		}
		return nil
	case command == "show":
		state, err := store.Open(config.StatePath())
		if err != nil {
			return err
		}
		defer state.Close()
		ctx := context.Background()
		// Show what agents are sent: project rules only as the user accepted them.
		base = base.Trust(func() (string, error) { return state.TrustedRules(ctx, project) })
		lead := leadName(ctx, state, project, cfg)
		name := lead
		if len(args) > 1 {
			name = args[1]
		}
		set, err := Effective(base, cfg, lead, name)
		if err != nil {
			return err
		}
		if base.Untrusted() {
			fmt.Fprintln(os.Stderr, rules.UntrustedNote)
		}
		if set.Empty() {
			fmt.Fprintf(stdout, "%s gets no rules\n", name)
			return nil
		}
		fmt.Fprintln(stdout, set.Text())
		return nil
	case (command == "path" || command == "edit") && len(args) == 2:
		path, err := base.Target(args[1])
		if err != nil {
			return err
		}
		if command == "path" {
			fmt.Fprintln(stdout, path)
			return nil
		}
		editor, err := rules.Editor(path)
		if err != nil {
			return err
		}
		editor.Stdin, editor.Stdout, editor.Stderr = os.Stdin, os.Stdout, os.Stderr
		return editor.Run()
	default:
		return fmt.Errorf("%s", usage)
	}
}

// Effective is what a configured agent is sent in chat: the lead gets
// roles/lead.md, everyone else their own role's rules.
func Effective(base rules.Set, cfg config.Config, lead, name string) (rules.Set, error) {
	for _, agent := range cfg.Fleet() {
		if agent.Name != name {
			continue
		}
		role := agent.Role
		if name == lead {
			role = "lead"
		}
		return rules.Scoped(base, role, name, "")
	}
	return rules.Set{}, fmt.Errorf("no configured agent named %q", name)
}

// leadName is the agent leading the project's chat: the one chosen with
// /lead, else the configured lead.
func leadName(ctx context.Context, state *store.Store, project string, cfg config.Config) string {
	if name, ok, _ := state.LeadAgent(ctx, project); ok {
		for _, agent := range cfg.Fleet() {
			if agent.Name == name {
				return name
			}
		}
	}
	return cfg.LeadAgent().Name
}
