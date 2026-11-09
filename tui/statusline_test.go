package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/store"
)

func TestStatusLineFollowsState(t *testing.T) {
	m := New(Options{Context: context.Background(), Project: "/p", Lead: config.Agent{Name: "lead", Driver: "opencode", Model: "opencode/deepseek"}})
	m.connecting, m.state = false, "ready"
	m.ctxUsed, m.ctxSize = 30, 200
	m.branch, m.dirty = "main", 1

	cases := []struct {
		name  string
		setup func(m *model)
		want  []string
	}{
		{"ready", func(m *model) {}, []string{"READY", "lead · deepseek · 15% context", "/p  ⎇ main ✱1", "/ commands"}},
		{"working", func(m *model) {
			m.busy, m.state, m.step, m.turnStarted = true, "working", "Read view.go", time.Now().Add(-14*time.Second)
		}, []string{"WORKING 14s", "lead · Read view.go", "esc cancel"}},
		{"permission", func(m *model) {
			m.permission = &acpdriver.PermissionRequest{Title: "edit view.go", Options: make([]acpdriver.PermissionOption, 2)}
		}, []string{"PERMISSION", "lead asks: edit view.go", "1-2 choose · esc deny"}},
		{"run", func(m *model) {
			m.job, m.jobStarted = "run", time.Now().Add(-190*time.Second)
			m.run = &store.Run{ID: "r1"}
			m.tasks = []store.Task{{Status: execution.StatusMerged}, {Status: execution.StatusReview}, {Status: "running"}, {Status: execution.StatusQueued}}
		}, []string{"RUNNING 3:10", "1/4 merged · 1 working · 1 review", "███░░░░░░░░░", "/tasks"}},
		{"review", func(m *model) {
			m.tasks = []store.Task{{Status: execution.StatusReview}, {Status: execution.StatusReview}}
		}, []string{"REVIEW", "2 tasks ready for review", "/tasks"}},
		{"exited", func(m *model) { m.state = "exited" }, []string{"EXITED", "opencode stopped", "/lead · /doctor"}},
	}
	for _, c := range cases {
		mm := m
		c.setup(&mm)
		line := ansi.Strip(mm.statusView(140))
		for _, want := range c.want {
			if !strings.Contains(line, want) {
				t.Errorf("%s: %q missing from %q", c.name, want, line)
			}
		}
	}

	// The panes and the two bottom rows fill the terminal; there is no header.
	if next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40}); lipgloss.Height(next.(model).screen()) != 40 {
		t.Fatalf("screen is not the terminal's height")
	}

	// A narrow terminal keeps the state and the key, and drops detail first.
	m.busy, m.state, m.step, m.turnStarted = true, "working", "a very long tool title that cannot fit", time.Now()
	narrow := ansi.Strip(m.statusView(40))
	if !strings.Contains(narrow, "WORKING") || !strings.Contains(narrow, "esc cancel") || strings.Contains(narrow, "\n") {
		t.Fatalf("narrow status line: %q", narrow)
	}
}

func TestParseGitStatus(t *testing.T) {
	for _, c := range []struct {
		out    string
		branch string
		dirty  int
	}{
		{"## master...origin/master [ahead 2]\n M tui/view.go\n?? tui/toast.go\n", "master", 2},
		{"## feature/x\n", "feature/x", 0},
		{"## No commits yet on main\n?? a\n", "main", 1},
		{"## HEAD (no branch)\n", "detached", 0},
	} {
		got := parseGitStatus(c.out)
		if got.branch != c.branch || got.dirty != c.dirty {
			t.Errorf("%q: got %+v", c.out, got)
		}
	}
}
