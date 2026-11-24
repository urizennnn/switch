package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

// The rules panel opens at launch: a message typed before the user sees it
// is kept, and its enter does not accept the rules unread.
func TestTypingDuringRulesReviewIsKept(t *testing.T) {
	m := New(Options{Context: context.Background()})
	m.rulesReview = &rulesReview{text: "# Project rules"}
	press := func(msg tea.KeyPressMsg) {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	for _, r := range "hi" {
		press(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.input.Value() != "hi" || m.rulesReview == nil {
		t.Fatalf("first enter: input %q, panel open %v", m.input.Value(), m.rulesReview != nil)
	}
	press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.input.Value() != "hi" || m.rulesReview != nil {
		t.Fatalf("second enter: input %q, panel open %v", m.input.Value(), m.rulesReview != nil)
	}
}

// Project rules that change mid-session are shown again before they are
// sent; enter accepts the rules that were shown, and esc stops asking.
func TestChangedRulesAreReviewedAgain(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	project := t.TempDir()
	path := filepath.Join(project, ".switch", "rules.md")
	writeTo := func(file, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write := func(text string) { t.Helper(); writeTo(path, text) }
	write("first")
	set, err := rules.Load(config.RulesConfig{Project: path})
	if err != nil {
		t.Fatal(err)
	}
	m := New(Options{Context: context.Background(), Project: project, Rules: set, Store: state})
	update := func(msg tea.Msg) {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	reviewing := func() string {
		if m.rulesReview == nil {
			return ""
		}
		return m.rulesReview.text
	}
	if !strings.Contains(reviewing(), "first") {
		t.Fatalf("launch: review = %q", reviewing())
	}

	write("second") // while the panel is open
	update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if update(dashboardTickMsg{}); !strings.Contains(reviewing(), "second") {
		t.Fatalf("change after the panel opened: review = %q", reviewing())
	}
	update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if update(dashboardTickMsg{}); reviewing() != "" {
		t.Fatalf("accepted rules shown again: %q", reviewing())
	}

	// /rules edits count as accepted only when the editor worked, the rules
	// were accepted before it, and no other rule file changed meanwhile.
	accepted := func() *rules.Set {
		t.Helper()
		set, err := rules.Project(m.rules)
		if err != nil {
			t.Fatal(err)
		}
		return &set
	}
	before := accepted()
	write("edited")
	update(editedMsg{path: path, before: before})
	if update(dashboardTickMsg{}); reviewing() != "" {
		t.Fatal("your own edit was shown for review")
	}
	for _, msg := range []editedMsg{{err: errors.New("no editor"), path: path, before: accepted()}, {path: path}} {
		write("edited again " + fmt.Sprint(msg.before != nil))
		update(msg)
		if update(dashboardTickMsg{}); !strings.Contains(reviewing(), "edited again") {
			t.Fatalf("%+v: review = %q", msg, reviewing())
		}
		update(tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	before = accepted()
	write("edited once more")
	writeTo(filepath.Join(project, ".switch", "rules", "roles", "builder.md"), "written by someone else meanwhile")
	update(editedMsg{path: path, before: before})
	if update(dashboardTickMsg{}); !strings.Contains(reviewing(), "someone else") {
		t.Fatalf("a change made during the edit was accepted unseen: review = %q", reviewing())
	}
	update(tea.KeyPressMsg{Code: tea.KeyEnter})

	// A permission prompt arriving during the review is drawn over it, so
	// its keys answer the prompt and do not decline the rules.
	write("asked during a prompt")
	update(dashboardTickMsg{})
	m.permission = &acpdriver.PermissionRequest{Options: []acpdriver.PermissionOption{{ID: "ok", Kind: "allow_once"}}}
	if update(tea.KeyPressMsg{Code: tea.KeyEscape}); m.permission != nil || m.rulesDeclined || reviewing() == "" {
		t.Fatalf("esc: prompt open %v, declined %v, review %q", m.permission != nil, m.rulesDeclined, reviewing())
	}
	update(tea.KeyPressMsg{Code: tea.KeyEnter})

	write("third")
	update(dashboardTickMsg{})
	update(tea.KeyPressMsg{Code: tea.KeyEscape})
	write("fourth")
	if update(dashboardTickMsg{}); reviewing() != "" || !m.rulesDeclined {
		t.Fatalf("declined: review = %q", reviewing())
	}
}
