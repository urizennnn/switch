package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
)

func TestRunWithoutRunExplains(t *testing.T) {
	next, cmd := model{}.handleSlash("/run")
	got := next.(model)
	if cmd != nil || got.busy || !strings.Contains(got.modal, "/plan") {
		t.Fatalf("modal = %q, busy = %v; want a /plan hint", got.modal, got.busy)
	}
}

func TestWorkerPermissionResumesJobStream(t *testing.T) {
	m := model{ctx: context.Background()}
	m.startJob("run", "running")

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
	m.jobStream <- "worker output"
	if msg, ok := cmd().(jobTextMsg); !ok || msg.text != "worker output" {
		t.Fatalf("resume read %#v, want job text", msg)
	}
	m.endJob()
}
