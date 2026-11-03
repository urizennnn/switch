package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/rules"
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
