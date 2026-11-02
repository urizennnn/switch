package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrInitCreatesProjectFiles(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()

	cfg, created, err := LoadOrInit(project)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("created %d files, want 2: %#v", len(created), created)
	}
	if cfg.Agent != "opencode" {
		t.Fatalf("agent = %q, want opencode", cfg.Agent)
	}
	if _, err := os.Stat(filepath.Join(project, ".switch", "config.toml")); err != nil {
		t.Fatalf("project config missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".switch", "rules.md")); err != nil {
		t.Fatalf("project rules missing: %v", err)
	}

	_, createdAgain, err := LoadOrInit(project)
	if err != nil {
		t.Fatalf("second LoadOrInit: %v", err)
	}
	if len(createdAgain) != 0 {
		t.Fatalf("second LoadOrInit created files: %#v", createdAgain)
	}
}
