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

func TestZenHidesSidebar(t *testing.T) {
	m := New(Options{Context: context.Background(), Project: "/p", Lead: config.Agent{Name: "lead", Driver: "opencode"}})
	m.width, m.height = 120, 30
	if !strings.Contains(m.View().Content, "WORKSPACE") {
		t.Fatal("sidebar missing before zen")
	}
	m.toggleZen()
	side, main := m.paneWidths(120)
	if side != 0 || main <= 100 || strings.Contains(m.View().Content, "WORKSPACE") {
		t.Fatalf("zen: sidebar=%d main=%d", side, main)
	}
	m.toggleZen()
	if !strings.Contains(m.View().Content, "WORKSPACE") {
		t.Fatal("sidebar did not come back")
	}
}

func TestResumePickerRules(t *testing.T) {
	m := model{
		lead:  config.Agent{Name: "lead", Driver: "opencode"},
		fleet: []config.Agent{{Name: "lead", Role: "lead", Driver: "opencode"}, {Name: "builder-1", Role: "builder", Driver: "claude"}},
		chat:  &chat.Chat{Session: &acpdriver.Session{ID: "current"}},
	}
	m.sessionPicker = &sessionPicker{sessions: []store.SessionSummary{
		{ID: "current", AgentName: "lead"},
		{ID: "gone", AgentName: "retired-agent"},
		{ID: "past", AgentName: "builder-1", Driver: "claude"},
	}}

	next, cmd := m.handleSessionPickerKey(keyEnter())
	m = next.(model)
	if cmd != nil || !strings.Contains(m.sessionPicker.err, "current session") {
		t.Fatalf("resuming the current session: err=%q", m.sessionPicker.err)
	}
	m.sessionPicker.index = 1
	next, cmd = m.handleSessionPickerKey(keyEnter())
	m = next.(model)
	if cmd != nil || !strings.Contains(m.sessionPicker.err, "no longer a configured agent") {
		t.Fatalf("unconfigured owner: err=%q", m.sessionPicker.err)
	}
	m.sessionPicker.index = 2
	next, cmd = m.handleSessionPickerKey(keyEnter())
	m = next.(model)
	if cmd == nil || m.sessionPicker != nil || m.lead.Name != "builder-1" || !m.busy {
		t.Fatalf("resume did not start: lead=%q busy=%v", m.lead.Name, m.busy)
	}
}

func TestResumedShowsThatSessionAndReportsFallback(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	_ = state.AppendMessage(ctx, "/p", "past", "user", "earlier work")

	agent := config.Agent{Name: "builder-1", Driver: "claude"}
	m := model{ctx: ctx, store: state, project: "/p", previousLead: config.Agent{Name: "lead"}}
	resumed := &chat.Chat{Project: "/p", Store: state, Session: &acpdriver.Session{ID: "past"}}
	next, _ := m.handleResumed(resumedMsg{agent: agent, chat: resumed, status: chat.Status{Resumed: true}})
	m = next.(model)
	if m.lead.Name != "builder-1" || m.modal != "" || !strings.Contains(m.renderTranscript(60), "earlier work") {
		t.Fatalf("resume: lead=%q modal=%q", m.lead.Name, m.modal)
	}

	fresh := &chat.Chat{Project: "/p", Store: state, Session: &acpdriver.Session{ID: "fresh"}}
	next, _ = m.handleResumed(resumedMsg{agent: agent, chat: fresh, status: chat.Status{ResumeError: errors.New("not found")}})
	m = next.(model)
	if !strings.Contains(m.modal, "new one was started") || len(m.entries) != 0 {
		t.Fatalf("fallback: modal=%q entries=%d", m.modal, len(m.entries))
	}
}
