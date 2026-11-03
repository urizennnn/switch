package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/store"
)

func TestRunWithoutRunExplains(t *testing.T) {
	next, cmd := model{}.handleSlash("/run")
	got := next.(model)
	if cmd != nil || got.job != "" || !strings.Contains(got.modal, "/plan") {
		t.Fatalf("modal = %q, job = %q; want a /plan hint", got.modal, got.job)
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
	if cmd != nil || !strings.Contains(got.modal, "/stop") {
		t.Fatalf("/plan during a run: modal = %q", got.modal)
	}
}

func TestWorkerPermissionResumesJobStream(t *testing.T) {
	m := model{ctx: context.Background()}
	m.startJob("run", "running", false)
	defer m.endJob()

	request := &acpdriver.PermissionRequest{Options: []acpdriver.PermissionOption{{ID: "allow", Kind: "allow_once"}}}
	next, _ := m.Update(jobPermissionMsg{request: request})
	m = next.(model)
	if m.permission != request || m.permissionResume == nil {
		t.Fatal("worker permission did not open the modal with a resume command")
	}

	next, cmd := m.handlePermissionKey(tea.KeyPressMsg{Code: '1', Text: "1"})
	m = next.(model)
	if m.permission != nil || cmd == nil || m.state != "running" {
		t.Fatalf("after answer: permission=%v cmd=%v state=%q", m.permission, cmd != nil, m.state)
	}

	// The resume command must read the job stream, not the chat session.
	m.jobStream <- jobText{agent: "builder-1", text: "worker output"}
	if msg, ok := cmd().(jobTextMsg); !ok || msg.text != "worker output" {
		t.Fatalf("resume read %#v, want job text", msg)
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

func TestFindTaskByPlanID(t *testing.T) {
	m := model{tasks: []store.Task{{ID: "run-1:add-flag"}, {ID: "run-1:docs"}}}
	if task, ok := m.findTask("docs"); !ok || task.ID != "run-1:docs" {
		t.Fatalf("findTask(docs) = %q, %v", task.ID, ok)
	}
	if _, ok := m.findTask("missing"); ok {
		t.Fatal("found a missing task")
	}
}
