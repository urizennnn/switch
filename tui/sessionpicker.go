package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/store"
)

// sessionPicker lists this project's past sessions for /resume.
type sessionPicker struct {
	sessions []store.SessionSummary
	index    int
	err      string
}

type resumedMsg struct {
	agent  config.Agent
	chat   *chat.Chat
	status chat.Status
	err    error
}

func (m model) openSessionPicker() (tea.Model, tea.Cmd) {
	sessions, err := m.store.Sessions(m.ctx, m.project, 30)
	if err != nil {
		m.setModal("resume", err.Error())
		return m, nil
	}
	if len(sessions) == 0 {
		m.setModal("resume", "no past sessions with messages in this project yet")
		return m, nil
	}
	m.sessionPicker = &sessionPicker{sessions: sessions}
	return m, nil
}

func (m model) handleSessionPickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	p := m.sessionPicker
	switch msg.String() {
	case "esc":
		m.sessionPicker = nil
	case "up":
		p.index = max(0, p.index-1)
		p.err = ""
	case "down":
		p.index = min(len(p.sessions)-1, p.index+1)
		p.err = ""
	case "enter":
		chosen := p.sessions[p.index]
		if chosen.ID == m.chat.Session.ID {
			p.err = "this is the current session"
			return m, nil
		}
		agent, ok := m.sessionAgent(chosen)
		if !ok {
			p.err = fmt.Sprintf("%s, which owned this session, is no longer a configured agent", orNone(chosen.AgentName))
			return m, nil
		}
		m.sessionPicker = nil
		m.previousLead = m.lead
		m.lead = agent
		m.bumpGen()
		m.busy = true
		m.state = "resuming"
		return m, resumeCmd(m.ctx, m.chat, agent, chosen.ID)
	}
	return m, nil
}

// sessionAgent finds the configured agent that owned a session: by name,
// or by driver for sessions recorded without one.
func (m model) sessionAgent(s store.SessionSummary) (config.Agent, bool) {
	if agent, ok := m.fleetAgent(s.AgentName); ok {
		return agent, true
	}
	if s.AgentName == "" {
		for _, agent := range m.dashboardFleet() {
			if agent.Driver == s.Driver {
				return agent, true
			}
		}
	}
	return config.Agent{}, false
}

func resumeCmd(ctx context.Context, conversation *chat.Chat, agent config.Agent, sessionID string) tea.Cmd {
	return func() tea.Msg {
		next, status, err := conversation.Resume(ctx, agent.Name, agent.Driver, agent.Model, sessionID)
		return resumedMsg{agent: agent, chat: next, status: status, err: err}
	}
}

func (m model) handleResumed(msg resumedMsg) (tea.Model, tea.Cmd) {
	m.busy = false
	m.state = "ready"
	if msg.err != nil {
		m.lead = m.previousLead
		m.setModal("resume failed", msg.err.Error())
		return m, m.listen()
	}
	m.chat = msg.chat
	m.lead = msg.agent
	m.status = msg.status
	_ = m.persistLead(msg.agent.Name)
	m.team.setLead(msg.agent.Name)
	m.home = false
	m.loadHistory()
	m.refreshViewport()
	m.refreshDashboard()
	if msg.status.ResumeError != nil {
		m.setModal("could not resume", msg.agent.Name+" could not reopen that session, so a new one was started.\n\n"+firstLine(msg.status.ResumeError.Error()))
	}
	return m, m.listen()
}

func (m model) sessionPickerContent(width int) string {
	p := m.sessionPicker
	var b strings.Builder
	for i, s := range p.sessions {
		marker := "  "
		if m.chat != nil && s.ID == m.chat.Session.ID {
			marker = "● "
		}
		title := strings.Join(strings.Fields(s.Title), " ")
		if title == "" {
			title = "(no messages from you)"
		}
		meta := fmt.Sprintf("%s · %s · %d msgs", orNone(s.AgentName), ago(s.UpdatedAt), s.Messages)
		titleWidth := max(10, width-8-len(meta))
		line := fmt.Sprintf("%s%-*s  %s", marker, titleWidth, truncate(title, titleWidth), meta)
		style := mutedStyle
		if i == p.index {
			style = activeStyle
		}
		b.WriteString(style.Width(max(10, width-4)).Render(truncate(line, width-4)))
		b.WriteString("\n")
	}
	if p.err != "" {
		b.WriteString("\n" + errorStyle.Render(p.err) + "\n")
	}
	return b.String()
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
