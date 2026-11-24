package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/urizennnn/switch/internal/rules"
)

// remember is /remember: it adds "- rule" to the project's local rules file,
// which agents reread before their next message. The user wrote the rule, so
// project rules they had accepted stay accepted.
func (m *model) remember(rule string) {
	if rule == "" {
		m.notify("usage", "/remember <rule>")
		return
	}
	trusted := !m.rulesDeclined && m.rulesTrusted()
	path, err := m.rules.Target("local")
	if err == nil && path == "" {
		err = errors.New("this project has no local rules file")
	}
	if err == nil {
		err = appendRule(path, "- "+rule)
	}
	if err != nil {
		m.fail("could not remember", err.Error())
		return
	}
	if next, err := m.rules.Reload(); err == nil {
		m.rules = next
	}
	if project, err := rules.Project(m.rules); trusted && err == nil {
		m.trustRules(project.Hash())
	}
	where := shortenHome(path)
	if rel, err := filepath.Rel(m.project, path); err == nil && !strings.HasPrefix(rel, "..") {
		where = rel
	}
	note := "agents get it with their next message"
	if m.rulesDeclined {
		note = "project rules are off this session"
	}
	m.notify("remembered", "added to "+where+" · "+note)
}

// rulesTrusted reports whether the user accepted the project's rules as they
// are now; a project without rules has nothing to accept.
func (m *model) rulesTrusted() bool {
	project, err := rules.Project(m.rules)
	return err == nil && m.accepted(project.Hash())
}

// appendRule adds a line to a rule file, creating the file and its directory.
// It appends, so a failed write cannot lose the rules already there.
func appendRule(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		line = "\n" + line
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line + "\n")
	return errors.Join(err, f.Close())
}
