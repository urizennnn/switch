package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
)

// Agent selection and rule defaults for a project.
type Config struct {
	Agent  string      `toml:"agent"`
	Model  string      `toml:"model"`
	Rules  RulesConfig `toml:"rules"`
	Run    RunConfig   `toml:"run"`
	Agents []Agent     `toml:"agents"`
	// Drivers run ACP agents Switch has no adapter for, or replace how a
	// built-in one is launched. Only the global config may set them.
	Drivers map[string]Driver `toml:"drivers"`
}

type Driver struct {
	Command []string          `toml:"command"`
	Env     map[string]string `toml:"env"`
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
	// IdleTimeout stops a worker turn that sends no events this long, in seconds.
	IdleTimeout int `toml:"idle_timeout"`
	MaxTurns    int `toml:"max_turns"`
	MaxTokens   int `toml:"max_tokens"`
	// HandoffAt is the share of its context window, in percent, at which an
	// agent is told to hand off; 0 never warns.
	HandoffAt int `toml:"handoff_at"`
	// Setup runs in a task's worktree before its first attempt, and in the
	// integration worktree before its checks, such as to install dependencies.
	Setup []string `toml:"setup"`
	// Verify runs on the integration branch with each merge's checks, and once
	// more before the run is completed.
	Verify []string `toml:"verify"`
	// Subagents bounds the helpers a worker may start for its task.
	Subagents SubagentConfig `toml:"subagents"`
	// Notify is a shell command run when a detached or headless run needs the
	// user, with SWITCH_EVENT, SWITCH_RUN, SWITCH_TASK and SWITCH_MESSAGE set.
	Notify string `toml:"notify"`
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
	// Escalate is the agent that gets one more attempt at this one's task
	// when its attempts run out, instead of the task being blocked.
	Escalate string `toml:"escalate"`
}

type RulesConfig struct {
	Global  string `toml:"global"`
	Project string `toml:"project"`
	Local   string `toml:"local"`
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
		},
		Run: RunConfig{
			MaxConcurrency: 2,
			MaxAttempts:    3,
			LeadReview:     true,
			VerifyTimeout:  600,
			TurnTimeout:    1800,
			IdleTimeout:    600,
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
	// Global rules are never shown in the project rules review, so a
	// repository must not choose which of your files count as them.
	globalRules, globalAgents := rulePath(cfg.Rules.Global, GlobalDir()), cfg.Agents
	cfg.Agents = nil // toml decodes into existing elements, so a project fleet would inherit global fields

	drivers, notify := cfg.Drivers, cfg.Run.Notify
	cfg.Drivers = nil
	project := ProjectConfigPath(projectDir)
	if err := decodeIfExists(project, &cfg); err != nil {
		return Config{}, fmt.Errorf("project config %s: %w", project, err)
	}
	if cfg.Drivers != nil {
		return Config{}, fmt.Errorf("project config %s: [drivers] choose what Switch runs, so a repository may not set them; put them in %s", project, global)
	}
	if cfg.Run.Notify != notify {
		return Config{}, fmt.Errorf("project config %s: run.notify runs a command on your machine, so a repository may not set it; put it in %s", project, global)
	}
	cfg.Drivers = drivers
	cfg.Rules.Global = globalRules
	cfg.Rules.Project, cfg.Rules.Local = rulePath(cfg.Rules.Project, projectDir), rulePath(cfg.Rules.Local, projectDir)
	if cfg.Agents == nil {
		cfg.Agents = globalAgents
	}
	if cfg.Run.HandoffAt < 0 || cfg.Run.HandoffAt > 100 {
		return Config{}, fmt.Errorf("handoff_at = %d: use a percentage from 0 (never) to 100", cfg.Run.HandoffAt)
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
	custom := map[string]acpdriver.Adapter{}
	for name, driver := range cfg.Drivers {
		key := acpdriver.DriverName(name)
		if len(driver.Command) == 0 {
			return Config{}, fmt.Errorf("driver %q needs a command", name)
		}
		if _, dup := custom[key]; dup {
			return Config{}, fmt.Errorf("driver %q is configured twice", key)
		}
		adapter := acpdriver.Adapter{Command: driver.Command}
		for k, v := range driver.Env {
			adapter.Env = append(adapter.Env, k+"="+v)
		}
		custom[key] = adapter
	}
	acpdriver.SetCustom(custom) // sessions start by driver name alone (a run stores only that)
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
	meta, err := toml.DecodeFile(path, target)
	if err != nil {
		return err
	}
	var unknown []string
	for _, key := range meta.Undecoded() {
		if key.String() != "rules.inject" { // older templates wrote it; it never did anything
			unknown = append(unknown, key.String())
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("unknown setting %s", strings.Join(unknown, ", "))
	}
	return nil
}

// rulePath makes a rule path absolute: ~ is your home, and a relative path
// is relative to dir, so it never depends on the process cwd.
func rulePath(path, dir string) string {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[1:])
		}
	}
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
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
	c.Agent = acpdriver.DriverName(c.Agent)
	for i, driver := range c.Run.Subagents.Drivers {
		c.Run.Subagents.Drivers[i] = acpdriver.DriverName(driver)
	}
	seen := make(map[string]bool, len(c.Agents))
	for i := range c.Agents {
		agent := &c.Agents[i]
		agent.Name = strings.TrimSpace(agent.Name)
		agent.Role = strings.TrimSpace(agent.Role)
		agent.Driver = acpdriver.DriverName(agent.Driver)
		agent.Model = strings.TrimSpace(agent.Model)
		if agent.Name == "" {
			return fmt.Errorf("agent %d: name is required", i+1)
		}
		if badName(agent.Name) {
			return fmt.Errorf("agent %q: names cannot contain spaces, slashes or control characters", agent.Name)
		}
		if seen[agent.Name] {
			return fmt.Errorf("duplicate agent name %q", agent.Name)
		}
		seen[agent.Name] = true
		if agent.Role == "" {
			agent.Role = "builder"
		}
		if badName(agent.Role) {
			return fmt.Errorf("agent %q: role %q cannot contain spaces, slashes or control characters", agent.Name, agent.Role)
		}
		if agent.Driver == "" {
			agent.Driver = c.Agent
		}
		if agent.Model == "" && agent.Driver == c.Agent {
			agent.Model = c.Model
		}
		agent.Handoff = strings.TrimSpace(agent.Handoff)
		agent.Escalate = strings.TrimSpace(agent.Escalate)
	}
	for _, agent := range c.Agents {
		if agent.Handoff == agent.Name || (agent.Handoff != "" && !seen[agent.Handoff]) {
			return fmt.Errorf("agent %q: handoff %q must be another configured agent", agent.Name, agent.Handoff)
		}
		if agent.Escalate == agent.Name || (agent.Escalate != "" && !seen[agent.Escalate]) {
			return fmt.Errorf("agent %q: escalate %q must be another configured agent", agent.Name, agent.Escalate)
		}
	}
	return nil
}

// badName reports a name that cannot name a rule file or be typed as one
// argument, such as /lead builder-1.
func badName(name string) bool {
	return name == "." || name == ".." || strings.ContainsFunc(name, func(r rune) bool {
		return r == '/' || r == '\\' || unicode.IsSpace(r) || unicode.IsControl(r)
	})
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

// Escalations maps each agent to the one that gets one more attempt at its
// task when its attempts run out; agents without one are left out.
func Escalations(fleet []Agent) map[string]string {
	targets := map[string]string{}
	for _, agent := range fleet {
		if agent.Escalate != "" {
			targets[agent.Name] = agent.Escalate
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
# Settings left commented out come from your global config
# (~/.config/switch/config.toml), else Switch's defaults; ones set here win.
# agent = "opencode"
# model = "opencode/deepseek-v4.1-flash-free"

[rules]
# Rules apply in order global -> project -> local; later ones win.
# Project rules: .switch/rules.md, plus .switch/rules/roles/<role>.md,
# .switch/rules/agents/<agent>.md and .switch/rules/tasks/<task>.md.
# Personal rules for this project: .switch/rules.local.md (keep it out of git).
# ignore_global = true   # leave out your global rules in this project

[run]
# max_concurrency = 2   # tasks running at once
# max_attempts = 3      # worker attempts per task before it is blocked
# lead_review = true    # the lead reviews each verified diff before you approve
# verify_timeout = 600  # seconds per verification command
# turn_timeout = 1800   # seconds per worker or review turn
# idle_timeout = 600    # seconds a worker turn may go without any agent activity
# max_turns = 50        # agent turns per run: workers, reviews, subagents (0 = unlimited)
# max_tokens = 0        # tokens per run (0 = unlimited)
# handoff_at = 80       # % of its context at which an agent is told to hand off (0 = never)
# setup = ["npm ci"]    # run in each new task worktree and before integration checks; gitignore what it writes
# verify = ["npm test"] # run on the integration branch with every merge, and before the run completes
# notify, the command run when a detached or headless run needs you, belongs in your global config.

[run.subagents]
# max_per_builder = 3   # helpers each worker may run at once (0 = off)
# drivers = ["claude", "codex"]  # adapters workers may start (default: any)

# The team; without one here or in your global config it is lead, builder-1
# and builder-2, all on agent. A list here replaces your global one whole.
# [[agents]]
# name = "lead"
# role = "lead"
# driver = "opencode"
# model = "opencode/deepseek-v4.1-flash-free"
#
# [[agents]]
# name = "builder-1"
# role = "builder"
# driver = "claude"
# handoff = "builder-2"  # continues builder-1's work when it runs out (default: the lead)
# escalate = "builder-2" # gets one more attempt when builder-1's attempts run out (default: blocked)
#
# [[agents]]
# name = "builder-2"
# role = "builder"
# driver = "codex"
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

// ProjectRoot is the project dir belongs to: the nearest folder, up to the
// git root, that has a .switch folder, else the git root. Outside a git
// repository it is dir itself.
func ProjectRoot(dir string) string {
	dir = cleanDir(dir)
	nearest := ""
	for at := dir; ; at = filepath.Dir(at) {
		if info, err := os.Stat(filepath.Join(at, ".switch")); err == nil && info.IsDir() && nearest == "" {
			nearest = at
		}
		if exists(filepath.Join(at, ".git")) {
			if nearest != "" {
				return nearest
			}
			return at
		}
		if filepath.Dir(at) == at {
			return dir
		}
	}
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
