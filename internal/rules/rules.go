package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urizennnn/switch/internal/config"
)

type Source struct {
	Path  string
	Scope string
	Text  string
}

type Set struct {
	Sources []Source
}

// Load reads global, project, and project-local rule files. Missing files are
// not an error; rules are optional.
func Load(cfg config.RulesConfig) (Set, error) {
	var set Set
	for _, item := range []struct {
		path  string
		scope string
	}{
		{cfg.Global, "global"},
		{cfg.Project, "project"},
		{cfg.Local, "local"},
	} {
		if err := set.add(expandHome(item.path), item.scope); err != nil {
			return Set{}, err
		}
	}
	return set, nil
}

// Scoped returns base plus role and task rules, in precedence order global →
// project → local → role → task. Role rules live in
// .switch/rules/roles/<role>.md and task rules in .switch/rules/tasks/<task>.md
// under project; both are optional. Lead-only guidance belongs in
// roles/lead.md, which workers never receive.
func Scoped(base Set, project, role, task string) (Set, error) {
	set := Set{Sources: append([]Source(nil), base.Sources...)}
	dir := filepath.Join(project, ".switch", "rules")
	if name, ok := fileName(role); ok {
		if err := set.add(filepath.Join(dir, "roles", name+".md"), "role:"+name); err != nil {
			return Set{}, err
		}
	}
	if name, ok := fileName(task); ok {
		if err := set.add(filepath.Join(dir, "tasks", name+".md"), "task:"+name); err != nil {
			return Set{}, err
		}
	}
	return set, nil
}

// fileName rejects names that would escape the rules directory; task IDs come
// from lead-written plans.
func fileName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	return name, true
}

func (s *Set) add(path, scope string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read rules %s: %w", path, err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	s.Sources = append(s.Sources, Source{Path: path, Scope: scope, Text: text})
	return nil
}

func (s Set) Empty() bool {
	return len(s.Sources) == 0
}

// Text returns the merged rules for display.
func (s Set) Text() string {
	if len(s.Sources) == 0 {
		return ""
	}
	var b strings.Builder
	for i, source := range s.Sources {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## %s rules (%s)\n\n%s", source.Scope, source.Path, source.Text)
	}
	return b.String()
}

// Prompt prepends the effective rules to a user message. ACP has no stable
// client-provided system prompt, so this is the portable injection point.
func (s Set) Prompt(user string) string {
	if len(s.Sources) == 0 {
		return user
	}
	var b strings.Builder
	b.WriteString("Follow these operating rules for this session:\n\n")
	b.WriteString(s.Text())
	b.WriteString("\n\nUser request:\n")
	b.WriteString(user)
	return b.String()
}

func expandHome(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}
