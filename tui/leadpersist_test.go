package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/store"
)

func TestPersistLeadWritesImmediately(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer state.Close()

	project := t.TempDir()
	m := model{store: state, project: project}

	if err := m.persistLead("builder-1"); err != nil {
		t.Fatalf("persistLead: %v", err)
	}
	name, ok, err := state.LeadAgent(context.Background(), project)
	if err != nil {
		t.Fatalf("LeadAgent: %v", err)
	}
	if !ok || name != "builder-1" {
		t.Fatalf("LeadAgent = %q, %v; want builder-1, true", name, ok)
	}

	if err := m.persistLead("builder-2"); err != nil {
		t.Fatalf("persistLead overwrite: %v", err)
	}
	name, ok, err = state.LeadAgent(context.Background(), project)
	if err != nil {
		t.Fatalf("LeadAgent after overwrite: %v", err)
	}
	if !ok || name != "builder-2" {
		t.Fatalf("LeadAgent after overwrite = %q, %v; want builder-2, true", name, ok)
	}
}

// A teammate answering an @mention or a switch_ask has its session in use:
// /lead and the lead picker wait for the answer, and say so.
func TestLeadSwitchWaitsForATeammatesAnswer(t *testing.T) {
	installAdapter(t, "exit 1") // the adapter is installed; nothing starts it
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "claude"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: t.TempDir(), Lead: lead, Fleet: []config.Agent{lead, builder}})
	defer m.cancel()
	m.team.asked["builder-1"] = true

	next, cmd := m.handleSlash("/lead builder-1")
	if got := next.(model); cmd != nil || got.lead.Name != "lead" || !strings.Contains(got.toastText(), "answering") {
		t.Fatalf("/lead builder-1 while it answers: lead %q, toast %q", got.lead.Name, got.toastText())
	}
	next, _ = m.handleSlash("/lead")
	m = next.(model)
	m.picker.index = 1
	next, cmd = m.Update(keyEnter())
	if got := next.(model); cmd != nil || got.lead.Name != "lead" || got.picker == nil || !strings.Contains(got.picker.err, "answering") {
		t.Fatalf("picking builder-1 while it answers: lead %q, picker %+v", got.lead.Name, got.picker)
	}
}

func TestLeadCommandRejectsBareAdapterName(t *testing.T) {
	m := model{
		lead: config.Agent{Name: "lead", Role: "lead", Driver: "opencode"},
		fleet: []config.Agent{
			{Name: "lead", Role: "lead", Driver: "opencode"},
			{Name: "builder-1", Role: "builder", Driver: "claude"},
		},
	}
	next, cmd := m.handleSlash("/lead codex")
	got := next.(model)
	if cmd != nil || got.busy {
		t.Fatal("/lead with an adapter name started a switch")
	}
	if got.lead.Name != "lead" {
		t.Fatalf("lead = %q, want unchanged", got.lead.Name)
	}
	if !strings.Contains(got.toastText(), "unknown agent") {
		t.Fatalf("toast = %q, want unknown agent", got.toastText())
	}
}
