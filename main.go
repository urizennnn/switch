package main

import (
	"log"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/tui"
)

func main() {
	f, err := tea.LogToFile("debug.log", "debug")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	if _, err := tea.NewProgram(tui.New()).Run(); err != nil {
		log.Fatal(err)
	}
}
