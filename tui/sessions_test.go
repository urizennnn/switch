package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/store"
)

func TestZenHidesSidebar(t *testing.T) {
	m := New(Options{Context: context.Background(), Project: "/p", Lead: config.Agent{Name: "lead", Driver: "opencode"}})
	m.width, m.height = 120, 30
	if v := m.View(); !strings.Contains(v.Content, "WORKSPACE") || !v.AltScreen {
		t.Fatal("sidebar missing before zen, or not in the alternate screen")
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
	// Older builds stored no agent name, and the driver as the config spelled it.
	if agent, ok := m.sessionAgent(store.SessionSummary{ID: "old", Driver: "Claude-Code"}); !ok || agent.Name != "builder-1" {
		t.Fatalf("a session stored under Claude-Code belongs to %q, %v", agent.Name, ok)
	}

	picker := m.sessionPicker
	next, cmd := m.handleSessionPickerKey(keyEnter())
	m = next.(model)
	if cmd != nil || m.sessionPicker != nil {
		t.Fatal("enter on the current session should just close the picker")
	}
	m.sessionPicker = picker
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
	if !strings.Contains(m.toastText(), "new one was started") || len(m.entries) != 0 {
		t.Fatalf("fallback: toast=%q entries=%d", m.toastText(), len(m.entries))
	}
}

func TestSidebarShowsOneLead(t *testing.T) {
	m := model{
		lead: config.Agent{Name: "builder-1", Role: "builder", Driver: "claude", Model: "sonnet"},
		fleet: []config.Agent{
			{Name: "lead", Role: "lead", Driver: "opencode"},
			{Name: "builder-1", Role: "builder", Driver: "claude"},
			{Name: "builder-2", Role: "builder", Driver: "codex"},
		},
	}
	out := m.sidebarContent(30)
	if strings.Count(out, "●") != 1 || !strings.Contains(out, "builder-1") || !strings.Contains(out, "claude · sonnet") {
		t.Fatalf("lead section wrong:\n%s", out)
	}
	if strings.Count(out, "○") != 2 || !strings.Contains(out, "standby · opencode") {
		t.Fatalf("an agent that is not leading is not listed as a standby:\n%s", out)
	}
	if !strings.Contains(out, "builder-2") || !strings.Contains(out, "WORKERS") {
		t.Fatalf("worker missing:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > 26 {
			t.Fatalf("line wider than the panel (%d): %q", w, line)
		}
	}
}

func TestSessionPickerAndResumeID(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	project := t.TempDir()
	_ = state.SaveChatSession(ctx, store.ChatSession{Project: project, Name: "lead", Agent: "opencode", SessionID: "old"})
	_ = state.AppendMessage(ctx, project, "old", "user", "an earlier question")

	m := New(Options{Context: ctx, Project: project, Store: state, Lead: config.Agent{Name: "lead", Driver: "opencode"}})
	m.chat = &chat.Chat{Agent: "opencode", Session: &acpdriver.Session{ID: "now"}}
	if m.ResumeID() != "" {
		t.Fatal("an empty session is offered for resuming")
	}
	m.appendUser("hi")
	if m.ResumeID() != "now" {
		t.Fatalf("ResumeID = %q", m.ResumeID())
	}

	next, _ := m.handleSlash("/session")
	m = next.(model)
	if p := m.sessionPicker; p == nil || len(p.sessions) != 2 || p.sessions[0].ID != "now" || p.sessions[1].ID != "old" {
		t.Fatalf("picker = %+v", m.sessionPicker)
	}
	m.width, m.height = 120, 30
	if out := m.sessionPickerContent(80); !strings.Contains(out, "current") || !strings.Contains(out, "switch --resume now") {
		t.Fatalf("picker content:\n%s", out)
	}
}

func TestSessionCommandsWaitForTheTurn(t *testing.T) {
	m := New(Options{Context: context.Background(), Project: "/p", Lead: config.Agent{Name: "lead", Driver: "opencode"},
		Fleet: []config.Agent{{Name: "lead", Driver: "opencode"}, {Name: "builder-1", Driver: "opencode"}}})
	m.chat = &chat.Chat{Session: &acpdriver.Session{ID: "now"}}
	m.connecting, m.state = false, "ready"
	// Mid-turn, and after the reply ended while its Send still writes to the
	// session: switching sessions or leads would close or copy it under Send.
	for _, at := range []struct{ busy, sending bool }{{true, true}, {false, true}} {
		m.busy, m.sending = at.busy, at.sending
		for _, command := range []string{"/session", "/new", "/lead builder-1"} {
			next, cmd := m.handleSlash(command)
			if got := next.(model); cmd != nil || got.sessionPicker != nil || got.gen != m.gen || !strings.Contains(got.toastText(), "busy") {
				t.Errorf("%s ran with busy=%v sending=%v", command, at.busy, at.sending)
			}
		}
	}
}

func TestWholeReplyIsSaved(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	m := New(Options{Context: ctx, Project: "/p", Store: state, Lead: config.Agent{Name: "lead", Driver: "opencode"}})
	m.chat = &chat.Chat{Project: "/p", Store: state, Session: &acpdriver.Session{ID: "s1"}}
	m.connecting, m.busy, m.state = false, true, "working"
	m.appendUser("restructure x")
	var cmd tea.Cmd
	for _, event := range []acpdriver.Event{
		{Type: acpdriver.EventMessage, Text: "I'll restructure "},
		{Type: acpdriver.EventMessage, Text: "x."},
		{Type: acpdriver.EventToolCall, Tool: &acpdriver.ToolInfo{ID: "e", Title: "Edit", Status: "completed"}},
		{Type: acpdriver.EventThought, Text: "check it"},
		{Type: acpdriver.EventMessage, Text: "Done."},
		{Type: acpdriver.EventTurn},
	} {
		var next tea.Model
		next, cmd = m.Update(agentEventMsg{gen: m.gen, event: event})
		m = next.(model)
	}
	m.bumpGen() // the turn's listener returns
	for _, c := range cmd().(tea.BatchMsg) {
		c()
	}
	messages, _ := state.Messages(ctx, "/p", "s1", 10)
	if len(messages) != 1 || messages[0].Content != "I'll restructure x.\n\nDone." {
		t.Fatalf("saved %+v", messages)
	}
}

func TestResumeFallbackAtLaunchIsReported(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	m := New(Options{Context: context.Background(), Project: "/p", Store: state, Lead: config.Agent{Name: "lead", Driver: "opencode"}, Resume: "gone"})
	fresh := &chat.Chat{Project: "/p", Store: state, Session: &acpdriver.Session{ID: "fresh"}}
	next, _ := m.Update(connectedMsg{chat: fresh, status: chat.Status{ResumeError: errors.New("session not found")}})
	if got := next.(model).toastText(); !strings.Contains(got, "new one was started") || !strings.Contains(got, "session not found") {
		t.Fatalf("toast = %q", got)
	}
}

func TestHistoryShowsWhatTheUserSaid(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	for i := range 52 {
		_ = state.AppendMessage(ctx, "/p", "s", "assistant", fmt.Sprintf("reply %d", i))
	}
	_ = state.AppendMessage(ctx, "/p", "s", "user", planModeNote+"plan the auth work")
	_ = state.AppendMessage(ctx, "/p", "s", "user", "[Switch] lead was the lead and handed the conversation to you near its limit")
	_ = state.AppendMessage(ctx, "/p", "s", "user", planModeNote+"[Switch] The user made you the lead in place of lead.") // sent in plan mode
	m := model{ctx: ctx, store: state, project: "/p", chat: &chat.Chat{Project: "/p", Store: state, Session: &acpdriver.Session{ID: "s"}}}
	m.loadHistory()
	if e := m.entries[0]; e.kind != entrySystem || !strings.Contains(e.text, "earlier messages") {
		t.Fatalf("first entry = %+v, want a note that earlier messages are not shown", e)
	}
	n := len(m.entries)
	if e := m.entries[n-3]; e.kind != entryUser || e.text != "plan the auth work" {
		t.Fatalf("plan-mode message shown as %+v", e)
	}
	for _, e := range m.entries[n-2:] {
		if e.kind != entrySystem || strings.Contains(e.text, "plan mode") {
			t.Fatalf("Switch's prompt shown as %+v", e)
		}
	}
	if m.ResumeID() == "" {
		t.Fatal("the user's own message no longer counts as a conversation")
	}
}
