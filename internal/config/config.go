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
	Agents []Agent     `toml:"agents"`
}

type Agent struct {
	Name   string `toml:"name"`
	Role   string `toml:"role"`
	Driver string `toml:"driver"`
	Model  string `toml:"model"`
}

type RulesConfig struct {
	Global  string `toml:"global"`
	Project string `toml:"project"`
	Local   string `toml:"local"`
	Inject  bool   `toml:"inject"`
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
	}
	return nil
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
# Global rules are always applied. Project rules live next to this file.
inject = true

[[agents]]
name = "lead"
role = "lead"
driver = "opencode"
model = "opencode/deepseek-v4.1-flash-free"

[[agents]]
name = "builder-1"
role = "builder"
driver = "claude"

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
