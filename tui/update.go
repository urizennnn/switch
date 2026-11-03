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
	"github.com/urizennnn/switch/internal/execution"
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
	agent    config.Agent
	chat     *chat.Chat
	status   chat.Status
	err      error
	previous *acpdriver.Session // the outgoing lead's session, kept warm
	adopted  *acpdriver.Session // the warm session used, if any
}

type adapterActionMsg struct {
	name   string
	action string
	output string
	err    error
}

// waitForActivity waits for the lead session's next event. stop closes when
// the lead changes, so a listener left on a parked session exits instead of
// taking that session's events later.
func waitForActivity(session *acpdriver.Session, gen int, stop <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-stop:
			return nil
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

// switchLeadCmd makes agent the lead, adopting its warm session when there is
// one. The outgoing lead's session stays open so it can be kept warm.
func switchLeadCmd(ctx context.Context, conversation *chat.Chat, agent config.Agent, warm *acpdriver.Session) tea.Cmd {
	return func() tea.Msg {
		msg := leadChangedMsg{agent: agent, previous: conversation.Session, adopted: warm}
		if warm != nil {
			msg.chat, msg.status, msg.err = conversation.Adopt(ctx, agent.Name, warm)
		} else {
			msg.chat, msg.status, msg.err = conversation.Connect(ctx, agent.Name, agent.Driver, agent.Model)
		}
		return msg
	}
}

type warmedMsg struct {
	name    string
	session *acpdriver.Session
	err     error
}

// warmCmds starts the team's other agents in the background.
func (m model) warmCmds() []tea.Cmd {
	var cmds []tea.Cmd
	for _, agent := range m.team.toWarm() {
		cmds = append(cmds, func() tea.Msg {
			session, err := chat.StartSession(m.ctx, m.project, agent.Name, agent.Driver, agent.Model, m.team.grant)
			return warmedMsg{name: agent.Name, session: session, err: err}
		})
	}
	return cmds
}

// listen waits for the current lead session's next event.
func (m model) listen() tea.Cmd {
	return waitForActivity(m.chat.Session, m.gen, m.listenStop)
}

// bumpGen starts a new lead generation: events from the old session are
// ignored and its listener stops.
func (m *model) bumpGen() {
	m.gen++
	if m.listenStop != nil {
		close(m.listenStop)
	}
	m.listenStop = make(chan struct{})
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.shutdown()
			return m, tea.Quit
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
				// Installs run in the background; the chat stays usable.
				m.adapterPicker = nil
				m.installing = choice.Name + " " + choice.Action
				return m, adapterActionCmd(m.ctx, choice.Name, choice.Action)
			}
			return m, nil
		}

		if msg.String() == "ctrl+o" {
			m.toggleZen()
			return m, nil
		}

		if m.rulesReview != nil {
			return m.handleRulesReviewKey(msg)
		}

		if m.rulesManager != nil {
			return m.handleRulesManagerKey(msg)
		}

		if m.sessionPicker != nil {
			return m.handleSessionPickerKey(msg)
		}

		if m.taskPicker != nil {
			return m.handleTaskPickerKey(msg)
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
			case m.busy && m.state == "working":
				_ = m.chat.Session.Cancel(context.Background())
				m.state = "cancelling"
			case m.job == "plan":
				// A run is long-lived; only /stop ends it, so a stray esc cannot.
				m.jobCancel()
				m.state = "cancelling"
			}
			return m, nil

		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				if m.chat == nil && m.connectErr != nil {
					cmd := m.reconnect()
					return m, cmd
				}
				return m, nil
			}
			if m.busy {
				return m, nil
			}
			if strings.HasPrefix(text, "/") {
				return m.handleSlash(text)
			}
			m.input.Reset()
			m.paletteOpen = false
			m.home = false
			m.appendUser(text)
			if m.chat == nil {
				// Still connecting: keep the message and send it once the lead is up.
				m.queued = strings.TrimSpace(m.queued + "\n\n" + text)
				m.refreshViewport()
				if m.connectErr != nil {
					cmd := m.reconnect()
					return m, cmd
				}
				return m, nil
			}
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
			if !m.home {
				m.appendError("could not start " + m.lead.Name + ": " + firstLine(msg.err.Error()) + "\npress enter to retry")
				m.refreshViewport()
			}
			return m, nil
		}
		m.chat = msg.chat
		m.chat.Rules = m.rules // the user may have declined project rules while it connected
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
		m.loadHistory()
		m.refreshDashboard()
		if m.queued != "" {
			text := m.queued
			m.queued = ""
			m.appendUser(text)
			m.busy = true
			m.state = "working"
			m.refreshViewport()
			return m, tea.Batch(append(m.warmCmds(), m.listen(), sendPrompt(m.ctx, m.chat, text))...)
		}
		m.refreshViewport()
		// The lead is up; start the rest of the team in the background.
		return m, tea.Batch(append(m.warmCmds(), m.listen())...)

	case editedMsg:
		return m.handleEdited(msg)

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
		m.permissionResume = m.listen()
		m.state = "permission"
		return m, nil

	case jobPermissionMsg:
		m.permission = msg.request
		m.permissionResume = m.waitJob()
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
		m.appendJobText(msg.agent, msg.text)
		return m, m.waitJob()

	case resumedMsg:
		return m.handleResumed(msg)

	case warmedMsg:
		if msg.err != nil {
			m.team.warmFailed(msg.name)
			return m, nil
		}
		if m.ctx.Err() != nil {
			go msg.session.Close(context.Background())
			return m, nil
		}
		m.team.park(msg.name, msg.session)
		return m, nil

	case dashboardTickMsg:
		if m.job == "" {
			m.refreshDashboard()
		}
		return m, dashboardTick()

	case detachMsg:
		m.refreshDashboard()
		m.setModal(msg.title, msg.body)
		return m, nil

	case jobChangedMsg:
		m.refreshDashboard()
		return m, m.waitJob()

	case taskActionMsg:
		m.refreshDashboard()
		m.setModal(msg.title, msg.body)
		return m, nil

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
		m.setModal("run "+msg.outcome, runOutcomeText(msg.outcome, msg.err))
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
			return m, m.listen()
		}
		m.chat = msg.chat
		m.status = msg.status
		m.loadHistory() // a new session starts with an empty transcript
		m.refreshViewport()
		m.refreshDashboard()
		m.setModal("new session", m.chat.Session.ID)
		return m, m.listen()

	case adapterActionMsg:
		m.installing = ""
		if msg.err != nil {
			m.setModal("adapter", msg.name+" "+msg.action+" failed\n\n"+msg.err.Error()+"\n"+msg.output)
			return m, nil
		}
		m.setModal("adapter", msg.name+" "+msg.action+" complete\n\n"+msg.output)
		if m.chat == nil {
			return m, nil
		}
		// A newly available adapter can join the warm team at once.
		return m, tea.Batch(m.warmCmds()...)

	case leadChangedMsg:
		m.busy = false
		m.state = "ready"
		if msg.err != nil {
			if m.previousLead.Name != "" {
				m.lead = m.previousLead
				_ = m.persistLead(m.previousLead.Name)
			}
			m.team.park(msg.agent.Name, msg.adopted)
			m.setModal("lead switch failed", msg.err.Error())
			return m, m.listen()
		}
		m.chat = msg.chat
		m.lead = msg.agent
		m.status = msg.status
		_ = m.persistLead(msg.agent.Name)
		m.team.setLead(msg.agent.Name)
		m.team.park(m.previousLead.Name, msg.previous) // instant to switch back
		// The new lead has its own session, so its transcript starts fresh.
		note := fmt.Sprintf("%s (%s) is now the lead", msg.agent.Name, msg.agent.Driver)
		_ = m.chat.Note(m.ctx, note)
		m.loadHistory()
		m.refreshViewport()
		m.refreshDashboard()
		m.setModal("lead", fmt.Sprintf("%s is now the lead (%s)", msg.agent.Name, msg.agent.Driver))
		return m, m.listen()
	}

	return m, nil
}

// beginLeadSwitch shows the new lead optimistically and starts its session in
// the background; leadChangedMsg reverts on failure.
func (m model) beginLeadSwitch(agent config.Agent) (tea.Model, tea.Cmd) {
	if m.chat == nil {
		m.setModal("lead", m.lead.Name+" is still connecting; switch once it is ready")
		return m, nil
	}
	m.previousLead = m.lead
	m.lead = agent
	if err := m.persistLead(agent.Name); err != nil {
		m.lead = m.previousLead
		m.setModal("lead", "could not save lead: "+err.Error())
		return m, nil
	}
	m.bumpGen()
	m.busy = true
	m.state = "starting lead"
	return m, switchLeadCmd(m.ctx, m.chat, agent, m.team.take(agent.Name))
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
		return m, m.listen()

	case acpdriver.EventThought:
		return m, m.listen()

	case acpdriver.EventToolCall, acpdriver.EventToolEdit:
		if event.Tool != nil {
			m.appendTool(event.Tool.ID, event.Tool.Title, event.Tool.Status)
			m.refreshViewport()
		}
		return m, m.listen()

	case acpdriver.EventPlan:
		items := make([]string, 0, len(event.Plan))
		for _, entry := range event.Plan {
			mark := "·"
			switch entry.Status {
			case "completed":
				mark = "✓"
			case "in_progress":
				mark = "›"
			}
			items = append(items, mark+" "+entry.Content)
		}
		m.setPlan(items)
		m.refreshViewport()
		return m, m.listen()

	case acpdriver.EventUsage:
		if event.Usage != nil {
			m.state = fmt.Sprintf("working · %d/%d tokens", event.Usage.Used, event.Usage.Size)
		}
		return m, m.listen()

	case acpdriver.EventTurn:
		text := m.closeAssistant()
		m.busy = false
		m.state = "ready"
		m.refreshViewport()

		cmds := []tea.Cmd{m.listen()}
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
		return m, m.listen()

	case acpdriver.EventExit:
		m.appendSystem("agent exited")
		m.busy = false
		m.state = "exited"
		m.refreshViewport()
		return m, nil
	}

	return m, m.listen()
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
	switch {
	case m.job == "plan":
		m.state = "planning"
	case m.job == "run" && !m.busy:
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
	case "/plan", "/run", "/doctor", "/lead", "/adapters", "/new", "/resume":
		if m.busy {
			m.setModal("busy", "wait for the current turn to finish")
			return m, nil
		}
	}
	switch command {
	case "/session", "/new", "/resume":
		if m.chat == nil {
			m.setModal(command[1:], m.lead.Name+" is still connecting; try again in a moment")
			return m, nil
		}
	case "/plan", "/run", "/adapters":
		// Never plan, start a second run, or install adapters mid-run.
		if m.job == "run" {
			m.setModal("run in progress", "/stop ends the run first")
			return m, nil
		}
	case "/tasks", "/review", "/agents", "/log", "/approve", "/reject", "/retry", "/diff":
		if m.run == nil {
			m.setModal("no run", "/plan <goal> or /plan --file <path> creates one")
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
		if len(fields) > 1 {
			m.previewRules(fields[1])
			break
		}
		return m.openRulesManager()

	case "/session":
		info := fmt.Sprintf("lead: %s\ndriver: %s\nmodel: %s\nsession: %s (%s)", m.lead.Name, m.chat.Agent, m.chat.Model, m.chat.Session.ID, sessionState(m.status.Resumed))
		if m.status.ResumeError != nil {
			info += "\n\nthe previous session could not be resumed:\n" + m.status.ResumeError.Error()
		}
		m.setModal("session", info)

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
		ctx := m.startJob("plan", "planning", true)
		return m, tea.Batch(
			startPlanCmd(ctx, m.project, goal, file, m.lead, m.rules, m.jobStream),
			m.waitJob(),
		)

	case "/run":
		if m.run == nil {
			m.setModal("run", "no run yet\n\n/plan <goal> or /plan --file <path> first")
			break
		}
		if m.runOwner > 0 {
			m.setModal("run in progress", fmt.Sprintf("pid %d is driving this run in the background\n\n/tasks and /review approve its work · /stop stops it", m.runOwner))
			break
		}
		if len(fields) > 1 && fields[1] == "--detach" {
			permission := "deny"
			if len(fields) > 2 && fields[2] == "--allow" {
				permission = "allow"
			}
			return m, detachCmd(m.project, m.run.ID, permission)
		}
		ctx := m.startJob("run", "running", false)
		engine, server, err := m.startRun(ctx)
		if err != nil {
			m.endJob()
			m.setModal("run failed", err.Error())
			break
		}
		m.engine = engine
		m.home = false
		return m, tea.Batch(runEngineCmd(ctx, engine, server, m.jobStream), m.waitJob())

	case "/stop":
		switch {
		case m.job == "run":
			m.jobCancel()
			m.state = "cancelling"
		case m.runOwner > 0 && m.run != nil:
			return m, stopDetachedCmd(m.run.ID)
		default:
			m.setModal("stop", "no run in progress")
		}

	case "/tasks", "/review":
		m.taskPicker = &taskPicker{}
		if command == "/review" {
			for i, task := range m.tasks {
				if task.Status == execution.StatusReview {
					m.taskPicker.index = i
					break
				}
			}
		}

	case "/resume":
		return m.openSessionPicker()

	case "/zen":
		m.toggleZen()

	case "/agents":
		m.setModal("agents", m.agentsContent())

	case "/log":
		m.setModal("run log", m.logContent())

	case "/approve", "/retry", "/diff":
		if len(fields) < 2 {
			m.setModal(command[1:], "usage: "+command+" <task>")
			break
		}
		task, ok := m.findTask(fields[1])
		if !ok {
			m.setModal(command[1:], "unknown task "+fields[1]+"\n/tasks lists them")
			break
		}
		return m, m.taskAction(command[1:], task.ID, "")

	case "/reject":
		if len(fields) < 2 {
			m.setModal("reject", "usage: /reject <task> <reason>")
			break
		}
		task, ok := m.findTask(fields[1])
		if !ok {
			m.setModal("reject", "unknown task "+fields[1]+"\n/tasks lists them")
			break
		}
		reason := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(text, "/reject")), fields[1]))
		return m, m.taskAction("reject", task.ID, reason)

	case "/doctor":
		m.busy = true
		m.state = "checking"
		return m, doctorCmd(m.project)

	case "/init":
		created, err := config.EnsureProject(m.project)
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
		if m.installing != "" {
			m.setModal("adapters", m.installing+" is still running; wait for it to finish")
			break
		}
		m.adapterPicker = buildAdapterPicker()
		m.picker = nil
		m.modal = ""
		m.modalTitle = ""

	case "/new":
		m.bumpGen()
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
	for _, agent := range result.Agents {
		names = append(names, agent.Name)
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

// reconnect retries starting the lead after a failed connection.
func (m *model) reconnect() tea.Cmd {
	m.connectErr = nil
	m.connecting = true
	m.state = "connecting"
	return m.connectCmd()
}

// toggleZen hides or shows the sidebar so the conversation gets the full width.
func (m *model) toggleZen() {
	m.zen = !m.zen
	m.layout()
	m.refreshViewport()
}
