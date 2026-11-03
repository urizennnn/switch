package main

import (
	"context"
	"fmt"
	"log"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/chatcli"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/doctor"
	"github.com/urizennnn/switch/internal/initcmd"
	"github.com/urizennnn/switch/internal/plancli"
	"github.com/urizennnn/switch/internal/probe"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
	buildversion "github.com/urizennnn/switch/internal/version"
	"github.com/urizennnn/switch/tui"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "__control":
			os.Exit(control(os.Args[2:]))
		case "version", "-v", "--version":
			fmt.Println(buildversion.Version)
			return
		case "help", "-h", "--help":
			usage()
			return
		default:
			fmt.Fprintln(os.Stderr, "switch is a TUI. Run `switch`, then type /help.")
			os.Exit(2)
		}
	}

	if err := runTUI(); err != nil {
		log.Fatal(err)
	}
}

// control is the internal, unadvertised entry point used by agents,
// automation, and tests until the Unix-socket control plane lands.
func control(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "internal control: chat, plan, probe, doctor, init")
		return 2
	}
	switch args[0] {
	case "chat":
		return chatcli.Run(args[1:])
	case "plan":
		return plancli.Run(args[1:])
	case "probe":
		return probe.Run(args[1:])
	case "doctor":
		return doctor.Run(args[1:])
	case "init":
		return initcmd.Run(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "internal control: unknown command %q\n", args[0])
		return 2
	}
}

func runTUI() error {
	project, err := os.Getwd()
	if err != nil {
		return err
	}

	cfg, created, err := config.LoadOrInit(project)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	ruleSet, err := rules.Load(cfg.Rules)
	if err != nil {
		return fmt.Errorf("rules: %w", err)
	}
	state, err := store.Open(config.StatePath())
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	defer state.Close()

	ctx := context.Background()
	lead := resolveLead(ctx, cfg, state, project)

	program := tea.NewProgram(tui.New(tui.Options{
		Context: ctx,
		Project: project,
		Rules:   ruleSet,
		Store:   state,
		Created: created,
		Fleet:   cfg.Fleet(),
		Lead:    lead,
	}))
	_, err = program.Run()
	return err
}

// resolveLead picks the lead from configured agents only: the stored lead
// name, then the agent that owns the stored chat session, then the config
// default. Names that are not configured agents, such as bare adapter names
// stored by older builds, are ignored.
func resolveLead(ctx context.Context, cfg config.Config, state *store.Store, project string) config.Agent {
	if name, ok, _ := state.LeadAgent(ctx, project); ok {
		for _, agent := range cfg.Fleet() {
			if agent.Name == name {
				return agent
			}
		}
	}
	if stored, ok, _ := state.GetChatSession(ctx, project); ok {
		for _, agent := range cfg.Fleet() {
			if agent.Driver == stored.Agent {
				if stored.Model != "" {
					agent.Model = stored.Model
				}
				return agent
			}
		}
	}
	return cfg.LeadAgent()
}

func usage() {
	fmt.Fprintf(os.Stderr, `switch — multi-agent orchestration for coding agents

Usage:
  switch                  start the TUI
  switch help             show this help
  switch version          print the version

Inside the TUI, type / to open the command palette:
  /plan <goal>            ask the lead to plan and create a run
  /plan --file <path>     import a plan JSON file
  /run                    dispatch the next ready task to a worker
  /doctor                 check config, rules, adapters, and state
  /init                   create .switch config and rules
  /rules                  show effective rules
  /session                show the current agent session
  /lead <agent>           choose which configured agent leads
  /adapters               install or uninstall ACP adapters
  /new                    start a new agent session
  /quit                   exit
`)
}
