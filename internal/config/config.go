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
	Agent string      `toml:"agent"`
	Model string      `toml:"model"`
	Rules RulesConfig `toml:"rules"`
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
