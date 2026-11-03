package rules

import (
	"os"
	"path/filepath"
	"strings"
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

	builder, err := Scoped(base, "builder", "", "add-flag")
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
	other, _ := Scoped(base, "tester", "", "other")
	if len(other.Sources) != 1 {
		t.Fatalf("tester scopes = %v, want project only", scopes(other))
	}
	// Path traversal in a plan-supplied task ID is ignored.
	escaped, _ := Scoped(base, "builder", "", "../../rules")
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

func TestGlobalProjectLocalAndAgentScopes(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, "rules.md"), "<!-- a note, never sent -->\nglobal rule")
	write(filepath.Join(home, "rules/roles/lead.md"), "global lead rule")
	write(filepath.Join(home, "rules/agents/claude.md"), "global claude rule")
	write(filepath.Join(project, ".switch/rules.md"), "project rule")
	write(filepath.Join(project, ".switch/rules.local.md"), "local rule")
	write(filepath.Join(project, ".switch/rules/roles/lead.md"), "project lead rule")
	write(filepath.Join(project, ".switch/rules/agents/claude.md"), "<!-- only a comment -->")
	cfg := config.RulesConfig{
		Global:  filepath.Join(home, "rules.md"),
		Project: filepath.Join(project, ".switch/rules.md"),
		Local:   filepath.Join(project, ".switch/rules.local.md"),
	}
	base, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Scoped(base, "lead", "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "global project local global role:lead role:lead global agent:claude"
	if got := strings.Join(scopes(set), " "); got != want {
		t.Fatalf("scopes = %q, want %q", got, want)
	}
	if strings.Contains(set.Text(), "never sent") {
		t.Fatal("HTML comments must not be sent")
	}

	// Edits are picked up without reloading by hand, and change the hash.
	before := set.Hash()
	write(filepath.Join(project, ".switch/rules.md"), "project rule, edited")
	after, _ := Scoped(base, "lead", "claude", "")
	if after.Hash() == before {
		t.Fatal("hash did not change after an edit")
	}

	// Declined project rules stay out, even after a reload.
	declined, _ := Scoped(base.WithoutProject(), "lead", "claude", "")
	if got := strings.Join(scopes(declined), " "); got != "global global role:lead global agent:claude" {
		t.Fatalf("declined scopes = %q", got)
	}

	// A project can turn global rules off.
	cfg.IgnoreGlobal = true
	noGlobal, _ := Load(cfg)
	scoped, _ := Scoped(noGlobal, "lead", "claude", "")
	if got := strings.Join(scopes(scoped), " "); got != "project local role:lead" {
		t.Fatalf("ignore_global scopes = %q", got)
	}
	if _, err := noGlobal.Target("global/roles/lead"); err == nil {
		t.Fatal("global target allowed with ignore_global")
	}

	// The trust check covers every project file, including unused ones.
	all, _ := Project(base)
	if got := strings.Join(scopes(all), " "); got != "project local role:lead" {
		t.Fatalf("project scopes = %q", got)
	}
}

func TestTarget(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	base, _ := Load(config.RulesConfig{
		Global:  filepath.Join(home, "rules.md"),
		Project: filepath.Join(project, ".switch/rules.md"),
		Local:   filepath.Join(project, ".switch/rules.local.md"),
	})
	for target, want := range map[string]string{
		"global":              filepath.Join(home, "rules.md"),
		"local":               filepath.Join(project, ".switch/rules.local.md"),
		"roles/reviewer":      filepath.Join(project, ".switch/rules/roles/reviewer.md"),
		"agents/builder-1.md": filepath.Join(project, ".switch/rules/agents/builder-1.md"),
		"global/agents/codex": filepath.Join(home, "rules/agents/codex.md"),
	} {
		if got, err := base.Target(target); err != nil || got != want {
			t.Errorf("Target(%q) = %q, %v; want %q", target, got, err, want)
		}
	}
	for _, bad := range []string{"roles/../x", "roles/", "global/tasks/x", "secrets/x", "roles/a/b"} {
		if _, err := base.Target(bad); err == nil {
			t.Errorf("Target(%q) accepted", bad)
		}
	}
}
