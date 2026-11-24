package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/store"
)

func TestFollowWorkersAndSubagents(t *testing.T) {
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: "/p", Lead: lead, Fleet: []config.Agent{lead, builder}})
	m.connecting, m.state, m.job = false, "ready", "run"
	m.run = &store.Run{ID: "r1"}
	m.tasks = []store.Task{{ID: "r1:a", Title: "auth refactor", Status: execution.StatusRunning, Assignee: "builder-1"}}
	m.subagents = []store.Subagent{
		{TaskID: "r1:a", Parent: "builder-1", Name: "scout", Status: execution.SubRunning, Brief: "find every caller", Driver: "claude"},
		{TaskID: "r1:a", Parent: "builder-1", Name: "tests", Status: execution.SubDone, Brief: "table tests", Summary: "12 tests added", Files: []string{"a_test.go"}, Driver: "claude"},
	}
	m.jobPartial = map[string]string{}
	m.appendJobText("builder-1/scout", "reading internal/store\n")
	m.appendJobText("builder-1", "splitting the work\n")

	screen := func(m model) string { return ansi.Strip(m.screen()) }
	key := func(m model, k string) model {
		msg := tea.KeyPressMsg{Code: tea.KeyTab}
		switch k {
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "right":
			msg = tea.KeyPressMsg{Code: tea.KeyRight}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		}
		next, _ := m.Update(msg)
		return next.(model)
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m = next.(model)

	home := screen(m)
	for _, want := range []string{"├ ◐ scout", "└ ✓ tests", "builder-1 › scout  reading internal/store", "1 working · 1 subagent"} {
		if !strings.Contains(home, want) {
			t.Errorf("home screen lacks %q", want)
		}
	}

	m = key(m, "tab") // the worker
	worker := screen(m)
	for _, want := range []string{"LEAD › BUILDER-1", "running · auth refactor", "› ◐ scout", "12 tests added", "splitting the work"} {
		if !strings.Contains(worker, want) {
			t.Errorf("worker view lacks %q", want)
		}
	}
	m = key(m, "right")
	m = key(m, "enter") // open the second card
	if m.focus != "builder-1/tests" {
		t.Fatalf("focus = %q after opening a card", m.focus)
	}
	sub := screen(m)
	for _, want := range []string{"LEAD › BUILDER-1 › TESTS", "edits a_test.go", "RESULT", "12 tests added"} {
		if !strings.Contains(sub, want) {
			t.Errorf("subagent view lacks %q", want)
		}
	}
	if m = key(key(m, "esc"), "esc"); m.focus != "" {
		t.Fatalf("esc twice left focus at %q", m.focus)
	}

	m.openAgent("scout")
	if m.focus != "builder-1/scout" || !strings.Contains(screen(m), "reading internal/store") {
		t.Fatalf("/agent scout: focus %q", m.focus)
	}

	m.permission = &acpdriver.PermissionRequest{Agent: "builder-1/scout", Title: "edit store.go"}
	if line := ansi.Strip(m.statusView(140)); !strings.Contains(line, "builder-1 › scout asks: edit store.go") {
		t.Fatalf("permission status line: %q", line)
	}
}

func TestStandbyAndWorkingLeadsCanBeFollowed(t *testing.T) {
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}
	b1 := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	b2 := config.Agent{Name: "builder-2", Role: "builder", Driver: "codex"}
	// Mid-run, the user made builder-1 the chat lead; it still works its task.
	m := New(Options{Context: context.Background(), Project: "/p", Lead: b1, Fleet: []config.Agent{lead, b1, b2}})
	m.tasks = []store.Task{{ID: "r1:a", Status: execution.StatusRunning, Assignee: "builder-1"}}
	if got := strings.Join(m.agentOrder(), ","); got != ",lead,builder-1,builder-2" {
		t.Fatalf("tab order = %q", got)
	}
	m.openAgent("builder-1")
	if m.focus != "builder-1" {
		t.Fatalf("/agent builder-1 focused %q, not its task", m.focus)
	}
	m.openAgent("lead")
	if m.focus != "lead" {
		t.Fatalf("/agent lead focused %q, not the standby lead", m.focus)
	}
	m.tasks = nil
	if m.openAgent("builder-1"); m.focus != "" {
		t.Fatalf("/agent on the chat lead with no task focused %q, not its chat", m.focus)
	}
	// The run's lead reviews stream under the chat lead's name.
	next, _ := m.Update(streamMsg{agent: "builder-1", event: acpdriver.Event{Type: acpdriver.EventStart, Text: "review r1:a"}})
	m = next.(model)
	if m.openAgent("builder-1"); m.focus != "builder-1" {
		t.Fatalf("the chat lead's review cannot be followed: focus %q", m.focus)
	}
	// Once it ends, the transcript, like one of an ask it answered before it
	// led, does not list the chat lead again.
	next, _ = m.Update(streamMsg{agent: "builder-1", event: acpdriver.Event{Type: acpdriver.EventTurn}})
	m = next.(model)
	if got := strings.Join(m.agentOrder(), ","); got != ",lead,builder-2" {
		t.Fatalf("tab order after the review = %q", got)
	}
	if m.openAgent("builder-1"); m.focus != "" {
		t.Fatalf("/agent on the idle chat lead focused %q, not its chat", m.focus)
	}
}
