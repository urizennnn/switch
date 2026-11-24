package acpdriver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	acp "github.com/coder/acp-go-sdk"
)

// toolDetail picks what a tool call acts on from its raw input, so a call
// titled "read" or "execute" can be shown as the file or command it is.
func toolDetail(raw any, locations []acp.ToolCallLocation) string {
	if input, ok := raw.(map[string]any); ok {
		if to, ok := input["to"].(string); ok && to != "" { // switch_ask
			return "→ " + oneLine(to)
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
		return oneLine(locations[0].Path)
	}
	return ""
}

// toolPaths returns the files a tool call names in its locations, its diffs
// (where a rename goes) and the usual path keys of its input, absolute
// against cwd, and whether every one is inside cwd once symlinks are
// resolved and outside git's and the agents' settings there. A call that
// names none is not.
func toolPaths(cwd string, call acp.ToolCallUpdate) ([]string, bool) {
	return checkPaths(cwd, toolNames(call))
}

// toolNames returns the paths a tool call names, as it spells them.
func toolNames(call acp.ToolCallUpdate) []string {
	var names []string
	for _, location := range call.Locations {
		names = append(names, location.Path)
	}
	for _, content := range call.Content {
		if content.Diff != nil {
			names = append(names, content.Diff.Path)
		}
	}
	if input, ok := call.RawInput.(map[string]any); ok {
		for _, key := range []string{"file_path", "filePath", "path", "notebook_path"} {
			if name, ok := input[key].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

// checkPaths is toolPaths for names already taken from tool calls.
func checkPaths(cwd string, names []string) ([]string, bool) {
	root, err := filepath.EvalSymlinks(cwd)
	inside := err == nil
	var paths []string
	for _, name := range names {
		if name == "" {
			continue
		}
		if name == "~" || strings.HasPrefix(name, "~/") {
			home, _ := os.UserHomeDir()
			name = home + name[1:]
		}
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		path = filepath.Clean(path)
		// Clean takes ".." lexically but the system takes it after a
		// symlink, so a path that uses it is never inside.
		inside = inside && !slices.Contains(strings.Split(filepath.ToSlash(name), "/"), "..")
		if inside {
			real, ok := resolve(path)
			rel, err := filepath.Rel(root, real)
			inside = ok && err == nil && filepath.IsLocal(rel) && !settings(rel)
		}
		if shown := oneLine(path); !slices.Contains(paths, shown) {
			paths = append(paths, shown)
		}
	}
	return paths, inside && len(paths) > 0
}

// settings reports whether rel goes through where git or an agent keeps
// settings it loads from the directory. They can run commands, so a worker
// that edited them unasked could grant itself what it has to ask for, or
// point its worktree's git at the project's own repository.
func settings(rel string) bool {
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		for _, setting := range []string{".git", ".claude", ".codex", ".opencode", ".mcp.json", "opencode.json", "opencode.jsonc"} {
			if strings.EqualFold(name, setting) { // Unicode folding, as macOS takes ſ for s
				return true
			}
		}
	}
	return false
}

// resolve evaluates the symlinks in path, or in its nearest ancestor that
// exists: a file to be created has none of its own. A dangling symlink does
// not resolve, as writing to it creates its target.
func resolve(path string) (string, bool) {
	rest := ""
	for {
		real, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(real, rest), true
		}
		parent := filepath.Dir(path)
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) || parent == path {
			return "", false
		}
		path, rest = parent, filepath.Join(filepath.Base(path), rest)
	}
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(plain(text)), " ")
}

// plain drops escape sequences and control characters but newlines and tabs
// from adapter text. Drawn outside markdown, a carriage return or a cursor
// movement would redraw the line, such as the command a user is to approve.
// A carriage return becomes a line break, so progress frames stay apart.
func plain(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(ansi.Strip(text), "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, text)
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
	return plain(strings.Join(parts, "\n\n"))
}
