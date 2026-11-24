package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"time"
)

// Call sends one request to the control socket and returns its result.
func Call(socket, token, method string, params any) (any, error) {
	return call(context.Background(), socket, token, method, params)
}

// call is Call until ctx ends, which closes the connection.
func call(ctx context.Context, socket, token, method string, params any) (any, error) {
	if socket == "" || token == "" {
		return nil, fmt.Errorf("%s and %s are not set; this only works inside a Switch agent session", EnvSocket, EnvToken)
	}
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := 30 * time.Second
	if method == "ask" || method == "wait" || method == "question" {
		deadline = waitBudget + 30*time.Second
	}
	_ = conn.SetDeadline(time.Now().Add(deadline))

	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(conn).Encode(Request{Token: token, Method: method, Params: raw}); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	return resp.Result, nil
}

// RunAgentCLI is `switch __control agent <method> [message | JSON arguments]`,
// for agents that prefer a shell command to MCP tools.
func RunAgentCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, agentUsage())
		return 2
	}
	text := strings.TrimSpace(strings.Join(args[1:], " "))
	var params any = map[string]string{"message": text}
	switch {
	case toolArguments(args[0], text):
		params = json.RawMessage(text)
	case args[0] == "ask" && len(args) > 1:
		params = map[string]string{"to": args[1], "message": strings.Join(args[2:], " ")}
	}
	result, err := Call(os.Getenv(EnvSocket), os.Getenv(EnvToken), args[0], params)
	if err != nil {
		fmt.Fprintln(stderr, "switch:", err)
		return 1
	}
	if text, ok := result.(string); ok {
		fmt.Fprintln(stdout, text)
		return 0
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(result)
	return 0
}

// toolArguments reports whether text is a JSON object that names an argument
// of method's MCP tool, rather than a message that happens to be JSON.
func toolArguments(method, text string) bool {
	var fields map[string]json.RawMessage
	i := slices.IndexFunc(tools, func(t tool) bool { return t.method == method })
	if i < 0 || json.Unmarshal([]byte(text), &fields) != nil {
		return false
	}
	properties, _ := tools[i].InputSchema["properties"].(map[string]any)
	for name := range fields {
		if properties[name] != nil {
			return true
		}
	}
	return false
}

// agentUsage lists each method with the arguments of its MCP tool.
func agentUsage() string {
	var b strings.Builder
	b.WriteString("usage: switch __control agent <method> [message]\n" +
		"       switch __control agent <method> '<JSON object of arguments>'\n" +
		"       switch __control agent ask <teammate> <message>\n\nmethods and their arguments:\n  whoami\n")
	for _, t := range tools {
		properties, _ := t.InputSchema["properties"].(map[string]any)
		line := fmt.Sprintf("  %-10s %s", t.method, strings.Join(slices.Sorted(maps.Keys(properties)), ", "))
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	return b.String()
}
