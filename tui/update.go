package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
)

type agentEventMsg struct {
	event acpdriver.Event
}

type permissionMsg struct {
	request *acpdriver.PermissionRequest
}

type turnResultMsg struct {
	err error
}

type saveResultMsg struct {
	err error
}

func waitForActivity(session *acpdriver.Session) tea.Cmd {
	return func() tea.Msg {
		select {
		case event, ok := <-session.Events:
			if !ok {
				return agentEventMsg{event: acpdriver.Event{Type: acpdriver.EventExit}}
			}
			return agentEventMsg{event: event}
		case request := <-session.Permissions():
			return permissionMsg{request: request}
		case <-session.Done():
			return agentEventMsg{event: acpdriver.Event{Type: acpdriver.EventExit}}
		}
	}
}

func sendPrompt(ctx context.Context, conversation *chat.Chat, text string) tea.Cmd {
	return func() tea.Msg {
		_, err := conversation.Send(ctx, text)
		return turnResultMsg{err: err}
	}
}

func saveAssistant(ctx context.Context, conversation *chat.Chat, text string) tea.Cmd {
	return func() tea.Msg {
		return saveResultMsg{err: conversation.SaveAssistant(ctx, text)}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if m.permission != nil {
			return m.handlePermissionKey(msg)
		}

		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "esc":
			if m.busy {
				_ = m.chat.Session.Cancel(context.Background())
				m.state = "cancelling"
			}
			return m, nil

		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" || m.busy {
				return m, nil
			}
			if strings.HasPrefix(text, "/") {
				return m.handleSlash(text)
			}
			m.input.Reset()
			m.appendUser(text)
			m.assistant.Reset()
			m.busy = true
			m.state = "working"
			m.refreshViewport()
			return m, sendPrompt(m.ctx, m.chat, text)
		}

		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewport.SetWidth(max(20, msg.Width-2))
		m.viewport.SetHeight(max(5, msg.Height-8))
		m.input.SetWidth(max(10, msg.Width-4))
		m.refreshViewport()
		return m, nil

	case agentEventMsg:
		return m.handleAgentEvent(msg.event)

	case permissionMsg:
		m.permission = msg.request
		m.state = "permission"
		return m, nil

	case turnResultMsg:
		if msg.err != nil {
			if m.state != "error" {
				m.appendError(msg.err.Error())
			}
			m.state = "error"
		} else if m.state == "working" {
			m.state = "ready"
		}
		m.busy = false
		m.refreshViewport()
		return m, nil

	case saveResultMsg:
		if msg.err != nil {
			m.appendError("save: " + msg.err.Error())
			m.refreshViewport()
		}
		return m, nil
	}

	return m, nil
}

func (m model) handleAgentEvent(event acpdriver.Event) (tea.Model, tea.Cmd) {
	switch event.Type {
	case acpdriver.EventMessage:
		m.appendAssistantChunk(event.Text)
		m.refreshViewport()
		return m, waitForActivity(m.chat.Session)

	case acpdriver.EventThought:
		return m, waitForActivity(m.chat.Session)

	case acpdriver.EventToolCall:
		if event.Tool != nil {
			m.appendSystem(fmt.Sprintf("[tool] %s (%s)", event.Tool.Title, event.Tool.Status))
			m.refreshViewport()
		}
		return m, waitForActivity(m.chat.Session)

	case acpdriver.EventToolEdit:
		if event.Tool != nil && event.Tool.Title != "" &&
			(event.Tool.Status == "completed" || event.Tool.Status == "failed") {
			m.appendSystem(fmt.Sprintf("[tool] %s: %s", event.Tool.Title, event.Tool.Status))
			m.refreshViewport()
		}
		return m, waitForActivity(m.chat.Session)

	case acpdriver.EventPlan:
		for _, entry := range event.Plan {
			m.appendSystem(fmt.Sprintf("[plan] %s: %s", entry.Status, entry.Content))
		}
		m.refreshViewport()
		return m, waitForActivity(m.chat.Session)

	case acpdriver.EventUsage:
		if event.Usage != nil {
			m.state = fmt.Sprintf("working · %d/%d tokens", event.Usage.Used, event.Usage.Size)
		}
		return m, waitForActivity(m.chat.Session)

	case acpdriver.EventTurn:
		m.closeAssistant()
		text := m.assistant.String()
		m.assistant.Reset()
		m.busy = false
		m.state = "ready"
		m.refreshViewport()

		cmds := []tea.Cmd{waitForActivity(m.chat.Session)}
		if strings.TrimSpace(text) != "" {
			cmds = append(cmds, saveAssistant(m.ctx, m.chat, text))
		}
		return m, tea.Batch(cmds...)

	case acpdriver.EventError:
		if event.Err != nil {
			m.appendError(event.Err.Error())
			m.refreshViewport()
		}
		m.busy = false
		m.state = "error"
		return m, waitForActivity(m.chat.Session)

	case acpdriver.EventExit:
		m.appendSystem("agent exited")
		m.busy = false
		m.state = "exited"
		m.refreshViewport()
		return m, nil
	}

	return m, waitForActivity(m.chat.Session)
}

func (m model) handlePermissionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.permission.Cancel()
		m.permission = nil
		m.state = "working"
		return m, waitForActivity(m.chat.Session)
	}

	index, err := strconv.Atoi(msg.String())
	if err != nil || index < 1 || index > len(m.permission.Options) {
		return m, nil
	}
	m.permission.Respond(m.permission.Options[index-1].ID)
	m.permission = nil
	m.state = "working"
	return m, waitForActivity(m.chat.Session)
}

func (m model) handleSlash(text string) (tea.Model, tea.Cmd) {
	command := strings.Fields(text)[0]
	switch command {
	case "/quit", "/exit":
		return m, tea.Quit
	case "/rules":
		if m.rules.Empty() {
			m.appendSystem("no rules loaded")
		} else {
			m.appendSystem(m.rules.Text())
		}
	case "/session":
		m.appendSystem(fmt.Sprintf("agent: %s\nmodel: %s\nsession: %s", m.chat.Agent, m.chat.Model, m.chat.Session.ID))
	case "/help":
		m.appendSystem("commands: /rules, /session, /help, /quit\nuse `switch chat --new` to start a new session")
	default:
		m.appendSystem("unknown command " + command + " (try /help)")
	}
	m.input.Reset()
	m.refreshViewport()
	return m, nil
}
