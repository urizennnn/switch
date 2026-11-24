package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/policy"
)

func TestSessionModesAndWorkerLimit(t *testing.T) {
	m := New(Options{Context: context.Background(), Run: config.RunConfig{MaxConcurrency: 2}})
	defer m.cancel()
	next, _ := m.handleSlash("/mode plan")
	m = next.(model)
	if m.permissionMode != policy.Plan {
		t.Fatalf("mode = %s", m.permissionMode)
	}
	next, _ = m.handleSlash("/run")
	if next.(model).job != "" {
		t.Fatal("plan mode started a worker run")
	}
	next, _ = m.handleSlash("/parallel 4")
	m = next.(model)
	if m.runConfig.MaxConcurrency != 4 {
		t.Fatalf("limit = %d", m.runConfig.MaxConcurrency)
	}
	m.job = "run"
	next, _ = m.handleSlash("/parallel 5")
	if next.(model).runConfig.MaxConcurrency != 4 {
		t.Fatal("changed concurrency during run")
	}
	next, _ = m.handleSlash("/mode full")
	if next.(model).permissionMode != policy.Plan {
		t.Fatal("changed mode during run")
	}
}

func TestPermissionPromptsQueueWhileEverySourceKeepsListening(t *testing.T) {
	m := New(Options{Context: context.Background()})
	defer m.cancel()
	m.chat = &chat.Chat{Session: &acpdriver.Session{ID: "s1"}}
	m.startJob("run", "running", false)
	defer m.endJob()
	options := []acpdriver.PermissionOption{{ID: "a", Name: "Allow once", Kind: "allow_once"}, {ID: "r", Name: "Reject", Kind: "reject_once"}}
	worker := &acpdriver.PermissionRequest{Agent: "builder-1", Kind: "edit", Options: options}
	teammate := &acpdriver.PermissionRequest{Agent: "builder-2", Kind: "execute", Options: options}
	lead := &acpdriver.PermissionRequest{Kind: "execute", Options: options}
	answers := []chan acpdriver.PermissionDecision{reply(worker), reply(teammate), reply(lead)}

	// A worker asks, and the run keeps streaming while the prompt is open.
	next, cmd := m.update(jobPermissionMsg{request: worker})
	m = next.(model)
	if m.permission != worker {
		t.Fatal("worker request not shown")
	}
	m.jobStream <- jobText{agent: "builder-3", text: "still streaming"}
	if msg, ok := cmd().(jobTextMsg); !ok || msg.text != "still streaming" {
		t.Fatalf("job listener read %#v while a prompt was open", msg)
	}

	// A teammate and the lead ask too: they wait their turn, and their
	// sources keep listening.
	next, cmd = m.update(teamPermissionMsg{request: teammate})
	m = next.(model)
	perms, again := m.teamPerms, &acpdriver.PermissionRequest{Agent: "builder-2"}
	go func() { perms <- again }()
	if msg, ok := cmd().(teamPermissionMsg); !ok || msg.request != again {
		t.Fatal("team listener not re-armed")
	}
	next, cmd = m.update(permissionMsg{gen: m.gen, request: lead})
	m = next.(model)
	if cmd == nil || m.permission != worker || len(m.permissionQueue) != 2 {
		t.Fatalf("later requests replaced the first: queue %d, lead listener %v", len(m.permissionQueue), cmd != nil)
	}
	if !strings.Contains(ansi.Strip(m.screen()), "2 more waiting") {
		t.Fatal("prompt does not say how many more wait")
	}

	// A digit pressed as the prompt opened is dropped, without holding
	// answers off; typing goes on in the input and holds them off.
	hold := m.permissionHold
	next, _ = m.Update(tea.KeyPressMsg{Code: '1', Text: "1"})
	m = next.(model)
	if m.permission != worker || m.input.Value() != "" || !m.permissionHold.Equal(hold) {
		t.Fatalf("a digit pressed early answered, was typed or held answers off; input %q", m.input.Value())
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = next.(model)
	if m.permission != worker || m.input.Value() != "x" || !m.permissionHold.After(hold) {
		t.Fatalf("typing did not reach the input or hold answers off; input %q", m.input.Value())
	}
	// A digit in the message being typed is typing too, not an answer.
	hold = m.permissionHold
	next, _ = m.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
	m = next.(model)
	if m.permission != worker || m.input.Value() != "x2" || !m.permissionHold.After(hold) {
		t.Fatalf("a digit typed mid-message was dropped or answered; input %q", m.input.Value())
	}

	// After a pause each answer goes to the request on screen and brings up
	// the next, held again.
	for i, want := range []*acpdriver.PermissionRequest{teammate, lead, nil} {
		m.permissionHold = time.Time{}
		next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		m = next.(model)
		if m.permission != want {
			t.Fatalf("after an answer the prompt shows %+v, want %+v", m.permission, want)
		}
		if want != nil && !time.Now().Before(m.permissionHold) {
			t.Fatal("the next prompt answers keys at once")
		}
		if len(answers[i]) != 1 || want != nil && len(answers[i+1]) != 0 {
			t.Fatalf("answer %d did not go to the request on screen", i+1)
		}
	}
	for _, answer := range answers {
		if got := <-answer; got.OptionID != "r" {
			t.Fatalf("esc answered %+v, want the reject option", got)
		}
	}
	if len(m.permissionQueue) != 0 || m.state != "running" {
		t.Fatalf("queue %d, state %q after every answer", len(m.permissionQueue), m.state)
	}
}

func TestRunPromptsEndWithTheRun(t *testing.T) {
	m := New(Options{Context: context.Background()})
	defer m.cancel()
	m.startJob("run", "running", false)
	first, teammate, second := &acpdriver.PermissionRequest{Agent: "builder-1", Kind: "edit"}, &acpdriver.PermissionRequest{Agent: "builder-1", Kind: "execute"}, &acpdriver.PermissionRequest{Agent: "builder-2", Kind: "edit"}
	answers := []chan acpdriver.PermissionDecision{reply(first), reply(second)}
	for _, msg := range []tea.Msg{jobPermissionMsg{request: first}, teamPermissionMsg{request: teammate}, jobPermissionMsg{request: second}} {
		next, _ := m.update(msg)
		m = next.(model)
	}
	m.endJob() // /stop, or the run finishing
	if m.permission != teammate || len(m.permissionQueue) != 0 {
		t.Fatalf("after the run the prompt shows %+v with %d waiting", m.permission, len(m.permissionQueue))
	}
	for _, answer := range answers {
		if len(answer) != 1 || !(<-answer).Cancel {
			t.Fatal("a stopped run's prompt was not cancelled")
		}
	}
}

// reply gives request the answer channel only acpdriver can make, so a test
// sees how the request was answered.
func reply(request *acpdriver.PermissionRequest) chan acpdriver.PermissionDecision {
	answer := make(chan acpdriver.PermissionDecision, 1)
	field := reflect.ValueOf(request).Elem().FieldByName("response")
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(answer))
	return answer
}

func TestPermissionPromptTakesKeysFromHiddenPanels(t *testing.T) {
	m := New(Options{Context: context.Background()})
	defer m.cancel()
	m.taskPicker = &taskPicker{}
	m.askPermission(&acpdriver.PermissionRequest{Kind: "edit", Options: []acpdriver.PermissionOption{{ID: "a", Kind: "allow_once"}}}, policy.Build, false)
	m.permissionHold = time.Time{}
	next, _ := m.Update(tea.KeyPressMsg{Code: '1', Text: "1"})
	if m = next.(model); m.permission != nil || m.taskPicker == nil {
		t.Fatal("a key went to the task list under the prompt")
	}
}

func TestFullModeStillAsksSubagentsBeforeNonReads(t *testing.T) {
	m := New(Options{Context: context.Background()})
	defer m.cancel()
	m.permissionMode = policy.Full
	for _, c := range []struct {
		agent, kind string
		inside      bool
		asks        bool
	}{
		{"builder-1", "edit", false, false},      // full mode covers workers
		{"builder-1/scout", "read", true, false}, // a subagent reads, as in build mode
		{"builder-1/scout", "edit", true, true},  // and asks for anything else, in its worker's worktree too
	} {
		next, _ := m.update(jobPermissionMsg{request: &acpdriver.PermissionRequest{Agent: c.agent, Kind: c.kind, Inside: c.inside}})
		if asked := next.(model).permission != nil; asked != c.asks {
			t.Errorf("%s %s: asked = %v, want %v", c.agent, c.kind, asked, c.asks)
		}
	}
}

// Planning only looks: the lead's reads in the project go ahead and anything
// else is refused, as in /mode plan.
func TestPlanningLeadMayRead(t *testing.T) {
	options := []acpdriver.PermissionOption{{ID: "a", Kind: "allow_once"}, {ID: "r", Kind: "reject_once"}}
	for kind, want := range map[string]string{"read": "a", "search": "a", "execute": "r", "edit": "r", "": "r"} {
		request := &acpdriver.PermissionRequest{Kind: kind, Inside: true, Options: options}
		answer := reply(request)
		planPermission(request)
		if got := (<-answer).OptionID; got != want {
			t.Errorf("%q request answered %q, want %q", kind, got, want)
		}
	}
	outside := &acpdriver.PermissionRequest{Kind: "read", Options: options}
	answer := reply(outside)
	planPermission(outside)
	if got := (<-answer).OptionID; got != "r" {
		t.Errorf("a read outside the project answered %q, want r", got)
	}
}

// Reads go ahead on their own only inside the asker's folder, and edits ask,
// a run worker's in its own worktree too; a prompt names the paths.
func TestOnlyRequestsInsideGoAheadOnTheirOwn(t *testing.T) {
	m := New(Options{Context: context.Background()})
	defer m.cancel()
	options := []acpdriver.PermissionOption{{ID: "a", Name: "Allow once", Kind: "allow_once"}, {ID: "r", Name: "Reject", Kind: "reject_once"}}
	for _, c := range []struct {
		agent, kind string
		inside, job bool
		asks        bool
	}{
		{"", "read", true, false, false},
		{"", "read", false, false, true},              // such as ~/.ssh
		{"builder-1", "edit", true, true, true},       // a worker in its own worktree: verify runs what it writes
		{"builder-1", "edit", false, true, true},      // or outside it
		{"", "edit", true, false, true},               // the lead's edits stay the user's call
		{"builder-1/scout", "edit", true, true, true}, // and a worker's subagent's
	} {
		m.permission = nil
		request := &acpdriver.PermissionRequest{Agent: c.agent, Kind: c.kind, Inside: c.inside, Paths: []string{"/home/me/.ssh/id_rsa"}, Options: options}
		answer := reply(request)
		m.askPermission(request, policy.Build, c.job)
		if asked := m.permission != nil; asked != c.asks {
			t.Errorf("%s %s inside=%v job=%v: asked = %v, want %v", c.agent, c.kind, c.inside, c.job, asked, c.asks)
		} else if !asked && (<-answer).OptionID != "a" {
			t.Errorf("%s %s inside=%v job=%v was not allowed", c.agent, c.kind, c.inside, c.job)
		} else if asked && !strings.Contains(ansi.Strip(m.screen()), "/home/me/.ssh/id_rsa") {
			t.Errorf("the prompt does not name the path")
		}
	}

	// A teammate's read outside its folder waits for the user too.
	read := &acpdriver.PermissionRequest{Kind: "read", Options: options}
	go m.team.permit("builder-1")(read)
	select {
	case got := <-m.teamPerms:
		if got != read {
			t.Fatalf("the user got %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a teammate's read outside its folder went ahead")
	}
}

// A request naming many files keeps its answers on screen: the paths are
// what a short terminal cuts.
func TestPermissionPromptKeepsItsAnswersOnScreen(t *testing.T) {
	m := New(Options{Context: context.Background()}) // 24 rows
	defer m.cancel()
	request := &acpdriver.PermissionRequest{Agent: "builder-1", Kind: "edit", Title: "apply_patch", Options: []acpdriver.PermissionOption{{ID: "a", Name: "Allow once", Kind: "allow_once"}, {ID: "r", Name: "Reject", Kind: "reject_once"}}}
	for i := range 20 {
		request.Paths = append(request.Paths, "/work/src/file"+strings.Repeat("x", i)+".go")
	}
	m.askPermission(request, policy.Build, true)
	screen := ansi.Strip(m.screen())
	for _, want := range []string{"edit · apply_patch", "1  Allow once", "2  Reject", request.Paths[0]} {
		if !strings.Contains(screen, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
}
