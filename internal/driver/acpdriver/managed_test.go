package acpdriver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedMetadata(t *testing.T) {
	codex, ok := Managed("codex")
	if !ok || codex.Package == "" {
		t.Fatalf("codex management = %#v, ok %v", codex, ok)
	}
	opencode, ok := Managed("opencode")
	if !ok || !opencode.External {
		t.Fatalf("opencode management = %#v, ok %v", opencode, ok)
	}
	if _, ok := Managed("nope"); ok {
		t.Fatal("unknown adapter should not be managed")
	}
}

func TestInstallAndUninstallCommands(t *testing.T) {
	dir := t.TempDir()
	npm := filepath.Join(dir, "npm")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\"\n"
	if err := os.WriteFile(npm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := Install(context.Background(), "codex")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !strings.Contains(out, "install -g "+codexACPPackage+"@"+codexACPVersion) {
		t.Fatalf("install args = %q", out)
	}

	out, err = Uninstall(context.Background(), "codex")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !strings.Contains(out, "uninstall -g "+codexACPPackage) {
		t.Fatalf("uninstall args = %q", out)
	}

	if _, err := Install(context.Background(), "opencode"); err == nil {
		t.Fatal("external adapter install should fail")
	}
}
