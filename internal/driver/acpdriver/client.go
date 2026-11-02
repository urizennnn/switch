package acpdriver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

// PermissionMode controls how the probe answers ACP permission requests.
type PermissionMode string

const (
	PermissionAsk   PermissionMode = "ask"
	PermissionAllow PermissionMode = "allow"
	PermissionDeny  PermissionMode = "deny"
)

// ParsePermissionMode validates a permission flag value.
func ParsePermissionMode(value string) (PermissionMode, error) {
	switch PermissionMode(strings.ToLower(strings.TrimSpace(value))) {
	case PermissionAsk:
		return PermissionAsk, nil
	case PermissionAllow:
		return PermissionAllow, nil
	case PermissionDeny:
		return PermissionDeny, nil
	default:
		return "", fmt.Errorf("invalid permission mode %q (want ask, allow, or deny)", value)
	}
}

// probeClient implements acp.Client for the Phase 0 probe.
//
// It is intentionally simple: it prints streamed updates and answers permission
// requests according to the selected mode. The production driver will replace
// the printing with an event channel consumed by the run actor and TUI.
type probeClient struct {
	out        io.Writer
	json       bool
	verbose    bool
	permission PermissionMode
	stdin      *bufio.Reader
	isTTY      bool

	mu sync.Mutex
}

var _ acp.Client = (*probeClient)(nil)

func newProbeClient(out io.Writer, stdin io.Reader, jsonOut, verbose bool, permission PermissionMode) *probeClient {
	client := &probeClient{
		out:        out,
		json:       jsonOut,
		verbose:    verbose,
		permission: permission,
		stdin:      bufio.NewReader(stdin),
	}
	if f, ok := stdin.(*os.File); ok {
		if info, err := f.Stat(); err == nil {
			client.isTTY = info.Mode()&os.ModeCharDevice != 0
		}
	}
	return client
}

func (c *probeClient) emitValue(value any) {
	if !c.json {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = json.NewEncoder(c.out).Encode(value)
}

func (c *probeClient) emit(event map[string]any) {
	c.emitValue(event)
}

func (c *probeClient) emitSession(sessionID acp.SessionId, event map[string]any) {
	event["sessionId"] = string(sessionID)
	c.emit(event)
}

func (c *probeClient) human(format string, args ...any) {
	if c.json {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.out, format, args...)
}

func (c *probeClient) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	u := params.Update

	switch {
	case u.AgentMessageChunk != nil:
		text := contentText(u.AgentMessageChunk.Content)
		c.emitSession(params.SessionId, map[string]any{"type": "agent_message", "text": text})
		c.human("%s", text)

	case u.AgentThoughtChunk != nil:
		text := contentText(u.AgentThoughtChunk.Content)
		c.emitSession(params.SessionId, map[string]any{"type": "agent_thought", "text": text})
		if c.verbose {
			c.human("\n[thinking] %s\n", text)
		}

	case u.ToolCall != nil:
		c.emitSession(params.SessionId, map[string]any{
			"type":   "tool_call",
			"id":     string(u.ToolCall.ToolCallId),
			"title":  u.ToolCall.Title,
			"status": string(u.ToolCall.Status),
			"kind":   string(u.ToolCall.Kind),
		})
		c.human("\n[tool] %s (%s)\n", u.ToolCall.Title, u.ToolCall.Status)

	case u.ToolCallUpdate != nil:
		status := ""
		if u.ToolCallUpdate.Status != nil {
			status = string(*u.ToolCallUpdate.Status)
		}
		title := ""
		if u.ToolCallUpdate.Title != nil {
			title = *u.ToolCallUpdate.Title
		}
		c.emitSession(params.SessionId, map[string]any{
			"type":   "tool_call_update",
			"id":     string(u.ToolCallUpdate.ToolCallId),
			"title":  title,
			"status": status,
		})
		if status != "" && c.verbose {
			label := title
			if label == "" {
				label = string(u.ToolCallUpdate.ToolCallId)
			}
			c.human("[tool] %s: %s\n", label, status)
		}

	case u.Plan != nil:
		entries := make([]map[string]any, 0, len(u.Plan.Entries))
		for _, entry := range u.Plan.Entries {
			entries = append(entries, map[string]any{
				"content":  entry.Content,
				"status":   string(entry.Status),
				"priority": string(entry.Priority),
			})
		}
		c.emitSession(params.SessionId, map[string]any{"type": "plan", "entries": entries})
		if c.verbose {
			c.human("\n[plan]\n")
			for _, entry := range u.Plan.Entries {
				c.human("  - [%s] %s\n", entry.Status, entry.Content)
			}
		}

	case u.UsageUpdate != nil:
		event := map[string]any{
			"type": "usage",
			"used": u.UsageUpdate.Used,
			"size": u.UsageUpdate.Size,
		}
		if u.UsageUpdate.Cost != nil {
			event["cost"] = map[string]any{
				"amount":   u.UsageUpdate.Cost.Amount,
				"currency": u.UsageUpdate.Cost.Currency,
			}
		}
		c.emitSession(params.SessionId, event)

	case u.CurrentModeUpdate != nil:
		c.emitSession(params.SessionId, map[string]any{"type": "mode", "mode": string(u.CurrentModeUpdate.CurrentModeId)})

	case u.ConfigOptionUpdate != nil:
		c.emitSession(params.SessionId, map[string]any{"type": "config_options", "options": configOptions(u.ConfigOptionUpdate.ConfigOptions)})

	case u.SessionInfoUpdate != nil:
		title := ""
		if u.SessionInfoUpdate.Title != nil {
			title = *u.SessionInfoUpdate.Title
		}
		c.emitSession(params.SessionId, map[string]any{"type": "session_info", "title": title})

	case u.AvailableCommandsUpdate != nil:
		if !c.verbose {
			return nil
		}
		commands := make([]map[string]any, 0, len(u.AvailableCommandsUpdate.AvailableCommands))
		for _, command := range u.AvailableCommandsUpdate.AvailableCommands {
			commands = append(commands, map[string]any{
				"name":        command.Name,
				"description": command.Description,
			})
		}
		c.emitSession(params.SessionId, map[string]any{"type": "available_commands", "commands": commands})

	case u.UserMessageChunk != nil:
		// The probe already knows what it sent; ignore the echo.

	default:
		c.emitSession(params.SessionId, map[string]any{"type": "unknown_update"})
	}

	return nil
}

func (c *probeClient) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	title := string(params.ToolCall.ToolCallId)
	if params.ToolCall.Title != nil && *params.ToolCall.Title != "" {
		title = *params.ToolCall.Title
	}

	options := make([]map[string]any, 0, len(params.Options))
	for _, option := range params.Options {
		options = append(options, map[string]any{
			"id":   string(option.OptionId),
			"name": option.Name,
			"kind": string(option.Kind),
		})
	}

	c.emitSession(params.SessionId, map[string]any{
		"type":    "permission",
		"title":   title,
		"options": options,
		"tool":    string(params.ToolCall.ToolCallId),
	})

	option, decision, ok := c.choosePermission(title, params.Options)
	c.emitSession(params.SessionId, map[string]any{
		"type":     "permission_decision",
		"tool":     string(params.ToolCall.ToolCallId),
		"decision": decision,
	})
	if !ok {
		return acp.RequestPermissionResponse{
			Outcome: acp.NewRequestPermissionOutcomeCancelled(),
		}, nil
	}
	return selected(option), nil
}

// choosePermission returns the exact option to select, a human-readable
// decision label, and whether an option was selected. A false third return
// means the request should be cancelled.
func (c *probeClient) choosePermission(title string, options []acp.PermissionOption) (acp.PermissionOption, string, bool) {
	switch c.permission {
	case PermissionAllow:
		return firstOption(options, acp.PermissionOptionKindAllowOnce,
			acp.PermissionOptionKindAllowAlways)
	case PermissionDeny:
		return firstOption(options, acp.PermissionOptionKindRejectOnce,
			acp.PermissionOptionKindRejectAlways)
	}

	if !c.isTTY {
		c.human("\n[permission] %s: no TTY, denying\n", title)
		return firstOption(options, acp.PermissionOptionKindRejectOnce,
			acp.PermissionOptionKindRejectAlways)
	}

	c.mu.Lock()
	fmt.Fprintf(c.out, "\n[permission] %s\n", title)
	for i, option := range options {
		fmt.Fprintf(c.out, "  %d. %s (%s)\n", i+1, option.Name, option.Kind)
	}
	fmt.Fprintf(c.out, "Choose an option [1-%d, empty = deny]: ", len(options))
	c.mu.Unlock()

	line, err := c.stdin.ReadString('\n')
	if err != nil {
		return firstOption(options, acp.PermissionOptionKindRejectOnce,
			acp.PermissionOptionKindRejectAlways)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return firstOption(options, acp.PermissionOptionKindRejectOnce,
			acp.PermissionOptionKindRejectAlways)
	}

	var index int
	if _, err := fmt.Sscanf(line, "%d", &index); err != nil {
		return firstOption(options, acp.PermissionOptionKindRejectOnce,
			acp.PermissionOptionKindRejectAlways)
	}
	index--
	if index < 0 || index >= len(options) {
		return firstOption(options, acp.PermissionOptionKindRejectOnce,
			acp.PermissionOptionKindRejectAlways)
	}
	return options[index], string(options[index].Kind), true
}

func firstOption(options []acp.PermissionOption, kinds ...acp.PermissionOptionKind) (acp.PermissionOption, string, bool) {
	for _, kind := range kinds {
		if option, ok := findOption(options, kind); ok {
			return option, string(kind), true
		}
	}
	for _, option := range options {
		if strings.HasPrefix(string(option.Kind), "allow") && len(kinds) > 0 && strings.HasPrefix(string(kinds[0]), "allow") {
			return option, string(option.Kind), true
		}
		if strings.HasPrefix(string(option.Kind), "reject") && len(kinds) > 0 && strings.HasPrefix(string(kinds[0]), "reject") {
			return option, string(option.Kind), true
		}
	}
	return acp.PermissionOption{}, "cancel", false
}

func selected(option acp.PermissionOption) acp.RequestPermissionResponse {
	return acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeSelected(option.OptionId),
	}
}

func findOption(options []acp.PermissionOption, kind acp.PermissionOptionKind) (acp.PermissionOption, bool) {
	for _, option := range options {
		if option.Kind == kind {
			return option, true
		}
	}
	return acp.PermissionOption{}, false
}

func contentText(content acp.ContentBlock) string {
	if content.Text != nil {
		return content.Text.Text
	}
	return ""
}

// The probe advertises fs and terminal capabilities as disabled. These methods
// are still required by acp.Client; they fail clearly if an adapter calls them
// anyway.

func (c *probeClient) ReadTextFile(ctx context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound("fs/read_text_file")
}

func (c *probeClient) WriteTextFile(ctx context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound("fs/write_text_file")
}

func (c *probeClient) CreateTerminal(ctx context.Context, params acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound("terminal/create")
}

func (c *probeClient) KillTerminal(ctx context.Context, params acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound("terminal/kill")
}

func (c *probeClient) TerminalOutput(ctx context.Context, params acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound("terminal/output")
}

func (c *probeClient) ReleaseTerminal(ctx context.Context, params acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound("terminal/release")
}

func (c *probeClient) WaitForTerminalExit(ctx context.Context, params acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound("terminal/wait_for_exit")
}
