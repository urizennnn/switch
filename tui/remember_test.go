package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

// /remember appends to the local rules file. Rules the user had accepted stay
// accepted, since the user wrote the new one; rules they never saw do not.
func TestRememberAddsALocalRule(t *testing.T) {
	if next, _ := (model{}).handleSlash("/remember use tabs"); !strings.Contains(next.(model).toastText(), "no local rules file") {
		t.Fatalf("toast = %q", next.(model).toastText())
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	project := t.TempDir()
	write := func(rel, text string) {
		t.Helper()
		path := filepath.Join(project, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".switch/rules.md", "project rule")
	write(".switch/rules.local.md", "- earlier rule") // no newline at the end
	base, err := rules.Load(config.RulesConfig{Global: filepath.Join(project, "none.md"),
		Project: filepath.Join(project, ".switch/rules.md"), Local: filepath.Join(project, ".switch/rules.local.md")})
	if err != nil {
		t.Fatal(err)
	}
	launch := func() model {
		m := New(Options{Context: context.Background(), Project: project, Store: state, Rules: base, Lead: config.Agent{Name: "lead"}})
		t.Cleanup(m.cancel)
		return m
	}
	m := launch()
	next, _ := m.Update(keyEnter()) // the user accepts the project's rules
	m = next.(model)

	next, _ = m.handleSlash("/remember use tabs, not spaces")
	m = next.(model)
	if data, _ := os.ReadFile(filepath.Join(project, ".switch/rules.local.md")); string(data) != "- earlier rule\n- use tabs, not spaces\n" {
		t.Fatalf("rules.local.md = %q", data)
	}
	if !strings.Contains(m.toastText(), "added to .switch/rules.local.md") || !strings.Contains(m.rules.Text(), "use tabs") {
		t.Fatalf("toast %q, rules %q", m.toastText(), m.rules.Text())
	}
	if launch().rulesReview != nil {
		t.Fatal("the next launch asks about a rule the user wrote")
	}

	write(".switch/rules/roles/builder.md", "a rule from a pull") // never shown to the user
	m.handleSlash("/remember one more")
	if data, _ := os.ReadFile(filepath.Join(project, ".switch/rules.local.md")); !strings.HasSuffix(string(data), "- one more\n") {
		t.Fatalf("rules.local.md = %q", data)
	}
	if launch().rulesReview == nil {
		t.Fatal("remembering trusted project rules the user has not seen")
	}
}
