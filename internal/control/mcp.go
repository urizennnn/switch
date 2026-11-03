package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"

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
	{Name: "switch_ask", method: "ask", Description: "Ask a teammate on your Switch team a question and wait for their answer. Use it to get a second opinion or knowledge from another agent; they do not edit files.",
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
	{Name: "switch_delegate", method: "delegate", Description: "Lead only: add a task to the run.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":  map[string]any{"type": "string"},
				"brief":  map[string]any{"type": "string"},
				"role":   map[string]any{"type": "string"},
				"deps":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"verify": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []string{"title", "brief", "role"},
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
	for scanner.Scan() {
		var msg rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			_ = encoder.Encode(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if len(msg.ID) == 0 {
			continue // a notification, such as notifications/initialized
		}
		reply := rpcMessage{JSONRPC: "2.0", ID: msg.ID}
		reply.Result, reply.Error = dispatchMCP(msg, socket, token)
		if err := encoder.Encode(reply); err != nil {
			return 1
		}
	}
	return 0
}

func dispatchMCP(msg rpcMessage, socket, token string) (any, *rpcError) {
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
		return map[string]any{"tools": tools}, nil
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
			result, err := Call(socket, token, t.method, arguments)
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

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}
