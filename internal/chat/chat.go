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
	SessionID string // resume this session instead of the stored one
	AgentLogs io.Writer
	// Grant returns the MCP servers and environment that let an agent's
	// session reach its team (switch_team, switch_ask).
	Grant func(name string) ([]acp.McpServer, []string)
	// Context describes the team to the lead; it is prepended to each message.
	Context func(name string) string
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
	Grant   func(name string) ([]acp.McpServer, []string)
	Context func(name string) string

	rulesSent string // hash of the rules this session was last given
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
	stored.Agent = acpdriver.DriverName(stored.Agent) // older builds stored it as configured, such as claude-code

	agent := acpdriver.DriverName(opts.Agent)
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
	sessionID := opts.SessionID
	if sessionID == "" && hasStored && !opts.New && stored.Agent == agent {
		sessionID = stored.SessionID
	}

	var resumeErr error
	resumed := false
	spec := sessionSpec(project, opts.Name, agent, model, sessionID, opts.Grant)
	spec.AgentLogs = opts.AgentLogs
	session, err := acpdriver.Open(ctx, spec)
	if err != nil && sessionID != "" {
		// The adapter itself failed; try once more without resuming.
		resumeErr = err
		spec.SessionID = ""
		session, err = acpdriver.Open(ctx, spec)
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
		Grant:   opts.Grant,
		Context: opts.Context,
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

// Send persists the user message and runs one turn. The lead's rules are
// sent with the first message of a session and again whenever the files
// change, not with every message. Events stream on chat.Session.Events while
// it runs.
func (c *Chat) Send(ctx context.Context, text string) (acp.PromptResponse, error) {
	if err := c.Store.AppendMessage(ctx, c.Project, c.Session.ID, "user", text); err != nil {
		return acp.PromptResponse{}, err
	}
	prompt, hash, err := c.withRules(text)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if c.Context != nil {
		if team := c.Context(c.Name); team != "" {
			prompt = team + "\n\n" + prompt
		}
	}
	resp, err := c.Session.Prompt(ctx, prompt)
	if err == nil {
		c.rulesSent = hash
	}
	return resp, err
}

// EffectiveRules returns the rules the lead is given: base rules plus roles/lead.md
// and this agent's own file, read fresh from disk.
func (c *Chat) EffectiveRules() (rules.Set, error) {
	return rules.Scoped(c.Rules, "lead", c.Name, "")
}

func (c *Chat) withRules(text string) (string, string, error) {
	set, err := c.EffectiveRules()
	if err != nil {
		return "", "", err
	}
	hash := set.Hash()
	switch {
	case hash == c.rulesSent:
		return text, hash, nil
	case set.Empty():
		return "Switch: the operating rules given earlier in this session no longer apply.\n\n" + text, hash, nil
	case c.rulesSent != "":
		return "Switch: your operating rules changed; these replace the earlier ones.\n\n" + set.Prompt(text), hash, nil
	default:
		return set.Prompt(text), hash, nil
	}
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
	session, err := acpdriver.Open(ctx, sessionSpec(c.Project, name, driver, model, sessionID, c.Grant))
	if err != nil {
		return nil, Status{}, err
	}
	next, status, err := c.Adopt(ctx, name, session)
	if err != nil {
		_ = session.Close(context.Background())
		return nil, Status{}, err
	}
	if c.Session != nil {
		_ = c.Session.Close(context.Background())
	}
	status.Resumed = sessionID != "" && session.ResumeError == nil
	return next, status, nil
}

// Connect opens a new session for another agent like SwitchAgent, but leaves
// the current session open so the caller can keep it warm.
func (c *Chat) Connect(ctx context.Context, name, driver, model string) (*Chat, Status, error) {
	session, err := StartSession(ctx, c.Project, name, driver, model, c.Grant)
	if err != nil {
		return nil, Status{}, err
	}
	next, status, err := c.Adopt(ctx, name, session)
	if err != nil {
		_ = session.Close(context.Background())
	}
	return next, status, err
}

// Adopt returns a Chat bound to an already running session, such as one
// started in the background. The current session is left open.
func (c *Chat) Adopt(ctx context.Context, name string, session *acpdriver.Session) (*Chat, Status, error) {
	next := *c
	next.rulesSent = "" // a different session has not seen the rules
	next.Session = session
	next.Name = name
	next.Agent = session.Agent
	next.Model = session.Model
	if err := next.persistSession(ctx); err != nil {
		return nil, Status{}, err
	}
	return &next, Status{
		Agent:       session.Agent,
		Model:       session.Model,
		SessionID:   session.ID,
		ResumeError: session.ResumeError,
		Caps:        session.Caps,
	}, nil
}

// StartSession starts a new session for an agent in the project, for callers
// that keep sessions warm before they are needed.
func StartSession(ctx context.Context, project, name, driver, model string, grant func(string) ([]acp.McpServer, []string)) (*acpdriver.Session, error) {
	return acpdriver.Open(ctx, sessionSpec(project, name, driver, model, "", grant))
}

func sessionSpec(project, name, driver, model, sessionID string, grant func(string) ([]acp.McpServer, []string)) acpdriver.Spec {
	if model == "" && driver == "opencode" {
		// OpenCode's default model can be blocked by provider privacy settings.
		model = "opencode/deepseek-v4.1-flash-free"
	}
	spec := acpdriver.Spec{Agent: driver, Cwd: project, Model: model, SessionID: sessionID}
	if grant != nil {
		spec.MCPServers, spec.Env = grant(name)
	}
	return spec
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
