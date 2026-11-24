package tui

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/policy"
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
	Resume  string // a session to reopen instead of starting a new one
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

	input     textinput.Model
	chatLines []string // the conversation, wrapped to the main pane
	chatTop   int      // first conversation line shown; -1 follows the end
	width     int
	height    int
	blurred   bool // the terminal reported it lost focus

	entries         []entry
	reply           string // the lead's reply so far this turn, every part of it
	queued          string // typed while the lead was busy or connecting; Update sends it once the lead is free
	busy            bool
	sending         bool // a Send to the lead has not returned yet
	state           string
	gen             int
	permission      *acpdriver.PermissionRequest // the prompt on screen
	permissionJob   bool                         // it is a run worker's
	permissionQueue []pendingPermission          // prompts waiting behind it
	permissionHold  time.Time                    // until then, keys cannot answer it
	answer          textinput.Model              // a question's typed answer, apart from the user's message
	err             error

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
	jobStarted       time.Time
	planText         string // the lead's raw plan output, shown as progress
	turnStarted      time.Time
	step             string // what the lead is doing in this turn
	ctxUsed, ctxSize int    // the lead's context window, when the adapter reports it
	clockRunning     bool
	branch           string
	dirty            int // uncommitted files
	engine           *execution.Engine
	runConfig        config.RunConfig
	permissionMode   policy.Mode
	taskPicker       *taskPicker
	sessionPicker    *sessionPicker
	rulesManager     *rulesManager
	toasts           []toast
	rulesReview      *rulesReview
	rulesDeclined    bool   // project rules left out this session
	zen              bool   // sidebar hidden
	installing       string // adapter action running in the background
	team             *team
	listenStop       chan struct{}
	runOwner         int     // PID of another process driving the run, such as a detached run
	fixing           string  // the task whose fix by hand Switch is checking
	planWarned       string  // the unfinished run /plan warned it would replace
	landing          landMsg // the preview /land showed; enter in its panel or /land again merges as it said
	picker           *leadPicker
	adapterPicker    *adapterPicker
	run              *store.Run
	tasks            []store.Task
	subagents        []store.Subagent
	runAgents        []store.RunAgent
	focus            string               // the agent the main pane follows; "" is the lead's chat
	card             int                  // the selected subagent card in a worker's view
	agentEntries     map[string][]entry   // each other agent's transcript this session
	agentBusy        map[string]time.Time // agents mid-turn, since when
	chatSubs         []store.Subagent     // subagents of teammates in the chat
	streams          chan streamMsg       // events from workers, teammates and subagents
	teamPerms        chan *acpdriver.PermissionRequest
	frame            int    // animation frame
	modalTop         int    // first line shown in a reading panel
	agentTop         int    // first line shown in an agent view; -1 follows the end
	showThoughts     bool   // ctrl+t
	showCommands     bool   // ctrl+e
	resume           string // session to reopen on connect
	handoffs         chan handoffMsg
	pendingHandoff   *handoffMsg     // starts when the lead's turn ends
	handoff          *handoffMsg     // delivered to the new lead once it connects
	limited          map[string]bool // leads a limit stopped since the user's last message
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
	answer := textinput.New()
	answer.Prompt = ""
	answer.Focus()

	m := model{
		ctx:            ctx,
		cancel:         cancel,
		project:        opts.Project,
		rules:          opts.Rules,
		store:          opts.Store,
		created:        opts.Created,
		fleet:          opts.Fleet,
		lead:           opts.Lead,
		runConfig:      opts.Run,
		permissionMode: policy.Build,
		input:          input,
		answer:         answer,
		chatTop:        -1,
		width:          80,
		height:         24,
		state:          "connecting",
		connecting:     true,
		agentTop:       -1,
		resume:         opts.Resume,
		home:           opts.Resume == "", // a reopened session shows its conversation
	}
	m.streams = make(chan streamMsg, 512)
	m.teamPerms = make(chan *acpdriver.PermissionRequest)
	m.agentEntries, m.agentBusy = map[string][]entry{}, map[string]time.Time{}
	m.team = newTeam(m.project, m.dashboardFleet(), m.lead.Name)
	m.team.base = ctx
	m.team.ctx, m.team.stop = context.WithCancel(ctx)
	m.team.events, m.team.perms, m.team.subCfg, m.team.rules = m.streams, m.teamPerms, opts.Run.Subagents, m.rules
	m.handoffs = make(chan handoffMsg, 1)
	m.team.handoffs, m.team.handoffAt = m.handoffs, opts.Run.HandoffAt
	m.listenStop = make(chan struct{})
	m.reviewRules()
	m.layout()
	return m
}

// Init renders the shell immediately and connects the lead in the background.
func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.connectCmd(), dashboardTick(), gitStatusCmd(m.project),
		waitStream(m.streams), waitTeamPermission(m.teamPerms), waitHandoff(m.handoffs))
}

type dashboardTickMsg struct{}

// dashboardTick refreshes run state written by other processes: a detached
// run, or a headless run in another terminal.
func dashboardTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return dashboardTickMsg{} })
}

// loadHistory shows the current session's saved transcript: a resumed
// session shows its conversation, a new one starts empty. Session plumbing
// (resume failures, adapter logs) stays out of it; /session shows that.
func (m *model) loadHistory() {
	m.entries = nil
	if len(m.created) > 0 {
		names := make([]string, 0, len(m.created))
		for _, path := range m.created {
			if rel, err := filepath.Rel(m.project, path); err == nil && !strings.HasPrefix(rel, "..") {
				path = rel
			}
			names = append(names, shortenHome(path))
		}
		m.appendSystem("created " + strings.Join(names, ", "))
		m.created = nil
	}
	if m.chat == nil {
		return
	}
	messages, err := m.chat.History(m.ctx, 51)
	if err != nil {
		return
	}
	if len(messages) > 50 {
		messages = messages[1:]
		m.appendSystem("earlier messages are not shown")
	}
	for _, message := range messages {
		switch message.Role {
		case "user":
			content := strings.TrimPrefix(message.Content, planModeNote)
			if strings.HasPrefix(content, "[Switch] ") { // Switch's own prompt, such as a handoff
				m.appendSystem(content)
				break
			}
			m.appendUser(content)
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
	defer func() {
		tasks, run := m.tasks, m.run
		if m.job != "run" && m.runOwner == 0 {
			tasks, run = nil, nil // nothing drives the run, so no task turn runs, whatever the store says
		}
		m.team.setRunTasks(tasks, m.runAgents)
		m.team.bindRun(m.store, run, m.engine)
		m.settleBusy()
	}()
	run, ok, err := m.store.LatestRun(m.ctx, m.project)
	if err != nil || !ok {
		m.run = nil
		m.tasks = nil
		m.subagents = nil
		m.runAgents = nil
		return
	}
	m.run = &run
	m.tasks, _ = m.store.TasksForRun(m.ctx, run.ID)
	m.subagents, _ = m.store.SubagentsForRun(m.ctx, run.ID)
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
	t := m.team
	return func() tea.Msg {
		t.listen()
		conversation, status, err := chat.Open(m.ctx, chat.Options{
			Project:   m.project,
			Name:      m.lead.Name,
			Agent:     m.lead.Driver,
			Model:     m.lead.Model,
			Rules:     m.rules,
			Store:     m.store,
			New:       m.resume == "", // every launch starts fresh unless --resume names a session
			SessionID: m.resume,
			Grant:     t.grant,
			Context:   t.describe,
		})
		return connectedMsg{chat: conversation, status: status, err: err}
	}
}

// ResumeID is the session to offer reopening after Switch exits: the lead's
// current session, once it has a conversation.
func (m model) ResumeID() string {
	if m.chat == nil || m.chat.Session == nil {
		return ""
	}
	for _, e := range m.entries {
		if e.kind == entryUser && e.who == "" {
			return m.chat.Session.ID
		}
	}
	return ""
}

func (m *model) shutdown() {
	// Save the visible lead on the way out too. This covers quitting while a
	// switch is still connecting, before leadChangedMsg can persist it.
	if m.lead.Name != "" {
		_ = m.persistLead(m.lead.Name)
	}
	m.stopJob()
	// Close the lead and the warm team in parallel, then stop anything left.
	var wg sync.WaitGroup
	if m.chat != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.chat.Close(context.Background())
		}()
	}
	if m.team != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.team.close()
		}()
	}
	wg.Wait()
	if m.cancel != nil {
		m.cancel()
	}
}
