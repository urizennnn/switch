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

type doctorResultMsg struct {
	report    string
	available bool
	err       error
}

type newSessionResultMsg struct {
	chat   *chat.Chat
	status chat.Status
	err    error
}

type leadChangedMsg struct {
	agent  config.Agent
	chat   *chat.Chat
	status chat.Status
	err    error
}

type adapterActionMsg struct {
	name   string
	action string
	output string
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

func doctorCmd(project string) tea.Cmd {
	return func() tea.Msg {
		report, available, err := doctor.Report(project)
		return doctorResultMsg{report: report, available: available, err: err}
	}
}

func newSessionCmd(ctx context.Context, conversation *chat.Chat) tea.Cmd {
	return func() tea.Msg {
		next, status, err := conversation.NewSession(ctx)
		return newSessionResultMsg{chat: next, status: status, err: err}
	}
}

func adapterActionCmd(ctx context.Context, name, action string) tea.Cmd {
	return func() tea.Msg {
		actionCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		var output string
		var err error
		switch action {
		case "install", "install globally":
			output, err = acpdriver.Install(actionCtx, name)
		case "uninstall":
			output, err = acpdriver.Uninstall(actionCtx, name)
		default:
			err = fmt.Errorf("no action for %s", name)
		}
		return adapterActionMsg{name: name, action: action, output: output, err: err}
	}
}

func switchLeadCmd(ctx context.Context, conversation *chat.Chat, agent config.Agent) tea.Cmd {
	return func() tea.Msg {
		next, status, err := conversation.SwitchAgent(ctx, agent.Driver, agent.Model)
		return leadChangedMsg{agent: agent, chat: next, status: status, err: err}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.shutdown()
			return m, tea.Quit
		}
		if m.chat == nil {
			if msg.String() == "enter" && m.connectErr != nil {
				m.connectErr = nil
				m.connecting = true
				m.state = "connecting"
				return m, m.connectCmd()
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}

		if m.adapterPicker != nil {
			switch msg.String() {
			case "esc":
				if m.adapterPicker.pending != "" {
					m.adapterPicker.pending = ""
					m.adapterPicker.err = ""
				} else {
					m.adapterPicker = nil
				}
				return m, nil
			case "up":
				if m.adapterPicker.index > 0 {
					m.adapterPicker.index--
					m.adapterPicker.pending = ""
					m.adapterPicker.err = ""
				}
				return m, nil
			case "down":
				if m.adapterPicker.index < len(m.adapterPicker.choices)-1 {
					m.adapterPicker.index++
					m.adapterPicker.pending = ""
					m.adapterPicker.err = ""
				}
				return m, nil
			case "enter":
				choice := m.adapterPicker.selected()
				if !choice.Managed {
					m.adapterPicker.err = choice.Name + " is managed externally"
					return m, nil
				}
				if m.adapterPicker.pending != choice.Name {
					m.adapterPicker.pending = choice.Name
					m.adapterPicker.err = adapterConfirmText(choice)
					return m, nil
				}
				m.adapterPicker = nil
				m.busy = true
				m.state = "adapter " + choice.Action
				return m, adapterActionCmd(m.ctx, choice.Name, choice.Action)
			}
			return m, nil
		}

		if m.picker != nil {
			switch msg.String() {
			case "esc":
				m.picker = nil
				return m, nil
			case "up":
				if m.picker.index > 0 {
					m.picker.index--
				}
				return m, nil
			case "down":
				if m.picker.index < len(m.picker.choices)-1 {
					m.picker.index++
				}
				return m, nil
			case "enter":
				choice := m.picker.choices[m.picker.index]
				if !choice.Available {
					m.picker.err = choice.Label + " is not available"
					return m, nil
				}
				if choice.Current {
					m.picker.err = choice.Label + " is already the lead"
					return m, nil
				}
				m.picker = nil
				return m.beginLeadSwitch(choice.Agent)
			}
			return m, nil
		}

		if m.modal != "" {
			switch msg.String() {
			case "esc", "enter":
				m.modal = ""
				m.modalTitle = ""
				return m, nil
			}
			// Let the user type immediately; the first key dismisses the
			// result panel instead of trapping input behind it.
			m.modal = ""
			m.modalTitle = ""
		}

		if m.permission != nil {
			return m.handlePermissionKey(msg)
		}

		if m.paletteOpen {
			switch msg.String() {
			case "up":
				if m.paletteIndex > 0 {
					m.paletteIndex--
				}
				return m, nil
			case "down":
				if m.paletteIndex < len(m.palette)-1 {
					m.paletteIndex++
				}
				return m, nil
			case "tab":
				m.fillSelectedCommand()
				return m, nil
			case "esc":
				m.paletteOpen = false
				return m, nil
			case "enter":
				return m.handlePaletteEnter()
			}
		}

		switch msg.String() {
		case "esc":
			switch {
			case m.jobCancel != nil:
				m.jobCancel()
				m.state = "cancelling"
			case m.busy && m.state == "working":
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
			m.paletteOpen = false
			m.home = false
			m.appendUser(text)
			m.assistant.Reset()
			m.busy = true
			m.state = "working"
			m.refreshViewport()
			return m, sendPrompt(m.ctx, m.chat, text)
		}

		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		m.refreshPalette()
		return m, cmd

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		m.refreshViewport()
		return m, nil

	case connectedMsg:
		m.connecting = false
		if msg.err != nil {
			m.connectErr = msg.err
			m.state = "error"
			return m, nil
		}
		m.chat = msg.chat
		m.status = msg.status
		if m.project == "" {
			m.project = m.chat.Project
		}
		if m.lead.Driver == "" {
			m.lead.Driver = m.chat.Agent
		}
		if m.lead.Model == "" {
			m.lead.Model = m.chat.Model
		}
		m.state = "ready"
		m.home = true
		m.transcript = m.initialTranscript()
		m.refreshViewport()
		m.refreshDashboard()
		return m, waitForActivity(m.chat.Session, m.gen)

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
		m.permissionResume = waitForActivity(m.chat.Session, m.gen)
		m.state = "permission"
		return m, nil

	case jobPermissionMsg:
		m.permission = msg.request
		m.permissionResume = waitJob(m.jobStream, m.jobPermissions)
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

	case jobTextMsg:
		if m.job == "" {
			return m, nil
		}
		m.jobBuffer.WriteString(msg.text)
		return m, waitJob(m.jobStream, m.jobPermissions)

	case jobStreamClosedMsg:
		return m, nil

	case planResultMsg:
		m.endJob()
		if msg.err != nil {
			m.setModal("plan failed", msg.err.Error())
		} else if msg.result != nil {
			m.setModal("plan", planSummary(msg.result))
		}
		return m, nil

	case runResultMsg:
		m.endJob()
		m.setModal(runSummary(msg.result, msg.err))
		return m, nil

	case doctorResultMsg:
		m.busy = false
		m.state = "ready"
		if msg.err != nil {
			m.setModal("doctor", msg.err.Error())
		} else {
			m.setModal("doctor", msg.report)
			if !msg.available {
				m.state = "agent unavailable"
			}
		}
		return m, nil

	case newSessionResultMsg:
		m.busy = false
		m.state = "ready"
		if msg.err != nil {
			m.setModal("new session failed", msg.err.Error())
			return m, waitForActivity(m.chat.Session, m.gen)
		}
		m.chat = msg.chat
		m.status = msg.status
		m.refreshDashboard()
		m.setModal("new session", m.chat.Session.ID)
		return m, waitForActivity(m.chat.Session, m.gen)

	case adapterActionMsg:
		m.busy = false
		m.state = "ready"
		if msg.err != nil {
			m.setModal("adapter", msg.name+" "+msg.action+" failed\n\n"+msg.err.Error()+"\n"+msg.output)
		} else {
			m.setModal("adapter", msg.name+" "+msg.action+" complete\n\n"+msg.output)
		}
		return m, nil

	case leadChangedMsg:
		m.busy = false
		m.state = "ready"
		if msg.err != nil {
			if m.previousLead.Name != "" {
				m.lead = m.previousLead
				_ = m.persistLead(m.previousLead.Name)
			}
			m.setModal("lead switch failed", msg.err.Error())
			return m, waitForActivity(m.chat.Session, m.gen)
		}
		m.chat = msg.chat
		m.lead = msg.agent
		m.status = msg.status
		_ = m.persistLead(msg.agent.Name)
		m.refreshDashboard()
		m.setModal("lead", fmt.Sprintf("%s is now the lead (%s)", msg.agent.Name, msg.agent.Driver))
		return m, waitForActivity(m.chat.Session, m.gen)
	}

	return m, nil
}

// beginLeadSwitch shows the new lead optimistically and starts its session in
// the background; leadChangedMsg reverts on failure.
func (m model) beginLeadSwitch(agent config.Agent) (tea.Model, tea.Cmd) {
	m.previousLead = m.lead
	m.lead = agent
	if err := m.persistLead(agent.Name); err != nil {
		m.lead = m.previousLead
		m.setModal("lead", "could not save lead: "+err.Error())
		return m, nil
	}
	m.gen++
	m.busy = true
	m.state = "starting lead"
	return m, switchLeadCmd(m.ctx, m.chat, agent)
}

func (m *model) setModal(title, body string) {
	m.modalTitle = title
	m.modal = body
	m.paletteOpen = false
}

// persistLead saves the user's choice immediately, before the slow ACP
// connection starts. Otherwise quitting during a lead switch loses the choice.
func (m *model) persistLead(name string) error {
	if m.store == nil {
		return nil
	}
	project := m.project
	if project == "" && m.chat != nil {
		project = m.chat.Project
	}
	if project == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return m.store.SetLeadAgent(ctx, project, name)
}

func (m *model) refreshPalette() {
	matches := matchCommands(m.input.Value())
	wasOpen := m.paletteOpen
	m.palette = matches
	m.paletteOpen = len(matches) > 0
	if m.paletteOpen && !wasOpen {
		m.paletteIndex = 0
	}
	if m.paletteIndex >= len(matches) {
		m.paletteIndex = max(0, len(matches)-1)
	}
	if !m.paletteOpen {
		m.paletteIndex = 0
	}
}

func (m *model) fillSelectedCommand() {
	if len(m.palette) == 0 {
		return
	}
	command := m.palette[m.paletteIndex]
	if command.NeedsArg {
		m.input.SetValue(command.Name + " ")
	} else {
		m.input.SetValue(command.Name)
	}
	m.input.CursorEnd()
	m.paletteOpen = false
}

func (m model) handlePaletteEnter() (tea.Model, tea.Cmd) {
	typed := strings.TrimSpace(m.input.Value())
	if command, ok := findCommand(typed); ok {
		m.paletteOpen = false
		return m.handleSlash(command.Name)
	}
	if len(m.palette) == 0 {
		return m, nil
	}
	command := m.palette[m.paletteIndex]
	if command.NeedsArg {
		m.input.SetValue(command.Name + " ")
		m.input.CursorEnd()
		m.paletteOpen = false
		return m, nil
	}
	m.paletteOpen = false
	return m.handleSlash(command.Name)
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

func adapterConfirmText(choice adapterChoice) string {
	switch choice.Action {
	case "install globally":
		return "press enter again to install " + choice.Name + " globally"
	case "uninstall":
		return "press enter again to uninstall " + choice.Name
	default:
		return "press enter again to install " + choice.Name
	}
}

func adapterAvailable(name string) bool {
	for _, status := range acpdriver.AdapterStatuses() {
		if status.Name == name {
			return status.Available
		}
	}
	return false
}

func (m model) handlePermissionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		m.permission.Cancel()
	} else {
		index, err := strconv.Atoi(msg.String())
		if err != nil || index < 1 || index > len(m.permission.Options) {
			return m, nil
		}
		m.permission.Respond(m.permission.Options[index-1].ID)
	}
	resume := m.permissionResume
	m.permission = nil
	m.permissionResume = nil
	switch m.job {
	case "plan":
		m.state = "planning"
	case "run":
		m.state = "running"
	default:
		m.state = "working"
	}
	return m, resume
}

func (m model) handleSlash(text string) (tea.Model, tea.Cmd) {
	m.paletteOpen = false
	m.input.Reset()
	fields := strings.Fields(text)
	command := fields[0]
	switch command {
	case "/plan", "/run", "/doctor", "/lead", "/adapters", "/new":
		if m.busy {
			m.setModal("busy", "wait for the current turn to finish")
			return m, nil
		}
	}
	switch command {
	case "/quit", "/exit":
		m.shutdown()
		return m, tea.Quit

	case "/home":
		m.home = true
		m.modal = ""
		m.modalTitle = ""
		m.refreshDashboard()

	case "/rules":
		if m.rules.Empty() {
			m.setModal("rules", "no rules loaded")
		} else {
			m.setModal("rules", m.rules.Text())
		}

	case "/session":
		m.setModal("session", fmt.Sprintf("lead: %s\ndriver: %s\nmodel: %s\nsession: %s", m.lead.Name, m.chat.Agent, m.chat.Model, m.chat.Session.ID))

	case "/plan":
		if len(fields) < 2 {
			m.setModal("plan", "usage: /plan <goal>  or  /plan --file <path>")
			break
		}
		var goal, file string
		if fields[1] == "--file" {
			if len(fields) < 3 {
				m.setModal("plan", "usage: /plan --file <path>")
				break
			}
			file, _ = filepath.Abs(fields[2])
		} else {
			goal = strings.TrimSpace(strings.TrimPrefix(text, "/plan"))
		}
		ctx, stream, permissions := m.startJob("plan", "planning")
		return m, tea.Batch(
			startPlanCmd(ctx, m.chat.Project, goal, file, m.lead, stream),
			waitJob(stream, permissions),
		)

	case "/run":
		if m.run == nil {
			m.setModal("run", "no run yet\n\n/plan <goal> or /plan --file <path> first")
			break
		}
		ctx, stream, permissions := m.startJob("run", "running")
		return m, tea.Batch(
			runTaskCmd(ctx, m.store, m.run.ID, m.rules, stream, permissions),
			waitJob(stream, permissions),
		)

	case "/doctor":
		m.busy = true
		m.state = "checking"
		return m, doctorCmd(m.chat.Project)

	case "/init":
		created, err := config.EnsureProject(m.chat.Project)
		if err != nil {
			m.setModal("init failed", err.Error())
		} else if len(created) == 0 {
			m.setModal("init", "project is already initialized")
		} else {
			m.setModal("init", "created:\n  "+strings.Join(created, "\n  "))
			m.refreshDashboard()
		}

	case "/lead":
		if len(fields) < 2 {
			m.picker = buildLeadPicker(m)
			break
		}
		agent, ok := m.fleetAgent(fields[1])
		switch {
		case !ok:
			m.setModal("lead", "unknown agent "+fields[1]+"\n\nuse /lead to list configured agents")
		case !adapterAvailable(agent.Driver):
			m.setModal("lead", agent.Name+" uses unavailable adapter "+agent.Driver)
		case agent.Name == m.lead.Name:
			m.setModal("lead", agent.Name+" is already the lead")
		default:
			return m.beginLeadSwitch(agent)
		}

	case "/adapters":
		m.adapterPicker = buildAdapterPicker()
		m.picker = nil
		m.modal = ""
		m.modalTitle = ""

	case "/new":
		m.gen++
		m.busy = true
		m.state = "new session"
		return m, newSessionCmd(m.ctx, m.chat)

	case "/help":
		m.setModal("help", helpText())

	default:
		m.setModal("unknown command", command+"\ntry /help")
	}

	return m, nil
}

func planSummary(result *planning.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "lead %s (%s)\n", result.Lead.Name, result.Lead.Driver)
	fmt.Fprintf(&b, "run %s planned\n", shortID(result.Run.ID))
	fmt.Fprintf(&b, "integration: %s\n", result.Run.IntegrationBranch)
	names := make([]string, 0, len(result.Agents))
	for _, item := range result.Agents {
		names = append(names, item.Agent.Name)
	}
	fmt.Fprintf(&b, "agents: %s\n", strings.Join(names, ", "))
	b.WriteString("tasks:\n")
	for _, task := range result.Plan.Tasks {
		deps := "none"
		if len(task.Deps) > 0 {
			deps = strings.Join(task.Deps, ",")
		}
		fmt.Fprintf(&b, "  %-12s %-8s deps=%-12s %s\n", task.ID, task.Role, deps, task.Title)
	}
	fmt.Fprintf(&b, "worktrees: %s", truncate(result.Root, 56))
	return b.String()
}
