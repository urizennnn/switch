package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
	buildversion "github.com/urizennnn/switch/internal/version"
)

// Run prints local diagnostics and returns a process exit code.
func Run(args []string) int {
	project := "."
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		project = args[0]
	}
	report, available, err := Report(project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: %v\n", err)
		return 1
	}
	fmt.Print(report)
	if !available {
		return 1
	}
	return 0
}

// Report returns configuration, adapter, rules, and state diagnostics.
func Report(project string) (string, bool, error) {
	project, err := filepath.Abs(project)
	if err != nil {
		return "", false, err
	}

	cfg, err := config.Load(project)
	if err != nil {
		return "", false, fmt.Errorf("config: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "switch doctor\nversion: %s\nproject: %s\n\n", buildversion.Version, project)

	projectConfig := config.ProjectConfigPath(project)
	globalConfig := filepath.Join(config.GlobalDir(), "config.toml")
	fmt.Fprintf(&b, "global config:  %s (%s)\n", globalConfig, state(globalConfig))
	fmt.Fprintf(&b, "project config: %s (%s)\n", projectConfig, state(projectConfig))
	fmt.Fprintf(&b, "agent:          %s\n", cfg.Agent)
	if cfg.Model != "" {
		fmt.Fprintf(&b, "model:          %s\n", cfg.Model)
	}
	fmt.Fprintf(&b, "state:          %s\n\n", config.StatePath())

	ruleSet, err := rules.Load(cfg.Rules)
	if err != nil {
		return "", false, fmt.Errorf("rules: %w", err)
	}
	if ruleSet.Empty() {
		b.WriteString("rules: none loaded\n")
	} else {
		for _, source := range ruleSet.Sources {
			fmt.Fprintf(&b, "rule: %-7s %s\n", source.Scope, source.Path)
		}
	}
	b.WriteString("\nadapters:\n")

	statuses := acpdriver.AdapterStatuses()
	acpdriver.PrintAdapterStatuses(&b)
	available := false
	for _, status := range statuses {
		if status.Name == cfg.Agent {
			available = status.Available
			break
		}
	}
	if !available {
		fmt.Fprintf(&b, "\nwarning: configured agent %q is not available\n", cfg.Agent)
	}

	stateStore, err := store.Open(config.StatePath())
	if err != nil {
		return "", available, fmt.Errorf("state: %w", err)
	}
	defer stateStore.Close()
	if _, ok, err := stateStore.GetChatSession(context.Background(), project); err != nil {
		fmt.Fprintf(&b, "\nstate session: %v\n", err)
	} else if ok {
		b.WriteString("\nchat session: found\n")
	} else {
		b.WriteString("\nchat session: none (run `switch` or `switch chat`)\n")
	}

	return b.String(), available, nil
}

func state(path string) string {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "missing"
		}
		return err.Error()
	}
	return "present"
}
