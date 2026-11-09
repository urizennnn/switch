package tui

import (
	"context"
	"testing"

	"github.com/urizennnn/switch/internal/config"
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
