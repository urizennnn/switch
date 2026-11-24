package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/store"
)

// A lead switch copies the Chat a Send still writes to, so it waits for the
// turn: a message queued at launch can start one while the picker is open.
func TestLeadSwitchWaitsForTheTurn(t *testing.T) {
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := model{ctx: context.Background(), lead: config.Agent{Name: "lead"}, chat: &chat.Chat{Session: &acpdriver.Session{ID: "s1"}},
		busy: true, sending: true, state: "working"}
	next, cmd := m.beginLeadSwitch(builder)
	if got := next.(model); cmd != nil || got.lead.Name != "lead" || got.gen != m.gen || !strings.Contains(got.toastText(), "busy") {
		t.Fatalf("switched mid-turn: lead %s, toast %q", got.lead.Name, got.toastText())
	}
}

// A handoff to a teammate that is answering, or working on a run task, waits until it is
// free: a switch then would start a second session beside its own.
func TestHandoffWaitsForABusyTarget(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	project := t.TempDir()
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode", Handoff: "builder-1"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: project, Store: state, Lead: lead, Fleet: []config.Agent{lead, builder}})
	defer m.cancel()
	m.chat = &chat.Chat{Project: project, Store: state, Session: &acpdriver.Session{ID: "s1"}}
	m.connecting, m.state = false, "ready"
	busy := func(on bool) { m.team.mu.Lock(); m.team.asked["builder-1"] = on; m.team.mu.Unlock() }
	busy(true)
	if _, err := m.team.Handoff("lead", "", "", "the README outline is agreed"); err != nil {
		t.Fatal(err)
	}
	next, _ := m.Update(<-m.handoffs)
	m = next.(model)
	next, _ = m.Update(dashboardTickMsg{})
	m = next.(model)
	if m.lead.Name != "lead" || m.pendingHandoff == nil || strings.Count(m.toastText(), "once it is free") != 1 {
		t.Fatalf("handoff to a busy teammate: lead %s, pending %v, toasts %q", m.lead.Name, m.pendingHandoff != nil, m.toastText())
	}
	busy(false)
	next, _ = m.Update(dashboardTickMsg{})
	if m = next.(model); m.lead.Name != "builder-1" || m.state != "starting lead" || m.pendingHandoff != nil {
		t.Fatalf("handoff once builder-1 is free: lead %s, state %s", m.lead.Name, m.state)
	}
}

// A prompt nobody waits for any more, as its turn ended or was stopped,
// leaves the screen and the queue rather than holding the next ones back.
func TestWithdrawnPromptsLeave(t *testing.T) {
	m := New(Options{Context: context.Background()})
	defer m.cancel()
	options := []acpdriver.PermissionOption{{ID: "a", Name: "Allow once", Kind: "allow_once"}}
	ended, queued, live, sub := &acpdriver.PermissionRequest{Agent: "builder-1", Kind: "edit", Options: options},
		&acpdriver.PermissionRequest{Agent: "builder-2", Kind: "edit", Options: options},
		&acpdriver.PermissionRequest{Agent: "builder-3", Kind: "edit", Options: options},
		&acpdriver.PermissionRequest{Agent: "builder-3/scout", Kind: "edit", Options: options}
	answers := []chan acpdriver.PermissionDecision{reply(ended), reply(queued), reply(live), reply(sub)}
	for _, request := range []*acpdriver.PermissionRequest{ended, queued, live, sub} {
		next, _ := m.update(teamPermissionMsg{request: request})
		m = next.(model)
	}
	ended.Cancel() // the driver, once the turn's request ended
	queued.Cancel()
	next, _ := m.Update(dashboardTickMsg{})
	if m = next.(model); m.permission != live || len(m.permissionQueue) != 1 {
		t.Fatalf("after the tick the prompt shows %+v with %d waiting", m.permission, len(m.permissionQueue))
	}
	m.withdrawPrompts("builder-3") // ctrl+x on builder-3
	if m.permission != nil || len(m.permissionQueue) != 0 {
		t.Fatalf("a stopped teammate's prompts stayed: %+v, %d waiting", m.permission, len(m.permissionQueue))
	}
	for _, answer := range answers[2:] {
		if got := <-answer; !got.Cancel {
			t.Fatalf("a stopped teammate's prompt was answered %+v", got)
		}
	}
}

// A subagent messaged as its parent's answer ends finds its session ended
// and stops, rather than running a turn on no session.
func TestSubagentMessagedAsItsSessionEnds(t *testing.T) {
	tm := fakeTeam(t)
	tm.subCfg.MaxPerBuilder = 1
	tm.asked["builder-1"] = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := tm.Spawn("builder-1", "", execution.SpawnRequest{Name: "scout", Brief: "look"}); err != nil {
		t.Fatal(err)
	}
	tm.Wait(ctx, "builder-1", "", nil)
	events := make(chan streamMsg) // nobody reads it yet: the follow-up waits before its turn
	tm.events = events
	if _, err := tm.Message("builder-1", "", "scout", "again"); err != nil {
		t.Fatal(err)
	}
	tm.mu.Lock() // stopSubs, after its Stop found nothing running
	tm.subs["builder-1/scout"].endSession()
	tm.mu.Unlock()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-events:
			case <-done:
				return
			}
		}
	}()
	if subs, _ := tm.Wait(ctx, "builder-1", "", nil); len(subs) != 1 || subs[0].Status != execution.SubStopped {
		t.Fatalf("subagents = %+v", subs)
	}
}

// /run on a completed run does not run its checks again: landed and pruned,
// they could only block it. Reject and retry say who picks the task up.
func TestRunCommandsOnAFinishedOrLiveRun(t *testing.T) {
	m := model{ctx: context.Background(), run: &store.Run{ID: "r1", Status: execution.RunCompleted},
		tasks: []store.Task{{ID: "r1:a", Status: execution.StatusMerged}}}
	next, cmd := m.handleSlash("/run")
	if got := next.(model); cmd != nil || got.job != "" || !strings.Contains(got.toastText(), "/land") {
		t.Fatalf("/run on a completed run: job %q, toast %q", got.job, got.toastText())
	}

	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	project := t.TempDir()
	if out, err := exec.Command("git", "-C", project, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git: %v %s", err, out)
	}
	if err := state.CreateRun(ctx, store.Run{ID: "r1", Project: project, Status: execution.RunWaiting}); err != nil {
		t.Fatal(err)
	}
	m = model{ctx: ctx, store: state, project: project, run: &store.Run{ID: "r1"}}
	for job, want := range map[string]string{"": "/run continues the run", "run": "the running run picks it up"} {
		if err := state.SaveTask(ctx, store.Task{ID: "r1:a", RunID: "r1", Title: "a", Status: execution.StatusReview}); err != nil {
			t.Fatal(err)
		}
		m.job = job
		if msg := m.taskAction("reject", "r1:a", "missing test")().(taskActionMsg); !strings.HasSuffix(msg.body, want) {
			t.Fatalf("reject with job %q: %s: %s", job, msg.title, msg.body)
		}
	}
}

// A run's [run] settings are read again for each engine, so raising a budget
// the run stopped at takes effect without restarting Switch; /parallel's
// limit stays.
func TestEnginesReadTheRunConfigAgain(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".switch"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := model{project: project, runConfig: config.RunConfig{MaxTurns: 1, MaxConcurrency: 3}}
	if err := os.WriteFile(config.ProjectConfigPath(project), []byte("[run]\nmax_turns = 200\nmax_concurrency = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := m.runSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxTurns != 200 || got.MaxConcurrency != 3 {
		t.Fatalf("engine config: max_turns %d, max_concurrency %d", got.MaxTurns, got.MaxConcurrency)
	}
}
