package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/rules"
)

func shortenHome(path string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

// rulesManager is /rules: the rule files of each scope, opened in $EDITOR.
type rulesManager struct {
	tab    int // 0 global, 1 project
	index  int
	files  [2][]rules.File
	naming bool // typing the name of a new rule file
	name   string
	err    string
}

// rulesReview shows a project's rules the first time they would be sent, since
// they come from the repository rather than from the user.
type rulesReview struct {
	text   string
	warned bool // an enter came with a message typed, so it was not taken as yes
}

type editedMsg struct{ err error }

var ruleTabs = [2]string{"global", "project"}

func (m model) openRulesManager() (tea.Model, tea.Cmd) {
	r := &rulesManager{tab: 1}
	r.files[0], r.files[1] = m.rules.Files()
	m.rulesManager = r
	return m, nil
}

func (m model) handleRulesManagerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	r := m.rulesManager
	if r.naming {
		switch msg.String() {
		case "esc":
			r.naming, r.name, r.err = false, "", ""
		case "enter":
			target := r.name
			if r.tab == 0 {
				target = "global/" + target
			}
			path, err := m.rules.Target(target)
			if err != nil {
				r.err = err.Error()
				return m, nil
			}
			r.naming, r.name = false, ""
			return m, m.editRules(path)
		case "backspace":
			if r.name != "" {
				r.name = r.name[:len(r.name)-1]
			}
		default:
			r.name += msg.Text
		}
		return m, nil
	}
	files := r.files[r.tab]
	switch msg.String() {
	case "esc", "q":
		m.rulesManager = nil
	case "tab", "left", "right":
		r.tab, r.index, r.err = 1-r.tab, 0, ""
	case "up":
		r.index = max(0, r.index-1)
	case "down":
		r.index = max(0, min(len(files)-1, r.index+1))
	case "n":
		r.naming, r.err = true, ""
	case "e", "enter":
		if len(files) == 0 {
			r.err = "global rules are turned off for this project; n creates a project rule"
			return m, nil
		}
		return m, m.editRules(files[r.index].Path)
	}
	return m, nil
}

func (m model) editRules(path string) tea.Cmd {
	editor, err := rules.Editor(path)
	if err != nil {
		return func() tea.Msg { return editedMsg{err: err} }
	}
	return tea.ExecProcess(editor, func(err error) tea.Msg { return editedMsg{err: err} })
}

// handleEdited reloads the rules after the editor closes. The chat rereads
// them on its next message and sends them again only if they changed.
func (m model) handleEdited(msg editedMsg) (tea.Model, tea.Cmd) {
	if next, err := m.rules.Reload(); err == nil {
		m.rules = next
	} else if msg.err == nil {
		msg.err = err
	}
	if !m.rulesDeclined {
		m.trustProjectRules() // the user just wrote them
	}
	if r := m.rulesManager; r != nil {
		r.files[0], r.files[1] = m.rules.Files()
		r.index = min(r.index, max(0, len(r.files[r.tab])-1))
		r.err = ""
		if msg.err != nil {
			r.err = "editor: " + msg.err.Error()
		}
	} else if msg.err != nil {
		m.fail("editor failed", msg.err.Error())
	}
	return m, nil
}

func (m model) rulesManagerContent(width int) string {
	r := m.rulesManager
	var b strings.Builder
	for i, name := range ruleTabs {
		style := mutedStyle
		if i == r.tab {
			style = activeStyle
		}
		b.WriteString(style.Render(" " + name + " "))
		b.WriteString("  ")
	}
	b.WriteString("\n" + dimStyle.Render(truncate(ruleTabNote[r.tab], max(10, width-6))) + "\n\n")
	files := r.files[r.tab]
	if len(files) == 0 {
		b.WriteString(mutedStyle.Render("  global rules are turned off for this project (ignore_global)") + "\n")
	}
	for i, file := range files {
		marker := "  "
		if i == r.index {
			marker = "› "
		}
		path := shortenHome(file.Path)
		if rel, err := filepath.Rel(m.project, file.Path); err == nil && !strings.HasPrefix(rel, "..") {
			path = rel
		}
		if !file.Exists {
			path += " (not created)"
		}
		scope := fmt.Sprintf("%-18s", truncate(file.Scope, 18))
		style := mutedStyle
		if i == r.index {
			style = activeStyle
		}
		b.WriteString(style.Render(truncate(marker+scope+" "+path, max(10, width-6))) + "\n")
	}
	if r.naming {
		prefix := ""
		if r.tab == 0 {
			prefix = "global/"
		}
		b.WriteString("\n" + textStyle.Render("new rule file: "+prefix+r.name+"▌") + "\n")
		b.WriteString(dimStyle.Render(newRuleHint[r.tab]) + "\n")
	}
	if r.err != "" {
		b.WriteString("\n" + errorStyle.Render(truncate(r.err, max(10, width-6))) + "\n")
	}
	return b.String()
}

var ruleTabNote = [2]string{
	"yours, in every project · project rules win on conflict",
	"committed with the repository · local is yours alone",
}

var newRuleHint = [2]string{
	"roles/<role> or agents/<agent> · enter to open in your editor",
	"roles/<role>, agents/<agent> or tasks/<task> · enter to open in your editor",
}

func rulesManagerFooter(r *rulesManager) string {
	if r.naming {
		return "enter create · esc cancel"
	}
	return "tab scope · ↑/↓ select · e edit · n new · esc close"
}

// previewRules is /rules <agent>: exactly what that agent is given, by scope.
func (m *model) previewRules(name string) {
	agent, ok := m.fleetAgent(name)
	if !ok {
		m.fail("unknown agent "+name, "/lead lists configured agents")
		return
	}
	role := agent.Role
	if agent.Name == m.lead.Name {
		role = "lead"
	}
	set, err := rules.Scoped(m.rules, role, agent.Name, "")
	switch {
	case err != nil:
		m.fail("could not read rules", err.Error())
	case set.Empty():
		m.setModal("rules for "+name, name+" gets no rules.\n\n/rules opens the rule files.")
	default:
		m.setModal("rules for "+name, set.Text())
	}
}

// reviewRules asks once about project rules the user has not seen, before
// they are sent to any agent.
func (m *model) reviewRules() {
	if m.store == nil || m.project == "" {
		return
	}
	project, err := rules.Project(m.rules)
	if err != nil || project.Empty() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	trusted, err := m.store.TrustedRules(ctx, m.project)
	if err != nil || trusted == project.Hash() {
		return
	}
	m.rulesReview = &rulesReview{text: project.Text()}
}

func (m *model) trustProjectRules() {
	project, err := rules.Project(m.rules)
	if err != nil || m.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = m.store.SetTrustedRules(ctx, m.project, project.Hash())
}

func (m model) handleRulesReviewKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// The panel opens at launch, so keys typed before the user sees it go to
	// the input, and the enter ending such a message does not accept rules
	// the user has not read.
	switch msg.String() {
	case "enter":
		if m.input.Value() != "" && !m.rulesReview.warned {
			m.rulesReview.warned = true
			m.notify("your message is kept", "read the project rules first: enter uses them, esc skips them")
			return m, nil
		}
		m.trustProjectRules()
		m.rulesReview = nil
	case "esc":
		m.rulesDeclined = true
		m.rules = m.rules.WithoutProject()
		if m.chat != nil {
			m.chat.Rules = m.rules
		}
		m.rulesReview = nil
		m.notify("project rules skipped", "they will not be sent this session; you will be asked again next time")
	default:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}
