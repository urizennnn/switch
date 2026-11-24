package control

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	for _, agent := range []store.RunAgent{
		{ID: "l", RunID: "run", Name: "lead", Role: "lead"},
		{ID: "b1", RunID: "run", Name: "builder-1", Role: "builder"},
		{ID: "b2", RunID: "run", Name: "builder-2", Role: "builder"},
	} {
		if err := state.SaveRunAgent(ctx, agent); err != nil {
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
		{"lead", "complete"},     // run:a is builder-1's task, not the lead's
		{"intruder", "complete"}, // not this agent's task
	} {
		_, err := call(forbidden.who, forbidden.method, map[string]string{"message": "x", "title": "t", "brief": "b", "role": "builder"})
		if err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Errorf("%s %s: err = %v, want forbidden", forbidden.who, forbidden.method, err)
		}
	}

	// Delegated tasks get the checks a plan's tasks get.
	for _, bad := range []map[string]any{
		{"title": "t", "brief": "b", "role": "developer", "acceptance": []string{"x"}}, // no worker has the role
		{"title": "t", "brief": "b", "role": "lead", "acceptance": []string{"x"}},      // the lead takes no tasks
		{"title": "t", "brief": "b", "role": "standby", "acceptance": []string{"x"}},   // nor does a standby lead
		{"title": "t", "brief": "b", "role": "builder"},                                // no acceptance criteria
	} {
		if _, err := call("lead", "delegate", bad); err == nil {
			t.Errorf("delegate %v was accepted", bad)
		} else if bad["role"] == "developer" && !strings.Contains(err.Error(), "builder") {
			t.Errorf("unknown role error does not name the run's roles: %v", err)
		}
	}
	got, err := call("lead", "delegate", map[string]any{"title": "docs", "brief": "write docs", "role": "builder", "deps": []string{"a"}, "acceptance": []string{"docs exist"}})
	if err != nil {
		t.Fatalf("lead delegate: %v", err)
	}
	id := got.(map[string]any)["id"].(string)
	if task, ok, _ := state.GetTask(ctx, id); !ok || task.Status != "blocked" || !strings.HasPrefix(task.LastError, "proposed by lead on a;") || len(task.Deps) != 1 || task.Deps[0] != "run:a" || len(task.Acceptance) != 1 {
		t.Fatalf("delegated task = %+v, %v", task, ok)
	}
	if events, _ := state.Events(ctx, "run", 50); len(events) < 4 {
		t.Fatalf("events = %d, want progress/complete/block/delegate logged", len(events))
	}

	// A turn's token dies with the turn.
	grant, _ := server.Grant(store.RunAgent{Name: "builder-1", Role: "builder"}, "run:a")
	token := strings.TrimPrefix(grant.Env[1], EnvToken+"=")
	if _, err := Call(server.Path, token, "whoami", nil); err != nil {
		t.Fatal(err)
	}
	grant.Revoke()
	if _, err := Call(server.Path, token, "whoami", nil); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("revoked token: %v", err)
	}
}

// A lead that took over a handed-off task reports on it like any worker,
// but only while it works on it.
func TestLeadReportsOnAHandedOffTask(t *testing.T) {
	server, state, tokens := setup(t)
	ctx := context.Background()
	server.SetSubagents(&fakeSubagents{})
	task, _, _ := state.GetTask(ctx, "run:a")
	task.Assignee = "lead"
	if err := state.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"complete", "block", "spawn"} {
		if _, err := Call(server.Path, tokens["lead"], method, map[string]string{"message": "x", "name": "scout", "brief": "b"}); err != nil {
			t.Errorf("lead %s on its own task: %v", method, err)
		}
	}
	task.Status = execution.StatusReviewing
	if err := state.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(server.Path, tokens["lead"], "block", map[string]string{"message": "x"}); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("lead block on a task under review: %v", err)
	}
}

func TestConcurrentDelegatesGetDistinctIDs(t *testing.T) {
	server, state, tokens := setup(t)
	const n = 8
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Call(server.Path, tokens["lead"], "delegate", map[string]any{"title": "t", "brief": "b", "role": "builder", "acceptance": []string{"x"}})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- got.(map[string]any)["id"].(string)
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	tasks, _ := state.TasksForRun(context.Background(), "run")
	if len(seen) != n || len(tasks) != 2+n {
		t.Fatalf("%d delegates got %d distinct ids and saved %d tasks", n, len(seen), len(tasks)-2)
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

	// Requests run side by side, so replies come in any order.
	replies := map[float64]map[string]any{}
	scanner := bufio.NewScanner(strings.NewReader(out.String()))
	for scanner.Scan() {
		var reply map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		replies[reply["id"].(float64)] = reply
	}
	if len(replies) != 5 {
		t.Fatalf("got %d replies, want 5 (notifications get none):\n%s", len(replies), out.String())
	}
	if v := replies[1]["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Fatalf("protocolVersion = %v", v)
	}
	if n := len(replies[2]["result"].(map[string]any)["tools"].([]any)); n != len(tools)-5 {
		t.Fatalf("tools/list = %d tools, want all but switch_delegate, switch_tasks, switch_steer, switch_report and switch_ask for a worker in a run", n)
	}
	if replies[3]["result"].(map[string]any)["isError"] != false {
		t.Fatalf("progress call = %v", replies[3])
	}
	if replies[4]["result"].(map[string]any)["isError"] != true {
		t.Fatalf("worker delegate should be a tool error: %v", replies[4])
	}
	if replies[5]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method = %v", replies[5])
	}
}

// A switch_wait that blocks does not hold up the agent's other calls, and a
// cancelled call stops and gets no reply.
func TestMCPBridgeRunsCallsConcurrently(t *testing.T) {
	server, _, tokens := setup(t)
	subs := &fakeSubagents{block: make(chan struct{})}
	server.SetSubagents(subs)
	t.Cleanup(func() { close(subs.block) }) // runs before the server closes

	stdin, input := io.Pipe()
	output, stdout := io.Pipe()
	exited := make(chan int, 1)
	go func() {
		exited <- serveMCP(stdin, stdout, server.Path, tokens["builder-1"])
		stdout.Close()
	}()
	send := func(line string) {
		if _, err := io.WriteString(input, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	replies := make(chan string)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			replies <- scanner.Text()
		}
		close(replies)
	}()

	send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"switch_wait","arguments":{}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"switch_progress","arguments":{"message":"meanwhile"}}}`)
	select {
	case got := <-replies:
		if !strings.Contains(got, `"id":2`) {
			t.Fatalf("first reply = %s, want the progress call's", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the progress call waited behind switch_wait")
	}
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"timeout"}}`)
	input.Close()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge kept waiting on a cancelled call")
	}
	if got, ok := <-replies; ok {
		t.Fatalf("the cancelled call got a reply: %s", got)
	}
}

// Each agent is offered only the tools the server lets its role use, and
// only those this kind of server serves (a run has no one to ask); what it
// is not offered, it may not call. In the chat the user can make any agent
// the lead, and the lead a teammate again, within one session, so each is
// offered both roles' tools and its role when it calls refuses the rest: the
// lead talks with the user and works on nothing itself, and only it hands
// off; a standby lead is a teammate.
func TestToolsFollowPermissions(t *testing.T) {
	budget := waitBudget
	t.Cleanup(func() { waitBudget = budget }) // after the servers close
	waitBudget = 200 * time.Millisecond       // a question let through fails the test rather than hold it up

	server, _, tokens := setup(t)
	scout, _ := server.Grant(store.RunAgent{ID: "run:a/scout", Name: "builder-1/scout", Role: "subagent"}, "run:a")
	tokens["scout"] = strings.TrimPrefix(scout.Env[1], EnvToken+"=")
	chat, err := Listen(Options{RunID: "teamkey8", StateDir: t.TempDir(), Team: &fakeTeam{}, Executable: "/bin/switch"})
	if err != nil {
		t.Fatal(err)
	}
	defer chat.Close()
	chat.SetSubagents(&fakeSubagents{questions: make(chan string, 8)})
	for name, agent := range map[string]store.RunAgent{"chat lead": {Name: "lead", Role: "lead"}, "teammate": {Name: "builder-1", Role: "builder"}, "standby": {Name: "lead-2", Role: "lead"}} {
		grant, _ := chat.Grant(agent, "")
		tokens[name] = strings.TrimPrefix(grant.Env[1], EnvToken+"=")
	}
	serverOf := func(token string) *Server {
		if _, err := Call(chat.Path, token, "whoami", nil); err == nil {
			return chat
		}
		return server
	}
	listed := func(token string) string {
		var out strings.Builder
		serveMCP(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"), &out, serverOf(token).Path, token)
		var reply struct {
			Result struct{ Tools []struct{ Name string } }
		}
		if err := json.Unmarshal([]byte(out.String()), &reply); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range reply.Result.Tools {
			names = append(names, tool.Name)
		}
		return " " + strings.Join(names, " ") + " "
	}
	leads := "switch_ask switch_handoff switch_delegate switch_tasks switch_steer"
	teammates := "switch_progress switch_question switch_spawn switch_wait switch_message switch_stop switch_subagents"
	for _, c := range []struct{ who, has, lacks, refused string }{
		{"lead", "switch_delegate switch_tasks switch_complete switch_spawn switch_question switch_handoff", "switch_report switch_ask switch_steer", ""},
		{"builder-1", "switch_complete switch_spawn switch_question switch_handoff", "switch_delegate switch_tasks switch_steer switch_report switch_ask", ""},
		{"scout", "switch_report switch_progress", "switch_ask switch_spawn switch_complete switch_handoff switch_delegate switch_question", ""},
		{"chat lead", leads + " " + teammates, "switch_complete switch_block switch_report", teammates},
		{"teammate", leads + " " + teammates, "switch_complete switch_block switch_report", "switch_handoff switch_delegate switch_tasks switch_steer"},
		{"standby", leads + " " + teammates, "switch_complete switch_block switch_report", "switch_handoff switch_delegate switch_tasks switch_steer"},
	} {
		srv, got := serverOf(tokens[c.who]), listed(tokens[c.who])
		call := func(name string) (method string, err error) {
			method = strings.TrimPrefix(name, "switch_")
			_, err = Call(srv.Path, tokens[c.who], method, map[string]string{"message": "x", "name": "scout", "brief": "b", "to": "builder-2"})
			return method, err
		}
		for _, name := range strings.Fields(c.has) {
			if !strings.Contains(got, " "+name+" ") {
				t.Errorf("%s is not offered %s:%s", c.who, name, got)
			}
		}
		for _, name := range strings.Fields(c.lacks) {
			if strings.Contains(got, " "+name+" ") {
				t.Errorf("%s is offered %s", c.who, name)
			}
			// What it is not offered, its role forbids, or its server does not serve.
			if method, err := call(name); err == nil || srv.serves(method) && !strings.Contains(err.Error(), "forbidden") {
				t.Errorf("%s calling %s: %v, want it forbidden", c.who, name, err)
			}
		}
		for _, name := range strings.Fields(c.refused) {
			if _, err := call(name); err == nil || !strings.Contains(err.Error(), "forbidden") {
				t.Errorf("%s calling %s: %v, want it forbidden for its role now", c.who, name, err)
			}
		}
	}
	// When the server cannot say (here, an unknown token), every tool is
	// listed; the server still refuses each call.
	if got := listed("forged"); strings.Count(got, "switch_") != len(tools) {
		t.Errorf("forged token is offered %s", got)
	}
	for _, tool := range tools {
		if _, ok := permissions[tool.method]; !ok {
			t.Errorf("%s has no permissions entry, so no agent is offered it", tool.Name)
		}
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

type fakeTeam struct {
	mu      sync.Mutex // guards asked, steered and lead: teammates answer side by side
	asked   []string
	steered []string
	lead    string        // who leads, after /lead or a handoff; "" is lead
	answer  chan struct{} // when set, Ask answers once it is closed
	started chan string   // when set, gets each question as it is asked
}

func (f *fakeTeam) Steer(_ context.Context, to, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steered = append(f.steered, to+": "+text)
	return "steered " + to, nil
}

func (f *fakeTeam) Members(caller string) []Member {
	return []Member{{Name: "lead", You: caller == "lead"}, {Name: "builder-1"}}
}

func (f *fakeTeam) Lead() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cmp.Or(f.lead, "lead")
}

func (f *fakeTeam) Ask(ctx context.Context, from, to, message string) (string, error) {
	f.mu.Lock()
	f.asked = append(f.asked, from+">"+to+":"+message)
	f.mu.Unlock()
	if f.started != nil {
		f.started <- message
	}
	if f.answer != nil {
		select {
		case <-f.answer:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
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

	// The chat has no run to add tasks to: delegate is refused, not a crash.
	if _, err := Call(server.Path, token, "delegate", map[string]any{"title": "t", "brief": "b", "role": "builder", "acceptance": []string{"x"}}); err == nil || !strings.Contains(err.Error(), "/plan") {
		t.Fatalf("chat delegate: %v", err)
	}
	who, err := Call(server.Path, token, "whoami", nil)
	if err != nil {
		t.Fatalf("server stopped answering after a chat delegate: %v", err)
	}
	// The chat lead is offered the tools for a run the user runs, which wait
	// for one, but not those for a worker's own task.
	if methods := who.(map[string]any)["methods"].([]any); !slices.Contains(methods, any("delegate")) || !slices.Contains(methods, any("tasks")) ||
		!slices.Contains(methods, any("steer")) || slices.Contains(methods, any("complete")) || !slices.Contains(methods, any("ask")) {
		t.Fatalf("chat lead methods = %v", methods)
	}
	// The user reads the lead's reply, not its live view.
	if _, err := Call(server.Path, token, "progress", map[string]string{"message": "x"}); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("the chat lead's progress = %v, want a refusal", err)
	}
	// Subagents report to their teammate; they do not consult others.
	sub, _ := server.Grant(store.RunAgent{ID: "builder-1/scout", Name: "builder-1/scout", Role: "subagent"}, "")
	if _, err := Call(server.Path, strings.TrimPrefix(sub.Env[1], EnvToken+"="), "ask", map[string]string{"to": "builder-2", "message": "q"}); err == nil || len(team.asked) != 1 {
		t.Fatalf("subagent ask = %v (%v)", err, team.asked)
	}
	// A standby lead answering a question may start subagents, as its prompt
	// says; the team decides when.
	subs := &fakeSubagents{}
	server.SetSubagents(subs)
	standby, _ := server.Grant(store.RunAgent{Name: "lead-2", Role: "lead"}, "")
	if _, err := Call(server.Path, strings.TrimPrefix(standby.Env[1], EnvToken+"="), "spawn", map[string]string{"name": "scout", "brief": "look"}); err != nil || len(subs.spawned) != 1 {
		t.Fatalf("standby lead spawn = %v (%v)", err, subs.spawned)
	}
}

// While the user runs a run, the chat's lead lists its tasks, proposes tasks
// to it, which wait blocked for the user, and steers its agents. Teammates
// and a standby lead do none of it, and once the run ends nothing is bound.
func TestChatLeadSeesTheBoundRun(t *testing.T) {
	_, state, _ := setup(t)
	ctx := context.Background()
	b, _, _ := state.GetTask(ctx, "run:b")
	b.Status, b.LastError = execution.StatusBlocked, "verification failed:\nexit 1"
	c := store.Task{ID: "run:c", RunID: "run", Title: "c", Brief: "do c", Role: "builder", Deps: []string{"run:a"}, Status: execution.StatusQueued}
	for _, task := range []store.Task{b, c} {
		if err := state.SaveTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := os.MkdirTemp("", "swd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	team := &fakeTeam{}
	chat, err := Listen(Options{RunID: "teamkey7", StateDir: dir, Team: team, Executable: "/bin/switch"})
	if err != nil {
		t.Fatal(err)
	}
	defer chat.Close()
	token := func(agent, role string) string {
		grant, _ := chat.Grant(store.RunAgent{Name: agent, Role: role}, "")
		return strings.TrimPrefix(grant.Env[1], EnvToken+"=")
	}
	lead, mate, standby := token("lead", "lead"), token("builder-1", "builder"), token("lead-2", "lead")
	params := map[string]any{"title": "docs", "brief": "write docs", "role": "builder", "acceptance": []string{"docs exist"}, "to": "builder-1", "message": "use v2"}
	for _, method := range []string{"tasks", "delegate", "steer"} {
		if _, err := Call(chat.Path, lead, method, params); err == nil {
			t.Errorf("chat %s with no run bound succeeded", method)
		}
	}

	woken := make(chan struct{}, 1)
	chat.Bind(state, "run", func() { woken <- struct{}{} })
	got, err := Call(chat.Path, lead, "tasks", nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks := map[string]map[string]any{}
	for _, item := range got.([]any) {
		task := item.(map[string]any)
		tasks[task["id"].(string)] = task
	}
	if tasks["a"]["assignee"] != "builder-1" || tasks["a"]["waits"] != nil || tasks["b"]["last_error"] != "verification failed:" ||
		tasks["b"]["waits"] != "blocked: verification failed:" || tasks["c"]["waits"] != "waits for a, which is running" {
		t.Fatalf("tasks = %v", got)
	}

	got, err = Call(chat.Path, lead, "delegate", params)
	if err != nil {
		t.Fatal(err)
	}
	id := got.(map[string]any)["id"].(string)
	proposal, ok, _ := state.GetTask(ctx, id)
	if !ok || proposal.Status != execution.StatusBlocked || !strings.Contains(proposal.LastError, "/retry "+planID(id)+" accepts it") ||
		!strings.Contains(fmt.Sprint(got), "/retry "+planID(id)) {
		t.Fatalf("proposal %v = %+v", got, proposal)
	}
	select {
	case <-woken:
	default:
		t.Fatal("the proposal did not wake the run's engine")
	}
	if events, _ := state.Events(ctx, "run", 50); !slices.ContainsFunc(events, func(e store.Event) bool { return e.Kind == "delegate" && e.Agent == "lead" }) {
		t.Fatalf("the proposal is not in the run's log: %+v", events)
	}
	if _, err := Call(chat.Path, lead, "steer", params); err != nil || len(team.steered) != 1 || team.steered[0] != "builder-1: The lead (lead) says: use v2" {
		t.Fatalf("steer = %v (%v)", err, team.steered)
	}
	for _, who := range []string{mate, standby} {
		for _, method := range []string{"tasks", "delegate", "steer"} {
			if _, err := Call(chat.Path, who, method, params); err == nil || !strings.Contains(err.Error(), "forbidden") {
				t.Errorf("a teammate's %s: %v, want forbidden", method, err)
			}
		}
	}
	// The tools follow whoever leads, whatever its configured role.
	team.mu.Lock()
	team.lead = "builder-1"
	team.mu.Unlock()
	if _, err := Call(chat.Path, mate, "tasks", nil); err != nil {
		t.Fatalf("tasks from builder-1 leading: %v", err)
	}
	if _, err := Call(chat.Path, lead, "tasks", nil); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("tasks from the lead on standby: %v, want forbidden", err)
	}

	chat.Bind(nil, "", nil)
	if _, err := Call(chat.Path, lead, "tasks", nil); err == nil {
		t.Fatal("the chat lead sees a run that ended")
	}
}

type fakeSubagents struct {
	running   int
	spawned   []string
	reported  string
	handoff   string
	used      int
	block     chan struct{} // when set, the subagent runs until it is closed
	questions chan string   // when set, gets each question asked and given up
	answers   chan string   // the user's answers
}

func (f *fakeSubagents) Question(ctx context.Context, agent, taskID, question string, options []string) string {
	f.questions <- fmt.Sprintf("%s on %s asks %s %v", agent, taskID, question, options)
	select {
	case answer := <-f.answers:
		return answer
	case <-ctx.Done():
		f.questions <- "gave up on " + question
		return "the user did not answer"
	}
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
func (f *fakeSubagents) Wait(ctx context.Context, _, _ string, _ []string) ([]store.Subagent, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return []store.Subagent{{Name: "scout", Status: execution.SubRunning}}, nil
		}
	}
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
		{"lead", "spawn"},     // run:a is builder-1's task, not the lead's
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

// In the chat only the lead can hand off, so only the lead is told to.
func TestOnlyTheChatLeadIsToldToHandOff(t *testing.T) {
	dir, err := os.MkdirTemp("", "swd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	chat, err := Listen(Options{RunID: "teamkeyh", StateDir: dir, Team: &fakeTeam{}, Executable: "/bin/switch"})
	if err != nil {
		t.Fatal(err)
	}
	defer chat.Close()
	chat.SetSubagents(&fakeSubagents{used: 85})
	token := func(agent, role string) string {
		grant, _ := chat.Grant(store.RunAgent{Name: agent, Role: role}, "")
		return strings.TrimPrefix(grant.Env[1], EnvToken+"=")
	}
	if got, err := Call(chat.Path, token("builder-1", "builder"), "progress", map[string]string{"message": "x"}); err != nil || strings.Contains(fmt.Sprint(got), "switch_handoff") {
		t.Fatalf("a teammate's progress = %v, %v", got, err)
	}
	if got, err := Call(chat.Path, token("lead", "lead"), "ask", map[string]string{"to": "builder-1", "message": "x"}); err != nil || !strings.Contains(fmt.Sprint(got), "switch_handoff") {
		t.Fatalf("the lead's ask = %v, %v", got, err)
	}
}

// The agent CLI makes every call an MCP tool can: a JSON object argument is
// the tool's arguments.
func TestAgentCLIPassesJSONArguments(t *testing.T) {
	server, state, tokens := setup(t)
	subs := &fakeSubagents{}
	server.SetSubagents(subs)
	t.Setenv(EnvSocket, server.Path)
	run := func(who string, args ...string) (string, int) {
		t.Setenv(EnvToken, tokens[who])
		var out strings.Builder
		code := RunAgentCLI(args, &out, &out)
		return out.String(), code
	}

	out, code := run("lead", "delegate", `{"title": "docs", "brief": "write docs", "role": "builder", "acceptance": ["docs exist"]}`)
	if code != 0 || !strings.Contains(out, "delegated-") {
		t.Fatalf("delegate = %d %s", code, out)
	}
	if tasks, _ := state.TasksForRun(context.Background(), "run"); len(tasks) != 3 ||
		!slices.ContainsFunc(tasks, func(task store.Task) bool { return slices.Equal(task.Acceptance, []string{"docs exist"}) }) {
		t.Fatalf("tasks = %+v, want the delegated one added with its acceptance criteria", tasks)
	}
	if out, code := run("builder-1", "spawn", `{"name": "scout", "brief": "find callers"}`); code != 0 || len(subs.spawned) != 1 {
		t.Fatalf("spawn = %d %s (%v)", code, out, subs.spawned)
	}
	if out, code := run("builder-1", "handoff", `{"message": "half done", "to": "lead"}`); code != 0 || subs.handoff != "builder-1→lead: half done" {
		t.Fatalf("handoff = %d %s (%q)", code, out, subs.handoff)
	}
	// Anything else is the message, as before, even a JSON result.
	if out, code := run("builder-1", "handoff", "{not", "json}"); code != 0 || subs.handoff != "builder-1→: {not json}" {
		t.Fatalf("plain handoff = %d %s (%q)", code, out, subs.handoff)
	}
	scout, _ := server.Grant(store.RunAgent{ID: "run:a/scout", Name: "builder-1/scout", Role: "subagent"}, "run:a")
	tokens["scout"] = strings.TrimPrefix(scout.Env[1], EnvToken+"=")
	if out, code := run("scout", "report", `{"callers": ["a.go"]}`); code != 0 || subs.reported != `run:a/scout: {"callers": ["a.go"]}` {
		t.Fatalf("JSON report = %d %s (%q)", code, out, subs.reported)
	}
	// The usage names every method and its arguments.
	out, code = run("builder-1")
	if code != 2 || !strings.Contains(out, "acceptance") {
		t.Fatalf("usage = %d %s", code, out)
	}
	for _, tool := range tools {
		if !strings.Contains(out, "\n  "+tool.method) {
			t.Errorf("usage does not list %s:\n%s", tool.method, out)
		}
	}
}

// Closing a server ends its requests in flight and idle connections at
// once: quitting must not wait for a teammate's answer.
func TestCloseEndsRequestsInFlight(t *testing.T) {
	dir, err := os.MkdirTemp("", "swq")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	team := &fakeTeam{answer: make(chan struct{}), started: make(chan string, 1)}
	server, err := Listen(Options{RunID: "teamkey3", StateDir: dir, Team: team, Executable: "/bin/switch"})
	if err != nil {
		t.Fatal(err)
	}
	grant, _ := server.Grant(store.RunAgent{Name: "lead", Role: "lead"}, "")
	token := strings.TrimPrefix(grant.Env[1], EnvToken+"=")
	idle, err := net.Dial("unix", server.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	go Call(server.Path, token, "ask", map[string]string{"to": "builder-1", "message": "q"})
	select {
	case <-team.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the question never reached the teammate")
	}
	closed := make(chan struct{})
	go func() {
		server.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for the teammate's answer")
	}
}

// The chat's server has a team but no run: every method answers or refuses
// with a reason, the server keeps serving, and a teammate's progress reaches
// the TUI.
func TestChatServerAnswersEveryMethod(t *testing.T) {
	dir, err := os.MkdirTemp("", "swa")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	var mu sync.Mutex
	var progress []string
	server, err := Listen(Options{RunID: "teamkey4", StateDir: dir, Team: &fakeTeam{}, Executable: "/bin/switch",
		OnText: func(agent, text string) { mu.Lock(); progress = append(progress, agent+": "+text); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	grant, _ := server.Grant(store.RunAgent{Name: "lead", Role: "lead"}, "")
	token := strings.TrimPrefix(grant.Env[1], EnvToken+"=")

	params := map[string]any{"message": "x", "to": "builder-1", "name": "scout", "brief": "b", "title": "t", "role": "builder", "acceptance": []string{"x"}}
	methods := []string{"approve", "merge"}
	for method := range permissions {
		methods = append(methods, method)
	}
	for _, subagents := range []Subagents{nil, &fakeSubagents{}} {
		server.SetSubagents(subagents)
		for _, method := range methods {
			// approve and merge are nobody's.
			if _, err := Call(server.Path, token, method, params); err == nil && (!server.serves(method) || permissions[method] == nil) {
				t.Errorf("%s succeeded in the chat", method)
			}
			if _, err := Call(server.Path, token, "whoami", nil); err != nil {
				t.Fatalf("server stopped answering after %s: %v", method, err)
			}
		}
	}
	mate, _ := server.Grant(store.RunAgent{Name: "builder-1", Role: "builder"}, "")
	if _, err := Call(server.Path, strings.TrimPrefix(mate.Env[1], EnvToken+"="), "progress", params); err != nil {
		t.Fatalf("teammate progress: %v", err)
	}
	mu.Lock()
	got := progress
	mu.Unlock()
	if !slices.Equal(got, []string{"builder-1: [progress] x\n"}) {
		t.Fatalf("progress = %q, want only the teammate's", got)
	}

	t.Setenv(EnvSocket, server.Path)
	t.Setenv(EnvToken, token)
	var out strings.Builder
	if code := RunAgentCLI([]string{"ask", "builder-1", "what", "now?"}, &out, &out); code != 0 || !strings.Contains(out.String(), "answer from builder-1") {
		t.Fatalf("CLI ask = %d %q", code, out.String())
	}
}

// switch_wait and switch_ask return within the budget, under MCP clients'
// own timeouts, and say to call again; calling again never asks twice.
func TestLongCallsReturnWithinTheBudget(t *testing.T) {
	budget := waitBudget
	t.Cleanup(func() { waitBudget = budget }) // after the servers close
	waitBudget = 200 * time.Millisecond

	server, _, tokens := setup(t)
	subs := &fakeSubagents{block: make(chan struct{})}
	server.SetSubagents(subs)
	if got, err := Call(server.Path, tokens["builder-1"], "wait", nil); err != nil || !strings.Contains(fmt.Sprint(got), "switch_wait again") {
		t.Fatalf("wait on a running subagent = %v, %v", got, err)
	}
	close(subs.block)
	if got, err := Call(server.Path, tokens["builder-1"], "wait", nil); err != nil || !strings.Contains(fmt.Sprint(got), "found it") || strings.Contains(fmt.Sprint(got), "again") {
		t.Fatalf("wait once it finished = %v, %v", got, err)
	}

	dir, err := os.MkdirTemp("", "swb")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	team := &fakeTeam{answer: make(chan struct{})}
	chat, err := Listen(Options{RunID: "teamkey5", StateDir: dir, Team: team, Executable: "/bin/switch"})
	if err != nil {
		t.Fatal(err)
	}
	defer chat.Close()
	grant, _ := chat.Grant(store.RunAgent{Name: "lead", Role: "lead"}, "")
	token := strings.TrimPrefix(grant.Env[1], EnvToken+"=")
	ask := func(to, message string) (any, error) {
		return Call(chat.Path, token, "ask", map[string]string{"to": to, "message": message})
	}
	for _, to := range []string{"builder-1", "builder-2"} {
		if got, err := ask(to, "thoughts?"); err != nil || !strings.Contains(fmt.Sprint(got), "still answering") {
			t.Fatalf("slow ask = %v, %v", got, err)
		}
	}
	if _, err := ask("builder-1", "something else"); err == nil || !strings.Contains(err.Error(), "earlier question") {
		t.Fatalf("another question while the first is answered: %v", err)
	}
	close(team.answer)
	if got, err := ask("builder-1", "thoughts?"); err != nil || got != "answer from builder-1" {
		t.Fatalf("asking again = %v, %v", got, err)
	}
	if got, err := ask("builder-1", "something else"); err != nil || got != "answer from builder-1" || len(team.asked) != 3 {
		t.Fatalf("next question = %v, %v; asked %v, want each question asked once", got, err, team.asked)
	}
	// builder-2's answer went uncollected for longer than a budget, so the
	// same question later is a new one.
	time.Sleep(2 * waitBudget)
	if got, err := ask("builder-2", "thoughts?"); err != nil || got != "answer from builder-2" || len(team.asked) != 4 {
		t.Fatalf("the same question later = %v, %v; asked %v, want it asked again", got, err, team.asked)
	}
}

// A worker asks the user without ending its turn: the call returns within
// the budget, and asking again waits on without asking twice until the
// answer comes. A question goes with the turn that asked it; in the chat a
// teammate asks the same way.
func TestQuestionWaitsForTheUser(t *testing.T) {
	budget := waitBudget
	t.Cleanup(func() { waitBudget = budget }) // after the servers close
	waitBudget = 200 * time.Millisecond

	server, _, tokens := setup(t)
	subs := &fakeSubagents{questions: make(chan string, 4), answers: make(chan string, 1)}
	server.SetSubagents(subs)
	ask := func(socket, token, question string, options ...string) (any, error) {
		return Call(socket, token, "question", map[string]any{"message": question, "options": options})
	}
	for _, bad := range []map[string]any{{"message": " "}, {"message": "which?", "options": strings.Fields("1 2 3 4 5 6 7 8 9 10")}} {
		if _, err := Call(server.Path, tokens["builder-1"], "question", bad); err == nil {
			t.Errorf("question %v was asked", bad)
		}
	}
	if got, err := ask(server.Path, tokens["builder-1"], "which db?", "pg", "mysql"); err != nil || !strings.Contains(fmt.Sprint(got), "switch_question again") {
		t.Fatalf("unanswered question = %v, %v", got, err)
	}
	if got := <-subs.questions; got != "builder-1 on run:a asks which db? [pg mysql]" {
		t.Fatalf("the user was asked %q", got)
	}
	if _, err := ask(server.Path, tokens["builder-1"], "which cache?"); err == nil || !strings.Contains(err.Error(), "earlier question") {
		t.Fatalf("another question while the first waits: %v", err)
	}
	subs.answers <- "pg"
	if got, err := ask(server.Path, tokens["builder-1"], "which db?", "pg", "mysql"); err != nil || got != "pg" {
		t.Fatalf("asking again = %v, %v; want the answer", got, err)
	}
	select {
	case got := <-subs.questions:
		t.Fatalf("asked again: %s", got)
	default:
	}

	grant, _ := server.Grant(store.RunAgent{Name: "builder-2", Role: "builder"}, "run:b")
	if _, err := ask(server.Path, strings.TrimPrefix(grant.Env[1], EnvToken+"="), "ready?"); err != nil {
		t.Fatal(err)
	}
	<-subs.questions
	grant.Revoke()
	if got := <-subs.questions; got != "gave up on ready?" {
		t.Fatalf("once the turn ended: %s", got)
	}
	// The next turn asking the same is asked anew, not handed the give-up.
	grant, _ = server.Grant(store.RunAgent{Name: "builder-2", Role: "builder"}, "run:c")
	if _, err := ask(server.Path, strings.TrimPrefix(grant.Env[1], EnvToken+"="), "ready?"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-subs.questions:
		if got != "builder-2 on run:c asks ready? []" {
			t.Fatalf("the next turn's question: %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the next turn got the earlier turn's give-up; the user was never asked")
	}
	grant.Revoke()
	<-subs.questions

	chat, err := Listen(Options{RunID: "teamkey9", StateDir: t.TempDir(), Team: &fakeTeam{}, Executable: "/bin/switch"})
	if err != nil {
		t.Fatal(err)
	}
	defer chat.Close()
	chat.SetSubagents(subs)
	mate, _ := chat.Grant(store.RunAgent{Name: "builder-1", Role: "builder"}, "")
	subs.answers <- "yes"
	if got, err := ask(chat.Path, strings.TrimPrefix(mate.Env[1], EnvToken+"="), "ship it?"); err != nil || got != "yes" || <-subs.questions != "builder-1 on  asks ship it? []" {
		t.Fatalf("a teammate's question = %v, %v", got, err)
	}
}

// A switch_ask the asker cancelled leaves the teammate answering: asking
// again collects that answer instead of asking twice, and another question
// replaces it.
func TestCancelledAskKeepsTheAnswer(t *testing.T) {
	dir, err := os.MkdirTemp("", "swc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	team := &fakeTeam{answer: make(chan struct{}), started: make(chan string, 2)}
	chat, err := Listen(Options{RunID: "teamkey6", StateDir: dir, Team: team, Executable: "/bin/switch"})
	if err != nil {
		t.Fatal(err)
	}
	defer chat.Close()
	grant, _ := chat.Grant(store.RunAgent{Name: "lead", Role: "lead"}, "")
	token := strings.TrimPrefix(grant.Env[1], EnvToken+"=")
	for _, to := range []string{"builder-1", "builder-2"} {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-team.started; cancel() }()
		if _, err := call(ctx, chat.Path, token, "ask", map[string]string{"to": to, "message": "q"}); err == nil {
			t.Fatal("a cancelled ask succeeded")
		}
	}
	close(team.answer)
	time.Sleep(100 * time.Millisecond) // a call still waiting for the answers would take them now
	if got, err := Call(chat.Path, token, "ask", map[string]string{"to": "builder-1", "message": "q"}); err != nil || got != "answer from builder-1" || len(team.asked) != 2 {
		t.Fatalf("asking again = %v, %v; asked %v, want the answer to the first ask", got, err, team.asked)
	}
	if got, err := Call(chat.Path, token, "ask", map[string]string{"to": "builder-2", "message": "next"}); err != nil || got != "answer from builder-2" || len(team.asked) != 3 {
		t.Fatalf("a new question = %v, %v; asked %v", got, err, team.asked)
	}
}
