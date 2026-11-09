package tui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	terminalansi "github.com/charmbracelet/x/ansi"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
)

// A transcript is a list of entries rendered per turn: user messages (or a
// teammate's question) in a marked block, thinking as a dim block that
// collapses once the agent moves on, replies as markdown, tool calls as one
// line each that updates in place, and the agent's plan as a checklist. The
// lead's transcript is the conversation; every other agent has one for its
// live view.

type entryKind int

const (
	entryUser entryKind = iota
	entryAssistant
	entryTool
	entryPlan
	entrySystem
	entryError
	entryThought
)

type entry struct {
	kind entryKind
	text string
	who  string // the asker of a user entry; "" is you
	open bool   // reply or thought still streaming

	// tool calls
	id             string // tool call ID, so status updates replace the line
	name           string // its first title, such as "shell" or "Read"
	detail         string // what it acts on, such as the command
	status         string
	toolKind       string
	output         string
	outputRendered string
	outputWidth    int

	start, end time.Time // thinking time

	rendered string // cached render for width and text length
	width    int
	length   int
}

var (
	userBarStyle = lipgloss.NewStyle().
			Border(lipgloss.ThickBorder(), false, false, false, true).
			BorderForeground(accent).
			PaddingLeft(1)
	askBarStyle    = userBarStyle.BorderForeground(violet)
	leadLabelStyle = lipgloss.NewStyle().Foreground(teal).Bold(true)
	toolStyle      = lipgloss.NewStyle().Foreground(muted)
	thoughtStyle   = lipgloss.NewStyle().Foreground(dim).Italic(true)
)

// Spinner frames for running tools and the activity line.
var (
	toolFrames     = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	activityFrames = []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}
)

// Transcript edits shared by the lead and every other agent.

func addUser(list *[]entry, who, text string) {
	closeOpen(list)
	*list = append(*list, entry{kind: entryUser, who: who, text: text})
}

// addChunk streams reply or thought text into the open entry of that kind.
func addChunk(list *[]entry, kind entryKind, text string) {
	if text == "" {
		return
	}
	if n := len(*list); n > 0 && (*list)[n-1].open && (*list)[n-1].kind == kind {
		(*list)[n-1].text += text
		return
	}
	closeOpen(list)
	*list = append(*list, entry{kind: kind, text: text, open: true, start: time.Now()})
}

// closeOpen ends a streaming reply or thought and returns the reply's text.
func closeOpen(list *[]entry) string {
	n := len(*list)
	if n == 0 || !(*list)[n-1].open {
		return ""
	}
	e := &(*list)[n-1]
	e.open, e.end, e.rendered = false, time.Now(), ""
	if e.kind == entryAssistant {
		return e.text
	}
	return ""
}

// addTool shows a tool call, or updates it when the same call reports again.
// A later title (opencode sends the command as one) becomes the detail.
func addTool(list *[]entry, tool acpdriver.ToolInfo) {
	for i := len(*list) - 1; i >= 0 && tool.ID != ""; i-- {
		e := &(*list)[i]
		if e.kind != entryTool || e.id != tool.ID {
			continue
		}
		if tool.Status != "" {
			e.status = tool.Status
		}
		if tool.Kind != "" {
			e.toolKind = tool.Kind
		}
		if tool.Output != "" && tool.Output != e.output {
			e.output = tool.Output
			e.outputRendered = ""
		}
		if tool.Detail != "" {
			e.detail = tool.Detail
		} else if tool.Title != "" && tool.Title != e.name && e.detail == "" {
			e.detail = tool.Title
		}
		e.rendered = ""
		return
	}
	name := tool.Title
	if name == "" {
		name = tool.Kind
	}
	if name == "" {
		return
	}
	closeOpen(list)
	*list = append(*list, entry{kind: entryTool, id: tool.ID, name: name, detail: tool.Detail, status: tool.Status, output: tool.Output, toolKind: tool.Kind})
}

func setPlanIn(list *[]entry, items []string) {
	text := strings.Join(items, "\n")
	for i := len(*list) - 1; i >= 0 && (*list)[i].kind != entryUser; i-- {
		if (*list)[i].kind == entryPlan {
			(*list)[i].text, (*list)[i].rendered = text, ""
			return
		}
	}
	closeOpen(list)
	*list = append(*list, entry{kind: entryPlan, text: text})
}

func addNote(list *[]entry, kind entryKind, text string) {
	closeOpen(list)
	*list = append(*list, entry{kind: kind, text: text})
}

func planItems(plan []acpdriver.PlanEntry) []string {
	items := make([]string, 0, len(plan))
	for _, entry := range plan {
		mark := "·"
		switch entry.Status {
		case "completed":
			mark = "✓"
		case "in_progress":
			mark = "›"
		}
		items = append(items, mark+" "+entry.Content)
	}
	return items
}

// applyEvent adds one session event to an agent's transcript.
func applyEvent(list *[]entry, event acpdriver.Event) {
	switch event.Type {
	case acpdriver.EventStart:
		if event.Text != "" {
			addUser(list, "task", event.Text)
		}
	case acpdriver.EventMessage:
		addChunk(list, entryAssistant, event.Text)
	case acpdriver.EventThought:
		addChunk(list, entryThought, event.Text)
	case acpdriver.EventToolCall, acpdriver.EventToolEdit:
		if event.Tool != nil {
			addTool(list, *event.Tool)
		}
	case acpdriver.EventPlan:
		setPlanIn(list, planItems(event.Plan))
	case acpdriver.EventTurn:
		closeOpen(list)
	case acpdriver.EventError:
		if event.Err != nil {
			addNote(list, entryError, event.Err.Error())
		}
	case acpdriver.EventExit:
		addNote(list, entrySystem, "agent exited")
	}
}

// activity is what an agent is doing now, from the end of its transcript.
func activity(entries []entry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		switch {
		case e.kind == entryThought && e.open:
			return "Thinking"
		case e.kind == entryAssistant && e.open:
			return "Writing"
		case e.kind == entryTool && (e.status == "pending" || e.status == "in_progress" || e.status == ""):
			return "Running " + strings.ToLower(e.name)
		case e.kind == entryUser:
			return "Thinking"
		}
	}
	return "Working"
}

// The lead's conversation.

func (m *model) appendUser(text string) { addUser(&m.entries, "", text) }

func (m *model) appendAssistantChunk(text string) { addChunk(&m.entries, entryAssistant, text) }

func (m *model) closeAssistant() string { return closeOpen(&m.entries) }

func (m *model) appendTool(id, title, status string) {
	addTool(&m.entries, acpdriver.ToolInfo{ID: id, Title: title, Status: status})
}

func (m *model) setPlan(items []string) { setPlanIn(&m.entries, items) }

func (m *model) appendSystem(text string) { addNote(&m.entries, entrySystem, text) }

func (m *model) appendError(text string) { addNote(&m.entries, entryError, text) }

// refreshViewport redraws the conversation, following new output unless the
// user has scrolled up.
func (m *model) refreshViewport() {
	follow := m.viewport.AtBottom() || m.viewport.TotalLineCount() == 0
	m.viewport.SetContent(terminalansi.Hardwrap(m.renderTranscript(m.viewport.Width()), m.viewport.Width(), true))
	if follow {
		m.viewport.GotoBottom()
	}
}

func (m *model) renderTranscript(width int) string {
	label := m.lead.Name
	if label == "" {
		label = "lead"
	}
	out := renderEntries(m.entries, max(20, width), label, m.frame, m.showThoughts, m.showCommands)
	if m.busy && m.state == "working" {
		out += "\n\n" + activityLine(activity(m.entries), m.turnStarted, m.frame, "esc to stop")
	}
	if out == "" {
		return systemStyle.Render("New session. Type a message below, or / for commands.")
	}
	return out
}

// activityLine is the animated line under a working agent.
func activityLine(verb string, since time.Time, frame int, hint string) string {
	glyph := brandStyle.Render(activityFrames[frame%len(activityFrames)])
	detail := []string{}
	if !since.IsZero() {
		detail = append(detail, elapsed(since))
	}
	if hint != "" {
		detail = append(detail, hint)
	}
	line := glyph + " " + brandStyle.Render(verb+"…")
	if len(detail) > 0 {
		line += dimStyle.Render(" (" + strings.Join(detail, " · ") + ")")
	}
	return line
}

func elapsed(start time.Time) string {
	if s := since(start); s != "" {
		return s
	}
	return "0s"
}

// renderEntries renders a transcript. Closed entries are cached; open ones
// re-render only when their text grew.
func renderEntries(entries []entry, width int, label string, frame int, showThoughts bool, showCommands ...bool) string {
	var b strings.Builder
	commandsOpen := len(showCommands) > 0 && showCommands[0]
	labelled := false // the agent label is shown once per turn
	for i := range entries {
		e := &entries[i]
		if e.kind == entryTool && e.output != "" && (!isCommand(*e) || commandsOpen) && (e.outputRendered == "" || e.outputWidth != width) {
			language := "text"
			if strings.HasPrefix(e.output, "File: ") {
				language = "diff"
			}
			e.outputRendered = renderCodeBlock(language, e.output, width, true)
			e.outputWidth = width
		}
		continued := false
		switch e.kind {
		case entryUser:
			labelled = false
		case entryAssistant:
			continued, labelled = labelled, true
		}
		if i > 0 {
			// Consecutive tool lines stay together.
			if !(e.kind == entryTool && entries[i-1].kind == entryTool) {
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
		switch {
		case e.kind == entryTool:
			b.WriteString(renderTool(*e, width, frame, commandsOpen))
		case e.kind == entryThought:
			b.WriteString(renderThought(*e, width, showThoughts))
		default:
			if e.rendered == "" || e.width != width || e.length != len(e.text) {
				e.rendered, e.width, e.length = renderEntry(*e, width, label, continued), width, len(e.text)
			}
			b.WriteString(e.rendered)
			if e.kind == entryAssistant && e.open && frame%8 < 5 {
				b.WriteString(brandStyle.Render(" ▍"))
			}
		}
	}
	return b.String()
}

func running(status string) bool {
	return status == "" || status == "pending" || status == "in_progress"
}

func renderEntry(e entry, width int, label string, continued bool) string {
	switch e.kind {
	case entryUser:
		switch e.who {
		case "":
			return userBarStyle.Width(width).Render(userMarkerStyle.Render("you") + "\n" + textStyle.Render(e.text))
		case "task":
			return askBarStyle.Width(width).Render(textStyle.Render(e.text))
		default:
			return askBarStyle.Width(width).Render(userMarkerStyle.Render(e.who) + "\n" + textStyle.Render(e.text))
		}
	case entryAssistant:
		if continued {
			return renderMarkdown(e.text, width)
		}
		return leadLabelStyle.Render(label) + "\n" + renderMarkdown(e.text, width)
	case entryTool:
		return renderTool(e, width, 0)
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

// renderTool is one tool call: a spinner while it runs, then ● or ✕, and its
// name with what it acts on, as in Read(main.go) or Shell(ls internal).
func isCommand(e entry) bool {
	name := strings.ToLower(strings.TrimSpace(e.name))
	return strings.EqualFold(e.toolKind, "execute") || name == "shell" || name == "bash" || name == "terminal" || name == "execute" || strings.HasPrefix(name, "shell ") || strings.HasPrefix(name, "shell(") || strings.HasPrefix(name, "bash ") || strings.HasPrefix(name, "bash(")
}

func renderTool(e entry, width, frame int, showCommands ...bool) string {
	mark := warningStyle.Render(toolFrames[frame%len(toolFrames)])
	switch e.status {
	case "completed":
		mark = successStyle.Render("●")
	case "failed":
		mark = errorStyle.Render("✕")
	}
	name := e.name
	if !strings.ContainsAny(name, " /") {
		name = strings.ToUpper(name[:1]) + name[1:] // a bare tool name, such as shell
	}
	head := textStyle.Render(name)
	room := max(8, width-4-lipgloss.Width(name))
	if e.detail != "" && !strings.Contains(e.name, e.detail) {
		head += dimStyle.Render("(") + toolStyle.Render(truncate(e.detail, room-2)) + dimStyle.Render(")")
	}
	if e.status == "failed" {
		head += errorStyle.Render(" failed")
	}
	result := "  " + mark + " " + head
	if e.output != "" {
		if isCommand(e) && (len(showCommands) == 0 || !showCommands[0]) {
			return result + dimStyle.Render(fmt.Sprintf(" · %d lines · ctrl+e to show", len(strings.Split(strings.TrimSuffix(e.output, "\n"), "\n"))))
		}
		output := e.outputRendered
		if output == "" || e.outputWidth != width {
			language := "text"
			if strings.HasPrefix(e.output, "File: ") {
				language = "diff"
			}
			output = renderCodeBlock(language, e.output, width, true)
		}
		result += "\n" + output
	}
	return result
}

// renderThought shows thinking as it streams, then one collapsed line
// (ctrl+t shows them all).
func renderThought(e entry, width int, show bool) string {
	text := strings.TrimSpace(e.text)
	if e.open {
		lines := strings.Split(thoughtStyle.Width(max(10, width-4)).Render(text), "\n")
		lines = lines[max(0, len(lines)-4):]
		return dimStyle.Render("  ∴ thinking") + "\n" + indent(strings.Join(lines, "\n"), "    ")
	}
	took := e.end.Sub(e.start).Round(time.Second)
	head := dimStyle.Render(fmt.Sprintf("  ∴ thought for %s", max(time.Second, took)))
	if !show {
		return head + dimStyle.Render(" · ctrl+t to show")
	}
	return head + "\n" + indent(thoughtStyle.Width(max(10, width-4)).Render(text), "    ")
}

func indent(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

var (
	markdownMu        sync.Mutex
	markdownRenderers = map[int]*glamour.TermRenderer{}
)

// transcriptStyle is glamour's dark style made quieter to match Switch:
// headings without "#" marks or backgrounds, inline code as colored text,
// and no red error boxes in code blocks.
func transcriptStyle() ansi.StyleConfig {
	style := styles.DarkStyleConfig
	zero := uint(0)
	style.Document.Margin = &zero // the transcript supplies its own margins
	style.Document.BlockPrefix, style.Document.BlockSuffix = "", ""

	str := func(s string) *string { return &s }
	yes := func() *bool { b := true; return &b }()
	style.Heading.Color = str("#3ee0c0")
	style.H1.StylePrimitive = ansi.StylePrimitive{Color: str("#3ee0c0"), Bold: yes}
	style.H2.StylePrimitive = ansi.StylePrimitive{Color: str("#3ee0c0"), Bold: yes}
	for _, h := range []*ansi.StyleBlock{&style.H3, &style.H4, &style.H5, &style.H6} {
		h.StylePrimitive = ansi.StylePrimitive{Color: str("#f4f2ec"), Bold: yes}
	}
	style.Code.StylePrimitive = ansi.StylePrimitive{Color: str("#e8b98a")}
	style.LinkText.Color = str("#3ee0c0")
	style.Link.Color = str("#7d8797")
	if style.CodeBlock.Chroma != nil {
		chroma := *style.CodeBlock.Chroma
		chroma.Error = chroma.Text // stray characters like → are not errors
		style.CodeBlock.Chroma = &chroma
	}
	return style
}

// renderMarkdown renders agent output; it falls back to plain wrapped text if
// the markdown cannot be rendered.
func renderMarkdown(text string, width int) string {
	lines := strings.Split(text, "\n")
	var parts, prose []string
	flush := func() {
		if len(prose) > 0 {
			if s := renderMarkdownPlain(strings.Join(prose, "\n"), width); s != "" {
				parts = append(parts, s)
			}
			prose = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimLeft(lines[i], " ")
		indent := len(lines[i]) - len(line)
		if indent > 3 || !(strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")) {
			prose = append(prose, lines[i])
			continue
		}
		marker := line[0]
		n := 0
		for n < len(line) && line[n] == marker {
			n++
		}
		if marker == '`' && strings.Contains(line[n:], "`") {
			prose = append(prose, lines[i])
			continue
		}
		flush()
		language := strings.TrimSpace(line[n:])
		var code []string
		closed := false
		for i++; i < len(lines); i++ {
			candidate := strings.TrimLeft(lines[i], " ")
			if len(lines[i])-len(candidate) <= 3 && len(candidate) >= n && strings.Trim(candidate, string(marker)+" \t") == "" && strings.HasPrefix(candidate, strings.Repeat(string(marker), n)) {
				closed = true
				break
			}
			code = append(code, lines[i])
		}
		parts = append(parts, renderCodeBlock(language, strings.Join(code, "\n"), width, closed))
	}
	flush()
	return strings.Join(parts, "\n\n")
}

func renderCodeBlock(language, code string, width int, closed bool) string {
	label := "code"
	if language != "" {
		label = strings.Fields(language)[0]
	}
	if !closed {
		label += " · streaming"
	}
	// Glamour handles syntax highlighting; Switch's shared border and text
	// colors keep the frame consistent with the rest of the interface.
	inner := max(8, width-8)
	code = strings.ReplaceAll(code, "\t", "    ")
	fence := "```"
	for strings.Contains(code, fence) {
		fence += "`"
	}
	language = strings.ReplaceAll(language, "`", "")
	body := renderMarkdownPlain(fence+language+"\n"+code+"\n"+fence, inner)
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(max(8, width-2)).Render(dimStyle.Render(label) + "\n" + body)
}

func renderMarkdownPlain(text string, width int) string {
	markdownMu.Lock()
	defer markdownMu.Unlock()
	renderer, ok := markdownRenderers[width]
	if !ok {
		var err error
		renderer, err = glamour.NewTermRenderer(glamour.WithStyles(transcriptStyle()), glamour.WithWordWrap(width))
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
