package control

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"

	buildversion "github.com/urizennnn/switch/internal/version"
)

// The MCP bridge speaks newline-delimited JSON-RPC 2.0 on stdio and forwards
// tool calls to the control socket, so agents report through the same
// handlers and authorization as the agent CLI.

const mcpProtocolVersion = "2025-06-18"

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	method      string
}

func messageSchema(description string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"message": map[string]any{"type": "string", "description": description}},
		"required":   []string{"message"},
	}
}

var tools = []tool{
	{Name: "switch_team", method: "team", Description: "List the agents on your Switch team: name, role, driver, and what each is doing.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
	{Name: "switch_ask", method: "ask", Description: "Ask a teammate on your Switch team a question and wait for their answer. Use it to get a second opinion or knowledge from another agent; they do not edit files. If they are still answering, it says so: call it again with the same arguments to get the answer.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":      map[string]any{"type": "string", "description": "the teammate's name, from switch_team"},
				"message": map[string]any{"type": "string", "description": "the question or request"},
			},
			"required": []string{"to", "message"},
		}},
	{Name: "switch_task", method: "task", Description: "Show your current Switch task: brief, acceptance criteria, verification commands, and feedback from earlier attempts.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
	{Name: "switch_progress", method: "progress", Description: "Report progress to the user watching the Switch run.",
		InputSchema: messageSchema("what you just did or are about to do")},
	{Name: "switch_complete", method: "complete", Description: "Report that your task is done, with a summary. Switch still runs verification.",
		InputSchema: messageSchema("summary of the change")},
	{Name: "switch_block", method: "block", Description: "Report that you cannot complete the task. The task is handed to the user instead of being verified.",
		InputSchema: messageSchema("why the task cannot be done and what is needed")},
	{Name: "switch_question", method: "question", Description: "Ask the user a question you cannot settle yourself, such as a choice between approaches, and wait for the answer without ending your turn. If they have not answered yet, it says so: call it again with the same arguments to keep waiting. When no user is watching, it says so: decide yourself, or switch_block.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string", "description": "the question, with what the user needs to answer it"},
				"options": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "up to 9 answers to pick from; omit to let the user type one"},
			},
			"required": []string{"message"},
		}},
	{Name: "switch_spawn", method: "spawn", Description: "Workers only: start a subagent for part of your task. It works in your worktree, reports only to you, and runs until it finishes; collect its result with switch_wait. You are responsible for checking its work.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":   map[string]any{"type": "string", "description": "a short name, such as scout or tests"},
				"brief":  map[string]any{"type": "string", "description": "what it must do and what result to report back"},
				"files":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "files or directories it may edit, relative to the worktree; omit for a read-only subagent"},
				"driver": map[string]any{"type": "string", "description": "the ACP adapter to run it on; defaults to yours"},
				"model":  map[string]any{"type": "string", "description": "the model; defaults to yours when the adapter is the same"},
			},
			"required": []string{"name", "brief"},
		}},
	{Name: "switch_wait", method: "wait", Description: "Workers only: wait for your subagents to finish (all of them, or the named ones) and get their results. It returns after a while with any still running; call it again to keep waiting.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"names": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "subagents to wait for; omit for all"}},
		}},
	{Name: "switch_message", method: "message", Description: "Workers only: send a follow-up to one of your subagents. A running one gets it when its current turn ends; a finished one starts again.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":    map[string]any{"type": "string"},
				"message": map[string]any{"type": "string"},
			},
			"required": []string{"name", "message"},
		}},
	{Name: "switch_stop", method: "stop", Description: "Workers only: stop one of your subagents, or all of them when name is omitted.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}},
	{Name: "switch_subagents", method: "subagents", Description: "Workers only: list your subagents with their status and results.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
	{Name: "switch_report", method: "report", Description: "Subagents only: give your result to the worker that started you, then end your turn.",
		InputSchema: messageSchema("your result: what you found or changed")},
	{Name: "switch_limits", method: "limits", Description: "Show how full your context window is, the share at which you should hand off, and who you hand off to.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
	{Name: "switch_handoff", method: "handoff", Description: "Hand your work to another agent before you run out of context or hit a usage limit. In a run, your task continues with that agent in the same worktree; in the chat, the lead role passes to them. End your turn after calling it.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string", "description": "the handoff note: what is done, what is left, where you were, and anything the next agent must know"},
				"to":      map[string]any{"type": "string", "description": "the agent to hand off to; defaults to your configured handoff (the lead when none)"},
			},
			"required": []string{"message"},
		}},
	{Name: "switch_delegate", method: "delegate", Description: "Lead only, during a run: propose a task for the run. It stays blocked until the user accepts it with /retry <id>.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":      map[string]any{"type": "string"},
				"brief":      map[string]any{"type": "string"},
				"role":       map[string]any{"type": "string", "description": "a role that a worker in this run has (switch_team lists them); never lead"},
				"deps":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"acceptance": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "observable completion criteria"},
				"verify":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []string{"title", "brief", "role", "acceptance"},
		}},
	{Name: "switch_tasks", method: "tasks", Description: "Lead only, during a run: list the run's tasks with status, assignee, attempts, the first line of the last error, and what each unfinished task waits for.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
	{Name: "switch_steer", method: "steer", Description: "Lead only, in the chat during a run: send a course correction to a worker's or teammate's running turn. An agent whose adapter cannot take it mid-turn gets it at the start of its next prompt; the result says which.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":      map[string]any{"type": "string", "description": "the agent, from switch_tasks or switch_team"},
				"message": map[string]any{"type": "string", "description": "what it should do differently"},
			},
			"required": []string{"to", "message"},
		}},
}

// RunMCP serves the bridge until stdin closes.
func RunMCP(stdin io.Reader, stdout io.Writer) int {
	return serveMCP(stdin, stdout, os.Getenv(EnvSocket), os.Getenv(EnvToken))
}

func serveMCP(stdin io.Reader, stdout io.Writer, socket, token string) int {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	encoder := json.NewEncoder(stdout)
	var (
		writing sync.Mutex // one reply at a time; guards failed
		failed  bool
		mu      sync.Mutex                        // guards calls
		calls   = map[string]context.CancelFunc{} // requests in flight, by ID
		wg      sync.WaitGroup
	)
	reply := func(msg rpcMessage) {
		writing.Lock()
		defer writing.Unlock()
		if encoder.Encode(msg) != nil {
			failed = true
		}
	}
	for scanner.Scan() {
		var msg rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			reply(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if len(msg.ID) == 0 { // a notification, such as notifications/initialized
			if msg.Method == "notifications/cancelled" {
				var params struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				_ = json.Unmarshal(msg.Params, &params)
				mu.Lock()
				if cancel := calls[string(params.RequestID)]; cancel != nil {
					cancel()
				}
				mu.Unlock()
			}
			continue
		}
		// Each request runs on its own, so a switch_wait or switch_ask does
		// not hold up the agent's other calls.
		ctx, cancel := context.WithCancel(context.Background())
		mu.Lock()
		calls[string(msg.ID)] = cancel
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			result, rpcErr := dispatchMCP(ctx, msg, socket, token)
			mu.Lock()
			delete(calls, string(msg.ID))
			mu.Unlock()
			if ctx.Err() == nil { // a cancelled request gets no reply
				reply(rpcMessage{JSONRPC: "2.0", ID: msg.ID, Result: result, Error: rpcErr})
			}
		}()
	}
	wg.Wait()
	if failed {
		return 1
	}
	return 0
}

func dispatchMCP(ctx context.Context, msg rpcMessage, socket, token string) (any, *rpcError) {
	switch msg.Method {
	case "initialize":
		version := mcpProtocolVersion
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(msg.Params, &params) == nil && params.ProtocolVersion != "" {
			version = params.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "switch", "version": buildversion.Version},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": allowedTools(socket, token)}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		for _, t := range tools {
			if t.Name != params.Name {
				continue
			}
			arguments := params.Arguments
			if len(arguments) == 0 {
				arguments = json.RawMessage("{}")
			}
			result, err := call(ctx, socket, token, t.method, arguments)
			if err != nil {
				return toolResult(err.Error(), true), nil
			}
			text, ok := result.(string)
			if !ok {
				data, _ := json.MarshalIndent(result, "", "  ")
				text = string(data)
			}
			return toolResult(text, false), nil
		}
		return nil, &rpcError{Code: -32602, Message: fmt.Sprintf("unknown tool %q", params.Name)}
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + msg.Method}
	}
}

// allowedTools is the tools the server's permissions let this agent call, as
// whoami lists them, or every tool when the server cannot say: it refuses
// what the agent may not call either way.
func allowedTools(socket, token string) []tool {
	result, err := Call(socket, token, "whoami", nil)
	info, _ := result.(map[string]any)
	methods, ok := info["methods"].([]any)
	if err != nil || !ok {
		return tools
	}
	offered := make([]tool, 0, len(tools))
	for _, t := range tools {
		if slices.Contains(methods, any(t.method)) {
			offered = append(offered, t)
		}
	}
	return offered
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}
