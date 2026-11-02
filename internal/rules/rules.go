package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urizennnn/switch/internal/config"
)

type Source struct {
	Path  string
	Scope string
	Text  string
}

type Set struct {
	Sources []Source
}

// Load reads global, project, and project-local rule files. Missing files are
// not an error; rules are optional.
func Load(cfg config.RulesConfig) (Set, error) {
	var set Set
	for _, item := range []struct {
		path  string
		scope string
	}{
		{cfg.Global, "global"},
		{cfg.Project, "project"},
		{cfg.Local, "local"},
	} {
		path := expandHome(item.path)
		if strings.TrimSpace(path) == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Set{}, fmt.Errorf("read rules %s: %w", path, err)
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		set.Sources = append(set.Sources, Source{
			Path:  path,
			Scope: item.scope,
			Text:  text,
		})
	}
	return set, nil
}

func (s Set) Empty() bool {
	return len(s.Sources) == 0
}

// Text returns the merged rules for display.
func (s Set) Text() string {
	if len(s.Sources) == 0 {
		return ""
	}
	var b strings.Builder
	for i, source := range s.Sources {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## %s rules (%s)\n\n%s", source.Scope, source.Path, source.Text)
	}
	return b.String()
}

// Prompt prepends the effective rules to a user message. ACP has no stable
// client-provided system prompt, so this is the portable injection point.
func (s Set) Prompt(user string) string {
	if len(s.Sources) == 0 {
		return user
	}
	var b strings.Builder
	b.WriteString("Follow these operating rules for this session:\n\n")
	b.WriteString(s.Text())
	b.WriteString("\n\nUser request:\n")
	b.WriteString(user)
	return b.String()
}

func expandHome(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}
