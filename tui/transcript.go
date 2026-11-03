package tui

import (
	"fmt"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
)

// The conversation is a list of entries rendered per turn: user messages in
// a marked block, lead replies as markdown, tool calls as one line each that
// updates in place, and the agent's plan as a checklist.

type entryKind int

const (
	entryUser entryKind = iota
	entryAssistant
	entryTool
	entryPlan
	entrySystem
	entryError
)

type entry struct {
	kind entryKind
	text string
	id   string // tool call ID, so status updates replace the line
	open bool   // assistant reply still streaming

	rendered string // cached render for width; closed entries only
	width    int
}

var (
	userBarStyle = lipgloss.NewStyle().
			Border(lipgloss.ThickBorder(), false, false, false, true).
			BorderForeground(accent).
			PaddingLeft(1)
	leadLabelStyle = lipgloss.NewStyle().Foreground(teal).Bold(true)
	toolStyle      = lipgloss.NewStyle().Foreground(dim)
)

func (m *model) appendUser(text string) {
	m.closeAssistant()
	m.entries = append(m.entries, entry{kind: entryUser, text: text})
}

func (m *model) appendAssistantChunk(text string) {
	if text == "" {
		return
	}
	if n := len(m.entries); n > 0 && m.entries[n-1].open {
		m.entries[n-1].text += text
		return
	}
	m.entries = append(m.entries, entry{kind: entryAssistant, text: text, open: true})
}

// closeAssistant ends the streaming reply and returns its text.
func (m *model) closeAssistant() string {
	if n := len(m.entries); n > 0 && m.entries[n-1].open {
		m.entries[n-1].open = false
		return m.entries[n-1].text
	}
	return ""
}

// appendTool shows a tool call, or updates it when the same call reports again.
func (m *model) appendTool(id, title, status string) {
	for i := len(m.entries) - 1; i >= 0 && id != ""; i-- {
		if m.entries[i].kind == entryTool && m.entries[i].id == id {
			if title == "" {
				title = strings.SplitN(m.entries[i].text, "\x00", 2)[0]
			}
			m.entries[i].text = title + "\x00" + status
			m.entries[i].rendered = ""
			return
		}
	}
	if title == "" {
		return
	}
	m.closeAssistant()
	m.entries = append(m.entries, entry{kind: entryTool, id: id, text: title + "\x00" + status})
}

// setPlan shows the agent's plan; each update replaces the previous one.
func (m *model) setPlan(items []string) {
	text := strings.Join(items, "\n")
	for i := len(m.entries) - 1; i >= 0 && m.entries[i].kind != entryUser; i-- {
		if m.entries[i].kind == entryPlan {
			m.entries[i].text, m.entries[i].rendered = text, ""
			return
		}
	}
	m.closeAssistant()
	m.entries = append(m.entries, entry{kind: entryPlan, text: text})
}

func (m *model) appendSystem(text string) {
	m.closeAssistant()
	m.entries = append(m.entries, entry{kind: entrySystem, text: text})
}

func (m *model) appendError(text string) {
	m.closeAssistant()
	m.entries = append(m.entries, entry{kind: entryError, text: text})
}

func (m *model) refreshViewport() {
	m.viewport.SetContent(m.renderTranscript(m.viewport.Width()))
	m.viewport.GotoBottom()
}

func (m *model) renderTranscript(width int) string {
	width = max(20, width)
	var b strings.Builder
	labelled := false // the lead label is shown once per turn
	for i := range m.entries {
		e := &m.entries[i]
		continued := false
		switch e.kind {
		case entryUser:
			labelled = false
		case entryAssistant:
			continued, labelled = labelled, true
		}
		if i > 0 {
			// Consecutive tool lines stay together.
			if !(e.kind == entryTool && m.entries[i-1].kind == entryTool) {
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
		if e.rendered == "" || e.width != width || e.open {
			e.rendered, e.width = renderEntry(*e, width, continued), width
		}
		b.WriteString(e.rendered)
	}
	if b.Len() == 0 {
		return systemStyle.Render("New session. Type a message below, or / for commands.")
	}
	return b.String()
}

func renderEntry(e entry, width int, continued bool) string {
	switch e.kind {
	case entryUser:
		return userBarStyle.Width(width).Render(userMarkerStyle.Render("you") + "\n" + textStyle.Render(e.text))
	case entryAssistant:
		if continued {
			return renderMarkdown(e.text, width)
		}
		return leadLabelStyle.Render("lead") + "\n" + renderMarkdown(e.text, width)
	case entryTool:
		title, status, _ := strings.Cut(e.text, "\x00")
		mark := "○"
		switch status {
		case "completed":
			mark = "●"
		case "failed":
			mark = "✕"
		}
		line := fmt.Sprintf("  %s %s", mark, title)
		if status != "" && status != "completed" {
			line += " · " + status
		}
		return toolStyle.Render(truncate(line, width))
	case entryPlan:
		var lines []string
		for _, item := range strings.Split(e.text, "\n") {
			lines = append(lines, "  "+item)
		}
		return dimStyle.Render("plan") + "\n" + mutedStyle.Render(strings.Join(lines, "\n"))
	case entryError:
		return errorStyle.Width(width).Render(e.text)
	default:
		return systemStyle.Width(width).Render(e.text)
	}
}

var (
	markdownMu        sync.Mutex
	markdownRenderers = map[int]*glamour.TermRenderer{}
)

// renderMarkdown renders agent output; it falls back to plain wrapped text if
// the markdown cannot be rendered.
func renderMarkdown(text string, width int) string {
	markdownMu.Lock()
	defer markdownMu.Unlock()
	renderer, ok := markdownRenderers[width]
	if !ok {
		style := styles.DarkStyleConfig
		zero := uint(0)
		style.Document.Margin = &zero // the transcript supplies its own margins
		style.Document.BlockPrefix, style.Document.BlockSuffix = "", ""
		var err error
		renderer, err = glamour.NewTermRenderer(glamour.WithStyles(style), glamour.WithWordWrap(width))
		if err != nil {
			return textStyle.Width(width).Render(text)
		}
		markdownRenderers[width] = renderer
	}
	out, err := renderer.Render(text)
	if err != nil {
		return textStyle.Width(width).Render(text)
	}
	return strings.Trim(out, "\n")
}
