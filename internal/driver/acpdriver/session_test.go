//go:build !windows

package acpdriver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as an ACP adapter, so sessions run against a real
// process without any agent installed.
func TestMain(m *testing.M) {
	if mode := os.Getenv("SWITCH_FAKE_ACP"); mode != "" {
		fakeAdapter(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeAdapter(mode string) {
	if path := os.Getenv("SWITCH_FAKE_PIDFILE"); path != "" { // a child that outlives the adapter
		child := exec.Command("sleep", "30")
		if child.Start() == nil {
			_ = os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
	}
	if mode == "crash" {
		fmt.Fprintln(os.Stderr, "first line")
		for i := range 5000 {
			fmt.Fprintf(os.Stderr, "noise %d\n", i)
		}
		fmt.Fprintln(os.Stderr, "fatal: out of cheese")
		fmt.Fprintln(os.Stderr, "    at handle (/x/index.js:429:15)")
	}
	out := json.NewEncoder(os.Stdout)
	reply := func(id json.RawMessage, result any) {
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	say := func(text, messageID string) {
		update := map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}}
		if messageID != "" {
			update["messageId"] = messageID
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": update}})
	}
	prompts := 0
	var turn json.RawMessage // the prompt a steer joins
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Prompt []struct{ Text string } `json:"prompt"`
			} `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil {
			continue
		}
		switch msg.Method {
		case "initialize":
			result := map[string]any{"protocolVersion": 1, "agentInfo": map[string]any{"name": "fake", "version": "1.2.3"}}
			if mode == "steer" {
				result["_meta"] = map[string]any{"steering": map[string]any{"supported": true}}
			}
			reply(msg.ID, result)
		case "_session/steering": // what codex-acp and claude-agent-acp answer
			if turn == nil {
				reply(msg.ID, map[string]any{"outcome": "promptRequired", "reason": "noRunningTurn"})
				break
			}
			reply(msg.ID, map[string]any{"outcome": "injected"})
			say(" steered: "+msg.Params.Prompt[0].Text, "m1")
			reply(turn, map[string]any{"stopReason": "end_turn"})
			turn = nil
		case "session/new":
			reply(msg.ID, map[string]any{"sessionId": "s1"})
			if mode == "deaf" { // it runs on but reads nothing more
				time.Sleep(20 * time.Second)
				return
			}
		case "session/prompt":
			prompts++
			switch {
			case mode == "abandon" && prompts == 1: // never answered
			case mode == "steer": // runs until it is steered
				turn = msg.ID
				say("working", "m1")
			case mode == "drop": // the connection drops while the adapter runs on
				_ = os.Stdout.Close()
			case mode == "crash":
				os.Exit(3)
			case mode == "limit" && prompts == 1: // the model's own words
				say("Retries now back off on 429", "m1")
				reply(msg.ID, map[string]any{"stopReason": "end_turn"})
			case mode == "limit": // how codex-acp reports a context-window stop
				say("Codex ran out of room in the model's context window. Start a new thread or clear earlier history before retrying.\n\n", "")
				reply(msg.ID, map[string]any{"stopReason": "end_turn"})
			default:
				say("pong", "m1")
				reply(msg.ID, map[string]any{"stopReason": "end_turn"})
			}
		}
	}
}

func openFake(t *testing.T, agent, mode string, env ...string) *Session {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Open's context bounds the adapter's whole life, not just its start.
	s, err := Open(context.Background(), Spec{Agent: agent, Command: []string{self}, Cwd: t.TempDir(), Env: append(env, "SWITCH_FAKE_ACP="+mode)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func turnContext(t *testing.T, timeout time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

func TestAbandonedTurnDoesNotFailTheNextOne(t *testing.T) {
	s := openFake(t, "fake", "abandon")
	if _, err := s.RunTurn(turnContext(t, 200*time.Millisecond), "hang", TurnOptions{}); err == nil {
		t.Fatal("a turn the adapter never answered succeeded")
	}
	reply, err := s.RunTurn(turnContext(t, 10*time.Second), "ping", TurnOptions{})
	if err != nil || reply != "pong" {
		t.Fatalf("next turn = %q, %v; want pong", reply, err)
	}
}

func TestDroppedConnectionEndsTheSession(t *testing.T) {
	s := openFake(t, "fake", "drop")
	if _, err := s.RunTurn(turnContext(t, 10*time.Second), "hi", TurnOptions{}); err == nil {
		t.Fatal("a turn on a dropped connection succeeded")
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session still looks alive after its connection dropped")
	}
}

func TestCloseStopsWhatTheAdapterLeftRunning(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	s := openFake(t, "fake", "ok", "SWITCH_FAKE_PIDFILE="+pidFile)
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(data))
	if !processAlive(pid) {
		t.Fatal("the adapter's child is not running")
	}
	_ = s.Close(context.Background())
	for deadline := time.Now().Add(3 * time.Second); processAlive(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("Close left the adapter's child running")
		}
	}
}

func TestAdapterExitReportsStatusAndStderrTail(t *testing.T) {
	s := openFake(t, "fake", "crash")
	_, err := s.RunTurn(turnContext(t, 10*time.Second), "hi", TurnOptions{})
	if err == nil {
		t.Fatal("a turn whose adapter exited succeeded")
	}
	for _, want := range []string{"exit status 3", "fatal: out of cheese"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %.200s", want, err)
		}
	}
	if stderr := s.Stderr(); strings.Contains(stderr, "first line") || len(stderr) > stderrTail {
		t.Errorf("stderr kept %d bytes, not just its tail", len(stderr))
	}
	if reason := LimitReason("", err); reason != "" {
		t.Errorf("a crash whose stderr mentions 429 counts as a limit: %s", reason)
	}
}

// A Prompt stuck writing to an adapter that stopped reading does not see its
// context, and the turn must end anyway.
func TestTurnEndsWhenTheAdapterStopsReading(t *testing.T) {
	s := openFake(t, "fake", "deaf")
	start := time.Now()
	if _, err := s.RunTurn(turnContext(t, 200*time.Millisecond), strings.Repeat("x", 1<<20), TurnOptions{}); err == nil {
		t.Fatal("a turn the adapter never read succeeded")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the turn ended %v after it started, not soon after its context", elapsed)
	}
}

func TestCodexLimitMessageStopsTheTurn(t *testing.T) {
	s := openFake(t, "codex", "limit")
	reply, err := s.RunTurn(turnContext(t, 10*time.Second), "first", TurnOptions{})
	if err != nil || !strings.Contains(reply, "429") {
		t.Fatalf("a reply that mentions a limit = %q, %v; want it as the reply", reply, err)
	}
	_, err = s.RunTurn(turnContext(t, 10*time.Second), "second", TurnOptions{})
	if LimitReason("", err) == "" {
		t.Fatalf("codex's context-window stop is not a limit: %v", err)
	}
}

func TestSteerJoinsTheRunningTurn(t *testing.T) {
	s := openFake(t, "fake", "steer")
	if !s.Caps.Steering {
		t.Fatal("the steering capability in initialize _meta was not read")
	}
	started := make(chan struct{})
	var once sync.Once
	done := make(chan string, 1)
	ctx := turnContext(t, 10*time.Second)
	go func() {
		reply, err := s.RunTurn(ctx, "go", TurnOptions{OnText: func(string) { once.Do(func() { close(started) }) }})
		if err != nil {
			reply = err.Error()
		}
		done <- reply
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never started")
	}
	if err := s.Steer(turnContext(t, 10*time.Second), "left"); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if reply := <-done; reply != "working steered: left" {
		t.Fatalf("turn = %q, want the steer in it", reply)
	}
	if err := s.Steer(turnContext(t, 10*time.Second), "late"); err == nil || !strings.Contains(err.Error(), "no turn is running") {
		t.Fatalf("a steer with no turn running = %v", err)
	}
}

func TestSteerNeedsAnAdapterThatSupportsIt(t *testing.T) {
	s := openFake(t, "fake", "ok")
	if err := s.Steer(turnContext(t, 10*time.Second), "left"); err == nil || !strings.Contains(err.Error(), "cannot steer") {
		t.Fatalf("Steer on an adapter without steering = %v", err)
	}
}

func TestConfiguredDriverRunsItsCommand(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	SetCustom(map[string]Adapter{
		"mine":     {Command: []string{self}, Env: []string{"SWITCH_FAKE_ACP=ok"}},
		"openclaw": {Command: []string{"/opt/openclaw", "acp"}},
	})
	t.Cleanup(func() { SetCustom(nil) })

	report, err := Handshake(turnContext(t, 10*time.Second), " Mine", t.TempDir())
	if err != nil || report.Agent != "mine" || report.Version != "1.2.3" {
		t.Fatalf("Handshake = %+v, %v", report, err)
	}
	s, err := Open(turnContext(t, 10*time.Second), Spec{Agent: "MINE", Cwd: t.TempDir()}) // its env picks the fake's mode
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if reply, err := s.RunTurn(turnContext(t, 10*time.Second), "ping", TurnOptions{}); err != nil || reply != "pong" {
		t.Fatalf("turn = %q, %v", reply, err)
	}

	var listed bool
	for _, status := range AdapterStatuses() {
		listed = listed || status.Name == "mine" && status.Available
	}
	if management, _ := Managed("mine"); !listed || !management.External {
		t.Fatalf("listed %v, management %+v: a configured driver is available and not npm's", listed, management)
	}
	// A built-in replaced by a command keeps what Switch knows about it.
	if claw, _ := ResolveAdapter("openclaw"); claw.Command[0] != "/opt/openclaw" || !claw.NoSessionMCP || len(claw.Env) == 0 {
		t.Fatalf("replaced openclaw = %+v", claw)
	}
}
