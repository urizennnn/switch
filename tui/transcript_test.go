package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"
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

func TestTranscriptToolsPlanAndMarkdown(t *testing.T) {
	m := model{viewport: viewport.New(viewport.WithWidth(60), viewport.WithHeight(20))}
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
	// Connecting sends it; a nil chat cannot be driven here, so only check
	// that a connection error keeps the message and asks for a retry.
	next, _ = m.Update(connectedMsg{err: errTest("adapter missing")})
	m = next.(model)
	if m.queued != "hello early" || !strings.Contains(m.renderTranscript(60), "press enter to retry") {
		t.Fatalf("after failed connect: queued=%q", m.queued)
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
