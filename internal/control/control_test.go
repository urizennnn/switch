package control

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/urizennnn/switch/internal/execution"
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

func TestSocketInUseIsRefused(t *testing.T) {
	dir, err := os.MkdirTemp("", "swc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	first, err := Listen(Options{RunID: "run", StateDir: dir, Executable: "/bin/switch"})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := Listen(Options{RunID: "run", StateDir: dir, Executable: "/bin/switch"}); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second Listen err = %v, want in use", err)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatalf("first socket removed: %v", err)
	}
	// A stale socket file with no listener is replaced.
	first.Close()
	stale, err := Listen(Options{RunID: "run", StateDir: dir, Executable: "/bin/switch"})
	if err != nil {
		t.Fatalf("Listen after close: %v", err)
	}
	stale.Close()
}

type fakeTeam struct{ asked []string }

func (f *fakeTeam) Members(caller string) []Member {
	return []Member{{Name: "lead", You: caller == "lead"}, {Name: "builder-1"}}
}

func (f *fakeTeam) Ask(_ context.Context, from, to, message string) (string, error) {
	f.asked = append(f.asked, from+">"+to+":"+message)
	return "answer from " + to, nil
}

func TestTeamMode(t *testing.T) {
	dir, err := os.MkdirTemp("", "swt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	team := &fakeTeam{}
	server, err := Listen(Options{RunID: "teamkey1", StateDir: dir, Team: team, Executable: "/bin/switch"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	grant, _ := server.Grant(store.RunAgent{Name: "lead", Role: "lead"}, "")
	token := strings.TrimPrefix(grant.Env[1], EnvToken+"=")

	members, err := Call(server.Path, token, "team", nil)
	if err != nil || len(members.([]any)) != 2 {
		t.Fatalf("team = %v, %v", members, err)
	}
	reply, err := Call(server.Path, token, "ask", map[string]string{"to": "builder-1", "message": "thoughts?"})
	if err != nil || reply != "answer from builder-1" || len(team.asked) != 1 || team.asked[0] != "lead>builder-1:thoughts?" {
		t.Fatalf("ask = %v, %v (%v)", reply, err, team.asked)
	}
	if _, err := Call(server.Path, token, "ask", map[string]string{"message": "no recipient"}); err == nil {
		t.Fatal("ask without a teammate succeeded")
	}
}

type fakeSubagents struct {
	running  int
	spawned  []string
	reported string
	handoff  string
	used     int
}

func (f *fakeSubagents) Handoff(from, taskID, to, note string) (string, error) {
	f.handoff = from + "→" + to + ": " + note
	return "handing off", nil
}
func (f *fakeSubagents) Limits(string) execution.Limits {
	return execution.Limits{Used: f.used, Size: 100, HandoffAt: 80, HandoffTo: "builder-2"}
}

func (f *fakeSubagents) Spawn(parent, taskID string, req execution.SpawnRequest) (store.Subagent, error) {
	f.spawned = append(f.spawned, parent+"/"+req.Name)
	f.running++
	return store.Subagent{Name: req.Name, Status: "running"}, nil
}
func (f *fakeSubagents) Wait(context.Context, string, string, []string) ([]store.Subagent, error) {
	f.running = 0
	return []store.Subagent{{Name: "scout", Status: "done", Summary: "found it"}}, nil
}
func (f *fakeSubagents) Message(string, string, string, string) (string, error) { return "sent", nil }
func (f *fakeSubagents) Stop(string, string, string) (int, error)               { return 0, nil }
func (f *fakeSubagents) List(string, string) ([]store.Subagent, error)          { return nil, nil }
func (f *fakeSubagents) Report(id, text string) error                           { f.reported = id + ": " + text; return nil }
func (f *fakeSubagents) RunningSubagents(string) int                            { return f.running }

func TestSubagentTools(t *testing.T) {
	server, _, tokens := setup(t)
	call := func(who, method string, params any) (any, error) {
		return Call(server.Path, tokens[who], method, params)
	}
	if _, err := call("builder-1", "spawn", map[string]string{"name": "scout", "brief": "b"}); err == nil || !strings.Contains(err.Error(), "available during a run") {
		t.Fatalf("spawn without a run: %v", err)
	}
	subs := &fakeSubagents{}
	server.SetSubagents(subs)
	grant, _ := server.Grant(store.RunAgent{ID: "run:a/scout", Name: "builder-1/scout", Role: "subagent"}, "run:a")
	for _, env := range grant.Env {
		if value, ok := strings.CutPrefix(env, EnvToken+"="); ok {
			tokens["scout"] = value
		}
	}

	for _, forbidden := range []struct{ who, method string }{
		{"lead", "spawn"},     // the lead runs workers, not subagents
		{"scout", "spawn"},    // one level only
		{"scout", "wait"},     // nor manage siblings
		{"intruder", "spawn"}, // not this agent's task
	} {
		if _, err := call(forbidden.who, forbidden.method, map[string]string{"name": "x", "brief": "b"}); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Errorf("%s %s: err = %v, want forbidden", forbidden.who, forbidden.method, err)
		}
	}
	if _, err := call("builder-1", "spawn", map[string]string{"name": "scout", "brief": "find callers"}); err != nil || len(subs.spawned) != 1 || subs.spawned[0] != "builder-1/scout" {
		t.Fatalf("spawn = %v %v", subs.spawned, err)
	}
	if _, err := call("builder-1", "complete", map[string]string{"message": "done"}); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("complete with a running subagent: %v", err)
	}
	if _, err := call("scout", "report", map[string]string{"message": "3 callers"}); err != nil || subs.reported != "run:a/scout: 3 callers" {
		t.Fatalf("report = %q, %v", subs.reported, err)
	}
	if _, err := call("builder-1", "report", map[string]string{"message": "x"}); err == nil {
		t.Fatal("a worker used switch_report")
	}
	got, err := call("builder-1", "wait", nil)
	if err != nil || !strings.Contains(fmt.Sprint(got), "found it") {
		t.Fatalf("wait = %v, %v", got, err)
	}
	if _, err := call("builder-1", "complete", map[string]string{"message": "done"}); err != nil {
		t.Fatalf("complete after wait: %v", err)
	}

	// Handoffs and limits.
	if _, err := call("scout", "handoff", map[string]string{"message": "x"}); err == nil {
		t.Fatal("a subagent handed off")
	}
	if _, err := call("builder-1", "handoff", map[string]string{"message": "half done", "to": "lead"}); err != nil || subs.handoff != "builder-1→lead: half done" {
		t.Fatalf("handoff = %q, %v", subs.handoff, err)
	}
	if got, _ := call("builder-1", "progress", map[string]string{"message": "x"}); strings.Contains(fmt.Sprint(got), "switch_handoff") {
		t.Fatal("warned below the threshold")
	}
	subs.used = 85
	got, _ = call("builder-1", "progress", map[string]string{"message": "x"})
	if !strings.Contains(fmt.Sprint(got), "85% full") || !strings.Contains(fmt.Sprint(got), "builder-2") {
		t.Fatalf("no handoff warning past the threshold: %v", got)
	}
	if got, err := call("builder-1", "limits", nil); err != nil || got.(map[string]any)["context_percent"] != float64(85) {
		t.Fatalf("limits = %v, %v", got, err)
	}
}
