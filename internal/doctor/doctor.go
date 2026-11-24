package doctor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	acpdriver.PrintAdapterStatuses(&b)
	available := checkTeam(&b, cfg, project)

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
		b.WriteString("\nchat session: none (run `switch`)\n")
	}

	return b.String(), available, nil
}

// checkTeam starts each adapter the team uses, once, to see that it speaks
// ACP and which version it is. It reports whether all of them do.
func checkTeam(w io.Writer, cfg config.Config, project string) bool {
	type use struct{ who, driver string }
	var uses []use
	for _, agent := range cfg.Fleet() {
		uses = append(uses, use{agent.Name, agent.Driver})
	}
	if cfg.Run.Subagents.MaxPerBuilder > 0 {
		for _, driver := range cfg.Run.Subagents.Drivers {
			uses = append(uses, use{"subagents", driver})
		}
	}

	type check struct {
		report acpdriver.CapabilitiesReport
		err    error
	}
	checks := map[string]*check{}
	var drivers []string
	var wg sync.WaitGroup
	for _, u := range uses {
		if checks[u.driver] != nil {
			continue
		}
		c := &check{}
		checks[u.driver] = c
		drivers = append(drivers, u.driver)
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			c.report, c.err = acpdriver.Handshake(ctx, u.driver, project)
		})
	}
	wg.Wait()

	fmt.Fprintln(w, "\nteam:")
	for _, u := range uses {
		c := checks[u.driver]
		result := strings.TrimSpace(c.report.DisplayName + " " + c.report.Version)
		if c.err != nil {
			result = "does not start"
		}
		fmt.Fprintf(w, "%-12s %-10s %s\n", u.who, u.driver, result)
	}

	sources := map[string]string{}
	for _, status := range acpdriver.AdapterStatuses() {
		sources[status.Name] = status.Source
	}
	ok := true
	for _, driver := range drivers {
		c := checks[driver]
		if c.err != nil {
			ok = false
			fmt.Fprintf(w, "\nwarning: %s does not start: %v\n", driver, c.err)
			if adapter, err := acpdriver.ResolveAdapter(driver); err == nil && adapter.InstallHint != "" {
				fmt.Fprintf(w, "  %s\n", adapter.InstallHint)
			}
			continue
		}
		// A global install runs instead of the pinned version, whatever it is.
		pin, _ := acpdriver.Managed(driver)
		if sources[driver] == "installed" && pin.Version != "" && pin.Version != "latest" && c.report.Version != pin.Version {
			fmt.Fprintf(w, "\nwarning: %s %s is installed globally, not the pinned %s; `npm install -g %s@%s` restores it\n",
				pin.Binary, c.report.Version, pin.Version, pin.Package, pin.Version)
		}
	}
	return ok
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
