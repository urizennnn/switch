package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
)

func BenchmarkScrollLargeToolOutput(b *testing.B) {
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}
	m := New(Options{Context: context.Background(), Lead: lead})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 35})
	m = next.(model)
	m.home = false
	addTool(&m.entries, acpdriver.ToolInfo{ID: "large", Title: "Read", Status: "completed", Output: strings.Repeat("func example() { println(\"hello\") }\n", 3000)})
	m.refreshViewport()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		next, _ := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
		m = next.(model)
		_ = m.View()
	}
}

func BenchmarkRefreshRunningToolOutput(b *testing.B) {
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}
	m := New(Options{Context: context.Background(), Lead: lead})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 35})
	m = next.(model)
	m.home = false
	addTool(&m.entries, acpdriver.ToolInfo{ID: "large", Title: "Read", Status: "in_progress", Output: strings.Repeat("func example() { println(\"hello\") }\n", 3000)})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.refreshViewport()
	}
}

// A long session: 250 turns, about 7.7k lines.
func longSession() model {
	m := New(Options{Context: context.Background(), Lead: config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 35})
	m = next.(model)
	m.home, m.connecting, m.state = false, false, "ready"
	m.chat = &chat.Chat{Session: &acpdriver.Session{ID: "s1"}}
	for i := range 250 {
		m.appendUser(fmt.Sprintf("please look at file %d and fix the thing", i))
		applyEvent(&m.entries, acpdriver.Event{Type: acpdriver.EventThought, Text: "I should read the file first."})
		for j := range 3 {
			applyEvent(&m.entries, acpdriver.Event{Type: acpdriver.EventToolCall, Tool: &acpdriver.ToolInfo{ID: fmt.Sprint(i, j), Title: "Read", Detail: fmt.Sprintf("file%d.go", j), Status: "completed"}})
		}
		applyEvent(&m.entries, acpdriver.Event{Type: acpdriver.EventMessage, Text: "## Summary\n\nI found **three** issues:\n\n- one in `main.go`\n- one in `util.go`\n- one in the tests\n\n```go\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n```\n\nThat should do it."})
		closeOpen(&m.entries)
	}
	m.appendUser("go on")
	m.busy, m.state = true, "working"
	m.refreshViewport()
	return m
}

// Each streamed chunk and each clock redraw of a long session; neither may
// grow with the transcript, or the lead's events back up.
func BenchmarkStreamLongSession(b *testing.B) {
	m := longSession()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		next, _ := m.Update(agentEventMsg{gen: m.gen, event: acpdriver.Event{Type: acpdriver.EventMessage, Text: "word "}})
		next, _ = next.(model).Update(clockMsg{})
		m = next.(model)
		_ = m.View()
	}
}

// While the lead works, its events only update the transcript; the clock
// redraws it, so a fast stream costs one redraw per tick.
func TestStreamedRepliesShowOnTheClock(t *testing.T) {
	m := longSession()
	defer m.cancel()
	next, _ := m.Update(agentEventMsg{gen: m.gen, event: acpdriver.Event{Type: acpdriver.EventMessage, Text: "streamed reply"}})
	next, _ = next.(model).Update(clockMsg{})
	if screen := ansi.Strip(next.(model).screen()); !strings.Contains(screen, "streamed reply") {
		t.Fatalf("the clock did not redraw the streamed reply:\n%s", screen)
	}
}
