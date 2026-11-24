package acpdriver

import (
	"context"
	"testing"
)

// A question is drawn like an adapter's title, so the agent's text cannot
// reach the terminal raw; skipping it, as esc does, is no answer.
func TestQuestionAnswers(t *testing.T) {
	q := NewQuestion("builder-1", "which\x1b]2;owned\a db?\n", []string{"pg", " ", "my\nsql"})
	if q.Title != "which db?" || len(q.Options) != 2 || q.Options[1].ID != "my sql" {
		t.Fatalf("question = %q %+v", q.Title, q.Options)
	}
	q.Respond("pg")
	if answer, ok := q.Answer(context.Background()); !ok || answer != "pg" {
		t.Fatalf("answer = %q, %v", answer, ok)
	}
	q = NewQuestion("builder-1", "why?", nil)
	q.Resolve(PermissionDeny)
	if _, ok := q.Answer(context.Background()); ok || q.Withdrawn() {
		t.Fatal("a skipped question was answered, or withdrawn")
	}
	// A question nobody waits for any more says so, and takes no answer.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := q.Answer(ctx); ok || !q.Withdrawn() {
		t.Fatal("a question whose asker stopped waiting is not withdrawn")
	}
	q.Respond("too late")
	if !q.Withdrawn() || (<-q.response).OptionID != "" {
		t.Fatal("a withdrawn question took an answer")
	}
}
