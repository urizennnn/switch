package planning

import (
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
