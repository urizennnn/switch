package rulescli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

// After /lead builder-1, show prints what the chat sends builder-1: the
// lead's rules.
func TestShowFollowsTheChosenLead(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project := t.TempDir()
	if _, err := config.EnsureProject(project); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"lead": "LEAD RULE", "builder": "BUILDER RULE"} {
		path := filepath.Join(project, ".switch", "rules", "roles", name+".md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	state, err := store.Open(config.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetLeadAgent(context.Background(), project, "builder-1"); err != nil {
		t.Fatal(err)
	}
	trust(t, state, project)
	state.Close()

	for _, args := range [][]string{{"show"}, {"show", "builder-1"}} {
		var out strings.Builder
		if err := run(project, args, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "LEAD RULE") || strings.Contains(out.String(), "BUILDER RULE") {
			t.Fatalf("%v:\n%s", args, out.String())
		}
	}
}

// Chat and runs leave out project rules changed since the user accepted
// them, so show must too.
func TestShowLeavesOutUnacceptedProjectRules(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project := t.TempDir()
	if _, err := config.EnsureProject(project); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, ".switch", "rules", "roles", "builder.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("OLD RULE"), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(config.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	trust(t, state, project)
	state.Close()
	if err := os.WriteFile(path, []byte("CHANGED RULE"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := run(project, []string{"show", "builder-1"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "RULE") || strings.Contains(out.String(), "Prefer small") {
		t.Fatalf("show printed unaccepted project rules:\n%s", out.String())
	}
}

// trust accepts the project's rules as they are now, as the TUI review does.
func trust(t *testing.T, state *store.Store, project string) {
	t.Helper()
	cfg, err := config.Load(project)
	if err != nil {
		t.Fatal(err)
	}
	base, err := rules.Load(cfg.Rules)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := rules.Project(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetTrustedRules(context.Background(), project, accepted.Hash()); err != nil {
		t.Fatal(err)
	}
}
