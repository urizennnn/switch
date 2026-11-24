package tui

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// TestMain lets this test binary stand in for an ACP adapter, so tests can
// run real sessions without starting any agent CLI.
func TestMain(m *testing.M) {
	if os.Getenv("SWITCH_FAKE_ACP") != "" {
		agent := &fakeAgent{}
		agent.conn = acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)
		<-agent.conn.Done()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeAdapter makes the claude adapter this binary: PATH holds only a script
// named like the adapter.
func fakeAdapter(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	installAdapter(t, "SWITCH_FAKE_ACP=1 exec '"+exe+"'")
}

// stuckAdapter makes the claude adapter one that never answers, like an
// adapter stuck starting.
func stuckAdapter(t *testing.T) {
	installAdapter(t, "exec /bin/sleep 30")
}

func installAdapter(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude-agent-acp"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

type fakeAgent struct {
	acp.Agent // methods Switch never calls
	conn      *acp.AgentSideConnection
}

func (*fakeAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
}

func (*fakeAgent) NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{SessionId: "fake"}, nil
}

func (*fakeAgent) Cancel(context.Context, acp.CancelNotification) error { return nil }

// Prompt answers "echo: <prompt>". A prompt with a line "wait" streams one
// chunk, and one with "flood" 600; both then wait until the turn is cancelled.
// One with a line "token" answers with its control-plane token.
func (a *fakeAgent) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
	var text string
	for _, block := range req.Prompt {
		if block.Text != nil {
			text += block.Text.Text
		}
	}
	say := func(chunk string) {
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: req.SessionId, Update: acp.UpdateAgentMessageText(chunk)})
	}
	lines := strings.Split(text, "\n")
	if slices.Contains(lines, "token") {
		say(os.Getenv("SWITCH_TOKEN"))
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if slices.Contains(lines, "wait") || slices.Contains(lines, "flood") {
		n := 1
		if slices.Contains(lines, "flood") {
			n = 600
		}
		for range n {
			say("working ")
		}
		<-ctx.Done()
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	say("echo: " + text)
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}
