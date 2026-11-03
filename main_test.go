package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/store"
)

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
}
