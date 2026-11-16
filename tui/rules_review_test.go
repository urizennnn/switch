package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
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
