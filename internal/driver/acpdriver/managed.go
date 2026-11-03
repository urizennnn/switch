package acpdriver

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type Management struct {
	Name     string
	Package  string
	Version  string
	External bool
}

func Managed(name string) (Management, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "codex":
		return Management{Name: "codex", Package: codexACPPackage, Version: codexACPVersion}, true
	case "claude", "claude-code":
		return Management{Name: "claude", Package: claudeACPPackage, Version: claudeACPVersion}, true
	case "openclaw":
		return Management{Name: "openclaw", Package: "openclaw", Version: "latest"}, true
	case "opencode":
		return Management{Name: "opencode", External: true}, true
	default:
		return Management{}, false
	}
}

func Install(ctx context.Context, name string) (string, error) {
	management, ok := Managed(name)
	if !ok {
		return "", fmt.Errorf("unknown adapter %q", name)
	}
	if management.External {
		return "", fmt.Errorf("%s is managed externally", name)
	}
	target := management.Package
	if management.Version != "" {
		target += "@" + management.Version
	}
	return runNPM(ctx, "install", "-g", target)
}

func Uninstall(ctx context.Context, name string) (string, error) {
	management, ok := Managed(name)
	if !ok {
		return "", fmt.Errorf("unknown adapter %q", name)
	}
	if management.External {
		return "", fmt.Errorf("%s is managed externally", name)
	}
	return runNPM(ctx, "uninstall", "-g", management.Package)
}

func runNPM(ctx context.Context, args ...string) (string, error) {
	if _, err := exec.LookPath("npm"); err != nil {
		return "", fmt.Errorf("npm is not installed")
	}
	cmd := exec.CommandContext(ctx, "npm", args...)
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		return text, fmt.Errorf("npm %s: %w", strings.Join(args, " "), err)
	}
	return text, nil
}
