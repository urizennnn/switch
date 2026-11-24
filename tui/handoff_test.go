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
	m.busy, m.sending, m.state = true, true, "working"
	next, _ := m.Update(<-m.handoffs)
	m = next.(model)
	if m.lead.Name != "lead" || m.pendingHandoff == nil {
		t.Fatal("handoff started mid-reply")
	}
	next, _ = m.Update(turnResultMsg{}) // Send returns before the reply's last events are read
	if m = next.(model); m.lead.Name != "lead" || !m.busy {
		t.Fatal("handoff or input started before the reply's last events")
	}
	next, _ = m.Update(agentEventMsg{gen: m.gen, event: acpdriver.Event{Type: acpdriver.EventTurn}})
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

func TestContextUsageIsPerSession(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: "/p", Store: state, Lead: lead, Fleet: []config.Agent{lead, builder}, Run: config.RunConfig{HandoffAt: 80}})
	session := func(id string) *chat.Chat {
		return &chat.Chat{Project: "/p", Store: state, Session: &acpdriver.Session{ID: id}}
	}
	m.chat = session("s1")
	used := func(m model, agent string) bool {
		return m.team.Limits(agent).Size > 0 || m.ctxSize > 0
	}
	full := func(m model) model {
		next, _ := m.Update(agentEventMsg{gen: m.gen, event: acpdriver.Event{Type: acpdriver.EventUsage, Usage: &acpdriver.UsageInfo{Used: 85, Size: 100}}})
		return next.(model)
	}

	m = full(m)
	next, _ := m.Update(newSessionResultMsg{chat: session("s2")})
	if m = next.(model); used(m, "lead") || strings.Contains(m.team.describe("lead"), "% full") {
		t.Fatalf("/new kept the old session's context: %q", m.team.describe("lead"))
	}

	m = full(m)
	next, _ = m.handleResumed(resumedMsg{agent: lead, chat: session("s3")})
	if m = next.(model); used(m, "lead") {
		t.Fatal("/resume kept the old session's context")
	}

	// A lead switch shows the new lead's own session: a warm one keeps what
	// it used, a new one starts empty.
	m.team.setUsage("builder-1", acpdriver.UsageInfo{Used: 30, Size: 100})
	warm := session("w")
	next, _ = m.Update(leadChangedMsg{agent: builder, chat: warm, adopted: warm.Session})
	if m = next.(model); m.ctxUsed != 30 || m.ctxSize != 100 {
		t.Fatalf("adopted a warm session: context %d/%d", m.ctxUsed, m.ctxSize)
	}
	m.team.setUsage("lead", acpdriver.UsageInfo{Used: 85, Size: 100})
	next, _ = m.Update(leadChangedMsg{agent: lead, chat: session("s4")})
	if m = next.(model); used(m, "lead") {
		t.Fatal("a new session for the lead kept an old session's context")
	}
}

func TestLimitHandoffsDoNotPingPong(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	project := t.TempDir()
	// Two leads on one subscription share its limit.
	a := config.Agent{Name: "a", Role: "lead", Driver: "claude", Handoff: "b"}
	b := config.Agent{Name: "b", Role: "lead", Driver: "claude", Handoff: "a"}
	m := New(Options{Context: context.Background(), Project: project, Store: state, Lead: a, Fleet: []config.Agent{a, b}})
	m.chat = &chat.Chat{Project: project, Store: state, Session: &acpdriver.Session{ID: "s1"}}
	m.connecting, m.state = false, "ready"
	limit := turnResultMsg{err: errors.New("You've hit your usage limit")}
	send := func(m model, text string) model {
		m.input.SetValue(text)
		next, _ := m.Update(keyEnter())
		if m = next.(model); !m.sending {
			t.Fatalf("%q was not sent", text)
		}
		return m
	}

	m = send(m, "go on")
	next, _ := m.Update(limit)
	if m = next.(model); m.lead.Name != "b" {
		t.Fatalf("a did not hand off: lead = %s", m.lead.Name)
	}
	next, _ = m.Update(leadChangedMsg{agent: b, chat: &chat.Chat{Project: project, Store: state, Session: &acpdriver.Session{ID: "s2"}}})
	m = next.(model)
	next, _ = m.Update(limit) // b is out too
	if m = next.(model); m.lead.Name != "b" || m.handoff != nil || m.pendingHandoff != nil || !strings.Contains(m.toastText(), "hit a limit") {
		t.Fatalf("b handed back to a: lead = %s, toast %q", m.lead.Name, m.toastText())
	}

	// The user's next message may hand off again.
	m = send(m, "try again")
	next, _ = m.Update(limit)
	if m = next.(model); m.lead.Name != "a" {
		t.Fatalf("after a new message: lead = %s", m.lead.Name)
	}
}

func TestLeadSwitchCarriesTheConversation(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	project := t.TempDir()
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	fakeAdapter(t) // /lead needs builder-1's adapter installed
	m := New(Options{Context: context.Background(), Project: project, Store: state, Lead: lead, Fleet: []config.Agent{lead, builder}})
	m.chat = &chat.Chat{Project: project, Store: state, Session: &acpdriver.Session{ID: "s1"}}
	m.connecting, m.state = false, "ready"
	m.appendUser("add a README")
	m.appendAssistantChunk("The outline is agreed: install, usage, config.")
	m.closeAssistant()

	// The user switches leads: the new lead gets the recent conversation.
	next, _ := m.handleSlash("/lead builder-1")
	if m = next.(model); m.lead.Name != "builder-1" || m.state != "starting lead" {
		t.Fatalf("lead switch did not start: state %s, toast %q", m.state, m.toastText())
	}
	next, cmd := m.Update(leadChangedMsg{agent: builder, chat: &chat.Chat{Project: project, Store: state, Session: &acpdriver.Session{ID: "s2"}}})
	m = next.(model)
	if cmd == nil || !m.sending || !strings.Contains(m.renderTranscript(80), "builder-1 has the recent conversation with lead") {
		t.Fatalf("no recap was sent:\n%s", m.renderTranscript(80))
	}
}

func TestRecapIsTheRecentConversation(t *testing.T) {
	m := model{lead: config.Agent{Name: "builder-1"}, previousLead: config.Agent{Name: "lead"}}
	if m.recap() != nil {
		t.Fatal("a recap of an empty conversation")
	}
	m.appendUser("add a README")
	m.appendTool("t", "Read", "completed")
	m.appendAssistantChunk("The outline is agreed.")
	m.closeAssistant()
	addAside(&m.entries, entry{kind: entryAssistant, who: "builder-2", text: "an @mention answer"})
	h := m.recap()
	if h == nil || h.from != "lead" || h.to != "builder-1" || !h.recap ||
		!strings.Contains(h.note, "The user: add a README") || !strings.Contains(h.note, "lead: The outline is agreed.") || strings.Contains(h.note, "Read") ||
		strings.Contains(h.note, "@mention") {
		t.Fatalf("recap = %+v", h)
	}
	m.appendUser(strings.Repeat("long ", 2000))
	if h := m.recap(); len([]rune(h.note)) > 3001 || !strings.HasSuffix(h.note, "long") {
		t.Fatalf("a long recap is %d runes, or lost its end", len([]rune(h.note)))
	}
}

func TestFailedHandoffKeepsTheNote(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: "/p", Store: state, Lead: builder, Fleet: []config.Agent{lead, builder}})
	m.chat = &chat.Chat{Project: "/p", Store: state, Session: &acpdriver.Session{ID: "s1"}}
	m.previousLead, m.busy, m.state = lead, true, "starting lead"
	m.handoff = &handoffMsg{from: "lead", to: "builder-1", note: "the README outline is agreed"}
	next, _ := m.Update(leadChangedMsg{agent: builder, err: errors.New("claude adapter is not installed")})
	if m = next.(model); m.lead.Name != "lead" || m.handoff != nil || !strings.Contains(m.entries[len(m.entries)-1].text, "the README outline is agreed") {
		t.Fatalf("lead = %s, note dropped: %+v", m.lead.Name, m.entries)
	}
	if saved, _ := state.Messages(context.Background(), "/p", "s1", 5); len(saved) != 1 || !strings.Contains(saved[0].Content, "the README outline is agreed") {
		t.Fatalf("the note is not in the old lead's session: %+v", saved)
	}
}
