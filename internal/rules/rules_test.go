package rules

import (
	"errors"
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

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// What a person reviews is what agents get: escapes, carriage returns and
// invisible characters cannot hide text in either.
func TestControlCharactersAreStripped(t *testing.T) {
	project := t.TempDir()
	rulesFile := filepath.Join(project, ".switch/rules.md")
	writeFile(t, rulesFile, "seen\x1b[8m hidden\x1b[0m\r\b\u202Eflipped\u200B\U000E0041\uFE0F\x07")
	writeFile(t, filepath.Join(project, ".switch/rules/roles/a\x1b[8mb.md"), "role rule")
	base, err := Load(config.RulesConfig{Project: rulesFile})
	if err != nil {
		t.Fatal(err)
	}
	all, err := Project(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{base.Text(), base.Prompt("hi"), all.Text()} {
		if strings.IndexFunc(text, func(r rune) bool {
			return r != '\n' && r < ' ' || r == '\u202E' || r == '\u200B' || r == '\U000E0041' || r == '\uFE0F'
		}) >= 0 {
			t.Fatalf("control characters survived: %q", text)
		}
	}
	if !strings.Contains(base.Text(), "seen hiddenflipped") {
		t.Fatalf("text = %q", base.Text())
	}
}

// A worker's subagent gets the worker's rules plus roles/subagent.md, in the
// run and in the chat alike; a rule file it cannot read leaves it the global
// rules only.
func TestSubagentRules(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(home, "rules.md"), "global rule")
	writeFile(t, filepath.Join(project, ".switch/rules.md"), "project rule")
	writeFile(t, filepath.Join(project, ".switch/rules/roles/builder.md"), "builder rule")
	writeFile(t, filepath.Join(project, ".switch/rules/roles/subagent.md"), "subagent rule")
	writeFile(t, filepath.Join(project, ".switch/rules/roles/lead.md"), "lead-only rule")
	writeFile(t, filepath.Join(project, ".switch/rules/tasks/t1.md"), "task rule")
	base, err := Load(config.RulesConfig{Global: filepath.Join(home, "rules.md"), Project: filepath.Join(project, ".switch/rules.md")})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(scopes(Subagent(base, "builder", "builder-1", "t1")), " "); got != "global project role:builder task:t1 role:subagent" {
		t.Fatalf("scopes = %q", got)
	}
	if err := os.MkdirAll(filepath.Join(project, ".switch/rules/agents/builder-1.md"), 0o755); err != nil {
		t.Fatal(err) // a folder where its rule file would be cannot be read
	}
	if got := strings.Join(scopes(Subagent(base, "builder", "builder-1", "t1")), " "); got != "global" {
		t.Fatalf("unreadable rules: scopes = %q, want global only", got)
	}
}

// Declined (or unset) project rules never fall back to roles/, agents/ or
// tasks/ in the working directory.
func TestNoProjectReadsNothingFromTheWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	writeFile(t, filepath.Join(cwd, "roles/lead.md"), "repo root role")
	writeFile(t, filepath.Join(cwd, "agents/claude.md"), "repo root agent")
	writeFile(t, filepath.Join(cwd, "tasks/t1.md"), "repo root task")
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "rules.md"), "global rule")
	for name, cfg := range map[string]config.RulesConfig{
		"declined": {Global: filepath.Join(home, "rules.md"), Project: filepath.Join(cwd, ".switch/rules.md")},
		"unset":    {Global: filepath.Join(home, "rules.md")},
	} {
		base, err := Load(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if name == "declined" {
			base = base.WithoutProject()
		}
		set, err := Scoped(base, "lead", "claude", "t1")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(scopes(set), " "); got != "global" {
			t.Fatalf("%s: scopes = %q, want global only", name, got)
		}
	}
}

// Project rules are sent only while they are exactly the ones the user
// accepted; a change or a failed check leaves them out until reviewed.
func TestTrustGatesProjectRules(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(home, "rules.md"), "global rule")
	writeFile(t, filepath.Join(project, ".switch/rules.md"), "project rule")
	writeFile(t, filepath.Join(project, ".switch/rules/roles/lead.md"), "project lead rule")
	cfg := config.RulesConfig{
		Global:  filepath.Join(home, "rules.md"),
		Project: filepath.Join(project, ".switch/rules.md"),
		Local:   filepath.Join(project, ".switch/rules.local.md"),
	}
	base, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	accepted, lookupErr := "", error(nil)
	gated := base.Trust(func() (string, error) { return accepted, lookupErr })
	sent := func() string {
		t.Helper()
		set, err := Scoped(gated, "lead", "", "")
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(scopes(set), " ")
	}

	if got := sent(); got != "global" || !gated.Untrusted() {
		t.Fatalf("never accepted: scopes = %q, untrusted = %v", got, gated.Untrusted())
	}
	all, _ := Project(base)
	accepted = all.Hash()
	if got := sent(); got != "global project role:lead" || gated.Untrusted() {
		t.Fatalf("accepted: scopes = %q, untrusted = %v", got, gated.Untrusted())
	}

	writeFile(t, filepath.Join(project, ".switch/rules/roles/lead.md"), "changed mid-session")
	if got := sent(); got != "global" {
		t.Fatalf("changed: scopes = %q, want project rules left out", got)
	}
	if reloaded, _ := gated.Reload(); !reloaded.Untrusted() {
		t.Fatal("Reload dropped the trust check")
	}
	changed, _ := Project(base)
	accepted = changed.Hash()
	if got := sent(); got != "global project role:lead" {
		t.Fatalf("change accepted: scopes = %q", got)
	}

	// Text read before a change is not sent, even when the change is accepted.
	writeFile(t, filepath.Join(project, ".switch/rules.md"), "edited after load")
	stale := base.Trust(func() (string, error) { p, _ := Project(base); return p.Hash(), nil })
	if !stale.Untrusted() {
		t.Fatal("a set holding text that is no longer on disk counted as trusted")
	}

	current, _ := Project(base)
	accepted, lookupErr = current.Hash(), errors.New("state database locked")
	if got := sent(); got != "global" {
		t.Fatalf("failed check: scopes = %q, want project rules left out", got)
	}
}

// Accepted rules are sent however the path to them is spelled: a project
// folder whose name looks like a glob, or an agent named in another case
// than its file on a case-insensitive file system (the macOS default).
func TestTrustedRulesSurviveHowPathsAreSpelled(t *testing.T) {
	project := filepath.Join(t.TempDir(), "Repo [old]")
	writeFile(t, filepath.Join(project, ".switch/rules.md"), "project rule")
	writeFile(t, filepath.Join(project, ".switch/rules/roles/lead.md"), "project lead rule")
	writeFile(t, filepath.Join(project, ".switch/rules/agents/claude.md"), "project claude rule")
	base, err := Load(config.RulesConfig{Project: filepath.Join(project, ".switch/rules.md")})
	if err != nil {
		t.Fatal(err)
	}
	all, err := Project(base)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(scopes(all), " "); got != "project role:lead agent:claude" {
		t.Fatalf("review scopes = %q, want every project file", got)
	}
	gated := base.Trust(func() (string, error) { return all.Hash(), nil })
	for _, agent := range []string{"claude", "Claude"} {
		set, err := Scoped(gated, "lead", agent, "")
		if err != nil {
			t.Fatal(err)
		}
		// A case-sensitive file system has no file for Claude, only the rest.
		if got := strings.Join(scopes(set), " "); !strings.HasPrefix(got, "project role:lead") {
			t.Fatalf("%s: scopes = %q, want the accepted project rules", agent, got)
		}
	}
}
