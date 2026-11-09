package acpdriver

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	acp "github.com/coder/acp-go-sdk"
)

// toolDetail picks what a tool call acts on from its raw input, so a call
// titled "read" or "execute" can be shown as the file or command it is.
func toolDetail(raw any, locations []acp.ToolCallLocation) string {
	if input, ok := raw.(map[string]any); ok {
		if to, ok := input["to"].(string); ok && to != "" { // switch_ask
			return "→ " + to
		}
		for _, key := range []string{"command", "cmd", "file_path", "filePath", "path", "pattern", "query", "url", "name", "description"} {
			switch value := input[key].(type) {
			case string:
				if value != "" {
					return oneLine(value)
				}
			case []any:
				parts := make([]string, 0, len(value))
				for _, part := range value {
					parts = append(parts, fmt.Sprint(part))
				}
				if len(parts) > 0 {
					return oneLine(strings.Join(parts, " "))
				}
			}
		}
	}
	if len(locations) > 0 && locations[0].Path != "" {
		return locations[0].Path
	}
	return ""
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// toolOutput keeps the adapter's displayable text and diffs. ACP content
// updates replace, rather than append to, the previous collection.
func toolOutput(contents []acp.ToolCallContent) string {
	var parts []string
	for _, item := range contents {
		switch {
		case item.Content != nil:
			if text := contentText(item.Content.Content); text != "" {
				parts = append(parts, text)
			}
		case item.Diff != nil:
			var b strings.Builder
			fmt.Fprintf(&b, "File: %s\n", item.Diff.Path)
			if item.Diff.OldText != nil {
				for _, line := range strings.Split(*item.Diff.OldText, "\n") {
					b.WriteString("- " + line + "\n")
				}
			}
			for _, line := range strings.Split(item.Diff.NewText, "\n") {
				b.WriteString("+ " + line + "\n")
			}
			parts = append(parts, strings.TrimRight(b.String(), "\n"))
		}
	}
	// Tool output is untrusted. Do not let terminal escape sequences in a
	// command's output control the surrounding interface.
	return ansi.Strip(strings.Join(parts, "\n\n"))
}
