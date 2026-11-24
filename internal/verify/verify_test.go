package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunPassFailTimeoutAndMissing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	results, ok := Run(ctx, dir, []string{"echo hi > out.txt", "grep -q hi out.txt"}, 5*time.Second, nil)
	if !ok || len(results) != 2 {
		t.Fatalf("pass: ok=%v results=%+v", ok, results)
	}

	results, ok = Run(ctx, dir, []string{"echo boom >&2; exit 3", "echo never"}, 5*time.Second, nil)
	if ok || len(results) != 2 || results[0].ExitCode != 3 || !strings.Contains(results[0].Output, "boom") {
		t.Fatalf("fail: ok=%v results=%+v", ok, results)
	}

	start := time.Now()
	results, ok = Run(ctx, dir, []string{"sleep 30"}, 200*time.Millisecond, nil)
	if ok || !results[0].TimedOut || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout: ok=%v results=%+v after %s", ok, results, time.Since(start))
	}

	results, ok = Run(ctx, dir, []string{"definitely-not-a-command-xyz"}, 5*time.Second, nil)
	if ok || results[0].ExitCode != 127 {
		t.Fatalf("missing command: ok=%v results=%+v", ok, results)
	}

	results, ok = Run(ctx, dir, nil, time.Second, nil)
	if !ok || len(results) != 0 {
		t.Fatalf("no commands should pass: ok=%v results=%+v", ok, results)
	}
	if !strings.Contains(Summary(results), "no verification commands") {
		t.Fatal("empty summary")
	}
}

// A command that fails once and then passes is flaky: it passes, with the
// failure kept as evidence, and the commands after it still run. One that
// fails twice fails, with both failures kept, since the rerun may only trip
// over what the first run left; reset runs before a rerun, and a timeout is
// not run again.
func TestRunRerunsAFailureOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	results, ok := Run(ctx, dir, []string{"test -f tried || { touch tried; echo first >&2; exit 1; }", "echo after"}, 5*time.Second, nil)
	if !ok || len(results) != 3 || results[0].Passed() || !results[1].Passed() || !results[1].Flaky || results[2].Output != "after\n" {
		t.Fatalf("flaky: ok=%v results=%+v", ok, results)
	}
	if summary := Summary(results); !strings.Contains(summary, "first") || !strings.Contains(summary, "passed on a rerun (flaky)") {
		t.Fatalf("summary hides the flaky failure:\n%s", summary)
	}

	results, ok = Run(ctx, dir, []string{"mkdir build || exit 1; echo compile error >&2; exit 1"}, 5*time.Second, nil)
	if summary := Summary(results); ok || len(results) != 2 || results[1].Flaky || !strings.Contains(summary, "compile error") || !strings.Contains(summary, "exists") {
		t.Fatalf("failing twice: ok=%v results=%+v", ok, results)
	}

	// reset undoes what the failed run wrote, so the rerun cannot pass on it.
	reset := func() error { return os.Remove(filepath.Join(dir, "golden")) }
	results, ok = Run(ctx, dir, []string{"test -f golden || { touch golden; exit 1; }"}, 5*time.Second, reset)
	if ok || len(results) != 2 || results[1].Flaky {
		t.Fatalf("reset: ok=%v results=%+v", ok, results)
	}

	results, _ = Run(ctx, dir, []string{"echo $$ >> slow; sleep 30"}, 200*time.Millisecond, nil)
	if slow, _ := os.ReadFile(filepath.Join(dir, "slow")); len(results) != 1 || !results[0].TimedOut || strings.Count(string(slow), "\n") != 1 {
		t.Fatalf("timeout rerun: results=%+v runs=%q", results, slow)
	}
}

func TestRunAddsTheEnvironment(t *testing.T) {
	results, ok := Run(context.Background(), t.TempDir(), []string{`test "$SWITCH_SLOT" = 2 && test -n "$PATH"`}, 5*time.Second, nil, "SWITCH_SLOT=2")
	if !ok {
		t.Fatalf("env not set: %+v", results)
	}
}
