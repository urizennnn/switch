package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/store"
)

// A review turn gets no Switch tools, and every other turn's grant ends with
// the turn, even when its adapter never starts.
func TestTurnGrantEndsWithTheTurn(t *testing.T) {
	control := &countingControl{}
	e := &Engine{opts: Options{Control: control}}
	missing := filepath.Join(t.TempDir(), "missing") // the session fails to open before any adapter starts
	for _, review := range []bool{true, false} {
		if _, err := e.acpTurn(context.Background(), TurnRequest{Agent: lead, Cwd: missing, Ephemeral: review}); err == nil {
			t.Fatal("a turn started in a missing directory")
		}
	}
	if control.grants != 1 || control.revokes != 1 {
		t.Fatalf("grants = %d, revokes = %d; want one for the worker turn, revoked", control.grants, control.revokes)
	}
}

// A worker whose adapter cannot be steered mid-turn gets what it was sent at
// the start of its next turn on the same task, not another task's; an agent
// with no turn running is refused.
func TestSteerWaitsForTheNextPromptWhenTheAdapterCannotTakeIt(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1})
	e := f.engine(config.RunConfig{})
	prompts := filepath.Join(t.TempDir(), "prompts")
	// Records each prompt and ends the turn; it says nothing of steering.
	script := `read -r _; echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
read -r _; echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s1"}}'
read -r line; printf '%s\n' "$line" >> '` + prompts + `'; echo '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}'
cat >/dev/null`
	acpdriver.SetCustom(map[string]acpdriver.Adapter{"fake": {Command: []string{"sh", "-c", script}}})
	t.Cleanup(func() { acpdriver.SetCustom(nil) })
	ctx := context.Background()

	if _, err := e.Steer(ctx, "builder-1", "use v2"); err == nil {
		t.Fatal("steered an agent with no turn running")
	}
	var reply string
	e.opts.OnEvent = func(agent string, event acpdriver.Event) {
		if event.Type == acpdriver.EventStart {
			reply, _ = e.Steer(ctx, agent, "use v2")
		}
	}
	turn := func(task, prompt string) {
		if _, err := e.acpTurn(ctx, TurnRequest{Agent: builder1, TaskID: task, Cwd: t.TempDir(), Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	turn("run:a", "first")
	if !strings.Contains(reply, "next turn on a") {
		t.Fatalf("steer during the turn = %q", reply)
	}
	if _, err := e.Steer(ctx, "builder-1", "late"); err == nil {
		t.Fatal("steered a turn that ended")
	}
	e.opts.OnEvent = nil
	turn("run:b", "other")
	turn("run:a", "second")
	data, _ := os.ReadFile(prompts)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || strings.Contains(lines[0], "use v2") || strings.Contains(lines[1], "use v2") || !strings.Contains(lines[2], "previous turn: use v2") ||
		strings.Index(lines[2], "use v2") > strings.Index(lines[2], "second") || strings.Contains(lines[2], "late") {
		t.Fatalf("prompts:\n%s", data)
	}
}

type countingControl struct{ grants, revokes int }

func (c *countingControl) Grant(store.RunAgent, string) (Grant, error) {
	c.grants++
	return Grant{Revoke: func() { c.revokes++ }}, nil
}

func (c *countingControl) TakeBlock(string) (string, bool) { return "", false }

// With no one to ask, as in a headless or detached run, a worker reads its
// own worktree, and what build mode would ask the user about, its edits
// included, gets the run's configured answer.
func TestUnattendedWorkersGetBuildModesAnswer(t *testing.T) {
	deny, allow := acpdriver.PermissionDeny, acpdriver.PermissionAllow
	worker := TurnRequest{Agent: builder1, Permission: deny}
	subagent := TurnRequest{Agent: store.RunAgent{Name: "builder-1/scout", Role: "subagent"}, Permission: deny}
	review := TurnRequest{Agent: lead, Permission: deny, Ephemeral: true}
	for _, c := range []struct {
		turn    TurnRequest
		request acpdriver.PermissionRequest
		want    acpdriver.PermissionMode
	}{
		{worker, acpdriver.PermissionRequest{Kind: "edit", Inside: true}, deny}, // verify would run what it wrote
		{TurnRequest{Agent: builder1, Permission: allow}, acpdriver.PermissionRequest{Kind: "edit", Inside: true}, allow},
		{worker, acpdriver.PermissionRequest{Kind: "read", Inside: true}, allow},
		{worker, acpdriver.PermissionRequest{Kind: "read"}, deny}, // outside its worktree, such as ~/.ssh
		{worker, acpdriver.PermissionRequest{Kind: "edit"}, deny},
		{worker, acpdriver.PermissionRequest{Kind: "execute", Inside: true}, deny},
		{TurnRequest{Agent: builder1, Permission: allow}, acpdriver.PermissionRequest{Kind: "read"}, allow},
		// A subagent reads its worker's worktree, and asks before editing it.
		{subagent, acpdriver.PermissionRequest{Kind: "read", Inside: true}, allow},
		{subagent, acpdriver.PermissionRequest{Kind: "edit", Inside: true}, deny},
		// A lead review only reads: its turn's answer stands for everything.
		{review, acpdriver.PermissionRequest{Kind: "edit", Inside: true}, deny},
	} {
		if got := unattended(c.turn, &c.request); got != c.want {
			t.Errorf("%s (%s, ephemeral %v) %s inside=%v with %s: %s, want %s", c.turn.Agent.Name, c.turn.Agent.Role, c.turn.Ephemeral, c.request.Kind, c.request.Inside, c.turn.Permission, got, c.want)
		}
	}
}
