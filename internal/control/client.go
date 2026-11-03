package control

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// Call sends one request to the control socket and returns its result.
func Call(socket, token, method string, params any) (any, error) {
	if socket == "" || token == "" {
		return nil, fmt.Errorf("%s and %s are not set; this only works inside a Switch agent session", EnvSocket, EnvToken)
	}
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	defer conn.Close()
	deadline := 30 * time.Second
	if method == "ask" {
		deadline = askTimeout + time.Minute
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

// RunAgentCLI is `switch __control agent <method> [message]`, for agents that
// prefer a shell command to MCP tools.
func RunAgentCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: switch __control agent <whoami|team|task|progress|complete|block> [message]\n       switch __control agent ask <teammate> <message>")
		return 2
	}
	params := map[string]string{"message": strings.Join(args[1:], " ")}
	if args[0] == "ask" && len(args) > 1 {
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
