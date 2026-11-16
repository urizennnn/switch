package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Agent selection and rule defaults for a project.
type Config struct {
	Agent  string      `toml:"agent"`
	Model  string      `toml:"model"`
	Rules  RulesConfig `toml:"rules"`
	Run    RunConfig   `toml:"run"`
	Agents []Agent     `toml:"agents"`
}

// RunConfig bounds task execution. Zero budgets mean unlimited.
type RunConfig struct {
	MaxConcurrency int  `toml:"max_concurrency"`
	MaxAttempts    int  `toml:"max_attempts"`
	LeadReview     bool `toml:"lead_review"`
	// VerifyTimeout is per verification command, in seconds.
	VerifyTimeout int `toml:"verify_timeout"`
	// TurnTimeout bounds one worker or review turn, in seconds.
	TurnTimeout int `toml:"turn_timeout"`
	MaxTurns    int `toml:"max_turns"`
	MaxTokens   int `toml:"max_tokens"`
	// HandoffAt is the share of its context window, in percent, at which an
	// agent is told to hand off; 0 never warns.
	HandoffAt int `toml:"handoff_at"`
	// Subagents bounds the helpers a worker may start for its task.
	Subagents SubagentConfig `toml:"subagents"`
}

type SubagentConfig struct {
	MaxPerBuilder int      `toml:"max_per_builder"` // running at once per worker; 0 turns subagents off
	Drivers       []string `toml:"drivers"`         // adapters workers may start; empty means any
}

type Agent struct {
	Name   string `toml:"name"`
	Role   string `toml:"role"`
	Driver string `toml:"driver"`
	Model  string `toml:"model"`
	// Handoff is the agent that continues this one's work when it runs out
	// of context or hits a usage limit.
	Handoff string `toml:"handoff"`
}

type RulesConfig struct {
	Global  string `toml:"global"`
	Project string `toml:"project"`
	Local   string `toml:"local"`
	Inject  bool   `toml:"inject"`
	// IgnoreGlobal leaves out the user's global rules, for a project whose
	// conventions conflict with them.
	IgnoreGlobal bool `toml:"ignore_global"`
}

func Defaults(projectDir string) Config {
	projectDir = cleanDir(projectDir)
	return Config{
		Agent: "opencode",
		Rules: RulesConfig{
			Global:  filepath.Join(GlobalDir(), "rules.md"),
			Project: filepath.Join(projectDir, ".switch", "rules.md"),
			Local:   filepath.Join(projectDir, ".switch", "rules.local.md"),
			Inject:  true,
		},
		Run: RunConfig{
			MaxConcurrency: 2,
			MaxAttempts:    3,
			LeadReview:     true,
			VerifyTimeout:  600,
			TurnTimeout:    1800,
			MaxTurns:       50,
			HandoffAt:      80,
			Subagents:      SubagentConfig{MaxPerBuilder: 3},
		},
	}
}

// Load merges the global config, the project config, and environment overrides.
func Load(projectDir string) (Config, error) {
	projectDir = cleanDir(projectDir)
	cfg := Defaults(projectDir)

	global := filepath.Join(GlobalDir(), "config.toml")
	if err := decodeIfExists(global, &cfg); err != nil {
		return Config{}, fmt.Errorf("global config %s: %w", global, err)
	}

	project := ProjectConfigPath(projectDir)
	if err := decodeIfExists(project, &cfg); err != nil {
		return Config{}, fmt.Errorf("project config %s: %w", project, err)
	}

	if value := strings.TrimSpace(os.Getenv("SWITCH_AGENT")); value != "" {
		cfg.Agent = value
	}
	if value := strings.TrimSpace(os.Getenv("SWITCH_MODEL")); value != "" {
		cfg.Model = value
	}
	if cfg.Agent == "" {
		cfg.Agent = "opencode"
	}
	if err := cfg.normalizeFleet(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func decodeIfExists(path string, target any) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("is a directory")
	}
	_, err = toml.DecodeFile(path, target)
	return err
}

// LoadOrInit creates the project config when it is missing, then loads the
// merged configuration. This is the default path for interactive commands.
func LoadOrInit(projectDir string) (Config, []string, error) {
	created, err := EnsureProject(projectDir)
	if err != nil {
		return Config{}, nil, err
	}
	global, err := EnsureGlobal()
	if err != nil {
		return Config{}, nil, err
	}
	created = append(created, global...)
	cfg, err := Load(projectDir)
	if err != nil {
		return Config{}, nil, err
	}
	return cfg, created, nil
}

// Fleet returns the configured agents, or a default lead plus two builders.
// LeadAgent returns the configured lead, or the first fleet agent.
func (c Config) LeadAgent() Agent {
	fleet := c.Fleet()
	for _, agent := range fleet {
		if agent.Role == "lead" {
			return agent
		}
	}
	if len(fleet) > 0 {
		return fleet[0]
	}
	return Agent{Name: "lead", Role: "lead", Driver: c.Agent, Model: c.Model}
}

func (c Config) Fleet() []Agent {
	if len(c.Agents) > 0 {
		return c.Agents
	}
	return []Agent{
		{Name: "lead", Role: "lead", Driver: c.Agent, Model: c.Model},
		{Name: "builder-1", Role: "builder", Driver: c.Agent, Model: c.Model},
		{Name: "builder-2", Role: "builder", Driver: c.Agent, Model: c.Model},
	}
}

func (c *Config) normalizeFleet() error {
	seen := make(map[string]bool, len(c.Agents))
	for i := range c.Agents {
		agent := &c.Agents[i]
		agent.Name = strings.TrimSpace(agent.Name)
		agent.Role = strings.TrimSpace(agent.Role)
		agent.Driver = strings.TrimSpace(agent.Driver)
		agent.Model = strings.TrimSpace(agent.Model)
		if agent.Name == "" {
			return fmt.Errorf("agent %d: name is required", i+1)
		}
		if seen[agent.Name] {
			return fmt.Errorf("duplicate agent name %q", agent.Name)
		}
		seen[agent.Name] = true
		if agent.Role == "" {
			agent.Role = "builder"
		}
		if agent.Driver == "" {
			agent.Driver = c.Agent
		}
		if agent.Model == "" && agent.Driver == c.Agent {
			agent.Model = c.Model
		}
		agent.Handoff = strings.TrimSpace(agent.Handoff)
	}
	for _, agent := range c.Agents {
		if agent.Handoff == agent.Name || (agent.Handoff != "" && !seen[agent.Handoff]) {
			return fmt.Errorf("agent %q: handoff %q must be another configured agent", agent.Name, agent.Handoff)
		}
	}
	return nil
}

// Handoffs maps each agent to the one that continues its work; agents
// without a handoff are left out.
func Handoffs(fleet []Agent) map[string]string {
	targets := map[string]string{}
	for _, agent := range fleet {
		if agent.Handoff != "" {
			targets[agent.Name] = agent.Handoff
		}
	}
	return targets
}

// EnsureProject creates the project config and a starter rules file.
func EnsureProject(projectDir string) ([]string, error) {
	projectDir = cleanDir(projectDir)
	dir := filepath.Join(projectDir, ".switch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	var created []string
	configPath := filepath.Join(dir, "config.toml")
	if !exists(configPath) {
		content := `# Switch project configuration
agent = "opencode"
model = "opencode/deepseek-v4.1-flash-free"

[rules]
# Rules apply in order global -> project -> local; later ones win.
# Project rules: .switch/rules.md, plus .switch/rules/roles/<role>.md,
# .switch/rules/agents/<agent>.md and .switch/rules/tasks/<task>.md.
# Personal rules for this project: .switch/rules.local.md (keep it out of git).
inject = true
# ignore_global = true   # leave out your global rules in this project

[run]
max_concurrency = 2   # tasks running at once
max_attempts = 3      # worker attempts per task before it is blocked
lead_review = true    # the lead reviews each verified diff before you approve
verify_timeout = 600  # seconds per verification command
turn_timeout = 1800   # seconds per worker or review turn
max_turns = 50        # worker turns per run (0 = unlimited)
max_tokens = 0        # tokens per run (0 = unlimited)
handoff_at = 80       # % of its context at which an agent is told to hand off (0 = never)

[run.subagents]
max_per_builder = 3   # helpers each worker may run at once (0 = off)
# drivers = ["claude", "codex"]  # adapters workers may start (default: any)

[[agents]]
name = "lead"
role = "lead"
driver = "opencode"
model = "opencode/deepseek-v4.1-flash-free"

[[agents]]
name = "builder-1"
role = "builder"
driver = "claude"
handoff = "builder-2" # continues builder-1's work when it runs out (default: the lead)

[[agents]]
name = "builder-2"
role = "builder"
driver = "codex"
`
		if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
			return nil, err
		}
		created = append(created, configPath)
	}

	rulesPath := filepath.Join(dir, "rules.md")
	if !exists(rulesPath) {
		content := `# Project rules

Guidance in this file applies only to this project.

- Prefer small, reviewable changes.
- Run the relevant checks before claiming work is complete.
`
		if err := os.WriteFile(rulesPath, []byte(content), 0o644); err != nil {
			return nil, err
		}
		created = append(created, rulesPath)
	}
	return created, nil
}

// EnsureGlobal creates the global rules file on first run. It starts as a
// comment, which is never sent to agents.
func EnsureGlobal() ([]string, error) {
	path := filepath.Join(GlobalDir(), "rules.md")
	if exists(path) {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	content := `<!--
Global rules for Switch. Rules here apply to every project you open with Switch. Project rules
(.switch/rules.md) come after these and win when they conflict.

Rules for one role or one agent go in rules/roles/<role>.md and
rules/agents/<agent>.md next to this file. Comments like this one are
never sent to agents.
-->
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return []string{path}, nil
}

func GlobalDir() string {
	if value := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); value != "" {
		return filepath.Join(value, "switch")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "switch")
	}
	return filepath.Join(home, ".config", "switch")
}

func StateDir() string {
	if value := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); value != "" {
		return filepath.Join(value, "switch")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "state", "switch")
	}
	return filepath.Join(home, ".local", "state", "switch")
}

func StatePath() string {
	return filepath.Join(StateDir(), "switch.db")
}

func ProjectConfigPath(projectDir string) string {
	return filepath.Join(cleanDir(projectDir), ".switch", "config.toml")
}

func cleanDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
