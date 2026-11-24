package plancli

import "testing"

func TestRunRejectsAskBecausePlanningIsHeadless(t *testing.T) {
	if code := Run([]string{"--permission", "ask", "a goal"}); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}
