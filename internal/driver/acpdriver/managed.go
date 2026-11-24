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
	Binary   string // what the package puts on PATH
	External bool
}

func Managed(name string) (Management, bool) {
	name = DriverName(name)
	if _, ok := customAdapter(name); ok { // Switch runs the configured command, not the package
		return Management{Name: name, External: true}, true
	}
	switch name {
	case "codex":
		return Management{Name: "codex", Package: codexACPPackage, Version: codexACPVersion, Binary: codexACPBinary}, true
	case "claude":
		return Management{Name: "claude", Package: claudeACPPackage, Version: claudeACPVersion, Binary: claudeACPBinary}, true
	case "openclaw":
		return Management{Name: "openclaw", Package: "openclaw", Version: "latest", Binary: openclawBinary}, true
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
	output, err := runNPM(ctx, "install", "-g", target)
	if err != nil {
		return output, err
	}
	if _, err := exec.LookPath(management.Binary); err != nil {
		return output, fmt.Errorf("npm installed %s, but %s is not on PATH; add npm's global bin folder (under `npm prefix -g`) to PATH", target, management.Binary)
	}
	return output, nil
}

func Uninstall(ctx context.Context, name string) (string, error) {
	management, ok := Managed(name)
	if !ok {
		return "", fmt.Errorf("unknown adapter %q", name)
	}
	if management.External {
		return "", fmt.Errorf("%s is managed externally", name)
	}
	output, err := runNPM(ctx, "uninstall", "-g", management.Package)
	if err != nil {
		return output, err
	}
	// npm succeeds for a package it never installed, such as a Homebrew one.
	if path, err := exec.LookPath(management.Binary); err == nil {
		return output, fmt.Errorf("%s is still installed at %s, not by npm; remove it the way it was installed", management.Binary, path)
	}
	return output, nil
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
