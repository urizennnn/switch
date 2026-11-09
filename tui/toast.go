package tui

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// Toasts are short notices in the top-right corner that never take the
// input: errors stay longer than information, and both expire on their own.
// Modals are kept for content the user reads, such as /help or a task.

type toastLevel int

const (
	toastInfo toastLevel = iota
	toastError
)

type toast struct {
	level toastLevel
	title string
	body  string
	until time.Time
}

const (
	maxToasts  = 3
	toastWidth = 46
)

func (m *model) notify(title, body string) { m.addToast(toastInfo, title, body, 4*time.Second) }

func (m *model) fail(title, body string) { m.addToast(toastError, title, body, 9*time.Second) }

func (m *model) addToast(level toastLevel, title, body string, ttl time.Duration) {
	m.pruneToasts()
	m.toasts = append(m.toasts, toast{level: level, title: title, body: strings.TrimSpace(body), until: time.Now().Add(ttl)})
	if len(m.toasts) > maxToasts {
		m.toasts = m.toasts[len(m.toasts)-maxToasts:]
	}
}

// pruneToasts drops expired toasts; the dashboard tick redraws the screen.
func (m *model) pruneToasts() {
	now := time.Now()
	kept := m.toasts[:0]
	for _, t := range m.toasts {
		if t.until.After(now) {
			kept = append(kept, t)
		}
	}
	m.toasts = kept
}

func (t toast) view() string {
	color, mark := teal, "●"
	if t.level == toastError {
		color, mark = red, "✕"
	}
	inner := toastWidth - 4
	head := lipgloss.NewStyle().Foreground(color).Bold(true).Render(mark + " " + truncate(t.title, inner-2))
	body := ""
	if t.body != "" {
		wrapped := lipgloss.NewStyle().Width(inner).Render(t.body)
		body = "\n" + textStyle.Render(truncateLines(wrapped, 4))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(color).
		Background(surface).
		Padding(0, 1).
		Width(toastWidth).
		Render(head + body)
}

// withToasts stacks live toasts in the top-right corner over the screen.
func (m model) withToasts(screen string, width int) string {
	now := time.Now()
	layers := []*lipgloss.Layer{lipgloss.NewLayer(screen)}
	y := 1
	for i := len(m.toasts) - 1; i >= 0; i-- { // newest on top
		if !m.toasts[i].until.After(now) {
			continue
		}
		box := m.toasts[i].view()
		layers = append(layers, lipgloss.NewLayer(box).X(max(0, width-lipgloss.Width(box)-1)).Y(y).Z(2))
		y += lipgloss.Height(box)
	}
	if len(layers) == 1 {
		return screen
	}
	return lipgloss.NewCompositor(layers...).Render()
}
