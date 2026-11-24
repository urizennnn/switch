package tui

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/store"
)

func TestCodeBlocksAreBoundedAndStreamingIsLabeled(t *testing.T) {
	if got := lipgloss.Width(renderCodeBlock("go", "func main() {\n\tfmt.Println(\"hello\")\n}", 62, true)); got > 62 {
		t.Errorf("code block width = %d, want <= 62", got)
	}
	got := ansi.Strip(renderMarkdown("Before\n\n```go\nfunc main() {}\n```\n\nAfter", 60))
	for _, want := range []string{"Before", "go", "func main()", "After", "╭", "╰"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "streaming") {
		t.Fatalf("closed block marked streaming: %s", got)
	}
	partial := ansi.Strip(renderMarkdown("```go\nfunc main()", 60))
	if !strings.Contains(partial, "streaming") || !strings.Contains(partial, "func main()") {
		t.Fatalf("partial block: %s", partial)
	}
}

func TestToolOutputAppearsInTranscript(t *testing.T) {
	var entries []entry
	addTool(&entries, acpdriver.ToolInfo{ID: "t1", Title: "Read", Status: "in_progress"})
	addTool(&entries, acpdriver.ToolInfo{ID: "t1", Status: "completed", Output: "line one\nline two"})
	got := ansi.Strip(renderTool(entries[0], 60, 0))
	for _, want := range []string{"Read", "line one", "line two", "╭", "╰"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestCommandOutputCollapsedAndExpandable(t *testing.T) {
	var entries []entry
	addTool(&entries, acpdriver.ToolInfo{ID: "cmd", Title: "Shell", Kind: "execute", Status: "completed", Output: "first line\nsecond line"})
	got := ansi.Strip(renderEntries(entries, 60, "lead", 0, false, false))
	if strings.Contains(got, "first line") || !strings.Contains(got, "2 lines") || !strings.Contains(got, "ctrl+e") {
		t.Fatalf("command not collapsed: %s", got)
	}
	got = ansi.Strip(renderEntries(entries, 60, "lead", 0, false, true))
	if !strings.Contains(got, "first line") || !strings.Contains(got, "second line") {
		t.Fatalf("command not expanded: %s", got)
	}
	got = ansi.Strip(renderEntries(entries, 60, "lead", 0, false, false))
	if strings.Contains(got, "first line") {
		t.Fatalf("command did not collapse again: %s", got)
	}
}

func TestRunningToolCachesOutputAndRefreshesOnChange(t *testing.T) {
	var entries []entry
	addTool(&entries, acpdriver.ToolInfo{ID: "t", Title: "Read", Status: "in_progress", Output: "first"})
	renderEntries(entries, 60, "lead", 0, false)
	cached := entries[0].outputRendered
	if cached == "" {
		t.Fatal("tool output was not cached")
	}
	renderEntries(entries, 60, "lead", 1, false)
	if entries[0].outputRendered != cached {
		t.Fatal("spinner invalidated unchanged output")
	}
	addTool(&entries, acpdriver.ToolInfo{ID: "t", Output: "second"})
	if entries[0].outputRendered != "" {
		t.Fatal("updated output left stale cache")
	}
	if got := ansi.Strip(renderEntries(entries, 60, "lead", 2, false)); !strings.Contains(got, "second") || strings.Contains(got, "first") {
		t.Fatalf("unexpected updated output: %s", got)
	}
}

func TestTranscriptToolsPlanAndMarkdown(t *testing.T) {
	m := model{}
	m.appendUser("make it so")
	m.appendTool("t1", "Read main.go", "pending")
	m.appendTool("t1", "", "completed") // a status update keeps the title
	m.setPlan([]string{"· read", "· edit"})
	m.setPlan([]string{"✓ read", "› edit"}) // replaces, not appends
	m.appendAssistantChunk("Done with **bold** and `code`.\n\n- one\n")
	m.appendAssistantChunk("- two")
	if got := m.closeAssistant(); !strings.Contains(got, "- two") {
		t.Fatalf("closeAssistant = %q", got)
	}

	kinds := map[entryKind]int{}
	for _, e := range m.entries {
		kinds[e.kind]++
	}
	if kinds[entryTool] != 1 || kinds[entryPlan] != 1 || kinds[entryAssistant] != 1 {
		t.Fatalf("entries = %+v", kinds)
	}
	out := m.renderTranscript(60)
	for _, want := range []string{"you", "make it so", "Read main.go", "✓ read", "lead", "bold", "one", "two"} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "**bold**") || strings.Contains(out, "pending") {
		t.Errorf("markdown or stale tool status left raw:\n%s", out)
	}
}

func TestMessageWhileConnectingIsSentOnConnect(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	m := New(Options{Context: context.Background(), Project: t.TempDir(), Store: state, Lead: config.Agent{Name: "lead", Driver: "opencode"}})
	m.input.SetValue("hello early")
	next, cmd := m.Update(keyEnter())
	m = next.(model)
	if cmd != nil || m.queued != "hello early" || m.busy {
		t.Fatalf("queued = %q busy = %v cmd = %v", m.queued, m.busy, cmd != nil)
	}
	if !strings.Contains(m.renderTranscript(60), "hello early") {
		t.Fatal("queued message not shown")
	}
	if m.home {
		t.Fatal("still on the overview after sending")
	}
	// A connection error keeps the message and asks for a retry.
	next, _ = m.Update(connectedMsg{err: errTest("adapter missing")})
	m = next.(model)
	if m.queued != "hello early" || !strings.Contains(m.renderTranscript(60), "press enter to retry") {
		t.Fatalf("after failed connect: queued=%q", m.queued)
	}
	// Once the lead is up, the message goes out.
	next, cmd = m.Update(connectedMsg{chat: &chat.Chat{Project: m.project, Store: state, Session: &acpdriver.Session{ID: "s1"}}})
	m = next.(model)
	if m.queued != "" || !m.busy || cmd == nil || m.entries[len(m.entries)-1].text != "hello early" {
		t.Fatalf("not sent on connect: queued=%q busy=%v", m.queued, m.busy)
	}
}

func TestMessagesTypedWhileTheLeadWorksWaitForItsTurnToEnd(t *testing.T) {
	m := New(Options{Context: context.Background(), Lead: config.Agent{Name: "lead", Driver: "opencode"}})
	defer m.cancel()
	m.chat = &chat.Chat{Session: &acpdriver.Session{ID: "s1"}}
	m.connecting, m.state = false, "ready"
	var cmd tea.Cmd
	update := func(msg tea.Msg) {
		var next tea.Model
		next, cmd = m.Update(msg)
		m = next.(model)
	}
	enter := func(text string) {
		m.input.SetValue(text)
		update(keyEnter())
	}
	turnEnded := agentEventMsg{gen: m.gen, event: acpdriver.Event{Type: acpdriver.EventTurn}}
	sent := func(text string) bool {
		last := m.entries[len(m.entries)-1]
		return m.busy && m.queued == "" && cmd != nil && last.kind == entryUser && last.text == text
	}

	enter("first")
	if !sent("first") {
		t.Fatal("a message to an idle lead did not go out")
	}

	// Commands still run while the lead works, or say why they cannot; one
	// that has to wait stays in the input.
	enter("/mode full")
	if !strings.Contains(m.toastText(), "work in progress") {
		t.Fatalf("/mode while busy: toast %q", m.toastText())
	}
	enter("/plan add retry logic")
	if !strings.Contains(m.toastText(), "busy") || m.input.Value() != "/plan add retry logic" {
		t.Fatalf("/plan while busy: input %q", m.input.Value())
	}

	enter("also add tests")
	if m.queued != "also add tests" || m.input.Value() != "" || !strings.Contains(ansi.Strip(m.renderTranscript(60)), "you · queued") {
		t.Fatalf("queued = %q, input = %q", m.queued, m.input.Value())
	}
	// It goes out once the turn's last event is in and its Send has
	// returned, in either order: Send writes down the rules it gave as it
	// returns, and the next Send reads them.
	update(turnEnded)
	if m.busy || m.queued == "" {
		t.Fatalf("sent before the last Send returned: busy %v, queued %q", m.busy, m.queued)
	}
	update(turnResultMsg{})
	if !sent("also add tests") {
		t.Fatalf("not sent once Send returned: queued %q busy %v", m.queued, m.busy)
	}
	enter("and docs")
	update(turnResultMsg{})
	if !m.busy || m.queued == "" {
		t.Fatal("Send returning ended the turn before its events")
	}
	update(turnEnded)
	if !sent("and docs") {
		t.Fatalf("not sent when the turn ended: queued %q busy %v", m.queued, m.busy)
	}

	// A failed turn hands queued text back to edit, and so does esc.
	enter("one more")
	next, _ := m.Update(agentEventMsg{gen: m.gen, event: acpdriver.Event{Type: acpdriver.EventError, Err: errTest("boom")}})
	if m = next.(model); m.queued != "" || m.input.Value() != "one more" || m.busy {
		t.Fatalf("after a failed turn: queued %q, input %q", m.queued, m.input.Value())
	}
	m.busy, m.state = true, "starting lead"
	enter("draft")
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m = next.(model); m.queued != "" || m.input.Value() != "draft" {
		t.Fatalf("after esc: queued %q, input %q", m.queued, m.input.Value())
	}
}

func TestPasteReachesTheInput(t *testing.T) {
	m := New(Options{Context: context.Background()})
	defer m.cancel()
	next, _ := m.Update(tea.PasteMsg{Content: "first line\nsecond line"})
	if m = next.(model); m.input.Value() != "first line second line" {
		t.Fatalf("input = %q after a multi-line paste", m.input.Value())
	}
	m.taskPicker = &taskPicker{}
	next, _ = m.Update(tea.PasteMsg{Content: " more"})
	if got := next.(model).input.Value(); got != "first line second line" {
		t.Fatalf("a paste reached the input behind an open list: %q", got)
	}
}

func TestRoleLabel(t *testing.T) {
	m := model{lead: config.Agent{Name: "builder-1", Role: "builder"}}
	for agent, want := range map[config.Agent]string{
		{Name: "builder-1", Role: "builder"}: "leading",
		{Name: "lead", Role: "lead"}:         "standby",
		{Name: "builder-2", Role: "builder"}: "builder",
	} {
		if got := m.roleLabel(agent); got != want {
			t.Errorf("roleLabel(%s) = %q, want %q", agent.Name, got, want)
		}
	}
}

func TestLeadLabelOncePerTurn(t *testing.T) {
	m := model{}
	m.appendUser("go")
	m.appendAssistantChunk("reading first")
	m.appendTool("t1", "read", "completed")
	m.appendAssistantChunk("all done")
	m.closeAssistant()
	m.appendUser("again")
	m.appendAssistantChunk("ok")
	m.closeAssistant()
	if n := strings.Count(m.renderTranscript(60), leadLabelStyle.Render("lead")+"\n"); n != 2 {
		t.Fatalf("lead label shown %d times, want once per turn (2)", n)
	}
}

// /copy takes the last reply in view, /copy code the newest code block in
// the replies, as an OSC 52 clipboard write.
func TestCopyTheLastReplyOrCodeBlock(t *testing.T) {
	m := model{}
	if next, cmd := m.handleSlash("/copy"); cmd != nil || !strings.Contains(next.(model).toastText(), "nothing to copy") {
		t.Fatal("/copy of an empty conversation")
	}
	m.appendUser("show me")
	m.appendAssistantChunk("Here:\n\n```go\nfunc a() {}\n```\n\n~~~sh\nmake test\n~~~\n")
	m.appendTool("t", "Read", "completed")
	m.appendAssistantChunk("Done.")
	m.closeAssistant()
	m.agentEntries = map[string][]entry{"builder-1": {{kind: entryAssistant, text: "from builder-1"}}}
	for _, c := range []struct{ focus, command, want string }{
		{"", "/copy", "Done."},
		{"", "/copy code", "make test"},
		{"builder-1", "/copy", "from builder-1"},
	} {
		m.focus = c.focus
		_, cmd := m.handleSlash(c.command)
		if cmd == nil || !reflect.DeepEqual(cmd(), tea.SetClipboard(c.want)()) {
			t.Errorf("%s following %q does not copy %q", c.command, c.focus, c.want)
		}
	}
}

// A teammate's answer arriving mid-reply goes above the reply, which streams
// on whole under the lead's name.
func TestAsideKeepsAStreamingReplyWhole(t *testing.T) {
	m := model{lead: config.Agent{Name: "lead"}}
	m.appendUser("go")
	m.appendAssistantChunk("Hello wor")
	addAside(&m.entries, entry{kind: entryAssistant, who: "builder-1", text: "an answer"})
	m.appendAssistantChunk("ld")
	if got := m.closeAssistant(); got != "Hello world" {
		t.Fatalf("the reply was split: %q", got)
	}
	out := m.renderTranscript(60)
	if strings.Index(out, "an answer") > strings.Index(out, "Hello world") || !strings.Contains(out, leadLabelStyle.Render("builder-1")+"\n") ||
		!strings.Contains(out, leadLabelStyle.Render("lead")+"\n") {
		t.Fatalf("transcript:\n%s", ansi.Strip(out))
	}
}

func TestHistoryIsScopedToTheSession(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	project := t.TempDir()
	_ = state.AppendMessage(ctx, project, "old-session", "user", "from the old session")
	_ = state.AppendMessage(ctx, project, "new-session", "user", "from this session")

	m := model{ctx: ctx, store: state, project: project}
	m.chat = &chat.Chat{Project: project, Store: state, Session: &acpdriver.Session{ID: "new-session"}}
	m.loadHistory()
	out := m.renderTranscript(60)
	if !strings.Contains(out, "from this session") || strings.Contains(out, "from the old session") {
		t.Fatalf("history shows the wrong session:\n%s", out)
	}

	m.chat.Session = &acpdriver.Session{ID: "brand-new"}
	m.loadHistory()
	if len(m.entries) != 0 {
		t.Fatalf("a new session shows %d old entries", len(m.entries))
	}
}

// When the lead's adapter exits, the transcript says how, once: on its own
// while idle, and not again in the error of a turn it ends.
func TestLeadExitSaysWhyOnce(t *testing.T) {
	exit := errors.New("codex adapter exited (exit status 1); agent stderr: fatal: out of cheese")
	count := func(m model) int { return strings.Count(ansi.Strip(m.renderTranscript(200)), "out of cheese") }

	next, _ := model{state: "ready"}.handleAgentEvent(acpdriver.Event{Type: acpdriver.EventExit, Err: exit})
	if got := count(next.(model)); got != 1 {
		t.Fatalf("an exit while idle is reported %d times", got)
	}
	next, _ = model{busy: true, state: "working"}.handleAgentEvent(acpdriver.Event{Type: acpdriver.EventExit, Err: exit})
	next, _ = next.(model).Update(turnResultMsg{err: exit})
	if got := count(next.(model)); got != 1 {
		t.Fatalf("an exit mid-turn is reported %d times", got)
	}
	if state := next.(model).state; state != "exited" { // the status line says the adapter stopped
		t.Fatalf("state after the exit and its failed Send = %q", state)
	}
}
