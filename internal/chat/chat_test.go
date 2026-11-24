package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

func TestRulesSentOncePerSessionAndOnChange(t *testing.T) {
	project := filepath.Join(t.TempDir(), ".switch", "rules.md")
	if err := os.MkdirAll(filepath.Dir(project), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(text string) {
		if err := os.WriteFile(project, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("be brief")
	base, err := rules.Load(config.RulesConfig{Project: project})
	if err != nil {
		t.Fatal(err)
	}
	c := &Chat{Name: "lead", Rules: base}

	send := func(text string) string {
		prompt, hash, err := c.withRules(text)
		if err != nil {
			t.Fatal(err)
		}
		c.rulesSent = hash // as Send does after a successful turn
		return prompt
	}
	if got := send("one"); !strings.Contains(got, "be brief") {
		t.Fatalf("first message has no rules: %q", got)
	}
	if got := send("two"); got != "two" {
		t.Fatalf("second message repeated the rules: %q", got)
	}
	write("be thorough")
	if got := send("three"); !strings.Contains(got, "replace the earlier ones") || !strings.Contains(got, "be thorough") {
		t.Fatalf("changed rules not resent: %q", got)
	}
	write("")
	if got := send("four"); !strings.Contains(got, "no longer apply") {
		t.Fatalf("removed rules not announced: %q", got)
	}
	if got := send("five"); got != "five" {
		t.Fatalf("empty rules sent again: %q", got)
	}
}

func TestOpenResumesASessionStoredUnderAnOlderDriverName(t *testing.T) {
	// Answers initialize and session/load, and session/new with another ID.
	fake := filepath.Join(t.TempDir(), "fake-acp")
	script := `#!/bin/sh
while read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
  *'"initialize"'*) result='{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}' ;;
  *'"session/load"'*) result='{}' ;;
  *'"session/new"'*) result='{"sessionId":"new"}' ;;
  *) continue ;;
  esac
  echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":$result}"
done
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	acpdriver.SetCustom(map[string]acpdriver.Adapter{"claude": {Command: []string{fake}}})
	t.Cleanup(func() { acpdriver.SetCustom(nil) })
	state, err := store.Open(filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	project := t.TempDir()
	// Older builds stored the driver as the config spelled it.
	if err := state.SaveChatSession(ctx, store.ChatSession{Project: project, Name: "lead", Agent: "Claude-Code", SessionID: "old"}); err != nil {
		t.Fatal(err)
	}

	c, status, err := Open(ctx, Options{Project: project, Name: "lead", Agent: "claude", Store: state})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	if status.SessionID != "old" || !status.Resumed {
		t.Fatalf("session %q, resumed %v: want the stored session resumed", status.SessionID, status.Resumed)
	}
}
