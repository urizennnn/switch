package store

import (
	"context"
	"testing"
)

func TestChatSessionRoundTrip(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if _, ok, err := store.GetChatSession(ctx, "/tmp/project"); err != nil || ok {
		t.Fatalf("initial GetChatSession = ok %v, err %v", ok, err)
	}

	want := ChatSession{
		Project:   "/tmp/project",
		Agent:     "opencode",
		Model:     "opencode/deepseek-v4.1-flash-free",
		SessionID: "ses_123",
	}
	if err := store.SaveChatSession(ctx, want); err != nil {
		t.Fatalf("SaveChatSession: %v", err)
	}

	got, ok, err := store.GetChatSession(ctx, want.Project)
	if err != nil || !ok {
		t.Fatalf("GetChatSession = ok %v, err %v", ok, err)
	}
	if got.Agent != want.Agent || got.Model != want.Model || got.SessionID != want.SessionID {
		t.Fatalf("session = %#v, want %#v", got, want)
	}
}

func TestMessagesChronological(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	for _, message := range []string{"one", "two", "three"} {
		if err := store.AppendMessage(ctx, "/tmp/project", "user", message); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}

	messages, err := store.Messages(ctx, "/tmp/project", 10)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3", len(messages))
	}
	if messages[0].Content != "one" || messages[2].Content != "three" {
		t.Fatalf("messages out of order: %#v", messages)
	}
}

func TestLeadAgentSetting(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	if _, ok, err := store.LeadAgent(ctx, "/tmp/project"); err != nil || ok {
		t.Fatalf("initial LeadAgent = ok %v, err %v", ok, err)
	}
	if err := store.SetLeadAgent(ctx, "/tmp/project", "builder-1"); err != nil {
		t.Fatalf("SetLeadAgent: %v", err)
	}
	name, ok, err := store.LeadAgent(ctx, "/tmp/project")
	if err != nil || !ok {
		t.Fatalf("LeadAgent = ok %v, err %v", ok, err)
	}
	if name != "builder-1" {
		t.Fatalf("lead = %q, want builder-1", name)
	}
}
