package tui

import (
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
)

// In the chat, the lead hands off by passing the lead role: when it nears
// its context limit it calls switch_handoff, and once its reply ends Switch
// makes the target the lead and sends it the note. When a limit stops the
// lead first, Switch hands off to its configured agent with the user's last
// message. Workers hand off in runs, through the engine.

type handoffMsg struct {
	from, to, note string
	recap          bool // the note is the conversation, for a lead the user picked
	waiting        bool // its target is busy, and the user was told
}

func waitHandoff(handoffs <-chan handoffMsg) tea.Cmd {
	return func() tea.Msg { return <-handoffs }
}

func (t *team) setUsage(agent string, usage acpdriver.UsageInfo) {
	if usage.Size <= 0 {
		return
	}
	t.mu.Lock()
	t.usage[agent] = usage
	t.mu.Unlock()
}

// resetUsage forgets an agent's context use when it gets a new session.
func (t *team) resetUsage(agent string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.usage, agent)
	t.mu.Unlock()
}

// Limits implements the control server's limits for chat agents.
func (t *team) Limits(agent string) execution.Limits {
	t.mu.Lock()
	usage := t.usage[agent]
	t.mu.Unlock()
	return execution.Limits{Used: usage.Used, Size: usage.Size, HandoffAt: t.handoffAt, HandoffTo: t.handoffTarget(agent)}
}

// handoffTarget is the agent configured to continue from, or else a standby
// lead; "" when there is none.
func (t *team) handoffTarget(from string) string {
	if agent, ok := t.agent(from); ok && agent.Handoff != "" {
		return agent.Handoff
	}
	for _, agent := range t.fleet {
		if agent.Role == "lead" && agent.Name != from {
			return agent.Name
		}
	}
	return ""
}

// Handoff implements switch_handoff in the chat, where only the lead hands
// off; it takes effect when the lead's reply ends.
func (t *team) Handoff(from, _, to, note string) (string, error) {
	if from != t.leadName() {
		return "", errors.New("in the chat only the lead hands off; answer the question you were asked")
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return "", errors.New("handoff needs a note: what the user wants, what is done, and what is left")
	}
	if to = strings.TrimSpace(to); to == "" {
		to = t.handoffTarget(from)
	}
	if _, ok := t.agent(to); !ok || to == from {
		return "", errors.New("name the teammate to hand off to (switch_team lists them)")
	}
	if t.handoffs == nil {
		return "", errors.New("handoffs are not available here")
	}
	select {
	case t.handoffs <- handoffMsg{from: from, to: to, note: note}:
	default:
		return "", errors.New("a handoff is already pending")
	}
	return fmt.Sprintf("%s takes over as lead when this reply ends, with your note. Finish your reply now with one line telling the user.", to), nil
}

// limitHandoff queues a handoff for a lead a limit stopped.
func (m *model) limitHandoff(reason string) {
	if m.limited == nil {
		m.limited = map[string]bool{}
	}
	m.limited[m.lead.Name] = true
	to := m.team.handoffTarget(m.lead.Name)
	switch {
	case to == "":
		m.fail(m.lead.Name+" hit a limit", reason+". Set handoff = \"<agent>\" for it in .switch/config.toml, or pick a lead with /lead.")
		return
	case m.limited[to]: // agents that share a limit would hand off to each other until it lifts
		m.fail(m.lead.Name+" hit a limit", reason+". "+to+" hit one too since your last message; send another to try again, or pick a lead with /lead.")
		return
	}
	note := fmt.Sprintf("%s stopped at a limit (%s) before it finished.", m.lead.Name, reason)
	for i := len(m.entries) - 1; i >= 0; i-- {
		if e := m.entries[i]; e.kind == entryUser && e.who == "" {
			note += " The user's last message was:\n" + e.text
			break
		}
	}
	m.pendingHandoff = &handoffMsg{from: m.lead.Name, to: to, note: note}
}

// afterTurn starts a pending handoff once the lead's turn has ended and its
// Send has returned: switching copies the Chat that Send still writes to.
func (m model) afterTurn() (tea.Model, tea.Cmd) {
	h := m.pendingHandoff
	if h == nil || m.busy || m.sending {
		return m, nil
	}
	agent, ok := m.fleetAgent(h.to)
	if !ok {
		m.pendingHandoff = nil
		m.fail("handoff failed", h.to+" is not a configured agent")
		return m, nil
	}
	// A switch now would start a second session beside the one answering or
	// working; the dashboard tick tries again.
	if status := m.team.status(h.to); status == "answering" || status == "working" {
		if !h.waiting {
			waiting := *h
			waiting.waiting = true
			m.pendingHandoff = &waiting
			m.notify(h.from+" hands off to "+h.to+" once it is free", h.to+" is "+status)
		}
		return m, nil
	}
	m.pendingHandoff = nil
	m.handoff = h
	m.notify(h.from+" is handing off to "+h.to, firstLine(h.note))
	return m.beginLeadSwitch(agent)
}

// deliverHandoff gives the new lead its predecessor's note.
func (m model) deliverHandoff() (model, tea.Cmd) {
	h := m.handoff
	m.handoff = nil
	if h == nil {
		return m, nil
	}
	shown := h.from + " handed off to " + h.to + ": " + h.note
	prompt := fmt.Sprintf("[Switch] %s was the lead and handed the conversation to you near its limit; you are now the lead. Its note:\n%s\n\n"+
		"Continue from where it left off.", h.from, h.note)
	if h.recap {
		shown = h.to + " has the recent conversation with " + h.from
		prompt = fmt.Sprintf("[Switch] The user made you the lead in place of %s. The recent conversation:\n\n%s\n\n"+
			"Reply in one line that you have this context; the user's next message says what to do.", h.from, h.note)
	}
	m.appendSystem(shown)
	m.refreshViewport()
	m.busy, m.sending, m.state = true, true, "working"
	return m, sendPrompt(m.ctx, m.chat, prompt, m.permissionMode)
}

// recap is the end of the conversation for the lead the user switches to,
// so the user need not explain it again; nil when there is none.
func (m model) recap() *handoffMsg {
	var b strings.Builder
	for _, e := range m.entries {
		switch {
		case e.kind == entryUser && e.who == "":
			b.WriteString("\n\nThe user: " + e.text)
		case e.kind == entryAssistant && e.who == "": // a teammate's answer to an @mention is not the lead's
			b.WriteString("\n\n" + m.previousLead.Name + ": " + e.text)
		}
	}
	note := []rune(strings.TrimSpace(b.String()))
	if len(note) == 0 {
		return nil
	}
	if len(note) > 3000 {
		note = append([]rune("…"), note[len(note)-3000:]...)
	}
	return &handoffMsg{from: m.previousLead.Name, to: m.lead.Name, note: string(note), recap: true}
}
