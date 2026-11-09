package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// Scrolling moves whatever text is in front: an open panel, the agent view
// being followed, or the conversation. ↑/↓ scroll a line (the terminal sends
// the mouse wheel as ↑/↓ in alternate scroll mode, which main turns on),
// pgup and pgdown half a page, and shift+↑/↓ a line even while a list has
// the arrows. Scrolling back to the end follows new output again.

// scrollKey reports how far a key scrolls, or 0 when it is not a scroll key
// or something else uses it.
func (m model) scrollKey(key string) int {
	if m.permission != nil {
		return 0
	}
	half := max(1, m.mainPaneHeight()/2)
	switch key {
	case "ctrl+home":
		return -1 << 30
	case "ctrl+end":
		return 1 << 30
	case "pgup":
		return -half
	case "pgdown":
		return half
	case "shift+up":
		return -1
	case "shift+down":
		return 1
	case "up", "down":
		if m.paletteOpen || m.taskPicker != nil || m.sessionPicker != nil || m.picker != nil ||
			m.adapterPicker != nil || m.rulesManager != nil {
			return 0 // these lists move their selection with the arrows
		}
		if key == "up" {
			return -1
		}
		return 1
	}
	return 0
}

func (m *model) scroll(delta int) {
	switch {
	case m.modal != "" || m.rulesReview != nil:
		total, room := m.modalLines()
		m.modalTop = max(0, min(total-room, m.modalTop+delta))
	case m.focus != "":
		total, room := m.agentLines()
		top := m.agentTop
		if top < 0 {
			top = max(0, total-room)
		}
		top = max(0, top+delta)
		if top >= total-room {
			top = -1 // back at the end: follow new output
		}
		m.agentTop = top
	case delta < 0:
		m.viewport.ScrollUp(-delta)
	default:
		m.viewport.ScrollDown(delta)
	}
}

// window is the part of lines a pane shows: from top, or the end when top
// is negative or past it.
func window(lines []string, top, room int) []string {
	if room <= 0 || len(lines) <= room {
		return lines
	}
	if top < 0 || top > len(lines)-room {
		top = len(lines) - room
	}
	return lines[top : top+room]
}

// below is the title note for a pane scrolled away from its end.
func below(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" · %d lines below", n)
}

// agentLines is the length of the followed agent's transcript and the rows
// it gets in the main pane.
func (m model) agentLines() (total, room int) {
	_, width := m.paneWidths(m.width)
	inner := max(10, width-4)
	head := strings.Join(m.agentHead(inner), "\n")
	return len(strings.Split(m.agentBody(inner), "\n")), m.agentRoom(lipgloss.Height(head))
}

func (m model) agentRoom(headLines int) int {
	return max(1, m.mainPaneHeight()-3-headLines-4)
}

// modalContent is the text of the open reading panel, wrapped to its width.
func (m model) modalContent() (title, content, footer string) {
	switch {
	case m.rulesReview != nil:
		title = "this project has rules"
		content = "Switch sends these to agents in this project. They come from the repository, so read them before they are used.\n\n" + m.rulesReview.text
		footer = "enter use them · esc not this session"
	default:
		title, content, footer = m.modalTitle, m.modal, "esc or enter to close"
	}
	width := min(82, max(44, m.width*2/3))
	return title, lipgloss.NewStyle().Width(max(20, width-8)).Render(content), footer
}

func (m model) modalRoom() int { return max(4, m.height-10) }

func (m model) modalLines() (total, room int) {
	_, content, _ := m.modalContent()
	return len(strings.Split(content, "\n")), m.modalRoom()
}

// scrolledModal draws the reading panel at its scroll position.
func (m model) scrolledModal(base string, width int) string {
	title, content, footer := m.modalContent()
	lines := strings.Split(content, "\n")
	room := m.modalRoom()
	if len(lines) > room {
		top := max(0, min(len(lines)-room, m.modalTop))
		content = strings.Join(lines[top:top+room], "\n")
		footer = fmt.Sprintf("lines %d-%d of %d · wheel or pgup/pgdn scroll · ", top+1, top+room, len(lines)) + footer
	}
	return m.withModal(base, width, title, content, footer)
}
