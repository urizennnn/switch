package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/doctor"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/planning"
)

type agentEventMsg struct {
	gen   int
	event acpdriver.Event
}

type permissionMsg struct {
	gen     int
	request *acpdriver.PermissionRequest
}

type turnResultMsg struct {
	err error
}

type saveResultMsg struct {
	err error
}

type planResultMsg struct {
	result *planning.Result
	err    error
}

type doctorResultMsg struct {
	report    string
	available bool
	err       error
}

type newSessionResultMsg struct {
	status chat.Status
	err    error
}

func waitForActivity(session *acpdriver.Session, gen int) tea.Cmd {
	return func() tea.Msg {
		select {
		case event, ok := <-session.Events:
			if !ok {
				return agentEventMsg{gen: gen, event: acpdriver.Event{Type: acpdriver.EventExit}}
			}
			return agentEventMsg{gen: gen, event: event}
		case request := <-session.Permissions():
			return permissionMsg{gen: gen, request: request}
		case <-session.Done():
			return agentEventMsg{gen: gen, event: acpdriver.Event{Type: acpdriver.EventExit}}
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

func planCmd(ctx context.Context, project, goal, file string) tea.Cmd {
	return func() tea.Msg {
		planCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		result, err := planning.Execute(planCtx, planning.Options{
			Project:    project,
			Goal:       goal,
			File:       file,
			Permission: acpdriver.PermissionDeny,
		})
		return planResultMsg{result: result, err: err}
	}
}

func doctorCmd(project string) tea.Cmd {
	return func() tea.Msg {
		report, available, err := doctor.Report(project)
		return doctorResultMsg{report: report, available: available, err: err}
	}
}

func newSessionCmd(ctx context.Context, conversation *chat.Chat) tea.Cmd {
	return func() tea.Msg {
		status, err := conversation.NewSession(ctx)
		return newSessionResultMsg{status: status, err: err}
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
			if m.busy && m.state == "working" {
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
		if msg.gen != m.gen {
			return m, nil
		}
		return m.handleAgentEvent(msg.event)

	case permissionMsg:
		if msg.gen != m.gen {
			return m, nil
		}
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

	case planResultMsg:
		m.busy = false
		m.state = "ready"
		if msg.err != nil {
			m.appendError("plan: " + msg.err.Error())
		} else if msg.result != nil {
			m.appendSystem(planSummary(msg.result))
		}
		m.refreshViewport()
		return m, nil

	case doctorResultMsg:
		m.busy = false
		m.state = "ready"
		if msg.err != nil {
			m.appendError("doctor: " + msg.err.Error())
		} else {
			m.appendSystem(msg.report)
			if !msg.available {
				m.state = "agent unavailable"
			}
		}
		m.refreshViewport()
		return m, nil

	case newSessionResultMsg:
		m.busy = false
		m.state = "ready"
		if msg.err != nil {
			m.appendError("new session: " + msg.err.Error())
			m.refreshViewport()
			return m, waitForActivity(m.chat.Session, m.gen)
		}
		m.status = msg.status
		m.status.Resumed = false
		m.appendSystem("new session: " + m.chat.Session.ID)
		m.refreshViewport()
		return m, waitForActivity(m.chat.Session, m.gen)
	}

	return m, nil
}

func (m model) handleAgentEvent(event acpdriver.Event) (tea.Model, tea.Cmd) {
	switch event.Type {
	case acpdriver.EventMessage:
		m.appendAssistantChunk(event.Text)
		m.refreshViewport()
		return m, waitForActivity(m.chat.Session, m.gen)

	case acpdriver.EventThought:
		return m, waitForActivity(m.chat.Session, m.gen)

	case acpdriver.EventToolCall:
		if event.Tool != nil {
			m.appendSystem(fmt.Sprintf("[tool] %s (%s)", event.Tool.Title, event.Tool.Status))
			m.refreshViewport()
		}
		return m, waitForActivity(m.chat.Session, m.gen)

	case acpdriver.EventToolEdit:
		if event.Tool != nil && event.Tool.Title != "" &&
			(event.Tool.Status == "completed" || event.Tool.Status == "failed") {
			m.appendSystem(fmt.Sprintf("[tool] %s: %s", event.Tool.Title, event.Tool.Status))
			m.refreshViewport()
		}
		return m, waitForActivity(m.chat.Session, m.gen)

	case acpdriver.EventPlan:
		for _, entry := range event.Plan {
			m.appendSystem(fmt.Sprintf("[plan] %s: %s", entry.Status, entry.Content))
		}
		m.refreshViewport()
		return m, waitForActivity(m.chat.Session, m.gen)

	case acpdriver.EventUsage:
		if event.Usage != nil {
			m.state = fmt.Sprintf("working · %d/%d tokens", event.Usage.Used, event.Usage.Size)
		}
		return m, waitForActivity(m.chat.Session, m.gen)

	case acpdriver.EventTurn:
		m.closeAssistant()
		text := m.assistant.String()
		m.assistant.Reset()
		m.busy = false
		m.state = "ready"
		m.refreshViewport()

		cmds := []tea.Cmd{waitForActivity(m.chat.Session, m.gen)}
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
		return m, waitForActivity(m.chat.Session, m.gen)

	case acpdriver.EventExit:
		m.appendSystem("agent exited")
		m.busy = false
		m.state = "exited"
		m.refreshViewport()
		return m, nil
	}

	return m, waitForActivity(m.chat.Session, m.gen)
}

func (m model) handlePermissionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.permission.Cancel()
		m.permission = nil
		m.state = "working"
		return m, waitForActivity(m.chat.Session, m.gen)
	}

	index, err := strconv.Atoi(msg.String())
	if err != nil || index < 1 || index > len(m.permission.Options) {
		return m, nil
	}
	m.permission.Respond(m.permission.Options[index-1].ID)
	m.permission = nil
	m.state = "working"
	return m, waitForActivity(m.chat.Session, m.gen)
}

func (m model) handleSlash(text string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(text)
	command := fields[0]
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

	case "/plan":
		if m.busy {
			m.appendSystem("busy; wait for the current turn to finish")
			break
		}
		if len(fields) < 2 {
			m.appendSystem("usage: /plan <goal>  or  /plan --file <path>")
			break
		}
		var goal, file string
		if fields[1] == "--file" {
			if len(fields) < 3 {
				m.appendSystem("usage: /plan --file <path>")
				break
			}
			file, _ = filepath.Abs(fields[2])
		} else {
			goal = strings.TrimSpace(strings.TrimPrefix(text, "/plan"))
		}
		m.input.Reset()
		m.busy = true
		m.state = "planning"
		m.appendSystem("lead is planning…")
		m.refreshViewport()
		return m, planCmd(m.ctx, m.chat.Project, goal, file)

	case "/doctor":
		if m.busy {
			m.appendSystem("busy; wait for the current turn to finish")
			break
		}
		m.input.Reset()
		m.busy = true
		m.state = "checking"
		return m, doctorCmd(m.chat.Project)

	case "/init":
		created, err := config.EnsureProject(m.chat.Project)
		if err != nil {
			m.appendError("init: " + err.Error())
		} else if len(created) == 0 {
			m.appendSystem("project is already initialized")
		} else {
			m.appendSystem("initialized:\n  " + strings.Join(created, "\n  "))
		}

	case "/new":
		if m.busy {
			m.appendSystem("busy; wait for the current turn to finish")
			break
		}
		m.input.Reset()
		m.gen++
		m.busy = true
		m.state = "new session"
		m.appendSystem("starting a new session…")
		m.refreshViewport()
		return m, newSessionCmd(m.ctx, m.chat)

	case "/help":
		m.appendSystem(`commands:
  /plan <goal> - ask the lead to plan and create a run
  /plan --file <path> - import a plan JSON file
  /doctor - check config, rules, adapters, and state
  /init - create .switch config and rules
  /rules - show effective rules
  /session - show the current agent session
  /new - start a new agent session
  /quit - exit`)

	default:
		m.appendSystem("unknown command " + command + " (try /help)")
	}

	m.input.Reset()
	m.refreshViewport()
	return m, nil
}

func planSummary(result *planning.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run %s planned\n", shortID(result.Run.ID))
	fmt.Fprintf(&b, "integration: %s\n", result.Run.IntegrationBranch)
	b.WriteString("agents:\n")
	for _, item := range result.Agents {
		fmt.Fprintf(&b, "  %-10s %-8s %s\n", item.Agent.Name, item.Agent.Role, item.Worktree.Path)
	}
	b.WriteString("tasks:\n")
	for _, task := range result.Plan.Tasks {
		deps := "none"
		if len(task.Deps) > 0 {
			deps = strings.Join(task.Deps, ",")
		}
		fmt.Fprintf(&b, "  %-12s %-8s deps=%-12s %s\n", task.ID, task.Role, deps, task.Title)
	}
	fmt.Fprintf(&b, "worktrees: %s", result.Root)
	return b.String()
}
