package control

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/urizennnn/switch/internal/store"
)

func setup(t *testing.T) (*Server, *store.Store, map[string]string) {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	ctx := context.Background()
	for _, task := range []store.Task{
		{ID: "run:a", RunID: "run", Title: "a", Brief: "do a", Role: "builder", Status: "running", Assignee: "builder-1"},
		{ID: "run:b", RunID: "run", Title: "b", Brief: "do b", Role: "builder", Status: "running", Assignee: "builder-2"},
	} {
		if err := state.SaveTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var progress []string
	server, err := Listen(Options{Store: state, RunID: "run", StateDir: t.TempDir(), Executable: "/bin/switch",
		OnText: func(agent, text string) { mu.Lock(); progress = append(progress, agent+text); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })

	tokens := map[string]string{}
	for name, grant := range map[string]struct {
		agent store.RunAgent
		task  string
	}{
		"builder-1": {store.RunAgent{Name: "builder-1", Role: "builder"}, "run:a"},
		"lead":      {store.RunAgent{Name: "lead", Role: "lead"}, "run:a"},
		// A builder holding a token for a task assigned to someone else.
		"intruder": {store.RunAgent{Name: "builder-3", Role: "builder"}, "run:b"},
	} {
		g, err := server.Grant(grant.agent, grant.task)
		if err != nil {
			t.Fatal(err)
		}
		for _, env := range g.Env {
			if value, ok := strings.CutPrefix(env, EnvToken+"="); ok {
				tokens[name] = value
			}
		}
		if len(g.MCPServers) != 1 || g.MCPServers[0].Stdio.Command != "/bin/switch" {
			t.Fatalf("grant MCP = %+v", g.MCPServers)
		}
	}
	return server, state, tokens
}

func TestSocketIsPrivate(t *testing.T) {
	server, _, _ := setup(t)
	info, err := os.Stat(server.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestAuthorization(t *testing.T) {
	server, state, tokens := setup(t)
	ctx := context.Background()
	call := func(who, method string, params any) (any, error) {
		return Call(server.Path, tokens[who], method, params)
	}

	if _, err := Call(server.Path, "forged", "whoami", nil); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("forged token: %v", err)
	}
	if got, err := call("builder-1", "whoami", nil); err != nil || got.(map[string]any)["agent"] != "builder-1" {
		t.Fatalf("whoami = %v, %v", got, err)
	}
	if got, err := call("builder-1", "task", nil); err != nil || got.(map[string]any)["brief"] != "do a" {
		t.Fatalf("task = %v, %v", got, err)
	}
	if _, err := call("builder-1", "progress", map[string]string{"message": "halfway"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("builder-1", "complete", map[string]string{"message": "added a"}); err != nil {
		t.Fatal(err)
	}
	if task, _, _ := state.GetTask(ctx, "run:a"); task.Summary != "added a" {
		t.Fatalf("summary = %q", task.Summary)
	}
	if _, err := call("builder-1", "block", map[string]string{"message": "needs a decision"}); err != nil {
		t.Fatal(err)
	}
	if reason, ok := server.TakeBlock("run:a"); !ok || reason != "needs a decision" {
		t.Fatalf("TakeBlock = %q, %v", reason, ok)
	}
	if _, ok := server.TakeBlock("run:a"); ok {
		t.Fatal("block was not cleared")
	}

	for _, forbidden := range []struct{ who, method string }{
		{"builder-1", "delegate"}, // workers cannot delegate
		{"builder-1", "approve"},  // nobody but the user approves
		{"lead", "approve"},
		{"lead", "complete"},     // the lead does not report on tasks
		{"intruder", "complete"}, // not this agent's task
	} {
		_, err := call(forbidden.who, forbidden.method, map[string]string{"message": "x", "title": "t", "brief": "b", "role": "builder"})
		if err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Errorf("%s %s: err = %v, want forbidden", forbidden.who, forbidden.method, err)
		}
	}

	got, err := call("lead", "delegate", map[string]any{"title": "docs", "brief": "write docs", "role": "builder", "deps": []string{"a"}})
	if err != nil {
		t.Fatalf("lead delegate: %v", err)
	}
	id := got.(map[string]any)["id"].(string)
	if task, ok, _ := state.GetTask(ctx, id); !ok || task.Status != "queued" || len(task.Deps) != 1 || task.Deps[0] != "run:a" {
		t.Fatalf("delegated task = %+v, %v", task, ok)
	}
	if events, _ := state.Events(ctx, "run", 50); len(events) < 4 {
		t.Fatalf("events = %d, want progress/complete/block/delegate logged", len(events))
	}
}

func TestDroppedConnectionDoesNotStopServer(t *testing.T) {
	server, _, tokens := setup(t)
	conn, err := net.Dial("unix", server.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte(`{"token": "half a req`))
	conn.Close()
	if _, err := Call(server.Path, tokens["builder-1"], "whoami", nil); err != nil {
		t.Fatalf("server died after a dropped connection: %v", err)
	}
}

func TestMCPBridge(t *testing.T) {
	server, _, tokens := setup(t)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"switch_progress","arguments":{"message":"via mcp"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"switch_delegate","arguments":{"title":"t","brief":"b","role":"builder"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"nope"}`,
	}, "\n") + "\n"
	var out strings.Builder
	if code := serveMCP(strings.NewReader(input), &out, server.Path, tokens["builder-1"]); code != 0 {
		t.Fatalf("exit %d", code)
	}

	var replies []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(out.String()))
	for scanner.Scan() {
		var reply map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		replies = append(replies, reply)
	}
	if len(replies) != 5 {
		t.Fatalf("got %d replies, want 5 (notifications get none):\n%s", len(replies), out.String())
	}
	if v := replies[0]["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Fatalf("protocolVersion = %v", v)
	}
	if n := len(replies[1]["result"].(map[string]any)["tools"].([]any)); n != len(tools) {
		t.Fatalf("tools/list = %d tools", n)
	}
	if replies[2]["result"].(map[string]any)["isError"] != false {
		t.Fatalf("progress call = %v", replies[2])
	}
	if replies[3]["result"].(map[string]any)["isError"] != true {
		t.Fatalf("worker delegate should be a tool error: %v", replies[3])
	}
	if replies[4]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method = %v", replies[4])
	}
}
