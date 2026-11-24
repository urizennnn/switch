package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorChecksEveryAgentOfTheTeam(t *testing.T) {
	home, bin, project := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin") // no real adapters
	t.Cleanup(func() { acpdriver.SetCustom(nil) })
	// Answers initialize, then runs until its stdin closes. It also stands in
	// for a codex-acp installed globally at another version than the pin.
	fake := filepath.Join(bin, "codex-acp")
	write(t, fake, `#!/bin/sh
read -r _; echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentInfo":{"name":"fake","version":"1.2.3"}}}'; cat >/dev/null`)
	write(t, filepath.Join(home, "switch", "config.toml"),
		fmt.Sprintf("[drivers.good]\ncommand = [%q]\n\n[drivers.gone]\ncommand = [%q]\n", fake, filepath.Join(bin, "missing")))
	team := `agent = "gone" # the legacy key, which no agent uses

[[agents]]
name = "lead"
role = "lead"
driver = "good"
`
	write(t, config.ProjectConfigPath(project), team)
	report, ok, err := Report(project)
	if err != nil || !ok || !strings.Contains(report, "fake 1.2.3") || strings.Contains(report, "warning") {
		t.Fatalf("a working team: ok %v, err %v\n%s", ok, err, report)
	}

	write(t, config.ProjectConfigPath(project), team+"\n[[agents]]\nname = \"builder-1\"\ndriver = \"Gone\"\n")
	report, ok, err = Report(project)
	if err != nil || ok || !strings.Contains(report, "warning: gone does not start") {
		t.Fatalf("a builder whose adapter is missing: ok %v, err %v\n%s", ok, err, report)
	}

	write(t, config.ProjectConfigPath(project), team+"\n[run.subagents]\ndrivers = [\"gone\"]\n")
	report, ok, err = Report(project)
	if err != nil || ok || !strings.Contains(report, "warning: gone does not start") {
		t.Fatalf("subagents may use a missing adapter: ok %v, err %v\n%s", ok, err, report)
	}
	write(t, config.ProjectConfigPath(project), team+"\n[run.subagents]\nmax_per_builder = 0\ndrivers = [\"gone\"]\n")
	if report, ok, err = Report(project); err != nil || !ok {
		t.Fatalf("subagents turned off: ok %v, err %v\n%s", ok, err, report)
	}

	write(t, config.ProjectConfigPath(project), team+"\n[[agents]]\nname = \"builder-1\"\ndriver = \"codex\"\n")
	report, ok, err = Report(project)
	if err != nil || !ok || !strings.Contains(report, "codex-acp 1.2.3 is installed globally, not the pinned") {
		t.Fatalf("a global codex-acp that is not the pin: ok %v, err %v\n%s", ok, err, report)
	}
}
