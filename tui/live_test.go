package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
)

func TestTranscriptThinkingToolsAndActivity(t *testing.T) {
	var list []entry
	addUser(&list, "", "walk the repo")
	applyEvent(&list, acpdriver.Event{Type: acpdriver.EventThought, Text: "I should list the folders first."})
	if got := activity(list); got != "Thinking" {
		t.Fatalf("activity = %q", got)
	}
	live := ansi.Strip(renderEntries(list, 80, "lead", 0, false))
	if !strings.Contains(live, "∴ thinking") || !strings.Contains(live, "list the folders") {
		t.Fatalf("live thought:\n%s", live)
	}

	// opencode names the call "shell", then sends the command as its title.
	applyEvent(&list, acpdriver.Event{Type: acpdriver.EventToolCall, Tool: &acpdriver.ToolInfo{ID: "c1", Title: "shell", Kind: "execute", Status: "pending"}})
	applyEvent(&list, acpdriver.Event{Type: acpdriver.EventToolEdit, Tool: &acpdriver.ToolInfo{ID: "c1", Title: "ls internal", Status: "in_progress"}})
	if got := activity(list); got != "Running shell" {
		t.Fatalf("activity = %q", got)
	}
	frame0 := ansi.Strip(renderEntries(list, 80, "lead", 0, false))
	frame1 := ansi.Strip(renderEntries(list, 80, "lead", 1, false))
	if !strings.Contains(frame0, "Shell(ls internal)") || frame0 == frame1 {
		t.Fatalf("running tool is not shown animated:\n%s", frame0)
	}
	if !strings.Contains(frame0, "∴ thought for") || strings.Contains(frame0, "list the folders") {
		t.Fatalf("thought did not collapse once the agent moved on:\n%s", frame0)
	}
	if shown := ansi.Strip(renderEntries(list, 80, "lead", 0, true)); !strings.Contains(shown, "list the folders") {
		t.Fatalf("ctrl+t does not show the thought:\n%s", shown)
	}

	applyEvent(&list, acpdriver.Event{Type: acpdriver.EventToolEdit, Tool: &acpdriver.ToolInfo{ID: "c1", Status: "completed"}})
	applyEvent(&list, acpdriver.Event{Type: acpdriver.EventMessage, Text: "# Overview\n\nUses `chi`."})
	done := ansi.Strip(renderEntries(list, 80, "lead", 0, false))
	if !strings.Contains(done, "● Shell(ls internal)") || strings.Contains(done, "#") || !strings.Contains(done, "Overview") {
		t.Fatalf("finished transcript:\n%s", done)
	}
	if line := ansi.Strip(activityLine("Writing", time.Now().Add(-3*time.Second), 0, "esc to stop")); line != "· Writing… (3s · esc to stop)" {
		t.Fatalf("activity line = %q", line)
	}
}

func TestTeammateLiveViewAndPermissions(t *testing.T) {
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: "/p", Lead: lead, Fleet: []config.Agent{lead, builder},
		Run: config.RunConfig{Subagents: config.SubagentConfig{MaxPerBuilder: 2}}})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(model)

	for _, event := range []acpdriver.Event{
		{Type: acpdriver.EventStart, Text: "lead asks: what does this repo do?"},
		{Type: acpdriver.EventToolCall, Tool: &acpdriver.ToolInfo{ID: "r", Title: "Read", Kind: "read", Detail: "go.mod", Status: "in_progress"}},
	} {
		next, _ = m.Update(streamMsg{agent: "builder-1", event: event})
		m = next.(model)
	}
	m.openAgent("builder-1")
	view := ansi.Strip(m.screen())
	for _, want := range []string{"lead asks: what does this repo do?", "Read(go.mod)", "Running read…", "ctrl+x"} {
		if !strings.Contains(view, want) {
			t.Errorf("live view lacks %q:\n%s", want, view)
		}
	}
	next, _ = m.Update(streamMsg{agent: "builder-1", event: acpdriver.Event{Type: acpdriver.EventTurn}})
	if m = next.(model); len(m.agentBusy) != 0 {
		t.Fatal("agent still busy after its turn ended")
	}

	// Reads in its folder go ahead; anything else is the user's call.
	read := &acpdriver.PermissionRequest{Kind: "read", Inside: true, Options: []acpdriver.PermissionOption{{ID: "ok", Kind: "allow_once"}}}
	m.team.permit("builder-1")(read)
	if read.Agent != "builder-1" {
		t.Fatal("permission not labelled with its agent")
	}
	shell := &acpdriver.PermissionRequest{Kind: "execute", Title: "rm -rf build", Options: []acpdriver.PermissionOption{{ID: "a", Name: "Allow once", Kind: "allow_once"}, {ID: "r", Name: "Reject", Kind: "reject_once"}}}
	go m.team.permit("builder-1")(shell)
	next, _ = m.Update(waitTeamPermission(m.teamPerms)())
	m = next.(model)
	panel := ansi.Strip(m.screen())
	for _, want := range []string{"builder-1 wants to", "execute · rm -rf build", "1  Allow once", "2  Reject"} {
		if !strings.Contains(panel, want) {
			t.Errorf("permission panel lacks %q", want)
		}
	}

	// Chat subagents are read-only and only while answering.
	if _, err := m.team.Spawn("builder-1", "", execution.SpawnRequest{Name: "scout", Brief: "look"}); err == nil || !strings.Contains(err.Error(), "while you answer") {
		t.Fatalf("spawn outside an answer: %v", err)
	}
	m.team.asked["builder-1"] = true
	if _, err := m.team.Spawn("builder-1", "", execution.SpawnRequest{Name: "scout", Brief: "look", Files: []string{"a.go"}}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("spawn with files: %v", err)
	}
}
