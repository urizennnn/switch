package tui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/store"
)

func TestLeadHandsOffInTheChat(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	project := t.TempDir()
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode", Handoff: "builder-1"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: project, Store: state, Lead: lead,
		Fleet: []config.Agent{lead, builder}, Run: config.RunConfig{HandoffAt: 80}})
	m.chat = &chat.Chat{Project: project, Store: state, Session: &acpdriver.Session{ID: "s1"}}
	m.connecting, m.state = false, "ready"

	// The lead learns how full it is, and to hand off past the threshold.
	m.team.setUsage("lead", acpdriver.UsageInfo{Used: 50, Size: 100})
	if note := m.team.describe("lead"); !strings.Contains(note, "50% full") || strings.Contains(note, "switch_handoff") {
		t.Fatalf("below the threshold: %q", note)
	}
	m.team.setUsage("lead", acpdriver.UsageInfo{Used: 85, Size: 100})
	if note := m.team.describe("lead"); !strings.Contains(note, "85% full") || !strings.Contains(note, "builder-1 takes over") {
		t.Fatalf("past the threshold: %q", note)
	}

	// Only the lead hands off in the chat.
	if _, err := m.team.Handoff("builder-1", "", "", "x"); err == nil {
		t.Fatal("a teammate handed off")
	}
	if _, err := m.team.Handoff("lead", "", "", "the user wants a README; the outline is agreed"); err != nil {
		t.Fatal(err)
	}

	// The handoff waits for the lead's reply to end, then switches the lead.
	m.busy, m.state = true, "working"
	next, _ := m.Update(<-m.handoffs)
	m = next.(model)
	if m.lead.Name != "lead" || m.pendingHandoff == nil {
		t.Fatal("handoff started mid-reply")
	}
	next, _ = m.Update(turnResultMsg{})
	m = next.(model)
	if m.lead.Name != "builder-1" || m.state != "starting lead" || m.handoff == nil {
		t.Fatalf("lead = %s state = %s after the reply ended", m.lead.Name, m.state)
	}

	// Once the new lead connects, it gets the note.
	next, cmd := m.Update(leadChangedMsg{agent: builder, chat: &chat.Chat{Project: project, Store: state, Session: &acpdriver.Session{ID: "s2"}}})
	m = next.(model)
	if cmd == nil || !m.busy || !strings.Contains(m.renderTranscript(80), "lead handed off to builder-1: the user wants a README") {
		t.Fatalf("note not delivered:\n%s", m.renderTranscript(80))
	}

	// A usage limit hands off on its own, to builder-1's lead fallback.
	m.busy, m.state = true, "working"
	m.appendUser("keep going")
	next, _ = m.Update(turnResultMsg{err: errors.New("You've hit your usage limit")})
	m = next.(model)
	if m.lead.Name != "lead" || m.handoff == nil || !strings.Contains(m.handoff.note, "keep going") {
		t.Fatalf("limit did not hand off: lead = %s", m.lead.Name)
	}
}
