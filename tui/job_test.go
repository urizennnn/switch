package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/policy"
	"github.com/urizennnn/switch/internal/store"
)

func TestRunWithoutRunExplains(t *testing.T) {
	next, cmd := model{}.handleSlash("/run")
	got := next.(model)
	if cmd != nil || got.job != "" || !strings.Contains(got.toastText(), "/plan") {
		t.Fatalf("toast = %q, job = %q; want a /plan hint", got.toastText(), got.job)
	}
}

func TestRunJobKeepsChatUsableAndGuardsPlan(t *testing.T) {
	m := model{ctx: context.Background(), run: &store.Run{ID: "run"}}
	m.startJob("run", "running", false)
	defer m.endJob()
	if m.busy {
		t.Fatal("a run job blocked the chat input")
	}
	next, cmd := m.handleSlash("/plan build it")
	got := next.(model)
	if cmd != nil || !strings.Contains(got.toastText(), "/stop") {
		t.Fatalf("/plan during a run: toast = %q", got.toastText())
	}
}

// A detached run is a run in progress too: /plan would hide it behind a newer
// run, and /adapters could remove the adapter its workers use.
func TestDetachedRunGuardsPlanAndAdapters(t *testing.T) {
	m := model{ctx: context.Background(), run: &store.Run{ID: "r1"}, runOwner: 4242}
	for _, command := range []string{"/plan next goal", "/adapters", "/run"} {
		next, cmd := m.handleSlash(command)
		got := next.(model)
		if cmd != nil || got.job != "" || got.adapterPicker != nil || !strings.Contains(got.toastText(), "/stop") {
			t.Errorf("%s during a detached run: toast %q", command, got.toastText())
		}
	}
}

// The TUI shows only the newest run, so planning again hides an unfinished
// one: /plan says so once, and goes ahead when sent again.
func TestPlanWarnsBeforeReplacingAnUnfinishedRun(t *testing.T) {
	m := model{ctx: context.Background(), run: &store.Run{ID: "r1"},
		tasks: []store.Task{{Status: execution.StatusMerged}, {Status: execution.StatusReview}, {Status: execution.StatusBlocked}}}
	next, cmd := m.handleSlash("/plan next goal")
	m = next.(model)
	if cmd != nil || m.job != "" || !strings.Contains(m.toastText(), "2 unmerged tasks") || m.input.Value() != "/plan next goal" {
		t.Fatalf("first /plan: job %q, input %q, toast %q", m.job, m.input.Value(), m.toastText())
	}
	next, cmd = m.handleSlash(m.input.Value())
	m = next.(model)
	defer m.endJob()
	if cmd == nil || m.job != "plan" {
		t.Fatalf("/plan sent again did not plan: job %q, toast %q", m.job, m.toastText())
	}
}

// The planner is a fresh session, so /plan hands it the conversation so
// far, and a bare /plan plans from that alone.
func TestPlanFromTheConversation(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	m := model{ctx: context.Background(), project: "/p", store: state, lead: config.Agent{Name: "lead"}}
	next, cmd := m.handleSlash("/plan")
	if got := next.(model); cmd != nil || got.job != "" || !strings.Contains(got.toastText(), "/plan <goal>") {
		t.Fatalf("bare /plan with nothing said: job %q, toast %q", got.job, got.toastText())
	}

	m.chat = &chat.Chat{Project: "/p", Store: state, Session: &acpdriver.Session{ID: "s1"}}
	if err := state.AppendMessage(context.Background(), "/p", "s1", "system", "lead (opencode) is now the lead"); err != nil {
		t.Fatal(err)
	}
	if next, cmd = m.handleSlash("/plan"); cmd != nil || next.(model).job != "" {
		t.Fatal("bare /plan planned from Switch's own note")
	}
	for _, message := range []struct{ role, content string }{
		{"user", "[Switch] The user made you the lead in place of a. The recent conversation:\n\nThe user: hi"},
		{"user", planModeNote + "status needs a --json flag"},
		{"assistant", "Agreed: status gets --json."},
	} {
		if err := state.AppendMessage(context.Background(), "/p", "s1", message.role, message.content); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := m.conversation(), "[switch]\n[Switch] The user made you the lead in place of a. The recent conversation:\n\nThe user: hi\n\n[user]\nstatus needs a --json flag\n\n[assistant]\nAgreed: status gets --json."; got != want {
		t.Fatalf("conversation =\n%s\nwant\n%s", got, want)
	}
	next, cmd = m.handleSlash("/plan")
	m = next.(model)
	defer m.endJob()
	if cmd == nil || m.job != "plan" {
		t.Fatalf("bare /plan after a conversation did not plan: job %q, toast %q", m.job, m.toastText())
	}
}

// /why says what each unfinished task waits for, as `__control status` does,
// and so does the result of a run that ends stuck.
func TestWhySaysWhatTasksWaitFor(t *testing.T) {
	m := model{ctx: context.Background(), run: &store.Run{ID: "r1", Status: execution.RunBlocked},
		runAgents: []store.RunAgent{{Name: "lead", Role: "lead"}, {Name: "builder-1", Role: "builder"}},
		tasks: []store.Task{
			{ID: "r1:schema", Role: "builder", Status: execution.StatusMerged},
			{ID: "r1:api", Role: "builder", Status: execution.StatusBlocked, LastError: "tests failed\nFAIL TestAPI"},
			{ID: "r1:docs", Role: "builder", Status: execution.StatusQueued, Deps: []string{"r1:api"}},
			{ID: "r1:ui", Role: "designer", Status: execution.StatusQueued},
		}}
	want := []string{"api              blocked: tests failed\n", "docs             waits for api, which is blocked", "ui               no designer worker in this run"}
	next, _ := m.handleSlash("/why")
	why := next.(model).modal
	for _, line := range want {
		if !strings.Contains(why, line) {
			t.Errorf("/why misses %q:\n%s", line, why)
		}
	}
	if strings.Contains(why, "schema") {
		t.Errorf("/why lists a merged task:\n%s", why)
	}
	next, _ = m.Update(runResultMsg{run: "r1", outcome: execution.RunBlocked})
	if outcome := next.(model).modal; !strings.Contains(outcome, "no task can make progress") || !strings.Contains(outcome, want[1]) {
		t.Fatalf("a blocked run does not say why:\n%s", outcome)
	}

	// Every task merged and the run blocked: the run's own checks failed.
	m.tasks = m.tasks[:1]
	if next, _ = m.handleSlash("/why"); next.(model).modal != "" || !strings.Contains(next.(model).toastText(), "own checks failed") {
		t.Fatalf("/why on a run its checks blocked: modal %q, toast %q", next.(model).modal, next.(model).toastText())
	}
	m.run.Status = execution.RunWaiting
	if next, _ = m.handleSlash("/why"); next.(model).modal != "" || !strings.Contains(next.(model).toastText(), "nothing waits") {
		t.Fatalf("/why with nothing waiting: modal %q, toast %q", next.(model).modal, next.(model).toastText())
	}
}

// A run that completes in the TUI gets its PR description written, as a
// headless one does.
func TestCompletedRunWritesItsPRDescription(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	project := t.TempDir()
	if err := state.CreateRun(ctx, store.Run{ID: "r1", Project: project, Goal: "add --json", IntegrationBranch: "switch/r1/integration", Status: execution.RunCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveTask(ctx, store.Task{ID: "r1:flag", RunID: "r1", Title: "Add the flag", Status: execution.StatusMerged}); err != nil {
		t.Fatal(err)
	}
	m := New(Options{Context: ctx, Project: project, Store: state, Lead: config.Agent{Name: "lead"}})
	defer m.cancel()
	next, _ := m.Update(runResultMsg{run: "r1", outcome: execution.RunCompleted})
	path := filepath.Join(execution.RunDir(config.StateDir(), "r1"), "summary.md")
	summary, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(summary), "Add the flag") {
		t.Fatalf("summary = %q, %v", summary, err)
	}
	if modal := next.(model).modal; !strings.Contains(modal, "PR description: "+shortenHome(path)) {
		t.Fatalf("the run result does not say where the PR description is:\n%s", modal)
	}
}

func TestJobLogKeepsAgentLinesWhole(t *testing.T) {
	m := model{ctx: context.Background()}
	m.startJob("run", "running", false)
	defer m.endJob()
	m.appendJobText("builder-1", "hel")
	m.appendJobText("builder-2", "other line\n")
	m.appendJobText("builder-1", "lo\nnext")
	want := "builder-2  other line\nbuilder-1  hello\nbuilder-1  next"
	if got := m.jobLogText(); got != want {
		t.Fatalf("log =\n%s\nwant\n%s", got, want)
	}
}

func TestJobLogUnfinishedLinesKeepTheirOrder(t *testing.T) {
	m := model{ctx: context.Background()}
	m.startJob("run", "running", false)
	defer m.endJob()
	for _, agent := range []string{"builder-3", "builder-1", "builder-2/scout", "builder-2"} {
		m.appendJobText(agent, "mid-line")
	}
	want := "builder-1  mid-line\nbuilder-2  mid-line\nbuilder-2 › scout  mid-line\nbuilder-3  mid-line"
	for range 20 { // map order changes from one redraw to the next
		if got := m.jobLogText(); got != want {
			t.Fatalf("log =\n%s\nwant\n%s", got, want)
		}
	}
}

// The run log panel shows the end of the output; /log reads all of it.
func TestLogReadsBackTheRunOutput(t *testing.T) {
	m := New(Options{Context: context.Background()})
	defer m.cancel()
	m.run = &store.Run{ID: "r1"}
	m.startJob("run", "running", false)
	defer m.endJob()
	for i := 1; i <= 100; i++ {
		m.appendJobText("builder-1", fmt.Sprintf("step %d\n", i))
	}
	next, _ := m.handleSlash("/log")
	m = next.(model)
	if screen := ansi.Strip(m.screen()); m.modalTitle != "run log" || !strings.Contains(screen, "step 100") || !strings.Contains(screen, "of 100") {
		t.Fatalf("/log during a run does not open at the end of its output:\n%s", screen)
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	if screen := ansi.Strip(next.(model).screen()); !strings.Contains(screen, "step 1 ") {
		t.Fatalf("/log does not scroll back to the start of the output:\n%s", screen)
	}
}

func TestFindTaskByPlanID(t *testing.T) {
	m := model{tasks: []store.Task{{ID: "run-1:add-flag"}, {ID: "run-1:docs"}}}
	if task, ok := m.findTask("docs"); !ok || task.ID != "run-1:docs" {
		t.Fatalf("findTask(docs) = %q, %v", task.ID, ok)
	}
	if _, ok := m.findTask("missing"); ok {
		t.Fatal("found a missing task")
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

func keyEnter() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter} }

func TestPlanLogShowsTasksNotJSON(t *testing.T) {
	m := model{ctx: context.Background(), lead: config.Agent{Name: "builder-1"}}
	m.startJob("plan", "planning", true)
	defer m.endJob()
	m.appendJobText("builder-1", "Looking at the repos first.\n[tool] shell (pending)\nThen I will split `go.mod` work.\n```json\n{\"goal\": \"g\", \"tasks\": [\n")
	m.appendJobText("builder-1", `{"id": "a", "title": "Map the \"repos\"", "files": [".switch/x.md"]}, {"id": "b", "title": "Research portal UI",`)
	m.appendJobText("builder-1", ` "brief": "read"}, {"id": "c",`)
	got := m.planProgress(80, 20)
	for _, want := range []string{"builder-1 is planning", "using shell", "split `go.mod` work", `Map the "repos"`, "Research portal UI", "drafting task 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan log missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"files"`) {
		t.Errorf("plan log shows raw JSON:\n%s", got)
	}
}

// A stopped, timed-out or cancelled run turn ends without a closing event:
// the run's state, not the stream, says the agent is done.
func TestRunTurnsThatEndWithoutAnEventStopWorking(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	project := t.TempDir()
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}
	b1 := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	b2 := config.Agent{Name: "builder-2", Role: "builder", Driver: "claude"}
	m := New(Options{Context: ctx, Project: project, Store: state, Lead: lead, Fleet: []config.Agent{lead, b1, b2}})
	defer m.cancel()
	work := store.Task{ID: "r1:a", RunID: "r1", Title: "auth", Status: execution.StatusRunning, Assignee: "builder-1"}
	review := store.Task{ID: "r1:b", RunID: "r1", Title: "docs", Status: execution.StatusReviewing, Assignee: "builder-2"}
	scout := store.Subagent{ID: "r1:a/scout", RunID: "r1", TaskID: "r1:a", Parent: "builder-1", Name: "scout", Status: execution.SubRunning}
	for _, err := range []error{
		state.CreateRun(ctx, store.Run{ID: "r1", Project: project, Status: "running"}),
		state.SaveRunAgent(ctx, store.RunAgent{ID: "r1:lead", RunID: "r1", Name: "lead", Role: "lead"}),
		state.SaveTask(ctx, work), state.SaveTask(ctx, review), state.SaveSubagent(ctx, scout),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	update := func(msg tea.Msg) {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	m.startJob("run", "running", false)
	// The worker's task, its subagent and the lead's review of builder-2's task.
	for _, name := range []string{"builder-1", "builder-1/scout", "lead"} {
		update(streamMsg{agent: name, event: acpdriver.Event{Type: acpdriver.EventStart}})
	}
	m.team.asked["builder-2"] = true // and a teammate answering in the chat
	update(streamMsg{agent: "builder-2", event: acpdriver.Event{Type: acpdriver.EventStart}})
	update(jobChangedMsg{})
	if len(m.agentBusy) != 4 {
		t.Fatalf("busy = %v while every turn runs", m.agentBusy)
	}

	// ctrl+x, a turn timeout and a stopped subagent: the store moves on, and
	// the run's 2s refresh notices, whatever else the run is doing.
	work.Status, review.Status, scout.Status = execution.StatusBlocked, execution.StatusReview, execution.SubStopped
	for _, err := range []error{state.SaveTask(ctx, work), state.SaveTask(ctx, review), state.SaveSubagent(ctx, scout)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	update(dashboardTickMsg{})
	if _, busy := m.agentBusy["builder-2"]; len(m.agentBusy) != 1 || !busy {
		t.Fatalf("busy = %v after the run's turns ended", m.agentBusy)
	}

	// /stop cuts a turn off; the run's end settles it.
	update(streamMsg{agent: "builder-1/scout", event: acpdriver.Event{Type: acpdriver.EventStart}})
	update(runResultMsg{outcome: execution.RunStopped, err: context.Canceled})
	if _, busy := m.agentBusy["builder-2"]; len(m.agentBusy) != 1 || !busy {
		t.Fatalf("busy = %v after the run ended", m.agentBusy)
	}
	update(streamMsg{agent: "builder-2", event: acpdriver.Event{Type: acpdriver.EventTurn}})
	delete(m.team.asked, "builder-2")
	if m.timing() {
		t.Fatal("the clock keeps running with nothing at work")
	}
	next, _ := m.handleSlash("/mode full")
	if got := next.(model).permissionMode; got != policy.Full {
		t.Fatalf("/mode refused after every turn ended: %s, %q", got, next.(model).toastText())
	}
}
