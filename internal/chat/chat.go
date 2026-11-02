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
	Agent   string
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
		resumeErr = err
		session, err = acpdriver.Open(ctx, acpdriver.Spec{
			Agent:     agent,
			Cwd:       project,
			Model:     model,
			SessionID: "",
			AgentLogs: opts.AgentLogs,
		})
	} else if err == nil && sessionID != "" {
		resumed = true
	}
	if err != nil {
		return nil, Status{}, err
	}

	chat := &Chat{
		Project: project,
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
	if err := c.Store.AppendMessage(ctx, c.Project, "user", text); err != nil {
		return acp.PromptResponse{}, err
	}
	return c.Session.Prompt(ctx, c.Rules.Prompt(text))
}

func (c *Chat) SaveAssistant(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return c.Store.AppendMessage(ctx, c.Project, "assistant", text)
}

func (c *Chat) NewSession(ctx context.Context) (Status, error) {
	_ = c.Session.Close(context.Background())
	session, err := acpdriver.Open(ctx, acpdriver.Spec{
		Agent: c.Agent,
		Cwd:   c.Project,
		Model: c.Model,
	})
	if err != nil {
		return Status{}, err
	}
	c.Session = session
	c.Model = session.Model
	if err := c.persistSession(ctx); err != nil {
		return Status{}, err
	}
	return Status{
		Agent:     c.Agent,
		Model:     session.Model,
		SessionID: session.ID,
		Caps:      session.Caps,
	}, nil
}

func (c *Chat) Close(ctx context.Context) error {
	return c.Session.Close(ctx)
}

func (c *Chat) persistSession(ctx context.Context) error {
	return c.Store.SaveChatSession(ctx, store.ChatSession{
		Project:   c.Project,
		Agent:     c.Agent,
		Model:     c.Model,
		SessionID: c.Session.ID,
	})
}
