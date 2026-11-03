package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
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
	if got := next.(model); got.adapterPicker != nil || !strings.Contains(got.modal, "still running") {
		t.Fatalf("second install allowed: modal=%q", got.modal)
	}
	next, _ = m.Update(adapterActionMsg{name: "codex", action: "install globally"})
	if got := next.(model); got.installing != "" || !strings.Contains(got.modal, "complete") {
		t.Fatalf("after install: installing=%q modal=%q", got.installing, got.modal)
	}
}
