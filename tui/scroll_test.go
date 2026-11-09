package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
)

func TestScrollConversationAgentViewAndPanels(t *testing.T) {
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: "/p", Lead: lead, Fleet: []config.Agent{lead, builder}})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = next.(model)
	update := func(msg tea.Msg) {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	// The terminal sends the wheel as arrow keys (alternate scroll mode).
	wheel := func(code rune) {
		for range 3 {
			update(tea.KeyPressMsg{Code: code})
		}
	}

	// The conversation: long output starts at the end and scrolls back.
	m.home = false
	var long []string
	for i := 1; i <= 80; i++ {
		long = append(long, fmt.Sprintf("line %d", i))
	}
	m.appendAssistantChunk(strings.Join(long, "\n\n"))
	m.closeAssistant()
	m.refreshViewport()
	if screen := ansi.Strip(m.screen()); !strings.Contains(screen, "line 80") || strings.Contains(screen, "LINES BELOW") {
		t.Fatalf("conversation does not start at the end")
	}
	if m.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("mouse wheel reporting is not enabled")
	}
	update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.viewport.AtBottom() {
		t.Fatal("mouse wheel up did not scroll the transcript")
	}
	update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if !m.viewport.AtBottom() {
		t.Fatal("mouse wheel down did not return to the end")
	}
	wheel(tea.KeyUp)
	update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if screen := ansi.Strip(m.screen()); strings.Contains(screen, "line 80") || !strings.Contains(screen, "LINES BELOW") {
		t.Fatalf("conversation did not scroll back:\n%s", screen)
	}
	// New output while scrolled back does not yank the view down.
	m.appendAssistantChunk("fresh")
	m.refreshViewport()
	if strings.Contains(ansi.Strip(m.screen()), "fresh") {
		t.Fatal("new output pulled the view to the end")
	}
	update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	if !strings.Contains(ansi.Strip(m.screen()), "fresh") {
		t.Fatal("ctrl+end did not return to the latest output")
	}
	update(tea.WindowSizeMsg{Width: 72, Height: 22})
	if !strings.Contains(ansi.Strip(m.screen()), "fresh") {
		t.Fatal("resizing while following lost the latest output")
	}

	// An agent view: scrolls back, then follows again at the end.
	for i := 1; i <= 60; i++ {
		update(streamMsg{agent: "builder-1", event: acpdriver.Event{Type: acpdriver.EventToolCall,
			Tool: &acpdriver.ToolInfo{ID: fmt.Sprint(i), Title: "Read", Detail: fmt.Sprintf("file%d.go", i), Status: "completed"}}})
	}
	m.openAgent("builder-1")
	if screen := ansi.Strip(m.screen()); !strings.Contains(screen, "file60.go") {
		t.Fatal("agent view does not start at the end")
	}
	for range 5 {
		wheel(tea.KeyUp)
	}
	if screen := ansi.Strip(m.screen()); strings.Contains(screen, "file60.go") || !strings.Contains(screen, "LINES BELOW") {
		t.Fatalf("agent view did not scroll back:\n%s", screen)
	}
	for range 20 {
		wheel(tea.KeyDown)
	}
	if m.agentTop != -1 {
		t.Fatalf("agent view does not follow again at the end (top %d)", m.agentTop)
	}

	// A long reading panel scrolls instead of closing on pgdown.
	m.setModal("help", strings.Join(long, "\n"))
	if !strings.Contains(ansi.Strip(m.screen()), "lines 1-") {
		t.Fatal("long panel has no scroll position")
	}
	update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if m.modal == "" || m.modalTop == 0 || strings.Contains(ansi.Strip(m.screen()), "line 1\n") {
		t.Fatalf("pgdown closed or did not scroll the panel (top %d)", m.modalTop)
	}
}
