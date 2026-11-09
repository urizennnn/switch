package acpdriver

import (
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestToolOutputDisplaysTextAndFileChanges(t *testing.T) {
	got := toolOutput([]acp.ToolCallContent{
		acp.ToolContent(acp.TextBlock("command output")),
		acp.ToolDiffContent("example.go", "new line", "old line"),
	})
	for _, want := range []string{"command output", "example.go", "- old line", "+ new line"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}
