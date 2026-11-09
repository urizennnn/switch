package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
