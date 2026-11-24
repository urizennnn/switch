package execution

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/store"
)

// Approving the last task ends a run nothing drives, as its loop would: the
// run's checks run once more and decide between completed and blocked, and
// a task left blocked blocks it; one left to start waits for a run. A
// process driving the run ends it itself.
func TestLastApprovalEndsTheRun(t *testing.T) {
	checked := filepath.Join(t.TempDir(), "checked")
	once := "test ! -f " + checked + " && touch " + checked // passes the merge's checks, not the run's last
	stuck := builderTask("b")
	stuck.Verify = []string{"exit 1"}
	for _, c := range []struct {
		verify []string
		driven bool
		others []store.Task
		want   string
	}{
		{nil, false, nil, RunCompleted},
		{[]string{once}, false, nil, RunBlocked},
		{nil, true, nil, RunWaiting},
		{nil, false, []store.Task{stuck}, RunBlocked},
		{nil, false, []store.Task{builderTask("c", "a")}, RunWaiting}, // c can start now, which only a run does
	} {
		f := newFixture(t, []store.RunAgent{lead, builder1}, append([]store.Task{builderTask("a")}, c.others...)...)
		ctx := context.Background()
		e := f.engine(config.RunConfig{Verify: c.verify, MaxAttempts: 1})
		if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
			t.Fatalf("Run = %q, %v", outcome, err)
		}
		var driver *RunLock
		if c.driven {
			var err error
			if driver, err = LockRun(e.opts.StateDir, "run", true); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.Approve(ctx, "run:a"); err != nil {
			t.Fatalf("Approve: %v", err)
		}
		driver.Release()
		if run, _, _ := f.state.GetRun(ctx, "run"); run.Status != c.want || f.task("run:a").Status != StatusMerged {
			t.Errorf("verify %q, driven %v, %d others: run %s, a %s; want run %s, a merged", c.verify, c.driven, len(c.others), run.Status, f.task("run:a").Status, c.want)
		}
	}
}

// A run nobody watches tells the user through [run] notify when it needs
// them: a task to review or blocked, a question, how the run ended. A TUI
// shows these itself.
func TestHeadlessRunNotifiesTheUser(t *testing.T) {
	log := filepath.Join(t.TempDir(), "notified")
	cfg := config.RunConfig{MaxAttempts: 1, Notify: `printf '%s|%s|%s|%s\n' "$SWITCH_EVENT" "$SWITCH_RUN" "$SWITCH_TASK" "$SWITCH_MESSAGE" >> ` + log}
	notified := func(want ...string) {
		t.Helper()
		var got []string
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if data, _ := os.ReadFile(log); strings.Count(string(data), "\n") >= len(want) {
				got = strings.Split(strings.TrimSpace(string(data)), "\n")
				break
			}
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("notified %q, want %q", got, want)
		}
		_ = os.Remove(log)
	}
	ctx := context.Background()

	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	f.engine(cfg).Question(ctx, "builder-1", "run:a", "watched?", nil) // a TUI's engine
	e := f.engine(cfg)
	e.opts.Headless = true
	if outcome, err := e.Run(ctx); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if got := e.Question(ctx, "builder-1", "run:a", "which db?\npg or mysql", nil); !strings.Contains(got, "no user is watching") {
		t.Fatalf("question in a run nobody watches = %q", got)
	}
	if err := e.Approve(ctx, "run:a"); err != nil {
		t.Fatal(err)
	}
	notified("review|run|a|a: waiting for approval", "question|run|a|builder-1 asks: which db?", "completed|run||run completed")

	b := builderTask("b")
	b.Verify = []string{"exit 1"}
	g := newFixture(t, []store.RunAgent{lead, builder1}, b)
	e = g.engine(cfg)
	e.opts.Headless = true
	if outcome, _ := e.Run(ctx); outcome != RunBlocked {
		t.Fatalf("Run = %q", outcome)
	}
	notified("blocked|run|b|b: attempt limit 1 reached: verification failed:", "blocked|run||run blocked: b: blocked: attempt limit 1 reached: verification failed:")
}

// In a run the user watches, a worker's question waits among their
// permission prompts, and its turn's idle watchdog waits with it.
func TestQuestionHoldsTheWatchdog(t *testing.T) {
	f := newFixture(t, []store.RunAgent{lead, builder1}, builderTask("a"))
	e := f.engine(config.RunConfig{IdleTimeout: 1})
	var asked *acpdriver.PermissionRequest
	e.opts.OnPermission = func(request *acpdriver.PermissionRequest) {
		asked = request
		go func() {
			time.Sleep(1500 * time.Millisecond) // past the idle timeout
			request.Respond(request.Options[1].ID)
		}()
	}
	fake := e.opts.Turn
	answers := make(chan string, 3) // one per attempt, should the watchdog stop the first
	e.opts.Turn = func(ctx context.Context, req TurnRequest) (TurnResult, error) {
		if req.Agent.Role != "lead" {
			answers <- e.Question(ctx, req.Agent.Name, req.TaskID, "which db?", []string{"pg", "mysql"})
		}
		return fake(ctx, req)
	}
	if outcome, err := e.Run(context.Background()); err != nil || outcome != RunWaiting {
		t.Fatalf("Run = %q, %v", outcome, err)
	}
	if got := <-answers; got != "mysql" || asked.Kind != acpdriver.KindQuestion || asked.Agent != "builder-1" || asked.Title != "which db?" {
		t.Fatalf("answer %q to %+v", got, asked)
	}
	if a := f.task("run:a"); a.Status != StatusReview || a.Attempts != 1 {
		t.Fatalf("a = %s on attempt %d, want review on attempt 1", a.Status, a.Attempts)
	}
}
