package chat

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

type Options struct {
	Project   string
	Name      string // configured agent name, recorded so the session can be resumed
	Agent     string
	Model     string
	Rules     rules.Set
	Store     *store.Store
	New       bool
	AgentLogs io.Writer
}

type Status struct {
	Agent       string
	Model       string
	SessionID   string
	Resumed     bool
	ResumeError error
	Caps        acpdriver.CapabilitiesReport
}

type Chat struct {
	Project string
	Name    string // configured agent name
	Agent   string // driver
	Model   string
	Rules   rules.Set
	Store   *store.Store
	Session *acpdriver.Session
}

// Open creates or resumes the project chat session.
func Open(ctx context.Context, opts Options) (*Chat, Status, error) {
	project, err := filepath.Abs(opts.Project)
	if err != nil {
		return nil, Status{}, err
	}

	stored, hasStored, err := opts.Store.GetChatSession(ctx, project)
	if err != nil {
		return nil, Status{}, fmt.Errorf("load chat session: %w", err)
	}

	agent := strings.TrimSpace(opts.Agent)
	if agent == "" && hasStored && !opts.New {
		agent = stored.Agent
	}
	if agent == "" {
		agent = "opencode"
	}

	model := strings.TrimSpace(opts.Model)
	if model == "" && hasStored && !opts.New && stored.Agent == agent {
		model = stored.Model
	}
	if model == "" && agent == "opencode" {
		// OpenCode's default model can be blocked by provider privacy settings.
		// Use a known free model for a working first-run experience.
		model = "opencode/deepseek-v4.1-flash-free"
	}

	sessionID := ""
	if hasStored && !opts.New && stored.Agent == agent {
		sessionID = stored.SessionID
	}

	var resumeErr error
	resumed := false
	session, err := acpdriver.Open(ctx, acpdriver.Spec{
		Agent:     agent,
		Cwd:       project,
		Model:     model,
		SessionID: sessionID,
		AgentLogs: opts.AgentLogs,
	})
	if err != nil && sessionID != "" {
		// The adapter itself failed; try once more without resuming.
		resumeErr = err
		session, err = acpdriver.Open(ctx, acpdriver.Spec{
			Agent:     agent,
			Cwd:       project,
			Model:     model,
			SessionID: "",
			AgentLogs: opts.AgentLogs,
		})
	} else if err == nil && sessionID != "" {
		// A failed resume falls back to a new session in the same process.
		resumeErr = session.ResumeError
		resumed = resumeErr == nil
	}
	if err != nil {
		return nil, Status{}, err
	}

	chat := &Chat{
		Project: project,
		Name:    opts.Name,
		Agent:   agent,
		Model:   session.Model,
		Rules:   opts.Rules,
		Store:   opts.Store,
		Session: session,
	}
	if err := chat.persistSession(ctx); err != nil {
		_ = session.Close(context.Background())
		return nil, Status{}, err
	}

	status := Status{
		Agent:       agent,
		Model:       session.Model,
		SessionID:   session.ID,
		Resumed:     resumed,
		ResumeError: resumeErr,
		Caps:        session.Caps,
	}
	return chat, status, nil
}

// Send persists the user message, prepends the effective rules, and runs one
// turn. Events stream on chat.Session.Events while it runs.
func (c *Chat) Send(ctx context.Context, text string) (acp.PromptResponse, error) {
	if err := c.Store.AppendMessage(ctx, c.Project, c.Session.ID, "user", text); err != nil {
		return acp.PromptResponse{}, err
	}
	return c.Session.Prompt(ctx, c.Rules.Prompt(text))
}

func (c *Chat) SaveAssistant(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return c.Store.AppendMessage(ctx, c.Project, c.Session.ID, "assistant", text)
}

// Note records a line in this session's transcript, such as a lead change.
func (c *Chat) Note(ctx context.Context, text string) error {
	return c.Store.AppendMessage(ctx, c.Project, c.Session.ID, "system", text)
}

// History returns this session's transcript, oldest first. A resumed session
// shows its own conversation; a new one starts empty.
func (c *Chat) History(ctx context.Context, limit int) ([]store.Message, error) {
	return c.Store.Messages(ctx, c.Project, c.Session.ID, limit)
}

// SwitchAgent opens a fresh session for another agent and returns a new Chat
// bound to it. The receiver is left untouched so callers on another goroutine
// can keep reading it; the old session is closed only once the new one is
// ready and persisted.
func (c *Chat) SwitchAgent(ctx context.Context, name, driver, model string) (*Chat, Status, error) {
	return c.reopen(ctx, name, driver, model, "")
}

// NewSession replaces the current session with a fresh one for the same agent.
func (c *Chat) NewSession(ctx context.Context) (*Chat, Status, error) {
	return c.SwitchAgent(ctx, c.Name, c.Agent, c.Model)
}

// Resume reopens a past session of this project with the agent that owns it.
// When the adapter cannot resume it, the new Chat has a fresh session and
// Status.ResumeError says why.
func (c *Chat) Resume(ctx context.Context, name, driver, model, sessionID string) (*Chat, Status, error) {
	return c.reopen(ctx, name, driver, model, sessionID)
}

func (c *Chat) reopen(ctx context.Context, name, driver, model, sessionID string) (*Chat, Status, error) {
	if model == "" && driver == "opencode" {
		model = "opencode/deepseek-v4.1-flash-free"
	}
	session, err := acpdriver.Open(ctx, acpdriver.Spec{
		Agent:     driver,
		Cwd:       c.Project,
		Model:     model,
		SessionID: sessionID,
	})
	if err != nil {
		return nil, Status{}, err
	}

	next := *c
	next.Session = session
	next.Name = name
	next.Agent = driver
	next.Model = session.Model
	if err := next.persistSession(ctx); err != nil {
		_ = session.Close(context.Background())
		return nil, Status{}, err
	}
	if c.Session != nil {
		_ = c.Session.Close(context.Background())
	}
	return &next, Status{
		Agent:       driver,
		Model:       session.Model,
		SessionID:   session.ID,
		Resumed:     sessionID != "" && session.ResumeError == nil,
		ResumeError: session.ResumeError,
		Caps:        session.Caps,
	}, nil
}

func (c *Chat) Close(ctx context.Context) error {
	return c.Session.Close(ctx)
}

func (c *Chat) persistSession(ctx context.Context) error {
	return c.Store.SaveChatSession(ctx, store.ChatSession{
		Project:   c.Project,
		Name:      c.Name,
		Agent:     c.Agent,
		Model:     c.Model,
		SessionID: c.Session.ID,
	})
}
