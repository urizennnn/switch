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

// Run prints local configuration, adapter, rules, and state diagnostics.
func Run(args []string) int {
	project := "."
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		project = args[0]
	}
	project, err := filepath.Abs(project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: %v\n", err)
		return 1
	}

	fmt.Printf("switch doctor\nversion: %s\nproject: %s\n\n", buildversion.Version, project)

	cfg, err := config.Load(project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	projectConfig := config.ProjectConfigPath(project)
	globalConfig := filepath.Join(config.GlobalDir(), "config.toml")
	fmt.Printf("global config:  %s (%s)\n", globalConfig, state(globalConfig))
	fmt.Printf("project config: %s (%s)\n", projectConfig, state(projectConfig))
	fmt.Printf("agent:          %s\n", cfg.Agent)
	if cfg.Model != "" {
		fmt.Printf("model:          %s\n", cfg.Model)
	}
	fmt.Printf("state:          %s\n", config.StatePath())
	fmt.Println()

	ruleSet, err := rules.Load(cfg.Rules)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rules: %v\n", err)
		return 1
	}
	if ruleSet.Empty() {
		fmt.Println("rules: none loaded")
	} else {
		for _, source := range ruleSet.Sources {
			fmt.Printf("rule: %-7s %s\n", source.Scope, source.Path)
		}
	}
	fmt.Println()

	fmt.Println("adapters:")
	acpdriver.PrintAdapterStatuses(os.Stdout)
	fmt.Println()

	stateStore, err := store.Open(config.StatePath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "state: %v\n", err)
		return 1
	}
	defer stateStore.Close()
	if _, ok, err := stateStore.GetChatSession(context.Background(), project); err != nil {
		fmt.Fprintf(os.Stderr, "state session: %v\n", err)
	} else if ok {
		fmt.Println("chat session: found")
	} else {
		fmt.Println("chat session: none (run `switch chat`)")
	}

	for _, status := range acpdriver.AdapterStatuses() {
		if status.Name == cfg.Agent {
			if !status.Available {
				fmt.Fprintf(os.Stderr, "\nwarning: configured agent %q is not available\n", cfg.Agent)
				return 1
			}
			return 0
		}
	}
	return 0
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
