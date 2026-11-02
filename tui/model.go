package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

type Options struct {
	Context context.Context
	Chat    *chat.Chat
	Rules   rules.Set
	Store   *store.Store
	Status  chat.Status
	Created []string
	Fleet   []config.Agent
	Lead    config.Agent
}

type model struct {
	ctx     context.Context
	chat    *chat.Chat
	rules   rules.Set
	store   *store.Store
	status  chat.Status
	created []string
	fleet   []config.Agent
	lead    config.Agent

	input    textinput.Model
	viewport viewport.Model
	width    int
	height   int

	transcript    string
	assistant     strings.Builder
	assistantOpen bool
	busy          bool
	state         string
	gen           int
	permission    *acpdriver.PermissionRequest
	err           error

	home         bool
	modalTitle   string
	modal        string
	run          *store.Run
	tasks        []store.Task
	runAgents    []store.RunAgent
	palette      []slashCommand
	paletteIndex int
	paletteOpen  bool
}

func New(opts Options) model {
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
		ctx:      opts.Context,
		chat:     opts.Chat,
		rules:    opts.Rules,
		store:    opts.Store,
		status:   opts.Status,
		created:  opts.Created,
		fleet:    opts.Fleet,
		lead:     opts.Lead,
		input:    input,
		viewport: viewportModel,
		width:    80,
		height:   24,
		state:    "ready",
		home:     true,
	}

	if m.lead.Name == "" {
		m.lead = config.Agent{Name: m.chat.Agent, Role: "lead", Driver: m.chat.Agent, Model: m.chat.Model}
	}

	m.transcript = m.initialTranscript()
	m.refreshViewport()
	m.refreshDashboard()
	m.layout()
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		textinput.Blink,
		waitForActivity(m.chat.Session, m.gen),
	)
}

func (m model) initialTranscript() string {
	var b strings.Builder

	if len(m.created) > 0 {
		b.WriteString(systemStyle.Render("initialized .switch/ config and rules"))
		b.WriteString("\n\n")
	}

	if m.status.ResumeError != nil {
		b.WriteString(systemStyle.Render(fmt.Sprintf("resume failed; started a new session (%v)", m.status.ResumeError)))
		b.WriteString("\n\n")
	}

	messages, err := m.store.Messages(m.ctx, m.chat.Project, 20)
	if err == nil {
		for _, message := range messages {
			switch message.Role {
			case "user":
				b.WriteString(labelStyle.Render("you"))
				b.WriteString("  ")
				b.WriteString(textStyle.Render(message.Content))
				b.WriteString("\n\n")
			case "assistant":
				b.WriteString(agentStyle.Render("switch"))
				b.WriteString("  ")
				b.WriteString(textStyle.Render(message.Content))
				b.WriteString("\n\n")
			}
		}
	}

	if b.Len() == 0 {
		b.WriteString(systemStyle.Render("New session. Type /help for commands or a message below."))
		b.WriteString("\n")
	}
	return b.String()
}

func (m *model) appendUser(text string) {
	if m.assistantOpen {
		m.transcript += "\n\n"
		m.assistantOpen = false
	}
	m.transcript += labelStyle.Render("you") + "  " + textStyle.Render(text) + "\n\n"
}

func (m *model) appendAssistantChunk(text string) {
	if text == "" {
		return
	}
	if !m.assistantOpen {
		if m.transcript != "" && !strings.HasSuffix(m.transcript, "\n\n") {
			m.transcript += "\n\n"
		}
		m.transcript += agentStyle.Render("switch") + "  "
		m.assistantOpen = true
	}
	m.transcript += textStyle.Render(text)
	m.assistant.WriteString(text)
}

func (m *model) closeAssistant() {
	if m.assistantOpen {
		m.transcript += "\n\n"
		m.assistantOpen = false
	}
}

func (m *model) appendSystem(text string) {
	m.closeAssistant()
	m.transcript += systemStyle.Render(text) + "\n\n"
}

func (m *model) appendError(text string) {
	m.closeAssistant()
	m.transcript += errorStyle.Render(text) + "\n\n"
}

func (m *model) refreshViewport() {
	content := strings.TrimRight(m.transcript, "\n")
	m.viewport.SetContent(content)
	m.viewport.GotoBottom()
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
	if m.store == nil || m.chat == nil {
		return
	}
	run, ok, err := m.store.LatestRun(m.ctx, m.chat.Project)
	if err != nil || !ok {
		m.run = nil
		m.tasks = nil
		m.runAgents = nil
		return
	}
	m.run = &run
	m.tasks, _ = m.store.TasksForRun(m.ctx, run.ID)
	m.runAgents, _ = m.store.RunAgents(m.ctx, run.ID)
}

func (m model) dashboardFleet() []config.Agent {
	if len(m.fleet) > 0 {
		return m.fleet
	}
	if m.chat == nil {
		return nil
	}
	return []config.Agent{{Name: "lead", Role: "lead", Driver: m.chat.Agent, Model: m.chat.Model}}
}

func (m model) taskCounts() (queued, running, done int) {
	for _, task := range m.tasks {
		switch task.Status {
		case "queued":
			queued++
		case "running", "assigned", "verifying", "review":
			running++
		case "done", "merged", "approved":
			done++
		}
	}
	return queued, running, done
}
