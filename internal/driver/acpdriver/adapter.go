package acpdriver

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Adapter describes how to launch a supported ACP agent.
type Adapter struct {
	Name        string
	Command     []string
	InstallHint string
	AuthHint    string
}

// AdapterStatus is the result of probing the local installation.
type AdapterStatus struct {
	Name        string
	Command     []string
	Available   bool
	Source      string
	InstallHint string
	AuthHint    string
}

const (
	codexACPVersion  = "2.5.0"
	claudeACPVersion = "0.100.0"
	codexACPPackage  = "@agentclientprotocol/codex-acp"
	claudeACPPackage = "@agentclientprotocol/claude-agent-acp"
	codexACPBinary   = "codex-acp"
	claudeACPBinary  = "claude-agent-acp"
	opencodeBinary   = "opencode"
	openclawBinary   = "openclaw"
	npxBinary        = "npx"
)

// ResolveAdapter returns the launch command for a named agent.
func ResolveAdapter(name string) (Adapter, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "opencode":
		return Adapter{
			Name:     "opencode",
			Command:  []string{opencodeBinary, "acp"},
			AuthHint: "run `opencode auth login` if the probe reports authentication required",
		}, nil

	case "codex":
		return Adapter{
			Name:        "codex",
			Command:     resolveWithFallback(codexACPBinary, codexACPPackage, codexACPVersion),
			InstallHint: "install with `npm install -g " + codexACPPackage + "@" + codexACPVersion + "` (or keep npx available)",
			AuthHint:    "log in with `codex login`, or set CODEX_API_KEY / OPENAI_API_KEY",
		}, nil

	case "claude", "claude-code":
		return Adapter{
			Name:        "claude",
			Command:     resolveWithFallback(claudeACPBinary, claudeACPPackage, claudeACPVersion),
			InstallHint: "install with `npm install -g " + claudeACPPackage + "@" + claudeACPVersion + "` (or keep npx available)",
			AuthHint:    "log in with `claude`, or set ANTHROPIC_API_KEY",
		}, nil

	case "openclaw":
		return Adapter{
			Name:        "openclaw",
			Command:     []string{openclawBinary, "acp"},
			InstallHint: "install OpenClaw and start its gateway with `openclaw gateway start`",
			AuthHint:    "ensure the OpenClaw gateway is running and authenticated",
		}, nil

	default:
		return Adapter{}, fmt.Errorf("unknown agent %q (supported: opencode, codex, claude, openclaw)", name)
	}
}

// resolveWithFallback prefers a globally installed adapter binary and falls
// back to npx with a pinned package version.
func resolveWithFallback(binary, pkg, version string) []string {
	if _, err := exec.LookPath(binary); err == nil {
		return []string{binary}
	}
	return []string{npxBinary, "-y", pkg + "@" + version}
}

// AdapterStatuses reports which adapters are locally available.
func AdapterStatuses() []AdapterStatus {
	names := []string{"opencode", "codex", "claude", "openclaw"}
	statuses := make([]AdapterStatus, 0, len(names))
	for _, name := range names {
		adapter, err := ResolveAdapter(name)
		if err != nil {
			continue
		}
		status := AdapterStatus{
			Name:        adapter.Name,
			Command:     adapter.Command,
			InstallHint: adapter.InstallHint,
			AuthHint:    adapter.AuthHint,
		}
		if len(adapter.Command) == 0 {
			statuses = append(statuses, status)
			continue
		}
		if _, err := exec.LookPath(adapter.Command[0]); err == nil {
			status.Available = true
			if adapter.Command[0] == npxBinary {
				status.Source = "npx"
			} else {
				status.Source = "installed"
			}
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// PrintAdapterStatuses writes a human-readable availability table.
func PrintAdapterStatuses(w io.Writer) {
	for _, status := range AdapterStatuses() {
		state := "missing"
		if status.Available {
			state = status.Source
		}
		fmt.Fprintf(w, "%-10s %-10s %s\n", status.Name, state, strings.Join(status.Command, " "))
		if !status.Available && status.InstallHint != "" {
			fmt.Fprintf(w, "%-10s %-10s %s\n", "", "", status.InstallHint)
		}
	}
}
