package tui

import (
	"context"
	"errors"
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
	"github.com/urizennnn/switch/internal/policy"
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
	report string
	err    error
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
			select {
			case event := <-session.Events: // sent before it exited, such as how it exited
				return agentEventMsg{gen: gen, event: event}
			default:
			}
			return agentEventMsg{gen: gen, event: acpdriver.Event{Type: acpdriver.EventExit}}
		}
	}
}

// planModeNote leads each message in plan mode. Send stores it with the
// message, so loadHistory takes it off again.
const planModeNote = "[Switch plan mode: investigate and plan only. Do not edit files, run mutating commands, or change external state.]\n\n"

func sendPrompt(ctx context.Context, conversation *chat.Chat, text string, mode policy.Mode) tea.Cmd {
	return func() tea.Msg {
		if mode == policy.Plan {
			text = planModeNote + text
		}
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
		report, _, err := doctor.Report(project) // the report names what does not work
		return doctorResultMsg{report: report, err: err}
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
	m.reply = ""
	if m.listenStop != nil {
		close(m.listenStop)
	}
	m.listenStop = make(chan struct{})
}

// Update sends queued messages once the lead is free, starts the status
// line's timer when a turn begins, keeps its clock ticking while anything
// runs, and pings a user who is away once Switch needs them.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	nm, ok := next.(model)
	if !ok {
		return next, cmd
	}
	started := nm.busy && !m.busy
	nm.team.setRules(nm.rules) // teammates get what the lead gets, whatever changed it
	// Send writes down the rules it gave as it returns, after the turn's last
	// event, so the next Send waits for that too.
	if !nm.busy && !nm.sending && nm.queued != "" && nm.chat != nil {
		text := nm.queued
		nm.queued, nm.limited = "", nil // a limit may have lifted
		nm.appendUser(text)
		nm.busy, nm.sending, nm.state, started = true, true, "working", true
		nm.refreshViewport()
		cmd = tea.Batch(cmd, sendPrompt(nm.ctx, nm.chat, text, nm.permissionMode))
	}
	if started {
		nm.turnStarted, nm.step = time.Now(), ""
	}
	if nm.timing() && !nm.clockRunning {
		nm.clockRunning = true
		cmd = tea.Batch(cmd, clockTick())
	}
	return nm, tea.Batch(cmd, nm.ping(m))
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		if m.permission != nil || m.paletteOpen || m.taskPicker != nil || m.sessionPicker != nil || m.picker != nil || m.adapterPicker != nil || m.rulesManager != nil {
			return m, nil
		}
		switch msg.Button {
		case tea.MouseWheelUp:
			m.scroll(-3)
		case tea.MouseWheelDown:
			m.scroll(3)
		}
		return m, nil
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.shutdown()
			return m, tea.Quit
		}
		// The prompt is drawn over every panel, so it takes the keys.
		if m.permission != nil {
			return m.handlePermissionKey(msg)
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

		if delta := m.scrollKey(msg.String()); delta != 0 {
			m.scroll(delta)
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
				if m.team.status(choice.Agent.Name) == "answering" {
					m.picker.err = answeringText(choice.Label)
					return m, nil
				}
				if m.busy || m.sending { // a message queued at launch can start a turn while the picker is open
					m.picker.err = "wait for the current turn to finish"
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
				land := m.modalTitle == landTitle
				m.modal = ""
				m.modalTitle = ""
				switch {
				case land && (msg.String() == "esc" || m.input.Value() != ""):
					m.landing = landMsg{} // a no, or an enter meant for the input: the next /land shows it again
				case land && m.input.Value() == "":
					// /land's checks again: the lead may have started a turn
					// since. The panel opens when git answers, maybe as the
					// user types, and an enter meant for that text is not a yes.
					return m.handleSlash("/land")
				}
				return m, nil
			}
			// Let the user type immediately; the first key dismisses the
			// result panel instead of trapping input behind it. A land panel
			// closed that way may never have been read, so it is not a yes.
			if m.modalTitle == landTitle {
				m.landing = landMsg{}
			}
			m.modal = ""
			m.modalTitle = ""
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

		// Following a worker or subagent: tab moves between agents, esc goes
		// back up, and a worker's subagent cards can be opened.
		switch key := msg.String(); {
		case key == "ctrl+e":
			m.showCommands = !m.showCommands
			m.refreshViewport()
			return m, nil
		case key == "ctrl+t":
			m.showThoughts = !m.showThoughts
			m.refreshViewport()
			return m, nil
		case key == "ctrl+x":
			return m.stopFocused()
		case key == "tab" || key == "shift+tab":
			if key == "tab" && m.completeMention() {
				return m, nil
			}
			step := 1
			if key == "shift+tab" {
				step = -1
			}
			m.cycleFocus(step)
			return m, nil
		case m.focus != "" && key == "esc":
			m.focusUp()
			return m, nil
		case m.focus != "" && m.input.Value() == "" && (key == "left" || key == "right" || key == "enter"):
			if _, isSub := m.focusedSubagent(); !isSub {
				if subs := m.subagentsOf(m.focus); len(subs) > 0 {
					switch key {
					case "left":
						m.card = max(0, m.card-1)
					case "right":
						m.card = min(len(subs)-1, m.card+1)
					default:
						m.setFocus(m.focus + "/" + subs[min(m.card, len(subs)-1)].Name)
					}
					return m, nil
				}
			}
		}

		switch msg.String() {
		case "esc":
			m.unqueue()
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
			if strings.HasPrefix(text, "/") {
				return m.handleSlash(text) // it refuses, with a notice, what must wait for the lead
			}
			if to, question, ok := m.mention(text); ok {
				return m.askTeammate(to, question)
			}
			m.input.Reset()
			m.paletteOpen = false
			m.home = false
			m.setFocus("") // the message goes to the lead, so show its chat
			// Update sends it as soon as the lead is connected and between turns.
			m.queued = strings.TrimSpace(m.queued + "\n\n" + text)
			m.refreshViewport()
			if m.chat == nil && m.connectErr != nil {
				cmd := m.reconnect()
				return m, cmd
			}
			return m, nil
		}
		return m, m.updateInput(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		m.refreshViewport()
		return m, nil

	case tea.FocusMsg:
		m.blurred = false
		return m, nil

	case tea.BlurMsg:
		m.blurred = true
		return m, nil

	case askedMsg:
		if msg.gen != m.gen { // asked in a conversation the user has since left
			if msg.err != nil {
				m.fail("ask "+msg.to, msg.err.Error())
			} else {
				m.notify(msg.to+" answered", "/agent "+msg.to+" shows it")
			}
			return m, nil
		}
		if msg.err != nil {
			addAside(&m.entries, entry{kind: entryError, text: msg.err.Error()})
		} else {
			addAside(&m.entries, entry{kind: entryAssistant, who: msg.to, text: msg.reply})
		}
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
		m.reportResume(msg.status)
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
		m.askPermission(msg.request, m.permissionMode, false)
		return m, m.listen()

	case jobPermissionMsg:
		mode := m.permissionMode
		if strings.Contains(msg.request.Agent, "/") {
			mode = m.helperMode() // a worker's subagent
		}
		m.askPermission(msg.request, mode, true)
		return m, m.waitJob()

	case turnResultMsg:
		m.sending = false
		if msg.err == nil {
			// EventTurn, which Prompt queues behind the reply's last update,
			// ends the turn; what waited for Send to return can go now.
			return m.afterTurn()
		}
		if m.state != "error" && m.state != "exited" { // the exit said why
			m.appendError(msg.err.Error())
			m.state = "error"
		}
		if reason := acpdriver.LimitReason("", msg.err); reason != "" && m.pendingHandoff == nil {
			m.limitHandoff(reason)
		}
		m.busy = false
		m.unqueue()
		m.refreshViewport()
		return m.afterTurn()

	case handoffMsg:
		m.pendingHandoff = &msg
		next, cmd := m.afterTurn() // at once when the lead is idle
		return next, tea.Batch(cmd, waitHandoff(m.handoffs))

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
		m.team.resetUsage(msg.name) // what it used belonged to a session this one replaces
		m.team.park(msg.name, msg.session)
		return m, nil

	case dashboardTickMsg:
		m.pruneToasts()
		m.reviewRules() // project rules changed outside Switch are shown before they are sent
		// During a run too: another process may approve its tasks.
		m.refreshDashboard()
		m.dropWithdrawn()
		next, cmd := m.afterTurn() // a handoff waiting for its target to be free
		return next, tea.Batch(cmd, dashboardTick(), gitStatusCmd(m.project))

	case clockMsg:
		m.clockRunning = false // Update rearms it while something runs
		m.frame++
		if m.focus == "" && m.busy {
			m.refreshViewport()
		}
		return m, nil

	case streamMsg:
		return m.handleStream(msg)

	case teamPermissionMsg:
		m.askPermission(msg.request, m.helperMode(), false)
		return m, waitTeamPermission(m.teamPerms)

	case gitStatusMsg:
		m.branch, m.dirty = msg.branch, msg.dirty
		return m, nil

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

	case fixShellMsg:
		return m.checkFix(msg)

	case fixedMsg:
		m.fixing = ""
		return m.update(taskActionMsg(msg))

	case landMsg:
		m.landing = msg // only a preview has a branch to merge
		m.setModal(msg.title, msg.body)
		return m, nil

	case jobStreamClosedMsg:
		return m, nil

	case planResultMsg:
		m.endJob()
		if errors.Is(msg.err, context.Canceled) {
			m.fail("plan failed", msg.err.Error())
		} else if msg.err != nil {
			m.setModal("plan failed", msg.err.Error()) // stays up: it can say where the lead's plan was kept
		} else if msg.result != nil {
			m.setModal("plan", planSummary(msg.result))
		}
		return m, nil

	case runResultMsg:
		m.endJob()
		body := runOutcomeText(msg.outcome, msg.err)
		switch {
		case msg.outcome == execution.RunCompleted && msg.err == nil:
			body += prDescription(m.ctx, m.store, msg.run) // as a headless run writes it
		case msg.outcome == execution.RunBlocked || msg.outcome == execution.RunBudget:
			if waits := m.waits(); waits != "" {
				body += "\n\n" + waits
			}
		}
		m.setModal("run "+msg.outcome, body)
		return m, nil

	case doctorResultMsg:
		m.busy = false
		m.state = "ready"
		if msg.err != nil {
			m.fail("doctor failed", msg.err.Error())
		} else {
			m.setModal("doctor", msg.report)
		}
		return m, nil

	case newSessionResultMsg:
		m.busy = false
		m.state = "ready"
		if msg.err != nil {
			m.fail("new session failed", msg.err.Error())
			return m, m.listen()
		}
		m.chat = msg.chat
		m.status = msg.status
		m.team.resetUsage(m.lead.Name)
		m.ctxUsed, m.ctxSize = 0, 0
		m.loadHistory() // a new session starts with an empty transcript
		m.refreshViewport()
		m.refreshDashboard()
		m.notify("new session", shortID(m.chat.Session.ID))
		return m, m.listen()

	case adapterActionMsg:
		m.installing = ""
		if msg.err != nil {
			m.fail(msg.name+" "+msg.action+" failed", firstLine(msg.err.Error())+"\n"+lastLine(msg.output))
			return m, nil
		}
		m.notify(msg.name+" "+msg.action+" complete", lastLine(msg.output))
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
			if h := m.handoff; h != nil { // the old lead was told h.to takes over
				note := h.to + " could not take over from " + h.from + ", whose handoff note was: " + h.note
				_ = m.chat.Note(m.ctx, note)
				m.appendSystem(note)
				m.refreshViewport()
			}
			m.handoff = nil
			m.fail("lead switch failed", msg.err.Error())
			return m, m.listen()
		}
		m.chat = msg.chat
		m.lead = msg.agent
		m.status = msg.status
		_ = m.persistLead(msg.agent.Name)
		m.team.setLead(msg.agent.Name)
		if msg.adopted == nil {
			m.team.resetUsage(msg.agent.Name) // a new session; a warm one keeps what it used
		}
		limits := m.team.Limits(msg.agent.Name)
		m.ctxUsed, m.ctxSize = limits.Used, limits.Size
		m.team.park(m.previousLead.Name, msg.previous) // instant to switch back
		// The new lead has its own session, so its transcript starts fresh.
		note := fmt.Sprintf("%s (%s) is now the lead", msg.agent.Name, msg.agent.Driver)
		_ = m.chat.Note(m.ctx, note)
		if m.handoff == nil {
			m.handoff = m.recap() // the user switched leads
		}
		m.loadHistory()
		m.refreshViewport()
		m.refreshDashboard()
		m.notify(msg.agent.Name+" is now the lead", msg.agent.Driver)
		m, deliver := m.deliverHandoff()
		return m, tea.Batch(m.listen(), deliver)

	default:
		// Pastes, and the input's own messages: ctrl+v's clipboard read and
		// the cursor blink. A list takes no text, so a paste is dropped while
		// one is open. The input is one line: pasted newlines become spaces.
		// They go to a question's answer while the user types one.
		if m.answering() {
			var cmd tea.Cmd
			m.answer, cmd = m.answer.Update(msg)
			return m, cmd
		}
		if _, paste := msg.(tea.PasteMsg); paste && (m.adapterPicker != nil || m.rulesManager != nil || m.sessionPicker != nil || m.taskPicker != nil || m.picker != nil) {
			return m, nil
		}
		return m, m.updateInput(msg)
	}
}

// updateInput hands msg to the input and keeps the command palette in step
// with its text.
func (m *model) updateInput(msg tea.Msg) tea.Cmd {
	value := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != value {
		m.refreshPalette()
	}
	return cmd
}

// answeringText says why a teammate cannot lead yet: the session answering
// is in use, and a switch would start a second one beside it.
func answeringText(name string) string {
	return name + " is answering a question; make it the lead once it has answered"
}

// beginLeadSwitch shows the new lead optimistically and starts its session in
// the background; leadChangedMsg reverts on failure.
func (m model) beginLeadSwitch(agent config.Agent) (tea.Model, tea.Cmd) {
	if m.chat == nil {
		m.notify("still connecting", m.lead.Name+" is still connecting; switch once it is ready")
		return m, nil
	}
	if m.busy || m.sending { // switching copies the Chat that a Send still writes to
		m.notify("busy", "wait for the current turn to finish")
		return m, nil
	}
	m.previousLead = m.lead
	m.lead = agent
	if err := m.persistLead(agent.Name); err != nil {
		m.lead = m.previousLead
		m.fail("could not save lead", err.Error())
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
	m.modalTop = 0
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
	case acpdriver.EventMessage, acpdriver.EventThought, acpdriver.EventToolCall, acpdriver.EventToolEdit, acpdriver.EventPlan:
		if event.Type == acpdriver.EventMessage && event.Text != "" {
			if n := len(m.entries); m.reply != "" && (n == 0 || !m.entries[n-1].open || m.entries[n-1].kind != entryAssistant) {
				m.reply += "\n\n" // the reply goes on after a tool call or a thought
			}
			m.reply += event.Text
		}
		applyEvent(&m.entries, event)
		m.step = strings.ToLower(activity(m.entries))
		if !m.busy { // while the lead works, the clock redraws: events come faster than redraws
			m.refreshViewport()
		}
		return m, m.listen()

	case acpdriver.EventUsage:
		if event.Usage != nil {
			m.ctxUsed, m.ctxSize = event.Usage.Used, event.Usage.Size
			m.team.setUsage(m.lead.Name, *event.Usage)
		}
		return m, m.listen()

	case acpdriver.EventTurn:
		m.closeAssistant()
		text := m.reply
		m.reply = ""
		m.busy = false
		m.state = "ready"
		if reason := acpdriver.LimitReason(event.StopReason, nil); reason != "" && m.pendingHandoff == nil {
			m.limitHandoff(reason)
		}
		m.refreshViewport()

		cmds := []tea.Cmd{m.listen()}
		if strings.TrimSpace(text) != "" {
			cmds = append(cmds, saveAssistant(m.ctx, m.chat, text))
		}
		next, cmd := m.afterTurn()
		return next, tea.Batch(append(cmds, cmd)...)

	case acpdriver.EventError:
		if event.Err != nil {
			m.appendError(event.Err.Error())
		}
		m.busy = false
		m.state = "error"
		m.reply = ""
		m.unqueue()
		m.refreshViewport()
		return m, m.listen()

	case acpdriver.EventExit:
		if event.Err != nil { // how it exited and the end of its stderr
			m.appendError(event.Err.Error())
		} else {
			m.appendSystem("agent exited")
		}
		m.busy = false
		m.state = "exited"
		m.reply = ""
		m.unqueue()
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
	key := msg.String()
	typed := strings.TrimSpace(m.answer.Value()) // only a question without options takes one
	choice, err := strconv.Atoi(key)
	answer := key == "esc" || err == nil && choice >= 1 && choice <= len(m.permission.Options) || key == "enter" && typed != ""
	if !answer {
		if m.answering() {
			var cmd tea.Cmd
			m.answer, cmd = m.answer.Update(msg)
			return m, cmd
		}
		// The prompt can open while the user types: typing goes on in the
		// input and holds answers off until it pauses.
		m.permissionHold = time.Now().Add(permissionPause)
		return m, m.updateInput(msg)
	}
	if time.Now().Before(m.permissionHold) {
		if key != "esc" && key != "enter" && m.input.Value() != "" { // a digit in the message being typed
			m.permissionHold = time.Now().Add(permissionPause)
			return m, m.updateInput(msg)
		}
		return m, nil // pressed before the prompt could be read: never picks an option
	}
	switch {
	case key == "esc":
		m.permission.Resolve(acpdriver.PermissionDeny) // reject, not ACP's "cancelled": that says the whole turn was cancelled
	case key == "enter":
		m.permission.Respond(typed)
	default:
		m.permission.Respond(m.permission.Options[choice-1].ID)
	}
	m.nextPermission()
	return m, nil
}

// unqueue puts messages still waiting for the lead back in the input, to be
// edited or sent again.
func (m *model) unqueue() {
	if m.queued == "" {
		return
	}
	m.input.SetValue(strings.TrimSpace(m.queued + " " + m.input.Value()))
	m.input.CursorEnd()
	m.queued = ""
	m.refreshViewport()
}

func (m model) handleSlash(text string) (tea.Model, tea.Cmd) {
	m.paletteOpen = false
	m.input.SetValue(text) // a command that has to wait stays in the input, whole
	m.input.CursorEnd()
	fields := strings.Fields(text)
	command := fields[0]
	switch command {
	case "/plan", "/run", "/doctor", "/lead", "/adapters", "/new", "/resume", "/session", "/land":
		if m.busy || m.sending { // a Send that has not returned still writes to this session
			m.notify("busy", "wait for the current turn to finish")
			return m, nil
		}
	}
	switch command {
	case "/session", "/new", "/resume":
		if m.chat == nil {
			m.notify("still connecting", m.lead.Name+" is still connecting; try again in a moment")
			return m, nil
		}
	case "/plan", "/run", "/adapters":
		// Never plan, start a second run, or install adapters mid-run,
		// detached runs included.
		if m.job == "run" || m.runOwner > 0 {
			m.notify("run in progress", "/stop ends the run first")
			return m, nil
		}
		if command == "/run" && m.fixing != "" { // its recovery would hand the task being checked back to a worker
			m.notify("checking your fix of "+shortTaskID(m.fixing), "/run waits for its check to finish")
			return m, nil
		}
		// The TUI shows only the newest run, so a new plan hides this one.
		if queued, active, review, _, blocked := m.taskCounts(); command == "/plan" && m.run != nil &&
			queued+active+review+blocked > 0 && m.planWarned != m.run.ID {
			m.planWarned = m.run.ID
			m.notify("the run is not finished", "a new plan hides its "+plural(queued+active+review+blocked, "unmerged task")+" · enter again to plan anyway")
			return m, nil
		}
	case "/tasks", "/review", "/agents", "/log", "/approve", "/reject", "/retry", "/fix", "/diff", "/why", "/land":
		if m.run == nil {
			m.notify("no run yet", "/plan <goal> or /plan --file <path> creates one")
			return m, nil
		}
	}
	m.input.Reset()
	switch command {
	case "/mode":
		if len(fields) == 1 {
			m.setModal("permission mode", fmt.Sprintf("Current: %s\n\n/mode build — allow reads in the agent's own folder; ask for other operations\n/mode plan — allow reads in the agent's own folder; deny other operations\n/mode full — automatically allow reported requests for this session\n\nThese policies apply only when an adapter requests permission; they are not a sandbox. Task merges always require your approval. Detached runs answer as build mode does, with their --allow setting where it would ask.", m.permissionMode))
			break
		}
		mode, ok := policy.Parse(fields[1])
		if !ok || len(fields) != 2 {
			m.notify("usage", "/mode build | plan | full")
			break
		}
		if m.busy || m.job != "" || len(m.agentBusy) != 0 || m.runOwner != 0 {
			m.notify("work in progress", "stop or finish active work before changing mode")
			break
		}
		m.permissionMode = mode
		if mode == policy.Full {
			m.notify("full mode enabled", "reported tool requests are automatically allowed this session; merge approval still applies")
		} else {
			m.notify("permission mode: "+string(mode), "applies to adapter permission requests in this session")
		}
	case "/parallel":
		if len(fields) == 1 {
			m.notify("foreground workers", fmt.Sprintf("maximum %d · /parallel <count> to change for this session", m.runConfig.MaxConcurrency))
			break
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil || n < 1 || n > 32 || len(fields) != 2 {
			m.notify("usage", "/parallel <count> (1–32)")
			break
		}
		if m.job != "" || m.runOwner != 0 {
			m.notify("run in progress", "stop or finish it before changing concurrency")
			break
		}
		m.runConfig.MaxConcurrency = n
		m.notify("foreground worker limit", fmt.Sprintf("up to %d tasks at once; subagents have a separate limit", n))
	case "/quit", "/exit":
		m.shutdown()
		return m, tea.Quit

	case "/home":
		m.home = true
		m.modal = ""
		m.modalTitle = ""
		m.refreshDashboard()

	case "/agent":
		m.openAgent(strings.Join(fields[1:], " "))

	case "/steer":
		to, message, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(text, "/steer")), " ")
		if message = strings.TrimSpace(message); message == "" {
			m.notify("usage", "/steer <agent> <text>")
			break
		}
		t, ctx := m.team, m.ctx
		return m, func() tea.Msg { // an adapter round trip, off the UI goroutine
			steerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			reply, err := t.Steer(steerCtx, to, message)
			if err != nil {
				return taskActionMsg{title: "steer failed", body: err.Error()}
			}
			return taskActionMsg{title: "steer " + to, body: reply}
		}

	case "/rules":
		if len(fields) > 1 {
			m.previewRules(fields[1])
			break
		}
		return m.openRulesManager()

	case "/session":
		return m.openSessionPicker()

	case "/plan":
		var goal, file, conversation string
		if len(fields) > 1 && fields[1] == "--file" {
			if len(fields) < 3 {
				m.notify("usage", "/plan --file <path>")
				break
			}
			file, _ = filepath.Abs(fields[2])
		} else {
			goal, conversation = strings.TrimSpace(strings.TrimPrefix(text, "/plan")), m.conversation()
			if goal == "" && conversation == "" {
				m.notify("nothing to plan from yet", "/plan <goal>, or talk the goal over with "+m.lead.Name+" first")
				break
			}
		}
		ctx := m.startJob("plan", "planning", true)
		return m, tea.Batch(
			startPlanCmd(ctx, m.project, goal, file, conversation, m.lead, m.rules, m.jobStream),
			m.waitJob(),
		)

	case "/run":
		if m.permissionMode == policy.Plan {
			m.notify("plan mode", "choose /mode build or /mode full before running workers")
			break
		}
		if m.run == nil {
			m.notify("no run yet", "/plan <goal> or /plan --file <path> first")
			break
		}
		m.refreshDashboard() // an accepted proposal gives a completed run work again
		// Its checks passed; running them again, maybe after /land and a prune
		// removed its worktree, could only flip a landed run to blocked.
		if queued, active, review, _, _ := m.taskCounts(); m.run.Status == execution.RunCompleted && queued+active+review == 0 {
			m.notify("the run is complete", "/land merges it into your branch")
			break
		}
		if len(fields) > 1 && fields[1] == "--detach" {
			permission := "deny"
			if len(fields) > 2 && fields[2] == "--allow" {
				permission = "allow"
			}
			return m, detachCmd(m.project, m.run.ID, permission, m.rulesDeclined)
		}
		ctx := m.startJob("run", "running", false)
		engine, server, err := m.startRun(ctx)
		if err != nil {
			m.endJob()
			m.fail("run failed", err.Error())
			break
		}
		m.engine = engine
		m.home = false
		return m, tea.Batch(runEngineCmd(ctx, engine, server, m.run.ID, m.jobStream), m.waitJob())

	case "/stop":
		switch {
		case m.job == "run":
			m.jobCancel()
			m.state = "cancelling"
		case m.runOwner > 0 && m.run != nil:
			return m, stopDetachedCmd(m.run.ID)
		default:
			m.notify("nothing to stop", "no run in progress")
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

	case "/why":
		if waits := m.waits(); waits != "" {
			m.setModal("why tasks wait", waits)
		} else if queued, active, review, _, blocked := m.taskCounts(); m.run.Status == execution.RunBlocked && queued+active+review+blocked == 0 {
			m.notify("the run is blocked", "every task is merged, but the run's own checks failed · /log shows why · /run runs them again")
		} else {
			m.notify("nothing waits", "every task is merged or under way")
		}

	case "/land":
		queued, active, review, _, blocked := m.taskCounts()
		switch {
		case m.run.Status != execution.RunCompleted && queued+active+review+blocked == 0:
			// Approvals without /run merge tasks; only a run runs its own checks.
			m.notify("the run is not complete", "every task is merged · /run completes it once the run's checks pass")
		case m.run.Status != execution.RunCompleted:
			m.notify("the run is not complete", "/land merges a completed run · /why says what waits")
		case m.landing.branch == m.run.IntegrationBranch:
			cmd := m.land()
			return m, cmd
		default:
			return m, landPreviewCmd(m.project, *m.run)
		}

	case "/log":
		if m.job == "run" { // the run log panel shows only the end of the output
			m.setModal("run log", m.jobLogText())
			total, room := m.modalLines()
			m.modalTop = max(0, total-room)
			break
		}
		m.setModal("run log", m.logContent())

	case "/approve", "/retry", "/diff":
		if len(fields) < 2 {
			m.notify("usage", command+" <task>")
			break
		}
		task, ok := m.findTask(fields[1])
		if !ok && command == "/approve" && fields[1] == "all" { // a task named all is approved alone
			var ids []string // in plan order, as the store lists them
			for _, task := range m.tasks {
				if task.Status == execution.StatusReview {
					ids = append(ids, task.ID)
				}
			}
			if len(ids) == 0 {
				m.notify("nothing to approve", "no task is waiting for review")
				break
			}
			return m, m.approveAll(ids)
		}
		if !ok {
			m.fail("unknown task "+fields[1], "/tasks lists them")
			break
		}
		return m, m.taskAction(command[1:], task.ID, "")

	case "/reject":
		if len(fields) < 2 {
			m.notify("usage", "/reject <task> <reason>")
			break
		}
		task, ok := m.findTask(fields[1])
		if !ok {
			m.fail("unknown task "+fields[1], "/tasks lists them")
			break
		}
		reason := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(text, "/reject")), fields[1]))
		return m, m.taskAction("reject", task.ID, reason)

	case "/fix":
		if len(fields) < 2 {
			m.notify("usage", "/fix <task>")
			break
		}
		return m.fixTask(fields[1])

	case "/doctor":
		m.busy = true
		m.state = "checking"
		return m, doctorCmd(m.project)

	case "/init":
		created, err := config.EnsureProject(m.project)
		if err != nil {
			m.fail("init failed", err.Error())
		} else if len(created) == 0 {
			m.notify("already initialized", "")
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
			m.fail("unknown agent "+fields[1], "/lead lists configured agents")
		case !adapterAvailable(agent.Driver):
			m.fail(agent.Name+" is unavailable", "its adapter "+agent.Driver+" is not installed; /adapters installs it")
		case agent.Name == m.lead.Name:
			m.notify(agent.Name+" is already the lead", "")
		case m.team.status(agent.Name) == "answering":
			m.notify("wait for "+agent.Name, answeringText(agent.Name))
		default:
			return m.beginLeadSwitch(agent)
		}

	case "/adapters":
		if m.installing != "" {
			m.notify("install running", m.installing+" is still running; wait for it to finish")
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

	case "/copy":
		code := len(fields) > 1 && fields[1] == "code"
		text, what := m.lastReply(code), "reply"
		if code {
			what = "code block"
		}
		if strings.TrimSpace(text) == "" {
			m.notify("nothing to copy", "no "+what+" here yet")
			break
		}
		m.notify("copied", "the last "+what)
		return m, tea.SetClipboard(text) // OSC 52, which the terminal must allow

	case "/remember":
		m.remember(strings.TrimSpace(strings.TrimPrefix(text, "/remember")))

	case "/help":
		m.setModal("help", helpText())

	default:
		m.fail("unknown command "+command, "/help lists commands")
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
	if result.Uncommitted > 0 {
		fmt.Fprintf(&b, "\nwarning: %d uncommitted changes are not in this run; workers start from the last commit", result.Uncommitted)
	}
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
	m.refreshViewport()
}
