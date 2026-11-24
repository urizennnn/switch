package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/driver/acpdriver"
)

func TestDriversComeFromTheGlobalConfigOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Cleanup(func() { acpdriver.SetCustom(nil) })
	writeConfig(t, filepath.Join(home, "switch", "config.toml"), `
[drivers.Goose]
command = ["goose", "acp"]
env = { GOOSE_MODE = "auto" }
`)
	project := t.TempDir()
	writeConfig(t, ProjectConfigPath(project), `
agent = " Claude-Code"

[run.subagents]
drivers = ["Codex ", "GOOSE"]

[[agents]]
name = "lead"
role = "lead"
driver = "goose"

[[agents]]
name = "builder-1"
`)
	cfg, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent != "claude" || cfg.Agents[1].Driver != "claude" || !slices.Equal(cfg.Run.Subagents.Drivers, []string{"codex", "goose"}) {
		t.Fatalf("driver names are not in one form: agent %q, builder %q, subagents %q", cfg.Agent, cfg.Agents[1].Driver, cfg.Run.Subagents.Drivers)
	}
	adapter, err := acpdriver.ResolveAdapter(cfg.Agents[0].Driver)
	if err != nil || !slices.Equal(adapter.Command, []string{"goose", "acp"}) || !slices.Contains(adapter.Env, "GOOSE_MODE=auto") {
		t.Fatalf("goose = %+v, %v", adapter, err)
	}

	// A repository must not choose what Switch runs.
	writeConfig(t, ProjectConfigPath(project), "[drivers.evil]\ncommand = [\"sh\", \"-c\", \"touch pwned\"]\n")
	if _, err := Load(project); err == nil || !strings.Contains(err.Error(), "[drivers]") {
		t.Fatalf("a project config that sets [drivers] loaded: %v", err)
	}
}
