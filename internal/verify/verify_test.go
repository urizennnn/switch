package verify

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunPassFailTimeoutAndMissing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	results, ok := Run(ctx, dir, []string{"echo hi > out.txt", "grep -q hi out.txt"}, 5*time.Second)
	if !ok || len(results) != 2 {
		t.Fatalf("pass: ok=%v results=%+v", ok, results)
	}

	results, ok = Run(ctx, dir, []string{"echo boom >&2; exit 3", "echo never"}, 5*time.Second)
	if ok || len(results) != 1 || results[0].ExitCode != 3 || !strings.Contains(results[0].Output, "boom") {
		t.Fatalf("fail: ok=%v results=%+v", ok, results)
	}

	start := time.Now()
	results, ok = Run(ctx, dir, []string{"sleep 30"}, 200*time.Millisecond)
	if ok || !results[0].TimedOut || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout: ok=%v results=%+v after %s", ok, results, time.Since(start))
	}

	results, ok = Run(ctx, dir, []string{"definitely-not-a-command-xyz"}, 5*time.Second)
	if ok || results[0].ExitCode != 127 {
		t.Fatalf("missing command: ok=%v results=%+v", ok, results)
	}

	results, ok = Run(ctx, dir, nil, time.Second)
	if !ok || len(results) != 0 {
		t.Fatalf("no commands should pass: ok=%v results=%+v", ok, results)
	}
	if !strings.Contains(Summary(results), "no verification commands") {
		t.Fatal("empty summary")
	}
}
