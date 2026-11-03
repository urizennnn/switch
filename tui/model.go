package tui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

type Options struct {
	Context context.Context
	Project string
	Rules   rules.Set
	Store   *store.Store
	Created []string
	Fleet   []config.Agent
	Lead    config.Agent
	Run     config.RunConfig
}

type model struct {
	ctx          context.Context
	cancel       context.CancelFunc
	project      string
	chat         *chat.Chat
	rules        rules.Set
	store        *store.Store
	status       chat.Status
	created      []string
	fleet        []config.Agent
	lead         config.Agent
	previousLead config.Agent

	input    textinput.Model
	viewport viewport.Model
	width    int
	height   int

	entries    []entry
	queued     string // sent once the lead connects
	busy       bool
	state      string
	gen        int
	permission *acpdriver.PermissionRequest
	err        error

	connecting       bool
	connectErr       error
	home             bool
	modalTitle       string
	modal            string
	job              string
	jobCancel        context.CancelFunc
	jobStream        chan jobText
	jobPermissions   chan *acpdriver.PermissionRequest
	jobChanges       chan struct{}
	jobLog           string
	jobPartial       map[string]string
	permissionResume tea.Cmd
	engine           *execution.Engine
	runConfig        config.RunConfig
	taskPicker       *taskPicker
	runOwner         int // PID of another process driving the run, such as a detached run
	picker           *leadPicker
	adapterPicker    *adapterPicker
	run              *store.Run
	tasks            []store.Task
	runAgents        []store.RunAgent
	palette          []slashCommand
	paletteIndex     int
	paletteOpen      bool
}

func New(opts Options) model {
	ctx, cancel := context.WithCancel(opts.Context)
	input := textinput.New()
	input.Placeholder = "State your wish"
	input.Prompt = ""
	input.SetVirtualCursor(true)
	input.SetWidth(60)
	input.Focus()

	viewportModel := viewport.New(
		viewport.WithWidth(80),
		viewport.WithHeight(18),
	)
	viewportModel.SoftWrap = true

	m := model{
		ctx:        ctx,
		cancel:     cancel,
		project:    opts.Project,
		rules:      opts.Rules,
		store:      opts.Store,
		created:    opts.Created,
		fleet:      opts.Fleet,
		lead:       opts.Lead,
		runConfig:  opts.Run,
		input:      input,
		viewport:   viewportModel,
		width:      80,
		height:     24,
		state:      "connecting",
		connecting: true,
		home:       true, // usable at once; the lead connects in the background
	}
	m.layout()
	return m
}

// Init renders the shell immediately and connects the lead in the background.
func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.connectCmd(), dashboardTick())
}

type dashboardTickMsg struct{}

// dashboardTick refreshes run state written by other processes: a detached
// run, or a headless run in another terminal.
func dashboardTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return dashboardTickMsg{} })
}

// loadHistory fills the transcript from the project's saved conversation.
// Session plumbing (resume failures, adapter logs) stays out of it; /session
// shows that.
func (m *model) loadHistory() {
	m.entries = nil
	if len(m.created) > 0 {
		m.appendSystem("initialized .switch/ config and rules")
	}
	messages, err := m.store.Messages(m.ctx, m.project, 20)
	if err != nil {
		return
	}
	for _, message := range messages {
		switch message.Role {
		case "user":
			m.appendUser(message.Content)
		case "assistant":
			m.appendAssistantChunk(message.Content)
			m.closeAssistant()
		case "system":
			m.appendSystem(message.Content)
		}
	}
}

func (m model) ruleSummary() string {
	if m.rules.Empty() {
		return "rules: none"
	}
	scopes := make([]string, 0, len(m.rules.Sources))
	for _, source := range m.rules.Sources {
		scopes = append(scopes, source.Scope)
	}
	return "rules: " + strings.Join(scopes, ", ")
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func (m *model) refreshDashboard() {
	if m.store == nil || m.project == "" {
		return
	}
	run, ok, err := m.store.LatestRun(m.ctx, m.project)
	if err != nil || !ok {
		m.run = nil
		m.tasks = nil
		m.runAgents = nil
		return
	}
	m.run = &run
	m.tasks, _ = m.store.TasksForRun(m.ctx, run.ID)
	m.runAgents, _ = m.store.RunAgents(m.ctx, run.ID)
	m.runOwner = 0
	if m.job != "run" {
		if pid, ok := execution.RunOwner(config.StateDir(), run.ID); ok {
			m.runOwner = pid
		}
	}
}

func (m model) dashboardFleet() []config.Agent {
	if len(m.fleet) > 0 {
		return m.fleet
	}
	return []config.Agent{m.lead}
}

// roleLabel is the role shown for an agent: whichever agent leads is
// "leading", and an agent configured as lead that is not leading now is
// "standby". It avoids the word "lead", which is often an agent's name.
func (m model) roleLabel(agent config.Agent) string {
	switch {
	case agent.Name == m.lead.Name:
		return "leading"
	case agent.Role == "lead":
		return "standby"
	default:
		return agent.Role
	}
}

// fleetAgent finds a configured agent by name. Bare adapter names are not
// agents and never match.
func (m model) fleetAgent(name string) (config.Agent, bool) {
	for _, agent := range m.dashboardFleet() {
		if agent.Name == name {
			return agent, true
		}
	}
	return config.Agent{}, false
}

// taskCounts groups task statuses for the sidebar and status bar.
func (m model) taskCounts() (queued, active, review, merged, blocked int) {
	for _, task := range m.tasks {
		switch task.Status {
		case execution.StatusQueued, execution.StatusInterrupted:
			queued++
		case execution.StatusReview:
			review++
		case execution.StatusMerged, "done":
			merged++
		case execution.StatusBlocked, "failed":
			blocked++
		default:
			active++
		}
	}
	return queued, active, review, merged, blocked
}

type connectedMsg struct {
	chat   *chat.Chat
	status chat.Status
	err    error
}

func (m model) connectCmd() tea.Cmd {
	return func() tea.Msg {
		conversation, status, err := chat.Open(m.ctx, chat.Options{
			Project: m.project,
			Agent:   m.lead.Driver,
			Model:   m.lead.Model,
			Rules:   m.rules,
			Store:   m.store,
		})
		return connectedMsg{chat: conversation, status: status, err: err}
	}
}

func (m *model) shutdown() {
	// Save the visible lead on the way out too. This covers quitting while a
	// switch is still connecting, before leadChangedMsg can persist it.
	if m.lead.Name != "" {
		_ = m.persistLead(m.lead.Name)
	}
	m.stopJob()
	if m.cancel != nil {
		m.cancel()
	}
	if m.chat != nil {
		_ = m.chat.Close(context.Background())
	}
}
