package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/control"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/policy"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

func testTeam() *team {
	return newTeam("/p", []config.Agent{
		{Name: "lead", Role: "lead", Driver: "opencode"},
		{Name: "builder-1", Role: "builder", Driver: "claude"},
		{Name: "builder-2", Role: "builder", Driver: "codex"},
	}, "builder-1")
}

func TestAskRules(t *testing.T) {
	tm := testTeam()
	ctx := context.Background()
	for _, c := range []struct{ from, to, want string }{
		{"builder-1", "builder-1", "yourself"},
		{"builder-1", "nobody", "no teammate"},
		{"builder-2", "builder-1", "is the lead"},
	} {
		if _, err := tm.Ask(ctx, c.from, c.to, "hi"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Ask(%s→%s) err = %v, want %q", c.from, c.to, err, c.want)
		}
	}
	// A teammate being consulted cannot consult others.
	tm.asked["builder-2"] = true
	if _, err := tm.Ask(ctx, "builder-2", "lead", "hi"); err == nil || !strings.Contains(err.Error(), "cannot consult") {
		t.Fatalf("nested ask err = %v", err)
	}
	if _, err := tm.Ask(ctx, "builder-1", "builder-2", "hi"); err == nil || !strings.Contains(err.Error(), "answering another") {
		t.Fatalf("busy teammate err = %v", err)
	}
}

func TestMembersAndDescribe(t *testing.T) {
	tm := testTeam()
	tm.warm["builder-2"] = &acpdriver.Session{}
	members := tm.Members("builder-1")
	roles := map[string]string{}
	for _, m := range members {
		roles[m.Name] = m.Role + "/" + m.Status
	}
	if roles["lead"] != "standby lead/idle" || roles["builder-1"] != "builder/leading" || roles["builder-2"] != "builder/ready" {
		t.Fatalf("members = %v", roles)
	}
	note := tm.describe("builder-1")
	if !strings.Contains(note, "You are builder-1") || !strings.Contains(note, "builder-2 (builder, codex, ready)") || strings.Contains(note, "switch_ask") {
		t.Fatalf("describe without a server = %q", note)
	}
	if took := tm.take("builder-2"); took == nil || tm.status("builder-2") != "idle" {
		t.Fatal("take did not remove the warm session")
	}
}

func TestStoppedListenerExits(t *testing.T) {
	stop := make(chan struct{})
	cmd := waitForActivity(&acpdriver.Session{}, 1, stop)
	done := make(chan any, 1)
	go func() { done <- cmd() }()
	close(stop)
	select {
	case msg := <-done:
		if msg != nil {
			t.Fatalf("stopped listener returned %#v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not stop")
	}
}

func TestChatStaysUsableDuringAdapterInstall(t *testing.T) {
	m := model{installing: "codex install globally"}
	if m.busy {
		t.Fatal("an install marked the chat busy")
	}
	next, _ := m.handleSlash("/adapters")
	if got := next.(model); got.adapterPicker != nil || !strings.Contains(got.toastText(), "still running") {
		t.Fatalf("second install allowed: toast=%q", got.toastText())
	}
	next, _ = m.Update(adapterActionMsg{name: "codex", action: "install globally"})
	if got := next.(model); got.installing != "" || !strings.Contains(got.toastText(), "complete") {
		t.Fatalf("after install: installing=%q toast=%q", got.installing, got.toastText())
	}
}

// A teammate's adapter can exit while it is parked. The listener reports how
// it exited, and the next question or lead switch starts a new session.
func TestExitedSessionIsNotReused(t *testing.T) {
	// Answers initialize and session/new, then runs until its stdin closes.
	script := `read -r _; echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
read -r _; echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s1"}}'
cat >/dev/null`
	session, err := acpdriver.Open(context.Background(), acpdriver.Spec{Agent: "fake", Command: []string{"sh", "-c", script}, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_ = session.Close(context.Background())

	if msg, _ := waitForActivity(session, 1, nil)().(agentEventMsg); msg.event.Type != acpdriver.EventExit || msg.event.Err == nil {
		t.Fatalf("listener got %+v, want the exit with how it ended", msg.event)
	}
	tm := testTeam()
	tm.park("builder-2", session)
	if tm.take("builder-2") != nil {
		t.Fatal("take returned a session whose adapter exited")
	}
}

func fakeTeam(t *testing.T) *team {
	fakeAdapter(t)
	tm := newTeam(t.TempDir(), []config.Agent{
		{Name: "lead", Role: "lead", Driver: "claude"},
		{Name: "builder-1", Role: "builder", Driver: "claude"},
	}, "lead")
	tm.ctx, tm.stop = context.WithCancel(context.Background()) // as New sets it
	t.Cleanup(tm.close)
	return tm
}

// stays reports whether a session's adapter is still running a moment later.
func stays(s *acpdriver.Session) bool {
	select {
	case <-s.Done():
		return false
	case <-time.After(200 * time.Millisecond):
		return true
	}
}

func TestAskedTeammateOutlivesTheRequest(t *testing.T) {
	tm := fakeTeam(t)
	for _, question := range []string{"one", "two"} {
		// The control server cancels the request once Ask returns.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		reply, err := tm.Ask(ctx, "lead", "builder-1", question)
		cancel()
		if err != nil || !strings.HasPrefix(reply, "echo: ") || !strings.Contains(reply, "\n"+question+"\n") {
			t.Fatalf("ask %q: reply %q, err %v", question, reply, err)
		}
		if s := tm.warm["builder-1"]; s == nil || !stays(s) || tm.status("builder-1") != "ready" {
			t.Fatalf("after ask %q the teammate is %s", question, tm.status("builder-1"))
		}
	}
	// Quitting ends the team's work first; the session waits for its graceful
	// Close, which might be the lead's if the user made builder-1 the lead.
	tm.stop()
	if !stays(tm.warm["builder-1"]) {
		t.Fatal("ending the team's work killed a teammate's session")
	}
}

func TestStartsGiveUpWithTheirRequest(t *testing.T) {
	tm := fakeTeam(t)
	stuckAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := tm.Ask(ctx, "lead", "builder-1", "hi"); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("an ask waited %s for a start past its deadline: %v", time.Since(start), err)
	}

	// The teammate answers while its subagent is still starting.
	tm.subCfg.MaxPerBuilder = 1
	tm.asked["builder-1"] = true
	if _, err := tm.Spawn("builder-1", "", execution.SpawnRequest{Name: "scout", Brief: "look"}); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if tm.stopSubs("builder-1"); time.Since(start) > 5*time.Second {
		t.Fatalf("stopping a starting subagent took %s", time.Since(start))
	}
}

func TestDeadTeammateIsNotReady(t *testing.T) {
	tm := fakeTeam(t)
	session, err := chat.StartSession(context.Background(), tm.project, "builder-1", "claude", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	tm.park("builder-1", session)
	_ = session.Close(context.Background()) // the adapter exits on its own
	if got := tm.status("builder-1"); got == "ready" {
		t.Fatal("a teammate whose adapter exited is ready")
	}
	if warm := tm.toWarm(); len(warm) != 1 || warm[0].Name != "builder-1" {
		t.Fatalf("toWarm = %v, want builder-1 started again", warm)
	}
	if tm.take("builder-1") != nil {
		t.Fatal("take handed out a dead session")
	}
}

func TestAbandonedAnswerIsNotTheNextAnswer(t *testing.T) {
	tm := fakeTeam(t)
	session, err := chat.StartSession(context.Background(), tm.project, "builder-1", "claude", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	tm.park("builder-1", session)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := tm.Ask(ctx, "lead", "builder-1", "wait"); err == nil {
		t.Fatal("an ask past its deadline answered")
	}
	if reply, err := tm.Ask(context.Background(), "lead", "builder-1", "two"); err != nil || !strings.Contains(reply, "\ntwo\n") {
		t.Fatalf("next ask: reply %q, err %v", reply, err)
	}
}

func TestFinishedChatSubagentStartsAgain(t *testing.T) {
	tm := fakeTeam(t)
	tm.subCfg.MaxPerBuilder = 2
	tm.asked["builder-1"] = true // answering a question
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result := func() store.Subagent {
		subs, _ := tm.Wait(ctx, "builder-1", "", nil)
		if len(subs) != 1 {
			t.Fatalf("subagents = %v", subs)
		}
		return subs[0]
	}
	if _, err := tm.Spawn("builder-1", "", execution.SpawnRequest{Name: "scout", Brief: "look around"}); err != nil {
		t.Fatal(err)
	}
	if sa := result(); sa.Status != execution.SubDone || !stays(tm.subs["builder-1/scout"].session) {
		t.Fatalf("first turn: %s %q, or its adapter died with it", sa.Status, sa.Summary)
	}
	if _, err := tm.Message("builder-1", "", "scout", "again"); err != nil {
		t.Fatal(err)
	}
	if sa := result(); sa.Status != execution.SubDone || sa.Summary != "echo: again" {
		t.Fatalf("follow-up: %s %q", sa.Status, sa.Summary)
	}
}

// A chat subagent's control-plane token ends with its session.
func TestChatSubagentTokenEndsWithItsSession(t *testing.T) {
	tm := fakeTeam(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	tm.listen()
	tm.subCfg.MaxPerBuilder = 1
	tm.asked["builder-1"] = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := tm.Spawn("builder-1", "", execution.SpawnRequest{Name: "scout", Brief: "token"}); err != nil {
		t.Fatal(err)
	}
	subs, _ := tm.Wait(ctx, "builder-1", "", nil)
	whoami := func() error {
		_, err := control.Call(tm.server.Path, subs[0].Summary, "whoami", nil) // the fake adapter answers with its token
		return err
	}
	if err := whoami(); err != nil {
		t.Fatalf("the token of a subagent whose session is open: %v", err)
	}
	tm.stopSubs("builder-1")
	if err := whoami(); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("the token once its session closed: %v", err)
	}
}

// A teammate's question waits among the permission prompts, whatever the
// mode: the user picks an option by its number, or types an answer in the
// prompt when it has none, and esc skips it. The message the user was
// typing stays theirs: typed on as the prompt opens, it goes on in the input
// and answers nothing. A question whose turn ended leaves the screen.
func TestTeammateAsksTheUser(t *testing.T) {
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "claude"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: t.TempDir(), Lead: lead, Fleet: []config.Agent{lead, builder}})
	defer m.shutdown()
	m.permissionMode = policy.Plan // it denies teammates' requests, never their questions
	turn, end := context.WithCancel(context.Background())
	m.team.turns["builder-1"] = turn // answering the lead
	m.input.SetValue("lead, please also fix the login bu")
	ask := func(options []string) chan string {
		answer := make(chan string, 1)
		go func() { answer <- m.team.Question(context.Background(), "builder-1", "", "which db?", options) }()
		next, _ := m.Update(waitTeamPermission(m.teamPerms)())
		m = next.(model)
		if screen := ansi.Strip(m.screen()); !strings.Contains(screen, "builder-1 asks you") || !strings.Contains(screen, "which db?") {
			t.Fatalf("the question is not shown:\n%s", screen)
		}
		return answer
	}
	keys := func(keys ...tea.KeyPressMsg) {
		for _, key := range keys {
			next, _ := m.Update(key)
			m = next.(model)
		}
	}
	enter := tea.KeyPressMsg{Code: tea.KeyEnter}

	answer := ask(nil)
	keys(tea.KeyPressMsg{Code: 'g', Text: "g"}, enter)
	if m.permission == nil || m.input.Value() != "lead, please also fix the login bug" || m.answer.Value() != "" {
		t.Fatalf("typing on as the question opened: prompt %v, input %q, answer %q", m.permission != nil, m.input.Value(), m.answer.Value())
	}
	m.permissionHold = time.Time{}
	keys(tea.KeyPressMsg{Code: 'o', Text: "o"}, tea.KeyPressMsg{Code: 'k', Text: "k"}, enter)
	if got := <-answer; got != "ok" || m.permission != nil || m.input.Value() != "lead, please also fix the login bug" {
		t.Fatalf("answer %q, prompt %+v, input %q", got, m.permission, m.input.Value())
	}
	for _, c := range []struct {
		options []string
		keys    []tea.KeyPressMsg
		want    string
	}{
		{[]string{"pg", "mysql"}, []tea.KeyPressMsg{{Code: '2', Text: "2"}}, "mysql"},
		{nil, []tea.KeyPressMsg{enter, {Code: tea.KeyEscape}}, "the user did not answer; decide yourself or switch_block"},
	} {
		answer := ask(c.options)
		m.permissionHold = time.Time{}
		keys(c.keys...)
		if got := <-answer; got != c.want || m.permission != nil {
			t.Fatalf("answer %q, prompt %+v; want %q", got, m.permission, c.want)
		}
	}

	answer = ask(nil)
	end()
	select {
	case request := <-m.teamPerms:
		next, _ := m.Update(teamPermissionMsg{request: request})
		m = next.(model)
	case <-time.After(10 * time.Second):
		t.Fatal("the question was not withdrawn once its turn ended")
	}
	if m.permission != nil || !strings.Contains(<-answer, "did not answer") {
		t.Fatalf("the question of a turn that ended is still shown: %+v", m.permission)
	}
}

// A teammate's token outlives its answers, but its question goes with the
// answer that asked it; with no answer under way the user is not asked.
func TestTeammateQuestionEndsWithItsAnswer(t *testing.T) {
	tm := fakeTeam(t)
	perms := make(chan *acpdriver.PermissionRequest, 2)
	tm.perms = perms
	ctx, cancelCtx := context.WithTimeout(context.Background(), time.Second)
	defer cancelCtx()
	if got := tm.Question(ctx, "builder-1", "", "which db?", nil); !strings.Contains(got, "no turn running") || len(perms) != 0 {
		t.Fatalf("a question with no answer under way = %q", got)
	}
	askCtx, cancel := context.WithCancel(context.Background())
	asked := make(chan error, 1)
	go func() {
		_, err := tm.Ask(askCtx, "lead", "builder-1", "wait")
		asked <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		tm.mu.Lock()
		answering := tm.turns["builder-1"] != nil
		tm.mu.Unlock()
		if answering {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("builder-1 never started answering")
		}
	}
	answer := make(chan string, 1)
	go func() { answer <- tm.Question(context.Background(), "builder-1", "", "which db?", nil) }()
	shown := <-perms
	cancel()
	<-asked
	select {
	case got := <-answer:
		if !strings.Contains(got, "did not answer") {
			t.Fatalf("the question once its answer ended = %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the question outlived the answer that asked it")
	}
	if withdrawn := <-perms; withdrawn != shown || !withdrawn.Withdrawn() {
		t.Fatal("the user's prompt was not withdrawn")
	}
}

func TestQuitWhileATeammateAnswers(t *testing.T) {
	fakeAdapter(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "claude"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: t.TempDir(), Lead: lead, Fleet: []config.Agent{lead, builder}})
	m.team.listen()
	if m.team.server == nil {
		t.Fatal("no team server")
	}
	grant, err := m.team.server.Grant(store.RunAgent{Name: "lead", Role: "lead"}, "")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range grant.Env {
		key, value, _ := strings.Cut(kv, "=")
		env[key] = value
	}
	go func() {
		_, _ = control.Call(env[control.EnvSocket], env[control.EnvToken], "ask", map[string]string{"to": "builder-1", "message": "flood"})
	}()
	// The answer streams more than the TUI buffers, and nothing reads it
	// while Update runs the quit.
	for deadline := time.Now().Add(10 * time.Second); len(m.streams) < cap(m.streams); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the teammate never filled the stream")
		}
	}
	quit := make(chan struct{})
	go func() {
		m.shutdown()
		close(quit)
	}()
	select {
	case <-quit:
	case <-time.After(10 * time.Second):
		t.Fatal("quitting hangs while a teammate answers")
	}
}

func TestAskWaitsForARunTask(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	project := t.TempDir()
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: ctx, Project: project, Store: state, Lead: lead, Fleet: []config.Agent{lead, builder}})
	if err := state.CreateRun(ctx, store.Run{ID: "r1", Project: project, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	task := store.Task{ID: "r1:a", RunID: "r1", Title: "auth", Status: execution.StatusRunning, Assignee: "builder-1"}
	if err := state.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	m.refreshDashboard() // nothing drives the run, as after a crash: what the store says is stale
	if got := m.team.status("builder-1"); got == "working" {
		t.Fatal("a run nothing drives keeps builder-1 working")
	}
	m.job = "run"
	m.refreshDashboard()
	if got := m.team.status("builder-1"); got != "working" {
		t.Fatalf("status = %q while builder-1 runs a task", got)
	}
	if _, err := m.team.Ask(ctx, "lead", "builder-1", "hi"); err == nil || !strings.Contains(err.Error(), "run task") {
		t.Fatalf("ask during a run task: %v", err)
	}
	task.Status = execution.StatusReviewing // the lead reviews it; builder-1 is free
	if err := state.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	m.refreshDashboard()
	if got := m.team.status("builder-1"); got == "working" {
		t.Fatal("builder-1 is working while the lead reviews its task")
	}
	// The run's lead reviews it, and stays the run's when the chat's changes.
	if err := state.SaveRunAgent(ctx, store.RunAgent{RunID: "r1", Name: "lead", Role: "lead"}); err != nil {
		t.Fatal(err)
	}
	m.refreshDashboard()
	m.team.setLead("builder-1")
	if got := m.team.status("lead"); got != "working" {
		t.Fatalf("the run's lead is %q while it reviews a task", got)
	}
	if _, err := m.team.Ask(ctx, "builder-1", "lead", "hi"); err == nil || !strings.Contains(err.Error(), "run task") {
		t.Fatalf("ask during the run lead's review: %v", err)
	}
}

// While a run is driven, by a detached Switch or this one, the chat's lead
// sees it through switch_tasks, and Steer reaches this Switch's workers; once
// nothing drives it, the lead sees none.
func TestChatLeadSeesTheActiveRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	project := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", project).CombinedOutput(); err != nil { // the engine's worktrees need one
		t.Fatalf("git init: %v\n%s", err, out)
	}
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "opencode"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: ctx, Project: project, Store: state, Lead: lead, Fleet: []config.Agent{lead, builder}})
	defer m.shutdown()
	m.team.listen()
	if err := state.CreateRun(ctx, store.Run{ID: "r1", Project: project, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveTask(ctx, store.Task{ID: "r1:a", RunID: "r1", Title: "auth", Status: execution.StatusRunning, Assignee: "builder-1"}); err != nil {
		t.Fatal(err)
	}
	grant, err := m.team.server.Grant(store.RunAgent{Name: "lead", Role: "lead"}, "")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range grant.Env {
		key, value, _ := strings.Cut(kv, "=")
		env[key] = value
	}
	tasks := func() (any, error) { return control.Call(env[control.EnvSocket], env[control.EnvToken], "tasks", nil) }

	m.refreshDashboard() // nothing drives the run
	if _, err := tasks(); err == nil {
		t.Fatal("the lead sees a run nothing drives")
	}
	detached, err := execution.LockRun(config.StateDir(), "r1", true)
	if err != nil {
		t.Fatal(err)
	}
	m.refreshDashboard()
	if got, err := tasks(); err != nil || !strings.Contains(fmt.Sprint(got), "auth") {
		t.Fatalf("tasks during a detached run = %v, %v", got, err)
	}
	detached.Release()
	engine, err := execution.New(ctx, execution.Options{Store: state, RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	m.job, m.engine = "run", engine
	m.refreshDashboard()
	if got, err := tasks(); err != nil || !strings.Contains(fmt.Sprint(got), "auth") || m.team.engine != engine {
		t.Fatalf("tasks during the run = %v, %v", got, err)
	}
	m.job, m.engine = "", nil
	m.refreshDashboard()
	if _, err := tasks(); err == nil || m.team.engine != nil {
		t.Fatal("the lead still sees the run once it ended")
	}
}

// A teammate whose adapter cannot be steered mid-answer gets what it was
// sent at the start of its next prompt, even when an answer fails to start
// in between. The lead and agents with no turn running are not steered.
func TestSteerATeammate(t *testing.T) {
	tm := fakeTeam(t)
	ctx := context.Background()
	for to, want := range map[string]string{"lead": "is the lead", "builder-1": "no turn running"} {
		if _, err := tm.Steer(ctx, to, "use v2"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Steer(%s) = %v, want %q", to, err, want)
		}
	}
	askCtx, cancel := context.WithCancel(ctx)
	asked := make(chan error, 1)
	go func() {
		_, err := tm.Ask(askCtx, "lead", "builder-1", "wait")
		asked <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		tm.mu.Lock()
		answering := tm.active["builder-1"] != nil
		tm.mu.Unlock()
		if answering {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("builder-1 never started answering")
		}
	}
	if reply, err := tm.Steer(ctx, "builder-1", "use v2"); err != nil || !strings.Contains(reply, "next prompt") {
		t.Fatalf("Steer while it answers = %q, %v", reply, err)
	}
	cancel()
	<-asked
	installAdapter(t, "exit 1")
	if _, err := tm.Ask(ctx, "lead", "builder-1", "lost"); err == nil {
		t.Fatal("builder-1 answered with an adapter that does not start")
	}
	fakeAdapter(t)
	if reply, err := tm.Ask(ctx, "lead", "builder-1", "two"); err != nil || !strings.HasPrefix(reply, "echo: Sent to you during your previous turn: use v2\n\n") {
		t.Fatalf("next answer = %q, %v", reply, err)
	}
}

func TestSteerCommand(t *testing.T) {
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "claude"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: t.TempDir(), Lead: lead, Fleet: []config.Agent{lead, builder}})
	defer m.shutdown()
	if next, cmd := m.handleSlash("/steer builder-1"); cmd != nil || !strings.Contains(next.(model).toastText(), "/steer <agent> <text>") {
		t.Fatalf("/steer without text: toast %q", next.(model).toastText())
	}
	_, cmd := m.handleSlash("/steer builder-1 use v2")
	if msg, ok := cmd().(taskActionMsg); !ok || msg.title != "steer failed" || !strings.Contains(msg.body, "no turn running") {
		t.Fatalf("/steer an idle teammate = %+v", msg)
	}
	if !strings.Contains(helpText(), "/steer <agent> <text>") {
		t.Fatal("/help does not list /steer")
	}
}

// "@builder-2 question" goes to builder-2 as switch_ask would, not to the
// lead, and the answer joins the conversation under its name.
func TestMentionAsksATeammate(t *testing.T) {
	fakeAdapter(t)
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "claude"}
	b1 := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	b2 := config.Agent{Name: "builder-2", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: t.TempDir(), Lead: lead, Fleet: []config.Agent{lead, b1, b2}})
	defer m.shutdown()
	update := func(msg tea.Msg) tea.Cmd {
		next, cmd := m.Update(msg)
		m = next.(model)
		return cmd
	}
	tab := tea.KeyPressMsg{Code: tea.KeyTab}

	// Tab completes a name as far as the names agree, then lists them.
	m.input.SetValue("@b")
	if update(tab); m.input.Value() != "@builder-" || m.focus != "" {
		t.Fatalf("tab gave %q, focus %q", m.input.Value(), m.focus)
	}
	if update(tab); m.input.Value() != "@builder-" || !strings.Contains(m.toastText(), "builder-1 · builder-2") {
		t.Fatalf("ambiguous tab gave %q, toast %q", m.input.Value(), m.toastText())
	}
	m.input.SetValue("@builder-2")
	if update(tab); m.input.Value() != "@builder-2 " {
		t.Fatalf("tab gave %q", m.input.Value())
	}
	for _, text := range []string{"@lead hi", "@nobody hi", "hi @builder-1"} {
		if _, _, ok := m.mention(text); ok {
			t.Errorf("%q is not for a teammate", text)
		}
	}

	m.input.SetValue("@builder-2 what does this repo do?")
	cmd := update(keyEnter())
	if cmd == nil || m.queued != "" || m.busy || m.input.Value() != "" {
		t.Fatalf("the mention went to the lead: queued %q, busy %v", m.queued, m.busy)
	}
	update(cmd())
	last := m.entries[len(m.entries)-1]
	if last.kind != entryAssistant || last.who != "builder-2" || !strings.Contains(last.text, "The user asks:\n\nwhat does this repo do?") ||
		!strings.Contains(last.text, "the user decides") {
		t.Fatalf("answer = %+v", last)
	}
	out := m.renderTranscript(80)
	if !strings.Contains(ansi.Strip(out), "you asked builder-2: what does this repo do?") || !strings.Contains(out, leadLabelStyle.Render("builder-2")) {
		t.Fatalf("conversation:\n%s", ansi.Strip(out))
	}

	// An answer that comes after the user left the conversation stays out of
	// the next one; the teammate's view keeps it.
	m.input.SetValue("@builder-2 and now?")
	cmd = update(keyEnter())
	m.bumpGen() // /new
	m.loadHistory()
	update(cmd())
	if len(m.entries) != 0 || !strings.Contains(m.toastText(), "builder-2 answered") {
		t.Fatalf("the late answer joined the new conversation: %+v", m.entries)
	}
}

func TestTeammatesGetTheirRules(t *testing.T) {
	fakeAdapter(t)
	project := t.TempDir()
	for rel, text := range map[string]string{
		".switch/rules.md":                "project rule",
		".switch/rules/roles/builder.md":  "builder rule",
		".switch/rules/roles/subagent.md": "subagent rule",
		".switch/rules/roles/lead.md":     "lead-only rule",
	} {
		path := filepath.Join(project, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base, err := rules.Load(config.RulesConfig{Global: filepath.Join(project, "none.md"),
		Project: filepath.Join(project, ".switch/rules.md"), Local: filepath.Join(project, ".switch/rules.local.md")})
	if err != nil {
		t.Fatal(err)
	}
	lead := config.Agent{Name: "lead", Role: "lead", Driver: "claude"}
	builder := config.Agent{Name: "builder-1", Role: "builder", Driver: "claude"}
	m := New(Options{Context: context.Background(), Project: project, Rules: base, Lead: lead, Fleet: []config.Agent{lead, builder},
		Run: config.RunConfig{Subagents: config.SubagentConfig{MaxPerBuilder: 1}}})
	defer m.shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	reply, err := m.team.Ask(ctx, "lead", "builder-1", "hi")
	if err != nil || !strings.Contains(reply, "project rule") || !strings.Contains(reply, "builder rule") || strings.Contains(reply, "lead-only") {
		t.Fatalf("teammate prompt: %q, %v", reply, err)
	}
	m.team.asked["builder-1"] = true // its subagents get its rules and roles/subagent.md
	if _, err := m.team.Spawn("builder-1", "", execution.SpawnRequest{Name: "scout", Brief: "look"}); err != nil {
		t.Fatal(err)
	}
	if subs, _ := m.team.Wait(ctx, "builder-1", "", nil); len(subs) != 1 || !strings.Contains(subs[0].Summary, "builder rule") || !strings.Contains(subs[0].Summary, "subagent rule") {
		t.Fatalf("subagent prompt: %+v", subs)
	}
	delete(m.team.asked, "builder-1")

	// Project rules the user skips are not sent to teammates either.
	m.rulesReview = &rulesReview{}
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(model)
	if reply, err := m.team.Ask(ctx, "lead", "builder-1", "hi"); err != nil || strings.Contains(reply, "project rule") || strings.Contains(reply, "builder rule") {
		t.Fatalf("after skipping project rules: %q, %v", reply, err)
	}
}
