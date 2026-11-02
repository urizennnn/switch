package main

import (
	"fmt"
	"log"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/probe"
	buildversion "github.com/urizennnn/switch/internal/version"
	"github.com/urizennnn/switch/tui"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "probe":
			os.Exit(probe.Run(os.Args[2:]))
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
	f, err := tea.LogToFile("debug.log", "debug")
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = tea.NewProgram(tui.New()).Run()
	return err
}

func usage() {
	fmt.Fprintf(os.Stderr, `switch — multi-agent orchestration for coding agents

Usage:
  switch                  start the TUI
  switch probe [flags]    connect to one ACP agent and stream a prompt
  switch version          print the version
  switch help             show this help

Probe examples:
  switch probe --list
  switch probe --agent opencode --no-prompt
  switch probe --agent opencode --model <model> --prompt "Say hello"
  switch probe --agent codex --no-prompt
  switch probe --agent claude --prompt "Say hello in one sentence"
  switch probe --agent opencode --session <session-id>
`)
}
