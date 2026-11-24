package acpdriver

import (
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
)

// Adapter describes how to launch a supported ACP agent.
type Adapter struct {
	Name        string
	Command     []string
	Env         []string // added to the adapter's environment, as KEY=VALUE
	InstallHint string
	AuthHint    string
	// NoSessionMCP: the adapter rejects per-session MCP servers, so agents
	// reach the control plane through the shell CLI only.
	NoSessionMCP bool
	// SessionKeyPrefix is sent as _meta.sessionKey when resuming, for adapters
	// that cannot map a session ID back to their own key after a restart.
	SessionKeyPrefix string
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

// DriverName is the one form of a driver name that Switch compares: trimmed,
// lower case, with aliases resolved.
func DriverName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "claude-code" {
		return "claude"
	}
	return name
}

var (
	customMu sync.Mutex
	custom   map[string]Adapter
)

// SetCustom sets the drivers configured with their own command, keyed by
// DriverName, replacing the earlier set. One named like a built-in adapter
// replaces its command and keeps its quirks.
func SetCustom(adapters map[string]Adapter) {
	customMu.Lock()
	defer customMu.Unlock()
	custom = adapters
}

func customAdapter(name string) (Adapter, bool) {
	customMu.Lock()
	defer customMu.Unlock()
	adapter, ok := custom[name]
	return adapter, ok
}

// ResolveAdapter returns the launch command for a named agent.
func ResolveAdapter(name string) (Adapter, error) {
	name = DriverName(name)
	adapter, err := builtinAdapter(name)
	if own, ok := customAdapter(name); ok {
		adapter.Name, adapter.Command = name, own.Command
		adapter.Env = append(adapter.Env, own.Env...)
		adapter.InstallHint = "check the command of [drivers." + name + "] in your Switch config"
		return adapter, nil
	}
	return adapter, err
}

func builtinAdapter(name string) (Adapter, error) {
	switch name {
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

	case "claude":
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
			Env:         []string{"OPENCLAW_HIDE_BANNER=1", "OPENCLAW_SUPPRESS_NOTES=1"},
			InstallHint: "install OpenClaw and start its gateway with `openclaw gateway start`",
			AuthHint:    "ensure the OpenClaw gateway is running and authenticated",
			// The ACP bridge refuses session MCP servers and, after a bridge
			// restart, resumes by bare session ID instead of the acp-bridge:<id>
			// key it created the session under.
			NoSessionMCP:     true,
			SessionKeyPrefix: "acp-bridge:",
		}, nil

	default:
		return Adapter{}, fmt.Errorf("unknown agent %q (supported: opencode, codex, claude, openclaw, or a [drivers] entry in your Switch config)", name)
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
	builtins := len(names)
	customMu.Lock()
	for name := range custom {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	customMu.Unlock()
	slices.Sort(names[builtins:])
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
