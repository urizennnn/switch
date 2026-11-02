package acpdriver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
	buildversion "github.com/urizennnn/switch/internal/version"
)

// ProbeOptions configures a single ACP probe run.
type ProbeOptions struct {
	Agent      string
	Command    []string
	Cwd        string
	Prompt     string
	Model      string
	SessionID  string
	Timeout    time.Duration
	Permission PermissionMode
	JSON       bool
	Verbose    bool
	AgentLogs  bool
	NoPrompt   bool

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// CapabilitiesReport is the normalized capability summary printed by probe.
type CapabilitiesReport struct {
	Type        string       `json:"type"`
	Agent       string       `json:"agent"`
	DisplayName string       `json:"displayName,omitempty"`
	Version     string       `json:"version,omitempty"`
	Command     []string     `json:"command"`
	Protocol    int          `json:"protocol"`
	LoadSession bool         `json:"loadSession"`
	Session     SessionCaps  `json:"session"`
	MCP         MCPCaps      `json:"mcp"`
	Prompt      PromptCaps   `json:"prompt"`
	AuthMethods []AuthMethod `json:"authMethods"`
	SessionID   string       `json:"sessionId,omitempty"`
	Config      []ConfigInfo `json:"configOptions,omitempty"`
	Modes       []ModeInfo   `json:"modes,omitempty"`
}

type SessionCaps struct {
	Resume bool `json:"resume"`
	Close  bool `json:"close"`
	List   bool `json:"list"`
	Fork   bool `json:"fork"`
	Delete bool `json:"delete"`
}

type MCPCaps struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

type PromptCaps struct {
	Image           bool `json:"image"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

type ConfigInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Current string   `json:"current"`
	Values  []string `json:"values,omitempty"`
}

type ModeInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Probe launches one ACP agent, reports its capabilities, optionally runs a
// prompt, and optionally resumes an existing session. It is the Phase 0
// end-to-end check for the driver layer.
func Probe(ctx context.Context, opts ProbeOptions) error {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.Permission == "" {
		opts.Permission = PermissionAsk
	}
	if opts.Agent == "" {
		opts.Agent = "opencode"
	}
	opts.Agent = strings.ToLower(strings.TrimSpace(opts.Agent))

	cwd, err := filepath.Abs(opts.Cwd)
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return fmt.Errorf("cwd %s: %w", cwd, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cwd %s is not a directory", cwd)
	}

	command := opts.Command
	if len(command) == 0 {
		adapter, err := ResolveAdapter(opts.Agent)
		if err != nil {
			return err
		}
		command = adapter.Command
	}
	if len(command) == 0 {
		return errors.New("no adapter command configured")
	}

	client := newProbeClient(opts.Stdout, opts.Stdin, opts.JSON, opts.Verbose, opts.Permission)

	if !opts.JSON {
		fmt.Fprintf(opts.Stdout, "agent:     %s\n", opts.Agent)
		fmt.Fprintf(opts.Stdout, "command:   %s\n", strings.Join(command, " "))
		fmt.Fprintf(opts.Stdout, "cwd:       %s\n\n", cwd)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(runCtx, command[0], command[1:]...)
	configureProcess(cmd)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	if opts.Agent == "openclaw" {
		cmd.Env = append(cmd.Env, "OPENCLAW_HIDE_BANNER=1", "OPENCLAW_SUPPRESS_NOTES=1")
	}
	var agentStderr bytes.Buffer
	if opts.AgentLogs {
		cmd.Stderr = opts.Stderr
	} else {
		cmd.Stderr = &agentStderr
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", command[0], err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	stop := func() {
		stopProcess(cmd, stdin, waitCh, cancel)
	}

	conn := acp.NewClientSideConnection(client, stdin, stdout)
	conn.SetLogger(slog.New(slog.NewTextHandler(opts.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

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
		stop()
		dumpAgentStderr(opts, &agentStderr)
		return formatACPError("initialize", err)
	}

	report := buildReport(opts.Agent, command, initResp)
	if opts.JSON {
		client.emitValue(report)
	} else {
		printReport(opts.Stdout, report)
	}

	session, err := openSession(ctx, conn, initResp, cwd, opts.SessionID)
	if err != nil {
		_ = closeSession(ctx, conn, initResp, "")
		stop()
		dumpAgentStderr(opts, &agentStderr)
		return formatACPError("session", err)
	}
	report.SessionID = string(session.ID)
	report.Config = configOptions(session.ConfigOptions)
	if session.Modes != nil {
		for _, mode := range session.Modes.AvailableModes {
			report.Modes = append(report.Modes, ModeInfo{ID: string(mode.Id), Name: mode.Name})
		}
	}

	if opts.Model != "" {
		updated, err := setSessionModel(ctx, conn, session.ID, session.ConfigOptions, opts.Model)
		if err != nil {
			_ = closeSession(ctx, conn, initResp, session.ID)
			stop()
			dumpAgentStderr(opts, &agentStderr)
			return err
		}
		session.ConfigOptions = updated
		report.Config = configOptions(updated)
	}

	client.emit(map[string]any{
		"type":          "session",
		"id":            string(session.ID),
		"resumed":       session.Resumed,
		"configOptions": report.Config,
		"modes":         report.Modes,
	})
	if !opts.JSON {
		state := "created"
		if session.Resumed {
			state = "resumed"
		}
		fmt.Fprintf(opts.Stdout, "\nsession:   %s (%s)\n", session.ID, state)
		if model := configValue(report.Config, "model"); model != "" {
			fmt.Fprintf(opts.Stdout, "model:     %s\n", model)
		}
		if len(report.Modes) > 0 {
			modes := make([]string, 0, len(report.Modes))
			for _, mode := range report.Modes {
				modes = append(modes, mode.ID)
			}
			fmt.Fprintf(opts.Stdout, "modes:     %s\n", strings.Join(modes, " "))
		}
	}

	if opts.NoPrompt {
		_ = closeSession(ctx, conn, initResp, session.ID)
		stop()
		return nil
	}

	promptCtx, promptCancel := context.WithTimeout(ctx, opts.Timeout)
	turn, err := conn.Prompt(promptCtx, acp.PromptRequest{
		SessionId: session.ID,
		Prompt:    []acp.ContentBlock{acp.TextBlock(opts.Prompt)},
	})
	promptCancel()
	if err != nil {
		_ = closeSession(ctx, conn, initResp, session.ID)
		stop()
		dumpAgentStderr(opts, &agentStderr)
		if isAuthError(err) && len(report.AuthMethods) > 0 {
			fmt.Fprintf(opts.Stderr, "\nauthentication may be required. Available methods:\n")
			for _, method := range report.AuthMethods {
				fmt.Fprintf(opts.Stderr, "  - %s: %s (%s)\n", method.ID, method.Name, method.Type)
			}
			if hint := authHint(opts.Agent); hint != "" {
				fmt.Fprintf(opts.Stderr, "hint: %s\n", hint)
			}
		}
		if hint := promptErrorHint(report, err); hint != "" {
			fmt.Fprintf(opts.Stderr, "hint: %s\n", hint)
		}
		return formatACPError("prompt", err)
	}

	event := map[string]any{
		"type":       "turn",
		"sessionId":  string(session.ID),
		"stopReason": string(turn.StopReason),
	}
	if turn.Usage != nil {
		event["usage"] = map[string]any{
			"inputTokens":  turn.Usage.InputTokens,
			"outputTokens": turn.Usage.OutputTokens,
			"totalTokens":  turn.Usage.TotalTokens,
		}
	}
	client.emit(event)
	if !opts.JSON {
		fmt.Fprintf(opts.Stdout, "\n\nturn:      %s", turn.StopReason)
		if turn.Usage != nil {
			fmt.Fprintf(opts.Stdout, " (%d tokens)", turn.Usage.TotalTokens)
		}
		fmt.Fprintln(opts.Stdout)
	}

	_ = closeSession(ctx, conn, initResp, session.ID)
	stop()
	return nil
}

type sessionState struct {
	ID            acp.SessionId
	Resumed       bool
	ConfigOptions []acp.SessionConfigOption
	Modes         *acp.SessionModeState
}

func openSession(ctx context.Context, conn *acp.ClientSideConnection, init acp.InitializeResponse, cwd, sessionID string) (sessionState, error) {
	if sessionID != "" {
		switch {
		case init.AgentCapabilities.SessionCapabilities.Resume != nil:
			resp, err := conn.ResumeSession(ctx, acp.ResumeSessionRequest{
				SessionId:  acp.SessionId(sessionID),
				Cwd:        cwd,
				McpServers: []acp.McpServer{},
			})
			if err != nil {
				return sessionState{}, err
			}
			if err := resp.Validate(); err != nil {
				return sessionState{}, err
			}
			return sessionState{
				ID:            acp.SessionId(sessionID),
				Resumed:       true,
				ConfigOptions: resp.ConfigOptions,
				Modes:         resp.Modes,
			}, nil

		case init.AgentCapabilities.LoadSession:
			resp, err := conn.LoadSession(ctx, acp.LoadSessionRequest{
				SessionId:  acp.SessionId(sessionID),
				Cwd:        cwd,
				McpServers: []acp.McpServer{},
			})
			if err != nil {
				return sessionState{}, err
			}
			if err := resp.Validate(); err != nil {
				return sessionState{}, err
			}
			return sessionState{
				ID:            acp.SessionId(sessionID),
				Resumed:       true,
				ConfigOptions: resp.ConfigOptions,
				Modes:         resp.Modes,
			}, nil

		default:
			return sessionState{}, fmt.Errorf("adapter does not advertise session resume/load support")
		}
	}

	resp, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		return sessionState{}, err
	}
	return sessionState{
		ID:            resp.SessionId,
		ConfigOptions: resp.ConfigOptions,
		Modes:         resp.Modes,
	}, nil
}

func setSessionModel(ctx context.Context, conn *acp.ClientSideConnection, sessionID acp.SessionId, options []acp.SessionConfigOption, model string) ([]acp.SessionConfigOption, error) {
	var target *acp.SessionConfigOptionSelect
	for i := range options {
		option := options[i].Select
		if option == nil {
			continue
		}
		if string(option.Id) == "model" || strings.EqualFold(option.Name, "model") {
			target = option
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("adapter does not expose a model config option")
	}

	if values := selectValues(target.Options); len(values) > 0 && !contains(values, model) {
		return nil, fmt.Errorf("unknown model %q; available models: %s", model, strings.Join(values, ", "))
	}

	resp, err := conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: sessionID,
			ConfigId:  target.Id,
			Value:     acp.SessionConfigValueId(model),
		},
	})
	if err != nil {
		return nil, formatACPError("set model", err)
	}
	if err := resp.Validate(); err != nil {
		return nil, fmt.Errorf("set model: %w", err)
	}
	return resp.ConfigOptions, nil
}

func selectValues(options acp.SessionConfigSelectOptions) []string {
	var values []string
	if options.Ungrouped != nil {
		for _, option := range *options.Ungrouped {
			values = append(values, string(option.Value))
		}
	}
	if options.Grouped != nil {
		for _, group := range *options.Grouped {
			for _, option := range group.Options {
				values = append(values, string(option.Value))
			}
		}
	}
	return values
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func configValue(options []ConfigInfo, id string) string {
	for _, option := range options {
		if option.ID == id {
			return option.Current
		}
	}
	return ""
}

func closeSession(ctx context.Context, conn *acp.ClientSideConnection, init acp.InitializeResponse, sessionID acp.SessionId) error {
	if sessionID == "" || init.AgentCapabilities.SessionCapabilities.Close == nil {
		return nil
	}
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := conn.CloseSession(closeCtx, acp.CloseSessionRequest{SessionId: sessionID})
	return err
}

func stopProcess(cmd *exec.Cmd, stdin io.WriteCloser, waitCh <-chan error, cancel context.CancelFunc) {
	_ = stdin.Close()

	select {
	case <-waitCh:
		return
	case <-time.After(2 * time.Second):
	}

	cancel()

	select {
	case <-waitCh:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-waitCh
	}
}

func buildReport(agent string, command []string, init acp.InitializeResponse) CapabilitiesReport {
	report := CapabilitiesReport{
		Type:        "capabilities",
		Agent:       agent,
		Command:     command,
		Protocol:    int(init.ProtocolVersion),
		LoadSession: init.AgentCapabilities.LoadSession,
		Session: SessionCaps{
			Resume: init.AgentCapabilities.SessionCapabilities.Resume != nil,
			Close:  init.AgentCapabilities.SessionCapabilities.Close != nil,
			List:   init.AgentCapabilities.SessionCapabilities.List != nil,
			Fork:   init.AgentCapabilities.SessionCapabilities.Fork != nil,
			Delete: init.AgentCapabilities.SessionCapabilities.Delete != nil,
		},
		MCP: MCPCaps{
			HTTP: init.AgentCapabilities.McpCapabilities.Http,
			SSE:  init.AgentCapabilities.McpCapabilities.Sse,
		},
		Prompt: PromptCaps{
			Image:           init.AgentCapabilities.PromptCapabilities.Image,
			EmbeddedContext: init.AgentCapabilities.PromptCapabilities.EmbeddedContext,
		},
	}
	if init.AgentInfo != nil {
		report.DisplayName = init.AgentInfo.Name
		if init.AgentInfo.Title != nil {
			report.DisplayName = *init.AgentInfo.Title
		}
		report.Version = init.AgentInfo.Version
	}
	for _, method := range init.AuthMethods {
		info := AuthMethod{}
		switch {
		case method.Agent != nil:
			info.ID = method.Agent.Id
			info.Name = method.Agent.Name
			info.Type = "agent"
			if method.Agent.Description != nil {
				info.Description = *method.Agent.Description
			}
		case method.Terminal != nil:
			info.ID = method.Terminal.Id
			info.Name = method.Terminal.Name
			info.Type = "terminal"
			if method.Terminal.Description != nil {
				info.Description = *method.Terminal.Description
			}
		case method.EnvVar != nil:
			info.ID = method.EnvVar.Id
			info.Name = method.EnvVar.Name
			info.Type = "env"
			if method.EnvVar.Description != nil {
				info.Description = *method.EnvVar.Description
			}
		}
		report.AuthMethods = append(report.AuthMethods, info)
	}
	return report
}

func printReport(w io.Writer, report CapabilitiesReport) {
	name := report.DisplayName
	if name == "" {
		name = report.Agent
	}
	fmt.Fprintf(w, "connected: %s %s\n", name, report.Version)
	fmt.Fprintf(w, "protocol:  %d\n", report.Protocol)
	fmt.Fprintf(w, "load:      %v\n", report.LoadSession)

	var sessions []string
	if report.Session.Resume {
		sessions = append(sessions, "resume")
	}
	if report.Session.Close {
		sessions = append(sessions, "close")
	}
	if report.Session.List {
		sessions = append(sessions, "list")
	}
	if report.Session.Fork {
		sessions = append(sessions, "fork")
	}
	if report.Session.Delete {
		sessions = append(sessions, "delete")
	}
	if len(sessions) > 0 {
		fmt.Fprintf(w, "sessions:  %s\n", strings.Join(sessions, " "))
	}

	fmt.Fprintf(w, "mcp:       http=%v sse=%v\n", report.MCP.HTTP, report.MCP.SSE)
	fmt.Fprintf(w, "prompt:    image=%v embedded=%v\n", report.Prompt.Image, report.Prompt.EmbeddedContext)

	if len(report.AuthMethods) == 0 {
		fmt.Fprintln(w, "auth:      none advertised")
	} else {
		parts := make([]string, 0, len(report.AuthMethods))
		for _, method := range report.AuthMethods {
			parts = append(parts, fmt.Sprintf("%s (%s)", method.ID, method.Type))
		}
		fmt.Fprintf(w, "auth:      %s\n", strings.Join(parts, ", "))
	}
}

func dumpAgentStderr(opts ProbeOptions, buf *bytes.Buffer) {
	if opts.AgentLogs || buf.Len() == 0 {
		return
	}
	text := strings.TrimSpace(buf.String())
	if text == "" {
		return
	}
	fmt.Fprintf(opts.Stderr, "\n--- agent stderr ---\n%s\n", text)
}

func formatACPError(stage string, err error) error {
	var requestErr *acp.RequestError
	if errors.As(err, &requestErr) {
		return fmt.Errorf("%s: ACP error %d: %s", stage, requestErr.Code, requestErr.Message)
	}
	return fmt.Errorf("%s: %w", stage, err)
}

func promptErrorHint(report CapabilitiesReport, err error) string {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "trains on request data") || strings.Contains(message, "privacy") {
		current := configValue(report.Config, "model")
		if current == "" {
			current = "unknown"
		}
		return fmt.Sprintf("the selected model is blocked by provider privacy settings (current: %s); choose another with --model", current)
	}
	return ""
}

func isAuthError(err error) bool {
	var requestErr *acp.RequestError
	if !errors.As(err, &requestErr) {
		return false
	}
	return requestErr.Code == -32000
}

func authHint(agent string) string {
	adapter, err := ResolveAdapter(agent)
	if err != nil {
		return ""
	}
	return adapter.AuthHint
}

func configOptions(options []acp.SessionConfigOption) []ConfigInfo {
	result := make([]ConfigInfo, 0, len(options))
	for _, option := range options {
		switch {
		case option.Select != nil:
			info := ConfigInfo{
				ID:      string(option.Select.Id),
				Name:    option.Select.Name,
				Current: string(option.Select.CurrentValue),
			}
			if option.Select.Options.Ungrouped != nil {
				for _, value := range *option.Select.Options.Ungrouped {
					info.Values = append(info.Values, string(value.Value))
				}
			}
			if option.Select.Options.Grouped != nil {
				for _, group := range *option.Select.Options.Grouped {
					for _, value := range group.Options {
						info.Values = append(info.Values, string(value.Value))
					}
				}
			}
			result = append(result, info)
		case option.Boolean != nil:
			result = append(result, ConfigInfo{
				ID:      string(option.Boolean.Id),
				Name:    option.Boolean.Name,
				Current: fmt.Sprintf("%v", option.Boolean.CurrentValue),
			})
		}
	}
	return result
}
