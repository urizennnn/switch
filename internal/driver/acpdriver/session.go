package acpdriver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	buildversion "github.com/urizennnn/switch/internal/version"
)

// Spec describes one ACP agent session.
type Spec struct {
	Agent     string
	Command   []string
	Cwd       string
	Model     string
	SessionID string // resume when non-empty
	Env       []string
	// AgentLogs, when non-nil, receives adapter stderr. Otherwise stderr is
	// buffered for diagnostics.
	AgentLogs io.Writer
}

type EventType string

const (
	EventMessage  EventType = "message"
	EventThought  EventType = "thought"
	EventToolCall EventType = "tool_call"
	EventToolEdit EventType = "tool_update"
	EventPlan     EventType = "plan"
	EventUsage    EventType = "usage"
	EventMode     EventType = "mode"
	EventConfig   EventType = "config"
	EventInfo     EventType = "info"
	EventTurn     EventType = "turn"
	EventError    EventType = "error"
	EventExit     EventType = "exit"
)

type Event struct {
	Type       EventType
	SessionID  string
	Text       string
	Tool       *ToolInfo
	Plan       []PlanEntry
	Usage      *UsageInfo
	StopReason string
	Raw        any
	Err        error
}

type ToolInfo struct {
	ID     string
	Title  string
	Status string
	Kind   string
}

type PlanEntry struct {
	Content  string
	Status   string
	Priority string
}

type UsageInfo struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	Used         int
	Size         int
	CostAmount   float64
	CostCurrency string
}

type PermissionOption struct {
	ID   string
	Name string
	Kind string
}

type PermissionRequest struct {
	SessionID  string
	ToolCallID string
	Title      string
	Options    []PermissionOption

	response chan PermissionDecision
}

type PermissionDecision struct {
	OptionID string
	Cancel   bool
}

func (p *PermissionRequest) Respond(optionID string) {
	select {
	case p.response <- PermissionDecision{OptionID: optionID}:
	default:
	}
}

func (p *PermissionRequest) Cancel() {
	select {
	case p.response <- PermissionDecision{Cancel: true}:
	default:
	}
}

// Session is a live ACP agent session. Events and permission requests are
// delivered on channels so a TUI or CLI can consume them independently.
type Session struct {
	ID     string
	Agent  string
	Cwd    string
	Model  string
	Caps   CapabilitiesReport
	Events <-chan Event

	events      chan Event
	permissions chan *PermissionRequest
	conn        *acp.ClientSideConnection
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	waitCh      <-chan error
	cancel      context.CancelFunc
	init        acp.InitializeResponse
	stderr      bytes.Buffer
	done        chan struct{}
	closeOnce   sync.Once
	exitMu      sync.Mutex
	exitErr     error
}

// Open starts an adapter, initializes ACP, and creates or resumes a session.
func Open(ctx context.Context, spec Spec) (*Session, error) {
	if spec.Agent == "" {
		spec.Agent = "opencode"
	}
	spec.Agent = strings.ToLower(strings.TrimSpace(spec.Agent))

	cwd, err := filepath.Abs(spec.Cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve cwd: %w", err)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return nil, fmt.Errorf("cwd %s: %w", cwd, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("cwd %s is not a directory", cwd)
	}

	command := spec.Command
	if len(command) == 0 {
		adapter, err := ResolveAdapter(spec.Agent)
		if err != nil {
			return nil, err
		}
		command = adapter.Command
	}

	runCtx, cancel := context.WithCancel(ctx)
	session := &Session{
		Agent:       spec.Agent,
		Cwd:         cwd,
		events:      make(chan Event, 512),
		permissions: make(chan *PermissionRequest, 32),
		done:        make(chan struct{}),
		cancel:      cancel,
	}
	session.Events = session.events

	cmd := exec.CommandContext(runCtx, command[0], command[1:]...)
	configureProcess(cmd)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	cmd.Env = append(cmd.Env, spec.Env...)
	if spec.Agent == "openclaw" {
		cmd.Env = append(cmd.Env, "OPENCLAW_HIDE_BANNER=1", "OPENCLAW_SUPPRESS_NOTES=1")
	}
	if spec.AgentLogs != nil {
		cmd.Stderr = spec.AgentLogs
	} else {
		cmd.Stderr = &session.stderr
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command[0], err)
	}
	session.cmd = cmd
	session.stdin = stdin

	waitCh := make(chan error, 1)
	session.waitCh = waitCh
	go func() {
		err := cmd.Wait()
		session.exitMu.Lock()
		session.exitErr = err
		session.exitMu.Unlock()
		select {
		case session.events <- Event{Type: EventExit, SessionID: session.ID, Err: err}:
		default:
		}
		close(session.done)
		waitCh <- err
	}()

	client := &sessionClient{session: session}
	conn := acp.NewClientSideConnection(client, stdin, stdout)
	conn.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	session.conn = conn

	initCtx, initCancel := context.WithTimeout(ctx, 60*time.Second)
	initResp, err := conn.Initialize(initCtx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
		ClientInfo: &acp.Implementation{
			Name:    "switch",
			Title:   acp.Ptr("Switch"),
			Version: buildversion.Version,
		},
	})
	initCancel()
	if err != nil {
		session.Close(context.Background())
		return nil, session.wrapError("initialize", err)
	}
	session.init = initResp

	state, err := openSession(ctx, conn, initResp, cwd, spec.SessionID)
	if err != nil {
		session.Close(context.Background())
		return nil, session.wrapError("session", err)
	}
	session.ID = string(state.ID)
	session.Caps = buildReport(spec.Agent, command, initResp)
	session.Caps.SessionID = session.ID
	session.Caps.Config = configOptions(state.ConfigOptions)
	if state.Modes != nil {
		for _, mode := range state.Modes.AvailableModes {
			session.Caps.Modes = append(session.Caps.Modes, ModeInfo{ID: string(mode.Id), Name: mode.Name})
		}
	}

	if spec.Model != "" {
		updated, err := setSessionModel(ctx, conn, state.ID, state.ConfigOptions, spec.Model)
		if err != nil {
			session.Close(context.Background())
			return nil, session.wrapError("set model", err)
		}
		session.Caps.Config = configOptions(updated)
	}
	session.Model = configValue(session.Caps.Config, "model")
	return session, nil
}

// Prompt sends a user message and returns when the turn ends. Events stream on
// Session.Events while it runs.
func (s *Session) Prompt(ctx context.Context, text string) (acp.PromptResponse, error) {
	resp, err := s.conn.Prompt(ctx, acp.PromptRequest{
		SessionId: acp.SessionId(s.ID),
		Prompt:    []acp.ContentBlock{acp.TextBlock(text)},
	})
	if err != nil {
		s.emit(Event{Type: EventError, SessionID: s.ID, Err: err})
		return resp, err
	}
	event := Event{Type: EventTurn, SessionID: s.ID, StopReason: string(resp.StopReason)}
	if resp.Usage != nil {
		event.Usage = &UsageInfo{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		}
	}
	s.emit(event)
	return resp, nil
}

func (s *Session) Cancel(ctx context.Context) error {
	return s.conn.Cancel(ctx, acp.CancelNotification{SessionId: acp.SessionId(s.ID)})
}

// Close ends the session and reaps the adapter process.
func (s *Session) Close(ctx context.Context) error {
	var err error
	s.closeOnce.Do(func() {
		closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = closeSession(closeCtx, s.conn, s.init, acp.SessionId(s.ID))
		cancel()

		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			select {
			case <-s.waitCh:
			case <-time.After(2 * time.Second):
				s.cancel()
				select {
				case <-s.waitCh:
				case <-time.After(2 * time.Second):
					_ = s.cmd.Process.Kill()
				}
			}
		}
	})
	return err
}

func (s *Session) Done() <-chan struct{} {
	return s.done
}

func (s *Session) Permissions() <-chan *PermissionRequest {
	return s.permissions
}

func (s *Session) Stderr() string {
	return strings.TrimSpace(s.stderr.String())
}

func (s *Session) ExitError() error {
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	return s.exitErr
}

func (s *Session) emit(event Event) {
	if event.SessionID == "" {
		event.SessionID = s.ID
	}
	select {
	case s.events <- event:
	case <-s.done:
	}
}

func (s *Session) wrapError(stage string, err error) error {
	message := formatACPError(stage, err)
	if stderr := s.Stderr(); stderr != "" {
		return fmt.Errorf("%w; agent stderr: %s", message, truncate(stderr, 2000))
	}
	return message
}

type sessionClient struct {
	session *Session
}

var _ acp.Client = (*sessionClient)(nil)

func (c *sessionClient) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	event, ok := normalizeUpdate(params)
	if !ok {
		return nil
	}
	c.session.emit(event)
	return nil
}

func (c *sessionClient) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	title := string(params.ToolCall.ToolCallId)
	if params.ToolCall.Title != nil && *params.ToolCall.Title != "" {
		title = *params.ToolCall.Title
	}
	request := &PermissionRequest{
		SessionID:  string(params.SessionId),
		ToolCallID: string(params.ToolCall.ToolCallId),
		Title:      title,
		response:   make(chan PermissionDecision, 1),
	}
	for _, option := range params.Options {
		request.Options = append(request.Options, PermissionOption{
			ID:   string(option.OptionId),
			Name: option.Name,
			Kind: string(option.Kind),
		})
	}

	select {
	case c.session.permissions <- request:
	case <-ctx.Done():
		return cancelledPermission(), nil
	case <-c.session.done:
		return cancelledPermission(), nil
	}

	select {
	case decision := <-request.response:
		if decision.Cancel || decision.OptionID == "" {
			return cancelledPermission(), nil
		}
		return acp.RequestPermissionResponse{
			Outcome: acp.NewRequestPermissionOutcomeSelected(acp.PermissionOptionId(decision.OptionID)),
		}, nil
	case <-ctx.Done():
		return cancelledPermission(), nil
	case <-c.session.done:
		return cancelledPermission(), nil
	}
}

func cancelledPermission() acp.RequestPermissionResponse {
	return acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeCancelled(),
	}
}

func (c *sessionClient) ReadTextFile(ctx context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound("fs/read_text_file")
}

func (c *sessionClient) WriteTextFile(ctx context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound("fs/write_text_file")
}

func (c *sessionClient) CreateTerminal(ctx context.Context, params acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound("terminal/create")
}

func (c *sessionClient) KillTerminal(ctx context.Context, params acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound("terminal/kill")
}

func (c *sessionClient) TerminalOutput(ctx context.Context, params acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound("terminal/output")
}

func (c *sessionClient) ReleaseTerminal(ctx context.Context, params acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound("terminal/release")
}

func (c *sessionClient) WaitForTerminalExit(ctx context.Context, params acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound("terminal/wait_for_exit")
}

func normalizeUpdate(params acp.SessionNotification) (Event, bool) {
	u := params.Update
	event := Event{SessionID: string(params.SessionId)}

	switch {
	case u.AgentMessageChunk != nil:
		event.Type = EventMessage
		event.Text = contentText(u.AgentMessageChunk.Content)
	case u.AgentThoughtChunk != nil:
		event.Type = EventThought
		event.Text = contentText(u.AgentThoughtChunk.Content)
	case u.ToolCall != nil:
		event.Type = EventToolCall
		event.Tool = &ToolInfo{
			ID:     string(u.ToolCall.ToolCallId),
			Title:  u.ToolCall.Title,
			Status: string(u.ToolCall.Status),
			Kind:   string(u.ToolCall.Kind),
		}
	case u.ToolCallUpdate != nil:
		event.Type = EventToolEdit
		tool := &ToolInfo{ID: string(u.ToolCallUpdate.ToolCallId)}
		if u.ToolCallUpdate.Title != nil {
			tool.Title = *u.ToolCallUpdate.Title
		}
		if u.ToolCallUpdate.Status != nil {
			tool.Status = string(*u.ToolCallUpdate.Status)
		}
		event.Tool = tool
	case u.Plan != nil:
		event.Type = EventPlan
		for _, entry := range u.Plan.Entries {
			event.Plan = append(event.Plan, PlanEntry{
				Content:  entry.Content,
				Status:   string(entry.Status),
				Priority: string(entry.Priority),
			})
		}
	case u.UsageUpdate != nil:
		event.Type = EventUsage
		event.Usage = &UsageInfo{
			Used: u.UsageUpdate.Used,
			Size: u.UsageUpdate.Size,
		}
		if u.UsageUpdate.Cost != nil {
			event.Usage.CostAmount = u.UsageUpdate.Cost.Amount
			event.Usage.CostCurrency = u.UsageUpdate.Cost.Currency
		}
	case u.CurrentModeUpdate != nil:
		event.Type = EventMode
		event.Text = string(u.CurrentModeUpdate.CurrentModeId)
	case u.ConfigOptionUpdate != nil:
		event.Type = EventConfig
		event.Raw = u.ConfigOptionUpdate.ConfigOptions
	case u.SessionInfoUpdate != nil:
		event.Type = EventInfo
		if u.SessionInfoUpdate.Title != nil {
			event.Text = *u.SessionInfoUpdate.Title
		}
	case u.AvailableCommandsUpdate != nil:
		event.Type = EventInfo
		event.Raw = u.AvailableCommandsUpdate.AvailableCommands
	default:
		return Event{}, false
	}
	return event, true
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
