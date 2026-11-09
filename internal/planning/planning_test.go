package planning

import (
	"context"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/plan"
)

func TestCheckWorkersNeedsANonLeadWorkerPerRole(t *testing.T) {
	planned := plan.Plan{Tasks: []plan.Task{{ID: "a", Role: "builder"}}}
	fleet := []config.Agent{{Name: "lead", Role: "lead"}, {Name: "builder-1", Role: "builder"}}
	if err := checkWorkers(planned, fleet, "lead"); err != nil {
		t.Fatalf("valid fleet rejected: %v", err)
	}
	// builder-1 leads, so nobody can build.
	if err := checkWorkers(planned, fleet, "builder-1"); err == nil || !strings.Contains(err.Error(), `"builder" worker`) {
		t.Fatalf("err = %v, want missing builder", err)
	}
}

func TestExecuteNeedsGitBeforePlanning(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// A goal with no reachable lead: the git check must fail first.
	_, err := Execute(context.Background(), Options{Project: t.TempDir(), Goal: "anything", Agent: "no-such-adapter"})
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("err = %v, want the git repository check", err)
	}
}
