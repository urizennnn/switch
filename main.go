package main

import (
	"context"
	"fmt"
	"log"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/chat"
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
		case "chat":
			os.Exit(chatcli.Run(os.Args[2:]))
		case "plan":
			os.Exit(plancli.Run(os.Args[2:]))
		case "probe":
			os.Exit(probe.Run(os.Args[2:]))
		case "doctor":
			os.Exit(doctor.Run(os.Args[2:]))
		case "init":
			os.Exit(initcmd.Run(os.Args[2:]))
		case "version", "-v", "--version":
			fmt.Println(buildversion.Version)
			return
		case "help", "-h", "--help":
			usage()
			return
		default:
			fmt.Fprintf(os.Stderr, "switch: unknown command %q\n\n", os.Args[1])
			usage()
			os.Exit(2)
		}
	}

	if err := runTUI(); err != nil {
		log.Fatal(err)
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
	conversation, status, err := chat.Open(ctx, chat.Options{
		Project: project,
		Agent:   cfg.Agent,
		Model:   cfg.Model,
		Rules:   ruleSet,
		Store:   state,
	})
	if err != nil {
		return fmt.Errorf("chat: %w", err)
	}
	defer conversation.Close(context.Background())

	f, err := tea.LogToFile("debug.log", "debug")
	if err != nil {
		return err
	}
	defer f.Close()

	program := tea.NewProgram(tui.New(tui.Options{
		Context: ctx,
		Chat:    conversation,
		Rules:   ruleSet,
		Store:   state,
		Status:  status,
		Created: created,
	}))
	_, err = program.Run()
	return err
}

func usage() {
	fmt.Fprintf(os.Stderr, `switch — multi-agent orchestration for coding agents

Usage:
  switch                  start the TUI (primary interface)
  switch help             show this help

TUI commands:
  /plan <goal>            ask the lead to plan and create a run
  /plan --file <path>     import a plan JSON file
  /doctor                 check config, rules, adapters, and state
  /init                   create .switch config and rules
  /rules                  show effective rules
  /session                show the current agent session
  /new                    start a new agent session
  /quit                   exit

Headless commands (automation):
  switch chat [flags]     chat without the TUI
  switch plan [goal]      plan and create worktrees without the TUI
  switch doctor [dir]     raw diagnostics
  switch init [dir]       create project config and rules
  switch probe [flags]    connect to one ACP agent and stream a prompt
  switch version          print the version
`)
}
