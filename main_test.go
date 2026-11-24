package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/config"
	controlplane "github.com/urizennnn/switch/internal/control"
	"github.com/urizennnn/switch/internal/store"
)

// An agent's shell carries its turn's token; it may look at the run, but
// not change it.
func TestAgentsCannotChangeTheRunFromTheirShell(t *testing.T) {
	t.Setenv(controlplane.EnvToken, "turn-token")
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // keep a missing check away from real state
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	empty := t.TempDir() // were a check missing, each command would stop at a flag it lacks, in an empty folder
	for _, command := range []string{"run", "stop", "plan", "approve", "reject", "retry", "prune", "status", "tasks", "diff", "log", "summary"} {
		stderr := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = w
		code := control([]string{command, "--cwd", empty, "--file", "missing.json", "--run", "none", "a"})
		os.Stderr = stderr
		w.Close()
		out, _ := io.ReadAll(r)
		refused := code != 0 && strings.Contains(string(out), "only the user")
		if looks := slices.Contains(strings.Fields("status tasks diff log summary"), command); refused == looks {
			t.Errorf("%s from an agent: exit %d, %q", command, code, out)
		}
	}
}

func TestResolveLeadIgnoresBareAdapterName(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer state.Close()

	ctx := context.Background()
	project := t.TempDir()
	cfg := config.Config{Agents: []config.Agent{
		{Name: "lead", Role: "lead", Driver: "opencode"},
		{Name: "builder-1", Role: "builder", Driver: "claude"},
	}}

	// An older build could store a bare adapter name as the lead.
	if err := state.SetLeadAgent(ctx, project, "codex"); err != nil {
		t.Fatal(err)
	}
	if got := resolveLead(ctx, cfg, state, project); got.Name != "lead" {
		t.Fatalf("stored adapter name: lead = %q, want config default", got.Name)
	}

	if err := state.SetLeadAgent(ctx, project, "builder-1"); err != nil {
		t.Fatal(err)
	}
	if got := resolveLead(ctx, cfg, state, project); got.Name != "builder-1" {
		t.Fatalf("stored agent name: lead = %q, want builder-1", got.Name)
	}

	// With no lead stored, the chat's driver picks it, as an older build spelled it.
	other := t.TempDir()
	if err := state.SaveChatSession(ctx, store.ChatSession{Project: other, Agent: "Claude-Code", SessionID: "ses_c"}); err != nil {
		t.Fatal(err)
	}
	if got := resolveLead(ctx, cfg, state, other); got.Name != "builder-1" {
		t.Fatalf("chat stored under Claude-Code: lead = %q, want builder-1", got.Name)
	}
}

func TestSessionLeadFindsTheOwner(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	project := t.TempDir()
	cfg := config.Config{Agents: []config.Agent{
		{Name: "lead", Role: "lead", Driver: "opencode"},
		{Name: "builder-1", Role: "builder", Driver: "claude"},
	}}
	if err := state.SaveChatSession(ctx, store.ChatSession{Project: project, Name: "builder-1", Agent: "claude", SessionID: "ses_b"}); err != nil {
		t.Fatal(err)
	}
	_ = state.AppendMessage(ctx, project, "ses_b", "user", "hello")

	if agent, err := sessionLead(ctx, cfg, state, project, "ses_b"); err != nil || agent.Name != "builder-1" {
		t.Fatalf("sessionLead = %q, %v", agent.Name, err)
	}
	if _, err := sessionLead(ctx, cfg, state, t.TempDir(), "ses_b"); err == nil {
		t.Fatal("found a session from another folder")
	}

	// Older builds stored no agent name, and the driver as the config spelled it.
	if err := state.SaveChatSession(ctx, store.ChatSession{Project: project, Agent: "Claude-Code", SessionID: "ses_old"}); err != nil {
		t.Fatal(err)
	}
	_ = state.AppendMessage(ctx, project, "ses_old", "user", "earlier")
	if agent, err := sessionLead(ctx, cfg, state, project, "ses_old"); err != nil || agent.Name != "builder-1" {
		t.Fatalf("sessionLead of a session stored under Claude-Code = %q, %v", agent.Name, err)
	}
}
