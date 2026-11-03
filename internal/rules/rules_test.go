package rules

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/urizennnn/switch/internal/config"
)

func TestScopedPrecedenceAndMissingFiles(t *testing.T) {
	project := t.TempDir()
	write := func(rel, text string) {
		path := filepath.Join(project, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".switch/rules.md", "project rule")
	write(".switch/rules/roles/builder.md", "builder rule")
	write(".switch/rules/roles/lead.md", "lead-only rule")
	write(".switch/rules/tasks/add-flag.md", "task rule")

	base, err := Load(config.RulesConfig{
		Global:  filepath.Join(project, "missing-global.md"),
		Project: filepath.Join(project, ".switch/rules.md"),
		Local:   filepath.Join(project, ".switch/rules.local.md"),
	})
	if err != nil {
		t.Fatal(err)
	}

	builder, err := Scoped(base, project, "builder", "add-flag")
	if err != nil {
		t.Fatal(err)
	}
	got := scopes(builder)
	want := []string{"project", "role:builder", "task:add-flag"}
	if len(got) != len(want) {
		t.Fatalf("scopes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scopes = %v, want %v", got, want)
		}
	}

	// No task file and an unknown role: only the base remains.
	other, _ := Scoped(base, project, "tester", "other")
	if len(other.Sources) != 1 {
		t.Fatalf("tester scopes = %v, want project only", scopes(other))
	}
	// Path traversal in a plan-supplied task ID is ignored.
	escaped, _ := Scoped(base, project, "builder", "../../rules")
	if len(escaped.Sources) != 2 {
		t.Fatalf("traversal scopes = %v", scopes(escaped))
	}
	// Scoped must not alias the caller's base slice.
	if len(base.Sources) != 1 {
		t.Fatalf("base mutated: %v", scopes(base))
	}
}

func scopes(set Set) []string {
	var out []string
	for _, s := range set.Sources {
		out = append(out, s.Scope)
	}
	return out
}
